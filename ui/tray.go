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

const debugPort = "52719"

var tunKeywords = []string{"mihomo", "meta", "clash", "sing-box", "wintun"}

// TrayManager 負責托盤 UI 的佈局、事件綁定與背景定時重新整理工作
type TrayManager struct {
	cm         *config.ConfigManager
	km         *kernel.KernelManager
	pm         *sysproxy.ProxyManager
	httpClient *http.Client
	bufPool    sync.Pool
	mTun       *systray.MenuItem
}

// NewTrayManager 初始化托盤 UI 管理器
func NewTrayManager(cm *config.ConfigManager, km *kernel.KernelManager, pm *sysproxy.ProxyManager) *TrayManager {
	return &TrayManager{
		cm:         cm,
		km:         km,
		pm:         pm,
		httpClient: &http.Client{Timeout: 500 * time.Millisecond},
		bufPool: sync.Pool{
			New: func() interface{} { return bytes.NewBuffer(make([]byte, 0, 2048)) },
		},
	}
}

// ==========================================
// 背景狀態機核心定時器 (消抖、防卡死、防 Socket 洩漏)
// ==========================================

// DoAPIRequest 封裝與 Mihomo 內核通訊的 REST API 請求，完美維護內存緩衝池
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
		buf = tm.bufPool.Get().(*bytes.Buffer)
		buf.Reset()
		if err := json.NewEncoder(buf).Encode(payload); err != nil {
			tm.bufPool.Put(buf)
			return nil, fmt.Errorf("marshal payload failed: %v", err)
		}
		bodyReader = buf
	} else if method == "PUT" || method == "POST" || method == "PATCH" {
		bodyReader = bytes.NewReader([]byte("{}"))
	}

	defer func() {
		if buf != nil {
			tm.bufPool.Put(buf)
		}
	}()

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	if secret := tm.cm.GetJsonConfig("secret"); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}

	resp, err := tm.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 204 || resp.ContentLength == 0 {
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil, nil
		}
		return nil, fmt.Errorf("API Status Error: %d", resp.StatusCode)
	}

	capacity := int64(512)
	if resp.ContentLength > 0 {
		capacity = resp.ContentLength
	}
	outBuf := bytes.NewBuffer(make([]byte, 0, capacity))

	limitReader := io.LimitReader(resp.Body, 10*1024*1024)
	if _, err := io.Copy(outBuf, limitReader); err != nil {
		return nil, fmt.Errorf("read response body failed: %v", err)
	}

	body := outBuf.Bytes()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, fmt.Errorf("API Error: %d, Response: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// CheckSystemState 負責向內核拉取實時數據並與本地預期狀態對齊
func (tm *TrayManager) CheckSystemState() int32 {
	if !tm.km.IsProcessRunning("mihomo.exe") {
		return 0 // StateStop
	}
	body, err := tm.DoAPIRequest("GET", "/configs", nil)
	if err != nil {
		// 🌟 核心優化：如果系統正處於初始化/重載空窗期，API 逾時不應誤判為內核停止，返回上一次成功狀態
		if tm.cm.IsSystemInitializing() {
			return tm.cm.GetLastState()
		}
		return 0 // StateStop
	}

	var currentConf struct {
		Tun struct {
			Enable bool `json:"enable"`
		} `json:"tun"`
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(body, &currentConf); err != nil {
		return 0 // StateStop
	}

	targetTunInJson := tm.cm.GetTunState()
	targetProxyInJson := tm.cm.GetProxyState()
	targetModeInJson := tm.cm.GetCurrentModeState()

	isInitializing := tm.cm.IsSystemInitializing()
	if !isInitializing && !tm.cm.IsSyncing() {
		realTunInConfig := tm.cm.GetJsonConfig("tun") == "true"
		realModeInConfig := tm.cm.GetJsonConfig("mode")

		if currentConf.Tun.Enable != targetTunInJson && currentConf.Tun.Enable != realTunInConfig {
			if currentConf.Tun.Enable {
				tm.cm.SetTunState(true)
				tm.cm.SaveJsonConfig("tun", "true")
				if tm.mTun != nil {
					tm.mTun.Check()
				}
			} else {
				tm.cm.SetTunState(false)
				tm.cm.SaveJsonConfig("tun", "false")
				if tm.mTun != nil {
					tm.mTun.Uncheck()
				}
			}
		}

		if currentConf.Mode != "" && currentConf.Mode != targetModeInJson && currentConf.Mode != realModeInConfig {
			tm.cm.SetCurrentModeState(currentConf.Mode)
			tm.cm.SaveJsonConfig("mode", currentConf.Mode)
		}
	}

	isTunActive := currentConf.Tun.Enable
	if isInitializing {
		isTunActive = targetTunInJson
	}

	if isTunActive {
		return 2 // StateTun
	}
	if targetProxyInJson {
		return 3 // StateProxy
	}
	return 4 // StateDefault
}

// MonitorIconState 獨立後台 Goroutine，定時重新整理托盤圖標並提供故障消抖
func (tm *TrayManager) MonitorIconState() {
	var successCounter int

	for {
		if tm.cm.IsReallyExiting() {
			return
		}

		if !tm.km.IsProcessRunning("mihomo.exe") {
			tm.cm.SetTunErrorCounter(0)
			successCounter = 0

			if tm.cm.GetLastState() != 0 {
				tm.UpdateIconByState(0) // StateStop
				tm.cm.SetLastState(0)
			}
		} else {
			curr := tm.CheckSystemState()
			isTunModeInConfig := tm.cm.GetTunState()
			isPhysicalLost := !tm.cm.IsTunInterfaceCurrentlyAlive()
			isInitializing := tm.cm.IsSystemInitializing()
			isCurrentlySyncing := tm.cm.IsSyncing()

			isBroken := (curr == 0) || (isTunModeInConfig && isPhysicalLost && !isInitializing && !isCurrentlySyncing)

			if isBroken {
				successCounter = 0
				if tm.cm.GetTunErrorCounter() < 5 {
					tm.cm.AddTunErrorCounter(1)
				}

				if tm.cm.GetTunErrorCounter() > 2 {
					targetState := int32(1) // StateError
					if curr == 0 {
						targetState = 0 // StateStop
					}

					if tm.cm.GetLastState() != targetState {
						tm.UpdateIconByState(int(targetState))
						tm.cm.SetLastState(targetState)
					}
				}
			} else {
				if isInitializing || isCurrentlySyncing {
					successCounter = 0
					if tm.cm.GetLastState() != curr {
						tm.UpdateIconByState(int(curr))
						tm.cm.SetLastState(curr)
					}
				} else {
					successCounter++
					currentErrCount := tm.cm.GetTunErrorCounter()

					if currentErrCount <= 2 || successCounter >= 3 {
						if successCounter >= 3 {
							tm.cm.SetTunErrorCounter(0)
						}

						if tm.cm.GetLastState() != curr {
							tm.UpdateIconByState(int(curr))
							tm.cm.SetLastState(curr)
						}
					}
				}
			}
		}
		time.Sleep(1 * time.Second)
	}
}

// WatchTunState 定時監控 Windows 物理虛擬網卡是否存在
func (tm *TrayManager) WatchTunState() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	lastHasTun := false
	if ifaces, err := net.Interfaces(); err == nil {
		for _, i := range ifaces {
			if tm.IsTunInterfaceMatch(i.Name) {
				lastHasTun = true
				break
			}
		}
	}
	tm.cm.SetTunInterfaceCurrentlyAlive(lastHasTun)

	confirmCount := 0
	for {
		select {
		case <-ticker.C:
			if tm.cm.IsReallyExiting() {
				return
			}
			if tm.cm.IsSystemInitializing() || tm.cm.IsSyncing() {
				confirmCount = 0
				continue
			}

			currentHasTun := false
			currentIfaces, err := net.Interfaces()
			if err != nil {
				continue
			}
			for _, i := range currentIfaces {
				if tm.IsTunInterfaceMatch(i.Name) {
					currentHasTun = true
					break
				}
			}

			if currentHasTun != lastHasTun {
				if currentHasTun {
					lastHasTun = true
					confirmCount = 0
					tm.cm.SetTunInterfaceCurrentlyAlive(true)
				} else {
					confirmCount++
					if confirmCount >= 2 {
						lastHasTun = false
						confirmCount = 0
						tm.cm.SetTunInterfaceCurrentlyAlive(false)
					}
				}
			} else {
				confirmCount = 0
			}
			tm.cm.SetHasFirstSynced(true)
		}
	}
}

// ==========================================
// 外部命令與組態重載 (消抖與安全優化)
// ==========================================

// ReloadConfigFile 控制前端發起配置重載，並在背景協程中進行安全的狀態對齊
func (tm *TrayManager) ReloadConfigFile() {
	tm.cm.SetSystemInitializing(true)
	isProxyEnabled := tm.cm.GetProxyState()

	if isProxyEnabled {
		tm.cm.SetLastAppliedProxy(true)
		tm.pm.SetProxyRegistry(false)
	}

	_, _ = tm.DoAPIRequest("PUT", "/configs?force=true", nil)

	// 🛠️ 【消抖優化】將重載後的阻塞緩衝與狀態修復完全放入獨立協程，防止阻塞 UI 執行緒
	go func() {
		defer tm.cm.SetSystemInitializing(false)
		time.Sleep(200 * time.Millisecond)

		isTunOn := tm.cm.GetJsonConfig("tun") == "true"
		modeStr := tm.cm.GetJsonConfig("mode")

		tm.cm.SetTunState(isTunOn)
		tm.cm.SetCurrentModeState(modeStr)

		if isProxyEnabled {
			tm.pm.SetProxyRegistry(true)
		}

		tm.SyncConfigToKernel()
	}()
}

// SyncConfigToKernel 發起 REST API 的 PATCH 請求，將本地期望狀態同步給內核
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

// LaunchWebUI 控制 Chrome/Edge 核心瀏覽器以擴展 App 模式強制居中拉起，並根治通訊埠洩漏
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

	// A. 如果快取中已有網頁視窗，安全讀取並置頂
	if hwnd := winapi.GetCachedWebUIHwnd(); hwnd != 0 {
		if winapi.IsWindowVisible(hwnd) {
			winapi.FocusWindowSilky(hwnd, tm.cm)
			return
		}
		winapi.SetCachedWebUIHwnd(0)
	}

	client := &http.Client{Timeout: 300 * time.Millisecond}
	isPortOccupied := false

	// B. 探測 CDP 偵錯通訊埠
	if resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/json", debugPort)); err == nil {
		isPortOccupied = true
		defer resp.Body.Close()
		var targets []map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&targets); err == nil {
			for _, t := range targets {
				pURL, _ := t["url"].(string)
				if strings.Contains(pURL, "/ui/") || strings.Contains(pURL, "setup") {
					id, _ := t["id"].(string)
					
					// 激活標籤頁
					if actResp, actErr := client.Get(fmt.Sprintf("http://127.0.0.1:%s/json/activate/%s", debugPort, id)); actErr == nil {
						_ = actResp.Body.Close()
					}

					// 🛠️ 【核心優化】非同步尋找並置頂已存在的 Chrome 視窗，傳遞配置中心鎖介面
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

	// C. 若偵錯通訊埠卡死但被佔用，暴力執行強殺釋放
	if isPortOccupied {
		killCmd := "for /f \"tokens=5\" %%a in ('netstat -aon ^| findstr :" + debugPort + " ^| findstr LISTENING') do taskkill /F /PID %%a"
		_ = exec.Command("cmd", "/c", killCmd).Run()
		time.Sleep(150 * time.Millisecond)
	}

	// D. 尋找系統中可用的現代瀏覽器路徑
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

		scrW := winapi.GetSystemMetrics(0) // SM_CXSCREEN
		scrH := winapi.GetSystemMetrics(1) // SM_CYSCREEN

		winW, winH := 1280, 768
		if scrW > 0 && scrH > 0 {
			w, h := float64(scrW), float64(scrH)
			aspectRatio := w / h
			switch {
			case scrW >= 3840:
				winW, winH = 1920, 1080
			case aspectRatio > 2.0:
				winW, winH = 1440, 900
			case aspectRatio <= 1.05:
				winW = int(w * 0.85)
				winH = int(h * 0.65)
				if winW < 800 { winW = 800 }
			case scrW >= 2560:
				winW, winH = 1600, 960
			case scrW >= 1920:
				winW, winH = 1280, 800
			case scrW == 1536 && scrH == 864:
				winW, winH = 1150, 680
			case scrW >= 1440:
				winW, winH = 1150, 720
			case scrW == 1366 && scrH == 768:
				winW, winH = 1050, 640
			case scrW <= 1280:
				winW = int(w * 0.92)
				winH = int(h * 0.88)
				if winW < 960 { winW = 960 }
				if winH < 580 { winH = 580 }
			default:
				winW = int(w * 0.75)
				winH = int(h * 0.75)
			}
		}

		winX := (scrW - winW) / 2
		winY := (scrH - winH) / 2
		if winX < 0 { winX = 0 }
		if winY < 0 { winY = 0 }

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

			// 🛠️ 【核心修復】巡迴內部激活時，對次級 HTTP 請求做 Body 讀空與關閉保護，防止臨時通訊埠枯竭
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

// ==========================================
// 托盤選單與組裝佈局
// ==========================================

// SetupTrayUI 綁定選單事件並組裝整個 UI 佈局 (完成原 main 的回呼註冊)
func (tm *TrayManager) SetupTrayUI() {
	tm.cm.SetSystemInitializing(1)

	tm.ToggleAutoStart(tm.CheckAutoStartStatus())
	tm.cm.EnsureDefaultConfig()
	tm.SniffAndSolidifyConfig()

	initProxyChecked := tm.cm.GetProxyState()
	initTunChecked := tm.cm.GetTunState()
	initModeChecked := tm.cm.GetCurrentModeState()

	tm.pm.SetProxyRegistry(initProxyChecked)
	tm.UpdateIconByState(0) // StateStop

	// 1. 滑鼠左鍵點擊截流事件
	systray.SetOnClick(func(menu systray.IMenu) {
		if tm.cm.IsSystemInitializing() {
			return
		}
		if !tm.cm.CheckAndThrottleClick(int64(1000 * time.Millisecond)) {
			return
		}
		go tm.LaunchWebUI()
	})

	// 2. 選單：進入 Web 面板
	mWeb := systray.AddMenuItem("進入 Web 面板", "")
	mWeb.Click(func() {
		if !tm.cm.CheckAndThrottleClick(int64(1000 * time.Millisecond)) {
			return
		}
		go tm.LaunchWebUI()
	})

	systray.AddSeparator()

	// 3. 選單：系統代理
	mProxy := systray.AddMenuItemCheckbox("系統代理", "", initProxyChecked)
	mProxy.Click(func() {
		next := !mProxy.Checked()
		tm.pm.SetProxyRegistry(next)
		if next { mProxy.Check() } else { mProxy.Uncheck() }
	})

	// 4. 選單：虛擬網卡 (TUN)
	tm.mTun = systray.AddMenuItemCheckbox("虛擬網卡 (TUN)", "", initTunChecked)
	tm.mTun.Click(func() {
		next := !tm.mTun.Checked()
		if next { tm.mTun.Check() } else { tm.mTun.Uncheck() }
		go tm.SetTunMode(next)
	})

	systray.AddSeparator()

	// 5. 選單：模式切換
	mModeRoot := systray.AddMenuItem("模式切換", "")
	modeMenus := make(map[string]*systray.MenuItem)
	setupMode := func(key, label string) {
		modeMenus[key] = mModeRoot.AddSubMenuItemCheckbox(label, "", initModeChecked == key)
		modeMenus[key].Click(func() {
			for k, menu := range modeMenus {
				if k == key { menu.Check() } else { menu.Uncheck() }
			}
			go tm.SetMihomoMode(key)
		})
	}
	setupMode("rule", "規則模式")
	setupMode("global", "全局模式")
	setupMode("direct", "直連模式")

	systray.AddSeparator()

	// 6. 選單：開啟目錄
	mDir := systray.AddMenuItem("打開目錄", "")
	mDir.Click(func() {
		windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(tm.cm.BaseDir()), nil, nil, windows.SW_SHOWNORMAL)
	})

	// 7. 選單：更多功能
	mMoreRoot := systray.AddMenuItem("更多", "")
	mAuto := mMoreRoot.AddSubMenuItemCheckbox("開機自啟動", "", tm.CheckAutoStartStatus())
	mAuto.Click(func() {
		next := !mAuto.Checked()
		tm.ToggleAutoStart(next)
		if next { mAuto.Check() } else { mAuto.Uncheck() }
	})

	mRestart := mMoreRoot.AddSubMenuItem("重启内核", "")
	mRestart.Click(func() {
		tm.cm.SetSystemInitializing(true)
		tm.cm.SetHasFirstSynced(false)
		tm.km.KillProcessByName("mihomo.exe")
		tm.SniffAndSolidifyConfig()
	})

	mReload := mMoreRoot.AddSubMenuItem("重载配置文件", "")
	mReload.Click(func() {
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

	// 8. 選單：完全退出
	mExit := systray.AddMenuItem("退出程序", "")
	mExit.Click(func() {
		tm.cm.MarkAsExiting()
		systray.Quit()
	})
}

// ==========================================
// 輔助工具方法
// ==========================================

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
		// 🛠️ 【核心修復】對排程任務參數路徑中的單引號進行徹底轉義，根治非標 Windows 用戶名引發的腳本中斷故障
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
		cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		if err := cmd.Run(); err == nil {
			success = true
		}
	} else {
		cmd := exec.Command("schtasks", "/Delete", "/TN", "\\"+taskName, "/F")
		cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
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
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
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
