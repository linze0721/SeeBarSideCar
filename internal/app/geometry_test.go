package app

import (
	"math"
	"testing"

	"magic/internal/plat"
	"magic/internal/proto"
)

// The deployed setup: Windows (two 150% monitors) with the Mac on its right.
var (
	winDisplay2 = plat.Monitor{Rect: plat.Rect{W: 3840, H: 2160}, UnitsPerLogical: 1.5, Scale: 1, Primary: true}
	winDisplay1 = plat.Monitor{Rect: plat.Rect{X: -2880, W: 2880, H: 1620}, UnitsPerLogical: 1.5, Scale: 1}
	macBuiltin  = plat.Monitor{Rect: plat.Rect{W: 1470, H: 956}, UnitsPerLogical: 1, Scale: 2, Primary: true}
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestGeometryMapRoundTrip(t *testing.T) {
	from := plat.Rect{X: 3840, Y: 363, W: 2205, H: 1434}
	to := plat.Rect{W: 1470, H: 956}
	for _, pt := range [][2]float64{{3840, 363}, {6045, 1797}, {4000.5, 1000.25}, {3700, 200}, {7000, 2500}} {
		x, y := mapPoint(pt[0], pt[1], from, to)
		bx, by := mapPoint(x, y, to, from)
		if !near(bx, pt[0]) || !near(by, pt[1]) {
			t.Errorf("round trip %v -> (%v,%v) -> (%v,%v)", pt, x, y, bx, by)
		}
	}
	if x, y := mapPoint(3840, 363, from, to); x != 0 || y != 0 {
		t.Errorf("origin maps to (%v,%v)", x, y)
	}
	if x, y := mapPoint(6045, 1797, from, to); !near(x, 1470) || !near(y, 956) {
		t.Errorf("far corner maps to (%v,%v)", x, y)
	}

	r := plat.Rect{X: 3940, Y: 463, W: 441, H: 287}
	m := mapRect(r, from, to)
	back := mapRect(m, to, from)
	if back.X > r.X || back.Y > r.Y || back.X+back.W < r.X+r.W || back.Y+back.H < r.Y+r.H {
		t.Errorf("mapRect round trip %+v -> %+v -> %+v does not cover the original", r, m, back)
	}
	if m != (plat.Rect{X: 66, Y: 66, W: 295, H: 192}) {
		t.Errorf("mapRect = %+v", m)
	}
}

func TestGeometryVirtualFor(t *testing.T) {
	win := []plat.Monitor{winDisplay1, winDisplay2}
	mac := []plat.Monitor{macBuiltin}

	vd, of, ok := virtualFor(win, mac, proto.EdgeRight)
	if !ok {
		t.Fatal("no virtual display for Windows")
	}
	if want := (plat.Rect{X: 3840, Y: 363, W: 2205, H: 1434}); vd.Rect != want {
		t.Errorf("Windows V = %+v, want %+v", vd.Rect, want)
	}
	if vd.UnitsPerLogical != 1.5 || of != macBuiltin.Rect {
		t.Errorf("Windows V upl %v of %+v", vd.UnitsPerLogical, of)
	}

	vd, of, ok = virtualFor(mac, win, proto.EdgeLeft)
	if !ok {
		t.Fatal("no virtual display for the Mac")
	}
	if want := (plat.Rect{X: -2560, Y: -242, W: 2560, H: 1440}); vd.Rect != want {
		t.Errorf("Mac V = %+v, want %+v", vd.Rect, want)
	}
	if vd.UnitsPerLogical != 1 || of != winDisplay2.Rect {
		t.Errorf("Mac V upl %v of %+v", vd.UnitsPerLogical, of)
	}

	if _, _, ok := virtualFor(mac, nil, proto.EdgeLeft); ok {
		t.Error("virtual display without peer monitors")
	}
}

// The two machines' maps agree across the shared edge: a point just inside
// the Mac's V maps to the Windows edge, and Windows' V maps it back.
func TestGeometryEdgeConsistency(t *testing.T) {
	win := []plat.Monitor{winDisplay1, winDisplay2}
	mac := []plat.Monitor{macBuiltin}
	wv, wof, _ := virtualFor(win, mac, proto.EdgeRight)
	mv, mof, _ := virtualFor(mac, win, proto.EdgeLeft)

	// Windows cursor on its right edge, mid DISPLAY2 → Mac V's right edge.
	x, y := mapPoint(3839, 1080, mof, mv.Rect)
	if !near(x, -2560+3839.0/1.5) || !near(y, -242+1080.0/1.5) {
		t.Errorf("Windows edge in Mac coords = (%v,%v)", x, y)
	}
	// Mac cursor on its left edge → Windows V's left edge.
	x, y = mapPoint(0, 478, wof, wv.Rect)
	if x != 3840 || !near(y, 363+478*1.5) {
		t.Errorf("Mac edge in Windows coords = (%v,%v)", x, y)
	}
}

func TestGeometryNearestIn(t *testing.T) {
	a := plat.Rect{W: 100, H: 100}
	b := plat.Rect{X: 100, Y: 20, W: 50, H: 50}
	cases := []struct{ x, y, wx, wy float64 }{
		{50, 50, 50, 50},     // inside a
		{120, 30, 120, 30},   // inside b
		{-10, 50, 0, 50},     // left of a
		{200, 40, 149, 40},   // right of b
		{110, 95, 99, 95},    // below b: a's right column is nearer
		{130, -10, 130, 20},  // above b
		{500, 500, 149, 69},  // far corner
		{98.5, 10, 98.5, 10}, // fractional, inside
	}
	for _, c := range cases {
		if x, y := nearestIn(c.x, c.y, a, b); x != c.wx || y != c.wy {
			t.Errorf("nearestIn(%v,%v) = (%v,%v), want (%v,%v)", c.x, c.y, x, y, c.wx, c.wy)
		}
	}
	// Empty rects are ignored.
	if x, y := nearestIn(-5, -5, plat.Rect{}, a); x != 0 || y != 0 {
		t.Errorf("nearestIn with empty rect = (%v,%v)", x, y)
	}
}

func TestGeometryAdjacentMonitor(t *testing.T) {
	win := []plat.Monitor{winDisplay1, winDisplay2}
	if m, _ := adjacentMonitor(win, proto.EdgeRight); m.Rect != winDisplay2.Rect {
		t.Errorf("right = %+v", m.Rect)
	}
	if m, _ := adjacentMonitor(win, proto.EdgeLeft); m.Rect != winDisplay1.Rect {
		t.Errorf("left = %+v", m.Rect)
	}
	// Both touch the top; DISPLAY2 is wider.
	if m, _ := adjacentMonitor(win, proto.EdgeTop); m.Rect != winDisplay2.Rect {
		t.Errorf("top = %+v", m.Rect)
	}
	if _, ok := adjacentMonitor(nil, proto.EdgeTop); ok {
		t.Error("adjacent monitor of nothing")
	}
}

// Edge mapping between the deployed machines: the whole Windows right edge
// maps onto the whole Mac left edge, inset by 12 logical pixels.
func TestGeometryEdgeMapping(t *testing.T) {
	w, m := winDisplay2.Rect, macBuiltin.Rect
	for _, c := range []struct{ y, wy int32 }{{0, 0}, {100, 44}, {1080, 478}, {2100, 929}, {2159, 955}} {
		f := edgeFrac(w, proto.EdgeRight, 3839, float64(c.y))
		if x, y := edgePoint(m, proto.EdgeLeft, f, logicalUnits(macBuiltin, entryLogical)); x != 12 || y != c.wy {
			t.Errorf("Windows y=%d -> Mac (%d,%d), want (12,%d)", c.y, x, y, c.wy)
		}
	}
	f := edgeFrac(m, proto.EdgeLeft, 0, 478)
	if x, y := edgePoint(w, proto.EdgeRight, f, logicalUnits(winDisplay2, returnLogical)); x != 3821 || y != 1080 {
		t.Errorf("Mac y=478 -> Windows (%d,%d), want (3821,1080)", x, y)
	}
	// Fractions clamp; insets stay inside small rects.
	if f := edgeFrac(m, proto.EdgeTop, -50, 0); f != 0 {
		t.Errorf("clamped fraction %v", f)
	}
	if x, y := edgePoint(plat.Rect{X: 10, Y: 10, W: 5, H: 5}, proto.EdgeBottom, 1, 100); x != 14 || y != 12 {
		t.Errorf("small rect edge point (%d,%d)", x, y)
	}
	if n := logicalUnits(plat.Monitor{UnitsPerLogical: 0.1}, entryLogical); n != 2 {
		t.Errorf("minimum inset %d", n)
	}
}

func TestGeometryEdgePixel(t *testing.T) {
	r := plat.Rect{X: 100, Y: 0, W: 200, H: 100}
	for _, c := range []struct {
		e    proto.Edge
		x, y int32
		on   bool
		dist int32
	}{
		{proto.EdgeRight, 299, 50, true, 0},
		{proto.EdgeRight, 298, 50, false, 1},
		{proto.EdgeRight, 299, 100, false, 0}, // below the span
		{proto.EdgeLeft, 100, 0, true, 0},
		{proto.EdgeLeft, 136, 0, false, 36},
		{proto.EdgeTop, 150, 0, true, 0},
		{proto.EdgeBottom, 150, 99, true, 0},
		{proto.EdgeBottom, 150, 90, false, 9},
	} {
		if on := onEdgePixel(r, c.e, c.x, c.y); on != c.on {
			t.Errorf("onEdgePixel(%v, %d,%d) = %v", c.e, c.x, c.y, on)
		}
		if d := edgeDistance(r, c.e, c.x, c.y); d != c.dist {
			t.Errorf("edgeDistance(%v, %d,%d) = %d, want %d", c.e, c.x, c.y, d, c.dist)
		}
	}
	if !pastSide(r, proto.EdgeLeft, 99.5, 50) || pastSide(r, proto.EdgeLeft, 100, 50) ||
		!pastSide(r, proto.EdgeRight, 300, 50) || pastSide(r, proto.EdgeRight, 299.9, 50) {
		t.Error("pastSide")
	}
}
