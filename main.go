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

func initLog() {
	// 创建或追加到 debug.log
	f, err := os.OpenFile("debug.log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		return
	}
	log.SetOutput(f)
	log.Println("========================================")
	log.Println("程序已启动: " + time.Now().Format("2006-01-02 15:04:05"))
	log.Println("========================================")
}

func main() {
	exePath, err := os.Executable()
	if err != nil {
		log.Fatalf("[Main] Failed to get executable path: %v", err)
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

	// 3. 核心组件初始化 (按严格依赖顺序)
	configMgr := config.NewConfigManager(baseDir, exePath)
	log.Printf("[Main] Config dir: %s", baseDir)
	
	kernelHooks := kernel.KernelHooks{
		OnKernelStarted: func() {
			log.Println("[Main] Kernel started successfully.")
		},
	}
	kernelMgr := kernel.NewKernelManager(configMgr, kernelHooks)
	
	// 核心修复：激活 Job Object！防止 mihomo.exe 变成孤儿/僵尸进程
	kernelMgr.InitJobObject()

	proxyMgr := sysproxy.NewProxyManager(configMgr, nil)
	trayMgr := ui.NewTrayManager(configMgr, kernelMgr, proxyMgr)

	// 4. 监听系统中断信号 (比如在终端按 Ctrl+C，或者被系统要求结束)
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("[Main] Received interrupt signal, gracefully shutting down...")
		systray.Quit() // 触发下方 Run 的 onExit 回调
	}()

	// 5. 极速启动后台事件监听协程
	go func() {
		// 仅给 systray 极短的内存分配时间，直接启动
		time.Sleep(100 * time.Millisecond)
		go kernelMgr.MonitorKernelDaemon()
		go trayMgr.MonitorIconState()
		go trayMgr.WatchTunState()
	}()

	// 6. 运行托盘 UI (阻塞主线程，直到程序退出)
	systray.Run(trayMgr.SetupTrayUI, func() {
		log.Println("[Main] Performing cleanup before exit...")
		
		// 标记程序正在退出，通知所有组件立刻结束轮询和等待
		configMgr.MarkAsExiting()

		// 优雅清理 WebUI 残留的调试窗口
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

		// 确保退出时系统代理已关闭，防止用户断网
		proxyMgr.SetProxyRegistry(false)
		
		// 稍微留一点时间让底层的 cleanup 执行完毕
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
