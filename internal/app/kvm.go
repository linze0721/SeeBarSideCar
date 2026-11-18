package app

import (
	"errors"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"magic/internal/keymap"
	"magic/internal/plat"
	"magic/internal/proto"
)

const (
	enterCooldown = 300 * time.Millisecond // no re-entry this soon after control returned
	// Distances from the shared edge in logical pixels of the monitor they
	// apply to: where the pointer lands on the peer (entry) and on us
	// (return), and how far it must travel away from the edge it arrived
	// at before that edge sends it back (both ways).
	entryLogical  = 12
	returnLogical = 12
	rearmLogical  = 24
	// After injecting input for a spanning window, cursor positions inside
	// our virtual display are that injection (or the OS re-reporting it),
	// not the user heading for the peer.
	spanInputQuiet = 500 * time.Millisecond
	// The shadow cursor: while our cursor is inside our virtual display the
	// peer shows it (at most one TCursor per cursorInterval); the peer
	// stops treating overlay input as the user's shadowHold after the last.
	cursorInterval = 8 * time.Millisecond
	shadowHold     = 300 * time.Millisecond

	hidEscape     uint16 = 0x29
	hidScrollLock uint16 = 0x47
)

// kvm is the pointer-sharing state. The controlling half runs on the
// Capture hook thread (and the move flush timer). The controlled half
// shares mu between reliable dispatch and the motion injector. mu orders
// before core.mu.
//
// A crossing at the edge pixel maps proportionally along the whole shared
// edge, from our adjacent monitor la to the peer's adjacent monitor pa.
// Where the cursor is inside a real virtual display (the user moved it
// there, or a native drag was released there) the virtual display's map
// applies, so the pointer appears where the user sees it.
//
// The pointer belongs to the machine owning what is under it: over one of
// our windows spanning onto the peer's screen the pointer is ours (we
// never enter the peer there and take it back when the virtual cursor
// reaches one), and the peer only shows it as a shadow cursor (TCursor).
type kvm struct {
	mu sync.Mutex

	// Controlling: our physical input drives the peer.
	controlling bool
	vx, vy      float64      // virtual cursor in the peer's global coordinates
	peer        *proto.Hello // peer's Hello as of the last move
	pa          plat.Monitor // peer monitor at the shared edge
	domain      []plat.Rect  // where vx,vy may be: the peer's monitors
	dragDomain  []plat.Rect  // domain plus the peer's virtual display, while buttons are held
	returned    time.Time    // when control last came back to us
	enteredAt   time.Time    // when control went to the peer
	leaveArmed  bool         // moving past pa's shared edge returns the pointer

	txLink    *link
	txEpoch   uint32
	txFence   uint64
	txPayload [motionSize]byte
	txPacket  [motionSize + proto.DatagramOverhead]byte
	lastMove  time.Time // when a motion datagram was sent
	moveAt    time.Time // when a coalesced move is due (zero when none)
	idleAt    time.Time // when to reliably synchronize the resting position
	timer     *time.Timer

	lctrl, lalt, lshift bool   // local left modifiers, for the release hotkey
	btns                uint32 // local physical buttons held, bit per plat button
	spanInputAt         time.Time
	// Where we last injected the pointer for a peer-driven spanning window.
	// The OS may re-report that position as if the user put it there; such
	// reports must not cross to the peer. Cleared once a captured position
	// differs, i.e. the user really moved this machine's mouse.
	spanX, spanY int32
	spanSet      bool
	dwellAt      time.Time // first move of the current stay in the trigger zone
	disarmed     bool      // the edge waits for the cursor to move away after a return

	// Shadow cursor sent while our cursor is inside our virtual display.
	shadowing    bool // the peer shows it; a hide is due when it leaves
	shPending    bool // a coalesced TCursor waits for the next slot
	shX, shY     int32
	shLast       time.Time
	shTimer      *time.Timer
	shTimerArmed bool
	// UnixNano until which the peer's pointer is on our screen: its shadow
	// cursor, or (math.MaxInt64) its controlled pointer until TLeave.
	// Atomic: overlays read it from the UI thread, which must not wait on
	// mu (mu is held across plat.Inject).
	shadowUntil atomic.Int64

	// Controlled: the peer's pointer is on our screen (TEnter..TLeave),
	// and the inputs it pressed on us that it has not released yet.
	controlled bool
	heldKeys   map[uint16]struct{}
	heldBtns   map[uint8]struct{}
}

// onEvent is the plat.Capture callback. Both send lanes enqueue without
// waiting for network I/O.
func (c *core) onEvent(ev plat.Event) {
	k := &c.k
	k.mu.Lock()
	defer k.mu.Unlock()

	switch ev.Kind {
	case plat.Key:
		switch ev.HID {
		case keymap.LeftCtrl:
			k.lctrl = ev.Down
		case keymap.LeftAlt:
			k.lalt = ev.Down
		case keymap.LeftShift:
			k.lshift = ev.Down
		}
	case plat.MouseButton:
		if ev.Down {
			k.btns |= 1 << ev.Button
		} else {
			k.btns &^= 1 << ev.Button
		}
	}
	if !k.controlling {
		switch ev.Kind {
		case plat.MouseMove:
			c.shadowLocked(ev.X, ev.Y)
			c.maybeEnterLocked(ev.X, ev.Y)
		case plat.MouseButton:
			k.dwellAt = time.Time{}
			if !ev.Down && k.btns == 0 {
				// A native drag released inside the virtual display.
				x, y := c.p.CursorPos()
				c.maybeDropEnterLocked(x, y)
			}
		}
		return
	}

	switch ev.Kind {
	case plat.MouseMove:
		c.moveLocked(ev.DX, ev.DY)
	case plat.Key:
		if ev.Down && (ev.HID == hidScrollLock || ev.HID == hidEscape && k.lctrl && k.lalt && k.lshift) {
			c.leaveLocked(c.homePointLocked())
			return
		}
		ev.HID = c.MapKey(ev.HID)
		c.sendInputLocked(ev)
	case plat.MouseButton:
		c.sendInputLocked(ev)
		if !ev.Down && k.btns == 0 {
			c.maybeDropLeaveLocked()
		}
	case plat.Wheel:
		c.sendInputLocked(ev)
	}
}

// canEnterLocked reports whether the pointer may go to the peer now, and
// the current link and peer Hello if so.
func (c *core) canEnterLocked() (*link, *proto.Hello, bool) {
	k := &c.k
	c.mu.Lock()
	l, peer := c.cur, c.peer
	c.mu.Unlock()
	ok := !k.controlled && k.btns == 0 && l != nil && peer != nil &&
		time.Since(k.returned) >= enterCooldown && time.Since(k.spanInputAt) >= spanInputQuiet
	return l, peer, ok
}

// maybeEnterLocked handles a local cursor move: once the cursor has stayed
// in the trigger zone (our real virtual display, or the outermost pixels
// of la at the shared edge) for the switch delay, the pointer goes to the
// peer: from inside the virtual display at its mapped point, from the edge
// pixel at the edge-mapped point of pa, inset from its edge.
func (c *core) maybeEnterLocked(x, y int32) {
	k := &c.k
	c.mu.Lock()
	la := localEdgeMonitor(c.mons, c.local, c.edge)
	v := c.virt
	c.mu.Unlock()
	if k.disarmed {
		// Away from the edge either way: back onto our monitor or deep
		// into the virtual display.
		if abs32(edgeDistance(la.Rect, c.edge, x, y)) < logicalUnits(la, rearmLogical) {
			k.dwellAt = time.Time{}
			return
		}
		k.disarmed = false
	}
	l, peer, ok := c.canEnterLocked()
	// Inside our virtual display only the user pushing this machine's
	// pointer there counts, not the peer driving a spanning window.
	inV := v.Real && v.Rect.Contains(x, y) && !k.spanDrivenLocked(x, y, logicalUnits(la, rearmLogical))
	if !ok || !inV && !onEdgePixel(la.Rect, c.edge, x, y) {
		k.dwellAt = time.Time{}
		return
	}
	now := time.Now()
	if k.dwellAt.IsZero() {
		k.dwellAt = now
	}
	if now.Sub(k.dwellAt) < time.Duration(c.cfg.SwitchDelayMS)*time.Millisecond {
		return
	}
	pa := peerEdgeMonitor(peer, c.edge)
	var px, py float64
	if inV && v.Of.W > 0 && v.Of.H > 0 {
		px, py = mapPoint(float64(x), float64(y), v.Rect, v.Of)
		px, py = nearestIn(px, py, pa.Rect)
	} else {
		f := edgeFrac(la.Rect, c.edge, float64(x), float64(y))
		ex, ey := edgePoint(pa.Rect, opposite(c.edge), f, logicalUnits(pa, entryLogical))
		px, py = float64(ex), float64(ey)
	}
	if _, own := c.spanAt(px, py); own {
		// One of our windows is there: the pointer stays ours and the user
		// works on the real window in our virtual display.
		k.dwellAt = time.Time{}
		return
	}
	log.Printf("kvm: pointer to peer at (%.0f,%.0f): cursor at (%d,%d), in virtual display %v", px, py, x, y, inV)
	c.enterLocked(l, peer, px, py)
}

// spanDrivenLocked reports whether the cursor at (x, y) is where the peer
// put it for a spanning window rather than where the user moved it: the
// peer injected recently, or the cursor is still within slack units of our
// last injected position. The OS re-reports injected positions (and
// nudges of the real mouse) as ordinary moves. Once the user moves the
// cursor farther away, the injected position is forgotten.
func (k *kvm) spanDrivenLocked(x, y, slack int32) bool {
	if time.Since(k.spanInputAt) < spanInputQuiet {
		return true
	}
	if !k.spanSet {
		return false
	}
	if abs32(x-k.spanX) <= slack && abs32(y-k.spanY) <= slack {
		return true
	}
	k.spanSet = false
	return false
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

// maybeDropEnterLocked hands the pointer to the peer when the last button
// came up inside our real virtual display: a window was dropped there, so
// the pointer lands where the virtual display maps it.
func (c *core) maybeDropEnterLocked(x, y int32) {
	if c.k.spanDrivenLocked(x, y, 2) {
		return
	}
	l, peer, ok := c.canEnterLocked()
	if !ok {
		return
	}
	v := c.Virtual()
	if !v.Real || v.Of.W <= 0 || !v.Rect.Contains(x, y) {
		return
	}
	px, py := mapPoint(float64(x), float64(y), v.Rect, v.Of)
	px, py = nearestIn(px, py, v.Of)
	if _, own := c.spanAt(px, py); own {
		return // dropped on one of our (now spanning) windows: stay here
	}
	log.Printf("kvm: pointer to peer at (%.0f,%.0f): drop at (%d,%d) in virtual display", px, py, x, y)
	c.enterLocked(l, peer, px, py)
}

// enterLocked starts controlling the peer with its cursor at (px, py).
func (c *core) enterLocked(l *link, peer *proto.Hello, px, py float64) {
	k := &c.k
	c.hideShadowLocked()
	k.txEpoch++
	if !c.enqueueInput(l, &proto.Msg{Type: proto.TEnter, X: round32(px), Y: round32(py), Epoch: k.txEpoch}, false) {
		return
	}
	k.controlling = true
	k.txLink, k.txFence = l, 0
	k.setPeer(peer, c.edge)
	k.vx, k.vy = px, py
	k.lastMove = time.Time{}
	k.dwellAt = time.Time{}
	k.enteredAt = time.Now()
	k.leaveArmed = false
	k.btns = 0
	c.p.SetGrab(true)
}

func (c *core) moveLocked(dx, dy int32) {
	k := &c.k
	c.mu.Lock()
	if c.peer != nil && c.peer != k.peer {
		k.setPeer(c.peer, c.edge)
	}
	mons, local := c.mons, c.local
	c.mu.Unlock()
	f := c.cfg.MouseScale * unitRatio(k.peer.Monitors, mons, c.edge)
	ux, uy := k.vx+float64(dx)*f, k.vy+float64(dy)*f
	if k.btns != 0 {
		// The peer's native drag may continue into its virtual display.
		k.vx, k.vy = nearestIn(ux, uy, k.dragDomain...)
	} else {
		x, y := nearestIn(ux, uy, k.domain...)
		// Like the local edge after a return, pa's shared edge only sends
		// the pointer back once it moved away from it or some time passed:
		// the first deltas after a grab may still carry motion toward us.
		pe := opposite(c.edge)
		if !k.leaveArmed && (time.Since(k.enteredAt) >= enterCooldown ||
			edgeDistance(k.pa.Rect, pe, round32(x), round32(y)) >= logicalUnits(k.pa, rearmLogical)) {
			k.leaveArmed = true
		}
		if k.leaveArmed && containsF(k.pa.Rect, x, y) && pastSide(k.pa.Rect, pe, ux, uy) {
			la := localEdgeMonitor(mons, local, c.edge)
			fr := edgeFrac(k.pa.Rect, pe, x, y)
			c.leaveLocked(edgePoint(la.Rect, c.edge, fr, logicalUnits(la, returnLogical)))
			return
		}
		k.vx, k.vy = x, y
		if id, own := c.spanAt(x, y); own {
			c.ontoOwnWindowLocked(id, x, y)
			return
		}
	}
	c.queueMotionLocked()
}

// maybeDropLeaveLocked returns the pointer to us when the last button came
// up while the virtual cursor is inside the peer's virtual display: the
// peer's native drag dropped a window there.
func (c *core) maybeDropLeaveLocked() {
	k := &c.k
	pv := k.peer.Virtual
	if pv.Rect.W <= 0 || pv.Of.W <= 0 || !containsF(pv.Rect, k.vx, k.vy) {
		return
	}
	x, y := mapPoint(k.vx, k.vy, pv.Rect, pv.Of)
	x, y = nearestIn(x, y, pv.Of)
	c.leaveLocked(round32(x), round32(y))
}

// setPeer records the peer's Hello, its monitor at the shared edge e and
// the domains of the virtual cursor: the peer's monitors (its bounds if it
// announced none; the bounding box would include unreachable gaps), plus
// its real virtual display while buttons are held.
func (k *kvm) setPeer(peer *proto.Hello, e proto.Edge) {
	k.peer = peer
	k.pa = peerEdgeMonitor(peer, e)
	k.domain = k.domain[:0]
	for _, m := range peer.Monitors {
		k.domain = append(k.domain, m.Rect)
	}
	if len(k.domain) == 0 {
		k.domain = append(k.domain, peer.Bounds)
	}
	k.dragDomain = append(k.dragDomain[:0], k.domain...)
	if peer.Virtual.Real {
		k.dragDomain = append(k.dragDomain, peer.Virtual.Rect)
	}
}

// localEdgeMonitor is our monitor at the shared edge e (the bounds if no
// monitors are known).
func localEdgeMonitor(mons []plat.Monitor, bounds plat.Rect, e proto.Edge) plat.Monitor {
	if m, ok := adjacentMonitor(mons, e); ok {
		return m
	}
	return plat.Monitor{Rect: bounds, UnitsPerLogical: 1}
}

// peerEdgeMonitor is the peer's monitor facing our edge e.
func peerEdgeMonitor(peer *proto.Hello, e proto.Edge) plat.Monitor {
	if m, ok := adjacentMonitor(peer.Monitors, opposite(e)); ok {
		return m
	}
	return plat.Monitor{Rect: peer.Bounds, UnitsPerLogical: 1}
}

// logicalUnits converts n logical pixels of m to its units, at least 2.
func logicalUnits(m plat.Monitor, n float64) int32 {
	upl := m.UnitsPerLogical
	if !(upl > 0) {
		upl = 1
	}
	return max(2, round32(n*upl))
}

// unitRatio is peer units per local unit across the shared edge, so the
// pointer keeps its logical speed on both machines.
func unitRatio(peer, local []plat.Monitor, e proto.Edge) float64 {
	pa, ok1 := adjacentMonitor(peer, opposite(e))
	la, ok2 := adjacentMonitor(local, e)
	if !ok1 || !ok2 || !(pa.UnitsPerLogical > 0) || !(la.UnitsPerLogical > 0) {
		return 1
	}
	return pa.UnitsPerLogical / la.UnitsPerLogical
}

// leaveLocked gives the pointer back to this machine at (x, y) and tells
// the peer.
func (c *core) leaveLocked(x, y int32) {
	log.Printf("kvm: pointer back at (%d,%d)", x, y)
	c.enqueueInput(c.k.txLink, &proto.Msg{Type: proto.TLeave, X: x, Y: y}, false)
	c.returnLocalLocked(x, y)
}

// returnLocalLocked ends Controlling without messaging the peer: releases
// the grab, puts the local cursor at (x, y) and disarms the edge until the
// cursor moves away from it.
func (c *core) returnLocalLocked(x, y int32) {
	k := &c.k
	k.controlling = false
	k.txLink = nil
	k.moveAt, k.idleAt = time.Time{}, time.Time{}
	if k.timer != nil {
		k.timer.Stop()
	}
	// The OS may deliver the up of a button pressed during the grab on
	// either side of it.
	k.btns = 0
	k.dwellAt = time.Time{}
	k.disarmed = true
	c.p.SetGrab(false)
	c.injectLocked(plat.Event{Kind: plat.MouseMove, X: x, Y: y})
	k.returned = time.Now()
}

// homePointLocked is where the cursor returns when control ends other than
// by crossing back: the virtual cursor edge-mapped onto our adjacent
// monitor, inset from the shared edge.
func (c *core) homePointLocked() (int32, int32) {
	k := &c.k
	c.mu.Lock()
	la := localEdgeMonitor(c.mons, c.local, c.edge)
	c.mu.Unlock()
	f := edgeFrac(k.pa.Rect, opposite(c.edge), k.vx, k.vy)
	return edgePoint(la.Rect, c.edge, f, logicalUnits(la, returnLogical))
}

// hookSend enqueues for the current link without blocking.
func (c *core) hookSend(m *proto.Msg, droppable bool) bool {
	l := c.link()
	if l == nil {
		return false
	}
	return c.enqueueInput(l, m, droppable)
}

// recvEnter: the peer hands us the pointer at (x, y) in our coordinates.
func (c *core) recvEnter(l *link, m *proto.Msg) {
	k := &c.k
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.controlling {
		// Both sides crossed at once: the lexicographically smaller name
		// keeps control; the other yields.
		if peer := c.Peer(); peer != nil && c.cfg.Name < peer.Name {
			return
		}
		c.returnLocalLocked(c.homePointLocked())
	}
	c.hideShadowLocked()
	k.controlled = true
	l.motion.latest.Store(&motionPosition{epoch: m.Epoch})
	k.shadowUntil.Store(math.MaxInt64)
	log.Printf("kvm: peer pointer here at (%d,%d)", m.X, m.Y)
	c.injectLocked(plat.Event{Kind: plat.MouseMove, X: m.X, Y: m.Y})
}

// recvCursor shows the peer's cursor, which is over one of its spanning
// windows, at (x, y) in our coordinates; hide ends that. Meanwhile our
// overlays ignore input: it is this injected cursor, not the user.
func (c *core) recvCursor(x, y int32, hide bool) {
	k := &c.k
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.controlling || k.controlled {
		return // stale: the pointer changed hands since it was sent
	}
	if hide {
		k.shadowUntil.Store(0)
		return
	}
	k.shadowUntil.Store(time.Now().Add(shadowHold).UnixNano())
	c.injectLocked(plat.Event{Kind: plat.MouseMove, X: x, Y: y})
}

// Shadowed implements Session.
func (c *core) Shadowed() bool {
	return time.Now().UnixNano() < c.k.shadowUntil.Load()
}

// shadowLocked follows a local cursor move: inside our real virtual
// display (and put there by the user, not by the peer driving one of our
// windows) the peer shows the cursor where it maps, so the user sees it
// over our windows spanning onto the peer's screen.
func (c *core) shadowLocked(x, y int32) {
	k := &c.k
	c.mu.Lock()
	la := localEdgeMonitor(c.mons, c.local, c.edge)
	v := c.virt
	l := c.cur
	c.mu.Unlock()
	if k.controlled || l == nil || !v.Real || v.Of.W <= 0 || v.Of.H <= 0 || !v.Rect.Contains(x, y) ||
		k.spanDrivenLocked(x, y, logicalUnits(la, rearmLogical)) {
		c.hideShadowLocked()
		return
	}
	px, py := mapPoint(float64(x), float64(y), v.Rect, v.Of)
	k.shX, k.shY = round32(px), round32(py)
	k.shPending, k.shadowing = true, true
	if wait := cursorInterval - time.Since(k.shLast); wait <= 0 {
		c.flushShadowLocked()
	} else if !k.shTimerArmed {
		k.shTimerArmed = true
		if k.shTimer == nil {
			k.shTimer = time.AfterFunc(wait, c.shadowTimer)
		} else {
			k.shTimer.Reset(wait)
		}
	}
}

func (c *core) shadowTimer() {
	c.k.mu.Lock()
	defer c.k.mu.Unlock()
	c.k.shTimerArmed = false
	if c.k.shadowing {
		c.flushShadowLocked()
	}
}

func (c *core) flushShadowLocked() {
	k := &c.k
	if !k.shPending {
		return
	}
	k.shPending = false
	k.shLast = time.Now()
	c.hookSend(&proto.Msg{Type: proto.TCursor, X: k.shX, Y: k.shY}, true)
}

// hideShadowLocked tells the peer to stop showing our cursor. Losing it
// is harmless: the peer's shadow expires after shadowHold.
func (c *core) hideShadowLocked() {
	k := &c.k
	if !k.shadowing {
		return
	}
	k.shadowing, k.shPending = false, false
	c.hookSend(&proto.Msg{Type: proto.TCursor, Seq: 1}, true)
}

// spanAt reports the front-most of our windows spanning onto the peer's
// screen at (x, y) in the peer's coordinates.
func (c *core) spanAt(x, y float64) (uint64, bool) {
	if c.f == nil {
		return 0, false
	}
	return c.f.SpanAt(x, y)
}

// ontoOwnWindowLocked returns the pointer while controlling: the virtual
// cursor at (x, y) is over our window id spanning onto the peer's screen,
// so our cursor goes to that window in our virtual display and the user
// works on it natively.
func (c *core) ontoOwnWindowLocked(id uint64, x, y float64) {
	v := c.Virtual()
	lx, ly := mapPoint(x, y, v.Of, v.Rect)
	lx, ly = nearestIn(lx, ly, v.Rect)
	log.Printf("kvm: pointer onto own spanned window %#x at (%.0f,%.0f)", id, lx, ly)
	c.leaveLocked(round32(lx), round32(ly))
	// The cursor is inside the virtual display, away from the edge.
	c.k.disarmed = false
}

func (c *core) recvInput(l *link, m *proto.Msg) {
	k := &c.k
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.controlled {
		return
	}
	ev := *m.Input
	lastSeq := l.motion.lastSeq.Load()
	if ev.Kind == plat.MouseMove && m.Seq < lastSeq {
		return // a newer OOB move overtook this idle synchronization
	}
	if m.Seq > lastSeq {
		l.motion.lastSeq.Store(m.Seq)
	}
	c.injectLocked(plat.Event{Kind: plat.MouseMove, X: m.X, Y: m.Y})
	if ev.Kind == plat.MouseMove {
		return // final position at rest, already injected above
	}
	switch ev.Kind {
	case plat.Key:
		if ev.Down {
			k.heldKeys[ev.HID] = struct{}{}
		} else {
			delete(k.heldKeys, ev.HID)
		}
	case plat.MouseButton:
		if ev.Down {
			k.heldBtns[ev.Button] = struct{}{}
		} else {
			delete(k.heldBtns, ev.Button)
		}
	}
	c.injectLocked(ev)
}

// recvLeave: the peer took its pointer back.
func (c *core) recvLeave(l *link) {
	k := &c.k
	k.mu.Lock()
	defer k.mu.Unlock()
	k.controlled = false
	l.motion.latest.Store(nil)
	k.shadowUntil.Store(0)
	c.releaseHeldLocked()
	if k.controlling {
		c.returnLocalLocked(c.homePointLocked())
	}
}

// lostPeer resets both halves after the link went away.
func (c *core) lostPeer() {
	k := &c.k
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.controlling {
		c.returnLocalLocked(c.homePointLocked())
	}
	k.controlled = false
	k.shadowing, k.shPending = false, false
	k.shadowUntil.Store(0)
	c.releaseHeldLocked()
}

func (c *core) releaseHeldLocked() {
	k := &c.k
	for hid := range k.heldKeys {
		c.injectLocked(plat.Event{Kind: plat.Key, HID: hid})
	}
	for b := range k.heldBtns {
		c.injectLocked(plat.Event{Kind: plat.MouseButton, Button: b})
	}
	clear(k.heldKeys)
	clear(k.heldBtns)
}

func (c *core) injectLocked(ev plat.Event) {
	if err := c.p.Inject(ev); err != nil {
		log.Printf("kvm: inject: %v", err)
	}
}

var errControlling = errors.New("kvm: controlling the peer; not injecting")

// InjectForPeer implements Session.
func (c *core) InjectForPeer(ev plat.Event) error {
	k := &c.k
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.controlling {
		return errControlling
	}
	k.spanInputAt = time.Now()
	if ev.Kind == plat.MouseMove {
		k.spanX, k.spanY, k.spanSet = ev.X, ev.Y, true
	}
	return c.p.Inject(ev)
}

func opposite(e proto.Edge) proto.Edge {
	switch e {
	case proto.EdgeLeft:
		return proto.EdgeRight
	case proto.EdgeRight:
		return proto.EdgeLeft
	case proto.EdgeTop:
		return proto.EdgeBottom
	case proto.EdgeBottom:
		return proto.EdgeTop
	}
	return e
}

func round32(v float64) int32 { return int32(math.Round(v)) }
