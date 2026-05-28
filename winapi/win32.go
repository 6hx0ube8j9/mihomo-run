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

type configContextInterface interface {
	CompareAndSwapFocusing(oldVal, newVal int32) bool
	SetFocusing(val int32)
}

var (
	u32       = windows.NewLazySystemDLL("user32.dll")
	k32       = windows.NewLazySystemDLL("kernel32.dll")
	wininet   = windows.NewLazySystemDLL("wininet.dll")
	imghelp   = windows.NewLazySystemDLL("imagehlp.dll")
	setOption = wininet.NewProc("InternetSetOptionW")
)

var (
	procEnumWindows        = u32.NewProc("EnumWindows")
	procGetClassName       = u32.NewProc("GetClassNameW")
	procIsWindowVisible    = u32.NewProc("IsWindowVisible")
	procGetWindowThread    = u32.NewProc("GetWindowThreadProcessId")
	procGetWindow          = u32.NewProc("GetWindow")
	procGetWindowText      = u32.NewProc("GetWindowTextW")
	procSetWindowPos       = u32.NewProc("SetWindowPos")
	procShowWindow         = u32.NewProc("ShowWindow")
	procSetForeground      = u32.NewProc("SetForegroundWindow")
	procBringToTop         = u32.NewProc("BringWindowToTop")
	procGetForeground      = u32.NewProc("GetForegroundWindow")
	procAttachThread       = u32.NewProc("AttachThreadInput")
	procGetCurrentThread   = k32.NewProc("GetCurrentThreadId")
	procKeybdEvent         = u32.NewProc("keybd_event")
	procGetSystemMetrics   = u32.NewProc("GetSystemMetrics")
	procSwitchToThisWindow = u32.NewProc("SwitchToThisWindow")
)

const (
	SW_RESTORE     = 9
	SWP_NOSIZE     = 0x0001
	SWP_NOMOVE     = 0x0002
	SWP_SHOWWINDOW = 0x0040
	SWP_SILKY      = SWP_NOSIZE | SWP_NOMOVE | SWP_SHOWWINDOW
	SM_CXSCREEN    = 0 // 【保留原版】：防止常量丢失报错
	SM_CYSCREEN    = 1 // 【保留原版】：防止常量丢失报错
)

var cachedWebUIHwnd atomic.Uintptr

func init() {
	procSetContext := u32.NewProc("SetProcessDpiAwarenessContext")
	_, _, err := procSetContext.Call(uintptr(0xfffffffc))
	// 【优化吸收】：更规范的错误判断逻辑
	if err != nil && uint32(err.(syscall.Errno)) != 0 {
		procSetAware := u32.NewProc("SetProcessDPIAware")
		_, _, _ = procSetAware.Call()
	}
}

func GetCachedWebUIHwnd() uintptr {
	return cachedWebUIHwnd.Load()
}

func SetCachedWebUIHwnd(hwnd uintptr) {
	cachedWebUIHwnd.Store(hwnd)
}

func IsWindowVisible(hwnd uintptr) bool {
	vis, _, _ := procIsWindowVisible.Call(hwnd)
	return vis != 0
}

func GetSystemMetrics(index int) int {
	res, _, _ := procGetSystemMetrics.Call(uintptr(index))
	return int(res)
}

func RefreshInternetOptions() {
	_, _, _ = setOption.Call(0, 37, 0, 0)
	_, _, _ = setOption.Call(0, 39, 0, 0)
}

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

	procKeybdEvent.Call(0x12, 0, 0, 0)

	if foreT != currT && foreT != 0 {
		procAttachThread.Call(foreT, currT, 1)
	}
	if targT != 0 && targT != currT {
		procAttachThread.Call(currT, targT, 1)
	}

	procShowWindow.Call(targetHwnd, SW_RESTORE)

	// 【优化吸收】：直接调用全局 proc，去掉原本在此处的性能消耗
	procSwitchToThisWindow.Call(targetHwnd, 1)

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

func FindAndFocusChromeWindow(mainPid uint32, cm configContextInterface) bool {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var foundHwnd uintptr

	procEnumWindows.Call(windows.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		if IsWindowVisible(hwnd) {
			var wndPid uint32
			_, _, _ = procGetWindowThread.Call(hwnd, uintptr(unsafe.Pointer(&wndPid)))

			var buf [256]uint16
			_, _, _ = procGetClassName.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), 256)
			className := windows.UTF16ToString(buf[:])

			if className == "Chrome_WidgetWin_1" {
				if wndPid == mainPid {
					foundHwnd = hwnd
					SetCachedWebUIHwnd(hwnd)
					return 0
				}

				// 【致命修复】：恢复原版被误删的子窗口层级穿透搜索！
				// 现代 Chromium 架构的窗口标题通常不在顶层，必须通过 GetWindow 往深层查找
				childCount := 0
				child, _, _ := procGetWindow.Call(hwnd, 5) // 5 = GW_CHILD
				for child != 0 {
					childCount++
					if childCount > 5 {
						break
					}
					child, _, _ = procGetWindow.Call(child, 2) // 2 = GW_HWNDNEXT
				}

				if childCount <= 5 {
					var titleBuf [512]uint16
					_, _, _ = procGetWindowText.Call(hwnd, uintptr(unsafe.Pointer(&titleBuf[0])), 512)
					wndTitle := strings.ToLower(windows.UTF16ToString(titleBuf[:]))

					if strings.Contains(wndTitle, "ui") || strings.Contains(wndTitle, "dashboard") || strings.Contains(wndTitle, "proxies") {
						foundHwnd = hwnd
						SetCachedWebUIHwnd(hwnd)
						return 0
					}
				}
			}
		}
		return 1
	}), 0)

	if foundHwnd != 0 {
		FocusWindowSilky(foundHwnd, cm)
		return true
	}
	return false
}
