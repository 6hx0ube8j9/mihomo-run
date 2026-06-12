package ui

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/energye/systray"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"mihomo-run/config"
	"mihomo-run/kernel"
	"mihomo-run/sysproxy"
	"mihomo-run/winapi"
)

//go:embed icons/*.ico
var iconFs embed.FS

const (
	MaxBrowserFocusRetry  = 20
	BrowserFocusSleepTime = 50 * time.Millisecond
)

var tunKeywords = []string{"mihomo", "meta", "clash", "sing-box", "wintun"}

var bufPool = sync.Pool{
	New: func() interface{} {
		return bytes.NewBuffer(make([]byte, 0, 2048))
	},
}

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
	mModes          map[string]*systray.MenuItem // 用于毫秒级同步模式UI
	chromeDebugPort string
	debugPortMu     sync.Mutex
}

func NewTrayManager(cm *config.ConfigManager, km *kernel.KernelManager, pm *sysproxy.ProxyManager) *TrayManager {
	return &TrayManager{
		cm: cm,
		km: km,
		pm: pm,
		mModes: make(map[string]*systray.MenuItem),
		httpClient: &http.Client{
			Timeout:   500 * time.Millisecond,
			Transport: &http.Transport{DisableKeepAlives: true},
		},
	}
}

func getFreePort() string {
	addr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:0")
	if err != nil {
		return "52719"
	}
	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return "52719"
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	_ = l.Close()
	return port
}

func (tm *TrayManager) WatchTunState() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if tm.cm.IsReallyExiting() {
				return
			}
			if !tm.cm.IsKernelActive() || !tm.cm.GetTunState() {
				tm.cm.SetTunAlive(false)
				continue
			}

			currentHasTun := false
			ifaces, err := net.Interfaces()
			if err == nil {
				for _, i := range ifaces {
					if tm.IsTunInterfaceMatch(i.Name) {
						currentHasTun = true
						break
					}
				}
			}
			tm.cm.SetTunAlive(currentHasTun)
		}
	}
}

func (tm *TrayManager) updateModeMenu(mode string) {
	for k, m := range tm.mModes {
		if k == mode {
			m.Check()
		} else {
			m.Uncheck()
		}
	}
}

func (tm *TrayManager) WatchCoreAPI() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if tm.cm.IsReallyExiting() {
				return
			}
			if !tm.cm.IsKernelActive() || tm.cm.IsSystemInitializing() || tm.cm.IsSyncing() {
				continue
			}

			body, err := tm.DoAPIRequest("GET", "/configs", nil)
			if err != nil {
				continue
			}

			var currentConf struct {
				Tun struct {
					Enable bool `json:"enable"`
				} `json:"tun"`
				Mode string `json:"mode"`
			}
			if err := json.Unmarshal(body, &currentConf); err != nil {
				continue
			}

			targetTunInJson := tm.cm.GetTunState()
			realTunInConfig := tm.cm.GetJsonConfig("tun") == "true"

			if currentConf.Tun.Enable != targetTunInJson && currentConf.Tun.Enable != realTunInConfig {
				if currentConf.Tun.Enable {
					tm.cm.SetTunState(true)
					tm.cm.SaveJsonConfig("tun", "true")
					if tm.mTun != nil && !tm.mTun.Checked() {
						tm.mTun.Check()
					}
				} else {
					tm.cm.SetTunState(false)
					tm.cm.SaveJsonConfig("tun", "false")
					if tm.mTun != nil && tm.mTun.Checked() {
						tm.mTun.Uncheck()
					}
				}
			}

			targetModeInJson := tm.cm.GetCurrentModeState()
			realModeInConfig := tm.cm.GetJsonConfig("mode")
			if currentConf.Mode != "" && currentConf.Mode != targetModeInJson && currentConf.Mode != realModeInConfig {
				tm.cm.SetCurrentModeState(currentConf.Mode)
				tm.cm.SaveJsonConfig("mode", currentConf.Mode)
				tm.updateModeMenu(currentConf.Mode)
			}
		}
	}
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

func (tm *TrayManager) DoAPIRequest(method, path string, payload interface{}) ([]byte, error) {
	apiAddr := strings.TrimSuffix(tm.cm.GetJsonConfig("external-controller"), "/")
	if apiAddr == "" {
		return nil, fmt.Errorf("api address is empty")
	}
	if !strings.HasPrefix(apiAddr, "http") {
		apiAddr = "http://" + apiAddr
	}
	url := apiAddr + "/" + strings.TrimPrefix(path, "/")

	var bodyReader io.Reader
	var buf *bytes.Buffer

	if payload != nil {
		buf = bufPool.Get().(*bytes.Buffer)
		buf.Reset()
		if err := json.NewEncoder(buf).Encode(payload); err != nil {
			if buf.Cap() <= 65536 {
				bufPool.Put(buf)
			}
			return nil, err
		}
		bodyReader = buf
	} else if method == http.MethodPut || method == http.MethodPost || method == http.MethodPatch {
		bodyReader = bytes.NewReader([]byte("{}"))
	}

	defer func() {
		if buf != nil {
			if buf.Cap() <= 65536 {
				bufPool.Put(buf)
			}
		}
	}()

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}

	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if secret := tm.cm.GetJsonConfig("secret"); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}

	resp, err := tm.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent || resp.ContentLength == 0 {
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil, nil
		}
		return nil, fmt.Errorf("API Status Error: %d", resp.StatusCode)
	}

	limitReader := io.LimitReader(resp.Body, 10*1024*1024)
	body, err := io.ReadAll(limitReader)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, fmt.Errorf("API Error: %d, Response: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

func (tm *TrayManager) ReloadConfigFile() {
	tm.cm.SetSyncing(true)
	tm.cm.SetSystemInitializing(true)
	isProxyEnabled := tm.cm.GetProxyState()
	_, _ = tm.DoAPIRequest("PUT", "/configs?force=true", nil)
	tm.SniffAndSolidifyConfig()

	go func() {
		defer func() {
			tm.cm.SetSystemInitializing(false)
			tm.cm.SetSyncing(false)
		}()

		isTunOn := tm.cm.GetJsonConfig("tun") == "true"
		modeStr := tm.cm.GetJsonConfig("mode")

		tm.cm.SetTunState(isTunOn)
		tm.cm.SetCurrentModeState(modeStr)
		tm.updateModeMenu(modeStr)

		if isProxyEnabled {
			tm.cm.SetLastAppliedProxy(false)
			tm.pm.SetProxyRegistry(true)
		}

		tm.SyncConfigToKernel()
	}()
}

func (tm *TrayManager) SyncConfigToKernel() {
	if !tm.cm.TryStartSyncing() {
		return
	}
	defer func() {
		tm.cm.SetSyncing(false)
		tm.cm.SetSystemInitializing(false)
	}()

	tunEnabled := tm.cm.GetTunState()
	currentMode := tm.cm.GetCurrentModeState()

	payload := map[string]interface{}{
		"tun": map[string]bool{"enable": tunEnabled},
	}
	if tm.cm.IsSystemInitializing() {
		payload["mode"] = currentMode
	}

	for i := 0; i < 4; i++ {
		if _, err := tm.DoAPIRequest("PATCH", "/configs", payload); err == nil {
			break
		}
		time.Sleep(time.Duration(i+1) * 300 * time.Millisecond)
	}
}

func (tm *TrayManager) LaunchWebUI() {
	apiAddr := tm.cm.GetJsonConfig("external-controller")
	secret := tm.cm.GetJsonConfig("secret")
	proxyAddr := "127.0.0.1:" + tm.cm.GetJsonConfig("port")
	baseUI := strings.TrimRight(apiAddr, "/")
	if !strings.HasPrefix(baseUI, "http") {
		baseUI = "http://" + baseUI
	}
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(baseUI, "http://"), "https://"))
	if port == "" {
		host, port = "127.0.0.1", "9090"
	}
	uiHostPort := host + ":" + port
	finalURL := fmt.Sprintf("%s/ui/?hostname=%s&port=%s&secret=%s#/proxies", baseUI, host, port, secret)

	if hwnd := winapi.GetCachedWebUIHwnd(); hwnd != 0 {
		if winapi.IsWindowVisible(hwnd) {
			winapi.FocusWindowSilky(hwnd, tm.cm)
			return
		}
		winapi.SetCachedWebUIHwnd(0)
	}

	tm.debugPortMu.Lock()
	if tm.chromeDebugPort == "" {
		tm.chromeDebugPort = getFreePort()
	}
	safeDebugPort := tm.chromeDebugPort
	tm.debugPortMu.Unlock()

	client := &http.Client{
		Timeout:   300 * time.Millisecond,
		Transport: &http.Transport{DisableKeepAlives: true},
	}

	if resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/json", safeDebugPort)); err == nil {
		defer resp.Body.Close()
		var targets []map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&targets); err == nil {
			for _, t := range targets {
				pURL, _ := t["url"].(string)
				if strings.Contains(pURL, "/ui/") || strings.Contains(pURL, "setup") {
					id, _ := t["id"].(string)
					if actResp, actErr := client.Get(fmt.Sprintf("http://127.0.0.1:%s/json/activate/%s", safeDebugPort, id)); actErr == nil {
						_ = actResp.Body.Close()
					}
					go func() {
						for i := 0; i < MaxBrowserFocusRetry; i++ {
							if winapi.FindAndFocusChromeWindow(0, tm.cm) {
								break
							}
							time.Sleep(BrowserFocusSleepTime)
						}
					}()
					return
				}
			}
		}
	}

	var browserPath string
	potentialPaths := []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("LocalAppData"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `BraveSoftware\Brave-Browser\Application\brave.exe`),
		filepath.Join(os.Getenv("LocalAppData"), `BraveSoftware\Brave-Browser\Application\brave.exe`),
		filepath.Join(os.Getenv("LocalAppData"), `Vivaldi\Application\vivaldi.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Vivaldi\Application\vivaldi.exe`),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Vivaldi\Application\vivaldi.exe`),
	}
	for _, p := range potentialPaths {
		if _, err := os.Stat(p); err == nil {
			browserPath = p
			break
		}
	}

	if browserPath != "" {
		userDataDir := filepath.Join(tm.cm.BaseDir(), "webcache")
		_ = os.MkdirAll(userDataDir, 0755)

		scrW := winapi.GetSystemMetrics(0)
		scrH := winapi.GetSystemMetrics(1)
		winW, winH, winX, winY := winapi.CalculateWindowBounds(scrW, scrH)

		args := []string{
			"--app=" + finalURL,
			"--remote-debugging-port=" + safeDebugPort,
			"--user-data-dir=" + userDataDir,
			"--window-size=" + strconv.Itoa(winW) + "," + strconv.Itoa(winH),
			"--window-position=" + strconv.Itoa(winX) + "," + strconv.Itoa(winY),
			"--proxy-server=" + proxyAddr,
			"--no-first-run",
			"--no-default-browser-check",
		}
		cmd := exec.Command(browserPath, args...)
		if err := cmd.Start(); err == nil {
			mainPid := uint32(cmd.Process.Pid)
			go func() {
				for i := 0; i < MaxBrowserFocusRetry; i++ {
					if winapi.FindAndFocusChromeWindow(mainPid, tm.cm) {
						break
					}
					time.Sleep(BrowserFocusSleepTime)
				}
			}()

			// 启动坚固无依赖的 CDP 底层拦截器
			go func() {
				time.Sleep(1 * time.Second)
				resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/json", safeDebugPort))
				if err != nil {
					return
				}
				defer resp.Body.Close()
				var targets []map[string]interface{}
				if json.NewDecoder(resp.Body).Decode(&targets) == nil {
					for _, t := range targets {
						if t["type"] == "page" {
							if ws, ok := t["webSocketDebuggerUrl"].(string); ok {
								tm.startCDPMonitor(ws, uiHostPort)
								break
							}
						}
					}
				}
			}()
		}
	} else {
		_ = exec.Command("cmd", "/c", "start", "", finalURL).Start()
	}
}

func (tm *TrayManager) CleanupWebUI() {
	tm.debugPortMu.Lock()
	safeDebugPort := tm.chromeDebugPort
	tm.debugPortMu.Unlock()

	if safeDebugPort == "" {
		return
	}

	client := &http.Client{
		Timeout:   200 * time.Millisecond,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	apiURL := fmt.Sprintf("http://127.0.0.1:%s/json", safeDebugPort)
	if resp, err := client.Get(apiURL); err == nil {
		var targets []map[string]interface{}
		if json.NewDecoder(resp.Body).Decode(&targets) == nil {
			for _, t := range targets {
				if id, ok := t["id"].(string); ok {
					_, _ = client.Get(fmt.Sprintf("http://127.0.0.1:%s/json/close/%s", safeDebugPort, id))
				}
			}
		}
		_ = resp.Body.Close()
	}
}

func (tm *TrayManager) SetupTrayUI() {
	tm.UpdateIconByState(0)
	systray.SetTooltip("Mihomo-Run")
	tm.cm.SetSystemInitializing(true)
	tm.cm.EnsureDefaultConfig()
	tm.SniffAndSolidifyConfig()

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
			tm.updateModeMenu(key)
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
	mAuto := mMoreRoot.AddSubMenuItemCheckbox("开机启动", "", tm.CheckAutoStartStatus())
	mAuto.Click(func() {
		if !tm.cm.CheckAndThrottleClick(int64(500 * time.Millisecond)) {
			return
		}
		next := !mAuto.Checked()
		tm.ToggleAutoStart(next)
		if next {
			mAuto.Check()
		} else {
			mAuto.Uncheck()
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
			tm.SniffAndSolidifyConfig()
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

func (tm *TrayManager) SetMihomoMode(mode string) {
	tm.cm.SaveJsonConfig("mode", mode)
	tm.cm.SetCurrentModeState(mode)
	_, _ = tm.DoAPIRequest("PATCH", "/configs", map[string]string{"mode": mode})
}

func (tm *TrayManager) SetTunMode(enable bool) {
	newID := tm.cm.AddGlobalOpID()
	tm.cm.SetSystemInitializing(true)
	tm.cm.SaveJsonConfig("tun", strconv.FormatBool(enable))
	tm.cm.SetTunState(enable)

	go func(opID int32) {
		defer func() {
			if tm.cm.GetGlobalOpID() == opID {
				tm.cm.SetSystemInitializing(false)
			}
		}()

		_, err := tm.DoAPIRequest("PATCH", "/configs", map[string]interface{}{
			"tun": map[string]bool{"enable": enable},
		})
		if err != nil {
			return
		}

		for i := 0; i < 15; i++ {
			if tm.cm.GetGlobalOpID() != opID {
				return
			}
			found := false
			ifaces, _ := net.Interfaces()
			for _, iface := range ifaces {
				if tm.IsTunInterfaceMatch(iface.Name) {
					found = true
					break
				}
			}
			if found == enable {
				tm.cm.SetTunAlive(enable)
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}(newID)
}

func (tm *TrayManager) SniffAndSolidifyConfig() {
	configPath := filepath.Join(tm.cm.BaseDir(), "config.yaml")
	file, err := os.Open(configPath)
	if err != nil {
		return
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	inTunSection := false
	foundMixed := false

	for scanner.Scan() {
		line := scanner.Text()
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = line[:idx]
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "mixed-port:") {
			if parts := strings.SplitN(trimmed, ":", 2); len(parts) == 2 {
				if port := strings.Trim(parts[1], " \"'"); port != "" {
					tm.cm.SaveJsonConfig("port", port)
					foundMixed = true
				}
			}
			continue
		}
		if !foundMixed && strings.HasPrefix(trimmed, "port:") {
			if parts := strings.SplitN(trimmed, ":", 2); len(parts) == 2 {
				if port := strings.Trim(parts[1], " \"'"); port != "" {
					tm.cm.SaveJsonConfig("port", port)
				}
			}
			continue
		}
		if strings.HasPrefix(trimmed, "tun:") {
			inTunSection = true
			continue
		}
		if inTunSection && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inTunSection = false
		}
		if inTunSection && strings.Contains(trimmed, "device:") {
			if parts := strings.SplitN(trimmed, ":", 2); len(parts) == 2 {
				if devName := strings.Trim(parts[1], " \"'"); devName != "" {
					tm.cm.SaveJsonConfig("tun_device", devName)
				}
			}
			continue
		}
		if strings.HasPrefix(trimmed, "external-controller:") {
			addr := strings.Trim(strings.TrimPrefix(trimmed, "external-controller:"), " \"'")
			if strings.HasPrefix(addr, ":") {
				addr = "127.0.0.1" + addr
			}
			if addr != "" {
				if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
					addr = "http://" + addr
				}
				tm.cm.SaveJsonConfig("external-controller", addr)
			}
			continue
		}
		if strings.HasPrefix(trimmed, "secret:") {
			val := strings.Trim(strings.TrimPrefix(trimmed, "secret:"), " \"'")
			tm.cm.SaveJsonConfig("secret", val)
			continue
		}
	}
}

func (tm *TrayManager) ToggleAutoStart(enable bool) {
	const taskName = "MihomoRunTask"
	if key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE); err == nil {
		_ = key.DeleteValue("MihomoRun")
		key.Close()
	}
	success := false
	if enable {
		safeExePath := strings.ReplaceAll(tm.cm.ExePath(), "'", "''")
		safeBaseDir := strings.ReplaceAll(tm.cm.BaseDir(), "'", "''")

		psScript := fmt.Sprintf(
			`$trigger = New-ScheduledTaskTrigger -AtLogOn; $trigger.Delay = 'PT6S'; `+
				`$action = New-ScheduledTaskAction -Execute '%s' -Argument '---autostart' -WorkingDirectory '%s'; `+
				`$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero); `+
				`Register-ScheduledTask -TaskName '%s' -Trigger $trigger -Action $action -Settings $settings -RunLevel Highest -Force`,
			safeExePath, safeBaseDir, taskName,
		)
		uni := []rune(psScript)
		b := make([]byte, len(uni)*2)
		for i, v := range uni {
			b[i*2] = byte(v)
			b[i*2+1] = byte(v >> 8)
		}
		encodedScript := base64.StdEncoding.EncodeToString(b)
		cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodedScript)
		cmd.SysProcAttr = &windows.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
		if err := cmd.Run(); err == nil {
			success = true
		}
	} else {
		cmd := exec.Command("schtasks", "/Delete", "/TN", "\\"+taskName, "/F")
		cmd.SysProcAttr = &windows.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
		if err := cmd.Run(); err == nil || !tm.CheckAutoStartStatus() {
			success = true
		}
	}
	if success {
		tm.cm.SaveJsonConfig("autostart", strconv.FormatBool(enable))
	}
}

func (tm *TrayManager) CheckAutoStartStatus() bool {
	cmd := exec.Command("schtasks", "/Query", "/TN", "MihomoRunTask")
	cmd.SysProcAttr = &windows.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.Run() == nil
}

func (tm *TrayManager) UpdateIconByState(state int) {
	files := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
	if state >= 0 && state < len(files) {
		if b, err := iconFs.ReadFile("icons/" + files[state]); err == nil {
			systray.SetIcon(b)
		}
	}
}

func (tm *TrayManager) IsTunInterfaceMatch(ifaceName string) bool {
	name := strings.ToLower(ifaceName)
	target := strings.ToLower(tm.cm.GetJsonConfig("tun_device"))
	if target != "" && strings.Contains(name, target) {
		return true
	}
	for _, kw := range tunKeywords {
		if strings.Contains(name, kw) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------
// 坚固无依赖的 CDP 底层拦截器 (安全、零依赖、无端口冲突)
// ---------------------------------------------------------

func (tm *TrayManager) startCDPMonitor(wsURL, uiHostPort string) {
	address := strings.TrimPrefix(wsURL, "ws://")
	parts := strings.SplitN(address, "/", 2)
	host, path := parts[0], "/"+parts[1]

	conn, err := net.Dial("tcp", host)
	if err != nil {
		return
	}
	defer conn.Close()

	req := fmt.Sprintf(
		"GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n",
		path, host,
	)
	_, _ = conn.Write([]byte(req))

	reader := bufio.NewReader(conn)
	for {
		line, _, err := reader.ReadLine()
		if err != nil || len(line) == 0 {
			break
		}
	}

	msgID := 1
	sendCmd := func(method string, params interface{}) {
		reqMap := map[string]interface{}{"id": msgID, "method": method}
		if params != nil {
			reqMap["params"] = params
		}
		payload, _ := json.Marshal(reqMap)
		msgID++

		length := len(payload)
		var frame []byte
		if length <= 125 {
			frame = append(frame, 0x81, byte(length))
		} else if length <= 65535 {
			frame = append(frame, 0x81, 126, byte(length>>8), byte(length&0xFF))
		} else {
			frame = append(frame, 0x81, 127, 0, 0, 0, 0, byte(length>>24), byte(length>>16), byte(length>>8), byte(length&0xFF))
		}
		frame = append(frame, payload...)
		_, _ = conn.Write(frame)
	}

	// 1. 拦截下载  2. 监听外部链接  3. 监听 WebUI API 动作
	sendCmd("Browser.setDownloadBehavior", map[string]interface{}{"behavior": "deny"})
	sendCmd("Target.setDiscoverTargets", map[string]interface{}{"discover": true})
	sendCmd("Network.enable", nil)

	for {
		payload, opcode, err := readWSFrameSafe(reader)
		if err != nil {
			break
		}
		if opcode == 9 {
			_, _ = conn.Write([]byte{0x8a, 0x00}) // 自动响应 Ping 保持连接存活
			continue
		}
		if opcode != 1 {
			continue // 忽略非文本帧
		}

		var ev struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(payload, &ev)

		switch ev.Method {
		case "Browser.downloadWillBegin":
			var p struct{ URL string `json:"url"` }
			_ = json.Unmarshal(ev.Params, &p)
			if p.URL != "" {
				go exec.Command("cmd", "/c", "start", "", p.URL).Start()
			}

		case "Target.targetCreated", "Target.targetInfoChanged":
			var p struct {
				TargetInfo struct {
					TargetId string `json:"targetId"`
					Type     string `json:"type"`
					URL      string `json:"url"`
				} `json:"targetInfo"`
			}
			_ = json.Unmarshal(ev.Params, &p)
			ti := p.TargetInfo
			if ti.Type == "page" && ti.URL != "" && ti.URL != "about:blank" && !strings.HasPrefix(ti.URL, "devtools://") {
				if !strings.Contains(ti.URL, uiHostPort) {
					sendCmd("Target.closeTarget", map[string]interface{}{"targetId": ti.TargetId})
					go exec.Command("cmd", "/c", "start", "", ti.URL).Start()
				}
			}

		case "Network.requestWillBeSent":
			var p struct {
				Request struct {
					URL      string `json:"url"`
					Method   string `json:"method"`
					PostData string `json:"postData"`
				} `json:"request"`
			}
			_ = json.Unmarshal(ev.Params, &p)
			req := p.Request
			if req.Method == "PATCH" && strings.Contains(req.URL, "/configs") && req.PostData != "" {
				var data map[string]interface{}
				if err := json.Unmarshal([]byte(req.PostData), &data); err == nil {
					if mode, ok := data["mode"].(string); ok {
						tm.cm.SetCurrentModeState(mode)
						tm.cm.SaveJsonConfig("mode", mode)
						tm.updateModeMenu(mode) // 毫秒级同步UI菜单
					}
					if tunMap, ok := data["tun"].(map[string]interface{}); ok {
						if enable, ok := tunMap["enable"].(bool); ok {
							tm.cm.SetTunState(enable)
							tm.cm.SaveJsonConfig("tun", strconv.FormatBool(enable))
							if tm.mTun != nil {
								if enable {
									tm.mTun.Check()
								} else {
									tm.mTun.Uncheck()
								}
							}
						}
					}
				}
			}
		}
	}
}

// 严谨的安全 WebSocket 数据帧读取器 (解决长文本分片与越界崩溃)
func readWSFrameSafe(r *bufio.Reader) ([]byte, int, error) {
	b0, err := r.ReadByte()
	if err != nil {
		return nil, 0, err
	}
	opcode := int(b0 & 0x0F)

	b1, err := r.ReadByte()
	if err != nil {
		return nil, 0, err
	}
	masked := b1&0x80 != 0
	payloadLen := int(b1 & 0x7F)

	if payloadLen == 126 {
		buf := make([]byte, 2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, 0, err
		}
		payloadLen = int(buf[0])<<8 | int(buf[1])
	} else if payloadLen == 127 {
		buf := make([]byte, 8)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, 0, err
		}
		payloadLen = int(buf[4])<<24 | int(buf[5])<<16 | int(buf[6])<<8 | int(buf[7])
	}

	var maskKey []byte
	if masked {
		maskKey = make([]byte, 4)
		if _, err := io.ReadFull(r, maskKey); err != nil {
			return nil, 0, err
		}
	}

	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, 0, err
	}

	if masked {
		for i := 0; i < payloadLen; i++ {
			payload[i] ^= maskKey[i%4]
		}
	}
	return payload, opcode, nil
}
