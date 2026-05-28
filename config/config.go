package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const CONFIG_FILE = "mihomo-run.json"

type ConfigManager struct {
	baseDir string
	exePath string

	configMu   sync.RWMutex
	configData map[string]string

	modeMu             sync.RWMutex
	currentModeState   string
	lastAppliedProxy   bool

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
	cm := &ConfigManager{
		baseDir:              baseDir,
		exePath:              exePath,
		configData:           make(map[string]string),
		isSystemInitializing: 1,
		lastState:            -1,
	}
	cm.loadFromFile()
	return cm
}

func (cm *ConfigManager) loadFromFile() {
	targetPath := filepath.Join(cm.baseDir, CONFIG_FILE)
	bytes, err := os.ReadFile(targetPath)
	if err == nil {
		cm.configMu.Lock()
		_ = json.Unmarshal(bytes, &cm.configData)
		cm.configMu.Unlock()
	}
}

func (cm *ConfigManager) BaseDir() string { return cm.baseDir }
func (cm *ConfigManager) ExePath() string { return cm.exePath }

func (cm *ConfigManager) GetJsonConfig(key string) string {
	cm.configMu.RLock()
	defer cm.configMu.RUnlock()
	return cm.configData[key]
}

func (cm *ConfigManager) SaveJsonConfig(key, value string) {
	cm.configMu.Lock()
	if cm.configData[key] == value {
		cm.configMu.Unlock()
		return
	}
	cm.configData[key] = value

	dataCopy := make(map[string]string, len(cm.configData))
	for k, v := range cm.configData {
		dataCopy[k] = v
	}
	cm.configMu.Unlock()

	version := atomic.AddInt32(&cm.configVersion, 1)
	go cm.syncToFile(dataCopy, version)
}

func (cm *ConfigManager) syncToFile(data map[string]string, version int32) {
	if version <= atomic.LoadInt32(&cm.lastWrittenVersion) {
		return
	}

	bytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return
	}

	targetPath := filepath.Join(cm.baseDir, CONFIG_FILE)
	tmpPath := targetPath + ".tmp"

	if err := os.WriteFile(tmpPath, bytes, 0644); err != nil {
		return
	}

	for i := 0; i < 3; i++ {
		if err := os.Rename(tmpPath, targetPath); err == nil {
			atomic.StoreInt32(&cm.lastWrittenVersion, version)
			return
		}
		time.Sleep(time.Duration(50*(i+1)) * time.Millisecond)
	}
	_ = os.Remove(tmpPath)
}

func (cm *ConfigManager) EnsureDefaultConfig() {
	if cm.GetJsonConfig("port") == "" {
		cm.SaveJsonConfig("port", "7890")
	}
	if cm.GetJsonConfig("external-controller") == "" {
		cm.SaveJsonConfig("external-controller", "127.0.0.1:9090")
	}
	if cm.GetJsonConfig("mode") == "" {
		cm.SaveJsonConfig("mode", "rule")
	}
}

func (cm *ConfigManager) IsSystemInitializing() bool {
	return atomic.LoadInt32(&cm.isSystemInitializing) == 1
}

func (cm *ConfigManager) SetSystemInitializing(val bool) {
	var i int32
	if val { i = 1 }
	atomic.StoreInt32(&cm.isSystemInitializing, i)
}

func (cm *ConfigManager) IsSyncing() bool {
	return atomic.LoadInt32(&cm.isSyncing) == 1
}

func (cm *ConfigManager) SetSyncing(val bool) {
	var i int32
	if val { i = 1 }
	atomic.StoreInt32(&cm.isSyncing, i)
}

func (cm *ConfigManager) CompareAndSwapSyncing(old, new int32) bool {
	return atomic.CompareAndSwapInt32(&cm.isSyncing, old, new)
}

func (cm *ConfigManager) IsReallyExiting() bool {
	return atomic.LoadInt32(&cm.isReallyExiting) == 1
}

func (cm *ConfigManager) MarkAsExiting() {
	atomic.StoreInt32(&cm.isReallyExiting, 1)
}

func (cm *ConfigManager) IsKernelActive() bool {
	return atomic.LoadInt32(&cm.isKernelActive) == 1
}

func (cm *ConfigManager) SetKernelActive(val bool) {
	var i int32
	if val { i = 1 }
	atomic.StoreInt32(&cm.isKernelActive, i)
}

func (cm *ConfigManager) GetTunState() bool {
	return atomic.LoadInt32(&cm.atomicTunState) == 1
}

func (cm *ConfigManager) SetTunState(val bool) {
	var i int32
	if val { i = 1 }
	atomic.StoreInt32(&cm.atomicTunState, i)
}

func (cm *ConfigManager) GetProxyState() bool {
	return atomic.LoadInt32(&cm.atomicProxyState) == 1
}

func (cm *ConfigManager) SetProxyState(val bool) {
	var i int32
	if val { i = 1 }
	atomic.StoreInt32(&cm.atomicProxyState, i)
}

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
	cm.modeMu.RLock()
	defer cm.modeMu.RUnlock()
	return cm.currentModeState
}

func (cm *ConfigManager) SetCurrentModeState(mode string) {
	cm.modeMu.Lock()
	defer cm.modeMu.Unlock()
	cm.currentModeState = mode
}

func (cm *ConfigManager) IsTunInterfaceCurrentlyAlive() bool {
	return atomic.LoadInt32(&cm.isTunInterfaceCurrentlyAlive) == 1
}

func (cm *ConfigManager) SetTunInterfaceCurrentlyAlive(val bool) {
	var i int32
	if val { i = 1 }
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
	if val { i = 1 }
	atomic.StoreInt32(&cm.hasFirstSynced, i)
}

func (cm *ConfigManager) CheckAndThrottleClick(throttleTimeMs int64) bool {
	now := time.Now().UnixMilli()
	last := atomic.LoadInt64(&cm.lastClickTime)
	if now-last < throttleTimeMs {
		return false
	}
	atomic.StoreInt64(&cm.lastClickTime, now)
	return true
}

func (cm *ConfigManager) CompareAndSwapFocusing(oldVal, newVal int32) bool {
	return atomic.CompareAndSwapInt32(&cm.isFocusing, oldVal, newVal)
}

func (cm *ConfigManager) SetFocusing(val int32) {
	atomic.StoreInt32(&cm.isFocusing, val)
}
