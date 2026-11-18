package plat

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"

	"magic/internal/keymap"
)

// injectMarker tags every event we synthesize (kCGEventSourceUserData) so
// our own tap can skip it while still seeing input injected by other tools.
const injectMarker int64 = 0x6D616763 // "magc"

// CGEventType values.
const (
	evLeftMouseDown     = 1
	evLeftMouseUp       = 2
	evRightMouseDown    = 3
	evRightMouseUp      = 4
	evMouseMoved        = 5
	evLeftMouseDragged  = 6
	evRightMouseDragged = 7
	evKeyDown           = 10
	evKeyUp             = 11
	evFlagsChanged      = 12
	evScrollWheel       = 22
	evOtherMouseDown    = 25
	evOtherMouseUp      = 26
	evOtherMouseDragged = 27
	evTapDisabledByTime = 0xFFFFFFFE
	evTapDisabledByUser = 0xFFFFFFFF
)

// CGEventField values.
const (
	fieldClickState       = 1
	fieldButtonNumber     = 3
	fieldDeltaX           = 4
	fieldDeltaY           = 5
	fieldKeycode          = 9
	fieldScrollAxis1      = 11
	fieldScrollAxis2      = 12
	fieldSourceUserData   = 42
	fieldScrollContinuous = 88
	fieldScrollPointAxis1 = 96
	fieldScrollPointAxis2 = 97
)

// CGEventFlags: device-independent masks plus the NX_DEVICE* side bits.
const (
	flagShift   uint64 = 0x20000
	flagControl uint64 = 0x40000
	flagOption  uint64 = 0x80000
	flagCommand uint64 = 0x100000

	devLCtl   uint64 = 0x1
	devLShift uint64 = 0x2
	devRShift uint64 = 0x4
	devLCmd   uint64 = 0x8
	devRCmd   uint64 = 0x10
	devLAlt   uint64 = 0x20
	devRAlt   uint64 = 0x40
	devRCtl   uint64 = 0x2000
)

const kVKCapsLock = 0x39

// modBits maps a modifier kVK to its side bit and device-independent mask.
var modBits = map[uint16]struct{ dev, mask uint64 }{
	0x3B: {devLCtl, flagControl}, // kVK_Control
	0x3E: {devRCtl, flagControl}, // kVK_RightControl
	0x38: {devLShift, flagShift}, // kVK_Shift
	0x3C: {devRShift, flagShift}, // kVK_RightShift
	0x3A: {devLAlt, flagOption},  // kVK_Option
	0x3D: {devRAlt, flagOption},  // kVK_RightOption
	0x37: {devLCmd, flagCommand}, // kVK_Command
	0x36: {devRCmd, flagCommand}, // kVK_RightCommand
}

// tapPlat is the Platform whose fn the tap feeds. The callback is a finite
// process-wide purego resource, hence package-level and created once.
var (
	tapPlat     atomic.Pointer[macPlat]
	tapCallback = purego.NewCallback(tapProc)
)

func (p *macPlat) Capture(fn func(Event)) error {
	if !p.capturing.CompareAndSwap(false, true) {
		return errors.New("plat: Capture already running")
	}
	if err := requestPermissions(); err != nil {
		p.capturing.Store(false)
		return err
	}
	p.fn = fn
	tapPlat.Store(p)
	var mask uint64
	for _, t := range []uint{
		evLeftMouseDown, evLeftMouseUp, evRightMouseDown, evRightMouseUp,
		evMouseMoved, evLeftMouseDragged, evRightMouseDragged,
		evKeyDown, evKeyUp, evFlagsChanged, evScrollWheel,
		evOtherMouseDown, evOtherMouseUp, evOtherMouseDragged,
	} {
		mask |= 1 << t
	}
	errc := make(chan error, 1)
	go func() {
		// The tap's run loop source fires on the thread whose run loop it
		// was added to.
		runtime.LockOSThread()
		// kCGSessionEventTap, kCGHeadInsertEventTap, kCGEventTapOptionDefault
		tap := cgEventTapCreate(1, 0, 0, mask, tapCallback, 0)
		if tap == 0 {
			errc <- fmt.Errorf("plat: CGEventTapCreate failed although permissions report granted; remove %s from Accessibility and Input Monitoring, re-add it, and restart", permissionHolder())
			return
		}
		src := cfMachPortCreateRunLoopSource(0, tap, 0)
		if src == 0 {
			errc <- errors.New("plat: CFMachPortCreateRunLoopSource failed")
			return
		}
		cfRunLoopAddSource(cfRunLoopGetCurrent(), src, kCFRunLoopCommonModes)
		cgEventTapEnable(tap, true)
		p.tap.Store(tap)
		errc <- nil
		cfRunLoopRun()
	}()
	if err := <-errc; err != nil {
		tapPlat.Store(nil)
		p.capturing.Store(false)
		return err
	}
	return nil
}

// requestPermissions checks Accessibility and Input Monitoring (both needed
// for an active event tap) and Screen Recording (window titles/capture).
// Missing ones trigger the system prompt, which also adds the responsible
// app to the list in System Settings so the user only has to flip it on.
// TCC caches the decision per process, so a grant only takes effect after
// a restart.
func requestPermissions() error {
	key, val := kAXTrustedCheckOptionPrompt, kCFBooleanTrue
	opts := cfDictionaryCreate(0, &key, &val, 1, kCFTypeDictionaryKeyCallBacks, kCFTypeDictionaryValueCallBacks)
	ax := axIsProcessTrustedWithOptions(opts)
	cfRelease(opts)
	// Preflight can report a stale denial right after the user toggles the
	// switch; the tap creation below is authoritative, so only prompt here.
	if !cgPreflightListenEventAccess() {
		cgRequestListenEventAccess()
	}
	if !cgPreflightScreenCaptureAccess() {
		cgRequestScreenCaptureAccess()
	}
	var missing []string
	if !ax {
		missing = append(missing, "Accessibility (辅助功能)")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("plat: macOS permission missing: %s. Enable %s there in System Settings › Privacy & Security (a prompt was just shown), then fully quit (Cmd+Q) and restart it",
		strings.Join(missing, ", "), permissionHolder())
}

// permissionHolder names the app TCC charges our permissions to: the .app
// bundle when launched as one, otherwise the terminal that started us.
func permissionHolder() string {
	exe, _ := os.Executable()
	if i := strings.Index(exe, ".app/Contents/MacOS/"); i >= 0 && os.Getenv("TERM_PROGRAM") == "" {
		return filepath.Base(exe[:i+4])
	}
	if t := os.Getenv("TERM_PROGRAM"); t != "" {
		switch t {
		case "Apple_Terminal":
			return "Terminal (终端)"
		case "vscode":
			return "Visual Studio Code (or Cursor)"
		}
		return t
	}
	return "the app that launched magic"
}

func tapProc(proxy uintptr, typ uint32, ev uintptr, refcon uintptr) uintptr {
	p := tapPlat.Load()
	if p == nil {
		return ev
	}
	if typ == evTapDisabledByTime || typ == evTapDisabledByUser {
		// The system disables slow taps (and on secure input); turn it back on.
		if tap := p.tap.Load(); tap != 0 {
			cgEventTapEnable(tap, true)
		}
		return ev
	}
	if ev == 0 || cgEventGetIntegerValueField(ev, fieldSourceUserData) == injectMarker {
		return ev
	}
	wasGrab := p.grab.Load()
	switch typ {
	case evMouseMoved, evLeftMouseDragged, evRightMouseDragged, evOtherMouseDragged:
		if wasGrab {
			// The cursor is detached from the mouse while grabbing, so only
			// the deltas carry motion.
			dx := int32(cgEventGetIntegerValueField(ev, fieldDeltaX))
			dy := int32(cgEventGetIntegerValueField(ev, fieldDeltaY))
			if dx == 0 && dy == 0 {
				return 0
			}
			p.fn(Event{Kind: MouseMove, DX: dx, DY: dy})
		} else {
			pt := cgEventGetLocation(ev)
			p.fn(Event{Kind: MouseMove, X: int32(math.Floor(pt.X)), Y: int32(math.Floor(pt.Y))})
		}
	case evLeftMouseDown, evLeftMouseUp:
		p.fn(Event{Kind: MouseButton, Button: ButtonLeft, Down: typ == evLeftMouseDown})
	case evRightMouseDown, evRightMouseUp:
		p.fn(Event{Kind: MouseButton, Button: ButtonRight, Down: typ == evRightMouseDown})
	case evOtherMouseDown, evOtherMouseUp:
		if b := otherButton(cgEventGetIntegerValueField(ev, fieldButtonNumber)); b != 0 {
			p.fn(Event{Kind: MouseButton, Button: b, Down: typ == evOtherMouseDown})
		}
	case evScrollWheel:
		// Axis1 > 0 scrolls content up and axis2 > 0 scrolls left, after the
		// user's natural-scrolling preference has been applied. The wire
		// format is device-relative (+DY = wheel away from user, the
		// Windows convention), so undo natural scrolling here; the peer
		// applies its own preference. Event wants +DX = right.
		var dy, dx int32
		if cgEventGetIntegerValueField(ev, fieldScrollContinuous) != 0 {
			// Trackpad / Magic Mouse pixel deltas: 10 px ≈ one notch.
			dy = int32(cgEventGetIntegerValueField(ev, fieldScrollPointAxis1)) * WheelNotch / 10
			dx = -int32(cgEventGetIntegerValueField(ev, fieldScrollPointAxis2)) * WheelNotch / 10
		} else {
			dy = int32(cgEventGetIntegerValueField(ev, fieldScrollAxis1)) * WheelNotch
			dx = -int32(cgEventGetIntegerValueField(ev, fieldScrollAxis2)) * WheelNotch
		}
		if naturalScrolling() {
			dx, dy = -dx, -dy
		}
		if dx != 0 || dy != 0 {
			p.fn(Event{Kind: Wheel, DX: dx, DY: dy})
		}
	case evKeyDown, evKeyUp:
		kc := uint16(cgEventGetIntegerValueField(ev, fieldKeycode))
		if hid, ok := keymap.MacToHID(kc); ok {
			p.fn(Event{Kind: Key, HID: hid, Down: typ == evKeyDown})
		}
	case evFlagsChanged:
		kc := uint16(cgEventGetIntegerValueField(ev, fieldKeycode))
		hid, ok := keymap.MacToHID(kc)
		if !ok {
			break
		}
		if kc == kVKCapsLock {
			// FlagsChanged reports the lock state, not the key: one event
			// per physical press, so send a full tap.
			p.fn(Event{Kind: Key, HID: hid, Down: true})
			p.fn(Event{Kind: Key, HID: hid, Down: false})
			break
		}
		// Modifiers: the side bit says whether this key is now held; other
		// flag keys (Fn) toggle.
		var down bool
		if m, ok := modBits[kc]; ok {
			down = cgEventGetFlags(ev)&m.dev != 0
		} else if int(kc) < len(p.tapMods) {
			down = !p.tapMods[kc]
		}
		if int(kc) < len(p.tapMods) {
			if p.tapMods[kc] == down {
				break // unrelated flag change
			}
			p.tapMods[kc] = down
		}
		p.fn(Event{Kind: Key, HID: hid, Down: down})
	}
	// Decide after fn: it may toggle the grab for this very event (edge
	// crossing). Swallow if grabbed before or after.
	if wasGrab || p.grab.Load() {
		return 0
	}
	return ev
}

func otherButton(n int64) uint8 {
	switch n {
	case 2:
		return ButtonMiddle
	case 3:
		return ButtonBack
	case 4:
		return ButtonForward
	}
	return 0
}

// SetGrab is called from the tap thread inside fn. CoreGraphics cursor
// calls are window-server requests that are safe off the main thread, so
// they run inline; nothing here waits on the main thread.
func (p *macPlat) SetGrab(on bool) {
	p.grabMu.Lock()
	defer p.grabMu.Unlock()
	if on == p.grab.Load() {
		return
	}
	if on {
		p.grab.Store(true)
		cgAssociateMouseAndMouseCursorPosition(false)
		if !p.hidden {
			cgDisplayHideCursor(0)
			p.hidden = true
		}
		return
	}
	p.grab.Store(false)
	cgAssociateMouseAndMouseCursorPosition(true)
	if p.hidden {
		cgDisplayShowCursor(0)
		p.hidden = false
	}
}

// injectState tracks what we have pressed so synthesized events carry
// consistent modifier flags, drag types and click counts.
type injectState struct {
	buttons uint8  // bit (Button-1) set while held
	flags   uint64 // current CGEventFlags
	mods    uint64 // side bits of held modifiers
	lastBtn uint8
	lastAt  time.Time
	lastPt  cgPoint
	clicks  int64
	wheelX  int32 // remainders below one pixel, in WheelNotch units
	wheelY  int32
}

const (
	doubleClickTime = 500 * time.Millisecond
	doubleClickDist = 4.0
)

func (p *macPlat) Inject(ev Event) error {
	p.injMu.Lock()
	defer p.injMu.Unlock()
	s := &p.inj
	switch ev.Kind {
	case MouseMove:
		// While a button is held, apps expect Dragged events, not Moved.
		typ, btn := uint32(evMouseMoved), uint32(0)
		switch {
		case s.buttons&(1<<(ButtonLeft-1)) != 0:
			typ = evLeftMouseDragged
		case s.buttons&(1<<(ButtonRight-1)) != 0:
			typ, btn = evRightMouseDragged, 1
		case s.buttons != 0:
			typ = evOtherMouseDragged
			for b := ButtonMiddle; b <= ButtonForward; b++ {
				if s.buttons&(1<<(b-1)) != 0 {
					btn = macButton(b)
					break
				}
			}
		}
		return p.post(cgEventCreateMouseEvent(p.src, typ, cgPoint{X: float64(ev.X), Y: float64(ev.Y)}, btn))
	case MouseButton:
		var typ uint32
		switch ev.Button {
		case ButtonLeft:
			typ = pickType(ev.Down, evLeftMouseDown, evLeftMouseUp)
		case ButtonRight:
			typ = pickType(ev.Down, evRightMouseDown, evRightMouseUp)
		case ButtonMiddle, ButtonBack, ButtonForward:
			typ = pickType(ev.Down, evOtherMouseDown, evOtherMouseUp)
		default:
			return errors.New("plat: unknown mouse button")
		}
		pt := cursorLocation()
		bit := uint8(1) << (ev.Button - 1)
		if ev.Down {
			now := time.Now()
			if s.lastBtn == ev.Button && now.Sub(s.lastAt) <= doubleClickTime &&
				math.Abs(pt.X-s.lastPt.X) <= doubleClickDist && math.Abs(pt.Y-s.lastPt.Y) <= doubleClickDist {
				s.clicks++
			} else {
				s.clicks = 1
			}
			s.lastBtn, s.lastAt, s.lastPt = ev.Button, now, pt
			s.buttons |= bit
		} else {
			s.buttons &^= bit
		}
		clicks := s.clicks
		if s.lastBtn != ev.Button || clicks < 1 {
			clicks = 1
		}
		e := cgEventCreateMouseEvent(p.src, typ, pt, macButton(ev.Button))
		if e == 0 {
			return errors.New("plat: CGEventCreateMouseEvent failed")
		}
		cgEventSetIntegerValueField(e, fieldClickState, clicks)
		return p.post(e)
	case Wheel:
		// 10 px per notch, the inverse of Capture's scaling. Remainders are
		// kept so fine-grained (trackpad) input from the peer adds up.
		s.wheelY += ev.DY
		s.wheelX += ev.DX
		py, px := s.wheelY*10/WheelNotch, s.wheelX*10/WheelNotch
		s.wheelY -= py * WheelNotch / 10
		s.wheelX -= px * WheelNotch / 10
		if px == 0 && py == 0 {
			return nil
		}
		// kCGScrollEventUnitPixel; wheel2 > 0 scrolls left. Wire deltas are
		// device-relative; apply this Mac's natural-scrolling preference.
		if naturalScrolling() {
			py, px = -py, -px
		}
		return p.post(cgEventCreateScrollWheelEvent2(p.src, 0, 2, py, -px, 0))
	case Key:
		vk, ok := keymap.HIDToMac(ev.HID)
		if !ok {
			return nil
		}
		e := cgEventCreateKeyboardEvent(p.src, vk, ev.Down)
		if e == 0 {
			return errors.New("plat: CGEventCreateKeyboardEvent failed")
		}
		if m, ok := modBits[vk]; ok {
			// Modifier keys travel as FlagsChanged carrying the new flags.
			if ev.Down {
				s.mods |= m.dev
			} else {
				s.mods &^= m.dev
			}
			s.flags = injectFlags(s.mods)
			cgEventSetType(e, evFlagsChanged)
		}
		return p.post(e)
	}
	return errors.New("plat: unknown event kind")
}

// post tags, flags and posts e, then releases it. Caller holds injMu.
func (p *macPlat) post(e uintptr) error {
	if e == 0 {
		return errors.New("plat: creating CGEvent failed")
	}
	cgEventSetIntegerValueField(e, fieldSourceUserData, injectMarker)
	cgEventSetFlags(e, p.inj.flags)
	cgEventPost(0, e) // kCGHIDEventTap
	cfRelease(e)
	return nil
}

// injectFlags derives CGEventFlags from the held modifiers' side bits.
func injectFlags(mods uint64) uint64 {
	f := mods
	for _, m := range modBits {
		if mods&m.dev != 0 {
			f |= m.mask
		}
	}
	return f
}

// macButton is the CGMouseButton number (kCGMouseButtonLeft = 0, ...).
func macButton(b uint8) uint32 {
	switch b {
	case ButtonRight:
		return 1
	case ButtonMiddle:
		return 2
	case ButtonBack:
		return 3
	case ButtonForward:
		return 4
	}
	return 0
}

func pickType(down bool, d, u uint32) uint32 {
	if down {
		return d
	}
	return u
}

// naturalScrolling reports com.apple.swipescrolldirection (default on),
// re-read at most every 2s since it is consulted per scroll event on the
// tap thread.
func naturalScrolling() bool {
	now := time.Now().UnixNano()
	if now-naturalAt.Load() < int64(2*time.Second) {
		return naturalOn.Load()
	}
	pool := objcAutoreleasePoolPush()
	defer objcAutoreleasePoolPop(pool)
	d := class("NSUserDefaults").Send(selStandardUserDefaults)
	key := nsString("com.apple.swipescrolldirection")
	on := d.Send(selObjectForKey, key) == 0 || objc.Send[bool](d, selBoolForKey, key)
	naturalOn.Store(on)
	naturalAt.Store(now)
	return on
}

var (
	naturalOn atomic.Bool
	naturalAt atomic.Int64

	selStandardUserDefaults = objc.RegisterName("standardUserDefaults")
	selObjectForKey         = objc.RegisterName("objectForKey:")
	selBoolForKey           = objc.RegisterName("boolForKey:")
)
