package kernel

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ConfigContextInterface 用於打破循環依賴，定義進程管理器所需的狀態存取介面
type ConfigContextInterface interface {
	BaseDir() string
	IsReallyExiting() bool
	IsSystemInitializing() bool
	SetSystemInitializing(val bool)
	IsSyncing() bool
	SetHasFirstSynced(val bool)
	SetKernelActive(active bool)
	IsKernelActive() bool
}

// KernelHooks 定義核心進程生命週期中觸發的外部事件（如配置同步）
type KernelHooks struct {
	OnKernelStarted func() // 核心啟動成功時觸發（用於觸發配置嗅探）
	OnKernelReady   func() // 核心運行 1 秒就緒後觸發（用於觸發 REST API 同步與代理註冊表更新）
}

// KernelManager 專職負責 Mihomo 核心進程的維護與安全守護
type KernelManager struct {
	hJob  windows.Handle
	cm    ConfigContextInterface
	hooks KernelHooks
}

// NewKernelManager 初始化進程管理器
func NewKernelManager(cm ConfigContextInterface, hooks KernelHooks) *KernelManager {
	return &KernelManager{
		cm:    cm,
		hooks: hooks,
	}
}

// ==========================================
// Windows Job Object 基礎建設 (防止孤兒進程殘留)
// ==========================================

// InitJobObject 初始化 Windows 作業物件，確保主程式退出時，所有子進程被作業系統強制瞬間連帶殺死
func (km *KernelManager) InitJobObject() {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}

	// 設置常數：當 Job Object 的最後一個控制代碼關閉時，殺死該 Job 內的所有進程
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}

	_, err = windows.SetInformationJobObject(
		h,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		windows.CloseHandle(h)
		return
	}
	km.hJob = h
}

// CloseJobObject 釋放 Job 物件控制代碼
func (km *KernelManager) CloseJobObject() {
	if km.hJob != 0 {
		windows.CloseHandle(km.hJob)
		km.hJob = 0
	}
}

// ==========================================
// 進程快照與強制清理工具 (系統底層封裝)
// ==========================================

// IsProcessRunning 透過 Windows 工具幫手快照，檢查全域是否有指定的進程正在運行（排除自身）
func (km *KernelManager) IsProcessRunning(name string) bool {
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(h, &pe); err != nil {
		return false
	}

	myPid := uint32(os.Getpid())
	for {
		if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), name) {
			if pe.ProcessID != myPid {
				return true
			}
		}
		if err := windows.Process32Next(h, &pe); err != nil {
			break
		}
	}
	return false
}

// KillProcessByName 根據進程名稱，暴力且乾淨地強殺歷史殘留或卡死的進程
func (km *KernelManager) KillProcessByName(name string) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return
	}
	defer windows.CloseHandle(snapshot)

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))

	if err := windows.Process32First(snapshot, &pe); err != nil {
		return
	}

	for {
		if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), name) {
			pid := pe.ProcessID
			if pid != uint32(os.Getpid()) {
				// 開啟進程時同時請求 TERMINATE 權限
				h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_TERMINATE, false, pid)
				if err == nil {
					_ = windows.TerminateProcess(h, 9) // 強制強殺
					windows.CloseHandle(h)             // 🛠️ 【核心優化】原版未及時釋放控制代碼，此處必須顯式關閉以防止控制代碼洩漏
				}
			}
		}
		if err := windows.Process32Next(snapshot, &pe); err != nil {
			break
		}
	}
}

// ==========================================
// 工業級進程守合進程 (Daemon)
// ==========================================

// MonitorKernelDaemon 核心守護進程，運作於獨立的 Goroutine 中，負責進程崩潰重啟與接管
func (km *KernelManager) MonitorKernelDaemon() {
	target := filepath.Join(km.cm.BaseDir(), "mihomo.exe")
	absBaseDir, _ := filepath.Abs(km.cm.BaseDir())

	for {
		// 1. 安全退出檢查：若托盤發出完全退出指令，立刻終止守護
		if km.cm.IsReallyExiting() {
			return
		}

		// 2. 接管外部進行檢查：若發現外部已有運作中的 mihomo.exe，則不重複拉起
		if km.kmIsProcessRunningActive("mihomo.exe") {
			if km.cm.IsSystemInitializing() && !km.cm.IsSyncing() {
				km.cm.SetSystemInitializing(false)
			}
			// 採取 5 秒低頻輕量級輪詢，徹底解脫 CPU 負載
			time.Sleep(5 * time.Second)
			continue
		}

		// 3. 準備親自拉起內核：嚴密同步內部狀態機
		km.cm.SetSystemInitializing(true)
		km.cm.SetHasFirstSynced(false)
		km.cm.SetKernelActive(false)

		// 清理卡死的歷史残留或佔用了通訊埠的僵屍內核
		km.KillProcessByName("mihomo.exe")
		time.Sleep(300 * time.Millisecond)

		// 4. 建構靜默拉起命令（不彈出 Windows 黑色 CMD 視窗）
		cmd := exec.Command(target, "-d", ".")
		cmd.Dir = absBaseDir
		cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}

		if err := cmd.Start(); err == nil {
			km.cm.SetKernelActive(true)

			// 進程拉起成功，立刻執行生命週期回呼 A（配置嗅探）
			if km.hooks.OnKernelStarted != nil {
				km.hooks.OnKernelStarted()
			}

			// 5. 將新拉起的進程實時綁定到 Windows Job Object
			if km.hJob != 0 {
				hp, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
				if err == nil {
					_ = windows.AssignProcessToJobObject(km.hJob, hp)
					windows.CloseHandle(hp) // 🛠️ 綁定完成後，立即關閉臨時進程控制代碼
				}
			}

			// 6. 異步就緒等待：1秒後觸發生命週期回呼 B（配置同步與註冊表更新）
			go func() {
				time.Sleep(1000 * time.Millisecond)
				if km.cm.IsKernelActive() && km.hooks.OnKernelReady != nil {
					km.hooks.OnKernelReady()
				}
			}()

			// 7. 阻塞等待進程退出（此處會卡住，直到內核崩潰或被主動殺死）
			_ = cmd.Wait()

			// 內核由於外部原因掛掉（如用戶在任務管理器強殺），解開核心激活狀態
			km.cm.SetKernelActive(false)
		} else {
			// 啟動徹底失敗（如二進位文件損壞、無執行權限）時的故障防禦
			km.cm.SetKernelActive(false)
			km.cm.SetHasFirstSynced(true)
			km.cm.SetSystemInitializing(false)
		}

		// 每次重啟核心之間的防振盪緩衝保護
		time.Sleep(1 * time.Second)
	}
}

// 內部代理包裝，確保程式碼結構一致
func (km *KernelManager) kmIsProcessRunningActive(name string) bool {
	return km.IsProcessRunning(name)
}
