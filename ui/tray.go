package ui

import (
	"embed"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/energye/systray"
	"golang.org/x/sys/windows"

	"mihomo-tray/config"
	"mihomo-tray/kernel"
	"mihomo-tray/sysproxy"
	"mihomo-tray/winapi"
)

//go:embed icons/*.ico
var iconFs embed.FS

const (
	StateStop    = 0
	StateError   = 1
	StateTun     = 2
	StateProxy   = 3
	StateDefault = 4
)

type TrayManager struct {
	cm              *config.ConfigManager
	km              *kernel.KernelManager
	pm              *sysproxy.ProxyManager
	httpClient      *http.Client
	mTun            *systray.MenuItem
	mProxy          *systray.MenuItem
	mModes          map[string]*systray.MenuItem
	chromeDebugPort string
	debugPortMu     sync.Mutex
}

func NewTrayManager(cm *config.ConfigManager, km *kernel.KernelManager, pm *sysproxy.ProxyManager) *TrayManager {
	return &TrayManager{
		cm:     cm,
		km:     km,
		pm:     pm,
		mModes: make(map[string]*systray.MenuItem),
		httpClient: &http.Client{
			Timeout:   500 * time.Millisecond,
			Transport: &http.Transport{DisableKeepAlives: true},
		},
	}
}

func (tm *TrayManager) SetupTrayUI() {
	tm.UpdateIconByState(0)
	systray.SetTooltip("Mihomo-Tray")
	tm.cm.SetSystemInitializing(true)
	tm.cm.EnsureDefaultConfig()
	tm.cm.SniffAndSolidifyConfig()

	initProxyChecked := tm.cm.GetProxyState()
	initTunChecked := tm.cm.GetTunState()
	initModeChecked := tm.cm.GetCurrentModeState()

	tm.pm.SetProxyRegistry(initProxyChecked)

	systray.SetOnClick(func(menu systray.IMenu) {
		if tm.cm.IsSystemInitializing() {
			return
		}
		if !tm.cm.CheckAndThrottleClick(int64(1000 * time.Millisecond)) {
			return
		}
		go tm.LaunchWebUI()
	})

	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	mWeb.Click(func() {
		if !tm.cm.CheckAndThrottleClick(int64(1000 * time.Millisecond)) {
			return
		}
		go tm.LaunchWebUI()
	})

	systray.AddSeparator()

	tm.mProxy = systray.AddMenuItemCheckbox("系统代理", "", initProxyChecked)
	tm.mProxy.Click(func() {
		if !tm.cm.CheckAndThrottleClick(int64(200 * time.Millisecond)) {
			return
		}
		next := !tm.mProxy.Checked()
		if next {
			tm.mProxy.Check()
		} else {
			tm.mProxy.Uncheck()
		}

		tm.cm.SaveJsonConfig("proxy", strconv.FormatBool(next))
		go tm.pm.SetProxyRegistry(next)
	})

	tm.mTun = systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", initTunChecked)
	tm.mTun.Click(func() {
		if !tm.cm.CheckAndThrottleClick(int64(800 * time.Millisecond)) {
			return
		}
		next := !tm.mTun.Checked()
		if next {
			tm.mTun.Check()
		} else {
			tm.mTun.Uncheck()
		}
		go tm.SetTunMode(next)
	})

	systray.AddSeparator()

	mModeRoot := systray.AddMenuItem("模式切换", "")
	setupMode := func(key, label string) {
		tm.mModes[key] = mModeRoot.AddSubMenuItemCheckbox(label, "", initModeChecked == key)
		tm.mModes[key].Click(func() {
			if !tm.cm.CheckAndThrottleClick(int64(500 * time.Millisecond)) {
				return
			}
			for k, menu := range tm.mModes {
				if k == key {
					menu.Check()
				} else {
					menu.Uncheck()
				}
			}
			go tm.SetMihomoMode(key)
		})
	}
	setupMode("rule", "规则模式")
	setupMode("global", "全局模式")
	setupMode("direct", "直连模式")

	systray.AddSeparator()

	mDir := systray.AddMenuItem("打开目录", "")
	mDir.Click(func() {
		windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(tm.cm.BaseDir()), nil, nil, windows.SW_SHOWNORMAL)
	})

	mMoreRoot := systray.AddMenuItem("更多", "")
	mAuto := mMoreRoot.AddSubMenuItemCheckbox("开机启动", "", winapi.CheckAutoStartStatus())
	mAuto.Click(func() {
		if !tm.cm.CheckAndThrottleClick(int64(500 * time.Millisecond)) {
			return
		}
		next := !mAuto.Checked()
		if winapi.ToggleAutoStart(tm.cm.ExePath(), tm.cm.BaseDir(), next) {
			tm.cm.SaveJsonConfig("autostart", strconv.FormatBool(next))
			if next {
				mAuto.Check()
			} else {
				mAuto.Uncheck()
			}
		}
	})

	mRestart := mMoreRoot.AddSubMenuItem("重启内核", "")
	mRestart.Click(func() {
		if !tm.cm.CheckAndThrottleClick(int64(1000 * time.Millisecond)) {
			return
		}
		tm.cm.SetSystemInitializing(true)
		tm.cm.SetHasFirstSynced(false)

		go func() {
			tm.km.KillProcessByName("mihomo.exe")
			tm.cm.SniffAndSolidifyConfig()
		}()
	})

	mReload := mMoreRoot.AddSubMenuItem("重载配置文件", "")
	mReload.Click(func() {
		if !tm.cm.CheckAndThrottleClick(int64(1000 * time.Millisecond)) {
			return
		}
		tm.ReloadConfigFile()
	})

	mEditConfig := mMoreRoot.AddSubMenuItem("编辑 config.yaml", "")
	mEditConfig.Click(func() {
		if !tm.cm.CheckAndThrottleClick(int64(1000 * time.Millisecond)) {
			return
		}
		configPath := filepath.Join(tm.cm.BaseDir(), "config.yaml")
		windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(configPath), nil, nil, windows.SW_SHOWNORMAL)
	})

	systray.AddSeparator()

	mExit := systray.AddMenuItem("退出程序", "")
	mExit.Click(func() {
		tm.cm.MarkAsExiting()
		tm.CleanupWebUI()
		systray.Quit()
	})
}

func (tm *TrayManager) evaluateTargetState() int32 {
	if !tm.cm.IsKernelActive() {
		return StateStop
	}

	wantTun := tm.cm.GetTunState()
	wantProxy := tm.cm.GetProxyState()

	if !wantTun {
		tm.cm.SetTunRecoveryStart(time.Time{})
		if wantProxy {
			return StateProxy
		}
		return StateDefault
	}

	if tm.cm.IsTunAlive() {
		tm.cm.SetTunRecoveryStart(time.Time{})
		return StateTun
	}
	if time.Since(tm.cm.GetTunStartTime()) < 8*time.Second {
		return StateTun
	}

	recoveryStart := tm.cm.GetTunRecoveryStart()
	if recoveryStart.IsZero() {
		tm.cm.SetTunRecoveryStart(time.Now())
		if last := tm.cm.GetLastState(); last != -1 {
			return last
		}
		return StateTun
	}

	if time.Since(recoveryStart) < 3*time.Second {
		if last := tm.cm.GetLastState(); last != -1 {
			return last
		}
		return StateTun
	}

	return StateError
}

func (tm *TrayManager) MonitorIconState() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if tm.cm.IsReallyExiting() {
				return
			}

			if tm.mProxy != nil {
				proxyIsOn := tm.cm.GetProxyState()
				if proxyIsOn && !tm.mProxy.Checked() {
					tm.mProxy.Check()
				} else if !proxyIsOn && tm.mProxy.Checked() {
					tm.mProxy.Uncheck()
				}
			}

			targetState := tm.evaluateTargetState()

			if tm.cm.GetLastState() != targetState {
				tm.UpdateIconByState(int(targetState))
				tm.cm.SetLastState(targetState)
			}
		}
	}
}

func (tm *TrayManager) UpdateIconByState(state int) {
	files := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
	if state >= 0 && state < len(files) {
		if b, err := iconFs.ReadFile("icons/" + files[state]); err == nil {
			systray.SetIcon(b)
		}
	}
}
