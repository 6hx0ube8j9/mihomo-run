package kernel

import (
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 保持原有的接口定义，确保 main.go 不会报错
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
	OnKernelReady   func() // 暂留，如果之前代码有用到
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

// InitJobObject 初始化 Windows 作业对象 (极佳的实践，防止产生孤儿进程)
func (km *KernelManager) InitJobObject() {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		log.Printf("[Kernel] Failed to create Job Object: %v", err)
		return
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, // 主进程死，子进程必死
		},
	}

	_, err = windows.SetInformationJobObject(
		h,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err == nil {
		km.hJob = h
	}
}

// MonitorKernelDaemon 核心优化：拥抱事件驱动，彻底告别盲目的 Sleep 轮询
func (km *KernelManager) MonitorKernelDaemon() {
	absBaseDir, _ := filepath.Abs(km.cm.BaseDir())
	target := filepath.Join(absBaseDir, "mihomo.exe")

	for {
		// 1. 检查是否正在退出
		if km.cm.IsReallyExiting() {
			return
		}

		// 2. 启动前，先大扫除，确保没有残留的僵尸进程占用端口
		km.KillProcessByName("mihomo.exe")
		time.Sleep(300 * time.Millisecond)

		// 3. 准备启动内核
		km.cm.SetSystemInitializing(true)
		km.cm.SetHasFirstSynced(false)
		km.cm.SetKernelActive(false)

		cmd := exec.Command(target, "-d", ".")
		cmd.Dir = absBaseDir
		cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}

		// 4. 尝试启动
		if err := cmd.Start(); err != nil {
			log.Printf("[Kernel] Failed to start mihomo: %v", err)
			time.Sleep(2 * time.Second) // 启动失败才等 2 秒重试
			continue
		}

		// 启动成功，更新状态并触发回调
		km.cm.SetKernelActive(true)
		log.Println("[Kernel] mihomo.exe started successfully.")
		if km.hooks.OnKernelStarted != nil {
			km.hooks.OnKernelStarted()
		}

		// 将进程绑定到 Job Object
		if km.hJob != 0 {
			hp, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
			if err == nil {
				_ = windows.AssignProcessToJobObject(km.hJob, hp)
				windows.CloseHandle(hp)
			}
		}

		// 5. 核心魔法：使用 channel 接收进程退出的信号
		processDone := make(chan error, 1)
		go func() {
			processDone <- cmd.Wait() // 阻塞直到进程自己死掉（OS级别事件，不耗CPU）
		}()

		// 6. 监听循环（0延迟响应）
		ticker := time.NewTicker(500 * time.Millisecond)
	WaitLoop:
		for {
			select {
			case <-processDone:
				// 事件A：内核进程崩溃或意外退出了！立刻跳出循环去重启
				log.Println("[Kernel] mihomo.exe exited.")
				break WaitLoop

			case <-ticker.C:
				// 事件B：定期检查前端是否下达了退出指令或需要更新初始化状态
				if km.cm.IsReallyExiting() {
					// 收到退出指令，主动杀掉进程，这会导致上面的 processDone 收到信号并清理
					km.KillProcessByName("mihomo.exe")
					ticker.Stop()
					return
				}

				// 同步初始化状态（保留你原有的业务逻辑）
				if km.cm.IsSystemInitializing() && !km.cm.IsSyncing() {
					km.cm.SetSystemInitializing(false)
				}
			}
		}
		ticker.Stop()
		km.cm.SetKernelActive(false)
	}
}

// KillProcessByName 通过 taskkill 强制清理指定进程
func (km *KernelManager) KillProcessByName(exeName string) {
	cmd := exec.Command("taskkill", "/F", "/T", "/IM", exeName)
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	_ = cmd.Run()
}

// kmIsProcessRunningActive 检查进程是否存活
func (km *KernelManager) kmIsProcessRunningActive(exeName string) bool {
	cmd := exec.Command("tasklist", "/FI", "IMAGENAME eq "+exeName, "/NH")
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), exeName)
}
