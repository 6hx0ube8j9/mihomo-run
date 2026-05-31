package sysproxy

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"mihomo-run/config"
)

var (
	wininet   = windows.NewLazySystemDLL("wininet.dll")
	setOption = wininet.NewProc("InternetSetOptionW")
)

const (
	INTERNET_OPTION_PER_CONNECTION_OPTION = 75
	INTERNET_OPTION_SETTINGS_CHANGED      = 39
	INTERNET_OPTION_REFRESH               = 37

	PROXY_TYPE_DIRECT = 1
	PROXY_TYPE_PROXY  = 2

	INTERNET_PER_CONN_FLAGS        = 1
	INTERNET_PER_CONN_PROXY_SERVER = 2
	INTERNET_PER_CONN_PROXY_BYPASS = 3
)

type internetPerConnOption struct {
	dwOption uint32
	Value    uintptr
}

type internetPerConnOptionList struct {
	dwSize        uint32
	pszConnection *uint16
	dwOptionCount uint32
	dwOptionError uint32
	pOptions      *internetPerConnOption
}

type ProxyManager struct {
	cm *config.ConfigManager
}

func NewProxyManager(cm *config.ConfigManager) *ProxyManager {
	return &ProxyManager{cm: cm}
}

func (pm *ProxyManager) SetProxyRegistry(enable bool) {
	// 1. 打开注册表（用来同步 Windows 设置面板的 UI）
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
		server := "127.0.0.1:" + port
		bypass := "<local>;localhost;127.*;10.*;192.168.*;172.16.*;172.17.*;172.18.*;172.19.*;172.20.*;172.21.*;172.22.*;172.23.*;172.24.*;172.25.*;172.26.*;172.27.*;172.28.*;172.29.*;172.30.*;172.31.*;169.254.*;::1"
		
		_ = key.SetDWordValue("ProxyEnable", 1)
		_ = key.SetStringValue("ProxyServer", server)
		_ = key.SetStringValue("ProxyOverride", bypass)
		_ = key.DeleteValue("AutoConfigURL")

		setNativeProxy(server, bypass)

		pm.cm.SaveJsonConfig("proxy", "true")
	} else {
		// 动作 A：修改注册表，让 Windows 设置里的开关变灰（关闭）
		_ = key.SetDWordValue("ProxyEnable", 0)
		_ = key.DeleteValue("ProxyServer")
		_ = key.DeleteValue("ProxyOverride")

		// 动作 B：调用原生 API，底层恢复直连
		clearNativeProxy()

		pm.cm.SaveJsonConfig("proxy", "false")
	}
}

// setNativeProxy 纯正原生 API，负责搞定底层的 UWP 流量和缓存
func setNativeProxy(server, bypass string) {
	options := make([]internetPerConnOption, 3)
	options[0].dwOption = INTERNET_PER_CONN_FLAGS
	options[0].Value = uintptr(PROXY_TYPE_PROXY | PROXY_TYPE_DIRECT)

	serverPtr, _ := windows.UTF16PtrFromString(server)
	options[1].dwOption = INTERNET_PER_CONN_PROXY_SERVER
	options[1].Value = uintptr(unsafe.Pointer(serverPtr))

	bypassPtr, _ := windows.UTF16PtrFromString(bypass)
	options[2].dwOption = INTERNET_PER_CONN_PROXY_BYPASS
	options[2].Value = uintptr(unsafe.Pointer(bypassPtr))

	list := internetPerConnOptionList{
		dwSize:        uint32(unsafe.Sizeof(internetPerConnOptionList{})),
		pszConnection: nil, 
		dwOptionCount: 3,
		dwOptionError: 0,
		pOptions:      &options[0],
	}

	// 执行 Option 75 覆写
	_, _, _ = setOption.Call(
		0,
		INTERNET_OPTION_PER_CONNECTION_OPTION,
		uintptr(unsafe.Pointer(&list)),
		uintptr(unsafe.Sizeof(list)),
	)

	// 全局广播通知
	_, _, _ = setOption.Call(0, INTERNET_OPTION_SETTINGS_CHANGED, 0, 0)
	_, _, _ = setOption.Call(0, INTERNET_OPTION_REFRESH, 0, 0)

	runtime.KeepAlive(serverPtr)
	runtime.KeepAlive(bypassPtr)
	runtime.KeepAlive(options)
}

func clearNativeProxy() {
	options := make([]internetPerConnOption, 1)
	options[0].dwOption = INTERNET_PER_CONN_FLAGS
	options[0].Value = uintptr(PROXY_TYPE_DIRECT)

	list := internetPerConnOptionList{
		dwSize:        uint32(unsafe.Sizeof(internetPerConnOptionList{})),
		pszConnection: nil,
		dwOptionCount: 1,
		dwOptionError: 0,
		pOptions:      &options[0],
	}

	_, _, _ = setOption.Call(
		0,
		INTERNET_OPTION_PER_CONNECTION_OPTION,
		uintptr(unsafe.Pointer(&list)),
		uintptr(unsafe.Sizeof(list)),
	)

	_, _, _ = setOption.Call(0, INTERNET_OPTION_SETTINGS_CHANGED, 0, 0)
	_, _, _ = setOption.Call(0, INTERNET_OPTION_REFRESH, 0, 0)

	runtime.KeepAlive(options)
}
