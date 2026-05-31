package sysproxy

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"

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
	if enable {
		port := pm.cm.GetJsonConfig("port")
		if port == "" || len(port) < 4 {
			port = "7890"
		}
		server := "127.0.0.1:" + port
		bypass := "<local>;localhost;127.*;10.*;192.168.*;172.16.*;172.17.*;172.18.*;172.19.*;172.20.*;172.21.*;172.22.*;172.23.*;172.24.*;172.25.*;172.26.*;172.27.*;172.28.*;172.29.*;172.30.*;172.31.*;169.254.*;::1"

		setNativeProxy(server, bypass)
		
		pm.cm.SaveJsonConfig("proxy", "true")
	} else {
		clearNativeProxy()
		
		pm.cm.SaveJsonConfig("proxy", "false")
	}
}

// setNativeProxy 纯正原生 API，自动同步系统底层缓存与外部 UI
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
		pszConnection: nil, // nil 代表全局 LAN 设置
		dwOptionCount: 3,
		dwOptionError: 0,
		pOptions:      &options[0],
	}

	// 核心调用：更新系统代理状态
	_, _, _ = setOption.Call(
		0,
		INTERNET_OPTION_PER_CONNECTION_OPTION,
		uintptr(unsafe.Pointer(&list)),
		uintptr(unsafe.Sizeof(list)),
	)

	// 发出全局广播，让系统立刻生效
	_, _, _ = setOption.Call(0, INTERNET_OPTION_SETTINGS_CHANGED, 0, 0)
	_, _, _ = setOption.Call(0, INTERNET_OPTION_REFRESH, 0, 0)

	// 防止 Go 极速垃圾回收提前释放指针
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
