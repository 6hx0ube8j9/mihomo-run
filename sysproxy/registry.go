package sysproxy

import (
	"strconv"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"mihomo-run/config"
)

var (
	wininet   = windows.NewLazySystemDLL("wininet.dll")
	setOption = wininet.NewProc("InternetSetOptionW")
)

type Win32NotificationBridge struct{}

func (b *Win32NotificationBridge) Refresh() {
	_, _, _ = setOption.Call(0, 37, 0, 0)
	_, _, _ = setOption.Call(0, 39, 0, 0)
}

type ProxyManager struct {
	cm *config.ConfigManager
	nb *Win32NotificationBridge
}

func NewProxyManager(cm *config.ConfigManager, nb *Win32NotificationBridge) *ProxyManager {
	return &ProxyManager{
		cm: cm,
		nb: nb,
	}
}

func (pm *ProxyManager) SetProxyRegistry(enable bool) {
	if pm.cm.IsProxyWriting() {
		return
	}
	pm.cm.SetProxyWriting(true)
	defer pm.cm.SetProxyWriting(false)

	if pm.cm.GetLastAppliedProxy() == enable {
		return
	}

	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()

	if enable {
		port := pm.cm.GetJsonConfig("port")
		if port == "" {
			port = "7890"
		}
		_ = k.SetDWordValue("ProxyEnable", 1)
		_ = k.SetStringValue("ProxyServer", "127.0.0.1:"+port)
		_ = k.SetStringValue("ProxyOverride", "<local>;localhost;127.*;10.*;172.16.*;172.17.*;172.18.*;172.19.*;172.20.*;172.21.*;172.22.*;172.23.*;172.24.*;172.25.*;172.26.*;172.27.*;172.28.*;172.29.*;172.30.*;172.31.*;192.168.*")
	} else {
		_ = k.SetDWordValue("ProxyEnable", 0)
	}

	pm.nb.Refresh()
	pm.cm.SetLastAppliedProxy(enable)
}

func (pm *ProxyManager) WatchProxyRegistry() {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.NOTIFY|registry.QUERY_VALUE)
	if err != nil {
		return
	}
	defer k.Close()

	for {
		if pm.cm.IsReallyExiting() {
			return
		}

		event, _ := windows.CreateEvent(nil, 0, 0, nil)
		err = windows.RegNotifyChangeKeyValue(windows.Handle(k), false, windows.REG_NOTIFY_CHANGE_LAST_SET, event, true)
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}

		s, _ := windows.WaitForSingleObject(event, 2000)
		windows.CloseHandle(event)

		if pm.cm.IsReallyExiting() {
			return
		}

		if s == windows.WAIT_OBJECT_0 {
			if pm.cm.IsProxyWriting() || pm.cm.IsSystemInitializing() {
				continue
			}

			val, _, err := k.GetIntegerValue("ProxyEnable")
			if err == nil {
				isRegEnabled := val == 1
				if isRegEnabled != pm.cm.GetProxyState() {
					pm.cm.SetProxyState(isRegEnabled)
					pm.cm.SaveJsonConfig("proxy", strconv.FormatBool(isRegEnabled))
					pm.cm.SetLastAppliedProxy(isRegEnabled)
				}
			}
		}
	}
}
