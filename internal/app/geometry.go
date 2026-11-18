package app

import (
	"math"

	"magic/internal/plat"
	"magic/internal/proto"
)

// Span geometry. Each machine owns a virtual display standing for the
// peer monitor adjacent to its configured edge; the two are related by a
// linear rect-to-rect map that also extends outside the rects, so both
// machines agree on positions across the shared edge.

// mapPoint maps (x, y) linearly from rect from to rect to.
func mapPoint(x, y float64, from, to plat.Rect) (float64, float64) {
	return float64(to.X) + (x-float64(from.X))*float64(to.W)/float64(from.W),
		float64(to.Y) + (y-float64(from.Y))*float64(to.H)/float64(from.H)
}

// mapRect maps r from rect from to rect to, rounding outward.
func mapRect(r, from, to plat.Rect) plat.Rect {
	x0, y0 := mapPoint(float64(r.X), float64(r.Y), from, to)
	x1, y1 := mapPoint(float64(r.X+r.W), float64(r.Y+r.H), from, to)
	ix, iy := int32(math.Floor(x0)), int32(math.Floor(y0))
	return plat.Rect{X: ix, Y: iy, W: int32(math.Ceil(x1)) - ix, H: int32(math.Ceil(y1)) - iy}
}

func intersects(a, b plat.Rect) bool {
	return a.X < b.X+b.W && b.X < a.X+a.W && a.Y < b.Y+b.H && b.Y < a.Y+a.H
}

// adjacentMonitor is the monitor of ms touching side e of their union,
// choosing the one with the longest extent along that side (ties: primary).
func adjacentMonitor(ms []plat.Monitor, e proto.Edge) (plat.Monitor, bool) {
	if len(ms) == 0 {
		return plat.Monitor{}, false
	}
	u := unionMonitors(ms)
	var best plat.Monitor
	var bestLen int32 = -1
	for _, m := range ms {
		r := m.Rect
		var touches bool
		var l int32
		switch e {
		case proto.EdgeLeft:
			touches, l = r.X == u.X, r.H
		case proto.EdgeRight:
			touches, l = r.X+r.W == u.X+u.W, r.H
		case proto.EdgeTop:
			touches, l = r.Y == u.Y, r.W
		case proto.EdgeBottom:
			touches, l = r.Y+r.H == u.Y+u.H, r.W
		}
		if touches && (l > bestLen || l == bestLen && m.Primary) {
			best, bestLen = m, l
		}
	}
	return best, bestLen >= 0
}

func unionMonitors(ms []plat.Monitor) plat.Rect {
	u := ms[0].Rect
	for _, m := range ms[1:] {
		r := m.Rect
		x0, y0 := min(u.X, r.X), min(u.Y, r.Y)
		x1, y1 := max(u.X+u.W, r.X+r.W), max(u.Y+u.H, r.Y+r.H)
		u = plat.Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
	}
	return u
}

// virtualFor computes this machine's virtual display: it sits outside the
// local adjacent monitor at edge e, has the peer adjacent monitor's logical
// size in local units, and is centered on the local monitor along the edge.
// It returns the request and the peer monitor rect it mirrors.
func virtualFor(local, peer []plat.Monitor, e proto.Edge) (plat.VirtualDisplay, plat.Rect, bool) {
	la, ok := adjacentMonitor(local, e)
	if !ok {
		return plat.VirtualDisplay{}, plat.Rect{}, false
	}
	pa, ok := adjacentMonitor(peer, opposite(e))
	if !ok {
		return plat.VirtualDisplay{}, plat.Rect{}, false
	}
	upl := la.UnitsPerLogical
	if !(upl > 0) {
		upl = 1
	}
	pupl := pa.UnitsPerLogical
	if !(pupl > 0) {
		pupl = 1
	}
	ratio := upl / pupl
	w := int32(math.Round(float64(pa.Rect.W) * ratio))
	h := int32(math.Round(float64(pa.Rect.H) * ratio))
	l := la.Rect
	var r plat.Rect
	switch e {
	case proto.EdgeLeft:
		r = plat.Rect{X: l.X - w, Y: l.Y + (l.H-h)/2, W: w, H: h}
	case proto.EdgeRight:
		r = plat.Rect{X: l.X + l.W, Y: l.Y + (l.H-h)/2, W: w, H: h}
	case proto.EdgeTop:
		r = plat.Rect{X: l.X + (l.W-w)/2, Y: l.Y - h, W: w, H: h}
	case proto.EdgeBottom:
		r = plat.Rect{X: l.X + (l.W-w)/2, Y: l.Y + l.H, W: w, H: h}
	}
	return plat.VirtualDisplay{Rect: r, UnitsPerLogical: upl}, pa.Rect, true
}

// nearestIn clamps (x, y) to the nearest point of the union of rects.
func nearestIn(x, y float64, rs ...plat.Rect) (float64, float64) {
	bx, by, bd := x, y, math.Inf(1)
	for _, r := range rs {
		if r.W <= 0 || r.H <= 0 {
			continue
		}
		cx := min(max(x, float64(r.X)), float64(r.X+r.W-1))
		cy := min(max(y, float64(r.Y)), float64(r.Y+r.H-1))
		if d := (cx-x)*(cx-x) + (cy-y)*(cy-y); d < bd {
			bx, by, bd = cx, cy, d
		}
	}
	return bx, by
}

func containsF(r plat.Rect, x, y float64) bool {
	return x >= float64(r.X) && y >= float64(r.Y) && x < float64(r.X+r.W) && y < float64(r.Y+r.H)
}

// edgeFrac is the position of (x, y) along side e of r as a fraction of
// that side, clamped to [0, 1].
func edgeFrac(r plat.Rect, e proto.Edge, x, y float64) float64 {
	if r.W <= 0 || r.H <= 0 {
		return 0.5
	}
	var f float64
	switch e {
	case proto.EdgeLeft, proto.EdgeRight:
		f = (y - float64(r.Y)) / float64(r.H)
	default:
		f = (x - float64(r.X)) / float64(r.W)
	}
	return min(max(f, 0), 1)
}

// edgePoint is the point of r at fraction f along side e, inset units
// inside r from that side (at most half of r).
func edgePoint(r plat.Rect, e proto.Edge, f float64, inset int32) (int32, int32) {
	switch e {
	case proto.EdgeLeft, proto.EdgeRight:
		inset = min(inset, (r.W-1)/2)
		y := min(max(r.Y+round32(f*float64(r.H)), r.Y), r.Y+r.H-1)
		if e == proto.EdgeLeft {
			return r.X + inset, y
		}
		return r.X + r.W - 1 - inset, y
	default:
		inset = min(inset, (r.H-1)/2)
		x := min(max(r.X+round32(f*float64(r.W)), r.X), r.X+r.W-1)
		if e == proto.EdgeTop {
			return x, r.Y + inset
		}
		return x, r.Y + r.H - 1 - inset
	}
}

// edgeDistance is how far (x, y) is inside r from its outermost pixel on
// side e; negative beyond it.
func edgeDistance(r plat.Rect, e proto.Edge, x, y int32) int32 {
	switch e {
	case proto.EdgeLeft:
		return x - r.X
	case proto.EdgeRight:
		return r.X + r.W - 1 - x
	case proto.EdgeTop:
		return y - r.Y
	}
	return r.Y + r.H - 1 - y
}

// onEdgePixel reports whether (x, y) is on the outermost pixel of side e
// of r, within r's span along that side. Points beyond it are in the
// virtual display, which has its own rules.
func onEdgePixel(r plat.Rect, e proto.Edge, x, y int32) bool {
	if edgeDistance(r, e, x, y) != 0 {
		return false
	}
	switch e {
	case proto.EdgeLeft, proto.EdgeRight:
		return y >= r.Y && y < r.Y+r.H
	}
	return x >= r.X && x < r.X+r.W
}

// pastSide reports whether (x, y) lies beyond side e of r.
func pastSide(r plat.Rect, e proto.Edge, x, y float64) bool {
	switch e {
	case proto.EdgeLeft:
		return x < float64(r.X)
	case proto.EdgeRight:
		return x >= float64(r.X+r.W)
	case proto.EdgeTop:
		return y < float64(r.Y)
	}
	return y >= float64(r.Y+r.H)
}
