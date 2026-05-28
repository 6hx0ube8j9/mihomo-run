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

		// 【优化吸收】：更严谨的操作，避免写一半失败导致系统代理崩溃
		errServer := key.SetStringValue("ProxyServer", serverStr)
		errEnable := key.SetDWordValue("ProxyEnable", 1)

		// 删除 PAC 配置，防止与系统全局代理冲突
		errPac := key.DeleteValue("AutoConfigURL")
		if errPac != nil && !errors.Is(errPac, syscall.ERROR_FILE_NOT_FOUND) {
			// 仅做静默处理，不中断主流程
		}

		if errServer == nil && errEnable == nil {
			success = true
		} else {
			// 【优化吸收】：回滚机制 - 如果设置服务器失败，则强行关闭代理，防止网络断网
			_ = key.SetDWordValue("ProxyEnable", 0)
		}
	} else {
		errEnable := key.SetDWordValue("ProxyEnable", 0)
		errServer := key.DeleteValue("ProxyServer")
		if errServer != nil && !errors.Is(errServer, syscall.ERROR_FILE_NOT_FOUND) {
			// 仅做静默处理
		}

		if errEnable == nil {
			success = true
		}
	}

	// 【核心修复】：只有当注册表真实操作成功后，才更新内存和保存配置，防止状态不一致
	if success {
		pm.cm.SetLastAppliedProxy(enable)
		
		// 找回被优化版弄丢的接口调用
		pm.cm.SetProxyState(enable)

		// 找回被优化版弄丢的本地配置保存，并且保留原版判断是否正在退出的安全机制
		if !pm.cm.IsReallyExiting() {
			pm.cm.SaveJsonConfig("proxy", strconv.FormatBool(enable))
		}

		// 找回原版的并发通知机制，防止 Windows API 拥塞导致 UI 假死
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
