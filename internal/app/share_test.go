package app

import (
	"image"
	"image/color"
	"image/draw"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"magic/internal/plat"
	"magic/internal/proto"
)

// Host geometry used throughout: the host's virtual display spans
// {1000,0,500,400} in host coordinates and mirrors the viewer monitor
// {0,0,1000,800}, so viewer coordinates are twice the host offsets.
var (
	testVRect = plat.Rect{X: 1000, Y: 0, W: 500, H: 400}
	testVOf   = plat.Rect{X: 0, Y: 0, W: 1000, H: 800}
)

// shareSession is one end of an in-memory Session pair. Send queues the
// message for the other end, whose Handle runs on a delivery goroutine
// (like the core's receive goroutine). sent records every message sent.
type shareSession struct {
	name string
	q    chan *proto.Msg
	sent chan *proto.Msg
	peer *shareSession
	h    func(*proto.Msg) bool

	mu       sync.Mutex
	v        proto.Virtual
	p        plat.Platform // InjectForPeer target, set by NewSharing callers
	shadowed bool
}

func (s *shareSession) Send(m *proto.Msg) error {
	select {
	case s.sent <- m:
	default:
	}
	if s.peer != nil {
		s.peer.q <- m
	}
	return nil
}

func (s *shareSession) Peer() *proto.Hello {
	if s.peer == nil {
		return nil
	}
	return &proto.Hello{Name: s.peer.name, OS: "darwin"}
}

func (s *shareSession) MapKey(hid uint16) uint16 { return hid + 1 }

func (s *shareSession) Virtual() proto.Virtual {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.v
}

func (s *shareSession) InjectForPeer(ev plat.Event) error { return s.p.Inject(ev) }
func (s *shareSession) LinkStats() LinkStats              { return LinkStats{} }

func (s *shareSession) Shadowed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shadowed
}

func (s *shareSession) setVirtual(v proto.Virtual) {
	s.mu.Lock()
	s.v = v
	s.mu.Unlock()
}

func (s *shareSession) run(done chan struct{}) {
	for {
		select {
		case m := <-s.q:
			s.h(m)
		case <-done:
			return
		}
	}
}

func newShareSession(name string) *shareSession {
	return &shareSession{name: name, q: make(chan *proto.Msg, 256), sent: make(chan *proto.Msg, 1024)}
}

func shareCfg() *Config {
	return &Config{Name: "t", FPS: 100, JPEGQuality: 90, Panel: "127.0.0.1:0"}
}

// testWindowImg is a 128x128 image of four distinct 64x64 tiles.
func testWindowImg() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 128, 128))
	colors := []color.RGBA{{200, 30, 30, 255}, {30, 200, 30, 255}, {30, 30, 200, 255}, {220, 220, 40, 255}}
	for i, c := range colors {
		r := image.Rect(i%2*64, i/2*64, i%2*64+64, i/2*64+64)
		draw.Draw(img, r, &image.Uniform{c}, image.Point{}, draw.Src)
	}
	return img
}

// paintTile paints tile (tx,ty) of window id's source image.
func paintTile(p *fakePlat, id uint64, tx, ty int, c color.RGBA) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := image.Rect(tx*64, ty*64, tx*64+64, ty*64+64)
	draw.Draw(p.wins[id].img, r, &image.Uniform{c}, image.Point{}, draw.Src)
}

func sourceImg(p *fakePlat, id uint64) *image.RGBA {
	p.mu.Lock()
	defer p.mu.Unlock()
	src := p.wins[id].img
	img := image.NewRGBA(src.Rect)
	copy(img.Pix, src.Pix)
	return img
}

// newShareHost is a host without a peer, its virtual display real.
func newShareHost(t *testing.T) (*Sharing, *fakePlat, *shareSession) {
	t.Helper()
	hp := newFakePlat("windows", plat.Rect{W: 1000, H: 800})
	hs := newShareSession("host")
	hs.setVirtual(proto.Virtual{Rect: testVRect, Of: testVOf, Real: true})
	hs.p = hp
	host := NewSharing(shareCfg(), hp, hs)
	t.Cleanup(host.Disconnected)
	return host, hp, hs
}

// sharePair wires a connected host (owning window 7 of app "App.exe",
// 128x128, bounds {1100,100,200,100}: inside its virtual display) and a
// viewer together.
func sharePair(t *testing.T) (host, viewer *Sharing, hp, vp *fakePlat, hs, vs *shareSession) {
	t.Helper()
	hp = newFakePlat("windows", plat.Rect{W: 1000, H: 800})
	vp = newFakePlat("darwin", plat.Rect{W: 1000, H: 800})
	hp.addWindow(plat.WindowInfo{ID: 7, App: "App.exe", Title: "doc", Bounds: plat.Rect{X: 1100, Y: 100, W: 200, H: 100}}, testWindowImg())
	hs, vs = newShareSession("host"), newShareSession("viewer")
	hs.peer, vs.peer = vs, hs
	hs.setVirtual(proto.Virtual{Rect: testVRect, Of: testVOf, Real: true})
	vs.setVirtual(proto.Virtual{Rect: plat.Rect{X: -500, W: 500, H: 400}, Of: plat.Rect{W: 1000, H: 800}, Real: true})
	hs.p, vs.p = hp, vp
	host = NewSharing(shareCfg(), hp, hs)
	viewer = NewSharing(shareCfg(), vp, vs)
	hs.h, vs.h = host.Handle, viewer.Handle
	done := make(chan struct{})
	go hs.run(done)
	go vs.run(done)
	host.Connected()
	viewer.Connected()
	t.Cleanup(func() {
		host.Disconnected()
		viewer.Disconnected()
		close(done)
	})
	return
}

// nextMsg returns the next sent message of type typ, skipping others.
func nextMsg(t *testing.T, ch chan *proto.Msg, typ proto.Type) *proto.Msg {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m := <-ch:
			if m.Type == typ {
				return m
			}
		case <-deadline:
			t.Fatalf("timed out waiting for message type %d", typ)
			return nil
		}
	}
}

// noMsg fails if a message of type typ is sent within d.
func noMsg(t *testing.T, ch chan *proto.Msg, typ proto.Type, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case m := <-ch:
			if m.Type == typ {
				t.Fatalf("unexpected message %+v", m)
			}
		case <-deadline:
			return
		}
	}
}

func shareImgClose(t *testing.T, got, want *image.RGBA) {
	t.Helper()
	if got == nil {
		t.Fatal("no frame shown")
	}
	if got.Rect != want.Rect {
		t.Fatalf("size %v, want %v", got.Rect, want.Rect)
	}
	for i := range want.Pix {
		d := int(got.Pix[i]) - int(want.Pix[i])
		if d < -12 || d > 12 {
			t.Fatalf("pixel byte %d = %d, want %d±12", i, got.Pix[i], want.Pix[i])
		}
	}
}

// overlay waits for the n-th (0-based) overlay the viewer created.
func overlay(t *testing.T, vp *fakePlat, n int) *fakeOverlay {
	t.Helper()
	var ov *fakeOverlay
	eventually(t, "overlay", func() bool {
		if l := vp.overlayList(); len(l) > n {
			ov = l[n]
		}
		return ov != nil
	})
	return ov
}

func overlayState(ov *fakeOverlay) (frames int, last *image.RGBA, bounds plat.Rect, title string, closed bool) {
	ov.mu.Lock()
	defer ov.mu.Unlock()
	return ov.frames, ov.last, ov.bounds, ov.title, ov.closedByApp
}

func TestShareLayout(t *testing.T) {
	host, hp, hs := newShareHost(t)
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	hp.addWindow(plat.WindowInfo{ID: 1, App: "a", Title: "inside", Bounds: plat.Rect{X: 1100, Y: 100, W: 200, H: 100}}, img)
	hp.addWindow(plat.WindowInfo{ID: 2, App: "b", Title: "straddle", Bounds: plat.Rect{X: 900, Y: 50, W: 200, H: 100}}, img)
	hp.addWindow(plat.WindowInfo{ID: 3, App: "c", Title: "outside", Bounds: plat.Rect{X: 0, Y: 0, W: 100, H: 100}}, img)
	host.Connected()

	got := nextMsg(t, hs.sent, proto.TLayout).Layout
	want := []proto.SpanWindow{
		{ID: 1, App: "a", Title: "inside", Rect: plat.Rect{X: 200, Y: 200, W: 400, H: 200}},
		{ID: 2, App: "b", Title: "straddle", Rect: plat.Rect{X: -200, Y: 100, W: 400, H: 200}},
	}
	if len(got) != len(want) {
		t.Fatalf("layout %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("layout[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	noMsg(t, hs.sent, proto.TLayout, 150*time.Millisecond) // unchanged: not resent

	hp.removeWindow(1)
	hp.addWindow(plat.WindowInfo{ID: 2, App: "b", Title: "straddle", Bounds: plat.Rect{X: 700, Y: 50, W: 200, H: 100}}, img)
	for len(nextMsg(t, hs.sent, proto.TLayout).Layout) != 0 {
	}
	noMsg(t, hs.sent, proto.TLayout, 150*time.Millisecond) // empty layout sent once

	// A phantom virtual display spans nothing.
	hp.addWindow(plat.WindowInfo{ID: 2, App: "b", Title: "straddle", Bounds: plat.Rect{X: 900, Y: 50, W: 200, H: 100}}, img)
	if l := nextMsg(t, hs.sent, proto.TLayout).Layout; len(l) != 1 || l[0].ID != 2 {
		t.Fatalf("layout %+v, want window 2", l)
	}
	hs.setVirtual(proto.Virtual{Rect: testVRect, Of: testVOf})
	if l := nextMsg(t, hs.sent, proto.TLayout).Layout; len(l) != 0 {
		t.Fatalf("layout %+v with a phantom display, want empty", l)
	}
	noMsg(t, hs.sent, proto.TLayout, 100*time.Millisecond)
}

func TestShareFramesAndDiff(t *testing.T) {
	_, _, hp, vp, hs, _ := sharePair(t)
	f := nextMsg(t, hs.sent, proto.TFrame).Frame
	if !f.Full || f.N != 1 || f.W != 128 || f.H != 128 || len(f.Tiles) != 4 {
		t.Fatalf("first frame: full=%v n=%d %dx%d tiles=%d", f.Full, f.N, f.W, f.H, len(f.Tiles))
	}
	ov := overlay(t, vp, 0)
	eventually(t, "first frame shown", func() bool { n, _, _, _, _ := overlayState(ov); return n >= 1 })
	_, last, bounds, title, _ := overlayState(ov)
	if bounds != (plat.Rect{X: 200, Y: 200, W: 400, H: 200}) || title != "App.exe — doc" {
		t.Fatalf("overlay at %+v titled %q", bounds, title)
	}
	shareImgClose(t, last, sourceImg(hp, 7))

	paintTile(hp, 7, 1, 1, color.RGBA{10, 240, 240, 255})
	f = nextMsg(t, hs.sent, proto.TFrame).Frame
	if f.Full || len(f.Tiles) != 1 || f.Tiles[0].X != 64 || f.Tiles[0].Y != 64 {
		t.Fatalf("delta frame: full=%v n=%d tiles=%+v", f.Full, f.N, f.Tiles)
	}
	eventually(t, "delta shown", func() bool { n, _, _, _, _ := overlayState(ov); return n >= 2 })
	_, last, _, _, _ = overlayState(ov)
	shareImgClose(t, last, sourceImg(hp, 7))
}

func TestShareAckWindow(t *testing.T) {
	host, hp, hs := newShareHost(t) // no peer: nothing is ever acked
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	hp.addWindow(plat.WindowInfo{ID: 7, App: "a", Title: "x", Bounds: plat.Rect{X: 1100, Y: 100, W: 64, H: 64}}, img)
	host.Connected()
	nextMsg(t, hs.sent, proto.TLayout)
	for i := range 20 {
		paintTile(hp, 7, 0, 0, color.RGBA{byte(i), 0, 0, 255})
		time.Sleep(10 * time.Millisecond)
	}
	frames := 0
	for len(hs.sent) > 0 {
		if m := <-hs.sent; m.Type == proto.TFrame {
			frames++
		}
	}
	if frames != 2 {
		t.Fatalf("%d frames in flight without acks, want 2", frames)
	}
	paintTile(hp, 7, 0, 0, color.RGBA{0, 99, 0, 255})
	host.Handle(&proto.Msg{Type: proto.TFrameAck, WinID: 7, Seq: 2})
	if f := nextMsg(t, hs.sent, proto.TFrame).Frame; f.N != 3 {
		t.Fatalf("frame after ack N=%d, want 3", f.N)
	}
}

func TestShareViewerFollowsLayout(t *testing.T) {
	_, viewer, hp, vp, hs, vs := sharePair(t)
	img := sourceImg(hp, 7)
	ov := overlay(t, vp, 0)

	// The window moves within the virtual display: the overlay follows.
	hp.addWindow(plat.WindowInfo{ID: 7, App: "App.exe", Title: "doc2", Bounds: plat.Rect{X: 1050, Y: 100, W: 200, H: 100}}, img)
	eventually(t, "overlay moved", func() bool {
		_, _, b, title, _ := overlayState(ov)
		return b == plat.Rect{X: 100, Y: 200, W: 400, H: 200} && title == "App.exe — doc2"
	})

	for len(hs.sent) > 0 {
		<-hs.sent
	}
	// The user closes the overlay: the host mutes the window.
	ov.closed()
	if m := nextMsg(t, vs.sent, proto.TWinClose); m.WinID != 7 {
		t.Fatalf("TWinClose for %d", m.WinID)
	}
	if l := nextMsg(t, hs.sent, proto.TLayout).Layout; len(l) != 0 {
		t.Fatalf("muted window still in layout: %+v", l)
	}
	noMsg(t, hs.sent, proto.TLayout, 150*time.Millisecond)
	eventually(t, "view forgotten", func() bool {
		viewer.mu.Lock()
		defer viewer.mu.Unlock()
		return len(viewer.views) == 0
	})

	// It leaves the virtual display and comes back: shown again.
	hp.addWindow(plat.WindowInfo{ID: 7, App: "App.exe", Title: "doc2", Bounds: plat.Rect{X: 100, Y: 100, W: 200, H: 100}}, img)
	time.Sleep(100 * time.Millisecond)
	hp.addWindow(plat.WindowInfo{ID: 7, App: "App.exe", Title: "doc2", Bounds: plat.Rect{X: 1050, Y: 100, W: 200, H: 100}}, img)
	if l := nextMsg(t, hs.sent, proto.TLayout).Layout; len(l) != 1 || l[0].ID != 7 {
		t.Fatalf("layout after return %+v", l)
	}
	ov2 := overlay(t, vp, 1)
	if _, _, _, _, closed := overlayState(ov); closed {
		t.Fatal("user-closed overlay closed again by the app")
	}

	// It leaves for good: the overlay is closed.
	hp.removeWindow(7)
	eventually(t, "overlay closed", func() bool { _, _, _, _, c := overlayState(ov2); return c })
}

func TestShareHostInput(t *testing.T) {
	host, hp, hs := newShareHost(t)
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	hp.addWindow(plat.WindowInfo{ID: 7, App: "a", Title: "x", Bounds: plat.Rect{X: 1100, Y: 100, W: 200, H: 100}}, img)
	hp.addWindow(plat.WindowInfo{ID: 8, App: "a", Title: "y", Bounds: plat.Rect{X: 0, Y: 0, W: 100, H: 100}}, img)
	host.Connected()
	nextMsg(t, hs.sent, proto.TLayout)

	// Window 8 is not spanning: rejected.
	host.Handle(&proto.Msg{Type: proto.TWinInput, WinID: 8, Input: &plat.Event{Kind: plat.MouseMove, X: 10, Y: 10}})
	host.Handle(&proto.Msg{Type: proto.TWinInput, WinID: 8, Input: &plat.Event{Kind: plat.MouseButton, Button: plat.ButtonLeft, Down: true}})
	if ev := hp.injections(); len(ev) != 0 {
		t.Fatalf("input to non-spanning window injected: %+v", ev)
	}

	host.Handle(&proto.Msg{Type: proto.TWinInput, WinID: 7, Input: &plat.Event{Kind: plat.MouseMove, X: 300, Y: 301}})
	host.Handle(&proto.Msg{Type: proto.TWinInput, WinID: 7, Input: &plat.Event{Kind: plat.MouseButton, Button: plat.ButtonLeft, Down: true}})
	host.Handle(&proto.Msg{Type: proto.TWinInput, WinID: 7, Input: &plat.Event{Kind: plat.Key, HID: 4, Down: true}})
	want := []plat.Event{
		{Kind: plat.MouseMove, X: 1150, Y: 150},
		{Kind: plat.MouseMove, X: 1150, Y: 150},
		{Kind: plat.MouseButton, X: 1150, Y: 150, Button: plat.ButtonLeft, Down: true},
		{Kind: plat.Key, HID: 4, Down: true},
	}
	ev := hp.injections()
	if len(ev) != len(want) {
		t.Fatalf("injected %+v, want %+v", ev, want)
	}
	for i := range want {
		if ev[i] != want[i] {
			t.Fatalf("injected[%d] = %+v, want %+v", i, ev[i], want[i])
		}
	}
	hp.mu.Lock()
	foc := append([]uint64(nil), hp.focused...)
	hp.mu.Unlock()
	if len(foc) != 1 || foc[0] != 7 {
		t.Fatalf("focused %v, want [7] (once)", foc)
	}

	// After the viewer closes it, input is rejected.
	host.Handle(&proto.Msg{Type: proto.TWinClose, WinID: 7})
	host.Handle(&proto.Msg{Type: proto.TWinInput, WinID: 7, Input: &plat.Event{Kind: plat.Key, HID: 5, Down: true}})
	if ev := hp.injections(); len(ev) != len(want) {
		t.Fatalf("input after close injected: %+v", ev[len(want):])
	}
}

func TestShareOverlayInput(t *testing.T) {
	_, _, hp, vp, _, vs := sharePair(t)
	ov := overlay(t, vp, 0)
	for len(vs.sent) > 0 {
		<-vs.sent
	}
	// A burst of moves coalesces; the button flushes the latest first.
	for x := int32(240); x <= 300; x += 4 {
		ov.input(plat.Event{Kind: plat.MouseMove, X: x, Y: 300})
	}
	ov.input(plat.Event{Kind: plat.MouseButton, Button: plat.ButtonLeft, Down: true})
	ov.input(plat.Event{Kind: plat.Key, HID: 4, Down: true})
	var got []*plat.Event
	for len(got) == 0 || got[len(got)-1].Kind != plat.Key {
		got = append(got, nextMsg(t, vs.sent, proto.TWinInput).Input)
	}
	if len(got) > 4 {
		t.Fatalf("%d messages for a move burst + click + key; moves not coalesced", len(got))
	}
	if prev := got[len(got)-3]; prev.Kind != plat.MouseMove || prev.X != 300 || prev.Y != 300 {
		t.Fatalf("move before click = %+v, want (300,300)", prev)
	}
	if got[len(got)-1].HID != 5 {
		t.Fatalf("key HID %d, want MapKey(4)=5", got[len(got)-1].HID)
	}

	eventually(t, "host injection", func() bool {
		ev := hp.injections()
		return len(ev) > 0 && ev[len(ev)-1].Kind == plat.Key
	})
	ev := hp.injections()
	n := len(ev)
	if ev[n-3] != (plat.Event{Kind: plat.MouseMove, X: 1150, Y: 150}) ||
		ev[n-2] != (plat.Event{Kind: plat.MouseButton, X: 1150, Y: 150, Button: plat.ButtonLeft, Down: true}) ||
		ev[n-1] != (plat.Event{Kind: plat.Key, HID: 5, Down: true}) {
		t.Fatalf("host injected %+v", ev)
	}
}

func TestShareDisconnectClosesOverlays(t *testing.T) {
	_, viewer, _, vp, _, _ := sharePair(t)
	ov := overlay(t, vp, 0)
	viewer.Disconnected()
	if _, _, _, _, closed := overlayState(ov); !closed {
		t.Fatal("overlay not closed on disconnect")
	}
}

func TestPanelStatus(t *testing.T) {
	sh := NewSharing(shareCfg(), newFakePlat("windows", plat.Rect{W: 100, H: 100}), newShareSession("x"))
	srv := httptest.NewServer(sh.panelHandler())
	defer srv.Close()
	sh.cfg.Panel = strings.TrimPrefix(srv.URL, "http://")

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "未连接") {
		t.Fatalf("index: %d %q", resp.StatusCode, b)
	}

	for _, path := range []string{"/", "/open", "/close"} {
		resp, err := http.Post(srv.URL+path, "application/x-www-form-urlencoded", strings.NewReader("id=7"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotFound {
			t.Fatalf("POST %s: %d, want 405/404", path, resp.StatusCode)
		}
	}

	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "evil.example"
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Host: %d, want 403", resp.StatusCode)
	}
}
