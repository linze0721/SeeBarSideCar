package plat

import (
	"errors"
	"image"
	"math"
	"sync"
	"sync/atomic"

	"github.com/ebitengine/purego/objc"

	"magic/internal/keymap"
)

// AppKit constants.
const (
	nsWindowStyleOverlay = 4 // borderless | miniaturizable
	nsBackingBuffered    = 2
	// NSTrackingMouseMoved | NSTrackingActiveAlways | NSTrackingInVisibleRect
	nsTrackingOptions = 0x2 | 0x80 | 0x200
)

var (
	selInitWithContentRect   = objc.RegisterName("initWithContentRect:styleMask:backing:defer:")
	selInitWithFrame         = objc.RegisterName("initWithFrame:")
	selSetReleasedWhenClosed = objc.RegisterName("setReleasedWhenClosed:")
	selSetContentView        = objc.RegisterName("setContentView:")
	selSetDelegate           = objc.RegisterName("setDelegate:")
	selSetAcceptsMouseMoved  = objc.RegisterName("setAcceptsMouseMovedEvents:")
	selOrderFront            = objc.RegisterName("orderFront:")
	selMakeFirstResponder    = objc.RegisterName("makeFirstResponder:")
	selSetTitle              = objc.RegisterName("setTitle:")
	selSetFrameDisplay       = objc.RegisterName("setFrame:display:")
	selFrame                 = objc.RegisterName("frame")
	selSetHasShadow          = objc.RegisterName("setHasShadow:")
	selSetOpaque             = objc.RegisterName("setOpaque:")
	selSetBackgroundColor    = objc.RegisterName("setBackgroundColor:")
	selBlackColor            = objc.RegisterName("blackColor")
	selSetMovableByBg        = objc.RegisterName("setMovableByWindowBackground:")
	selSetLevel              = objc.RegisterName("setLevel:")
	selIsKeyWindow           = objc.RegisterName("isKeyWindow")
	selBackingScaleFactor    = objc.RegisterName("backingScaleFactor")
	selClose                 = objc.RegisterName("close")
	selSetNeedsDisplay       = objc.RegisterName("setNeedsDisplay:")
	selBounds                = objc.RegisterName("bounds")
	selCurrentContext        = objc.RegisterName("currentContext")
	selCGContext             = objc.RegisterName("CGContext")
	selLocationInWindow      = objc.RegisterName("locationInWindow")
	selButtonNumber          = objc.RegisterName("buttonNumber")
	selKeyCode               = objc.RegisterName("keyCode")
	selModifierFlags         = objc.RegisterName("modifierFlags")
	selScrollingDeltaX       = objc.RegisterName("scrollingDeltaX")
	selScrollingDeltaY       = objc.RegisterName("scrollingDeltaY")
	selHasPreciseDeltas      = objc.RegisterName("hasPreciseScrollingDeltas")
	selObject                = objc.RegisterName("object")
	selInitTrackingArea      = objc.RegisterName("initWithRect:options:owner:userInfo:")
	selAddTrackingArea       = objc.RegisterName("addTrackingArea:")
)

// overlays maps both the MagicView and its NSWindow to the Go overlay.
// Main thread only.
var (
	overlays     = map[objc.ID]*macOverlay{}
	overlayOnce  sync.Once
	overlayErr   error
	viewDelegate objc.ID // shared MagicWinDelegate instance, never released
)

type macOverlay struct {
	input  func(Event)
	closed func()

	closing atomic.Bool // programmatic Close requested; no closed callback
	gone    atomic.Bool // window closed, objects released

	mu     sync.Mutex
	next   uintptr // latest CGImage not yet shown (retained), under mu
	nw, nh int     // its size
	queued bool    // an apply is pending on the main thread

	// Main thread only.
	win, view, tracking objc.ID
	shown               bool    // ordered front (after the first frame)
	img                 uintptr // CGImage being displayed
	fw, fh              int
	keys                map[uint16]bool
	mods                [128]bool
	buttons             uint8 // bit (Button-1) set while held
	scrollX, scrollY    float64
}

var _ Overlay = (*macOverlay)(nil)

func registerOverlayClasses() error {
	overlayOnce.Do(func() {
		yes := func(objc.ID, objc.SEL) bool { return true }
		// Borderless windows can't become key/main unless the class says so.
		_, err := objc.RegisterClass("MagicPanelWindow", objc.GetClass("NSWindow"), nil, nil, []objc.MethodDef{
			{Cmd: objc.RegisterName("canBecomeKeyWindow"), Fn: yes},
			{Cmd: objc.RegisterName("canBecomeMainWindow"), Fn: yes},
		})
		if err != nil {
			overlayErr = err
			return
		}
		_, err = objc.RegisterClass("MagicView", objc.GetClass("NSView"), nil, nil, []objc.MethodDef{
			{Cmd: objc.RegisterName("isFlipped"), Fn: yes},
			{Cmd: objc.RegisterName("acceptsFirstResponder"), Fn: yes},
			{Cmd: objc.RegisterName("acceptsFirstMouse:"), Fn: func(objc.ID, objc.SEL, objc.ID) bool { return true }},
			{Cmd: objc.RegisterName("drawRect:"), Fn: viewDrawRect},
			{Cmd: objc.RegisterName("mouseMoved:"), Fn: viewMouseMoved},
			{Cmd: objc.RegisterName("mouseDragged:"), Fn: viewMouseMoved},
			{Cmd: objc.RegisterName("rightMouseDragged:"), Fn: viewMouseMoved},
			{Cmd: objc.RegisterName("otherMouseDragged:"), Fn: viewMouseMoved},
			{Cmd: objc.RegisterName("mouseDown:"), Fn: viewButton(ButtonLeft, true)},
			{Cmd: objc.RegisterName("mouseUp:"), Fn: viewButton(ButtonLeft, false)},
			{Cmd: objc.RegisterName("rightMouseDown:"), Fn: viewButton(ButtonRight, true)},
			{Cmd: objc.RegisterName("rightMouseUp:"), Fn: viewButton(ButtonRight, false)},
			{Cmd: objc.RegisterName("otherMouseDown:"), Fn: viewButton(0, true)},
			{Cmd: objc.RegisterName("otherMouseUp:"), Fn: viewButton(0, false)},
			{Cmd: objc.RegisterName("scrollWheel:"), Fn: viewScroll},
			{Cmd: objc.RegisterName("keyDown:"), Fn: viewKeyDown},
			{Cmd: objc.RegisterName("keyUp:"), Fn: viewKey(false)},
			{Cmd: objc.RegisterName("performKeyEquivalent:"), Fn: viewKeyEquivalent},
			{Cmd: objc.RegisterName("flagsChanged:"), Fn: viewFlags},
		})
		if err != nil {
			overlayErr = err
			return
		}
		dc, err := objc.RegisterClass("MagicWinDelegate", objc.GetClass("NSObject"), nil, nil, []objc.MethodDef{
			{Cmd: objc.RegisterName("windowWillClose:"), Fn: winWillClose},
			{Cmd: objc.RegisterName("windowDidResignKey:"), Fn: winResignKey},
		})
		if err != nil {
			overlayErr = err
			return
		}
		viewDelegate = objc.ID(dc).Send(selNew)
	})
	return overlayErr
}

// primaryHeight is the height of the display at the global origin. CG
// global coordinates are top-left based on it; Cocoa's are bottom-left.
func primaryHeight() float64 { return cgDisplayBounds(cgMainDisplayID()).H }

// cocoaRect converts a global top-left rect to a Cocoa screen rect.
func cocoaRect(r Rect) cgRect {
	return cgRect{
		X: float64(r.X),
		Y: primaryHeight() - float64(r.Y) - float64(r.H),
		W: float64(max(r.W, 1)),
		H: float64(max(r.H, 1)),
	}
}

// NewOverlay creates the window hidden and orders it front (without
// activating the app or taking key) once the first frame arrives, so no
// blank window flashes.
func (p *macPlat) NewOverlay(title string, rect Rect, input func(Event), closed func()) (Overlay, error) {
	v := &macOverlay{input: input, closed: closed, keys: map[uint16]bool{}}
	var err error
	onMainSync(func() {
		if err = registerOverlayClasses(); err != nil {
			return
		}
		frame := cocoaRect(rect)
		v.win = class("MagicPanelWindow").Send(selAlloc).Send(selInitWithContentRect, frame, uint64(nsWindowStyleOverlay), uint64(nsBackingBuffered), false)
		if v.win == 0 {
			err = errors.New("plat: creating NSWindow failed")
			return
		}
		v.view = class("MagicView").Send(selAlloc).Send(selInitWithFrame, cgRect{W: frame.W, H: frame.H})
		v.tracking = class("NSTrackingArea").Send(selAlloc).Send(selInitTrackingArea, cgRect{}, uint64(nsTrackingOptions), v.view, objc.ID(0))
		v.view.Send(selAddTrackingArea, v.tracking)
		overlays[v.view] = v
		overlays[v.win] = v
		v.win.Send(selSetReleasedWhenClosed, false)
		v.win.Send(selSetContentView, v.view)
		v.win.Send(selSetDelegate, viewDelegate)
		v.win.Send(selSetAcceptsMouseMoved, true)
		v.win.Send(selSetHasShadow, true)
		v.win.Send(selSetOpaque, true)
		v.win.Send(selSetBackgroundColor, class("NSColor").Send(selBlackColor))
		v.win.Send(selSetMovableByBg, false)
		v.win.Send(selSetLevel, int64(0)) // NSNormalWindowLevel
		v.win.Send(selSetTitle, nsString(title))
		// initWithContentRect: may constrain the frame to a screen.
		v.win.Send(selSetFrameDisplay, frame, false)
		v.win.Send(selMakeFirstResponder, v.view)
	})
	if err != nil {
		return nil, err
	}
	return v, nil
}

// SetFrame converts img into a CGImage on the caller's goroutine (the
// pixels are copied into a CFData) and hands only the newest pending image
// to the main thread, so a slow UI drops frames instead of queueing them.
func (v *macOverlay) SetFrame(img *image.RGBA) {
	if img == nil || v.gone.Load() {
		return
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return
	}
	var pix []byte
	if img.Stride == w*4 {
		pix = img.Pix[img.PixOffset(b.Min.X, b.Min.Y):][:w*h*4]
	} else {
		pix = make([]byte, w*h*4)
		for y := range h {
			copy(pix[y*w*4:][:w*4], img.Pix[img.PixOffset(b.Min.X, b.Min.Y+y):])
		}
	}
	data := cfDataCreate(0, &pix[0], len(pix))
	if data == 0 {
		return
	}
	prov := cgDataProviderCreateWithCFData(data)
	cfRelease(data)
	if prov == 0 {
		return
	}
	// kCGImageAlphaNoneSkipLast | kCGBitmapByteOrder32Big: R,G,B,x bytes.
	cg := cgImageCreate(w, h, 8, 32, w*4, deviceRGB, 5|0x4000, prov, 0, false, 0)
	cgDataProviderRelease(prov)
	if cg == 0 {
		return
	}
	v.mu.Lock()
	if v.next != 0 {
		cgImageRelease(v.next)
	}
	v.next, v.nw, v.nh = cg, w, h
	queue := !v.queued
	v.queued = true
	v.mu.Unlock()
	if queue {
		onMain(v.applyFrame)
	}
}

func (v *macOverlay) applyFrame() {
	v.mu.Lock()
	cg, w, h := v.next, v.nw, v.nh
	v.next, v.queued = 0, false
	v.mu.Unlock()
	if cg == 0 {
		return
	}
	if v.gone.Load() {
		cgImageRelease(cg)
		return
	}
	if v.img != 0 {
		cgImageRelease(v.img)
	}
	v.img, v.fw, v.fh = cg, w, h
	v.view.Send(selSetNeedsDisplay, true)
	if !v.shown {
		v.shown = true
		v.win.Send(selOrderFront, objc.ID(0))
	}
}

// SetBounds needs no coalescing: layouts arrive at most at the host's
// scan rate and the main queue applies them in order.
func (v *macOverlay) SetBounds(rect Rect) {
	if v.gone.Load() {
		return
	}
	onMain(func() {
		if !v.gone.Load() {
			v.win.Send(selSetFrameDisplay, cocoaRect(rect), true)
		}
	})
}

func (v *macOverlay) SetTitle(title string) {
	if v.gone.Load() {
		return
	}
	onMain(func() {
		if !v.gone.Load() {
			v.win.Send(selSetTitle, nsString(title))
		}
	})
}

func (v *macOverlay) Close() {
	if v.gone.Load() || !v.closing.CompareAndSwap(false, true) {
		return
	}
	onMain(func() {
		if !v.gone.Load() {
			v.win.Send(selClose) // windowWillClose: sees closing and disposes
		}
	})
}

// dispose drops the window's objects once it has closed. It runs from a
// later main-loop turn because the window is still mid-close in
// windowWillClose:.
func (v *macOverlay) dispose() {
	v.gone.Store(true)
	delete(overlays, v.view)
	delete(overlays, v.win)
	onMain(func() {
		v.win.Send(selSetDelegate, objc.ID(0))
		v.tracking.Send(selRelease)
		v.view.Send(selRelease)
		v.win.Send(selRelease)
		if v.img != 0 {
			cgImageRelease(v.img)
			v.img = 0
		}
		v.mu.Lock()
		if v.next != 0 {
			cgImageRelease(v.next)
			v.next = 0
		}
		v.mu.Unlock()
	})
}

func winWillClose(_ objc.ID, _ objc.SEL, note objc.ID) {
	v := overlays[note.Send(selObject)]
	if v == nil {
		return
	}
	v.releaseAll()
	v.dispose()
	if !v.closing.Load() && v.closed != nil {
		go v.closed()
	}
}

func winResignKey(_ objc.ID, _ objc.SEL, note objc.ID) {
	if v := overlays[note.Send(selObject)]; v != nil {
		v.releaseAll()
	}
}

// releaseAll lifts everything held so the remote side doesn't end up with
// stuck keys/buttons when focus moves away mid-press (e.g. Cmd+Tab).
func (v *macOverlay) releaseAll() {
	for hid := range v.keys {
		v.input(Event{Kind: Key, HID: hid})
	}
	clear(v.keys)
	v.mods = [128]bool{}
	for b := ButtonLeft; b <= ButtonForward; b++ {
		if v.buttons&(1<<(b-1)) != 0 {
			v.input(Event{Kind: MouseButton, Button: b})
		}
	}
	v.buttons = 0
}

func viewDrawRect(self objc.ID, _ objc.SEL, _ cgRect) {
	v := overlays[self]
	gc := class("NSGraphicsContext").Send(selCurrentContext)
	if gc == 0 {
		return
	}
	ctx := objc.Send[uintptr](gc, selCGContext)
	if ctx == 0 {
		return
	}
	b := objc.Send[cgRect](self, selBounds)
	cgContextSetRGBFillColor(ctx, 0, 0, 0, 1)
	cgContextFillRect(ctx, b)
	if v == nil || v.img == 0 {
		return
	}
	// The view is flipped (y down) but CGContextDrawImage assumes y up.
	cgContextSaveGState(ctx)
	cgContextTranslateCTM(ctx, 0, b.H)
	cgContextScaleCTM(ctx, 1, -1)
	// 1:1 when the view's backing pixels match the image; smoothing would
	// only blur it.
	bs := objc.Send[float64](v.win, selBackingScaleFactor)
	if math.Round(b.W*bs) == float64(v.fw) && math.Round(b.H*bs) == float64(v.fh) {
		cgContextSetInterpolationQuality(ctx, 1) // kCGInterpolationNone
	} else {
		cgContextSetInterpolationQuality(ctx, 3) // kCGInterpolationHigh
	}
	cgContextDrawImage(ctx, cgRect{W: b.W, H: b.H}, v.img)
	cgContextRestoreGState(ctx)
}

// globalPos maps the event's position to global top-left points. Drags
// keep reporting outside the window; those positions stay meaningful.
func (v *macOverlay) globalPos(ev objc.ID) (int32, int32) {
	loc := objc.Send[cgPoint](ev, selLocationInWindow)
	f := objc.Send[cgRect](v.win, selFrame)
	x := f.X + loc.X
	y := primaryHeight() - (f.Y + loc.Y)
	return int32(math.Floor(x)), int32(math.Floor(y))
}

func viewMouseMoved(self objc.ID, _ objc.SEL, ev objc.ID) {
	if v := overlays[self]; v != nil {
		x, y := v.globalPos(ev)
		v.input(Event{Kind: MouseMove, X: x, Y: y})
	}
}

func viewButton(b uint8, down bool) func(objc.ID, objc.SEL, objc.ID) {
	return func(self objc.ID, _ objc.SEL, ev objc.ID) {
		v := overlays[self]
		if v == nil {
			return
		}
		btn := b
		if btn == 0 {
			if btn = otherButton(objc.Send[int64](ev, selButtonNumber)); btn == 0 {
				return
			}
		}
		// Position first: the app presses at the last MouseMove position,
		// and clicks can arrive without a prior move (window activation).
		x, y := v.globalPos(ev)
		v.input(Event{Kind: MouseMove, X: x, Y: y})
		bit := uint8(1) << (btn - 1)
		if down {
			v.buttons |= bit
		} else {
			if v.buttons&bit == 0 {
				return // press happened outside the overlay
			}
			v.buttons &^= bit
		}
		v.input(Event{Kind: MouseButton, Button: btn, Down: down})
	}
}

func viewScroll(self objc.ID, _ objc.SEL, ev objc.ID) {
	v := overlays[self]
	if v == nil {
		return
	}
	// scrollingDelta has the user's natural-scrolling preference applied;
	// +Y scrolls content up, +X scrolls left. The wire is device-relative
	// (see Capture), so undo the preference.
	scale := float64(WheelNotch)
	if objc.Send[bool](ev, selHasPreciseDeltas) {
		scale = WheelNotch / 10.0 // 10 pt ≈ one notch
	}
	if naturalScrolling() {
		scale = -scale
	}
	v.scrollY += objc.Send[float64](ev, selScrollingDeltaY) * scale
	v.scrollX -= objc.Send[float64](ev, selScrollingDeltaX) * scale
	dx, dy := math.Trunc(v.scrollX), math.Trunc(v.scrollY)
	v.scrollX -= dx
	v.scrollY -= dy
	if dx != 0 || dy != 0 {
		v.input(Event{Kind: Wheel, DX: int32(dx), DY: int32(dy)})
	}
}

func viewKey(down bool) func(objc.ID, objc.SEL, objc.ID) {
	return func(self objc.ID, _ objc.SEL, ev objc.ID) {
		v := overlays[self]
		if v == nil {
			return
		}
		hid, ok := keymap.MacToHID(objc.Send[uint16](ev, selKeyCode))
		if !ok {
			return
		}
		if down {
			v.keys[hid] = true // repeats are forwarded as further downs
		} else {
			if !v.keys[hid] {
				return
			}
			delete(v.keys, hid)
		}
		v.input(Event{Kind: Key, HID: hid, Down: down})
	}
}

var viewKeyDown = viewKey(true)

// viewKeyEquivalent forwards Cmd/Ctrl combos (Cmd-W, Cmd-Q, ...) that AppKit
// offers as key equivalents before keyDown:, and claims them so AppKit
// neither beeps nor acts on them locally. Only the key window's view takes
// them; other overlays' views see the call too.
func viewKeyEquivalent(self objc.ID, _ objc.SEL, ev objc.ID) bool {
	v := overlays[self]
	if v == nil || !objc.Send[bool](v.win, selIsKeyWindow) {
		return false
	}
	viewKeyDown(self, 0, ev)
	return true
}

func viewFlags(self objc.ID, _ objc.SEL, ev objc.ID) {
	v := overlays[self]
	if v == nil {
		return
	}
	kc := objc.Send[uint16](ev, selKeyCode)
	hid, ok := keymap.MacToHID(kc)
	if !ok || int(kc) >= len(v.mods) {
		return
	}
	if kc == kVKCapsLock {
		v.input(Event{Kind: Key, HID: hid, Down: true})
		v.input(Event{Kind: Key, HID: hid, Down: false})
		return
	}
	down := !v.mods[kc]
	m, isMod := modBits[kc]
	if isMod {
		down = objc.Send[uint64](ev, selModifierFlags)&m.dev != 0
	}
	if v.mods[kc] == down {
		return
	}
	v.mods[kc] = down
	if down {
		v.keys[hid] = true
	} else {
		delete(v.keys, hid)
	}
	v.input(Event{Kind: Key, HID: hid, Down: down})
	if !down && isMod && m.mask == flagCommand {
		// AppKit never delivers keyUp for keys released while Command is
		// held; lift them now so the remote side has no stuck keys.
		for k := range v.keys {
			if k < keymap.LeftCtrl || k > keymap.RightGUI {
				v.input(Event{Kind: Key, HID: k})
				delete(v.keys, k)
			}
		}
	}
}
