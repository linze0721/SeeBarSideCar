package app

import (
	"errors"
	"image"
	"slices"
	"sync"
	"testing"
	"time"

	"magic/internal/plat"
)

// fakePlat is an in-memory plat.Platform. Tests drive local input through
// push and inspect what the core did through the recorded fields.
type fakePlat struct {
	os string

	mu        sync.Mutex
	bounds    plat.Rect
	cx, cy    int32
	capture   func(plat.Event)
	grab      bool
	injected  []plat.Event
	onInject  func(plat.Event) // optional observer installed before starting the core
	clipText  string
	clipFiles []string
	clipImage []byte
	clipSeq   uint64
	clipSets  int // clipboard setter calls
	clipGets  int // clipboard content reads (not sequence polls)
	seqGets   int // ClipboardSeq calls
	wins      map[uint64]fakeWindow
	order     []uint64 // window IDs in insertion order
	focused   []uint64
	overlays  []*fakeOverlay

	monitors []plat.Monitor // nil: one monitor covering bounds
	vd       *plat.VirtualDisplay
	vdErr    error                  // SetVirtualDisplay(non-nil) fails with it
	vdReqs   []*plat.VirtualDisplay // every SetVirtualDisplay argument
}

type fakeWindow struct {
	info plat.WindowInfo
	img  *image.RGBA
}

var _ plat.Platform = (*fakePlat)(nil)

func newFakePlat(os string, bounds plat.Rect) *fakePlat {
	return &fakePlat{os: os, bounds: bounds, wins: make(map[uint64]fakeWindow)}
}

func (p *fakePlat) OS() string { return p.os }

func (p *fakePlat) Bounds() plat.Rect {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bounds
}

func (p *fakePlat) setBounds(r plat.Rect) {
	p.mu.Lock()
	p.bounds = r
	p.mu.Unlock()
}

func (p *fakePlat) Monitors() []plat.Monitor {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.monitors == nil {
		return []plat.Monitor{{Rect: p.bounds, UnitsPerLogical: 1, Scale: 1, Primary: true}}
	}
	return append([]plat.Monitor(nil), p.monitors...)
}

// setMonitors replaces the real monitors; Bounds becomes their union.
func (p *fakePlat) setMonitors(ms ...plat.Monitor) {
	p.mu.Lock()
	p.monitors = ms
	p.bounds = unionMonitors(ms)
	p.mu.Unlock()
}

func (p *fakePlat) SetVirtualDisplay(v *plat.VirtualDisplay) (plat.Rect, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v == nil {
		p.vdReqs = append(p.vdReqs, nil)
		p.vd = nil
		return plat.Rect{}, nil
	}
	vc := *v
	p.vdReqs = append(p.vdReqs, &vc)
	if p.vdErr != nil {
		p.vd = nil
		return plat.Rect{}, p.vdErr
	}
	p.vd = &vc
	return vc.Rect, nil
}

func (p *fakePlat) setVDErr(err error) {
	p.mu.Lock()
	p.vdErr = err
	p.mu.Unlock()
}

// virtual returns the applied virtual display (nil if none) and the number
// of SetVirtualDisplay calls so far.
func (p *fakePlat) virtual() (*plat.VirtualDisplay, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.vd, len(p.vdReqs)
}

func (p *fakePlat) CursorPos() (int32, int32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cx, p.cy
}

func (p *fakePlat) Capture(fn func(plat.Event)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.capture != nil {
		return errors.New("fake: capture already installed")
	}
	p.capture = fn
	return nil
}

// push delivers a local input event the way a capture hook would. A
// non-grabbed MouseMove also moves the local cursor.
func (p *fakePlat) push(ev plat.Event) {
	p.mu.Lock()
	fn := p.capture
	if ev.Kind == plat.MouseMove && !p.grab {
		p.cx, p.cy = ev.X, ev.Y
	}
	p.mu.Unlock()
	if fn != nil {
		fn(ev)
	}
}

func (p *fakePlat) SetGrab(on bool) {
	p.mu.Lock()
	p.grab = on
	p.mu.Unlock()
}

func (p *fakePlat) grabbed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.grab
}

func (p *fakePlat) Inject(ev plat.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ev.Kind == plat.MouseMove {
		p.cx, p.cy = ev.X, ev.Y
	}
	p.injected = append(p.injected, ev)
	if p.onInject != nil {
		p.onInject(ev)
	}
	return nil
}

func (p *fakePlat) injections() []plat.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]plat.Event(nil), p.injected...)
}

func (p *fakePlat) ClipboardSeq() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seqGets++
	return p.clipSeq
}

func (p *fakePlat) seqPolls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seqGets
}

func (p *fakePlat) ClipboardText() (string, uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clipGets++
	return p.clipText, p.clipSeq, nil
}

func (p *fakePlat) SetClipboardText(text string) (uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clipSets++
	p.clipText, p.clipFiles, p.clipImage = text, nil, nil
	p.clipSeq++
	return p.clipSeq, nil
}

func (p *fakePlat) ClipboardFiles() ([]string, uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clipGets++
	return append([]string(nil), p.clipFiles...), p.clipSeq, nil
}

func (p *fakePlat) ClipboardImage() ([]byte, uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clipGets++
	return slices.Clone(p.clipImage), p.clipSeq, nil
}

func (p *fakePlat) SetClipboardImage(png []byte, text string) (uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clipSets++
	p.clipText, p.clipFiles, p.clipImage = text, nil, slices.Clone(png)
	p.clipSeq++
	return p.clipSeq, nil
}

// copyImage simulates the user copying an image locally.
func (p *fakePlat) copyImage(png []byte) {
	p.mu.Lock()
	p.clipText, p.clipFiles, p.clipImage = "", nil, slices.Clone(png)
	p.clipSeq++
	p.mu.Unlock()
}

func (p *fakePlat) clipboardImage() (png []byte, sets int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.clipImage), p.clipSets
}

func (p *fakePlat) SetClipboardFiles(paths []string) (uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clipSets++
	p.clipText, p.clipFiles, p.clipImage = "", slices.Clone(paths), nil
	p.clipSeq++
	return p.clipSeq, nil
}

// copyFiles simulates the user copying files locally.
func (p *fakePlat) copyFiles(paths ...string) {
	p.mu.Lock()
	p.clipText, p.clipFiles, p.clipImage = "", slices.Clone(paths), nil
	p.clipSeq++
	p.mu.Unlock()
}

func (p *fakePlat) files() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.clipFiles...)
}

// copyText simulates the user copying text locally.
func (p *fakePlat) copyText(text string) {
	p.mu.Lock()
	p.clipText, p.clipFiles, p.clipImage = text, nil, nil
	p.clipSeq++
	p.mu.Unlock()
}

func (p *fakePlat) clipboard() (text string, sets, gets int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clipText, p.clipSets, p.clipGets
}

// addWindow adds or replaces a window; ListWindows keeps insertion order.
func (p *fakePlat) addWindow(info plat.WindowInfo, img *image.RGBA) {
	p.mu.Lock()
	if _, ok := p.wins[info.ID]; !ok {
		p.order = append(p.order, info.ID)
	}
	p.wins[info.ID] = fakeWindow{info: info, img: img}
	p.mu.Unlock()
}

func (p *fakePlat) removeWindow(id uint64) {
	p.mu.Lock()
	delete(p.wins, id)
	p.order = slices.DeleteFunc(p.order, func(o uint64) bool { return o == id })
	p.mu.Unlock()
}

func (p *fakePlat) ListWindows() ([]plat.WindowInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ws := make([]plat.WindowInfo, 0, len(p.order))
	for _, id := range p.order {
		ws = append(ws, p.wins[id].info)
	}
	return ws, nil
}

func (p *fakePlat) CaptureWindow(id uint64) (*image.RGBA, plat.Rect, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.wins[id]
	if !ok {
		return nil, plat.Rect{}, errors.New("fake: no such window")
	}
	img := image.NewRGBA(w.img.Rect)
	copy(img.Pix, w.img.Pix)
	return img, w.info.Bounds, nil
}

func (p *fakePlat) FocusWindow(id uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.wins[id]; !ok {
		return errors.New("fake: no such window")
	}
	p.focused = append(p.focused, id)
	return nil
}

func (p *fakePlat) NewOverlay(title string, rect plat.Rect, input func(plat.Event), closed func()) (plat.Overlay, error) {
	ov := &fakeOverlay{input: input, closed: closed, bounds: rect, title: title}
	p.mu.Lock()
	p.overlays = append(p.overlays, ov)
	p.mu.Unlock()
	return ov, nil
}

func (p *fakePlat) overlayList() []*fakeOverlay {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*fakeOverlay(nil), p.overlays...)
}

// fakeOverlay records what the app showed; tests call input/closed to act
// as the user. closedByApp is set by Close.
type fakeOverlay struct {
	input  func(plat.Event)
	closed func()

	mu          sync.Mutex
	frames      int
	last        *image.RGBA // copy of the latest frame
	bounds      plat.Rect
	title       string
	closedByApp bool
}

func (o *fakeOverlay) SetFrame(img *image.RGBA) {
	c := image.NewRGBA(img.Rect)
	copy(c.Pix, img.Pix)
	o.mu.Lock()
	o.frames++
	o.last = c
	o.mu.Unlock()
}

func (o *fakeOverlay) SetBounds(rect plat.Rect) {
	o.mu.Lock()
	o.bounds = rect
	o.mu.Unlock()
}

func (o *fakeOverlay) SetTitle(t string) {
	o.mu.Lock()
	o.title = t
	o.mu.Unlock()
}

func (o *fakeOverlay) Close() {
	o.mu.Lock()
	o.closedByApp = true
	o.mu.Unlock()
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	eventuallyWithin(t, 5*time.Second, what, cond)
}

func eventuallyWithin(t *testing.T, wait time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
