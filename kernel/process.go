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

func (km *KernelManager) CloseJobObject() {
	if km.hJob != 0 {
		windows.CloseHandle(km.hJob)
		km.hJob = 0
	}
}

// 坚持使用最高效的原生 Win32 API 遍历进程快照
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

// 使用原生 API 发送终结信号，精确且无额外开销
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

		// 1. 若外部进程已经存在，则保持观察
		if km.kmIsProcessRunningActive("mihomo.exe") {
			if km.cm.IsSystemInitializing() && !km.cm.IsSyncing() {
				km.cm.SetSystemInitializing(false)
			}
			time.Sleep(2 * time.Second)
			continue
		}

		// 2. 准备启动流程
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

		// 等待内核端口初始化的合理时延
		go func() {
			time.Sleep(1000 * time.Millisecond)
			if km.cm.IsKernelActive() && km.hooks.OnKernelReady != nil {
				km.hooks.OnKernelReady()
			}
		}()

		// 4. 利用通道实现非阻塞的进程结束监控
		processDone := make(chan error, 1)
		go func() {
			processDone <- cmd.Wait()
		}()

		ticker := time.NewTicker(500 * time.Millisecond)
	WaitLoop:
		for {
			select {
			case <-processDone:
				// 内核异常崩溃，跳出循环进行重启
				break WaitLoop
			case <-ticker.C:
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
		time.Sleep(1 * time.Second) // 避免因启动配置错误导致死循环飙升 CPU
	}
}

func (km *KernelManager) kmIsProcessRunningActive(name string) bool {
	return km.IsProcessRunning(name)
}
