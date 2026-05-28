package main

import (
	"encoding/json"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/energye/systray"
	"golang.org/x/sys/windows"

	"mihomo-run/config"
	"mihomo-run/kernel"
	"mihomo-run/sysproxy"
	"mihomo-run/ui"
)

const APP_MUTEX = "Mihomo_Unique_Mutex"

func main() {
	// 1. 基本路徑初始化
	exePath, err := os.Executable()
	if err != nil {
		return
	}
	baseDir := filepath.Dir(exePath)
	_ = os.Chdir(baseDir)

	// 2. 嚴密創建 Windows 互斥鎖，防止多開衝突
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	hM, err := windows.CreateMutex(nil, false, mName)
	if err != nil || windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		if hM != 0 {
			windows.CloseHandle(hM)
		}
		return
	}
	defer windows.CloseHandle(hM)

	// 3. 檢查開機啟動參數
	isAutostart := false
	for _, arg := range os.Args {
		if arg == "---autostart" {
			isAutostart = true
			break
		}
	}

	// 4. 管理員權限檢查與自動提權（UAC 隔離）
	if !isAdmin() && !isAutostart {
		runAsAdmin(exePath, baseDir)
		return
	}

	// 5. 【結構化組裝】初始化各大模組經理
	configMgr := config.NewConfigManager(baseDir, exePath)
	win32API := &sysproxy.Win32NotificationBridge{} // 隱式實現 sysproxy 的 Win32APIInterface

	proxyMgr := sysproxy.NewProxyManager(configMgr, win32API)

	// 定義核心行程的生命週期勾子（Hooks）
	kernelHooks := kernel.KernelHooks{
		OnKernelStarted: func() {
			// 行程剛拉起時，立即嗅探本地 yaml 的通訊埠配置
			// 這是在獨立協程中執行的，不會卡住主守護 loop
			trayTemp := ui.NewTrayManager(configMgr, nil, nil)
			trayTemp.SniffAndSolidifyConfig()
		},
		OnKernelReady: func() {
			// 行程運行 1 秒穩定就緒後，發起 REST API 同步，並重新整理系統註冊表
			trayTemp := ui.NewTrayManager(configMgr, nil, proxyMgr)
			trayTemp.SyncConfigToKernel()

			if configMgr.GetJsonConfig("proxy") == "true" {
				configMgr.SetLastAppliedProxy(false)
				proxyMgr.SetProxyRegistry(true)
			}
		},
	}

	kernelMgr := kernel.NewKernelManager(configMgr, kernelHooks)
	kernelMgr.InitJobObject()
	defer kernelMgr.CloseJobObject()

	// 初始化終極托盤管理
	trayMgr := ui.NewTrayManager(configMgr, kernelMgr, proxyMgr)

	// 6. 系統級關機/中斷訊號捕獲：收到訊號後優雅調用托盤退出
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		systray.Quit()
	}()

	// 7. 異步啟動核心進程守護與圖標循環
	go func() {
		time.Sleep(1 * time.Second)
		go kernelMgr.MonitorKernelDaemon()
		go trayMgr.MonitorIconState()
		go trayMgr.WatchTunState()
	}()

	// 8. 移交控制權，啟動 GUI 訊息循環（此處將阻塞主執行緒，直到觸發選單退出）
	systray.Run(trayMgr.SetupTrayUI, func() {
		// onExit 回呼邏輯：安全清理 CDP 網頁面板與註冊表代理
		configMgr.MarkAsExiting()
		client := &http.Client{Timeout: 200 * time.Millisecond}
		apiURL := "http://127.0.0.1:52719/json"
		if resp, err := client.Get(apiURL); err == nil {
			var targets []map[string]interface{}
			if json.NewDecoder(resp.Body).Decode(&targets) == nil {
				for _, t := range targets {
					if id, ok := t["id"].(string); ok {
						if closeResp, closeErr := client.Get("http://127.0.0.1:52719/json/close/" + id); closeErr == nil {
							_ = closeResp.Body.Close()
						}
					}
				}
			}
			resp.Body.Close()
		}
		proxyMgr.SetProxyRegistry(false)
		systray.Quit()
		time.Sleep(100 * time.Millisecond)
	})
}

// ==========================================
// 輔助權限管理工具 (Windows 原生提權)
// ==========================================

func isAdmin() bool {
	var token windows.Token
	err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token)
	if err != nil {
		return false
	}
	defer token.Close()
	return token.IsElevated()
}

func runAsAdmin(exe, dir string) {
	verb, _ := syscall.UTF16PtrFromString("runas")
	exePtr, _ := syscall.UTF16PtrFromString(exe)
	cwdPtr, _ := syscall.UTF16PtrFromString(dir)
	_ = windows.ShellExecute(0, verb, exePtr, nil, cwdPtr, windows.SW_SHOWNORMAL)
}
