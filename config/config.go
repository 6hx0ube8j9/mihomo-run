package main

import (
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

const (
	APP_MUTEX     = "Mihomo_Unique_Mutex"
	SHOW_UI_EVENT = "Mihomo_Unique_Mutex_ShowUI"
)

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
		
		eName, _ := windows.UTF16PtrFromString(SHOW_UI_EVENT)
		hEvent, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, eName)
		if err == nil && hEvent != 0 {
			windows.SetEvent(hEvent)
			windows.CloseHandle(hEvent)
		}
		return
	}
	defer windows.CloseHandle(hM)

	eName, _ := windows.UTF16PtrFromString(SHOW_UI_EVENT)
	hShowUIEvent, _ := windows.CreateEvent(nil, false, false, eName)
	if hShowUIEvent != 0 {
		defer windows.CloseHandle(hShowUIEvent)
	}

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
	win32API := &sysproxy.Win32NotificationBridge{}
	proxyMgr := sysproxy.NewProxyManager(configMgr, win32API)

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
		go trayMgr.WatchCoreAPI()
		go proxyMgr.WatchProxyRegistry()

		if hShowUIEvent != 0 {
			go func() {
				for {
					s, _ := windows.WaitForSingleObject(hShowUIEvent, windows.INFINITE)
					if s == windows.WAIT_OBJECT_0 {
						if configMgr.CheckAndThrottleClick(int64(1000 * time.Millisecond)) {
							go trayMgr.LaunchWebUI()
						}
					} else {
						break
					}
				}
			}()
		}
	}()

	systray.Run(func() {
		trayMgr.SetupTrayUI()
		close(uiReady)
	}, func() {
		configMgr.MarkAsExiting()
		trayMgr.CleanupWebUI()
		proxyMgr.SetProxyRegistry(false)
		systray.Quit()
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
