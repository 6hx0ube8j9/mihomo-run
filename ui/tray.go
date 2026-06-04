package ui

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
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

const debugPort = "52719"

var tunKeywords = []string{"mihomo", "meta", "clash", "sing-box", "wintun"}

var bufPool = sync.Pool{
	New: func() interface{} {
		return bytes.NewBuffer(make([]byte, 0, 2048))
	},
}

// 定义状态机常量
const (
	StateStop    = 0
	StateError   = 1
	StateTun     = 2
	StateProxy   = 3
	StateDefault = 4
)

type TrayManager struct {
	cm         *config.ConfigManager
	km         *kernel.KernelManager
	pm         *sysproxy.ProxyManager
	httpClient *http.Client
	mTun       *systray.MenuItem
	mProxy     *systray.MenuItem
}

func NewTrayManager(cm *config.ConfigManager, km *kernel.KernelManager, pm *sysproxy.ProxyManager) *TrayManager {
	return &TrayManager{
		cm:         cm,
		km:         km,
		pm:         pm,
		httpClient: &http.Client{Timeout: 500 * time.Millisecond},
	}
}

func (tm *TrayManager) WatchTunState() {
	var failCount int

	for {
		if tm.cm.IsReallyExiting() {
			return
		}

		if !tm.cm.IsKernelActive() || !tm.cm.GetTunState() {
			tm.cm.UpdateTunAliveStatus(false)
			failCount = 0 
			time.Sleep(1 * time.Second)
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
		
		tm.cm.UpdateTunAliveStatus(currentHasTun)
		sleepDuration := 1 * time.Second
		
		if !currentHasTun {
			failCount++
			if failCount > 5 {
				sleepDuration = 3 * time.Second
			}
		} else {
			failCount = 0 
		}

		time.Sleep(sleepDuration)
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
			}
		}
	}
}

func (tm *TrayManager) evaluateTargetState() int32 {
	if !tm.cm.IsKernelActive() {
		return StateStop
	}

	wantTun := tm.cm.GetJsonConfig("tun") == "true"
	wantProxy := tm.cm.GetJsonConfig("proxy") == "true"

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
			bufPool.Put(buf)
			return nil, err
		}
		bodyReader = buf
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		if buf != nil {
			bufPool.Put(buf)
		}
		return nil, err
	}

	secret := tm.cm.GetJsonConfig("secret")
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}

	resp, err := tm.httpClient.Do(req)
	
	if buf != nil {
		bufPool.Put(buf)
	}

	if err != nil {
		return nil, err
	}
	
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("API error: %s", string(respBody))
	}

	return respBody, nil
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

		if isProxyEnabled {
			tm.cm.SetLastAppliedProxy(false)
			tm.pm.SetProxyRegistry(true)
		}

		tm.SyncConfigToKernel()
	}()
}

func (tm *TrayManager) SyncConfigToKernel() {
	if !tm.cm.CompareAndSwapSyncing(0, 1) {
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
	finalURL := fmt.Sprintf("%s/ui/?hostname=%s&port=%s&secret=%s#/proxies", baseUI, host, port, secret)

	if hwnd := winapi.GetCachedWebUIHwnd(); hwnd != 0 {
		if winapi.IsWindowVisible(hwnd) {
			winapi.FocusWindowSilky(hwnd, tm.cm)
			return
		}
		winapi.SetCachedWebUIHwnd(0)
	}

	client := &http.Client{Timeout: 300 * time.Millisecond}
	isPortOccupied := false

	if resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/json", debugPort)); err == nil {
		isPortOccupied = true
		defer resp.Body.Close()
		var targets []map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&targets); err == nil {
			for _, t := range targets {
				pURL, _ := t["url"].(string)
				if strings.Contains(pURL, "/ui/") || strings.Contains(pURL, "setup") {
					id, _ := t["id"].(string)

					if actResp, actErr := client.Get(fmt.Sprintf("http://127.0.0.1:%s/json/activate/%s", debugPort, id)); actErr == nil {
						_ = actResp.Body.Close()
					}

					go func() {
						for i := 0; i < 20; i++ {
							if winapi.FindAndFocusChromeWindow(0, tm.cm) {
								break
							}
							time.Sleep(50 * time.Millisecond)
						}
					}()
					return
				}
			}
		}
	} else {
		conn, dialErr := net.DialTimeout("tcp", "127.0.0.1:"+debugPort, 50*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			isPortOccupied = true
		}
	}

	if isPortOccupied {
		killCmd := fmt.Sprintf("for /f \"tokens=5\" %%a in ('netstat -aon ^| findstr :%s ^| findstr LISTENING') do taskkill /F /PID %%a", debugPort)
		cmd := exec.Command("cmd", "/c", killCmd)
		cmd.SysProcAttr = &windows.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
		_ = cmd.Run()
		time.Sleep(150 * time.Millisecond)
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
			"--remote-debugging-port=" + debugPort,
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
				for i := 0; i < 20; i++ {
					if winapi.FindAndFocusChromeWindow(mainPid, tm.cm) {
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
			}()
		}
	} else {
		_ = exec.Command("cmd", "/c", "start", "", finalURL).Start()
	}
}

func (tm *TrayManager) CleanupWebUI() {
	client := &http.Client{Timeout: 200 * time.Millisecond}
	apiURL := fmt.Sprintf("http://127.0.0.1:%s/json", debugPort)
	if resp, err := client.Get(apiURL); err == nil {
		var targets []map[string]interface{}
		if json.NewDecoder(resp.Body).Decode(&targets) == nil {
			for _, t := range targets {
				if id, ok := t["id"].(string); ok {
					_, _ = client.Get(fmt.Sprintf("http://127.0.0.1:%s/json/close/%s", debugPort, id))
				}
			}
		}
		_ = resp.Body.Close()
	}
}

func (tm *TrayManager) SetupTrayUI() {
	tm.UpdateIconByState(0)
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
		tm.cm.SetProxyState(next)
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
	modeMenus := make(map[string]*systray.MenuItem)
	setupMode := func(key, label string) {
		modeMenus[key] = mModeRoot.AddSubMenuItemCheckbox(label, "", initModeChecked == key)
		modeMenus[key].Click(func() {
			if !tm.cm.CheckAndThrottleClick(int64(500 * time.Millisecond)) {
				return
			}
			for k, menu := range modeMenus {
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
	mAuto := mMoreRoot.AddSubMenuItemCheckbox("开机自启动", "", tm.CheckAutoStartStatus())
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
				// 替换为新的状态汇报方法
				tm.cm.UpdateTunAliveStatus(enable)
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
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
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
			log.Println("[UI] AutoStart enabled successfully.")
		} else {
			log.Printf("[UI] Failed to enable AutoStart: %v\n", err)
		}
	} else {
		cmd := exec.Command("schtasks", "/Delete", "/TN", "\\"+taskName, "/F")
		cmd.SysProcAttr = &windows.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
		if err := cmd.Run(); err == nil || !tm.CheckAutoStartStatus() {
			success = true
			log.Println("[UI] AutoStart disabled successfully.")
		} else {
			log.Printf("[UI] Failed to disable AutoStart: %v\n", err)
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
