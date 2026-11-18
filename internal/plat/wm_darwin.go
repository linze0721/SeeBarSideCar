package plat

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"math"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

const (
	pasteboardTypeString = "public.utf8-plain-text" // NSPasteboardTypeString
	pasteboardTypePNG    = "public.png"
	pasteboardTypeTIFF   = "public.tiff"
	bitmapImageTypePNG   = uint64(4) // NSBitmapImageFileTypePNG
)

var (
	selGeneralPasteboard = objc.RegisterName("generalPasteboard")
	selChangeCount       = objc.RegisterName("changeCount")
	selStringForType     = objc.RegisterName("stringForType:")
	selClearContents     = objc.RegisterName("clearContents")
	selSetStringForType  = objc.RegisterName("setString:forType:")

	selDataForType        = objc.RegisterName("dataForType:")
	selSetDataForType     = objc.RegisterName("setData:forType:")
	selDataWithBytes      = objc.RegisterName("dataWithBytes:length:")
	selBytes              = objc.RegisterName("bytes")
	selLength             = objc.RegisterName("length")
	selImageRepWithData   = objc.RegisterName("imageRepWithData:")
	selRepUsingType       = objc.RegisterName("representationUsingType:properties:")
	selTIFFRepresentation = objc.RegisterName("TIFFRepresentation")
	selDictionary         = objc.RegisterName("dictionary")

	selReadObjects       = objc.RegisterName("readObjectsForClasses:options:")
	selWriteObjects      = objc.RegisterName("writeObjects:")
	selClass             = objc.RegisterName("class")
	selArrayWithObject   = objc.RegisterName("arrayWithObject:")
	selArrayWithCapacity = objc.RegisterName("arrayWithCapacity:")
	selAddObject         = objc.RegisterName("addObject:")
	selDictWithObjForKey = objc.RegisterName("dictionaryWithObject:forKey:")
	selNumberWithBool    = objc.RegisterName("numberWithBool:")
	selFileURLWithPath   = objc.RegisterName("fileURLWithPath:")
	selPath              = objc.RegisterName("path")
)

func (p *macPlat) ClipboardSeq() (seq uint64) {
	withPool(func() {
		pb := class("NSPasteboard").Send(selGeneralPasteboard)
		seq = uint64(objc.Send[int64](pb, selChangeCount))
	})
	return seq
}

// ClipboardText reports "" while file URLs are on the pasteboard: Finder
// also puts the file names there as plain text, and files take precedence.
func (p *macPlat) ClipboardText() (text string, seq uint64, err error) {
	withPool(func() {
		pb := class("NSPasteboard").Send(selGeneralPasteboard)
		seq = uint64(objc.Send[int64](pb, selChangeCount))
		if len(pasteboardFiles(pb)) == 0 {
			text = goStringNS(pb.Send(selStringForType, nsString(pasteboardTypeString)))
		}
	})
	return text, seq, nil
}

func (p *macPlat) SetClipboardText(text string) (seq uint64, err error) {
	withPool(func() {
		pb := class("NSPasteboard").Send(selGeneralPasteboard)
		pb.Send(selClearContents)
		if !objc.Send[bool](pb, selSetStringForType, nsString(text), nsString(pasteboardTypeString)) {
			err = errors.New("plat: NSPasteboard setString:forType: failed")
		}
		seq = uint64(objc.Send[int64](pb, selChangeCount))
	})
	return seq, err
}

func (p *macPlat) ClipboardFiles() (paths []string, seq uint64, err error) {
	withPool(func() {
		pb := class("NSPasteboard").Send(selGeneralPasteboard)
		seq = uint64(objc.Send[int64](pb, selChangeCount))
		paths = pasteboardFiles(pb)
	})
	return paths, seq, nil
}

// pasteboardFiles returns the paths of the pasteboard's file URLs. Needs
// an autorelease pool.
func pasteboardFiles(pb objc.ID) []string {
	classes := class("NSArray").Send(selArrayWithObject, class("NSURL").Send(selClass))
	opts := class("NSDictionary").Send(selDictWithObjForKey,
		class("NSNumber").Send(selNumberWithBool, true), objc.ID(nsPasteboardURLReadingFileURLsOnlyKey))
	urls := pb.Send(selReadObjects, classes, opts)
	if urls == 0 {
		return nil
	}
	var paths []string
	for i := range objc.Send[uint64](urls, selCount) {
		if p := goStringNS(urls.Send(selObjectAtIndex, i).Send(selPath)); p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

func (p *macPlat) SetClipboardFiles(paths []string) (seq uint64, err error) {
	withPool(func() {
		urls := class("NSMutableArray").Send(selArrayWithCapacity, uint64(len(paths)))
		for _, path := range paths {
			urls.Send(selAddObject, class("NSURL").Send(selFileURLWithPath, nsString(path)))
		}
		pb := class("NSPasteboard").Send(selGeneralPasteboard)
		pb.Send(selClearContents)
		if !objc.Send[bool](pb, selWriteObjects, urls) {
			err = errors.New("plat: NSPasteboard writeObjects: failed")
		}
		seq = uint64(objc.Send[int64](pb, selChangeCount))
	})
	return seq, err
}

// ClipboardImage reads PNG or TIFF even when file URLs are present. The app
// decides whether readable files take precedence. Finder's ICNS is ignored.
func (p *macPlat) ClipboardImage() (png []byte, seq uint64, err error) {
	withPool(func() {
		pb := class("NSPasteboard").Send(selGeneralPasteboard)
		seq = uint64(objc.Send[int64](pb, selChangeCount))
		data := pb.Send(selDataForType, nsString(pasteboardTypePNG))
		if data == 0 {
			tiff := pb.Send(selDataForType, nsString(pasteboardTypeTIFF))
			if tiff == 0 {
				return
			}
			rep := class("NSBitmapImageRep").Send(selImageRepWithData, tiff)
			data = rep.Send(selRepUsingType, bitmapImageTypePNG, class("NSDictionary").Send(selDictionary))
			if data == 0 {
				err = errors.New("plat: could not convert clipboard TIFF to PNG")
				return
			}
		}
		n := objc.Send[uint64](data, selLength)
		png = bytes.Clone(unsafe.Slice(objc.Send[*byte](data, selBytes), n))
	})
	return png, seq, err
}

func (p *macPlat) SetClipboardImage(png []byte, text string) (seq uint64, err error) {
	withPool(func() {
		pb := class("NSPasteboard").Send(selGeneralPasteboard)
		defer func() { seq = uint64(objc.Send[int64](pb, selChangeCount)) }()

		data := class("NSData").Send(selDataWithBytes, unsafe.SliceData(png), uint64(len(png)))
		rep := class("NSBitmapImageRep").Send(selImageRepWithData, data)
		tiff := rep.Send(selTIFFRepresentation)
		if tiff == 0 {
			err = errors.New("plat: could not convert clipboard PNG to TIFF")
			return
		}

		pb.Send(selClearContents)
		if !objc.Send[bool](pb, selSetDataForType, data, nsString(pasteboardTypePNG)) {
			err = errors.New("plat: NSPasteboard setData:forType: failed for public.png")
			return
		}
		if !objc.Send[bool](pb, selSetDataForType, tiff, nsString(pasteboardTypeTIFF)) {
			err = errors.New("plat: NSPasteboard setData:forType: failed for public.tiff")
			return
		}
		if text != "" && !objc.Send[bool](pb, selSetStringForType, nsString(text), nsString(pasteboardTypeString)) {
			err = errors.New("plat: NSPasteboard setString:forType: failed")
		}
	})
	return seq, err
}

// CGWindowListOption values.
const (
	winListOnScreenOnly     = 1
	winListIncludingWindow  = 8
	winListExcludeDesktop   = 16
	winImageBoundsIgnoreFrm = 1
	winImageBestResolution  = 8
)

func (p *macPlat) ListWindows() ([]WindowInfo, error) {
	arr := cgWindowListCopyWindowInfo(winListOnScreenOnly|winListExcludeDesktop, 0)
	if arr == 0 {
		return nil, errors.New("plat: CGWindowListCopyWindowInfo failed")
	}
	defer cfRelease(arr)
	var out []WindowInfo
	for i := range cfArrayGetCount(arr) {
		w, ok := parseWindow(cfArrayGetValueAtIndex(arr, i))
		if !ok || int32(w.PID) == p.pid || w.Bounds.W < 50 || w.Bounds.H < 50 {
			continue
		}
		out = append(out, w)
	}
	return out, nil
}

// parseWindow reads one kCGWindow* dictionary; only normal-layer, visible
// windows are ok. Window titles need Screen Recording permission; without
// it (or for untitled windows) the app name stands in.
func parseWindow(d uintptr) (WindowInfo, bool) {
	if d == 0 {
		return WindowInfo{}, false
	}
	if layer, _ := cfInt(cfDictionaryGetValue(d, cfStr("kCGWindowLayer"))); layer != 0 {
		return WindowInfo{}, false
	}
	if a, ok := cfFloat(cfDictionaryGetValue(d, cfStr("kCGWindowAlpha"))); ok && a == 0 {
		return WindowInfo{}, false
	}
	id, ok := cfInt(cfDictionaryGetValue(d, cfStr("kCGWindowNumber")))
	if !ok {
		return WindowInfo{}, false
	}
	pid, _ := cfInt(cfDictionaryGetValue(d, cfStr("kCGWindowOwnerPID")))
	var r cgRect
	if b := cfDictionaryGetValue(d, cfStr("kCGWindowBounds")); b == 0 || !cgRectMakeWithDictionaryRepresentation(b, &r) {
		return WindowInfo{}, false
	}
	w := WindowInfo{
		ID:    uint64(id),
		PID:   uint32(pid),
		App:   goStringCF(cfDictionaryGetValue(d, cfStr("kCGWindowOwnerName"))),
		Title: goStringCF(cfDictionaryGetValue(d, cfStr("kCGWindowName"))),
		Bounds: Rect{
			X: int32(math.Round(r.X)), Y: int32(math.Round(r.Y)),
			W: int32(math.Round(r.W)), H: int32(math.Round(r.H)),
		},
	}
	if w.Title == "" {
		w.Title = w.App
	}
	return w, true
}

// lookupWindow returns the current info of one window (any layer check
// applies), or false when it no longer exists.
func lookupWindow(id uint32) (WindowInfo, bool) {
	arr := cgWindowListCopyWindowInfo(winListIncludingWindow, id)
	if arr == 0 {
		return WindowInfo{}, false
	}
	defer cfRelease(arr)
	for i := range cfArrayGetCount(arr) {
		if w, ok := parseWindow(cfArrayGetValueAtIndex(arr, i)); ok && w.ID == uint64(id) {
			return w, true
		}
	}
	return WindowInfo{}, false
}

func (p *macPlat) CaptureWindow(id uint64) (*image.RGBA, Rect, error) {
	w, ok := lookupWindow(uint32(id))
	if !ok {
		return nil, Rect{}, fmt.Errorf("plat: window %d not found", id)
	}
	var img uintptr
	if cgWindowListCreateImage != nil {
		null := cgRect{X: math.Inf(1), Y: math.Inf(1)} // CGRectNull: the window's own bounds
		img = cgWindowListCreateImage(null, winListIncludingWindow, uint32(id), winImageBoundsIgnoreFrm|winImageBestResolution)
	}
	if img == 0 {
		var err error
		if img, err = p.sckCapture(uint32(id)); err != nil {
			return nil, Rect{}, err
		}
	}
	defer cgImageRelease(img)
	rgba := cgImageToRGBA(img)
	if rgba == nil {
		return nil, Rect{}, errors.New("plat: window image is empty (grant Screen Recording in System Settings › Privacy & Security)")
	}
	return rgba, w.Bounds, nil
}

// cgImageToRGBA draws img into a new premultiplied RGBA buffer, which is
// exactly image.RGBA's layout.
func cgImageToRGBA(img uintptr) *image.RGBA {
	w, h := cgImageGetWidth(img), cgImageGetHeight(img)
	if w <= 0 || h <= 0 {
		return nil
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	// The context writes into Go memory across several calls.
	var pin runtime.Pinner
	pin.Pin(&out.Pix[0])
	defer pin.Unpin()
	// kCGImageAlphaPremultipliedLast | kCGBitmapByteOrder32Big = R,G,B,A bytes.
	ctx := cgBitmapContextCreate(unsafe.Pointer(&out.Pix[0]), w, h, 8, out.Stride, deviceRGB, 1|0x4000)
	if ctx == 0 {
		return nil
	}
	cgContextDrawImage(ctx, cgRect{W: float64(w), H: float64(h)}, img)
	cgContextRelease(ctx)
	return out
}

// ScreenCaptureKit fallback (macOS 14+), used when CGWindowListCreateImage
// is unavailable or returns nothing.
var (
	sckOnce sync.Once
	sckErr  error

	selGetShareableContent    = objc.RegisterName("getShareableContentWithCompletionHandler:")
	selWindows                = objc.RegisterName("windows")
	selWindowID               = objc.RegisterName("windowID")
	selInitWithDesktopIndepWn = objc.RegisterName("initWithDesktopIndependentWindow:")
	selContentRect            = objc.RegisterName("contentRect")
	selPointPixelScale        = objc.RegisterName("pointPixelScale")
	selSetWidth               = objc.RegisterName("setWidth:")
	selSetHeight              = objc.RegisterName("setHeight:")
	selSetShowsCursor         = objc.RegisterName("setShowsCursor:")
	selCaptureImage           = objc.RegisterName("captureImageWithFilter:configuration:completionHandler:")
)

const sckTimeout = 2 * time.Second

func loadSCK() error {
	sckOnce.Do(func() {
		if _, err := purego.Dlopen("/System/Library/Frameworks/ScreenCaptureKit.framework/ScreenCaptureKit", purego.RTLD_NOW|purego.RTLD_GLOBAL); err != nil {
			sckErr = fmt.Errorf("plat: loading ScreenCaptureKit: %w", err)
			return
		}
		if objc.GetClass("SCScreenshotManager") == 0 {
			sckErr = errors.New("plat: SCScreenshotManager unavailable (needs macOS 14+)")
		}
	})
	return sckErr
}

// asyncResult hands a completion-handler result to a waiter that may have
// given up; abandoned results are released by drop.
type asyncResult struct {
	mu        sync.Mutex
	abandoned bool
	ch        chan [2]uintptr
	drop      func(v [2]uintptr)
}

func (r *asyncResult) deliver(v [2]uintptr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.abandoned {
		r.drop(v)
		return
	}
	r.ch <- v
}

func (r *asyncResult) wait() ([2]uintptr, bool) {
	select {
	case v := <-r.ch:
		return v, true
	case <-time.After(sckTimeout):
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.abandoned = true
	select {
	case v := <-r.ch:
		return v, true
	default:
		return [2]uintptr{}, false
	}
}

func newAsyncResult(drop func([2]uintptr)) *asyncResult {
	return &asyncResult{ch: make(chan [2]uintptr, 1), drop: drop}
}

// sckWindow returns a retained SCWindow for id. The shareable-content
// query is slow, so the id→SCWindow table is reused for a short while.
func (p *macPlat) sckWindow(id uint32) (objc.ID, error) {
	p.sckMu.Lock()
	defer p.sckMu.Unlock()
	if time.Since(time.Unix(0, p.sckStamp)) < sckTimeout {
		if w, ok := p.sckCache[id]; ok {
			return w.Send(selRetain), nil
		}
	}
	res := newAsyncResult(func(v [2]uintptr) {
		if v[0] != 0 {
			objc.ID(v[0]).Send(selRelease)
		}
	})
	var errText string
	block := objc.NewBlock(func(_ objc.Block, content, nserr objc.ID) {
		if content == 0 {
			errText = nsErrorString(nserr)
			res.deliver([2]uintptr{})
			return
		}
		res.deliver([2]uintptr{uintptr(content.Send(selRetain))})
	})
	class("SCShareableContent").Send(selGetShareableContent, block)
	block.Release()
	v, ok := res.wait()
	if !ok {
		return 0, errors.New("plat: ScreenCaptureKit did not answer (Screen Recording permission?)")
	}
	content := objc.ID(v[0])
	if content == 0 {
		return 0, fmt.Errorf("plat: ScreenCaptureKit: %s (grant Screen Recording in System Settings › Privacy & Security)", errText)
	}
	defer content.Send(selRelease)
	for _, w := range p.sckCache {
		w.Send(selRelease)
	}
	clear(p.sckCache)
	wins := content.Send(selWindows)
	for i := range objc.Send[uint64](wins, selCount) {
		w := wins.Send(selObjectAtIndex, i)
		p.sckCache[objc.Send[uint32](w, selWindowID)] = w.Send(selRetain)
	}
	p.sckStamp = time.Now().UnixNano()
	w, ok := p.sckCache[id]
	if !ok {
		return 0, fmt.Errorf("plat: window %d not shareable", id)
	}
	return w.Send(selRetain), nil
}

// sckCapture returns a retained CGImage of the window.
func (p *macPlat) sckCapture(id uint32) (img uintptr, err error) {
	if err := loadSCK(); err != nil {
		return 0, err
	}
	win, err := p.sckWindow(id)
	if err != nil {
		return 0, err
	}
	var block objc.Block
	var errText string
	res := newAsyncResult(func(v [2]uintptr) {
		if v[0] != 0 {
			cgImageRelease(v[0])
		}
	})
	withPool(func() {
		defer win.Send(selRelease)
		filter := class("SCContentFilter").Send(selAlloc).Send(selInitWithDesktopIndepWn, win)
		if filter == 0 {
			err = errors.New("plat: SCContentFilter init failed")
			return
		}
		defer filter.Send(selRelease)
		rect := objc.Send[cgRect](filter, selContentRect)
		scale := float64(objc.Send[float32](filter, selPointPixelScale))
		if scale <= 0 {
			scale = 1
		}
		conf := class("SCStreamConfiguration").Send(selNew)
		defer conf.Send(selRelease)
		conf.Send(selSetWidth, uint64(max(1, math.Round(rect.W*scale))))
		conf.Send(selSetHeight, uint64(max(1, math.Round(rect.H*scale))))
		conf.Send(selSetShowsCursor, false)
		block = objc.NewBlock(func(_ objc.Block, image uintptr, nserr objc.ID) {
			if image == 0 {
				errText = nsErrorString(nserr)
				res.deliver([2]uintptr{})
				return
			}
			res.deliver([2]uintptr{cgImageRetain(image)})
		})
		class("SCScreenshotManager").Send(selCaptureImage, filter, conf, block)
		block.Release()
	})
	if err != nil {
		return 0, err
	}
	v, ok := res.wait()
	if !ok {
		return 0, errors.New("plat: ScreenCaptureKit capture timed out")
	}
	if v[0] == 0 {
		return 0, fmt.Errorf("plat: ScreenCaptureKit capture failed: %s (grant Screen Recording in System Settings › Privacy & Security)", errText)
	}
	return v[0], nil
}

var (
	selRunningAppWithPID = objc.RegisterName("runningApplicationWithProcessIdentifier:")
	selActivateWithOpts  = objc.RegisterName("activateWithOptions:")
)

func (p *macPlat) FocusWindow(id uint64) error {
	w, ok := lookupWindow(uint32(id))
	if !ok {
		return fmt.Errorf("plat: window %d not found", id)
	}
	var activated bool
	withPool(func() {
		app := class("NSRunningApplication").Send(selRunningAppWithPID, int32(w.PID))
		if app != 0 {
			// NSApplicationActivateIgnoringOtherApps
			activated = objc.Send[bool](app, selActivateWithOpts, uint64(2))
		}
	})
	if !activated {
		return fmt.Errorf("plat: activating %s (pid %d) failed", w.App, w.PID)
	}
	// Raising the specific window needs Accessibility; failure here still
	// leaves the app in front.
	raiseAXWindow(int32(w.PID), uint32(id), w.Title)
	return nil
}

func raiseAXWindow(pid int32, id uint32, title string) {
	app := axUIElementCreateApplication(pid)
	if app == 0 {
		return
	}
	defer cfRelease(app)
	var wins uintptr
	if axUIElementCopyAttributeValue(app, cfStr("AXWindows"), &wins) != 0 || wins == 0 {
		return
	}
	defer cfRelease(wins)
	n := cfArrayGetCount(wins)
	for i := range n {
		el := cfArrayGetValueAtIndex(wins, i)
		if axWindowMatches(el, id, title) {
			axUIElementSetAttributeValue(el, cfStr("AXMain"), kCFBooleanTrue)
			axUIElementPerformAction(el, cfStr("AXRaise"))
			return
		}
	}
}

// axWindowMatches uses the private _AXUIElementGetWindow when present and
// falls back to the title.
func axWindowMatches(el uintptr, id uint32, title string) bool {
	if axUIElementGetWindow != nil {
		var wid uint32
		if axUIElementGetWindow(el, &wid) == 0 {
			return wid == id
		}
	}
	var t uintptr
	if axUIElementCopyAttributeValue(el, cfStr("AXTitle"), &t) != 0 || t == 0 {
		return false
	}
	defer cfRelease(t)
	return goStringCF(t) == title
}
