package app

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"log"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"magic/internal/plat"
	"magic/internal/proto"
)

const (
	shareTile         = 64                    // diff/encode tile edge in pixels
	shareMaxInFlight  = 2                     // unacked frames before the streamer pauses
	shareMaxErrors    = 3                     // consecutive capture failures before giving up
	shareScanInterval = 33 * time.Millisecond // spanning-window scan period
	shareRefocusAfter = 2 * time.Second       // re-focus on click if the last focus is older
	shareMoveInterval = 8 * time.Millisecond
	shareSlowAck      = 150 * time.Millisecond // a frame acked later lowers the JPEG quality
	shareQualityStep  = 10
	shareMinQuality   = 40
)

// Sharing implements Feature: it streams local windows that span into our
// virtual display to the peer (host side, this file) and shows the peer's
// spanning windows as overlays (viewer side, overlay.go). Handle runs on
// the core's receive goroutine.
type Sharing struct {
	cfg *Config
	p   plat.Platform
	s   Session

	mu sync.Mutex
	// host side
	scanStop    chan struct{} // nil while the scan goroutine is not running
	scanDone    chan struct{}
	layout      []proto.SpanWindow                 // last layout sent to the peer
	spans       atomic.Pointer[[]proto.SpanWindow] // layout, for SpanAt on the hook thread
	streams     map[uint64]*streamer
	muted       map[uint64]bool             // closed by the viewer; skipped until they leave V
	failed      map[uint64]proto.SpanWindow // capture gave up; skipped while the entry is unchanged
	ptID        uint64                      // window of the last injected pointer position
	ptX, ptY    int32
	lastFocus   uint64
	lastFocusAt time.Time
	// viewer side
	views     map[uint64]*remoteView
	dismissed map[uint64]bool // closed by the user; ignored until the layout drops them
}

var _ Feature = (*Sharing)(nil)

func NewSharing(cfg *Config, p plat.Platform, s Session) *Sharing {
	return &Sharing{
		cfg:       cfg,
		p:         p,
		s:         s,
		streams:   make(map[uint64]*streamer),
		muted:     make(map[uint64]bool),
		failed:    make(map[uint64]proto.SpanWindow),
		views:     make(map[uint64]*remoteView),
		dismissed: make(map[uint64]bool),
	}
}

// SpanAt implements Feature: the front-most window of the last sent layout
// whose rect (peer coordinates) contains (x, y).
func (sh *Sharing) SpanAt(x, y float64) (uint64, bool) {
	p := sh.spans.Load()
	if p == nil {
		return 0, false
	}
	for _, e := range *p {
		if containsF(e.Rect, x, y) {
			return e.ID, true
		}
	}
	return 0, false
}

// setLayoutLocked records the layout sent to the peer; sh.mu held.
func (sh *Sharing) setLayoutLocked(layout []proto.SpanWindow) {
	sh.layout = layout
	if layout == nil {
		sh.spans.Store(nil)
	} else {
		sh.spans.Store(&layout)
	}
}

// streamer captures one local window and sends diffed frames to the peer.
type streamer struct {
	id       uint64
	stop     chan struct{}
	stopOnce sync.Once
	acked    atomic.Uint64 // highest Frame.N acked by the viewer

	// JPEG quality adapts to the link: down shareQualityStep when a frame
	// took longer than shareSlowAck to be acked, up by half a step per
	// fast ack. Under mu: the receive goroutine (acks) and the stream
	// goroutine use them.
	mu      sync.Mutex
	sentAt  map[uint64]time.Time
	quality int
}

func (st *streamer) halt() { st.stopOnce.Do(func() { close(st.stop) }) }

// sent records when frame n went out.
func (st *streamer) sent(n uint64) {
	st.mu.Lock()
	st.sentAt[n] = time.Now()
	st.mu.Unlock()
}

// ack adapts the quality to how long frames up to n took to be acked.
func (st *streamer) ack(n uint64, maxQuality int) {
	now := time.Now()
	st.mu.Lock()
	defer st.mu.Unlock()
	for fn, at := range st.sentAt {
		if fn > n {
			continue
		}
		delete(st.sentAt, fn)
		if fn != n {
			continue
		}
		if now.Sub(at) > shareSlowAck {
			st.quality = max(shareMinQuality, st.quality-shareQualityStep)
		} else {
			st.quality = min(maxQuality, st.quality+shareQualityStep/2)
		}
	}
}

func (st *streamer) jpegQuality() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.quality
}

func (sh *Sharing) Handle(m *proto.Msg) bool {
	switch m.Type {
	case proto.TLayout:
		sh.handleLayout(m.Layout)
	case proto.TWinClose:
		sh.mute(m.WinID)
	case proto.TFrameAck:
		sh.mu.Lock()
		st := sh.streams[m.WinID]
		sh.mu.Unlock()
		if st != nil {
			for {
				cur := st.acked.Load()
				if m.Seq <= cur || st.acked.CompareAndSwap(cur, m.Seq) {
					break
				}
			}
			st.ack(m.Seq, sh.cfg.JPEGQuality)
		}
	case proto.TWinInput:
		if m.Input != nil {
			sh.handleInput(m.WinID, *m.Input)
		}
	case proto.TFrame:
		if m.Frame != nil {
			sh.handleFrame(m.WinID, m.Frame)
		}
	default:
		return false
	}
	return true
}

// Connected starts scanning for windows spanning into the virtual display.
func (sh *Sharing) Connected() {
	sh.stopScan()
	stop, done := make(chan struct{}), make(chan struct{})
	sh.mu.Lock()
	sh.scanStop, sh.scanDone = stop, done
	sh.mu.Unlock()
	go sh.scan(stop, done)
}

func (sh *Sharing) Disconnected() {
	sh.stopScan()
	sh.mu.Lock()
	streams := sh.streams
	sh.streams = make(map[uint64]*streamer)
	sh.setLayoutLocked(nil)
	clear(sh.muted)
	clear(sh.failed)
	views := sh.views
	sh.views = make(map[uint64]*remoteView)
	clear(sh.dismissed)
	sh.mu.Unlock()
	for _, st := range streams {
		st.halt()
	}
	for _, rv := range views {
		rv.close()
	}
}

// stopScan stops the scan goroutine and waits for it, so no layout of the
// old connection is sent afterwards.
func (sh *Sharing) stopScan() {
	sh.mu.Lock()
	stop, done := sh.scanStop, sh.scanDone
	sh.scanStop, sh.scanDone = nil, nil
	sh.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
}

func (sh *Sharing) scan(stop, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(shareScanInterval)
	defer t.Stop()
	var lastErr string
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		v := sh.s.Virtual()
		var wins []plat.WindowInfo
		if v.Real && v.Rect.W > 0 && v.Rect.H > 0 && v.Of.W > 0 && v.Of.H > 0 {
			var err error
			if wins, err = sh.p.ListWindows(); err != nil {
				if s := err.Error(); s != lastErr {
					lastErr = s
					log.Printf("share: list windows: %v", err)
				}
				continue
			}
			lastErr = ""
		}
		sh.update(v, wins)
	}
}

// update recomputes the spanning windows from wins (front to back), sends
// the layout when it changed and starts/stops streamers to match.
func (sh *Sharing) update(v proto.Virtual, wins []plat.WindowInfo) {
	var layout []proto.SpanWindow
	present := make(map[uint64]bool)
	sh.mu.Lock()
	for _, w := range wins {
		if !intersects(w.Bounds, v.Rect) || present[w.ID] {
			continue
		}
		present[w.ID] = true
		if sh.muted[w.ID] {
			continue
		}
		e := proto.SpanWindow{ID: w.ID, App: w.App, Title: w.Title, Rect: mapRect(w.Bounds, v.Rect, v.Of)}
		if f, ok := sh.failed[w.ID]; ok {
			if f == e {
				continue
			}
			delete(sh.failed, w.ID)
		}
		layout = append(layout, e)
	}
	for id := range sh.muted {
		if !present[id] {
			delete(sh.muted, id)
		}
	}
	for id := range sh.failed {
		if !present[id] {
			delete(sh.failed, id)
		}
	}
	var stop []*streamer
	for id, st := range sh.streams {
		if !slices.ContainsFunc(layout, func(e proto.SpanWindow) bool { return e.ID == id }) {
			delete(sh.streams, id)
			stop = append(stop, st)
		}
	}
	var start []*streamer
	for _, e := range layout {
		if sh.streams[e.ID] == nil {
			st := &streamer{id: e.ID, stop: make(chan struct{}), sentAt: make(map[uint64]time.Time),
				quality: sh.cfg.JPEGQuality}
			sh.streams[e.ID] = st
			start = append(start, st)
		}
	}
	changed := !slices.Equal(layout, sh.layout)
	if changed {
		sh.setLayoutLocked(layout)
	}
	sh.mu.Unlock()

	for _, st := range stop {
		st.halt()
	}
	// The layout goes first: the viewer drops frames of unknown windows.
	if changed {
		sh.send(&proto.Msg{Type: proto.TLayout, Layout: layout})
	}
	for _, st := range start {
		go sh.stream(st)
	}
}

// mute handles the viewer's TWinClose: the window leaves the layout until
// it stops intersecting the virtual display.
func (sh *Sharing) mute(id uint64) {
	sh.mu.Lock()
	sh.muted[id] = true
	st := sh.streams[id]
	delete(sh.streams, id)
	sh.mu.Unlock()
	if st != nil {
		st.halt()
	}
}

func (sh *Sharing) stream(st *streamer) {
	tick := time.NewTicker(time.Second / time.Duration(sh.cfg.FPS))
	defer tick.Stop()
	var (
		prev *image.RGBA
		n    uint64
		errs int
	)
	for {
		select {
		case <-st.stop:
			return
		case <-tick.C:
		}
		if n-min(st.acked.Load(), n) >= shareMaxInFlight {
			continue
		}
		img, _, err := sh.p.CaptureWindow(st.id)
		if err != nil {
			if errs++; errs >= shareMaxErrors {
				log.Printf("share: capture window %#x: %v; dropping it", st.id, err)
				sh.captureFailed(st)
				return
			}
			continue
		}
		errs = 0
		full := prev == nil || prev.Rect != img.Rect
		tiles := sh.encodeTiles(img, prev, full, st.jpegQuality())
		prev = img
		if !full && len(tiles) == 0 {
			continue
		}
		select {
		case <-st.stop:
			return
		default:
		}
		n++
		sz := img.Rect.Size()
		st.sent(n)
		sh.send(&proto.Msg{Type: proto.TFrame, WinID: st.id, Frame: &proto.Frame{
			N: n, W: int32(sz.X), H: int32(sz.Y), Full: full, Tiles: tiles,
		}})
	}
}

// captureFailed drops st's window from the layout until its entry changes,
// unless the stream was already stopped.
func (sh *Sharing) captureFailed(st *streamer) {
	sh.mu.Lock()
	if sh.streams[st.id] == st {
		delete(sh.streams, st.id)
		for _, e := range sh.layout {
			if e.ID == st.id {
				sh.failed[st.id] = e
			}
		}
	}
	sh.mu.Unlock()
	st.halt()
}

// encodeTiles JPEG-encodes every tile of img (full) or only those that
// differ from prev. Tile coordinates are relative to img.Rect.Min.
func (sh *Sharing) encodeTiles(img, prev *image.RGBA, full bool, quality int) []proto.Tile {
	var tiles []proto.Tile
	var buf bytes.Buffer
	opt := &jpeg.Options{Quality: quality}
	r := img.Rect
	for y := r.Min.Y; y < r.Max.Y; y += shareTile {
		for x := r.Min.X; x < r.Max.X; x += shareTile {
			t := image.Rect(x, y, x+shareTile, y+shareTile).Intersect(r)
			if !full && tileEqual(img, prev, t) {
				continue
			}
			buf.Reset()
			if err := jpeg.Encode(&buf, img.SubImage(t), opt); err != nil {
				log.Printf("share: jpeg: %v", err)
				continue
			}
			tiles = append(tiles, proto.Tile{
				X:    int32(x - r.Min.X),
				Y:    int32(y - r.Min.Y),
				JPEG: bytes.Clone(buf.Bytes()),
			})
		}
	}
	return tiles
}

// tileEqual compares rectangle t (inside both images' identical Rect).
func tileEqual(a, b *image.RGBA, t image.Rectangle) bool {
	w := t.Dx() * 4
	for y := t.Min.Y; y < t.Max.Y; y++ {
		ia, ib := a.PixOffset(t.Min.X, y), b.PixOffset(t.Min.X, y)
		if !bytes.Equal(a.Pix[ia:ia+w], b.Pix[ib:ib+w]) {
			return false
		}
	}
	return true
}

// handleInput injects viewer input into spanning window id. Pointer
// positions arrive in the peer's coordinates over the monitor our virtual
// display stands for; button and wheel events land at the last position.
func (sh *Sharing) handleInput(id uint64, ev plat.Event) {
	v := sh.s.Virtual()
	sh.mu.Lock()
	spanning := sh.streams[id] != nil
	var at *plat.Event
	if spanning && ev.Kind == plat.MouseMove && v.Of.W > 0 && v.Of.H > 0 {
		x, y := mapPoint(float64(ev.X), float64(ev.Y), v.Of, v.Rect)
		sh.ptID, sh.ptX, sh.ptY = id, int32(math.Floor(x)), int32(math.Floor(y))
	}
	if spanning && sh.ptID == id && (ev.Kind == plat.MouseMove || ev.Kind == plat.MouseButton || ev.Kind == plat.Wheel) {
		at = &plat.Event{Kind: plat.MouseMove, X: sh.ptX, Y: sh.ptY}
	}
	sh.mu.Unlock()
	if !spanning {
		return // stale input for a window that left the virtual display
	}
	switch ev.Kind {
	case plat.MouseMove:
		if at != nil {
			sh.inject(*at)
		}
	case plat.MouseButton, plat.Wheel:
		if at != nil {
			ev.X, ev.Y = at.X, at.Y
			sh.inject(*at)
		}
		if ev.Kind == plat.MouseButton && ev.Down {
			sh.focus(id, true)
		}
		sh.inject(ev)
	case plat.Key:
		sh.focus(id, false)
		sh.inject(ev)
	}
}

// focus raises window id when another window was focused last, or (stale)
// when the last focus is older than shareRefocusAfter.
func (sh *Sharing) focus(id uint64, stale bool) {
	sh.mu.Lock()
	need := sh.lastFocus != id || (stale && time.Since(sh.lastFocusAt) > shareRefocusAfter)
	if need {
		sh.lastFocus, sh.lastFocusAt = id, time.Now()
	}
	sh.mu.Unlock()
	if need {
		if err := sh.p.FocusWindow(id); err != nil {
			log.Printf("share: focus window %#x: %v", id, err)
		}
	}
}

func (sh *Sharing) inject(ev plat.Event) {
	// While we control the peer our cursor is not ours to move; the peer
	// stops sending input once the pointer is back here.
	if err := sh.s.InjectForPeer(ev); err != nil && !errors.Is(err, errControlling) {
		log.Printf("share: inject: %v", err)
	}
}

func (sh *Sharing) send(m *proto.Msg) {
	if err := sh.s.Send(m); err != nil {
		log.Printf("share: send: %v", err)
	}
}
