package plat

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"

	"magic/internal/keymap"
)

// injectMarker tags every event we synthesize (dwExtraInfo) so our own hooks
// can skip it while still seeing input injected by other tools.
const injectMarker uintptr = 0x6D616763 // "magc"

const (
	whKeyboardLL = 13
	whMouseLL    = 14

	wmKeyDown       = 0x0100
	wmKeyUp         = 0x0101
	wmSysKeyDown    = 0x0104
	wmSysKeyUp      = 0x0105
	wmMouseMove     = 0x0200
	wmLButtonDown   = 0x0201
	wmLButtonUp     = 0x0202
	wmRButtonDown   = 0x0204
	wmRButtonUp     = 0x0205
	wmMButtonDown   = 0x0207
	wmMButtonUp     = 0x0208
	wmMouseWheel    = 0x020A
	wmXButtonDown   = 0x020B
	wmXButtonUp     = 0x020C
	wmMouseHWheel   = 0x020E
	llkhfExtended   = 0x01
	llkhfUp         = 0x80
	mapvkVKToVSCEx  = 4
	xbutton1        = 1
	xbutton2        = 2
	inputMouse      = 0
	inputKeyboard   = 1
	keyExtended     = 0x0001
	keyUp           = 0x0002
	keyScanCode     = 0x0008
	vkMenu          = 0x12
	vkPause         = 0x13
	vkNumLock       = 0x90
	mouseMove       = 0x0001
	mouseLeftDown   = 0x0002
	mouseLeftUp     = 0x0004
	mouseRightDown  = 0x0008
	mouseRightUp    = 0x0010
	mouseMiddleDown = 0x0020
	mouseMiddleUp   = 0x0040
	mouseXDown      = 0x0080
	mouseXUp        = 0x0100
	mouseWheel      = 0x0800
	mouseHWheel     = 0x1000
	mouseVirtDesk   = 0x4000
	mouseAbsolute   = 0x8000
)

type msllHook struct {
	pt        point
	mouseData uint32
	flags     uint32
	time      uint32
	extra     uintptr
}

type kbdllHook struct {
	vkCode   uint32
	scanCode uint32
	flags    uint32
	time     uint32
	extra    uintptr
}

type mouseInput struct {
	dx, dy    int32
	mouseData uint32
	flags     uint32
	time      uint32
	extra     uintptr
}

type keybdInput struct {
	vk, scan uint16
	flags    uint32
	time     uint32
	extra    uintptr
}

// winInput is INPUT; MOUSEINPUT is the largest union member, so keyboard
// inputs are written through a keybdInput view of the same storage (its
// dwExtraInfo sits at a different offset, so set it through that view).
type winInput struct {
	typ uint32
	mi  mouseInput
}

// hookPlat is the Platform whose fn the hooks feed. Hook callbacks are a
// finite process-wide resource, hence package-level and created once.
var (
	hookPlat  atomic.Pointer[winPlat]
	mouseHook = windows.NewCallback(mouseHookProc)
	kbdHook   = windows.NewCallback(kbdHookProc)
)

func (p *winPlat) Capture(fn func(Event)) error {
	if !p.capturing.CompareAndSwap(false, true) {
		return errors.New("plat: Capture already running")
	}
	p.fn = fn
	hookPlat.Store(p)
	errc := make(chan error, 1)
	go func() {
		// Low-level hooks are called on the installing thread while it pumps messages.
		runtime.LockOSThread()
		mh, _, e := procSetWindowsHookExW.Call(whMouseLL, mouseHook, hinstance, 0)
		if mh == 0 {
			errc <- callErr("SetWindowsHookEx(WH_MOUSE_LL)", e)
			return
		}
		kh, _, e := procSetWindowsHookExW.Call(whKeyboardLL, kbdHook, hinstance, 0)
		if kh == 0 {
			errc <- callErr("SetWindowsHookEx(WH_KEYBOARD_LL)", e)
			return
		}
		errc <- nil
		var m winMsg
		for {
			r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if r == 0 || int32(r) == -1 {
				return
			}
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()
	if err := <-errc; err != nil {
		hookPlat.Store(nil)
		p.capturing.Store(false)
		return err
	}
	return nil
}

func mouseHookProc(nCode, wParam, lParam uintptr) uintptr {
	p := hookPlat.Load()
	if int32(nCode) < 0 || p == nil {
		r, _, _ := procCallNextHookEx.Call(0, nCode, wParam, lParam)
		return r
	}
	h := (*msllHook)(uptr(lParam))
	if h.extra == injectMarker {
		r, _, _ := procCallNextHookEx.Call(0, nCode, wParam, lParam)
		return r
	}
	wasGrab := p.grab.Load()
	var ev Event
	switch wParam {
	case wmMouseMove:
		ev = Event{Kind: MouseMove, X: h.pt.X, Y: h.pt.Y}
		if wasGrab {
			// The hook runs before the cursor moves, so the delta is
			// against where the cursor is now: usually the park point, but
			// an Inject (window input from the peer) may have moved it.
			// The move SetCursorPos may echo has pt == cursor.
			var cur point
			procGetCursorPos.Call(uintptr(unsafe.Pointer(&cur)))
			ev.DX, ev.DY = h.pt.X-cur.X, h.pt.Y-cur.Y
			if ev.DX == 0 && ev.DY == 0 {
				return 1
			}
			// Re-park so motion never runs into a screen edge.
			procSetCursorPos.Call(uintptr(p.parkX.Load()), uintptr(p.parkY.Load()))
		}
	case wmLButtonDown, wmLButtonUp:
		ev = Event{Kind: MouseButton, Button: ButtonLeft, Down: wParam == wmLButtonDown}
	case wmRButtonDown, wmRButtonUp:
		ev = Event{Kind: MouseButton, Button: ButtonRight, Down: wParam == wmRButtonDown}
	case wmMButtonDown, wmMButtonUp:
		ev = Event{Kind: MouseButton, Button: ButtonMiddle, Down: wParam == wmMButtonDown}
	case wmXButtonDown, wmXButtonUp:
		ev = Event{Kind: MouseButton, Down: wParam == wmXButtonDown}
		switch h.mouseData >> 16 {
		case xbutton1:
			ev.Button = ButtonBack
		case xbutton2:
			ev.Button = ButtonForward
		}
	case wmMouseWheel:
		ev = Event{Kind: Wheel, DY: int32(int16(h.mouseData >> 16))}
	case wmMouseHWheel:
		ev = Event{Kind: Wheel, DX: int32(int16(h.mouseData >> 16))}
	}
	if ev.Kind != 0 && (ev.Kind != MouseButton || ev.Button != 0) {
		p.fn(ev)
	}
	if swallow(p, wasGrab) {
		return 1
	}
	r, _, _ := procCallNextHookEx.Call(0, nCode, wParam, lParam)
	return r
}

func kbdHookProc(nCode, wParam, lParam uintptr) uintptr {
	p := hookPlat.Load()
	if int32(nCode) < 0 || p == nil {
		r, _, _ := procCallNextHookEx.Call(0, nCode, wParam, lParam)
		return r
	}
	h := (*kbdllHook)(uptr(lParam))
	if h.extra == injectMarker {
		r, _, _ := procCallNextHookEx.Call(0, nCode, wParam, lParam)
		return r
	}
	wasGrab := p.grab.Load()
	switch wParam {
	case wmKeyDown, wmKeyUp, wmSysKeyDown, wmSysKeyUp:
		sc := keymap.WinScan{Code: uint16(h.scanCode), Extended: h.flags&llkhfExtended != 0}
		if sc.Code == 0 {
			// Some drivers and injectors report only a VK; derive its scan code.
			r, _, _ := procMapVirtualKeyW.Call(uintptr(h.vkCode), mapvkVKToVSCEx)
			sc = keymap.WinScan{Code: uint16(r & 0xFF), Extended: r>>8 == 0xE0}
		}
		if hid, ok := keymap.WinToHID(sc); ok {
			p.fn(Event{Kind: Key, HID: hid, Down: h.flags&llkhfUp == 0})
		}
	}
	if swallow(p, wasGrab) {
		return 1
	}
	r, _, _ := procCallNextHookEx.Call(0, nCode, wParam, lParam)
	return r
}

// swallow decides after fn ran, because fn may toggle the grab for this
// very event (edge crossing). Entering: the crossing move must not move the
// cursor off the park point. Leaving: the event belonged to the remote side
// and the cursor is positioned by fn's Inject instead.
// SendInput/SetCursorPos inside fn only queue input; the system delivers it
// to our hooks after this proc returns, so nothing here waits on itself.
func swallow(p *winPlat, wasGrab bool) bool {
	return wasGrab || p.grab.Load()
}

func (p *winPlat) SetGrab(on bool) {
	p.grabMu.Lock()
	defer p.grabMu.Unlock()
	if on == p.grab.Load() {
		return
	}
	if on {
		// The primary monitor's origin is (0,0) in virtual-screen coordinates.
		px, py := sysMetric(smCXScreen)/2, sysMetric(smCYScreen)/2
		p.parkX.Store(px)
		p.parkY.Store(py)
		setCursorsHidden(true)
		// Grab first so the move SetCursorPos may echo is dropped as a
		// zero delta rather than reported as an absolute jump.
		p.grab.Store(true)
		procSetCursorPos.Call(uintptr(px), uintptr(py))
		return
	}
	p.grab.Store(false)
	setCursorsHidden(false)
}

// Cursor hiding: ShowCursor only affects our own thread's windows, so the
// system cursors are replaced with blank ones. SPI_SETCURSORS reloads the
// user's scheme from disk; it runs on ungrab, Run return, console
// Ctrl+C/close, and on the next New in case the process died while grabbing.
// SetGrab is called from the hook proc, which must stay well under
// LowLevelHooksTimeout, so swapping cursors happens on cursorWorker.
var (
	ocrIDs = [...]uintptr{
		32512, 32513, 32514, 32515, 32516, 32642, 32643, 32644, 32645,
		32646, 32648, 32649, 32650, 32651, 32671, 32672,
	}
	cursorsHidden atomic.Bool // actual state; guarded for writes by cursorMu
	cursorWant    atomic.Bool
	cursorMu      sync.Mutex
	cursorKick    = make(chan struct{}, 1)
	cursorOnce    sync.Once
	blankAND      [32 * 32 / 8]byte // 32x32 1bpp, all transparent
	blankXOR      [len(blankAND)]byte
)

func init() {
	for i := range blankAND {
		blankAND[i] = 0xFF
	}
}

func setCursorsHidden(on bool) {
	cursorOnce.Do(func() { go cursorWorker() })
	cursorWant.Store(on)
	select {
	case cursorKick <- struct{}{}:
	default: // worker already pending; it reads the latest cursorWant
	}
}

func cursorWorker() {
	for range cursorKick {
		if cursorWant.Load() {
			hideCursors()
		} else {
			restoreCursors()
		}
	}
}

func hideCursors() {
	cursorMu.Lock()
	defer cursorMu.Unlock()
	if cursorsHidden.Load() || !cursorWant.Load() {
		return
	}
	cursorsHidden.Store(true)
	for _, id := range ocrIDs {
		// SetSystemCursor takes ownership of (and destroys) the handle, so
		// each id needs its own blank cursor.
		c, _, _ := procCreateCursor.Call(hinstance, 0, 0, 32, 32,
			uintptr(unsafe.Pointer(&blankAND[0])), uintptr(unsafe.Pointer(&blankXOR[0])))
		if c == 0 {
			continue
		}
		if r, _, _ := procSetSystemCursor.Call(c, id); r == 0 {
			procDestroyCursor.Call(c)
		}
	}
}

// restoreCursors is also called synchronously on exit paths.
func restoreCursors() {
	cursorWant.Store(false)
	cursorMu.Lock()
	defer cursorMu.Unlock()
	if cursorsHidden.Load() {
		cursorsHidden.Store(false)
		procSystemParametersInfoW.Call(spiSetCursors, 0, 0, 0)
	}
}

func (p *winPlat) Inject(ev Event) error {
	var in [2]winInput
	n := 1
	switch ev.Kind {
	case MouseMove:
		// Absolute SendInput (not SetCursorPos) goes through the input stack,
		// so hover/WM_MOUSEMOVE and raw-input consumers see a real move.
		vs := virtualScreen()
		in[0] = winInput{typ: inputMouse, mi: mouseInput{
			dx:    normAbs(ev.X-vs.X, vs.W),
			dy:    normAbs(ev.Y-vs.Y, vs.H),
			flags: mouseMove | mouseAbsolute | mouseVirtDesk,
		}}
	case MouseButton:
		mi := mouseInput{}
		switch ev.Button {
		case ButtonLeft:
			mi.flags = pick(ev.Down, mouseLeftDown, mouseLeftUp)
		case ButtonRight:
			mi.flags = pick(ev.Down, mouseRightDown, mouseRightUp)
		case ButtonMiddle:
			mi.flags = pick(ev.Down, mouseMiddleDown, mouseMiddleUp)
		case ButtonBack:
			mi.flags, mi.mouseData = pick(ev.Down, mouseXDown, mouseXUp), xbutton1
		case ButtonForward:
			mi.flags, mi.mouseData = pick(ev.Down, mouseXDown, mouseXUp), xbutton2
		default:
			return errors.New("plat: unknown mouse button")
		}
		in[0] = winInput{typ: inputMouse, mi: mi}
	case Wheel:
		n = 0
		if ev.DY != 0 {
			in[n] = winInput{typ: inputMouse, mi: mouseInput{flags: mouseWheel, mouseData: uint32(ev.DY)}}
			n++
		}
		if ev.DX != 0 {
			in[n] = winInput{typ: inputMouse, mi: mouseInput{flags: mouseHWheel, mouseData: uint32(ev.DX)}}
			n++
		}
		if n == 0 {
			return nil
		}
	case Key:
		sc, ok := keymap.HIDToWin(ev.HID)
		if !ok {
			return nil
		}
		in[0].typ = inputKeyboard
		ki := (*keybdInput)(unsafe.Pointer(&in[0].mi))
		ki.scan = sc.Code
		if sc.Code == 0x45 {
			// keymap uses the hook/lParam convention: NumLock = E0 45,
			// Pause = 45. A raw 0x45 scan code injects NumLock and Pause's
			// real E1 1D 45 sequence cannot be expressed, so use VKs.
			if sc.Extended {
				ki.vk, ki.flags = vkNumLock, keyExtended
			} else {
				ki.vk = vkPause
			}
		} else {
			ki.flags = keyScanCode
			if sc.Extended {
				ki.flags |= keyExtended
			}
		}
		if !ev.Down {
			ki.flags |= keyUp
		}
	default:
		return errors.New("plat: unknown event kind")
	}
	return sendInput(in[:n])
}

func sendInput(in []winInput) error {
	for i := range in {
		if in[i].typ == inputKeyboard {
			(*keybdInput)(unsafe.Pointer(&in[i].mi)).extra = injectMarker
		} else {
			in[i].mi.extra = injectMarker
		}
	}
	r, _, e := procSendInput.Call(uintptr(len(in)), uintptr(unsafe.Pointer(&in[0])), unsafe.Sizeof(in[0]))
	if int(r) != len(in) {
		// Typically UIPI: the foreground window belongs to a more privileged process.
		return callErr("SendInput", e)
	}
	return nil
}

// normAbs maps pixel v in [0,size) to 0..65535 such that Windows'
// (n*size)>>16 lands exactly on v again.
func normAbs(v, size int32) int32 {
	if size <= 0 {
		return 0
	}
	n := (int64(v)*65536 + int64(size) - 1) / int64(size)
	return int32(min(max(n, 0), 65535))
}

func pick(down bool, d, u uint32) uint32 {
	if down {
		return d
	}
	return u
}
