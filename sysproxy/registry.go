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

		bypass := "<local>;localhost;127.*;10.*;192.168.*;172.16.*;172.17.*;172.18.*;172.19.*;172.20.*;172.21.*;172.22.*;172.23.*;172.24.*;172.25.*;172.26.*;172.27.*;172.28.*;172.29.*;172.30.*;172.31.*;169.254.*;::1"
		_ = key.SetStringValue("ProxyOverride", bypass)
		_ = key.DeleteValue("AutoConfigURL")

		pm.cm.SaveJsonConfig("proxy", "true")
	} else {
		_ = key.SetDWordValue("ProxyEnable", 0)
		_ = key.DeleteValue("ProxyServer")
		_ = key.DeleteValue("ProxyOverride")

		pm.cm.SaveJsonConfig("proxy", "false")
	}

	connKey, errConn := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings\Connections`, registry.SET_VALUE)
	if errConn == nil {
		_ = connKey.DeleteValue("DefaultConnectionSettings")
		_ = connKey.DeleteValue("SavedLegacySettings")
		connKey.Close()
	}

	_, _, _ = setOption.Call(0, INTERNET_OPTION_SETTINGS_CHANGED, 0, 0)
	_, _, _ = setOption.Call(0, INTERNET_OPTION_REFRESH, 0, 0)
}
