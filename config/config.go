package config

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const CONFIG_FILE = "mihomo-tray.json"

type AppState struct {
	CurrentMode          string
	LastAppliedProxy     bool
	IsSystemInitializing bool
	IsSyncing            bool
	GlobalOpID           int32
	HasFirstSynced       bool
	IsKernelActive       bool
	IsFocusing           bool
	IsReallyExiting      bool
	TunStartTime         time.Time
	TunAlive             bool
	TunRecoveryStart     time.Time
	LastState            int32
	ProxyEnabled         bool
	TunEnabled           bool
	LastClickTime        int64
	IsProxyWriting       bool
}

type ConfigManager struct {
	baseDir string
	exePath string

	mu         sync.RWMutex
	configData map[string]string
	state      AppState
}

func NewConfigManager(baseDir, exePath string) *ConfigManager {
	return &ConfigManager{
		baseDir:    baseDir,
		exePath:    exePath,
		configData: make(map[string]string),
		state: AppState{
			IsSystemInitializing: true,
			LastState:            -1,
		},
	}
}

func (cm *ConfigManager) EnsureDefaultConfig() {
	tmpFiles, _ := filepath.Glob(filepath.Join(cm.baseDir, CONFIG_FILE+".tmp*"))
	for _, f := range tmpFiles {
		_ = os.Remove(f)
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()

	cfgPath := filepath.Join(cm.baseDir, CONFIG_FILE)
	defaults := map[string]string{
		"proxy":               "false",
		"tun":                 "false",
		"autostart":           "false",
		"mode":                "rule",
		"port":                "7890",
		"tun_device":          "mihomo",
		"external-controller": "http://127.0.0.1:9090",
		"secret":              "",
	}

	fileData := make(map[string]string)
	f, err := os.Open(cfgPath)
	if err == nil {
		_ = json.NewDecoder(f).Decode(&fileData)
		f.Close()
	}

	hasChanges := false
	for k, v := range defaults {
		fileVal, exists := fileData[k]
		if !exists || fileVal == "" {
			fileData[k] = v
			hasChanges = true
		}
	}

	if hasChanges || err != nil {
		if b, marshalErr := json.Marshal(fileData); marshalErr == nil {
			tmpPath := cfgPath + ".tmp_init"
			if writeErr := os.WriteFile(tmpPath, b, 0644); writeErr == nil {
				for i := 0; i < 3; i++ {
					if renameErr := os.Rename(tmpPath, cfgPath); renameErr == nil {
						break
					}
					time.Sleep(time.Duration(50*(i+1)) * time.Millisecond)
				}
			}
		}
	}

	for k, v := range fileData {
		cm.configData[k] = v
	}

	cm.state.ProxyEnabled = (cm.configData["proxy"] == "true")
	cm.state.TunEnabled = (cm.configData["tun"] == "true")
	if cm.state.TunEnabled {
		cm.state.TunStartTime = time.Now()
	}
	cm.state.CurrentMode = cm.configData["mode"]
}

func (cm *ConfigManager) SaveJsonConfig(key, value string) {
	cm.mu.Lock()
	if key == "" || cm.configData[key] == value {
		cm.mu.Unlock()
		return
	}

	cm.configData[key] = value

	switch key {
	case "tun":
		cm.state.TunEnabled = (value == "true")
		if cm.state.TunEnabled {
			cm.state.TunStartTime = time.Now()
		} else {
			cm.state.TunStartTime = time.Time{}
		}
	case "mode":
		cm.state.CurrentMode = value
	case "proxy":
		cm.state.ProxyEnabled = (value == "true")
	}

	dataCopy := make(map[string]string, len(cm.configData))
	for k, v := range cm.configData {
		dataCopy[k] = v
	}
	cm.mu.Unlock()

	b, err := json.Marshal(dataCopy)
	if err != nil {
		return
	}

	cfgPath := filepath.Join(cm.baseDir, CONFIG_FILE)
	tmpPath := cfgPath + ".tmp_" + strconv.FormatInt(time.Now().UnixNano(), 10)

	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return
	}

	var writeSuccess bool
	defer func() {
		_ = f.Close()
		if !writeSuccess {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err = f.Write(b); err != nil {
		return
	}
	if err = f.Sync(); err != nil {
		return
	}

	writeSuccess = true
	_ = f.Close()

	if err := os.Rename(tmpPath, cfgPath); err != nil {
		_ = os.Remove(tmpPath)
	}
}

func (cm *ConfigManager) SniffAndSolidifyConfig() {
	configPath := filepath.Join(cm.BaseDir(), "config.yaml")
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
					cm.SaveJsonConfig("port", port)
					foundMixed = true
				}
			}
			continue
		}
		if !foundMixed && strings.HasPrefix(trimmed, "port:") {
			if parts := strings.SplitN(trimmed, ":", 2); len(parts) == 2 {
				if port := strings.Trim(parts[1], " \"'"); port != "" {
					cm.SaveJsonConfig("port", port)
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
					cm.SaveJsonConfig("tun_device", devName)
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
				cm.SaveJsonConfig("external-controller", addr)
			}
			continue
		}
		if strings.HasPrefix(trimmed, "secret:") {
			val := strings.Trim(strings.TrimPrefix(trimmed, "secret:"), " \"'")
			cm.SaveJsonConfig("secret", val)
			continue
		}
	}
}

func (cm *ConfigManager) GetJsonConfig(key string) string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.configData[key]
}

func (cm *ConfigManager) BaseDir() string { return cm.baseDir }
func (cm *ConfigManager) ExePath() string { return cm.exePath }

func (cm *ConfigManager) GetLastAppliedProxy() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.state.LastAppliedProxy
}

func (cm *ConfigManager) SetLastAppliedProxy(enable bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.state.LastAppliedProxy = enable
}

func (cm *ConfigManager) GetCurrentModeState() string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.state.CurrentMode
}

func (cm *ConfigManager) SetCurrentModeState(mode string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.state.CurrentMode = mode
}

func (cm *ConfigManager) IsSystemInitializing() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.state.IsSystemInitializing
}

func (cm *ConfigManager) SetSystemInitializing(val bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.state.IsSystemInitializing = val
}

func (cm *ConfigManager) IsSyncing() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.state.IsSyncing
}

func (cm *ConfigManager) SetSyncing(val bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.state.IsSyncing = val
}

func (cm *ConfigManager) TryStartSyncing() bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if cm.state.IsSyncing {
		return false
	}
	cm.state.IsSyncing = true
	return true
}

func (cm *ConfigManager) GetProxyState() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.state.ProxyEnabled
}

func (cm *ConfigManager) SetProxyState(enable bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.state.ProxyEnabled = enable
}

func (cm *ConfigManager) GetTunState() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.state.TunEnabled
}

func (cm *ConfigManager) SetTunState(enable bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.state.TunEnabled = enable
}

func (cm *ConfigManager) IsReallyExiting() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.state.IsReallyExiting
}

func (cm *ConfigManager) MarkAsExiting() {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.state.IsReallyExiting = true
}

func (cm *ConfigManager) GetLastState() int32 {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.state.LastState
}

func (cm *ConfigManager) SetLastState(state int32) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.state.LastState = state
}

func (cm *ConfigManager) AddGlobalOpID() int32 {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.state.GlobalOpID++
	return cm.state.GlobalOpID
}

func (cm *ConfigManager) GetGlobalOpID() int32 {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.state.GlobalOpID
}

func (cm *ConfigManager) SetHasFirstSynced(val bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.state.HasFirstSynced = val
}

func (cm *ConfigManager) IsKernelActive() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.state.IsKernelActive
}

func (cm *ConfigManager) SetKernelActive(active bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.state.IsKernelActive = active
}

func (cm *ConfigManager) TryStartFocusing() bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if cm.state.IsFocusing {
		return false
	}
	cm.state.IsFocusing = true
	return true
}

func (cm *ConfigManager) SetFocusing(val bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.state.IsFocusing = val
}

func (cm *ConfigManager) CheckAndThrottleClick(thresholdNano int64) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	now := time.Now().UnixNano()
	if now-cm.state.LastClickTime < thresholdNano {
		return false
	}
	cm.state.LastClickTime = now
	return true
}

func (cm *ConfigManager) IsProxyWriting() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.state.IsProxyWriting
}

func (cm *ConfigManager) SetProxyWriting(val bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.state.IsProxyWriting = val
}

func (cm *ConfigManager) GetTunStartTime() time.Time {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.state.TunStartTime
}

func (cm *ConfigManager) SetTunStartTime(t time.Time) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.state.TunStartTime = t
}

func (cm *ConfigManager) IsTunAlive() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.state.TunAlive
}

func (cm *ConfigManager) SetTunAlive(alive bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.state.TunAlive = alive
}

func (cm *ConfigManager) GetTunRecoveryStart() time.Time {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.state.TunRecoveryStart
}

func (cm *ConfigManager) SetTunRecoveryStart(t time.Time) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.state.TunRecoveryStart = t
}
