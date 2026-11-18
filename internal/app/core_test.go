package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/xtaci/kcp-go/v5"

	"magic/internal/keymap"
	"magic/internal/plat"
	"magic/internal/proto"
)

const testSecret = "correct horse battery"

// The deployed machines: A is the Windows laptop (3840x2160 at 150%) with
// the Mac (1470x956 points) on its right; B is the Mac. So:
//   - A's virtual display is the Mac in A units: {3840, 363, 2205, 1434};
//   - B's virtual display is A's monitor in B units: {-2560, -242, 2560, 1440};
//   - crossing insets are 18 A units (12 logical at 150%) and 12 B units,
//     the rearm distance on A is 36 units.
var (
	winMon = plat.Monitor{Rect: plat.Rect{W: 3840, H: 2160}, UnitsPerLogical: 1.5, Scale: 1, Primary: true}
	macMon = plat.Monitor{Rect: plat.Rect{W: 1470, H: 956}, UnitsPerLogical: 1, Scale: 2, Primary: true}

	macBounds = macMon.Rect

	aVirtual = plat.Rect{X: 3840, Y: 363, W: 2205, H: 1434}
	bVirtual = plat.Rect{X: -2560, Y: -242, W: 2560, H: 1440}
)

const testSwitchDelay = 50 * time.Millisecond

func testCfg(name, edge, secret string, clipboard bool) *Config {
	t := true
	return &Config{
		Name:           name,
		Listen:         "127.0.0.1:0",
		Secret:         secret,
		Edge:           edge,
		SwapCmdCtrl:    &t,
		Clipboard:      &clipboard,
		MouseScale:     1,
		VirtualDisplay: &t,
		SwitchDelayMS:  int(testSwitchDelay / time.Millisecond),
		FPS:            30,
		JPEGQuality:    70,
		// 8 MiB keeps the transfer tests' 3 MiB file under the cap.
		FileClipMaxMB: 8,
	}
}

// node is one running core.
type node struct {
	c      *core
	p      *fakePlat
	f      Feature // nil: no window sharing
	addr   string
	cancel context.CancelFunc
	done   chan struct{}
}

func (n *node) stop(t *testing.T) {
	t.Helper()
	n.cancel()
	select {
	case <-n.done:
	case <-time.After(5 * time.Second):
		t.Fatal("core did not stop")
	}
}

func newNode(t *testing.T, cfg *Config, p *fakePlat) *node {
	t.Helper()
	c := newCore(cfg, p)
	addr, err := c.listen()
	if err != nil {
		t.Fatal(err)
	}
	return &node{c: c, p: p, addr: addr.String()}
}

func (n *node) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel, n.done = cancel, make(chan struct{})
	go func() {
		defer close(n.done)
		if err := n.c.run(ctx, n.f); err != nil {
			t.Error(err)
		}
	}()
	t.Cleanup(func() { n.stop(t) })
}

// pair starts two cores that dial each other: A (winMon) has the peer on
// its right, B (macMon) has A on its left.
func pair(t *testing.T, osA, osB string, clipboard bool, secretB string) (a, b *node) {
	t.Helper()
	return pairWith(t, osA, osB, clipboard, secretB, nil)
}

// pairWith is pair with a hook to adjust the nodes before they start.
func pairWith(t *testing.T, osA, osB string, clipboard bool, secretB string, setup func(a, b *node)) (a, b *node) {
	t.Helper()
	pa, pb := newFakePlat(osA, winMon.Rect), newFakePlat(osB, macMon.Rect)
	pa.setMonitors(winMon)
	pb.setMonitors(macMon)
	a = newNode(t, testCfg("alpha", "right", testSecret, clipboard), pa)
	b = newNode(t, testCfg("bravo", "left", secretB, clipboard), pb)
	a.c.cfg.Peer, b.c.cfg.Peer = b.addr, a.addr
	if setup != nil {
		setup(a, b)
	}
	a.start(t)
	b.start(t)
	return a, b
}

// connected waits for the link both sides keep and for both virtual
// displays to be applied and announced to the other side. Both peers dial
// at start, and an in-flight dial of the smaller name replaces a link
// dialed by the larger one, so a link must also last a moment.
func connected(t *testing.T, a, b *node) {
	t.Helper()
	shared := func() *link {
		la, lb := a.c.link(), b.c.link()
		if la == nil || lb == nil || a.c.Peer() == nil || b.c.Peer() == nil ||
			port(la.ctl.nc.LocalAddr()) != port(lb.ctl.nc.RemoteAddr()) ||
			port(lb.ctl.nc.LocalAddr()) != port(la.ctl.nc.RemoteAddr()) {
			return nil
		}
		return la
	}
	eventually(t, "one lasting shared link", func() bool {
		l := shared()
		if l == nil {
			return false
		}
		time.Sleep(200 * time.Millisecond)
		return shared() == l
	})
	eventually(t, "virtual displays announced", func() bool {
		va, vb := a.c.Virtual(), b.c.Virtual()
		pa, pb := a.c.Peer(), b.c.Peer()
		return va.Rect.W > 0 && vb.Rect.W > 0 && pa != nil && pb != nil && pa.Virtual == vb && pb.Virtual == va
	})
}

func lastInject(p *fakePlat) (plat.Event, bool) {
	evs := p.injections()
	if len(evs) == 0 {
		return plat.Event{}, false
	}
	return evs[len(evs)-1], true
}

func injected(p *fakePlat, want plat.Event) func() bool {
	return func() bool {
		for _, ev := range p.injections() {
			if ev == want {
				return true
			}
		}
		return false
	}
}

func controlled(n *node) func() bool {
	return func() bool {
		n.c.k.mu.Lock()
		defer n.c.k.mu.Unlock()
		return n.c.k.controlled
	}
}

func grabbed(n *node) func() bool { return n.p.grabbed }

func move(n *node, x, y int32) { n.p.push(plat.Event{Kind: plat.MouseMove, X: x, Y: y}) }

// push moves A's cursor to (x, y) twice, the switch delay apart, as a
// hook reports a user pushing against the screen edge.
func push(a *node, x, y int32) {
	move(a, x, y)
	time.Sleep(testSwitchDelay + 5*time.Millisecond)
	move(a, x, y)
}

// cross pushes A's cursor against the middle of its right edge: B gets
// the pointer at the middle of its left edge, 12 units in.
func cross(t *testing.T, a, b *node) {
	t.Helper()
	push(a, 3839, 1080)
	if !a.p.grabbed() {
		t.Fatal("A not grabbed after pushing against the edge")
	}
	eventually(t, "B entry point", injected(b.p, plat.Event{Kind: plat.MouseMove, X: 12, Y: 478}))
	eventually(t, "B controlled", controlled(b))
}

func returned(t *testing.T, a *node, x, y int32) {
	t.Helper()
	if a.p.grabbed() {
		t.Fatal("A still grabbed after leaving")
	}
	if ev, _ := lastInject(a.p); ev != (plat.Event{Kind: plat.MouseMove, X: x, Y: y}) {
		t.Fatalf("A return inject = %+v, want MouseMove %d,%d", ev, x, y)
	}
}

// back moves the controlled pointer from B's entry point past B's left
// edge once the leave hysteresis allows it: A gets the pointer back.
func back(a *node) {
	time.Sleep(enterCooldown)
	a.p.push(plat.Event{Kind: plat.MouseMove, DX: -30})
}

// From deep inside A's real virtual display the pointer appears where
// the user sees it on B (the virtual display's map), and the first small
// delta back toward the shared edge does not bounce it back to A.
func TestKVMEnterFromInsideVirtual(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)
	push(a, 3853, 1080) // 13 units inside V
	if !a.p.grabbed() {
		t.Fatal("not grabbed inside the virtual display")
	}
	// (3853-3840) * 1470/2205 = 8.67, (1080-363) * 956/1434 = 478.
	eventually(t, "B entry point", injected(b.p, plat.Event{Kind: plat.MouseMove, X: 9, Y: 478}))

	// Leftover motion toward A right after the grab: 15 A units are 10 B
	// units, past B's left edge, yet the pointer stays on B.
	a.p.push(plat.Event{Kind: plat.MouseMove, DX: -15})
	if !a.p.grabbed() {
		t.Fatal("bounced back right after entering")
	}
	// Moving 24+ units away from B's edge arms it at once.
	a.p.push(plat.Event{Kind: plat.MouseMove, DX: 45})
	a.p.push(plat.Event{Kind: plat.MouseMove, DX: -30})
	if !a.p.grabbed() {
		t.Fatal("left before crossing B's edge")
	}
	a.p.push(plat.Event{Kind: plat.MouseMove, DX: -30})
	returned(t, a, 3821, 1080)

	// Deep inside (the field case: Mac cursor far left in its V).
	time.Sleep(enterCooldown)
	move(a, 3000, 1080)
	push(a, 5310, 400)
	// (5310-3840) * 1470/2205 = 980, (400-363) * 956/1434 = 24.67.
	eventually(t, "B deep entry point", injected(b.p, plat.Event{Kind: plat.MouseMove, X: 980, Y: 25}))
}

// After entering, B's shared edge sends the pointer back only once it
// moved away from it or enterCooldown passed.
func TestKVMLeaveHysteresis(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)
	cross(t, a, b)
	a.p.push(plat.Event{Kind: plat.MouseMove, DX: -30})
	if !a.p.grabbed() {
		t.Fatal("left immediately after entering")
	}
	eventually(t, "clamped at B's edge", injected(b.p, plat.Event{Kind: plat.MouseMove, X: 0, Y: 478}))
	time.Sleep(enterCooldown)
	a.p.push(plat.Event{Kind: plat.MouseMove, DX: -3})
	returned(t, a, 3821, 1080)
}

func TestVirtualDisplayHello(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)

	vd, _ := a.p.virtual()
	if vd == nil || *vd != (plat.VirtualDisplay{Rect: aVirtual, UnitsPerLogical: 1.5}) {
		t.Fatalf("A virtual display = %+v", vd)
	}
	vd, _ = b.p.virtual()
	if vd == nil || *vd != (plat.VirtualDisplay{Rect: bVirtual, UnitsPerLogical: 1}) {
		t.Fatalf("B virtual display = %+v", vd)
	}
	if got, want := b.c.Peer().Virtual, (proto.Virtual{Rect: aVirtual, Of: macMon.Rect, Real: true}); got != want {
		t.Fatalf("B sees A's virtual %+v, want %+v", got, want)
	}
	if got, want := a.c.Peer().Virtual, (proto.Virtual{Rect: bVirtual, Of: winMon.Rect, Real: true}); got != want {
		t.Fatalf("A sees B's virtual %+v, want %+v", got, want)
	}
	if len(a.c.Peer().Monitors) != 1 || a.c.Peer().Monitors[0] != macMon {
		t.Fatalf("A sees B's monitors %+v", a.c.Peer().Monitors)
	}

	// B's monitor changes: A re-creates its virtual display to match.
	b.p.setMonitors(plat.Monitor{Rect: plat.Rect{W: 1728, H: 1117}, UnitsPerLogical: 1, Scale: 2, Primary: true})
	want := plat.VirtualDisplay{Rect: plat.Rect{X: 3840, Y: 242, W: 2592, H: 1676}, UnitsPerLogical: 1.5}
	eventually(t, "A virtual display follows B's monitor", func() bool {
		vd, _ := a.p.virtual()
		return vd != nil && *vd == want && b.c.Peer().Virtual.Rect == want.Rect
	})

	b.stop(t)
	eventually(t, "A virtual display removed", func() bool { vd, _ := a.p.virtual(); return vd == nil })
	if v := a.c.Virtual(); v != (proto.Virtual{}) {
		t.Fatalf("A Virtual after disconnect = %+v", v)
	}
}

// The whole shared edge crosses, including the rows above and below A's
// virtual display, top to top and bottom to bottom, inset from B's edge.
func TestKVMEdgeMapping(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)
	for _, tc := range []struct{ y, wantY int32 }{{100, 44}, {2100, 929}, {0, 0}, {2159, 955}} {
		time.Sleep(enterCooldown)
		move(a, 3000, tc.y) // rearm after the previous return
		push(a, 3839, tc.y)
		if !a.p.grabbed() {
			t.Fatalf("y=%d: not grabbed", tc.y)
		}
		eventually(t, "B entry point", injected(b.p, plat.Event{Kind: plat.MouseMove, X: 12, Y: tc.wantY}))
		// Straight back out: A gets the matching row, 18 units in.
		back(a)
		wantY := int32(float64(tc.wantY) / 956 * 2160)
		if ev, _ := lastInject(a.p); a.p.grabbed() || ev.X != 3821 || ev.Y < wantY-2 || ev.Y > wantY+2 {
			t.Fatalf("y=%d: return inject %+v (grabbed %v), want 3821,~%d", tc.y, ev, a.p.grabbed(), wantY)
		}
	}
}

func TestKVMDwell(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)

	// One move at the edge does not cross, however long ago it was.
	move(a, 3839, 1080)
	if a.p.grabbed() {
		t.Fatal("crossed on the first move")
	}
	time.Sleep(testSwitchDelay + 5*time.Millisecond)
	// Leaving the trigger zone resets the dwell.
	move(a, 3800, 1080)
	move(a, 3839, 1080)
	if a.p.grabbed() {
		t.Fatal("crossed although the cursor left the edge in between")
	}
	time.Sleep(testSwitchDelay / 2)
	move(a, 3839, 1000)
	if a.p.grabbed() {
		t.Fatal("crossed before the switch delay")
	}
	time.Sleep(testSwitchDelay/2 + 5*time.Millisecond)
	move(a, 3839, 1080)
	if !a.p.grabbed() {
		t.Fatal("not crossed after the switch delay")
	}
	eventually(t, "B entry point", injected(b.p, plat.Event{Kind: plat.MouseMove, X: 12, Y: 478}))

	// A real virtual display is part of the trigger zone.
	a.p.push(plat.Event{Kind: plat.Key, HID: hidScrollLock, Down: true})
	time.Sleep(enterCooldown)
	move(a, 3000, 1080)
	push(a, 4942, 1080)
	if !a.p.grabbed() {
		t.Fatal("not crossed from inside the virtual display")
	}
}

func TestKVMCrossAndReturn(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)
	cross(t, a, b)

	// 150 A units are 100 B units.
	a.p.push(plat.Event{Kind: plat.MouseMove, DX: 150, DY: 15})
	eventually(t, "relative move on B", injected(b.p, plat.Event{Kind: plat.MouseMove, X: 112, Y: 488}))

	a.p.push(plat.Event{Kind: plat.Key, HID: keymap.LeftCtrl, Down: true})
	a.p.push(plat.Event{Kind: plat.Key, HID: keymap.LeftCtrl})
	eventually(t, "Ctrl arrives as Cmd", injected(b.p, plat.Event{Kind: plat.Key, HID: keymap.LeftGUI}))
	if !injected(b.p, plat.Event{Kind: plat.Key, HID: keymap.LeftGUI, Down: true})() {
		t.Fatal("Cmd down missing on B")
	}

	// Without buttons B's virtual display is not part of the domain: past
	// B's left edge the pointer comes back, 18 units inside A's edge.
	a.p.push(plat.Event{Kind: plat.MouseMove, DX: -180, DY: -15})
	returned(t, a, 3821, 1080)
	eventually(t, "B controlled state cleared", func() bool { return !controlled(b)() })
	for _, ev := range b.p.injections() {
		if ev.Kind == plat.MouseMove && ev.X < 0 {
			t.Fatalf("B got a move into its virtual display: %+v", ev)
		}
	}

	// Hysteresis: after the cooldown the edge stays disarmed until the
	// cursor has been 36 units away from it.
	time.Sleep(enterCooldown)
	move(a, 3810, 1080)
	push(a, 3839, 1080)
	if a.p.grabbed() {
		t.Fatal("re-entered before moving away from the edge")
	}
	move(a, 3803, 1080)
	push(a, 3839, 1080)
	if !a.p.grabbed() {
		t.Fatal("did not re-enter after moving away from the edge")
	}
}

// Right after returning, the edge does not re-enter even if rearmed.
func TestKVMCooldown(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)
	cross(t, a, b)
	back(a)
	returned(t, a, 3821, 1080)
	move(a, 3000, 1080)
	move(a, 3839, 1080)
	move(a, 3839, 1080)
	if a.p.grabbed() {
		t.Fatal("re-entered within the cooldown")
	}
}

// Input injected for a spanning window does not count as the user
// heading for the peer.
func TestKVMSpanInputQuiet(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)
	if err := a.c.InjectForPeer(plat.Event{Kind: plat.MouseMove, X: 4942, Y: 1080}); err != nil {
		t.Fatal(err)
	}
	push(a, 4942, 1080)
	if a.p.grabbed() {
		t.Fatal("crossed on input injected for a spanning window")
	}
	time.Sleep(spanInputQuiet)
	cross(t, a, b)
}

// The Mac hosts a window spanning into its virtual display while the
// Windows pointer drives it through the overlay: every injection warps
// the Mac cursor into its virtual display and the OS re-reports it,
// unmarked and slightly jittered, as a local move. That must never send
// the pointer to Windows, during the interaction or after it, until the
// user moves the Mac's own mouse away and back.
func TestKVMSpanHostNoEnter(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)
	var x, y int32
	for i := range 25 { // 1.25 s
		x, y = int32(-1500+20*i), int32(300+5*i)
		if err := b.c.InjectForPeer(plat.Event{Kind: plat.MouseMove, X: x, Y: y}); err != nil {
			t.Fatal(err)
		}
		move(b, x, y)
		move(b, x+1, y-1)
		time.Sleep(testSwitchDelay)
	}
	// The peer stopped driving; the OS keeps re-reporting, the real mouse
	// gets nudged.
	time.Sleep(spanInputQuiet)
	push(b, x, y)
	push(b, x+10, y+10)
	if b.p.grabbed() || controlled(a)() {
		t.Fatal("span host sent the pointer to the peer")
	}
	for _, ev := range a.p.injections() {
		if ev.Kind == plat.MouseMove {
			t.Fatalf("peer pointer injected on A: %+v", ev)
		}
	}

	// The user moves the Mac's mouse out and back into the virtual display.
	move(b, 700, 478)
	push(b, -500, 478)
	if !b.p.grabbed() {
		t.Fatal("user push into the virtual display did not cross")
	}
	eventually(t, "A controlled", controlled(a))
}

// The peer's pointer is on us: our own edge and virtual display never
// send it back as a TEnter.
func TestKVMNoEnterWhileControlled(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)
	cross(t, a, b)
	push(b, 0, 478)
	push(b, -500, 478)
	if b.p.grabbed() {
		t.Fatal("controlled side entered the peer")
	}
}

// A native drag (button held) continues into the virtual display; the
// pointer crosses only once the button is released there, at the virtual
// display's map.
func TestKVMEnterWaitsForButtons(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)

	move(a, 1000, 1080)
	a.p.push(plat.Event{Kind: plat.MouseButton, Button: plat.ButtonLeft, Down: true})
	push(a, 3839, 1080)
	push(a, 4942, 1080)
	if a.p.grabbed() {
		t.Fatal("entered with a button held")
	}
	a.p.push(plat.Event{Kind: plat.MouseButton, Button: plat.ButtonLeft})
	if !a.p.grabbed() {
		t.Fatal("did not enter on button release inside the virtual display")
	}
	// (4942-3840) * 1470/2205 = 734.67, (1080-363) * 956/1434 = 478.
	eventually(t, "B entry point", injected(b.p, plat.Event{Kind: plat.MouseMove, X: 735, Y: 478}))
	for _, ev := range b.p.injections() {
		if ev.Kind == plat.MouseButton {
			t.Fatalf("local button forwarded: %+v", ev)
		}
	}
}

// While controlling, a held button extends the domain into B's virtual
// display (B's native drag continues there); release returns the pointer
// at the virtual display's map.
func TestKVMLeaveWaitsForButtons(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)
	cross(t, a, b)

	a.p.push(plat.Event{Kind: plat.MouseButton, Button: plat.ButtonLeft, Down: true})
	a.p.push(plat.Event{Kind: plat.MouseMove, DX: -100})
	if !a.p.grabbed() {
		t.Fatal("left B with a button held")
	}
	eventually(t, "drag into B's virtual display", injected(b.p, plat.Event{Kind: plat.MouseMove, X: -55, Y: 478}))

	a.p.push(plat.Event{Kind: plat.MouseButton, Button: plat.ButtonLeft})
	// (-54.67+2560) * 1.5 = 3758, (478+242) * 1.5 = 1080.
	returned(t, a, 3758, 1080)
	eventually(t, "button up on B", injected(b.p, plat.Event{Kind: plat.MouseButton, Button: plat.ButtonLeft}))
	eventually(t, "B controlled state cleared", func() bool { return !controlled(b)() })
}

// A button pressed during the grab whose up the OS delivers after the grab
// ended must not block later crossings.
func TestKVMButtonResetOnGrab(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)
	cross(t, a, b)
	a.p.push(plat.Event{Kind: plat.MouseButton, Button: plat.ButtonRight, Down: true})
	a.p.push(plat.Event{Kind: plat.Key, HID: hidScrollLock, Down: true})
	if a.p.grabbed() {
		t.Fatal("ScrollLock did not release the grab")
	}
	time.Sleep(enterCooldown)
	move(a, 3000, 1080)
	push(a, 3839, 1080)
	if !a.p.grabbed() {
		t.Fatal("a button held across the grab change blocks crossing")
	}
}

// A phantom virtual display (creation failed or disabled) still lets the
// pointer cross at the edge pixel and come back inset from it.
func TestKVMPhantomVirtual(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(a, b *node)
		calls bool
	}{
		{"error", func(a, b *node) { a.p.setVDErr(errors.New("no driver")) }, true},
		{"disabled", func(a, b *node) { f := false; a.c.cfg.VirtualDisplay = &f }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := pairWith(t, "windows", "darwin", false, testSecret, tc.setup)
			connected(t, a, b)
			if v := a.c.Virtual(); v != (proto.Virtual{Rect: aVirtual, Of: macMon.Rect}) {
				t.Fatalf("A Virtual = %+v, want phantom", v)
			}
			if vd, n := a.p.virtual(); vd != nil || (n > 0) != tc.calls {
				t.Fatalf("A virtual display %+v after %d calls", vd, n)
			}

			push(a, 3838, 1080)
			if a.p.grabbed() {
				t.Fatal("grabbed short of the edge")
			}
			cross(t, a, b)
			back(a)
			returned(t, a, 3821, 1080)
		})
	}
}

func TestKVMNoSwapSameOS(t *testing.T) {
	a, b := pair(t, "windows", "windows", false, testSecret)
	connected(t, a, b)
	cross(t, a, b)
	a.p.push(plat.Event{Kind: plat.Key, HID: keymap.LeftGUI, Down: true})
	eventually(t, "GUI passes through", injected(b.p, plat.Event{Kind: plat.Key, HID: keymap.LeftGUI, Down: true}))
	if injected(b.p, plat.Event{Kind: plat.Key, HID: keymap.LeftCtrl, Down: true})() {
		t.Fatal("key swapped between equal OSes")
	}
}

func TestKVMEmergencyRelease(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)
	cross(t, a, b)
	a.p.push(plat.Event{Kind: plat.Key, HID: hidScrollLock, Down: true})
	// B's left edge middle maps to A's right edge middle, inset.
	returned(t, a, 3821, 1080)
	eventually(t, "B controlled state cleared", func() bool { return !controlled(b)() })
	for _, ev := range b.p.injections() {
		if ev.Kind == plat.Key && ev.HID == hidScrollLock {
			t.Fatal("release hotkey was forwarded")
		}
	}

	time.Sleep(enterCooldown)
	move(a, 3000, 1080)
	cross(t, a, b)
	for _, k := range []uint16{keymap.LeftCtrl, keymap.LeftAlt, keymap.LeftShift, hidEscape} {
		a.p.push(plat.Event{Kind: plat.Key, HID: k, Down: true})
	}
	if a.p.grabbed() {
		t.Fatal("Ctrl+Alt+Shift+Esc did not release the grab")
	}
}

// B going away (it says goodbye) ends control on A at once and releases
// what A held down on B.
func TestKVMDisconnectWhileControlling(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)
	cross(t, a, b)
	const keyA = 0x04
	a.p.push(plat.Event{Kind: plat.Key, HID: keyA, Down: true})
	eventually(t, "key held on B", injected(b.p, plat.Event{Kind: plat.Key, HID: keyA, Down: true}))

	b.stop(t)
	start := time.Now()
	eventually(t, "A ungrabbed", func() bool { return !a.p.grabbed() })
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("A noticed the goodbye after %v", d)
	}
	eventually(t, "A cursor returned", func() bool {
		ev, ok := lastInject(a.p)
		return ok && ev == plat.Event{Kind: plat.MouseMove, X: 3821, Y: 1080}
	})
	if !injected(b.p, plat.Event{Kind: plat.Key, HID: keyA})() {
		t.Fatal("B did not release the held key")
	}
	if a.c.Peer() != nil {
		t.Fatal("A still reports a peer")
	}
}

// The controlled side said goodbye: B releases A's held keys at once.
func TestByeReleasesControlled(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)
	cross(t, a, b)
	const keyA = 0x04
	a.p.push(plat.Event{Kind: plat.Key, HID: keyA, Down: true})
	eventually(t, "key held on B", injected(b.p, plat.Event{Kind: plat.Key, HID: keyA, Down: true}))
	a.stop(t)
	start := time.Now()
	eventually(t, "B released the key", injected(b.p, plat.Event{Kind: plat.Key, HID: keyA}))
	eventually(t, "B not controlled", func() bool { return !controlled(b)() })
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("B noticed the goodbye after %v", d)
	}
	eventually(t, "B virtual display removed", func() bool { vd, _ := b.p.virtual(); return vd == nil })
}

func TestClipboardBothWays(t *testing.T) {
	a, b := pair(t, "darwin", "windows", true, testSecret)
	connected(t, a, b)
	// The first poll only records the baseline; copy after it.
	eventually(t, "baseline polls", func() bool { return a.p.seqPolls() > 0 && b.p.seqPolls() > 0 })

	a.p.copyText("from alpha")
	eventually(t, "clipboard on B", func() bool { text, _, _ := b.p.clipboard(); return text == "from alpha" })
	b.p.copyText("from bravo")
	eventually(t, "clipboard on A", func() bool { text, _, _ := a.p.clipboard(); return text == "from bravo" })

	// Let both sides poll several more times: an echo would set again.
	polls := a.p.seqPolls()
	eventually(t, "more polls", func() bool { return a.p.seqPolls() >= polls+3 })
	if _, sets, _ := a.p.clipboard(); sets != 1 {
		t.Fatalf("A clipboard set %d times, want 1", sets)
	}
	if _, sets, _ := b.p.clipboard(); sets != 1 {
		t.Fatalf("B clipboard set %d times, want 1", sets)
	}
}

// rawPeer is a hand-driven peer for exact message accounting: a for a
// listener that a dials. It returns the control connection and every
// non-ping message received on either session; bulk messages it sends go
// through the second Conn.
func rawPeer(t *testing.T, a *node) (*proto.Conn, chan *proto.Msg) {
	t.Helper()
	ctl, _, msgs := rawPeerBulk(t, a)
	return ctl, msgs
}

func rawPeerBulk(t *testing.T, a *node) (ctl, bulk *proto.Conn, msgs chan *proto.Msg) {
	t.Helper()
	ctl, bulk = openRawPeer(t, a)
	if err := ctl.Send(&proto.Msg{Type: proto.TReady}); err != nil {
		t.Fatal(err)
	}
	msgs = make(chan *proto.Msg, 64)
	recv := func(pc *proto.Conn) {
		for {
			m, err := pc.Recv()
			if err != nil {
				return
			}
			if m.Type != proto.TPing && m.Type != proto.THello {
				msgs <- m
			}
		}
	}
	go recv(ctl)
	go recv(bulk)
	return ctl, bulk, msgs
}

// openRawPeer stops at the last step before confirming the pair to the dialer.
func openRawPeer(t *testing.T, a *node) (ctl, bulk *proto.Conn) {
	t.Helper()
	ln, err := kcp.ListenWithOptions("127.0.0.1:0", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	a.c.cfg.Peer = ln.Addr().String()
	a.start(t)
	accept := func() (*proto.Conn, *proto.Msg) {
		s, err := ln.AcceptKCP()
		if err != nil {
			t.Fatal(err)
		}
		tuneSession(s)
		pc, err := proto.Handshake(s, testSecret, false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pc.Close() })
		m, err := pc.Recv()
		if err != nil || m.Type != proto.THello || m.Hello == nil {
			t.Fatalf("first message %+v, %v", m, err)
		}
		return pc, m
	}
	ctl, hello := accept()
	if err := ctl.Send(&proto.Msg{Type: proto.THello, Hello: &proto.Hello{Name: "raw", OS: "windows", Bounds: winMon.Rect, Version: proto.Version}}); err != nil {
		t.Fatal(err)
	}
	bulk, bh := accept()
	if !bytes.Equal(bh.Hello.Token, hello.Hello.Token) || bh.Hello.Name != "" {
		t.Fatalf("bulk Hello %+v does not match control token %x", bh.Hello, hello.Hello.Token)
	}
	return ctl, bulk
}

// dialRaw dials n as a hand-driven peer named name and completes the
// control handshake and Hello exchange with token; it returns the control
// Conn and n's Hello.
func dialRaw(t *testing.T, n *node, name string, token []byte) (*proto.Conn, *proto.Hello) {
	t.Helper()
	pc := dialRawSession(t, n)
	if err := pc.Send(&proto.Msg{Type: proto.THello, Hello: &proto.Hello{Name: name, OS: "darwin", Bounds: macMon.Rect, Token: token, Version: proto.Version}}); err != nil {
		t.Fatal(err)
	}
	m, err := pc.Recv()
	if err != nil || m.Type != proto.THello {
		t.Fatalf("reply %+v, %v", m, err)
	}
	return pc, m.Hello
}

func dialRawSession(t *testing.T, n *node) *proto.Conn {
	t.Helper()
	s, err := kcp.DialWithOptions(n.addr, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	tuneSession(s)
	pc, err := proto.Handshake(s, testSecret, true)
	if err != nil {
		t.Fatal(err)
	}
	return pc
}

func newToken() []byte {
	tok := make([]byte, tokenSize)
	rand.Read(tok)
	return tok
}

// A second link from the peer's name that sends TBye (a dying process,
// or a duplicate that lost) leaves the working link alone.
func TestStaleLinkByeKeepsCurrent(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)
	var cur *link
	eventually(t, "settled link", func() bool { cur = a.c.link(); return cur != nil && cur.dialedBy == "alpha" })

	tok := newToken()
	pc, _ := dialRaw(t, a, "bravo", tok)
	bulk := dialRawSession(t, a)
	if err := bulk.Send(&proto.Msg{Type: proto.THello, Hello: &proto.Hello{Token: tok}}); err != nil {
		t.Fatal(err)
	}
	if err := pc.Send(&proto.Msg{Type: proto.TBye}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if a.c.link() != cur || a.c.Peer() == nil || b.c.Peer() == nil {
		t.Fatal("a stale link's goodbye replaced or dropped the current link")
	}
}

// The peer restarts without a goodbye reaching us: its new link replaces
// the dead one right away instead of being refused until the dead one
// times out (which made the new process reconnect in a loop).
func TestPeerRestartReplacesDeadLink(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)
	var old *link
	eventually(t, "settled link", func() bool { old = a.c.link(); return old != nil && old.dialedBy == "alpha" })

	// B's socket for that link vanishes silently; B redials as "bravo".
	b.c.ln.Close()
	eventually(t, "link replaced", func() bool {
		l := a.c.link()
		return l != nil && l != old && l.dialedBy == "bravo" && b.c.Peer() != nil
	})
}

func TestClipboardNoEcho(t *testing.T) {
	a := newNode(t, testCfg("alpha", "right", testSecret, true), newFakePlat("darwin", macBounds))
	pc, msgs := rawPeer(t, a)
	eventually(t, "connection", func() bool { return a.c.Peer() != nil })
	eventually(t, "baseline poll", func() bool { return a.p.seqPolls() > 0 })

	if err := pc.Send(&proto.Msg{Type: proto.TClipboard, Text: "remote"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "clipboard set", func() bool { text, _, _ := a.p.clipboard(); return text == "remote" })
	a.p.copyText("local")
	select {
	case m := <-msgs:
		if m.Type != proto.TClipboard || m.Text != "local" {
			t.Fatalf("got %+v, want TClipboard local", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("local copy not sent")
	}

	polls := a.p.seqPolls()
	eventually(t, "more polls", func() bool { return a.p.seqPolls() >= polls+3 })
	select {
	case m := <-msgs:
		t.Fatalf("unexpected message %+v", m)
	default:
	}
}

func port(a net.Addr) int { return a.(*net.UDPAddr).Port }

func TestSimultaneousDialsCollapse(t *testing.T) {
	for range 3 {
		a, b := pair(t, "darwin", "windows", false, testSecret)
		connected(t, a, b)
		var la, lb *link
		eventually(t, "one shared link", func() bool {
			la, lb = a.c.link(), b.c.link()
			return la != nil && lb != nil && port(la.ctl.nc.LocalAddr()) == port(lb.ctl.nc.RemoteAddr()) &&
				port(lb.ctl.nc.LocalAddr()) == port(la.ctl.nc.RemoteAddr())
		})
		if la.dialedBy != "alpha" || lb.dialedBy != "alpha" {
			t.Fatalf("kept link dialed by %q/%q, want alpha", la.dialedBy, lb.dialedBy)
		}
		// The losing duplicate must not knock out the winner later.
		time.Sleep(200 * time.Millisecond)
		if a.c.link() != la || b.c.link() != lb {
			t.Fatal("link changed after settling")
		}
		a.stop(t)
		b.stop(t)
	}
}

func TestWrongSecret(t *testing.T) {
	a, b := pair(t, "darwin", "windows", false, "a different secret")
	time.Sleep(500 * time.Millisecond)
	if a.c.link() != nil || b.c.link() != nil || a.c.Peer() != nil || b.c.Peer() != nil {
		t.Fatal("connected despite a wrong secret")
	}
}

func TestSendNotConnected(t *testing.T) {
	c := newCore(testCfg("alpha", "right", testSecret, false), newFakePlat("darwin", macBounds))
	if err := c.Send(&proto.Msg{Type: proto.TPing}); err == nil {
		t.Fatal("Send succeeded without a peer")
	}
}
