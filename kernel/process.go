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

type KernelHooks struct {
	OnKernelStarted func()
	OnKernelReady   func()
}

type KernelManager struct {
	hJob  windows.Handle
	cm    ConfigContextInterface
	hooks KernelHooks
}

func NewKernelManager(cm ConfigContextInterface, hooks KernelHooks) *KernelManager {
	return &KernelManager{
		cm:    cm,
		hooks: hooks,
	}
}

func (km *KernelManager) InitJobObject() {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}

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

// 【保留原版】：防止其他文件调用报错
func (km *KernelManager) CloseJobObject() {
	if km.hJob != 0 {
		windows.CloseHandle(km.hJob)
		km.hJob = 0
	}
}

// 【极度关键的保留】：坚决使用原版的高性能 Win32 API 遍历进程
// 绝不能使用优化版那种 exec.Command("tasklist") 的低效做法
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

// 【极度关键的保留】：使用原生 API 杀进程，安静且高效
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
				h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_TERMINATE, false, pid)
				if err == nil {
					_ = windows.TerminateProcess(h, 9)
					windows.CloseHandle(h)
				}
			}
		}
		if err := windows.Process32Next(snapshot, &pe); err != nil {
			break
		}
	}
}

func (km *KernelManager) MonitorKernelDaemon() {
	target := filepath.Join(km.cm.BaseDir(), "mihomo.exe")
	absBaseDir, _ := filepath.Abs(km.cm.BaseDir())

	for {
		if km.cm.IsReallyExiting() {
			return
		}

		// 1. 如果检测到外部已经有进程在跑（可能是之前遗留或者独立启动的），保持观察
		if km.kmIsProcessRunningActive("mihomo.exe") {
			if km.cm.IsSystemInitializing() && !km.cm.IsSyncing() {
				km.cm.SetSystemInitializing(false)
			}
			time.Sleep(2 * time.Second) // 缩短休眠，提高响应速度
			continue
		}

		// 2. 准备启动流程（恢复原版严谨的状态管理）
		km.cm.SetSystemInitializing(true)
		km.cm.SetHasFirstSynced(false)
		km.cm.SetKernelActive(false)

		km.KillProcessByName("mihomo.exe")
		time.Sleep(300 * time.Millisecond)

		cmd := exec.Command(target, "-d", ".")
		cmd.Dir = absBaseDir
		cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}

		if err := cmd.Start(); err != nil {
			km.cm.SetKernelActive(false)
			km.cm.SetHasFirstSynced(true)
			km.cm.SetSystemInitializing(false)
			time.Sleep(1 * time.Second)
			continue
		}

		// 3. 启动成功，更新状态并触发回调
		km.cm.SetKernelActive(true)
		if km.hooks.OnKernelStarted != nil {
			km.hooks.OnKernelStarted()
		}

		if km.hJob != 0 {
			hp, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
			if err == nil {
				_ = windows.AssignProcessToJobObject(km.hJob, hp)
				windows.CloseHandle(hp)
			}
		}

		// 【修复】：找回被优化版弄丢的 OnKernelReady 延迟通知
		go func() {
			time.Sleep(1000 * time.Millisecond)
			if km.cm.IsKernelActive() && km.hooks.OnKernelReady != nil {
				km.hooks.OnKernelReady()
			}
		}()

		// 【融合优化精华】：引入通道（channel）监控进程退出事件
		processDone := make(chan error, 1)
		go func() {
			processDone <- cmd.Wait() // 阻塞等待底层进程结束，完全不耗费 CPU
		}()

		ticker := time.NewTicker(500 * time.Millisecond)
	WaitLoop:
		for {
			select {
			case <-processDone:
				// 事件驱动：一旦 mihomo 意外崩溃，瞬间捕获跳出循环，实现 0 延迟重启
				break WaitLoop

			case <-ticker.C:
				// 定时检测外部状态（如是否点了退出程序，是否需要同步初始状态）
				if km.cm.IsReallyExiting() {
					km.KillProcessByName("mihomo.exe")
					ticker.Stop()
					return
				}
				if km.cm.IsSystemInitializing() && !km.cm.IsSyncing() {
					km.cm.SetSystemInitializing(false)
				}
			}
		}

		ticker.Stop()
		km.cm.SetKernelActive(false)
		time.Sleep(1 * time.Second) // 避免无限死循环重启导致 CPU 飙升
	}
}

// 【保留原版】：确保接口签名一致
func (km *KernelManager) kmIsProcessRunningActive(name string) bool {
	return km.IsProcessRunning(name)
}
