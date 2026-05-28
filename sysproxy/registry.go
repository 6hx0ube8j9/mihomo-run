package sysproxy

import (
	"errors"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const REG_PROXY = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

var (
	modWininet         = windows.NewLazySystemDLL("wininet.dll")
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
		return
	}
	defer key.Close()

	if enable {
		if err := key.SetDWordValue("ProxyEnable", 1); err != nil {
			return
		}

		port := pm.cm.GetJsonConfig("port")
		if port == "" || len(port) < 4 {
			port = "7890"
		}
		
		if err := key.SetStringValue("ProxyServer", "127.0.0.1:"+port); err != nil {
			_ = key.SetDWordValue("ProxyEnable", 0)
			return
		}

		errDel := key.DeleteValue("AutoConfigURL")
		if errDel != nil && !errors.Is(errDel, syscall.ERROR_FILE_NOT_FOUND) {
			_ = key.SetDWordValue("ProxyEnable", 0)
			_ = key.DeleteValue("ProxyServer")
			return
		}
	} else {
		if err := key.SetDWordValue("ProxyEnable", 0); err != nil {
			return
		}
		
		errDelServer := key.DeleteValue("ProxyServer")
		if errDelServer != nil && !errors.Is(errDelServer, syscall.ERROR_FILE_NOT_FOUND) {
			_ = key.SetDWordValue("ProxyEnable", 1)
			return
		}
	}

	pm.cm.SetLastAppliedProxy(enable)
	pm.cm.SetProxyState(enable)

	if !pm.cm.IsReallyExiting() {
		pm.cm.SaveJsonConfig("proxy", strconv.FormatBool(enable))
	}

	go func() {
		pm.win.RefreshInternetOptions()
	}()
}

type Win32NotificationBridge struct{}

func (w *Win32NotificationBridge) RefreshInternetOptions() {
	_, _, _ = procInternetSetOpt.Call(0, 37, 0, 0)
	_, _, _ = procInternetSetOpt.Call(0, 39, 0, 0)
}
