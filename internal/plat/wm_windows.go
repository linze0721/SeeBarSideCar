package plat

import (
	"errors"
	"fmt"
	"image"
	"path/filepath"
	"runtime"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	cfUnicodeText  = 13
	cfHDROP        = 15
	gmemMoveable   = 0x0002
	dropEffectCopy = 1

	gwOwner         = 4
	wsExToolWindow  = 0x00000080
	swRestore       = 9
	pwRenderFull    = 2
	dibRGBColors    = 0
	biRGB           = 0
	gwlExStyleIndex = -20
)

type bitmapInfo struct {
	size                  uint32
	width, height         int32
	planes, bitCount      uint16
	compression, sizeImg  uint32
	xPelsPerM, yPelsPerM  int32
	clrUsed, clrImportant uint32
	colors                [1]uint32
}

func newBitmapInfo(w, h int32) bitmapInfo {
	// Negative height = top-down rows, matching image.RGBA.
	return bitmapInfo{size: 40, width: w, height: -h, planes: 1, bitCount: 32, compression: biRGB}
}

// --- clipboard ---

var (
	shell32 = windows.NewLazySystemDLL("shell32.dll")

	procDragQueryFileW           = shell32.NewProc("DragQueryFileW")
	procRegisterClipboardFormatW = user32.NewProc("RegisterClipboardFormatW")
)

func clipSeq() uint64 {
	r, _, _ := procGetClipboardSequenceNumber.Call()
	return uint64(r)
}

func (p *winPlat) ClipboardSeq() uint64 { return clipSeq() }

// openClipboard must run on a locked OS thread: clipboard ownership is
// per thread until CloseClipboard.
func openClipboard(hwnd uintptr) error {
	var err error
	for range 20 {
		r, _, e := procOpenClipboard.Call(hwnd)
		if r != 0 {
			return nil
		}
		err = e
		time.Sleep(10 * time.Millisecond) // another app holds it, usually briefly
	}
	return callErr("OpenClipboard", err)
}

func (p *winPlat) ClipboardText() (string, uint64, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	seq := clipSeq()
	if r, _, _ := procIsClipboardFormatAvailable.Call(cfUnicodeText); r == 0 {
		return "", seq, nil
	}
	if err := openClipboard(0); err != nil {
		return "", seq, err
	}
	defer procCloseClipboard.Call()
	h, _, e := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", seq, callErr("GetClipboardData", e)
	}
	ptr, _, e := procGlobalLock.Call(h)
	if ptr == 0 {
		return "", seq, callErr("GlobalLock", e)
	}
	defer procGlobalUnlock.Call(h)
	sz, _, _ := procGlobalSize.Call(h)
	u := unsafe.Slice((*uint16)(uptr(ptr)), sz/2)
	for i, c := range u {
		if c == 0 {
			u = u[:i]
			break
		}
	}
	return string(utf16.Decode(u)), seq, nil
}

// ClipboardFiles reads CF_HDROP; Explorer's file copy offers no
// CF_UNICODETEXT, so ClipboardText reports "" for it.
func (p *winPlat) ClipboardFiles() ([]string, uint64, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	seq := clipSeq()
	if r, _, _ := procIsClipboardFormatAvailable.Call(cfHDROP); r == 0 {
		return nil, seq, nil
	}
	if err := openClipboard(0); err != nil {
		return nil, seq, err
	}
	defer procCloseClipboard.Call()
	h, _, e := procGetClipboardData.Call(cfHDROP)
	if h == 0 {
		return nil, seq, callErr("GetClipboardData", e)
	}
	n, _, _ := procDragQueryFileW.Call(h, 0xFFFFFFFF, 0, 0)
	var paths []string
	var buf []uint16
	for i := range n {
		l, _, _ := procDragQueryFileW.Call(h, i, 0, 0)
		if l == 0 {
			continue
		}
		if int(l)+1 > len(buf) {
			buf = make([]uint16, l+1)
		}
		l, _, _ = procDragQueryFileW.Call(h, i, uintptr(unsafe.Pointer(&buf[0])), l+1)
		paths = append(paths, string(utf16.Decode(buf[:l])))
	}
	return paths, seq, nil
}

func (p *winPlat) SetClipboardText(text string) (uint64, error) {
	return setClipboard(func() error { return setClipText(text) })
}

func setClipText(text string) error {
	u := utf16.Encode([]rune(text))
	u = append(u, 0)
	b := unsafe.Slice((*byte)(unsafe.Pointer(&u[0])), len(u)*2)
	return setClipData(cfUnicodeText, b)
}

func (p *winPlat) SetClipboardFiles(paths []string) (uint64, error) {
	// DROPFILES{pFiles uint32=20, pt POINT{0,0}, fNC BOOL=0, fWide BOOL=1}
	// then NUL-terminated UTF-16 paths and a final NUL.
	const hdr = 20
	var u []uint16
	for _, p := range paths {
		u = append(u, utf16.Encode([]rune(p))...)
		u = append(u, 0)
	}
	u = append(u, 0)
	b := make([]byte, hdr+len(u)*2)
	*(*uint32)(unsafe.Pointer(&b[0])) = hdr
	*(*uint32)(unsafe.Pointer(&b[16])) = 1
	copy(b[hdr:], unsafe.Slice((*byte)(unsafe.Pointer(&u[0])), len(u)*2))
	effect := []byte{dropEffectCopy, 0, 0, 0}
	cfEffect, _, e := procRegisterClipboardFormatW.Call(uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("Preferred DropEffect"))))
	if cfEffect == 0 {
		return 0, callErr("RegisterClipboardFormatW", e)
	}
	return setClipboard(func() error {
		if err := setClipData(cfHDROP, b); err != nil {
			return err
		}
		// Tells Explorer the paste is a copy, not a cut-move.
		return setClipData(cfEffect, effect)
	})
}

// setClipboard opens and empties the clipboard, runs put, and returns the
// sequence number after CloseClipboard.
func setClipboard(put func() error) (uint64, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// EmptyClipboard assigns ownership to the opener's window. Using a
	// NULL window for a write leaves no owner and can make SetClipboardData fail.
	const hwndMessage = ^uintptr(2) // HWND_MESSAGE == (HWND)-3
	hwnd, _, e := procCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("STATIC"))), 0, 0,
		0, 0, 0, 0, hwndMessage, 0, 0, 0)
	if hwnd == 0 {
		return 0, callErr("CreateWindowExW", e)
	}
	defer procDestroyWindow.Call(hwnd)
	if err := openClipboard(hwnd); err != nil {
		return 0, err
	}
	err := func() error {
		defer procCloseClipboard.Call()
		if r, _, e := procEmptyClipboard.Call(); r == 0 {
			return callErr("EmptyClipboard", e)
		}
		return put()
	}()
	if err != nil {
		return 0, err
	}
	return clipSeq(), nil
}

// setClipData copies b into moveable global memory and hands it to the
// open clipboard as format cf.
func setClipData(cf uintptr, b []byte) error {
	h, _, e := procGlobalAlloc.Call(gmemMoveable, uintptr(len(b)))
	if h == 0 {
		return callErr("GlobalAlloc", e)
	}
	ptr, _, e := procGlobalLock.Call(h)
	if ptr == 0 {
		procGlobalFree.Call(h)
		return callErr("GlobalLock", e)
	}
	copy(unsafe.Slice((*byte)(uptr(ptr)), len(b)), b)
	procGlobalUnlock.Call(h)
	if r, _, e := procSetClipboardData.Call(cf, h); r == 0 {
		procGlobalFree.Call(h) // ownership passes to the system only on success
		return callErr("SetClipboardData", e)
	}
	return nil
}

// --- window enumeration ---

var (
	enumMu    sync.Mutex
	enumHWNDs []windows.HWND
	enumProc  = windows.NewCallback(func(hwnd windows.HWND, _ uintptr) uintptr {
		enumHWNDs = append(enumHWNDs, hwnd)
		return 1
	})
)

func topLevelWindows() []windows.HWND {
	enumMu.Lock()
	defer enumMu.Unlock()
	enumHWNDs = enumHWNDs[:0]
	windows.EnumWindows(enumProc, nil)
	return append([]windows.HWND(nil), enumHWNDs...)
}

func isIconic(h windows.HWND) bool {
	r, _, _ := procIsIconic.Call(uintptr(h))
	return r != 0
}

func windowTitle(h windows.HWND) string {
	n, _, _ := procGetWindowTextLengthW.Call(uintptr(h))
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	r, _, _ := procGetWindowTextW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf[:r])
}

func frameBounds(h windows.HWND) (windows.Rect, error) {
	var r windows.Rect
	if err := windows.DwmGetWindowAttribute(h, windows.DWMWA_EXTENDED_FRAME_BOUNDS, unsafe.Pointer(&r), uint32(unsafe.Sizeof(r))); err == nil {
		return r, nil
	}
	// DWM attribute unavailable (e.g. some legacy windows): the full window rect.
	if ok, _, e := procGetWindowRect.Call(uintptr(h), uintptr(unsafe.Pointer(&r))); ok == 0 {
		return r, callErr("GetWindowRect", e)
	}
	return r, nil
}

func exeName(pid uint32) string {
	ph, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(ph)
	var buf [windows.MAX_LONG_PATH]uint16
	n := uint32(len(buf))
	if windows.QueryFullProcessImageName(ph, 0, &buf[0], &n) != nil {
		return ""
	}
	return filepath.Base(windows.UTF16ToString(buf[:n]))
}

func (p *winPlat) ListWindows() ([]WindowInfo, error) {
	exStyleIdx := int32(gwlExStyleIndex)
	names := make(map[uint32]string)
	var out []WindowInfo
	for _, h := range topLevelWindows() {
		if !windows.IsWindowVisible(h) || isIconic(h) {
			continue
		}
		if owner, _, _ := procGetWindow.Call(uintptr(h), gwOwner); owner != 0 {
			continue
		}
		if ex, _, _ := procGetWindowLongW.Call(uintptr(h), uintptr(exStyleIdx)); ex&wsExToolWindow != 0 {
			continue
		}
		var cloaked uint32
		if windows.DwmGetWindowAttribute(h, windows.DWMWA_CLOAKED, unsafe.Pointer(&cloaked), 4) == nil && cloaked != 0 {
			continue
		}
		title := windowTitle(h)
		if title == "" {
			continue
		}
		var pid uint32
		if _, err := windows.GetWindowThreadProcessId(h, &pid); err != nil || pid == p.ownPID {
			continue
		}
		r, err := frameBounds(h)
		if err != nil || r.Right <= r.Left || r.Bottom <= r.Top {
			continue
		}
		app, ok := names[pid]
		if !ok {
			app = exeName(pid)
			names[pid] = app
		}
		out = append(out, WindowInfo{
			ID: uint64(h), PID: pid, App: app, Title: title,
			Bounds: Rect{X: r.Left, Y: r.Top, W: r.Right - r.Left, H: r.Bottom - r.Top},
		})
	}
	return out, nil
}

// --- window capture ---

// winCapture is a per-window memory DC + DIB reused while the window size
// stays the same, so steady streaming does not churn GDI objects.
type winCapture struct {
	mu     sync.Mutex
	dc     uintptr
	bmp    uintptr
	oldBmp uintptr
	bits   unsafe.Pointer
	w, h   int32
	dead   bool // released; a racing CaptureWindow must not recreate GDI objects
}

func (c *winCapture) release() {
	if c.bmp != 0 {
		procSelectObject.Call(c.dc, c.oldBmp)
		procDeleteObject.Call(c.bmp)
		c.bmp, c.bits, c.w, c.h = 0, nil, 0, 0
	}
	c.dead = true
	if c.dc != 0 {
		procDeleteDC.Call(c.dc)
		c.dc = 0
	}
}

func (c *winCapture) ensure(w, h int32) error {
	if c.dead {
		return errors.New("plat: window closed")
	}
	if c.dc == 0 {
		dc, _, e := procCreateCompatibleDC.Call(0)
		if dc == 0 {
			return callErr("CreateCompatibleDC", e)
		}
		c.dc = dc
	}
	if c.bmp != 0 && c.w == w && c.h == h {
		return nil
	}
	if c.bmp != 0 {
		procSelectObject.Call(c.dc, c.oldBmp)
		procDeleteObject.Call(c.bmp)
		c.bmp = 0
	}
	bi := newBitmapInfo(w, h)
	var bits uintptr
	bmp, _, e := procCreateDIBSection.Call(c.dc, uintptr(unsafe.Pointer(&bi)), dibRGBColors,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bmp == 0 {
		return callErr("CreateDIBSection", e)
	}
	c.oldBmp, _, _ = procSelectObject.Call(c.dc, bmp)
	c.bmp, c.bits, c.w, c.h = bmp, uptr(bits), w, h
	return nil
}

func (p *winPlat) captureState(h windows.HWND) *winCapture {
	p.capMu.Lock()
	defer p.capMu.Unlock()
	key := uintptr(h)
	if c, ok := p.caps[key]; ok {
		return c
	}
	// New window being streamed: drop state of windows that no longer exist.
	for k, c := range p.caps {
		if !windows.IsWindow(windows.HWND(k)) {
			delete(p.caps, k)
			c.mu.Lock()
			c.release()
			c.mu.Unlock()
		}
	}
	c := &winCapture{}
	p.caps[key] = c
	return c
}

func (p *winPlat) CaptureWindow(id uint64) (*image.RGBA, Rect, error) {
	h := windows.HWND(id)
	if !windows.IsWindow(h) {
		return nil, Rect{}, fmt.Errorf("plat: window %#x does not exist", id)
	}
	if isIconic(h) {
		return nil, Rect{}, fmt.Errorf("plat: window %#x is minimized", id)
	}
	var wr windows.Rect
	if ok, _, e := procGetWindowRect.Call(uintptr(h), uintptr(unsafe.Pointer(&wr))); ok == 0 {
		return nil, Rect{}, callErr("GetWindowRect", e)
	}
	fb, err := frameBounds(h)
	if err != nil {
		return nil, Rect{}, err
	}
	ww, wh := wr.Right-wr.Left, wr.Bottom-wr.Top
	if ww <= 0 || wh <= 0 {
		return nil, Rect{}, fmt.Errorf("plat: window %#x has empty bounds", id)
	}
	// Crop the invisible resize borders: frame bounds relative to the window rect.
	x0, y0 := max(fb.Left-wr.Left, 0), max(fb.Top-wr.Top, 0)
	x1, y1 := min(fb.Right-wr.Left, ww), min(fb.Bottom-wr.Top, wh)
	if x1 <= x0 || y1 <= y0 {
		x0, y0, x1, y1 = 0, 0, ww, wh
	}

	c := p.captureState(h)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensure(ww, wh); err != nil {
		return nil, Rect{}, err
	}
	if r, _, e := procPrintWindow.Call(uintptr(h), c.dc, pwRenderFull); r == 0 {
		return nil, Rect{}, callErr("PrintWindow", e)
	}
	procGdiFlush.Call()

	src := unsafe.Slice((*byte)(c.bits), int(ww)*int(wh)*4)
	img := image.NewRGBA(image.Rect(0, 0, int(x1-x0), int(y1-y0)))
	stride := int(ww) * 4
	for y := y0; y < y1; y++ {
		s := src[int(y)*stride+int(x0)*4 : int(y)*stride+int(x1)*4]
		d := img.Pix[int(y-y0)*img.Stride:]
		for i := 0; i < len(s); i += 4 {
			d[i], d[i+1], d[i+2], d[i+3] = s[i+2], s[i+1], s[i], 255
		}
	}
	return img, Rect{X: wr.Left + x0, Y: wr.Top + y0, W: x1 - x0, H: y1 - y0}, nil
}

// --- focus ---

func (p *winPlat) FocusWindow(id uint64) error {
	h := windows.HWND(id)
	if !windows.IsWindow(h) {
		return fmt.Errorf("plat: window %#x does not exist", id)
	}
	if isIconic(h) {
		procShowWindow.Call(uintptr(h), swRestore)
	}
	if setForeground(h) {
		return nil
	}
	// Foreground lock: only the process that received the last input may
	// change the foreground. Attaching to the foreground thread's input
	// queue shares that right with us.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	cur := windows.GetCurrentThreadId()
	if fg := windows.GetForegroundWindow(); fg != 0 {
		if fgTID, _ := windows.GetWindowThreadProcessId(fg, nil); fgTID != 0 && fgTID != cur {
			if r, _, _ := procAttachThreadInput.Call(uintptr(cur), uintptr(fgTID), 1); r != 0 {
				defer procAttachThreadInput.Call(uintptr(cur), uintptr(fgTID), 0)
			}
		}
	}
	if setForeground(h) {
		return nil
	}
	// Last resort: an injected Alt tap makes us "the last input source".
	var in [2]winInput
	for i := range in {
		in[i].typ = inputKeyboard
		ki := (*keybdInput)(unsafe.Pointer(&in[i].mi))
		ki.vk = vkMenu
		if i == 1 {
			ki.flags = keyUp
		}
	}
	if err := sendInput(in[:]); err != nil {
		return err
	}
	if setForeground(h) {
		return nil
	}
	return errors.New("plat: SetForegroundWindow refused (foreground lock)")
}

func setForeground(h windows.HWND) bool {
	procSetForegroundWindow.Call(uintptr(h))
	procBringWindowToTop.Call(uintptr(h))
	return windows.GetForegroundWindow() == h
}
