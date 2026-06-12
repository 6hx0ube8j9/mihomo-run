package winapi

import (
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"mihomo-run/config"
)

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
	SM_CXSCREEN    = 0
	SM_CYSCREEN    = 1
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

func CalculateWindowBounds(scrW, scrH int) (winW, winH, winX, winY int) {
	winW, winH = 1280, 768
	if scrW > 0 && scrH > 0 {
		w, h := float64(scrW), float64(scrH)
		aspectRatio := w / h
		switch {
		case scrW >= 3840:
			winW, winH = 1920, 1080
		case aspectRatio > 2.0:
			winW, winH = 1440, 900
		case aspectRatio <= 1.05:
			winW = int(w * 0.85)
			winH = int(h * 0.65)
			if winW < 800 {
				winW = 800
			}
		case scrW >= 2560:
			winW, winH = 1600, 960
		case scrW >= 1920:
			winW, winH = 1280, 800
		case scrW == 1536 && scrH == 864:
			winW, winH = 1150, 680
		case scrW >= 1440:
			winW, winH = 1150, 720
		case scrW == 1366 && scrH == 768:
			winW, winH = 1050, 640
		case scrW <= 1280:
			winW = int(w * 0.92)
			winH = int(h * 0.88)
			if winW < 960 {
				winW = 960
			}
			if winH < 580 {
				winH = 580
			}
		default:
			winW = int(w * 0.75)
			winH = int(h * 0.75)
		}
	}

	winX = (scrW - winW) / 2
	winY = (scrH - winH) / 2
	if winX < 0 {
		winX = 0
	}
	if winY < 0 {
		winY = 0
	}
	return
}

func FocusWindowSilky(targetHwnd uintptr, cm *config.ConfigManager) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if !cm.TryStartFocusing() {
		return
	}
	defer cm.SetFocusing(false)

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

func FindAndFocusChromeWindow(mainPid uint32, cm *config.ConfigManager) bool {
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

				childCount := 0
				child, _, _ := procGetWindow.Call(hwnd, 5)
				for child != 0 {
					childCount++
					if childCount > 5 {
						break
					}
					child, _, _ = procGetWindow.Call(child, 2)
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
