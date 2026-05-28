package winapi

import (
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	u32       = windows.NewLazySystemDLL("user32.dll")
	k32       = windows.NewLazySystemDLL("kernel32.dll")
	wininet   = windows.NewLazySystemDLL("wininet.dll")
	setOption = wininet.NewProc("InternetSetOptionW")
)

var (
	procEnumWindows      = u32.NewProc("EnumWindows")
	procGetClassName     = u32.NewProc("GetClassNameW")
	procIsWindowVisible  = u32.NewProc("IsWindowVisible")
	procGetWindowThread  = u32.NewProc("GetWindowThreadProcessId")
	procGetWindow        = u32.NewProc("GetWindow")
	procGetWindowText    = u32.NewProc("GetWindowTextW")
	procSetWindowPos     = u32.NewProc("SetWindowPos")
	procShowWindow       = u32.NewProc("ShowWindow")
	procSetForeground    = u32.NewProc("SetForegroundWindow")
	procBringToTop       = u32.NewProc("BringWindowToTop")
	procGetForeground    = u32.NewProc("GetForegroundWindow")
	procAttachThread     = u32.NewProc("AttachThreadInput")
	procGetCurrentThread = k32.NewProc("GetCurrentThreadId")
	procKeybdEvent       = u32.NewProc("keybd_event")
	procGetSystemMetrics = u32.NewProc("GetSystemMetrics")
	// 优化：提前加载 SwitchToThisWindow
	procSwitchToThisWindow = u32.NewProc("SwitchToThisWindow")
)

const (
	SW_RESTORE   = 9
	SWP_NOSIZE   = 0x0001
	SWP_NOMOVE   = 0x0002
	SWP_SHOWWINDOW = 0x0040
	SWP_SILKY    = SWP_NOSIZE | SWP_NOMOVE | SWP_SHOWWINDOW
)

var cachedWebUIHwnd atomic.Uintptr

func init() {
	procSetContext := u32.NewProc("SetProcessDpiAwarenessContext")
	_, _, err := procSetContext.Call(uintptr(0xfffffffc))
	if err != nil && uint32(err.(syscall.Errno)) != 0 {
		procSetAware := u32.NewProc("SetProcessDPIAware")
		_, _, _ = procSetAware.Call()
	}
}

// ... (GetCachedWebUIHwnd, SetCachedWebUIHwnd, IsWindowVisible 等保持不变) ...

func FocusWindowSilky(targetHwnd uintptr, cm configContextInterface) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if !cm.CompareAndSwapFocusing(0, 1) {
		return
	}
	defer cm.SetFocusing(0)

	currT, _, _ := procGetCurrentThread.Call()
	foreH, _, _ := procGetForeground.Call()
	foreT, _, _ := procGetWindowThread.Call(foreH, 0)
	targT, _, _ := procGetWindowThread.Call(targetHwnd, 0)

	// 模拟 Alt 键按下，防止 Windows 阻止前台焦点切换
	procKeybdEvent.Call(0x12, 0, 0, 0)

	if foreT != currT && foreT != 0 {
		procAttachThread.Call(foreT, currT, 1)
	}
	if targT != 0 && targT != currT {
		procAttachThread.Call(currT, targT, 1)
	}

	procShowWindow.Call(targetHwnd, SW_RESTORE)
	
	// 使用预加载的 procSwitchToThisWindow
	_, _, _ = procSwitchToThisWindow.Call(targetHwnd, 1)

	procSetForeground.Call(targetHwnd)
	procBringToTop.Call(targetHwnd)

	procSetWindowPos.Call(targetHwnd, uintptr(0xFFFFFFFFFFFFFFFF), 0, 0, 0, 0, SWP_SILKY)

	if targT != 0 && targT != currT {
		procAttachThread.Call(currT, targT, 0)
	}
	if foreT != currT && foreT != 0 {
		procAttachThread.Call(foreT, currT, 0)
	}

	procKeybdEvent.Call(0x12, 0, 2, 0)

	time.AfterFunc(400*time.Millisecond, func() {
		procSetWindowPos.Call(targetHwnd, uintptr(0xFFFFFFFFFFFFFFFE), 0, 0, 0, 0, SWP_SILKY)
	})
}
