package ui

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mihomo-tray/winapi"
)

const (
	MaxBrowserFocusRetry  = 20
	BrowserFocusSleepTime = 50 * time.Millisecond
)

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
