package sysproxy

import (
	"errors"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	REG_PROXY                        = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	INTERNET_OPTION_SETTINGS_CHANGED = 39
	INTERNET_OPTION_REFRESH          = 37
)

var (
	modWininet         = windows.NewLazySystemDLL("wininet.dll")
	procInternetSetOpt = modWininet.NewProc("InternetSetOptionW")
)

type ConfigInterface interface {
	GetJsonConfig(key string) string
	SaveJsonConfig(key, value string)
	GetLastAppliedProxy() bool
	SetLastAppliedProxy(enable bool)
	GetProxyState() bool
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
	key, err := registry.OpenKey(registry.CURRENT_USER, REG_PROXY, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return
	}
	defer key.Close()

	currentEnable, _, err := key.GetIntegerValue("ProxyEnable")
	realEnabled := (err == nil && currentEnable == 1)

	if pm.cm.GetLastAppliedProxy() == enable && realEnabled == enable {
		return
	}

	success := false

	if enable {
		port := pm.cm.GetJsonConfig("port")
		if port == "" || len(port) < 4 {
			port = "7890"
		}
		serverStr := "127.0.0.1:" + port

		errServer := key.SetStringValue("ProxyServer", serverStr)
		errEnable := key.SetDWordValue("ProxyEnable", 1)
		bypassStr := "<local>;localhost;127.*;10.*;100.64.*;169.254.*;172.16.*;192.168.*"
		errBypass := key.SetStringValue("ProxyOverride", bypassStr)

		errPac := key.DeleteValue("AutoConfigURL")
		if errPac != nil && !errors.Is(errPac, syscall.ERROR_FILE_NOT_FOUND) {
		}

		if errServer == nil && errEnable == nil && errBypass == nil {
			success = true
		} else {
			_ = key.SetDWordValue("ProxyEnable", 0)
		}
	} else {
		errEnable := key.SetDWordValue("ProxyEnable", 0)

		if errEnable == nil {
			success = true
		}
	}

	if success {
		pm.cm.SetLastAppliedProxy(enable)
		pm.cm.SetProxyState(enable)

		if !pm.cm.IsReallyExiting() {
			pm.cm.SaveJsonConfig("proxy", strconv.FormatBool(enable))
		}

		if pm.win != nil {
			go func() {
				pm.win.RefreshInternetOptions()
			}()
		}
	}
}

func (pm *ProxyManager) WatchProxyRegistry() {
	key, err := registry.OpenKey(registry.CURRENT_USER, REG_PROXY, registry.NOTIFY|registry.QUERY_VALUE)
	if err != nil {
		return
	}
	defer key.Close()

	for {
		if pm.cm.IsReallyExiting() {
			return
		}

		err := windows.RegNotifyChangeKeyValue(
			windows.Handle(key),
			false,
			windows.REG_NOTIFY_CHANGE_LAST_SET,
			0,
			false,
		)

		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		if pm.cm.IsReallyExiting() {
			return
		}

		expectedProxy := pm.cm.GetProxyState()

		if !expectedProxy {
			continue
		}
		
		val, _, err := key.GetIntegerValue("ProxyEnable")
		realProxy := (err == nil && val == 1)

		serverStr, _, errStr := key.GetStringValue("ProxyServer")
		expectedPort := pm.cm.GetJsonConfig("port")
		if expectedPort == "" || len(expectedPort) < 4 {
			expectedPort = "7890"
		}
		expectedServer := "127.0.0.1:" + expectedPort
		
		isPortHijacked := (errStr == nil && serverStr != expectedServer)

		if realProxy && isPortHijacked {
			pm.cm.SetLastAppliedProxy(false)
			pm.cm.SetProxyState(false)
			pm.cm.SaveJsonConfig("proxy", "false")
			continue
		}

		if !realProxy {
			time.Sleep(200 * time.Millisecond)
			pm.cm.SetLastAppliedProxy(false)
			pm.SetProxyRegistry(true)
		}
	}
}

type Win32NotificationBridge struct{}

func (w *Win32NotificationBridge) RefreshInternetOptions() {
	_, _, _ = procInternetSetOpt.Call(0, INTERNET_OPTION_SETTINGS_CHANGED, 0, 0)
	_, _, _ = procInternetSetOpt.Call(0, INTERNET_OPTION_REFRESH, 0, 0)
}
