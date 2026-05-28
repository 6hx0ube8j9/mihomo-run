package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const CONFIG_FILE = "mihomo-run.json"

type ConfigManager struct {
	baseDir string
	exePath string

	configMu         sync.RWMutex
	configData       map[string]string
	currentModeState string
	lastAppliedProxy bool

	isSystemInitializing         int32
	isSyncing                    int32
	globalOpID                   int32
	hasFirstSynced               int32
	isKernelActive               int32
	isFocusing                   int32
	isReallyExiting              int32
	isTunInterfaceCurrentlyAlive int32
	configVersion                int32
	lastWrittenVersion           int32
	lastState                    int32
	tunErrorCounter              int32
	atomicProxyState             int32 
	atomicTunState               int32
	lastClickTime                int64
}

func NewConfigManager(baseDir, exePath string) *ConfigManager {
	return &ConfigManager{
		baseDir:              baseDir,
		exePath:              exePath,
		configData:           make(map[string]string),
		isSystemInitializing: 1, 
		lastState:            -1,
	}
}

func (cm *ConfigManager) GetJsonConfig(key string) string {
	cm.configMu.RLock()
	defer cm.configMu.RUnlock()
	return cm.configData[key]
}

func (cm *ConfigManager) EnsureDefaultConfig() {
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
			tmpPath := cfgPath + ".tmp"
			if writeErr := os.WriteFile(tmpPath, b, 0644); writeErr == nil {
				_ = os.Rename(tmpPath, cfgPath)
			}
		}
	}

	cm.configMu.Lock()
	for k, v := range fileData {
		cm.configData[k] = v
	}
	currentProxy := cm.configData["proxy"]
	currentTun := cm.configData["tun"]
	currentMode := cm.configData["mode"]
	cm.configMu.Unlock()

	if currentProxy == "true" {
		cm.SetProxyState(true)
	} else {
		cm.SetProxyState(false)
	}

	if currentTun == "true" {
		cm.SetTunState(true)
	} else {
		cm.SetTunState(false)
	}

	cm.SetCurrentModeState(currentMode)
}

func (cm *ConfigManager) SaveJsonConfig(key, value string) {
	cm.configMu.Lock()
	if key != "" {
		if cm.configData[key] == value {
			cm.configMu.Unlock()
			return
		}
		cm.configData[key] = value

		switch key {
		case "proxy":
			if value == "true" {
				atomic.StoreInt32(&cm.atomicProxyState, 1)
			} else {
				atomic.StoreInt32(&cm.atomicProxyState, 0)
			}
		case "tun":
			if value == "true" {
				atomic.StoreInt32(&cm.atomicTunState, 1)
			} else {
				atomic.StoreInt32(&cm.atomicTunState, 0)
			}
		case "mode":
			cm.currentModeState = value
		}
	}

	b, err := json.Marshal(cm.configData)
	cm.configMu.Unlock()

	if err != nil {
		return
	}

	myVersion := atomic.AddInt32(&cm.configVersion, 1)

	go func(dataToWrite []byte, version int32) {
		cfgPath := filepath.Join(cm.baseDir, CONFIG_FILE)
		tmpPath := cfgPath + ".tmp." + strconv.FormatInt(int64(version), 10)

		if err := os.WriteFile(tmpPath, dataToWrite, 0644); err == nil {
			currentWritten := atomic.LoadInt32(&cm.lastWrittenVersion)
			if version < currentWritten {
				_ = os.Remove(tmpPath)
				return
			}
			for {
				current := atomic.LoadInt32(&cm.lastWrittenVersion)
				if version <= current {
					_ = os.Remove(tmpPath)
					return
				}
				if atomic.CompareAndSwapInt32(&cm.lastWrittenVersion, current, version) {
					break
				}
			}
			if renameErr := os.Rename(tmpPath, cfgPath); renameErr != nil {
				_ = os.Remove(tmpPath)
			}
		}
	}(b, myVersion)
}


func (cm *ConfigManager) BaseDir() string { return cm.baseDir }
func (cm *ConfigManager) ExePath() string { return cm.exePath }

func (cm *ConfigManager) GetLastAppliedProxy() bool {
	cm.configMu.RLock()
	defer cm.configMu.RUnlock()
	return cm.lastAppliedProxy
}

func (cm *ConfigManager) SetLastAppliedProxy(enable bool) {
	cm.configMu.Lock()
	defer cm.configMu.Unlock()
	cm.lastAppliedProxy = enable
}

func (cm *ConfigManager) GetCurrentModeState() string {
	cm.configMu.RLock()
	defer cm.configMu.RUnlock()
	return cm.currentModeState
}

func (cm *ConfigManager) SetCurrentModeState(mode string) {
	cm.configMu.Lock()
	defer cm.configMu.Unlock()
	cm.currentModeState = mode
}

func (cm *ConfigManager) IsSystemInitializing() bool {
	return atomic.LoadInt32(&cm.isSystemInitializing) == 1
}

func (cm *ConfigManager) SetSystemInitializing(val bool) {
	var i int32
	if val {
		i = 1
	}
	atomic.StoreInt32(&cm.isSystemInitializing, i)
}

func (cm *ConfigManager) IsSyncing() bool {
	return atomic.LoadInt32(&cm.isSyncing) == 1
}

func (cm *ConfigManager) SetSyncing(val bool) {
	var i int32
	if val {
		i = 1
	}
	atomic.StoreInt32(&cm.isSyncing, i)
}

func (cm *ConfigManager) CompareAndSwapSyncing(oldVal, newVal int32) bool {
	return atomic.CompareAndSwapInt32(&cm.isSyncing, oldVal, newVal)
}

func (cm *ConfigManager) GetProxyState() bool {
	return atomic.LoadInt32(&cm.atomicProxyState) == 1
}

func (cm *ConfigManager) SetProxyState(enable bool) {
	var i int32
	if enable {
		i = 1
	}
	atomic.StoreInt32(&cm.atomicProxyState, i)
}

func (cm *ConfigManager) GetTunState() bool {
	return atomic.LoadInt32(&cm.atomicTunState) == 1
}

func (cm *ConfigManager) SetTunState(enable bool) {
	var i int32
	if enable {
		i = 1
	}
	atomic.StoreInt32(&cm.atomicTunState, i)
}

func (cm *ConfigManager) IsReallyExiting() bool {
	return atomic.LoadInt32(&cm.isReallyExiting) == 1
}

func (cm *ConfigManager) MarkAsExiting() {
	atomic.StoreInt32(&cm.isReallyExiting, 1)
}

func (cm *ConfigManager) IsTunInterfaceCurrentlyAlive() bool {
	return atomic.LoadInt32(&cm.isTunInterfaceCurrentlyAlive) == 1
}

func (cm *ConfigManager) SetTunInterfaceCurrentlyAlive(alive bool) {
	var i int32
	if alive {
		i = 1
	}
	atomic.StoreInt32(&cm.isTunInterfaceCurrentlyAlive, i)
}

func (cm *ConfigManager) GetTunErrorCounter() int32 {
	return atomic.LoadInt32(&cm.tunErrorCounter)
}

func (cm *ConfigManager) SetTunErrorCounter(val int32) {
	atomic.StoreInt32(&cm.tunErrorCounter, val)
}

func (cm *ConfigManager) AddTunErrorCounter(delta int32) int32 {
	return atomic.AddInt32(&cm.tunErrorCounter, delta)
}

func (cm *ConfigManager) GetLastState() int32 {
	return atomic.LoadInt32(&cm.lastState)
}

func (cm *ConfigManager) SetLastState(state int32) {
	atomic.StoreInt32(&cm.lastState, state)
}

func (cm *ConfigManager) AddGlobalOpID() int32 {
	return atomic.AddInt32(&cm.globalOpID, 1)
}

func (cm *ConfigManager) GetGlobalOpID() int32 {
	return atomic.LoadInt32(&cm.globalOpID)
}

func (cm *ConfigManager) SetHasFirstSynced(val bool) {
	var i int32
	if val {
		i = 1
	}
	atomic.StoreInt32(&cm.hasFirstSynced, i)
}

func (cm *ConfigManager) IsKernelActive() bool {
	return atomic.LoadInt32(&cm.isKernelActive) == 1
}

func (cm *ConfigManager) SetKernelActive(active bool) {
	var i int32
	if active {
		i = 1
	}
	atomic.StoreInt32(&cm.isKernelActive, i)
}

func (cm *ConfigManager) CompareAndSwapFocusing(oldVal, newVal int32) bool {
	return atomic.CompareAndSwapInt32(&cm.isFocusing, oldVal, newVal)
}

func (cm *ConfigManager) SetFocusing(val int32) {
	atomic.StoreInt32(&cm.isFocusing, val)
}

func (cm *ConfigManager) CheckAndThrottleClick(thresholdNano int64) bool {
	now := time.Now().UnixNano()
	last := atomic.LoadInt64(&cm.lastClickTime)
	if now-last < thresholdNano {
		return false
	}
	atomic.StoreInt64(&cm.lastClickTime, now)
	return true
}
