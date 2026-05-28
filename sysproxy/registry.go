package sysproxy

import (
	"errors"
	"log"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const REG_PROXY = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// 保留底层的 DLL 声明
var (
	modWininet         = windows.NewLazySystemDLL("wininet.dll")
	procInternetSetOpt = modWininet.NewProc("InternetSetOptionW")
)

type ConfigInterface interface {
	GetJsonConfig(key string) string
	SaveJsonConfig(key, value string)
	GetLastAppliedProxy() bool
	SetLastAppliedProxy(enable bool)
	SetProxyState(enable bool) // 若原有代码需要
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

// SetProxyRegistry 核心优化：增加严格的错误处理、日志输出与状态一致性校验
func (pm *ProxyManager) SetProxyRegistry(enable bool) {
	// 1. 避免无意义的重复设置
	if pm.cm.GetLastAppliedProxy() == enable {
		return
	}

	// 2. 打开注册表键
	key, err := registry.OpenKey(registry.CURRENT_USER, REG_PROXY, registry.SET_VALUE)
	if err != nil {
		log.Printf("[SysProxy] CRITICAL: Failed to open registry key: %v", err)
		return
	}
	defer key.Close()

	success := false

	if enable {
		port := pm.cm.GetJsonConfig("port")
		if port == "" || len(port) < 4 {
			port = "7890" // 默认 fallback
		}
		serverStr := "127.0.0.1:" + port

		// 尝试设置代理服务器和开启标志位
		errServer := key.SetStringValue("ProxyServer", serverStr)
		errEnable := key.SetDWordValue("ProxyEnable", 1)

		// 删除 PAC 配置，防止与系统代理冲突
		errPac := key.DeleteValue("AutoConfigURL")
		if errPac != nil && !errors.Is(errPac, syscall.ERROR_FILE_NOT_FOUND) {
			log.Printf("[SysProxy] Warning: Failed to clean AutoConfigURL: %v", errPac)
		}

		if errServer == nil && errEnable == nil {
			success = true
			log.Printf("[SysProxy] System proxy ENABLED on %s", serverStr)
		} else {
			log.Printf("[SysProxy] ERROR: Failed to enable proxy. ServerErr: %v, EnableErr: %v", errServer, errEnable)
			// 回滚机制：如果写入一半失败了，强制关闭代理
			_ = key.SetDWordValue("ProxyEnable", 0)
		}
	} else {
		// 关闭代理
		errEnable := key.SetDWordValue("ProxyEnable", 0)
		errServer := key.DeleteValue("ProxyServer")
		if errServer != nil && !errors.Is(errServer, syscall.ERROR_FILE_NOT_FOUND) {
			log.Printf("[SysProxy] Warning: Failed to clean ProxyServer: %v", errServer)
		}

		if errEnable == nil {
			success = true
			log.Printf("[SysProxy] System proxy DISABLED")
		} else {
			log.Printf("[SysProxy] ERROR: Failed to disable proxy: %v", errEnable)
		}
	}

	// 3. 核心修复：只有真实操作成功了，才去更新内存状态并通知 Windows
	if success {
		pm.cm.SetLastAppliedProxy(enable)
		// 通知 Windows 系统刷新代理设置（非常重要，否则有些应用不生效）
		if pm.win != nil {
			pm.win.RefreshInternetOptions()
		}
	}
}
