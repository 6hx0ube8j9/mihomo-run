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
		return
	}
	defer windows.CloseHandle(hM)

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

	configMgr := config.NewConfigManager(baseDir, exePath)
	proxyMgr := sysproxy.NewProxyManager(configMgr)

	kernelHooks := kernel.KernelHooks{
		OnKernelStarted: func() {
			trayTemp := ui.NewTrayManager(configMgr, nil, nil)
			trayTemp.SniffAndSolidifyConfig()
		},
		OnKernelReady: func() {
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
		systray.Quit()
	}()

	uiReady := make(chan struct{})

	go func() {
		<-uiReady
		go kernelMgr.MonitorKernelDaemon()
		go trayMgr.MonitorIconState()
		go trayMgr.WatchTunState()
	}()

	systray.Run(func() {
		trayMgr.SetupTrayUI()
		close(uiReady)
	}, func() {
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
