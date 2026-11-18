package app

import (
	"encoding/binary"
	"math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"magic/internal/plat"
	"magic/internal/proto"
)

func TestMotionOOBDeliversLatestPosition(t *testing.T) {
	var active atomic.Bool
	var oobSeen atomic.Int32
	a, b := motionPair(t, func(p []byte, upstream bool) bool {
		if !active.Load() || !upstream {
			return false
		}
		// No reliable fallback can reach B. Drop the first OOB packet too.
		return !isOOB(p) || oobSeen.Add(1) == 1
	}, nil)
	active.Store(true)
	a.p.push(plat.Event{Kind: plat.MouseMove, DX: 5})
	a.p.push(plat.Event{Kind: plat.MouseMove, DX: 15, DY: -4})
	eventually(t, "latest position over OOB", injected(b.p, motionEvent(120, 96)))
	if oobSeen.Load() < 2 || injected(b.p, motionEvent(105, 100))() {
		t.Fatal("first motion should have been lost without blocking the newer one")
	}
}

func TestMotionFenceRejectsLateMove(t *testing.T) {
	a, b := motionPair(t, func(p []byte, upstream bool) bool { return upstream && isOOB(p) }, nil)
	// Allocate a counter before the real forwarded move, but deliver the
	// packet only after its successor's fence has arrived with the click.
	epoch := motionEpoch(a)
	old := sealMotion(a, epoch, 101, 100)
	a.p.push(plat.Event{Kind: plat.MouseMove, DX: 50, DY: 25})
	click := plat.Event{Kind: plat.MouseButton, Button: plat.ButtonLeft, Down: true}
	a.p.push(click)
	eventually(t, "click after pointer position", injected(b.p, click))
	got := b.p.injections()
	if i := slices.Index(got, click); i < 1 || got[i-1] != motionEvent(150, 125) {
		t.Fatalf("click was not preceded by its position: %+v", got)
	}
	b.c.link().recvMotion(old)
	assertNoMotion(t, b, 101, 100)
	// The advanced fence must not suppress a genuinely later packet.
	b.c.link().recvMotion(sealMotion(a, epoch, 151, 126))
	eventually(t, "post-fence move", injected(b.p, motionEvent(151, 126)))
}

func TestMotionRejectsOldEpochAfterReenter(t *testing.T) {
	a, b := motionPair(t, nil, nil)
	oldEpoch := motionEpoch(a)
	a.p.push(plat.Event{Kind: plat.Key, HID: hidScrollLock, Down: true})
	eventually(t, "peer no longer controlling", func() bool { return !controlled(b)() })
	enterMotionPair(t, a, b, 200, 100)
	newEpoch := motionEpoch(a)
	if newEpoch != oldEpoch+1 {
		t.Fatalf("re-enter epoch = %d; want %d", newEpoch, oldEpoch+1)
	}
	// A high authenticated counter cannot substitute for a current epoch.
	b.c.link().recvMotion(sealMotion(a, oldEpoch, 99, 99))
	assertNoMotion(t, b, 99, 99)
	b.c.link().recvMotion(sealMotion(a, newEpoch, 201, 101))
	eventually(t, "current epoch move", injected(b.p, motionEvent(201, 101)))
}

func TestMotionDeliversRestingPositionReliably(t *testing.T) {
	var dropped atomic.Int32
	a, b := motionPair(t, func(p []byte, upstream bool) bool {
		if upstream && isOOB(p) {
			dropped.Add(1)
			return true
		}
		return false
	}, nil)
	a.p.push(plat.Event{Kind: plat.MouseMove, DX: 10})
	a.p.push(plat.Event{Kind: plat.MouseMove, DX: 20, DY: 2})
	final := motionEvent(130, 102)
	eventually(t, "reliable resting position", injected(b.p, final))
	if dropped.Load() == 0 || injected(b.p, motionEvent(110, 100))() {
		t.Fatal("did not exercise OOB loss and reliable recovery")
	}
	time.Sleep(3 * motionIdle)
	if got := b.p.injections(); !slices.Equal(got, []plat.Event{motionEvent(100, 100), final}) {
		t.Fatalf("want a single reliable rest update after entry, got %+v", got)
	}
}

func assertNoMotion(t *testing.T, b *node, x, y int32) {
	t.Helper()
	// Allow the asynchronous worker ample time to inject an incorrect move.
	time.Sleep(3 * motionIdle)
	if injected(b.p, motionEvent(x, y))() {
		t.Fatalf("unexpected motion at (%d,%d)", x, y)
	}
}

func motionEvent(x, y int32) plat.Event {
	return plat.Event{Kind: plat.MouseMove, X: x, Y: y}
}

func motionEpoch(a *node) uint32 {
	a.c.k.mu.Lock()
	defer a.c.k.mu.Unlock()
	return a.c.k.txEpoch
}

func sealMotion(a *node, epoch uint32, x, y int32) []byte {
	p := binary.LittleEndian.AppendUint32([]byte{1}, epoch)
	p = binary.LittleEndian.AppendUint32(p, uint32(x))
	p = binary.LittleEndian.AppendUint32(p, uint32(y))
	return a.c.link().ctl.pc.Datagram().Seal(nil, p)
}

func enterMotionPair(t *testing.T, a, b *node, x, y int32) {
	t.Helper()
	a.c.k.mu.Lock()
	a.c.enterLocked(a.c.link(), a.c.Peer(), float64(x), float64(y))
	a.c.k.mu.Unlock()
	eventually(t, "enter delivered", injected(b.p, motionEvent(x, y)))
}

func motionPair(t *testing.T, drop func([]byte, bool) bool, observe func(plat.Event)) (a, b *node) {
	t.Helper()
	a = newNode(t, testCfg("alpha", "right", testSecret, false), newFakePlat("windows", winMon.Rect))
	b = newNode(t, testCfg("bravo", "left", testSecret, false), newFakePlat("darwin", macMon.Rect))
	b.p.onInject = observe
	// Only A dials; both its control and bulk connections go through the proxy.
	a.c.cfg.Peer = proxyUDP(t, b.addr, drop)
	b.c.cfg.Peer = "127.0.0.1:0"
	b.start(t)
	a.start(t)
	eventually(t, "both peers connected", func() bool { return a.c.Peer() != nil && b.c.Peer() != nil })
	enterMotionPair(t, a, b, 100, 100)
	return a, b
}

// This stack has neither FEC nor KCP block encryption: its OOB header is
// the four-byte conversation ID followed by the fork's little-endian marker.
func isOOB(udp []byte) bool {
	return len(udp) >= 6 && binary.LittleEndian.Uint16(udp[4:]) == 0x01f3
}

// proxyUDP preserves each session's source address via a separate upstream
// UDP socket. drop is called in both directions, concurrently.
func proxyUDP(t *testing.T, peer string, drop func([]byte, bool) bool) string {
	t.Helper()
	upstream, err := net.ResolveUDPAddr("udp4", peer)
	if err != nil {
		t.Fatal(err)
	}
	front, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		clients := make(map[netip.AddrPort]*net.UDPConn)
		var wg sync.WaitGroup
		defer func() {
			for _, conn := range clients {
				conn.Close()
			}
			wg.Wait()
		}()
		var buf [2048]byte
		for {
			n, src, err := front.ReadFromUDPAddrPort(buf[:])
			if err != nil {
				return
			}
			if drop != nil && drop(buf[:n], true) {
				continue
			}
			conn := clients[src]
			if conn == nil {
				conn, err = net.DialUDP("udp4", nil, upstream)
				if err != nil {
					t.Error(err)
					return
				}
				clients[src] = conn
				wg.Go(func() {
					var reply [2048]byte
					for {
						n, err := conn.Read(reply[:])
						if err != nil {
							return
						}
						if drop == nil || !drop(reply[:n], false) {
							front.WriteToUDPAddrPort(reply[:n], src)
						}
					}
				})
			}
			conn.Write(buf[:n])
		}
	}()
	t.Cleanup(func() { front.Close(); <-done })
	return front.LocalAddr().String()
}

func TestMotionUnderPacketLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("500 moves per lane over a lossy UDP proxy")
	}
	for _, reliable := range []bool{true, false} {
		name := "oob"
		if reliable {
			name = "reliable"
		}
		t.Run(name, func(t *testing.T) {
			const moves = 500
			type arrival struct {
				index int
				at    time.Time
			}
			arrivals := make(chan arrival, moves)
			var seen [moves]bool // accessed only by the fake's serialized injection callback
			a, b := motionPair(t, func([]byte, bool) bool { return rand.IntN(10) == 0 }, func(ev plat.Event) {
				if i := int(ev.X) - 101; ev.Kind == plat.MouseMove && ev.Y == 100 && i >= 0 && i < moves && !seen[i] {
					seen[i] = true
					arrivals <- arrival{i, time.Now()}
				}
			})
			l := a.c.link()
			if reliable {
				// Reproduce the old control session and its ordered stream.
				for _, s := range []*session{l.ctl, b.c.link().ctl} {
					s.kcp.SetMinRTO(30)
					s.kcp.SetDupPush(0)
				}
			}
			var sent [moves]time.Time
			for i := range sent {
				sent[i] = time.Now()
				if reliable {
					ev := plat.Event{Kind: plat.MouseMove}
					a.c.enqueueInput(l, &proto.Msg{Type: proto.TInput, X: 101 + int32(i), Y: 100, Input: &ev}, true)
				} else {
					a.p.push(plat.Event{Kind: plat.MouseMove, DX: 1})
				}
				if i+1 < moves {
					time.Sleep(4 * time.Millisecond)
				}
			}
			eventually(t, "final experiment position", injected(b.p, motionEvent(100+moves, 100)))
			latencies := make([]time.Duration, 0, moves)
			for len(arrivals) > 0 {
				r := <-arrivals
				latencies = append(latencies, r.at.Sub(sent[r.index]))
			}
			if reliable && len(latencies) != moves || !reliable && len(latencies) < 350 {
				t.Fatalf("unexpected delivery count: %d/%d", len(latencies), moves)
			}
			slices.Sort(latencies)
			percentile := func(p int) time.Duration { return latencies[(p*len(latencies)+99)/100-1] }
			t.Logf("10%% UDP loss, %d/%d moves injected: p50=%v p99=%v", len(latencies), moves, percentile(50), percentile(99))
		})
	}
}
