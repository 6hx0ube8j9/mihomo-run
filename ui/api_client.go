package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var bufPool = sync.Pool{
	New: func() interface{} {
		return bytes.NewBuffer(make([]byte, 0, 2048))
	},
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

func (tm *TrayManager) WatchCoreAPI() {
	ticker := time.NewTicker(3 * time.Second)
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

				for k, m := range tm.mModes {
					if k == currentConf.Mode {
						m.Check()
					} else {
						m.Uncheck()
					}
				}
			}
		}
	}
}

func (tm *TrayManager) ReloadConfigFile() {
	tm.cm.SetSyncing(true)
	tm.cm.SetSystemInitializing(true)
	isProxyEnabled := tm.cm.GetProxyState()

	_, _ = tm.DoAPIRequest("PUT", "/configs?force=true", nil)
	tm.cm.SniffAndSolidifyConfig()

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

		payload := map[string]interface{}{
			"tun":  map[string]bool{"enable": isTunOn},
			"mode": modeStr,
		}

		for i := 0; i < 4; i++ {
			if _, err := tm.DoAPIRequest("PATCH", "/configs", payload); err == nil {
				break
			}
			time.Sleep(time.Duration(i+1) * 300 * time.Millisecond)
		}
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
			if tm.IsTunInterfaceMatch("") { 
			}
			time.Sleep(200 * time.Millisecond)
		}
	}(newID)
}
