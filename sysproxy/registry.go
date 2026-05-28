package sysproxy

import (
	"errors"
	"log"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const REG_PROXY = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

var (
	modWininet           = windows.NewLazySystemDLL("wininet.dll")
	procInternetSetOpt = modWininet.NewProc("InternetSetOptionW")
)

type ConfigInterface interface {
	GetJsonConfig(key string) string
	SaveJsonConfig(key, value string)
	GetLastAppliedProxy() bool
	SetLastAppliedProxy(enable bool)
	SetProxyState(enable bool)
	IsReallyExiting() bool
}

type Win32APIInterface interface {
	RefreshInternetOptions()
}

type ProxyManager struct {
	cm  ConfigInterface
	win Win32APIInterface
}

func NewProxyManager(cm ConfigInterface, win Win32APIInterface) *ProxyManager {
	return &ProxyManager{
		cm:  cm,
		win: win,
	}
}

func (pm *ProxyManager) SetProxyRegistry(enable bool) {
	if pm.cm.GetLastAppliedProxy() == enable {
		return
	}

	key, err := registry.OpenKey(registry.CURRENT_USER, REG_PROXY, registry.SET_VALUE)
	if err != nil {
		log.Printf("[SysProxy] Failed to open registry: %v", err)
		return
	}
	defer key.Close()

	success := false
	if enable {
		port := pm.cm.GetJsonConfig("port")
		if port == "" || len(port) < 4 {
			port = "7890"
		}
		
		_ = key.SetStringValue("ProxyServer", "127.0.0.1:"+port)
		_ = key.SetDWordValue("ProxyEnable", 1)
		
		errPac := key.DeleteValue("AutoConfigURL")
		if errPac != nil && !errors.Is(errPac, syscall.ERROR_FILE_NOT_FOUND) {
			log.Printf("[SysProxy] Warning: Failed to clean AutoConfigURL: %v", errPac)
		}
		success = true
	} else {
		_ = key.SetDWordValue("ProxyEnable", 0)
		errServer := key.DeleteValue("ProxyServer")
		if errServer != nil && !errors.Is(errServer, syscall.ERROR_FILE_NOT_FOUND) {
			log.Printf("[SysProxy] Warning: Failed to clean ProxyServer: %v", errServer)
		}
		success = true
	}

	if success {
		pm.cm.SetLastAppliedProxy(enable)
		pm.cm.SetProxyState(enable)

		if !pm.cm.IsReallyExiting() {
			pm.cm.SaveJsonConfig("proxy", strconv.FormatBool(enable))
		}

		if pm.win != nil {
			pm.win.RefreshInternetOptions()
		}
	}
}

type Win32NotificationBridge struct{}

func (w *Win32NotificationBridge) RefreshInternetOptions() {
	_, _, _ = procInternetSetOpt.Call(0, 37, 0, 0)
	_, _, _ = procInternetSetOpt.Call(0, 39, 0, 0)
}
