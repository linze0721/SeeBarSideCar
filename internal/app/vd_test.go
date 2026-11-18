package app

import (
	"testing"
	"time"

	"github.com/xtaci/kcp-go/v5"

	"magic/internal/plat"
	"magic/internal/proto"
)

// vdApplied waits up to 2 s for n's platform to hold virtual display want.
func vdApplied(t *testing.T, what string, n *node, want plat.VirtualDisplay) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if vd, _ := n.p.virtual(); vd != nil && *vd == want {
			return
		}
		if time.Now().After(deadline) {
			vd, calls := n.p.virtual()
			t.Fatalf("%s: virtual display %+v after %d calls, want %+v", what, vd, calls, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Both machines create their virtual display right after connecting, and
// again after a reconnect.
func TestVirtualDisplayOnConnect(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	wantA := plat.VirtualDisplay{Rect: aVirtual, UnitsPerLogical: 1.5}
	wantB := plat.VirtualDisplay{Rect: bVirtual, UnitsPerLogical: 1}
	vdApplied(t, "A after connect", a, wantA)
	vdApplied(t, "B after connect", b, wantB)

	// B goes away: A removes its display. A new B process comes up and
	// dials A: both displays are created again.
	b.stop(t)
	eventually(t, "A display removed", func() bool { vd, _ := a.p.virtual(); return vd == nil })
	pb := newFakePlat("darwin", macMon.Rect)
	pb.setMonitors(macMon)
	b2 := newNode(t, testCfg("bravo", "left", testSecret, false), pb)
	b2.c.cfg.Peer = a.addr
	b2.start(t)
	vdApplied(t, "A after reconnect", a, wantA)
	vdApplied(t, "B after reconnect", b2, wantB)
}

// A display going to sleep drops out of the platform's monitor list; the
// virtual displays stay (the field bug: the Mac's asleep display left both
// sides without one).
func TestVirtualDisplayDisplayAsleep(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)
	_, calls := a.p.virtual()
	b.p.mu.Lock()
	b.p.monitors = []plat.Monitor{}
	b.p.mu.Unlock()
	time.Sleep(boundsPoll + 500*time.Millisecond)
	if vd, n := a.p.virtual(); vd == nil || n != calls {
		t.Fatalf("A virtual display %+v after %d calls (was %d) while B's display sleeps", vd, n, calls)
	}
	if vd, _ := b.p.virtual(); vd == nil {
		t.Fatal("B removed its virtual display while its display sleeps")
	}
}

// A peer of another protocol version (old builds send 0) is refused at
// Hello time, on both the dialing and the listening side.
func TestVersionMismatchRefused(t *testing.T) {
	a := newNode(t, testCfg("alpha", "right", testSecret, false), newFakePlat("windows", winMon.Rect))
	a.c.cfg.Peer = "127.0.0.1:9"
	a.start(t)

	// Listening side: an old dialer.
	pc := dialRawSession(t, a)
	if err := pc.Send(&proto.Msg{Type: proto.THello, Hello: &proto.Hello{Name: "bravo", OS: "darwin", Bounds: macMon.Rect, Token: newToken()}}); err != nil {
		t.Fatal(err)
	}
	if m, err := pc.Recv(); err != nil || m.Type != proto.THello || m.Hello.Version != proto.Version {
		t.Fatalf("reply %+v, %v; want our Hello with Version %d", m, err, proto.Version)
	}
	time.Sleep(200 * time.Millisecond)
	if a.c.link() != nil {
		t.Fatal("linked with an old dialer")
	}

	// Dialing side: an old listener answers. The dialer gives up on it and
	// backs off instead of redialing every second.
	ln, err := kcp.ListenWithOptions("127.0.0.1:0", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	d := newNode(t, testCfg("delta", "right", testSecret, false), newFakePlat("windows", winMon.Rect))
	d.c.cfg.Peer = ln.Addr().String()
	d.start(t)
	attempts := make(chan time.Time, 16)
	go func() {
		for {
			s, err := ln.AcceptKCP()
			if err != nil {
				return
			}
			go func() {
				defer s.Close()
				pc, err := proto.Handshake(s, testSecret, false)
				if err != nil {
					return
				}
				m, err := pc.Recv()
				if err != nil || m.Hello == nil || m.Hello.Name == "" {
					return
				}
				attempts <- time.Now()
				pc.Send(&proto.Msg{Type: proto.THello, Hello: &proto.Hello{Name: "old", OS: "darwin", Bounds: macMon.Rect}})
				for {
					if _, err := pc.Recv(); err != nil {
						return
					}
				}
			}()
		}
	}()
	var times []time.Time
	for len(times) < 3 {
		select {
		case at := <-attempts:
			times = append(times, at)
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d dial attempts", len(times))
		}
	}
	if d.c.link() != nil {
		t.Fatal("linked with an old listener")
	}
	// Backoff 1 s then 2 s.
	if gap := times[2].Sub(times[1]); gap < 1500*time.Millisecond {
		t.Fatalf("redial after %v; backoff not growing (%v)", gap, times)
	}
}
