package sysproxy

import (
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"mihomo-run/config"
)

var (
	wininet   = windows.NewLazySystemDLL("wininet.dll")
	setOption = wininet.NewProc("InternetSetOptionW")
)

const (
	INTERNET_OPTION_SETTINGS_CHANGED = 39
	INTERNET_OPTION_REFRESH          = 37
)

type ProxyManager struct {
	cm *config.ConfigManager
}

func NewProxyManager(cm *config.ConfigManager) *ProxyManager {
	return &ProxyManager{cm: cm}
}

func (pm *ProxyManager) SetProxyRegistry(enable bool) {
	if pm.cm.GetLastAppliedProxy() == enable {
		return
	}

	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer key.Close()

	if enable {
		port := pm.cm.GetJsonConfig("port")
		if port == "" || len(port) < 4 {
			port = "7890"
		}
		
		_ = key.SetDWordValue("ProxyEnable", 1)
		_ = key.SetStringValue("ProxyServer", "127.0.0.1:"+port)
		
		bypass := "<local>;localhost;127.*;10.*;172.16.*;192.168.*;::1"
		_ = key.SetStringValue("ProxyOverride", bypass)
		
		_ = key.DeleteValue("AutoConfigURL")

		if !pm.cm.IsReallyExiting() {
			pm.cm.SaveJsonConfig("proxy", "true")
		}
	} else {
		_ = key.SetDWordValue("ProxyEnable", 0)
		_ = key.DeleteValue("ProxyServer")
		_ = key.DeleteValue("ProxyOverride")

		if !pm.cm.IsReallyExiting() {
			pm.cm.SaveJsonConfig("proxy", "false")
		}
	}

	pm.cm.SetLastAppliedProxy(enable)

	_, _, _ = setOption.Call(0, INTERNET_OPTION_SETTINGS_CHANGED, 0, 0)
	_, _, _ = setOption.Call(0, INTERNET_OPTION_REFRESH, 0, 0)
}
