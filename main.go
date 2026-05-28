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

func initLog(baseDir string) {
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

	initLog(baseDir)

	configMgr := config.NewConfigManager(baseDir, exePath)
	log.Printf("[Main] Config initialized at: %s", baseDir)

	win32API := &sysproxy.Win32NotificationBridge{}
	proxyMgr := sysproxy.NewProxyManager(configMgr, win32API)

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
	defer kernelMgr.CloseJobObject()

	trayMgr := ui.NewTrayManager(configMgr, kernelMgr, proxyMgr)

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("[Main] Received interrupt signal, gracefully shutting down...")
		systray.Quit()
	}()

	go func() {
		time.Sleep(100 * time.Millisecond)
		go kernelMgr.MonitorKernelDaemon()
		go trayMgr.MonitorIconState()
		go trayMgr.WatchTunState()
	}()

	systray.Run(trayMgr.SetupTrayUI, func() {
		log.Println("[Main] Performing cleanup before exit...")
		configMgr.MarkAsExiting()

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
