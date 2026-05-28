package main

import (
	"encoding/json"
	"log"
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

// 【优化吸收】：加入日志功能，但改写到程序运行目录下，避免无管理员权限时往 C 盘根目录写文件导致闪退
func initLog(baseDir string) {
	logPath := filepath.Join(baseDir, "mihomo_run_debug.log")
	f, err := os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		return
	}
	log.SetOutput(f)
	log.Println("=== Mihomo Run Started ===")
}

func main() {
	exePath, err := os.Executable()
	if err != nil {
		return
	}
	baseDir := filepath.Dir(exePath)
	_ = os.Chdir(baseDir)

	// 1. 单例锁保护 (避免用户疯狂双击导致多开)
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	hM, err := windows.CreateMutex(nil, false, mName)
	if err != nil || windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		if hM != 0 {
			windows.CloseHandle(hM)
		}
		return // 程序已在运行，静默退出
	}
	defer windows.CloseHandle(hM)

	// 2. 检查自启与 UAC 提权
	isAutostart := false
	for _, arg := range os.Args {
		if arg == "---autostart" {
			isAutostart = true
			break
		}
	}

	if !isAdmin() && !isAutostart {
		runAsAdmin(exePath, baseDir)
		return
	}

	// 初始化日志 (此时已确保 UAC 提权或常规运行)
	initLog(baseDir)

	// 3. 核心组件初始化
	configMgr := config.NewConfigManager(baseDir, exePath)
	log.Printf("[Main] Config initialized at: %s", baseDir)

	// 【致命修复 1】：找回原版的 Win32 桥接器，否则开启代理系统无法感知
	win32API := &sysproxy.Win32NotificationBridge{}
	proxyMgr := sysproxy.NewProxyManager(configMgr, win32API)

	// 【致命修复 2】：找回原版的 KernelHooks 核心同步逻辑！缺少这段，托盘 UI 和内核会彻底断连
	kernelHooks := kernel.KernelHooks{
		OnKernelStarted: func() {
			log.Println("[Main] Kernel started, sniffing config...")
			trayTemp := ui.NewTrayManager(configMgr, nil, nil)
			trayTemp.SniffAndSolidifyConfig()
		},
		OnKernelReady: func() {
			log.Println("[Main] Kernel ready, syncing config and proxy...")
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
	// 【保留原版】：防止系统资源泄漏
	defer kernelMgr.CloseJobObject()

	trayMgr := ui.NewTrayManager(configMgr, kernelMgr, proxyMgr)

	// 4. 监听系统中断信号
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("[Main] Received interrupt signal, gracefully shutting down...")
		systray.Quit()
	}()

	// 5. 【优化吸收】：极速启动后台事件监听协程 (1秒降低为100ms)
	go func() {
		time.Sleep(100 * time.Millisecond)
		go kernelMgr.MonitorKernelDaemon()
		go trayMgr.MonitorIconState()
		go trayMgr.WatchTunState()
	}()

	// 6. 运行托盘 UI (阻塞主线程)
	systray.Run(trayMgr.SetupTrayUI, func() {
		log.Println("[Main] Performing cleanup before exit...")
		configMgr.MarkAsExiting()

		// 【优化吸收】：清理残留 WebUI 时，使用更宽松的 500ms 超时，提升清理成功率
		client := &http.Client{Timeout: 500 * time.Millisecond}
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
		log.Println("[Main] Cleanup finished, goodbye!")
	})
}

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
