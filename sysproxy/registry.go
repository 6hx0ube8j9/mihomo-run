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

	success := false

	if enable {
		port := pm.cm.GetJsonConfig("port")
		if port == "" || len(port) < 4 {
			port = "7890"
		}
		serverStr := "127.0.0.1:" + port

		// 严格模式：捕获每一步的写入结果
		errServer := key.SetStringValue("ProxyServer", serverStr)
		errEnable := key.SetDWordValue("ProxyEnable", 1)

		// 删除 PAC 配置，防止与系统全局代理冲突（静默处理错误）
		errPac := key.DeleteValue("AutoConfigURL")
		if errPac != nil && !errors.Is(errPac, syscall.ERROR_FILE_NOT_FOUND) {
			// Do nothing
		}

		if errServer == nil && errEnable == nil {
			success = true
		} else {
			// 写入失败的回滚机制，防止断网
			_ = key.SetDWordValue("ProxyEnable", 0)
		}
	} else {
		errEnable := key.SetDWordValue("ProxyEnable", 0)
		errServer := key.DeleteValue("ProxyServer")
		if errServer != nil && !errors.Is(errServer, syscall.ERROR_FILE_NOT_FOUND) {
			// Do nothing
		}

		if errEnable == nil {
			success = true
		}
	}

	// 核心安全逻辑：仅在底层注册表真实修改成功后，才更新业务状态
	if success {
		pm.cm.SetLastAppliedProxy(enable)
		pm.cm.SetProxyState(enable)

		if !pm.cm.IsReallyExiting() {
			pm.cm.SaveJsonConfig("proxy", strconv.FormatBool(enable))
		}

		// 异步通知 Windows 刷新网络设置，防止阻塞主线程
		if pm.win != nil {
			go func() {
				pm.win.RefreshInternetOptions()
			}()
		}
	}
}

type Win32NotificationBridge struct{}

func (w *Win32NotificationBridge) RefreshInternetOptions() {
	_, _, _ = procInternetSetOpt.Call(0, 37, 0, 0)
	_, _, _ = procInternetSetOpt.Call(0, 39, 0, 0)
}
