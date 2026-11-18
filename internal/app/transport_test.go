package app

import (
	"image"
	"testing"
	"time"

	"magic/internal/plat"
	"magic/internal/proto"
)

// Layouts, frames and file data share the ordered bulk stream.
func TestTransportRouting(t *testing.T) {
	a := newNode(t, testCfg("alpha", "right", testSecret, false), newFakePlat("windows", winMon.Rect))
	ctl, bulk, msgs := rawPeerBulk(t, a)
	eventually(t, "connection", func() bool { return a.c.Peer() != nil })
	l := a.c.link()

	ctlIn, bulkIn := l.ctl.nc.in.Load(), l.bulk.nc.in.Load()
	if err := bulk.Send(&proto.Msg{Type: proto.TFrame, WinID: 9, Frame: &proto.Frame{N: 1}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "frame over bulk", func() bool { return l.bulk.nc.in.Load() > bulkIn })
	// A layout must precede its first frame on the same stream. Control can
	// be delayed independently, e.g. while replacing a simultaneous dial.
	layout := &proto.Msg{Type: proto.TLayout, Layout: []proto.SpanWindow{{ID: 9}}}
	bulkBefore := l.bulk.nc.out.Load()
	if err := a.c.Send(layout); err != nil {
		t.Fatal(err)
	}
	if m := <-msgs; m.Type != proto.TLayout {
		t.Fatalf("got %+v, want the layout", m)
	}
	if l.bulk.nc.out.Load() == bulkBefore {
		t.Fatal("layout not sent over the bulk session")
	}

	frame := &proto.Msg{Type: proto.TFrame, WinID: 9, Frame: &proto.Frame{N: 1, W: 1, H: 1, Full: true}}
	ctlOut, bulkOut := l.ctl.nc.out.Load(), l.bulk.nc.out.Load()
	if err := a.c.Send(frame); err != nil {
		t.Fatal(err)
	}
	if m := <-msgs; m.Type != proto.TFrame {
		t.Fatalf("got %+v, want the frame", m)
	}
	if l.bulk.nc.out.Load() == bulkOut || l.ctl.nc.out.Load() != ctlOut {
		t.Fatal("frame not sent over the bulk session")
	}

	bulkOut = l.bulk.nc.out.Load()
	if err := a.c.Send(&proto.Msg{Type: proto.TClipboard, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if m := <-msgs; m.Type != proto.TClipboard {
		t.Fatalf("got %+v, want the clipboard", m)
	}
	if l.bulk.nc.out.Load() != bulkOut {
		t.Fatal("control message sent over the bulk session")
	}
	if err := ctl.Send(&proto.Msg{Type: proto.TPing}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "ping over control", func() bool { return l.ctl.nc.in.Load() > ctlIn })
}

func TestTransportWaitsForPeerToPairBulk(t *testing.T) {
	a := newNode(t, testCfg("alpha", "right", testSecret, false), newFakePlat("windows", winMon.Rect))
	ctl, _ := openRawPeer(t, a)
	// The listening peer has read our token but has not confirmed the link.
	// If it shuts down now, there must be no ghost peer/display to remove.
	time.Sleep(200 * time.Millisecond)
	if p := a.c.Peer(); p != nil {
		t.Fatalf("published peer %q before it accepted the paired link", p.Name)
	}
	if err := ctl.Send(&proto.Msg{Type: proto.TReady}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "confirmed link", func() bool { return a.c.Peer() != nil })
}

// Losing the bulk session ends the link like losing the control session.
func TestTransportBulkLossClosesLink(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)
	l := a.c.link()
	l.bulk.close()
	eventually(t, "link closed", func() bool {
		select {
		case <-l.finished:
			return true
		default:
			return false
		}
	})
	connected(t, a, b) // and the peers reconnect
}

// A bulk session must carry the token of a link's control session.
func TestTransportTokenMismatch(t *testing.T) {
	old := bulkPairTimeout
	bulkPairTimeout = 300 * time.Millisecond
	t.Cleanup(func() { bulkPairTimeout = old })
	a := newNode(t, testCfg("alpha", "right", testSecret, false), newFakePlat("windows", winMon.Rect))
	a.c.cfg.Peer = "127.0.0.1:9" // a never dials anything useful
	a.start(t)

	ctl, _ := dialRaw(t, a, "bravo", newToken())
	bulk := dialRawSession(t, a)
	if err := bulk.Send(&proto.Msg{Type: proto.THello, Hello: &proto.Hello{Token: newToken()}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * bulkPairTimeout)
	if a.c.link() != nil || a.c.Peer() != nil {
		t.Fatal("link up with a bulk session of another token")
	}
	// Both sessions were dropped.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, pc := range []*proto.Conn{ctl, bulk} {
			for {
				if _, err := pc.Recv(); err != nil {
					break
				}
			}
		}
	}()
	ctl.Close()
	bulk.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sessions not closed")
	}
}

// Input latency is unaffected by saturated bulk traffic: pings over the
// control session stay fast while the bulk session carries frames as fast
// as it can (at least 5 MB during the measurement).
func TestTransportInputUnderBulkLoad(t *testing.T) {
	old := pingInterval
	pingInterval = 20 * time.Millisecond
	t.Cleanup(func() { pingInterval = old })
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)
	l := a.c.link()

	// Frames for an unknown window: B decodes nothing, just reads.
	tile := proto.Tile{JPEG: make([]byte, 64<<10)}
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-stop:
				return
			default:
			}
			f := &proto.Frame{N: 1, W: 1, H: 1, Tiles: []proto.Tile{tile, tile, tile, tile}}
			if a.c.Send(&proto.Msg{Type: proto.TFrame, WinID: 999, Frame: f}) != nil {
				return
			}
		}
	}()
	defer func() { close(stop); <-stopped }()
	time.Sleep(100 * time.Millisecond) // bulk queue and KCP window full

	start := l.bulk.nc.out.Load()
	a.c.meter.mu.Lock()
	a.c.meter.rtts = a.c.meter.rtts[:0]
	a.c.meter.mu.Unlock()
	eventually(t, "5 MB of frames and pings meanwhile", func() bool {
		a.c.meter.mu.Lock()
		defer a.c.meter.mu.Unlock()
		return len(a.c.meter.rtts) >= 5 && l.bulk.nc.out.Load()-start >= 5<<20
	})
	if st := a.c.LinkStats(); st.AppRTTMaxms >= 50 {
		t.Fatalf("control RTT %.1fms (max %.1fms) under bulk load", st.AppRTTms, st.AppRTTMaxms)
	}
	if a.c.link() != l {
		t.Fatal("link dropped under load")
	}
}

// JPEG quality drops while frames are acked slowly and recovers on fast
// acks.
func TestShareQualityAdapts(t *testing.T) {
	host, hp, hs := newShareHost(t)
	hp.addWindow(plat.WindowInfo{ID: 7, App: "a", Title: "x", Bounds: plat.Rect{X: 1100, Y: 100, W: 64, H: 64}},
		image.NewRGBA(image.Rect(0, 0, 64, 64)))
	host.Connected()
	nextMsg(t, hs.sent, proto.TLayout)
	nextMsg(t, hs.sent, proto.TFrame)
	host.mu.Lock()
	st := host.streams[7]
	host.mu.Unlock()

	st.sent(100)
	st.mu.Lock()
	st.sentAt[100] = time.Now().Add(-time.Second)
	st.mu.Unlock()
	st.ack(100, 90)
	if q := st.jpegQuality(); q != 90-shareQualityStep {
		t.Fatalf("quality after a slow ack %d", q)
	}
	for range 10 {
		st.sent(101)
		st.mu.Lock()
		st.sentAt[101] = time.Now().Add(-time.Second)
		st.mu.Unlock()
		st.ack(101, 90)
	}
	if q := st.jpegQuality(); q != shareMinQuality {
		t.Fatalf("quality floor %d", q)
	}
	for range 20 {
		st.sent(102)
		st.ack(102, 90)
	}
	if q := st.jpegQuality(); q != 90 {
		t.Fatalf("quality after fast acks %d, want back at 90", q)
	}
}
