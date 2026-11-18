package plat

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	shcore   = windows.NewLazySystemDLL("shcore.dll")
	dwmapi   = windows.NewLazySystemDLL("dwmapi.dll")

	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procGetSystemMetrics              = user32.NewProc("GetSystemMetrics")
	procGetCursorPos                  = user32.NewProc("GetCursorPos")
	procSetCursorPos                  = user32.NewProc("SetCursorPos")
	procSetWindowsHookExW             = user32.NewProc("SetWindowsHookExW")
	procCallNextHookEx                = user32.NewProc("CallNextHookEx")
	procGetMessageW                   = user32.NewProc("GetMessageW")
	procDispatchMessageW              = user32.NewProc("DispatchMessageW")
	procSendInput                     = user32.NewProc("SendInput")
	procMapVirtualKeyW                = user32.NewProc("MapVirtualKeyW")
	procCreateCursor                  = user32.NewProc("CreateCursor")
	procDestroyCursor                 = user32.NewProc("DestroyCursor")
	procSetSystemCursor               = user32.NewProc("SetSystemCursor")
	procSystemParametersInfoW         = user32.NewProc("SystemParametersInfoW")
	procOpenClipboard                 = user32.NewProc("OpenClipboard")
	procCloseClipboard                = user32.NewProc("CloseClipboard")
	procEmptyClipboard                = user32.NewProc("EmptyClipboard")
	procGetClipboardData              = user32.NewProc("GetClipboardData")
	procSetClipboardData              = user32.NewProc("SetClipboardData")
	procIsClipboardFormatAvailable    = user32.NewProc("IsClipboardFormatAvailable")
	procGetClipboardSequenceNumber    = user32.NewProc("GetClipboardSequenceNumber")
	procGetWindow                     = user32.NewProc("GetWindow")
	procGetWindowLongW                = user32.NewProc("GetWindowLongW")
	procIsIconic                      = user32.NewProc("IsIconic")
	procGetWindowTextLengthW          = user32.NewProc("GetWindowTextLengthW")
	procGetWindowTextW                = user32.NewProc("GetWindowTextW")
	procGetWindowRect                 = user32.NewProc("GetWindowRect")
	procPrintWindow                   = user32.NewProc("PrintWindow")
	procShowWindow                    = user32.NewProc("ShowWindow")
	procSetForegroundWindow           = user32.NewProc("SetForegroundWindow")
	procBringWindowToTop              = user32.NewProc("BringWindowToTop")
	procAttachThreadInput             = user32.NewProc("AttachThreadInput")
	procRegisterClassExW              = user32.NewProc("RegisterClassExW")
	procCreateWindowExW               = user32.NewProc("CreateWindowExW")
	procDefWindowProcW                = user32.NewProc("DefWindowProcW")
	procDestroyWindow                 = user32.NewProc("DestroyWindow")
	procPostMessageW                  = user32.NewProc("PostMessageW")
	procPostQuitMessage               = user32.NewProc("PostQuitMessage")
	procBeginPaint                    = user32.NewProc("BeginPaint")
	procEndPaint                      = user32.NewProc("EndPaint")
	procInvalidateRect                = user32.NewProc("InvalidateRect")
	procSetWindowPos                  = user32.NewProc("SetWindowPos")
	procGetDpiForWindow               = user32.NewProc("GetDpiForWindow")
	procGetMonitorInfoW               = user32.NewProc("GetMonitorInfoW")
	procEnumDisplayMonitors           = user32.NewProc("EnumDisplayMonitors")
	procEnumDisplayDevicesW           = user32.NewProc("EnumDisplayDevicesW")
	procEnumDisplaySettingsW          = user32.NewProc("EnumDisplaySettingsW")
	procChangeDisplaySettingsExW      = user32.NewProc("ChangeDisplaySettingsExW")
	procGetDisplayConfigBufferSizes   = user32.NewProc("GetDisplayConfigBufferSizes")
	procQueryDisplayConfig            = user32.NewProc("QueryDisplayConfig")
	procDisplayConfigGetDeviceInfo    = user32.NewProc("DisplayConfigGetDeviceInfo")
	procDisplayConfigSetDeviceInfo    = user32.NewProc("DisplayConfigSetDeviceInfo")
	procClientToScreen                = user32.NewProc("ClientToScreen")
	procSetWindowTextW                = user32.NewProc("SetWindowTextW")
	procLoadCursorW                   = user32.NewProc("LoadCursorW")
	procLoadIconW                     = user32.NewProc("LoadIconW")
	procSetCapture                    = user32.NewProc("SetCapture")
	procReleaseCapture                = user32.NewProc("ReleaseCapture")
	procFillRect                      = user32.NewProc("FillRect")

	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procStretchDIBits      = gdi32.NewProc("StretchDIBits")
	procSetDIBitsToDevice  = gdi32.NewProc("SetDIBitsToDevice")
	procSetStretchBltMode  = gdi32.NewProc("SetStretchBltMode")
	procSetBrushOrgEx      = gdi32.NewProc("SetBrushOrgEx")
	procGetStockObject     = gdi32.NewProc("GetStockObject")
	procGdiFlush           = gdi32.NewProc("GdiFlush")

	procGlobalAlloc           = kernel32.NewProc("GlobalAlloc")
	procGlobalFree            = kernel32.NewProc("GlobalFree")
	procGlobalLock            = kernel32.NewProc("GlobalLock")
	procGlobalUnlock          = kernel32.NewProc("GlobalUnlock")
	procGlobalSize            = kernel32.NewProc("GlobalSize")
	procGetModuleHandleW      = kernel32.NewProc("GetModuleHandleW")
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")

	procSetProcessDpiAwareness = shcore.NewProc("SetProcessDpiAwareness")
	procGetDpiForMonitor       = shcore.NewProc("GetDpiForMonitor")

	procDwmExtendFrameIntoClientArea = dwmapi.NewProc("DwmExtendFrameIntoClientArea")
)

const (
	smCXScreen        = 0
	smCYScreen        = 1
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79

	spiSetCursors = 0x57
)

type point struct{ X, Y int32 }

type winMsg struct {
	hwnd     uintptr
	message  uint32
	wParam   uintptr
	lParam   uintptr
	time     uint32
	pt       point
	lPrivate uint32
}

type winPlat struct {
	ownPID uint32

	capturing atomic.Bool
	fn        func(Event) // set once before the hooks are installed
	grab      atomic.Bool
	parkX     atomic.Int32
	parkY     atomic.Int32
	grabMu    sync.Mutex

	capMu sync.Mutex
	caps  map[uintptr]*winCapture

	vddMu sync.Mutex
	vdd   vdd
}

var _ Platform = (*winPlat)(nil)

var (
	hinstance   uintptr
	ctrlHandler = windows.NewCallback(func(ctrlType uintptr) uintptr {
		restoreCursors()
		return 0 // let the next handler (Go runtime / default) terminate the process
	})
	ctrlOnce sync.Once
)

func New() (Platform, error) {
	if err := setDPIAware(); err != nil {
		return nil, err
	}
	hinstance, _, _ = procGetModuleHandleW.Call(0)
	// A previous run killed while grabbing may have left blank system cursors.
	procSystemParametersInfoW.Call(spiSetCursors, 0, 0, 0)
	ctrlOnce.Do(func() { procSetConsoleCtrlHandler.Call(ctrlHandler, 1) })
	return &winPlat{
		ownPID: windows.GetCurrentProcessId(),
		caps:   make(map[uintptr]*winCapture),
		vdd:    vdd{index: -1},
	}, nil
}

// Run calls app on the calling goroutine: Win32 has no main-thread
// requirement, every window/hook owns its own locked OS thread.
func Run(app func()) error {
	defer restoreCursors()
	app()
	return nil
}

func setDPIAware() error {
	if procSetProcessDpiAwarenessContext.Find() == nil {
		// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 == (HANDLE)-4.
		r, _, e := procSetProcessDpiAwarenessContext.Call(^uintptr(3))
		if r != 0 || errors.Is(e, windows.ERROR_ACCESS_DENIED) { // access denied: already set (manifest)
			return nil
		}
		return fmt.Errorf("plat: SetProcessDpiAwarenessContext: %w", e)
	}
	if procSetProcessDpiAwareness.Find() == nil {
		const processPerMonitorDpiAware = 2
		hr, _, _ := procSetProcessDpiAwareness.Call(processPerMonitorDpiAware)
		if hr == 0 || uint32(hr) == 0x80070005 { // E_ACCESSDENIED: already set
			return nil
		}
		return fmt.Errorf("plat: SetProcessDpiAwareness: HRESULT %#x", uint32(hr))
	}
	return errors.New("plat: per-monitor DPI awareness unavailable (Windows 8.1+ required)")
}

func (p *winPlat) OS() string { return "windows" }

func sysMetric(i uintptr) int32 {
	r, _, _ := procGetSystemMetrics.Call(i)
	return int32(r)
}

func virtualScreen() Rect {
	return Rect{
		X: sysMetric(smXVirtualScreen), Y: sysMetric(smYVirtualScreen),
		W: sysMetric(smCXVirtualScreen), H: sysMetric(smCYVirtualScreen),
	}
}

func (p *winPlat) CursorPos() (x, y int32) {
	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	return pt.X, pt.Y
}

// uptr turns a Win32 handle/pointer value into an unsafe.Pointer without
// tripping vet's uintptr->Pointer check; the memory is never Go-managed.
func uptr(u uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&u)) }

func callErr(name string, e error) error {
	if e == nil || errors.Is(e, windows.Errno(0)) {
		return fmt.Errorf("plat: %s failed", name)
	}
	return fmt.Errorf("plat: %s: %w", name, e)
}
