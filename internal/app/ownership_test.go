package app

import (
	"errors"
	"testing"
	"time"

	"magic/internal/plat"
)

// B (the Mac) owns window 42, dragged left into its virtual display
// {-2560,-242,2560,1440}, so it spans onto A's (Windows') monitor at
// {840,363,1500,900}.
const ownWin = 42

var (
	ownWinBounds = plat.Rect{X: -2000, Y: 0, W: 1000, H: 600}
	ownWinOnA    = plat.Rect{X: 840, Y: 363, W: 1500, H: 900}
)

// pairShare is pair with window sharing on both sides and B's window
// spanning onto A, shown there as an overlay.
func pairShare(t *testing.T) (a, b *node, ov *fakeOverlay) {
	t.Helper()
	a, b = pairWith(t, "windows", "darwin", false, testSecret, func(a, b *node) {
		a.f = NewSharing(a.c.cfg, a.p, a.c)
		b.f = NewSharing(b.c.cfg, b.p, b.c)
		b.p.addWindow(plat.WindowInfo{ID: ownWin, App: "Google Chrome", Title: "doc", Bounds: ownWinBounds}, testWindowImg())
	})
	connected(t, a, b)
	eventually(t, "B's layout", func() bool {
		id, ok := b.f.SpanAt(float64(ownWinOnA.X+10), float64(ownWinOnA.Y+10))
		return ok && id == ownWin
	})
	return a, b, overlay(t, a.p, 0)
}

// winInputOnB reports whether B injected an overlay move A forwarded from
// (1000,500) on A: (1000/1.5-2560, 500/1.5-242) floored.
func winInputOnB(b *node) bool {
	return injected(b.p, plat.Event{Kind: plat.MouseMove, X: -1894, Y: 91})()
}

// Controlling A, B's virtual cursor reaching B's own spanned window gives
// the pointer back to B, onto the real window in its virtual display.
func TestOwnershipControllingOntoOwnWindow(t *testing.T) {
	a, b, _ := pairShare(t)
	push(b, 0, 478)
	if !b.p.grabbed() {
		t.Fatal("B did not cross at its edge")
	}
	eventually(t, "A controlled", controlled(a))

	// Over A's empty desktop: stays on A (300 B units are 450 A units).
	b.p.push(plat.Event{Kind: plat.MouseMove, DX: -300})
	if !b.p.grabbed() {
		t.Fatal("left over the peer's empty desktop")
	}
	// Onto B's window: 800 B units are 1200 A units, to (2171, 1080).
	b.p.push(plat.Event{Kind: plat.MouseMove, DX: -800})
	// (2171/1.5 - 2560, 1080/1.5 - 242) = (-1112.7, 478).
	returned(t, b, -1113, 478)
	eventually(t, "A released", func() bool { return !controlled(a)() })

	// The user works on the window natively: B keeps the pointer.
	push(b, -1200, 478)
	push(b, -1100, 470)
	if b.p.grabbed() {
		t.Fatal("re-entered A while over B's own window")
	}
}

// Local B, cursor in its virtual display: over its own window it stays
// local and A shows the cursor; over the empty area it crosses.
func TestOwnershipLocalOverOwnWindow(t *testing.T) {
	a, b, _ := pairShare(t)
	push(b, -1013, 478)
	push(b, -1013, 478)
	if b.p.grabbed() || controlled(a)() {
		t.Fatal("crossed while over B's own window")
	}
	// (-1013+2560)*1.5 = 2320.5, (478+242)*1.5 = 1080.
	eventually(t, "shadow cursor on A", injected(a.p, plat.Event{Kind: plat.MouseMove, X: 2321, Y: 1080}))
	if !a.c.Shadowed() {
		t.Fatal("A not shadowed")
	}

	push(b, -2400, 478)
	if !b.p.grabbed() {
		t.Fatal("did not cross from the empty virtual display area")
	}
	// (-2400+2560)*1.5 = 240.
	eventually(t, "B's pointer on A", injected(a.p, plat.Event{Kind: plat.MouseMove, X: 240, Y: 1080}))
	eventually(t, "A controlled", controlled(a))
}

// A shows B's cursor over B's overlay: overlay input is that cursor, not
// the user, and is dropped until the shadow ends.
func TestOwnershipShadowDropsOverlayInput(t *testing.T) {
	a, b, ov := pairShare(t)
	move(b, -1013, 478)
	eventually(t, "A shadowed", a.c.Shadowed)
	ov.input(plat.Event{Kind: plat.MouseMove, X: 1000, Y: 500})
	time.Sleep(100 * time.Millisecond)
	if winInputOnB(b) {
		t.Fatal("overlay input forwarded while shadowed")
	}

	// B's cursor leaves the virtual display: hide.
	move(b, 700, 478)
	eventually(t, "A shadow hidden", func() bool { return !a.c.Shadowed() })
	ov.input(plat.Event{Kind: plat.MouseMove, X: 1000, Y: 500})
	eventually(t, "overlay input after hide", func() bool { return winInputOnB(b) })

	// A shadow that is not refreshed expires.
	a.c.recvCursor(10, 10, false)
	if !a.c.Shadowed() {
		t.Fatal("TCursor did not shadow")
	}
	time.Sleep(shadowHold + 20*time.Millisecond)
	if a.c.Shadowed() {
		t.Fatal("shadow did not expire")
	}
}

func TestOwnershipInjectRefusedWhileControlling(t *testing.T) {
	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)
	cross(t, a, b)
	n := len(a.p.injections())
	if err := a.c.InjectForPeer(plat.Event{Kind: plat.MouseMove, X: 4000, Y: 500}); !errors.Is(err, errControlling) {
		t.Fatalf("InjectForPeer while controlling = %v", err)
	}
	if len(a.p.injections()) != n {
		t.Fatal("injected while controlling")
	}
}

// The field sequence: the Mac's cursor deep in its virtual display over
// its spanned Chrome window. Before, the Mac entered Windows there,
// Windows' overlay fed the pointer back as window input, the Mac warped
// its grabbed cursor and bounced back within 78 ms.
func TestOwnershipFieldNoPingPong(t *testing.T) {
	a, b, ov := pairShare(t)
	for range 4 {
		push(b, -1712, 517)
	}
	if b.p.grabbed() || controlled(a)() {
		t.Fatal("Mac entered Windows over its own spanned window")
	}

	// Even when the Mac does control Windows (via its edge), Windows'
	// overlay does not feed the Mac's pointer back as window input.
	push(b, 0, 478)
	eventually(t, "A controlled", controlled(a))
	ov.input(plat.Event{Kind: plat.MouseMove, X: 1000, Y: 500})
	time.Sleep(100 * time.Millisecond)
	if winInputOnB(b) || !b.p.grabbed() {
		t.Fatal("overlay input while controlled reached the Mac")
	}
}
