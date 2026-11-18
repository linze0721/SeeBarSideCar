package plat

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"structs"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// AppKit must be driven from the process's main thread. Package init runs
// on it, so pin the main goroutine there; Run then hands the thread to
// [NSApp run] and the app runs on another goroutine.
func init() { runtime.LockOSThread() }

// CoreGraphics geometry. CGFloat is double on 64-bit, and all-double
// structs are HFAs that purego passes/returns in float registers on arm64.
type cgPoint struct {
	_    structs.HostLayout
	X, Y float64
}

type cgSize struct {
	_    structs.HostLayout
	W, H float64
}

type cgRect struct {
	_          structs.HostLayout
	X, Y, W, H float64
}

const (
	cfStringEncodingUTF8 = 0x08000100
	cfNumberSInt64Type   = 4
	cfNumberDoubleType   = 13
)

var (
	// CoreFoundation
	cfRelease                       func(cf uintptr)
	cfStringCreateWithCString       func(alloc uintptr, s string, enc uint32) uintptr
	cfStringGetLength               func(s uintptr) int
	cfStringGetMaximumSizeForEncode func(n int, enc uint32) int
	cfStringGetCString              func(s uintptr, buf *byte, size int, enc uint32) bool
	cfArrayGetCount                 func(a uintptr) int
	cfArrayGetValueAtIndex          func(a uintptr, i int) uintptr
	cfDictionaryGetValue            func(d, key uintptr) uintptr
	cfNumberGetValue                func(n uintptr, typ int, out unsafe.Pointer) bool
	cfDataCreate                    func(alloc uintptr, bytes *byte, n int) uintptr
	cfMachPortCreateRunLoopSource   func(alloc, port uintptr, order int) uintptr
	cfRunLoopGetCurrent             func() uintptr
	cfRunLoopAddSource              func(rl, src, mode uintptr)
	cfRunLoopRun                    func()
	kCFRunLoopCommonModes           uintptr
	kCFBooleanTrue                  uintptr

	// CoreGraphics
	cgGetActiveDisplayList                 func(max uint32, ids *uint32, n *uint32) int32
	cgGetOnlineDisplayList                 func(max uint32, ids *uint32, n *uint32) int32
	cgDisplayBounds                        func(id uint32) cgRect
	cgMainDisplayID                        func() uint32
	cgDisplayIsMain                        func(id uint32) int32 // boolean_t
	cgDisplayMirrorsDisplay                func(id uint32) uint32
	cgDisplayCopyDisplayMode               func(id uint32) uintptr
	cgDisplayModeGetPixelWidth             func(mode uintptr) int
	cgDisplayModeRelease                   func(mode uintptr)
	cgBeginDisplayConfiguration            func(cfg *uintptr) int32
	cgConfigureDisplayOrigin               func(cfg uintptr, id uint32, x, y int32) int32
	cgConfigureDisplayMirrorOfDisplay      func(cfg uintptr, id, master uint32) int32
	cgCompleteDisplayConfiguration         func(cfg uintptr, opt uint32) int32
	cgCancelDisplayConfiguration           func(cfg uintptr) int32
	cgEventCreate                          func(src uintptr) uintptr
	cgEventGetLocation                     func(ev uintptr) cgPoint
	cgEventTapCreate                       func(tap, place, opts uint32, mask uint64, cb, refcon uintptr) uintptr
	cgEventTapEnable                       func(tap uintptr, on bool)
	cgEventGetIntegerValueField            func(ev uintptr, field uint32) int64
	cgEventSetIntegerValueField            func(ev uintptr, field uint32, v int64)
	cgEventGetFlags                        func(ev uintptr) uint64
	cgEventSetFlags                        func(ev uintptr, flags uint64)
	cgEventSetType                         func(ev uintptr, typ uint32)
	cgEventPost                            func(tap uint32, ev uintptr)
	cgEventSourceCreate                    func(state int32) uintptr
	cgEventCreateMouseEvent                func(src uintptr, typ uint32, pt cgPoint, button uint32) uintptr
	cgEventCreateKeyboardEvent             func(src uintptr, vk uint16, down bool) uintptr
	cgEventCreateScrollWheelEvent2         func(src uintptr, units, count uint32, w1, w2, w3 int32) uintptr
	cgAssociateMouseAndMouseCursorPosition func(connected bool) int32
	cgDisplayHideCursor                    func(display uint32) int32
	cgDisplayShowCursor                    func(display uint32) int32
	cgWindowListCopyWindowInfo             func(opts, rel uint32) uintptr
	cgRectMakeWithDictionaryRepresentation func(dict uintptr, r *cgRect) bool
	cgImageGetWidth                        func(img uintptr) int
	cgImageGetHeight                       func(img uintptr) int
	cgImageRetain                          func(img uintptr) uintptr
	cgImageRelease                         func(img uintptr)
	cgImageCreate                          func(w, h, bpc, bpp, bpr int, cs uintptr, info uint32, provider uintptr, decode uintptr, interpolate bool, intent int32) uintptr
	cgDataProviderCreateWithCFData         func(data uintptr) uintptr
	cgDataProviderRelease                  func(p uintptr)
	cgColorSpaceCreateDeviceRGB            func() uintptr
	cgBitmapContextCreate                  func(data unsafe.Pointer, w, h, bpc, bpr int, cs uintptr, info uint32) uintptr
	cgContextRelease                       func(ctx uintptr)
	cgContextDrawImage                     func(ctx uintptr, r cgRect, img uintptr)
	cgContextSaveGState                    func(ctx uintptr)
	cgContextRestoreGState                 func(ctx uintptr)
	cgContextTranslateCTM                  func(ctx uintptr, tx, ty float64)
	cgContextScaleCTM                      func(ctx uintptr, sx, sy float64)
	cgContextSetInterpolationQuality       func(ctx uintptr, q int32)
	cgContextSetRGBFillColor               func(ctx uintptr, r, g, b, a float64)
	cgContextFillRect                      func(ctx uintptr, r cgRect)
	// Deprecated in macOS 14 and obsoleted in the 15 SDK, but still exported;
	// nil when missing (ScreenCaptureKit is used instead).
	cgWindowListCreateImage func(r cgRect, opts, id, imgOpts uint32) uintptr

	// ApplicationServices (HIServices)
	axUIElementCreateApplication  func(pid int32) uintptr
	axUIElementCopyAttributeValue func(el, attr uintptr, out *uintptr) int32
	axUIElementSetAttributeValue  func(el, attr, value uintptr) int32
	axUIElementPerformAction      func(el, action uintptr) int32
	axUIElementGetWindow          func(el uintptr, id *uint32) int32 // private, may be nil
	axIsProcessTrustedWithOptions func(opts uintptr) bool
	kAXTrustedCheckOptionPrompt   uintptr
	// Input Monitoring / Screen Recording (macOS 10.15+).
	cgPreflightListenEventAccess    func() bool
	cgRequestListenEventAccess      func() bool
	cgPreflightScreenCaptureAccess  func() bool
	cgRequestScreenCaptureAccess    func() bool
	cfDictionaryCreate              func(alloc uintptr, keys, vals *uintptr, n int, kcb, vcb uintptr) uintptr
	kCFTypeDictionaryKeyCallBacks   uintptr
	kCFTypeDictionaryValueCallBacks uintptr

	// AppKit
	nsPasteboardURLReadingFileURLsOnlyKey uintptr // NSString constant

	// libSystem / libobjc
	dispatchAsyncF          func(queue, ctx, fn uintptr)
	pthreadMainNP           func() int32
	objcAutoreleasePoolPush func() uintptr
	objcAutoreleasePoolPop  func(pool uintptr)
	dispatchMainQ           uintptr // &_dispatch_main_q == dispatch_get_main_queue()

	deviceRGB uintptr // shared CGColorSpace, never released
)

var (
	libOnce sync.Once
	libErr  error
)

func loadLibs() error {
	libOnce.Do(func() { libErr = doLoadLibs() })
	return libErr
}

func doLoadLibs() (err error) {
	// RegisterLibFunc panics on a missing symbol.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("plat: loading macOS frameworks: %v", r)
		}
	}()
	open := func(path string) uintptr {
		h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			panic(err)
		}
		return h
	}
	cf := open("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation")
	cg := open("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics")
	as := open("/System/Library/Frameworks/ApplicationServices.framework/ApplicationServices")
	ak := open("/System/Library/Frameworks/AppKit.framework/AppKit")
	sys := open("/usr/lib/libSystem.B.dylib")
	objcLib := open("/usr/lib/libobjc.A.dylib")

	purego.RegisterLibFunc(&cfRelease, cf, "CFRelease")
	purego.RegisterLibFunc(&cfStringCreateWithCString, cf, "CFStringCreateWithCString")
	purego.RegisterLibFunc(&cfStringGetLength, cf, "CFStringGetLength")
	purego.RegisterLibFunc(&cfStringGetMaximumSizeForEncode, cf, "CFStringGetMaximumSizeForEncoding")
	purego.RegisterLibFunc(&cfStringGetCString, cf, "CFStringGetCString")
	purego.RegisterLibFunc(&cfArrayGetCount, cf, "CFArrayGetCount")
	purego.RegisterLibFunc(&cfArrayGetValueAtIndex, cf, "CFArrayGetValueAtIndex")
	purego.RegisterLibFunc(&cfDictionaryGetValue, cf, "CFDictionaryGetValue")
	purego.RegisterLibFunc(&cfNumberGetValue, cf, "CFNumberGetValue")
	purego.RegisterLibFunc(&cfDataCreate, cf, "CFDataCreate")
	purego.RegisterLibFunc(&cfMachPortCreateRunLoopSource, cf, "CFMachPortCreateRunLoopSource")
	purego.RegisterLibFunc(&cfRunLoopGetCurrent, cf, "CFRunLoopGetCurrent")
	purego.RegisterLibFunc(&cfRunLoopAddSource, cf, "CFRunLoopAddSource")
	purego.RegisterLibFunc(&cfRunLoopRun, cf, "CFRunLoopRun")
	kCFRunLoopCommonModes = mustGlobal(cf, "kCFRunLoopCommonModes")
	kCFBooleanTrue = mustGlobal(cf, "kCFBooleanTrue")
	purego.RegisterLibFunc(&cfDictionaryCreate, cf, "CFDictionaryCreate")
	kCFTypeDictionaryKeyCallBacks = mustAddr(cf, "kCFTypeDictionaryKeyCallBacks")
	kCFTypeDictionaryValueCallBacks = mustAddr(cf, "kCFTypeDictionaryValueCallBacks")

	purego.RegisterLibFunc(&cgGetActiveDisplayList, cg, "CGGetActiveDisplayList")
	purego.RegisterLibFunc(&cgGetOnlineDisplayList, cg, "CGGetOnlineDisplayList")
	purego.RegisterLibFunc(&cgDisplayBounds, cg, "CGDisplayBounds")
	purego.RegisterLibFunc(&cgEventCreate, cg, "CGEventCreate")
	purego.RegisterLibFunc(&cgEventGetLocation, cg, "CGEventGetLocation")
	purego.RegisterLibFunc(&cgEventTapCreate, cg, "CGEventTapCreate")
	purego.RegisterLibFunc(&cgMainDisplayID, cg, "CGMainDisplayID")
	purego.RegisterLibFunc(&cgDisplayIsMain, cg, "CGDisplayIsMain")
	purego.RegisterLibFunc(&cgDisplayMirrorsDisplay, cg, "CGDisplayMirrorsDisplay")
	purego.RegisterLibFunc(&cgDisplayCopyDisplayMode, cg, "CGDisplayCopyDisplayMode")
	purego.RegisterLibFunc(&cgDisplayModeGetPixelWidth, cg, "CGDisplayModeGetPixelWidth")
	purego.RegisterLibFunc(&cgDisplayModeRelease, cg, "CGDisplayModeRelease")
	purego.RegisterLibFunc(&cgBeginDisplayConfiguration, cg, "CGBeginDisplayConfiguration")
	purego.RegisterLibFunc(&cgConfigureDisplayOrigin, cg, "CGConfigureDisplayOrigin")
	purego.RegisterLibFunc(&cgConfigureDisplayMirrorOfDisplay, cg, "CGConfigureDisplayMirrorOfDisplay")
	purego.RegisterLibFunc(&cgCompleteDisplayConfiguration, cg, "CGCompleteDisplayConfiguration")
	purego.RegisterLibFunc(&cgCancelDisplayConfiguration, cg, "CGCancelDisplayConfiguration")
	purego.RegisterLibFunc(&cgEventTapEnable, cg, "CGEventTapEnable")
	purego.RegisterLibFunc(&cgEventGetIntegerValueField, cg, "CGEventGetIntegerValueField")
	purego.RegisterLibFunc(&cgEventSetIntegerValueField, cg, "CGEventSetIntegerValueField")
	purego.RegisterLibFunc(&cgEventGetFlags, cg, "CGEventGetFlags")
	purego.RegisterLibFunc(&cgEventSetFlags, cg, "CGEventSetFlags")
	purego.RegisterLibFunc(&cgEventSetType, cg, "CGEventSetType")
	purego.RegisterLibFunc(&cgEventPost, cg, "CGEventPost")
	purego.RegisterLibFunc(&cgEventSourceCreate, cg, "CGEventSourceCreate")
	purego.RegisterLibFunc(&cgEventCreateMouseEvent, cg, "CGEventCreateMouseEvent")
	purego.RegisterLibFunc(&cgEventCreateKeyboardEvent, cg, "CGEventCreateKeyboardEvent")
	purego.RegisterLibFunc(&cgEventCreateScrollWheelEvent2, cg, "CGEventCreateScrollWheelEvent2")
	purego.RegisterLibFunc(&cgAssociateMouseAndMouseCursorPosition, cg, "CGAssociateMouseAndMouseCursorPosition")
	purego.RegisterLibFunc(&cgDisplayHideCursor, cg, "CGDisplayHideCursor")
	purego.RegisterLibFunc(&cgDisplayShowCursor, cg, "CGDisplayShowCursor")
	purego.RegisterLibFunc(&cgWindowListCopyWindowInfo, cg, "CGWindowListCopyWindowInfo")
	purego.RegisterLibFunc(&cgRectMakeWithDictionaryRepresentation, cg, "CGRectMakeWithDictionaryRepresentation")
	purego.RegisterLibFunc(&cgImageGetWidth, cg, "CGImageGetWidth")
	purego.RegisterLibFunc(&cgImageGetHeight, cg, "CGImageGetHeight")
	purego.RegisterLibFunc(&cgImageRetain, cg, "CGImageRetain")
	purego.RegisterLibFunc(&cgImageRelease, cg, "CGImageRelease")
	purego.RegisterLibFunc(&cgImageCreate, cg, "CGImageCreate")
	purego.RegisterLibFunc(&cgDataProviderCreateWithCFData, cg, "CGDataProviderCreateWithCFData")
	purego.RegisterLibFunc(&cgDataProviderRelease, cg, "CGDataProviderRelease")
	purego.RegisterLibFunc(&cgColorSpaceCreateDeviceRGB, cg, "CGColorSpaceCreateDeviceRGB")
	purego.RegisterLibFunc(&cgBitmapContextCreate, cg, "CGBitmapContextCreate")
	purego.RegisterLibFunc(&cgContextRelease, cg, "CGContextRelease")
	purego.RegisterLibFunc(&cgContextDrawImage, cg, "CGContextDrawImage")
	purego.RegisterLibFunc(&cgContextSaveGState, cg, "CGContextSaveGState")
	purego.RegisterLibFunc(&cgContextRestoreGState, cg, "CGContextRestoreGState")
	purego.RegisterLibFunc(&cgContextTranslateCTM, cg, "CGContextTranslateCTM")
	purego.RegisterLibFunc(&cgContextScaleCTM, cg, "CGContextScaleCTM")
	purego.RegisterLibFunc(&cgContextSetInterpolationQuality, cg, "CGContextSetInterpolationQuality")
	purego.RegisterLibFunc(&cgContextSetRGBFillColor, cg, "CGContextSetRGBFillColor")
	purego.RegisterLibFunc(&cgContextFillRect, cg, "CGContextFillRect")
	purego.RegisterLibFunc(&cgPreflightListenEventAccess, cg, "CGPreflightListenEventAccess")
	purego.RegisterLibFunc(&cgRequestListenEventAccess, cg, "CGRequestListenEventAccess")
	purego.RegisterLibFunc(&cgPreflightScreenCaptureAccess, cg, "CGPreflightScreenCaptureAccess")
	purego.RegisterLibFunc(&cgRequestScreenCaptureAccess, cg, "CGRequestScreenCaptureAccess")
	if sym, err := purego.Dlsym(cg, "CGWindowListCreateImage"); err == nil && sym != 0 {
		purego.RegisterFunc(&cgWindowListCreateImage, sym)
	}

	purego.RegisterLibFunc(&axUIElementCreateApplication, as, "AXUIElementCreateApplication")
	purego.RegisterLibFunc(&axUIElementCopyAttributeValue, as, "AXUIElementCopyAttributeValue")
	purego.RegisterLibFunc(&axUIElementSetAttributeValue, as, "AXUIElementSetAttributeValue")
	purego.RegisterLibFunc(&axUIElementPerformAction, as, "AXUIElementPerformAction")
	purego.RegisterLibFunc(&axIsProcessTrustedWithOptions, as, "AXIsProcessTrustedWithOptions")
	kAXTrustedCheckOptionPrompt = mustGlobal(as, "kAXTrustedCheckOptionPrompt")
	if sym, err := purego.Dlsym(as, "_AXUIElementGetWindow"); err == nil && sym != 0 {
		purego.RegisterFunc(&axUIElementGetWindow, sym)
	}

	nsPasteboardURLReadingFileURLsOnlyKey = mustGlobal(ak, "NSPasteboardURLReadingFileURLsOnlyKey")

	purego.RegisterLibFunc(&dispatchAsyncF, sys, "dispatch_async_f")
	purego.RegisterLibFunc(&pthreadMainNP, sys, "pthread_main_np")
	q, err := purego.Dlsym(sys, "_dispatch_main_q")
	if err != nil {
		panic(err)
	}
	dispatchMainQ = q
	purego.RegisterLibFunc(&objcAutoreleasePoolPush, objcLib, "objc_autoreleasePoolPush")
	purego.RegisterLibFunc(&objcAutoreleasePoolPop, objcLib, "objc_autoreleasePoolPop")

	deviceRGB = cgColorSpaceCreateDeviceRGB()
	return nil
}

// mustGlobal reads a pointer-sized exported variable (e.g. a CFStringRef
// constant); Dlsym returns the variable's address.
func mustGlobal(lib uintptr, name string) uintptr {
	addr, err := purego.Dlsym(lib, name)
	if err != nil {
		panic(err)
	}
	return *(*uintptr)(uptr(addr))
}

// mustAddr returns an exported variable's address (for struct globals such
// as the CFDictionary callback tables, which are passed by pointer).
func mustAddr(lib uintptr, name string) uintptr {
	addr, err := purego.Dlsym(lib, name)
	if err != nil {
		panic(err)
	}
	return addr
}

// uptr turns a C pointer value into an unsafe.Pointer without tripping
// vet's uintptr->Pointer check; the memory is never Go-managed.
func uptr(u uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&u)) }

// Main-thread dispatch. dispatch_async_f takes a C function pointer and a
// context word; the context indexes a registry of pending Go funcs.
var (
	mainMu    sync.Mutex
	mainFuncs = map[uintptr]func(){}
	mainNext  uintptr
	mainTramp = purego.NewCallback(func(ctx uintptr) {
		mainMu.Lock()
		fn := mainFuncs[ctx]
		delete(mainFuncs, ctx)
		mainMu.Unlock()
		if fn != nil {
			pool := objcAutoreleasePoolPush()
			fn()
			objcAutoreleasePoolPop(pool)
		}
	})
)

func isMainThread() bool { return pthreadMainNP() == 1 }

// onMain queues fn on the main thread and returns immediately.
func onMain(fn func()) {
	mainMu.Lock()
	mainNext++
	ctx := mainNext
	mainFuncs[ctx] = fn
	mainMu.Unlock()
	dispatchAsyncF(dispatchMainQ, ctx, mainTramp)
}

// onMainSync runs fn on the main thread and waits for it. On the main
// thread itself fn runs inline (waiting there would deadlock).
func onMainSync(fn func()) {
	if isMainThread() {
		pool := objcAutoreleasePoolPush()
		fn()
		objcAutoreleasePoolPop(pool)
		return
	}
	done := make(chan struct{})
	onMain(func() {
		defer close(done)
		fn()
	})
	<-done
}

// withPool runs fn inside an autorelease pool. Pools are per OS thread, so
// the goroutine stays on its thread for the pool's lifetime.
func withPool(fn func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pool := objcAutoreleasePoolPush()
	defer objcAutoreleasePoolPop(pool)
	fn()
}

// Objective-C helpers.
var (
	selAlloc               = objc.RegisterName("alloc")
	selNew                 = objc.RegisterName("new")
	selRelease             = objc.RegisterName("release")
	selRetain              = objc.RegisterName("retain")
	selStringWithUTF8      = objc.RegisterName("stringWithUTF8String:")
	selUTF8String          = objc.RegisterName("UTF8String")
	selSharedApplication   = objc.RegisterName("sharedApplication")
	selSetActivationPolicy = objc.RegisterName("setActivationPolicy:")
	selRun                 = objc.RegisterName("run")
	selStop                = objc.RegisterName("stop:")
	selOtherEventWithType  = objc.RegisterName("otherEventWithType:location:modifierFlags:timestamp:windowNumber:context:subtype:data1:data2:")
	selPostEventAtStart    = objc.RegisterName("postEvent:atStart:")
	selLocalizedDesc       = objc.RegisterName("localizedDescription")
	selCount               = objc.RegisterName("count")
	selObjectAtIndex       = objc.RegisterName("objectAtIndex:")
)

func class(name string) objc.ID { return objc.ID(objc.GetClass(name)) }

// nsString returns an autoreleased NSString.
func nsString(s string) objc.ID { return class("NSString").Send(selStringWithUTF8, s) }

func goStringNS(s objc.ID) string {
	if s == 0 {
		return ""
	}
	return goCString(objc.Send[uintptr](s, selUTF8String))
}

func goCString(p uintptr) string {
	if p == 0 {
		return ""
	}
	n := 0
	for *(*byte)(uptr(p + uintptr(n))) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(uptr(p)), n))
}

func nsErrorString(e objc.ID) string {
	if e == 0 {
		return "unknown error"
	}
	return goStringNS(e.Send(selLocalizedDesc))
}

// CoreFoundation helpers.
var (
	cfStrMu    sync.Mutex
	cfStrCache = map[string]uintptr{}
)

// cfStr returns a cached, never-released CFString (the CFSTR equivalent).
func cfStr(s string) uintptr {
	cfStrMu.Lock()
	defer cfStrMu.Unlock()
	if c, ok := cfStrCache[s]; ok {
		return c
	}
	c := cfStringCreateWithCString(0, s, cfStringEncodingUTF8)
	cfStrCache[s] = c
	return c
}

func goStringCF(s uintptr) string {
	if s == 0 {
		return ""
	}
	n := cfStringGetMaximumSizeForEncode(cfStringGetLength(s), cfStringEncodingUTF8) + 1
	buf := make([]byte, n)
	if !cfStringGetCString(s, &buf[0], n, cfStringEncodingUTF8) {
		return ""
	}
	for i, b := range buf {
		if b == 0 {
			return string(buf[:i])
		}
	}
	return string(buf)
}

func cfInt(n uintptr) (int64, bool) {
	if n == 0 {
		return 0, false
	}
	var v int64
	ok := cfNumberGetValue(n, cfNumberSInt64Type, unsafe.Pointer(&v))
	return v, ok
}

func cfFloat(n uintptr) (float64, bool) {
	if n == 0 {
		return 0, false
	}
	var v float64
	ok := cfNumberGetValue(n, cfNumberDoubleType, unsafe.Pointer(&v))
	return v, ok
}

type macPlat struct {
	pid int32

	// input_darwin.go
	capturing atomic.Bool
	fn        func(Event) // set once before the tap is installed
	tap       atomic.Uintptr
	grab      atomic.Bool
	grabMu    sync.Mutex
	hidden    bool        // cursor hidden by us; under grabMu
	tapMods   [128]bool   // modifier keycodes held; tap thread only
	inj       injectState // under injMu
	injMu     sync.Mutex
	src       uintptr            // CGEventSource for injected events
	sckMu     sync.Mutex         // serializes ScreenCaptureKit lookups
	sckCache  map[uint32]objc.ID // windowID -> retained SCWindow
	sckStamp  int64              // unix nanos of sckCache fill

	// vdisplay_darwin.go
	vdMu     sync.Mutex
	vd       objc.ID // retained CGVirtualDisplay; main thread sends only
	vdW, vdH int32   // its size in points
	vdID     atomic.Uint32
}

var _ Platform = (*macPlat)(nil)

func New() (Platform, error) {
	if err := loadLibs(); err != nil {
		return nil, err
	}
	p := &macPlat{
		pid:      int32(os.Getpid()),
		src:      cgEventSourceCreate(1), // kCGEventSourceStateHIDSystemState
		sckCache: map[uint32]objc.ID{},
	}
	if p.src == 0 {
		return nil, errors.New("plat: CGEventSourceCreate failed")
	}
	enableBackgroundCursor()
	return p, nil
}

// Run owns the main thread: it starts NSApplication, runs app on another
// goroutine and returns once app has returned and the event loop stopped.
// Regular activation policy so overlay windows can become key and receive
// keyboard input.
func Run(app func()) error {
	if err := loadLibs(); err != nil {
		return err
	}
	if !isMainThread() {
		return errors.New("plat: Run must be called from the main goroutine")
	}
	pool := objcAutoreleasePoolPush()
	nsApp := class("NSApplication").Send(selSharedApplication)
	nsApp.Send(selSetActivationPolicy, 0) // NSApplicationActivationPolicyRegular
	objcAutoreleasePoolPop(pool)
	go func() {
		app()
		onMain(func() {
			nsApp.Send(selStop, objc.ID(0))
			// stop: takes effect only after the loop handles an event, and
			// a dispatched block is not one; post a no-op event to wake it.
			ev := class("NSEvent").Send(selOtherEventWithType,
				uint64(15), // NSEventTypeApplicationDefined
				cgPoint{}, uint64(0), float64(0), int64(0), objc.ID(0),
				int16(0), int64(0), int64(0))
			nsApp.Send(selPostEventAtStart, ev, true)
		})
	}()
	nsApp.Send(selRun)
	// Never leave the cursor hidden/detached behind.
	if p := tapPlat.Load(); p != nil {
		p.SetGrab(false)
	}
	return nil
}

func (p *macPlat) OS() string { return "darwin" }

func (p *macPlat) CursorPos() (x, y int32) {
	pt := cursorLocation()
	return int32(math.Floor(pt.X)), int32(math.Floor(pt.Y))
}

func cursorLocation() cgPoint {
	ev := cgEventCreate(0)
	if ev == 0 {
		return cgPoint{}
	}
	pt := cgEventGetLocation(ev)
	cfRelease(ev)
	return pt
}

// enableBackgroundCursor sets the private "SetsCursorInBackground"
// connection property so CGDisplayHideCursor works while another app is
// frontmost (the normal case while grabbing). Best effort: ignored when the
// private symbols are missing.
func enableBackgroundCursor() {
	var def func() int32
	var set func(cid, target int32, key, value uintptr) int32
	for _, path := range []string{
		"/System/Library/PrivateFrameworks/SkyLight.framework/SkyLight",
		"/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics",
	} {
		h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			continue
		}
		ds, err1 := purego.Dlsym(h, "_CGSDefaultConnection")
		ss, err2 := purego.Dlsym(h, "CGSSetConnectionProperty")
		if err1 != nil || err2 != nil || ds == 0 || ss == 0 {
			continue
		}
		purego.RegisterFunc(&def, ds)
		purego.RegisterFunc(&set, ss)
		cid := def()
		set(cid, cid, cfStr("SetsCursorInBackground"), kCFBooleanTrue)
		return
	}
}
