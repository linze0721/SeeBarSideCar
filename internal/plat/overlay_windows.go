package plat

import (
	"image"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"

	"magic/internal/keymap"
)

const (
	wmDestroy        = 0x0002
	wmSize           = 0x0005
	wmKillFocus      = 0x0008
	wmPaint          = 0x000F
	wmClose          = 0x0010
	wmEraseBkgnd     = 0x0014
	wmNCCreate       = 0x0081
	wmNCHitTest      = 0x0084
	wmDPIChanged     = 0x02E0
	wmAppBounds      = 0x8000 + 1
	wmAppSetTitle    = 0x8000 + 2
	csVRedraw        = 0x0001
	csHRedraw        = 0x0002
	wsPopup          = 0x80000000
	wsMinimizeBox    = 0x00020000
	wsSysMenu        = 0x00080000
	wsExAppWindow    = 0x00040000
	htClient         = 1
	swShowNoActivate = 4
	sizeRestored     = 0
	swpNoZOrder      = 0x0004
	swpNoActivate    = 0x0010
	halftone         = 4
	srcCopy          = 0x00CC0020
	blackBrush       = 4
	idcArrow         = 32512
	idiApplication   = 32512
	mkButtons        = 0x0001 | 0x0002 | 0x0010 | 0x0020 | 0x0040
	dwmncrpEnabled   = 2
	dwmwcpRound      = 2
	overlayStyle     = wsPopup | wsMinimizeBox | wsSysMenu // minimize/Close from the taskbar, no frame
	overlayExStyle   = wsExAppWindow
	dwmShadowMargin  = 1
)

type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   uintptr
	icon       uintptr
	cursor     uintptr
	background uintptr
	menuName   *uint16
	className  *uint16
	iconSm     uintptr
}

type paintStruct struct {
	hdc         uintptr
	erase       int32
	rcPaint     windows.Rect
	restore     int32
	incUpdate   int32
	rgbReserved [32]byte
}

var (
	overlayClass     = windows.StringToUTF16Ptr("magicOverlay")
	overlayClassOnce sync.Once
	overlayClassErr  error
	overlayWndProc   = windows.NewCallback(overlayProc)

	// Overlays by HWND; pending holds overlays between CreateWindowEx and
	// WM_NCCREATE, keyed by an id passed as lpParam (not a Go pointer).
	overlays   sync.Map
	pending    sync.Map
	overlayIDs atomic.Uintptr
)

type winOverlay struct {
	hwnd    uintptr
	input   func(Event)
	closing atomic.Bool
	done    chan struct{}

	mu     sync.Mutex
	buf    []byte // BGRA, top-down
	fw, fh int32
	rect   Rect
	title  string

	// Window-thread only.
	cw, ch  int32
	keys    map[uint16]bool
	buttons uint8 // bit (Button-1) set while held
}

var _ Overlay = (*winOverlay)(nil)

type dwmMargins struct{ left, right, top, bottom int32 }

func registerOverlayClass() error {
	overlayClassOnce.Do(func() {
		cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
		icon, _, _ := procLoadIconW.Call(0, idiApplication)
		wc := wndClassEx{
			style:     csHRedraw | csVRedraw,
			wndProc:   overlayWndProc,
			instance:  hinstance,
			icon:      icon,
			cursor:    cursor,
			className: overlayClass,
		}
		wc.size = uint32(unsafe.Sizeof(wc))
		if r, _, e := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
			overlayClassErr = callErr("RegisterClassEx", e)
		}
	})
	return overlayClassErr
}

func (p *winPlat) NewOverlay(title string, rect Rect, input func(Event), closed func()) (Overlay, error) {
	if err := registerOverlayClass(); err != nil {
		return nil, err
	}
	rect.W, rect.H = max(rect.W, 1), max(rect.H, 1)
	v := &winOverlay{input: input, done: make(chan struct{}), rect: rect, keys: make(map[uint16]bool)}
	titlePtr, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return nil, err
	}
	errc := make(chan error, 1)
	go func() {
		// A window's messages are delivered to the thread that created it.
		runtime.LockOSThread()
		defer close(v.done)
		id := overlayIDs.Add(1)
		pending.Store(id, v)
		hwnd, _, e := procCreateWindowExW.Call(overlayExStyle,
			uintptr(unsafe.Pointer(overlayClass)), uintptr(unsafe.Pointer(titlePtr)),
			overlayStyle, uintptr(rect.X), uintptr(rect.Y), uintptr(rect.W), uintptr(rect.H),
			0, 0, hinstance, id)
		pending.Delete(id)
		if hwnd == 0 {
			errc <- callErr("CreateWindowEx", e)
			return
		}
		v.decorate()
		// Creation may have been DPI-adjusted; pin the exact rect again.
		v.applyBounds()
		// Overlays appear while the user drags on the other machine; taking
		// focus here would steal it from whatever they are typing into.
		procShowWindow.Call(hwnd, swShowNoActivate)
		errc <- nil
		var m winMsg
		for {
			// No TranslateMessage: WM_CHAR/WM_SYSCHAR are unused and would
			// only produce menu beeps for Alt combos.
			r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if r == 0 || int32(r) == -1 {
				break
			}
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
		if !v.closing.Load() && closed != nil {
			closed()
		}
	}()
	if err := <-errc; err != nil {
		return nil, err
	}
	return v, nil
}

func (v *winOverlay) SetFrame(img *image.RGBA) {
	if img == nil {
		return
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return
	}
	v.mu.Lock()
	n := w * h * 4
	if cap(v.buf) < n {
		v.buf = make([]byte, n)
	}
	v.buf = v.buf[:n]
	for y := range h {
		s := img.Pix[img.PixOffset(b.Min.X, b.Min.Y+y):][:w*4]
		d := v.buf[y*w*4:][:w*4]
		for i := 0; i < len(s); i += 4 {
			d[i], d[i+1], d[i+2], d[i+3] = s[i+2], s[i+1], s[i], 255
		}
	}
	v.fw, v.fh = int32(w), int32(h)
	v.mu.Unlock()
	procInvalidateRect.Call(v.hwnd, 0, 0)
}

// SetBounds is posted like SetTitle; the window thread applies it.
func (v *winOverlay) SetBounds(rect Rect) {
	rect.W, rect.H = max(rect.W, 1), max(rect.H, 1)
	v.mu.Lock()
	changed := rect != v.rect
	v.rect = rect
	v.mu.Unlock()
	if changed {
		procPostMessageW.Call(v.hwnd, wmAppBounds, 0, 0)
	}
}

// SetTitle is posted rather than calling SetWindowText cross-thread, which
// would block on the window thread (possibly busy inside input()).
func (v *winOverlay) SetTitle(title string) {
	v.mu.Lock()
	v.title = title
	v.mu.Unlock()
	procPostMessageW.Call(v.hwnd, wmAppSetTitle, 0, 0)
}

func (v *winOverlay) Close() {
	select {
	case <-v.done:
		return
	default:
	}
	v.closing.Store(true)
	procPostMessageW.Call(v.hwnd, wmClose, 0, 0)
}

// decorate gives the frameless popup a DWM shadow (forced non-client
// rendering + 1px extended frame) and, on Windows 11, rounded corners.
// Failures only cost looks.
func (v *winOverlay) decorate() {
	h := windows.HWND(v.hwnd)
	policy := uint32(dwmncrpEnabled)
	windows.DwmSetWindowAttribute(h, windows.DWMWA_NCRENDERING_POLICY, unsafe.Pointer(&policy), 4)
	if procDwmExtendFrameIntoClientArea.Find() == nil {
		m := dwmMargins{dwmShadowMargin, dwmShadowMargin, dwmShadowMargin, dwmShadowMargin}
		procDwmExtendFrameIntoClientArea.Call(v.hwnd, uintptr(unsafe.Pointer(&m)))
	}
	corner := uint32(dwmwcpRound)
	windows.DwmSetWindowAttribute(h, windows.DWMWA_WINDOW_CORNER_PREFERENCE, unsafe.Pointer(&corner), 4)
}

// applyBounds moves/sizes the window (no non-client area: window rect ==
// client rect) to the current rect.
func (v *winOverlay) applyBounds() {
	if iconic, _, _ := procIsIconic.Call(v.hwnd); iconic != 0 {
		return // would move the taskbar icon placeholder; redone on restore
	}
	v.mu.Lock()
	r := v.rect
	v.mu.Unlock()
	procSetWindowPos.Call(v.hwnd, 0, uintptr(r.X), uintptr(r.Y), uintptr(r.W), uintptr(r.H), swpNoZOrder|swpNoActivate)
}

// toScreen maps client coordinates to global (physical pixel) coordinates.
func (v *winOverlay) toScreen(lParam uintptr) (int32, int32) {
	pt := point{int32(int16(lParam)), int32(int16(lParam >> 16))}
	procClientToScreen.Call(v.hwnd, uintptr(unsafe.Pointer(&pt)))
	return pt.X, pt.Y
}

func (v *winOverlay) button(b uint8, down bool, wParam uintptr) {
	bit := uint8(1) << (b - 1)
	if down {
		v.buttons |= bit
		procSetCapture.Call(v.hwnd)
	} else {
		if v.buttons&bit == 0 {
			return // press happened outside the overlay
		}
		v.buttons &^= bit
		if wParam&mkButtons == 0 {
			procReleaseCapture.Call()
		}
	}
	v.input(Event{Kind: MouseButton, Button: b, Down: down})
}

// releaseAll lifts everything held so the remote side doesn't end up with
// stuck keys/buttons when focus moves away mid-press (e.g. Alt+Tab).
func (v *winOverlay) releaseAll() {
	for hid := range v.keys {
		v.input(Event{Kind: Key, HID: hid})
	}
	clear(v.keys)
	for b := ButtonLeft; b <= ButtonForward; b++ {
		if v.buttons&(1<<(b-1)) != 0 {
			v.input(Event{Kind: MouseButton, Button: b})
		}
	}
	v.buttons = 0
}

func (v *winOverlay) paint() {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(v.hwnd, uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}
	defer procEndPaint.Call(v.hwnd, uintptr(unsafe.Pointer(&ps)))
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.buf) == 0 {
		brush, _, _ := procGetStockObject.Call(blackBrush)
		procFillRect.Call(hdc, uintptr(unsafe.Pointer(&ps.rcPaint)), brush)
		return
	}
	bi := newBitmapInfo(v.fw, v.fh)
	bits, info := uintptr(unsafe.Pointer(&v.buf[0])), uintptr(unsafe.Pointer(&bi))
	if v.cw == v.fw && v.ch == v.fh {
		procSetDIBitsToDevice.Call(hdc, 0, 0, uintptr(v.fw), uintptr(v.fh),
			0, 0, 0, uintptr(v.fh), bits, info, dibRGBColors)
		return
	}
	procSetStretchBltMode.Call(hdc, halftone)
	procSetBrushOrgEx.Call(hdc, 0, 0, 0) // required after selecting HALFTONE
	procStretchDIBits.Call(hdc, 0, 0, uintptr(v.cw), uintptr(v.ch),
		0, 0, uintptr(v.fw), uintptr(v.fh), bits, info, dibRGBColors, srcCopy)
}

func overlayProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	if msg == wmNCCreate {
		// CREATESTRUCT.lpCreateParams is the first field.
		id := *(*uintptr)(uptr(lParam))
		if pv, ok := pending.Load(id); ok {
			v := pv.(*winOverlay)
			v.hwnd = hwnd
			overlays.Store(hwnd, v)
		}
	}
	vv, ok := overlays.Load(hwnd)
	if !ok {
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
		return r
	}
	v := vv.(*winOverlay)
	switch msg {
	case wmPaint:
		v.paint()
		return 0
	case wmEraseBkgnd:
		return 1 // paint covers the whole client area; avoids flicker
	case wmSize:
		v.cw, v.ch = int32(lParam&0xFFFF), int32(lParam>>16&0xFFFF)
		if wParam == sizeRestored {
			// Catches SetBounds that arrived while minimized.
			var wr windows.Rect
			procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&wr)))
			v.mu.Lock()
			r := v.rect
			v.mu.Unlock()
			if (Rect{X: wr.Left, Y: wr.Top, W: wr.Right - wr.Left, H: wr.Bottom - wr.Top}) != r {
				procPostMessageW.Call(hwnd, wmAppBounds, 0, 0)
			}
		}
		return 0
	case wmAppBounds:
		v.applyBounds()
		return 0
	case wmDPIChanged:
		return 0 // the rect is in physical pixels already; keep it as set
	case wmNCHitTest:
		return htClient // no OS dragging/resizing; the owner's window drives the rect
	case wmAppSetTitle:
		v.mu.Lock()
		t, err := windows.UTF16PtrFromString(v.title)
		v.mu.Unlock()
		if err == nil {
			procSetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(t)))
		}
		return 0
	case wmMouseMove:
		x, y := v.toScreen(lParam)
		v.input(Event{Kind: MouseMove, X: x, Y: y})
		return 0
	case wmLButtonDown, wmLButtonUp:
		v.button(ButtonLeft, msg == wmLButtonDown, wParam)
		return 0
	case wmRButtonDown, wmRButtonUp:
		v.button(ButtonRight, msg == wmRButtonDown, wParam)
		return 0
	case wmMButtonDown, wmMButtonUp:
		v.button(ButtonMiddle, msg == wmMButtonDown, wParam)
		return 0
	case wmXButtonDown, wmXButtonUp:
		switch wParam >> 16 & 0xFFFF {
		case xbutton1:
			v.button(ButtonBack, msg == wmXButtonDown, wParam)
		case xbutton2:
			v.button(ButtonForward, msg == wmXButtonDown, wParam)
		}
		return 1 // XBUTTON messages must return TRUE
	case wmMouseWheel:
		v.input(Event{Kind: Wheel, DY: int32(int16(wParam >> 16))})
		return 0
	case wmMouseHWheel:
		v.input(Event{Kind: Wheel, DX: int32(int16(wParam >> 16))})
		return 1 // TRUE marks horizontal wheel as handled
	case wmKeyDown, wmKeyUp, wmSysKeyDown, wmSysKeyUp:
		sc := keymap.WinScan{Code: uint16(lParam >> 16 & 0xFF), Extended: lParam>>24&1 != 0}
		if sc.Code == 0 {
			r, _, _ := procMapVirtualKeyW.Call(wParam, mapvkVKToVSCEx)
			sc = keymap.WinScan{Code: uint16(r & 0xFF), Extended: r>>8 == 0xE0}
		}
		if hid, ok := keymap.WinToHID(sc); ok {
			down := msg == wmKeyDown || msg == wmSysKeyDown
			if down {
				v.keys[hid] = true
			} else {
				delete(v.keys, hid)
			}
			v.input(Event{Kind: Key, HID: hid, Down: down})
		}
		return 0 // also keeps Alt/F10 from entering the system menu
	case wmKillFocus:
		v.releaseAll()
		return 0
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		overlays.Delete(hwnd)
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}
