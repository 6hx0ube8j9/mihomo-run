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

// 透過 LazyDLL 載入 Windows 系統核心庫
var (
	u32      = windows.NewLazySystemDLL("user32.dll")
	k32      = windows.NewLazySystemDLL("kernel32.dll")
	wininet  = windows.NewLazySystemDLL("wininet.dll")
	imghelp  = windows.NewLazySystemDLL("imagehlp.dll")
	setOption = wininet.NewProc("InternetSetOptionW")
)

// 收攏所有 Win32 API 過程調用
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
)

// 視窗定位常數
const (
	SW_RESTORE     = 9
	SWP_NOSIZE     = 0x0001
	SWP_NOMOVE     = 0x0002
	SWP_SHOWWINDOW = 0x0040
	SWP_SILKY      = SWP_NOSIZE | SWP_NOMOVE | SWP_SHOWWINDOW
	SM_CXSCREEN    = 0 // 主顯示器寬度
	SM_CYSCREEN    = 1 // 主顯示器高度
)

// 使用原子變數安全隔離快取的 WebUI 視窗控制代碼（Hwnd）
var cachedWebUIHwnd atomic.Uintptr

func init() {
	// 還原原版高 DPI 意識初始化，確保外觀在 2K/4K 螢幕下不模糊
	procSetContext := u32.NewProc("SetProcessDpiAwarenessContext")
	_, _, err := procSetContext.Call(uintptr(0xfffffffc)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2
	if err == nil || uint32(err.(syscall.Errno)) == 0 {
		return
	}
	procSetAware := u32.NewProc("SetProcessDPIAware")
	_, _, _ = procSetAware.Call()
}

// ==========================================
// 快取視窗控制代碼的原子讀寫方法（徹底消除 Data Race）
// ==========================================

// GetCachedWebUIHwnd 獲取快取的網頁視窗控制代碼
func GetCachedWebUIHwnd() uintptr {
	return cachedWebUIHwnd.Load()
}

// SetCachedWebUIHwnd 設定快取的網頁視窗控制代碼
func SetCachedWebUIHwnd(hwnd uintptr) {
	cachedWebUIHwnd.Store(hwnd)
}

// IsWindowVisible 安全包裝 Win32 API 檢查視窗是否可見
func IsWindowVisible(hwnd uintptr) bool {
	vis, _, _ := procIsWindowVisible.Call(hwnd)
	return vis != 0
}

// GetSystemMetrics 獲取 Windows 系統度量指標（如螢幕解析度）
func GetSystemMetrics(index int) int {
	res, _, _ := procGetSystemMetrics.Call(uintptr(index))
	return int(res)
}

// RefreshInternetOptions 重新整理系統網絡選項（通知系統代理已變更）
func RefreshInternetOptions() {
	_, _, _ = setOption.Call(0, 37, 0, 0) // INTERNET_OPTION_REFRESH
	_, _, _ = setOption.Call(0, 39, 0, 0) // INTERNET_OPTION_SETTINGS_CHANGED
}

// ==========================================
// 執行緒安全重構：絲滑視窗聚焦
// ==========================================

// configContextInterface 用於打破循環依賴，接收來自 pkg/config 的管理器
type configContextInterface interface {
	CompareAndSwapFocusing(oldVal, newVal int32) bool
	SetFocusing(val int32)
}

// FocusWindowSilky 將指定視窗強制且絲滑地置頂聚焦（已修正執行緒斷鏈隱患）
func FocusWindowSilky(targetHwnd uintptr, cm configContextInterface) {
	// 1. 【核心修復】強制鎖定目前 Goroutine 到作業系統固定執行緒
	// 確保 AttachThreadInput 與解綁操作發生在同一個 Win32 執行緒上下文中
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

	// 模擬按下 Alt 鍵，繞過 Windows 的 SetForegroundWindow 權限限制
	procKeybdEvent.Call(0x12, 0, 0, 0)

	// 與目前前景視窗執行緒綁定輸入佇列
	if foreT != currT && foreT != 0 {
		procAttachThread.Call(foreT, currT, 1)
	}
	if targT != 0 && targT != currT {
		procAttachThread.Call(currT, targT, 1)
	}

	// 顯示並啟用視窗
	procShowWindow.Call(targetHwnd, SW_RESTORE)

	winuser := windows.NewLazySystemDLL("user32.dll")
	switchToThisWindow := winuser.NewProc("SwitchToThisWindow")
	_, _, _ = switchToThisWindow.Call(targetHwnd, 1)

	procSetForeground.Call(targetHwnd)
	procBringToTop.Call(targetHwnd)

	// 強制頂層置頂（HWND_TOPMOST = -1）
	procSetWindowPos.Call(targetHwnd, uintptr(0xFFFFFFFFFFFFFFFF), 0, 0, 0, 0, SWP_SILKY)

	// 【解綁操作】因為 LockOSThread 保護，此處 currT 必定與上方一致，完美防卡死
	if targT != 0 && targT != currT {
		procAttachThread.Call(currT, targT, 0)
	}
	if foreT != currT && foreT != 0 {
		procAttachThread.Call(foreT, currT, 0)
	}

	// 放開 Alt 鍵
	procKeybdEvent.Call(0x12, 0, 2, 0)

	// 400 毫秒後解除強制最高置頂（HWND_NOTOPMOST = -2），還原正常的視窗層級，防止遮擋其他軟體
	time.AfterFunc(400*time.Millisecond, func() {
		procSetWindowPos.Call(targetHwnd, uintptr(0xFFFFFFFFFFFFFFFE), 0, 0, 0, 0, SWP_SILKY)
	})
}

// ==========================================
// 視窗非同步列舉與快取定位
// ==========================================

// FindAndFocusChromeWindow 非同步尋找並聚焦符合特定特徵的 Chrome 核心視窗
func FindAndFocusChromeWindow(mainPid uint32, cm configContextInterface) bool {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var foundHwnd uintptr

	// 調用 EnumWindows 列舉所有頂層視窗
	procEnumWindows.Call(windows.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		if IsWindowVisible(hwnd) {
			var wndPid uint32
			_, _, _ = procGetWindowThread.Call(hwnd, uintptr(unsafe.Pointer(&wndPid)))

			var buf [256]uint16
			_, _, _ = procGetClassName.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), 256)
			className := windows.UTF16ToString(buf[:])

			// 篩選 Chrome 核心瀏覽器的視窗類別名
			if className == "Chrome_WidgetWin_1" {
				// 情況 A：視窗 PID 直接匹配主進程 PID
				if wndPid == mainPid {
					foundHwnd = hwnd
					SetCachedWebUIHwnd(hwnd)
					return 0 // 找到目標，停止列舉
				}

				// 情況 B：深度遍歷子視窗，還原原版對 Mihomo WebUI 標題特徵的防禦性嗅探
				childCount := 0
				child, _, _ := procGetWindow.Call(hwnd, 5) // GW_CHILD
				for child != 0 {
					childCount++
					if childCount > 5 {
						break
					}
					child, _, _ = procGetWindow.Call(child, 2) // GW_HWNDNEXT
				}

				if childCount <= 5 {
					var titleBuf [512]uint16
					_, _, _ = procGetWindowText.Call(hwnd, uintptr(unsafe.Pointer(&titleBuf[0])), 512)
					wndTitle := strings.ToLower(windows.UTF16ToString(titleBuf[:]))

					// 匹配 WebUI 特有關鍵字
					if strings.Contains(wndTitle, "ui") || strings.Contains(wndTitle, "dashboard") || strings.Contains(wndTitle, "proxies") {
						foundHwnd = hwnd
						SetCachedWebUIHwnd(hwnd)
						return 0 // 找到目標，停止列舉
					}
				}
			}
		}
		return 1 // 繼續列举
	}), 0)

	if foundHwnd != 0 {
		FocusWindowSilky(foundHwnd, cm)
		return true
	}
	return false
}
