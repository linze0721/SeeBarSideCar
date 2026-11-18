package app

import (
	"encoding/binary"
	"sync/atomic"
	"time"

	"magic/internal/plat"
	"magic/internal/proto"
)

const (
	motionMove = 1
	motionSize = 13 // kind (u8), epoch (u32), x and y (i32), all little-endian
	motionIdle = 16 * time.Millisecond
)

func (c *core) queueMotionLocked() {
	k := &c.k
	interval := 4 * time.Millisecond
	// Use the control SRTT reported in LinkStats, without computing the
	// unrelated bulk rates and ping-window statistics on the hook thread.
	if len(k.txLink.in) == 0 && k.txLink.ctl.kcp.GetSRTT() < 25 {
		interval = 2 * time.Millisecond
	}
	now := time.Now()
	k.idleAt = now.Add(motionIdle)
	k.moveAt = k.lastMove.Add(interval)
	c.flushMotionLocked(now)
}

func (c *core) motionTimer() {
	c.k.mu.Lock()
	defer c.k.mu.Unlock()
	if c.k.controlling {
		c.flushMotionLocked(time.Now())
	}
}

func (c *core) flushMotionLocked(now time.Time) {
	k := &c.k
	if !k.moveAt.IsZero() && !now.Before(k.moveAt) {
		k.moveAt = time.Time{}
		k.lastMove = now
		p := k.txPayload[:]
		p[0] = motionMove
		binary.LittleEndian.PutUint32(p[1:], k.txEpoch)
		binary.LittleEndian.PutUint32(p[5:], uint32(round32(k.vx)))
		binary.LittleEndian.PutUint32(p[9:], uint32(round32(k.vy)))
		l := k.txLink
		pkt := l.ctl.pc.Datagram().Seal(k.txPacket[:0], p)
		if l.ctl.kcp.SendOOB(pkt) == nil {
			k.txFence = binary.LittleEndian.Uint64(pkt)
		}
	}
	if k.idleAt.IsZero() {
		return
	}
	if !now.Before(k.idleAt) {
		k.idleAt = time.Time{}
		c.sendInputLocked(plat.Event{Kind: plat.MouseMove})
		return
	}
	next := k.idleAt
	if !k.moveAt.IsZero() && k.moveAt.Before(next) {
		next = k.moveAt
	}
	if k.timer == nil {
		k.timer = time.AfterFunc(time.Until(next), c.motionTimer)
	} else {
		k.timer.Reset(time.Until(next))
	}
}

func (c *core) sendInputLocked(ev plat.Event) {
	k := &c.k
	// The reliable position supersedes any move not yet sent. The fence
	// refers only to packets sent, including packets lost in transit.
	k.moveAt = time.Time{}
	c.enqueueInput(k.txLink, &proto.Msg{Type: proto.TInput, Input: &ev,
		X: round32(k.vx), Y: round32(k.vy), Seq: k.txFence}, false)
}

type motionPosition struct {
	seq   uint64
	epoch uint32
	x, y  int32
}

type motionLane struct {
	latest  atomic.Pointer[motionPosition] // nil unless the peer is controlling
	lastSeq atomic.Uint64                  // advanced under k.mu by both injection paths
	wake    chan struct{}
}

// recvMotion runs on KCP's input path. Only the worker touches the platform
// or takes k.mu; the slot retains the highest sequence even after wakeup.
func (l *link) recvMotion(pkt []byte) {
	if len(pkt) != motionSize+proto.DatagramOverhead || l.motion.latest.Load() == nil {
		return
	}
	seq, p, err := l.ctl.pc.Datagram().Open(pkt)
	if err != nil || p[0] != motionMove {
		return
	}
	epoch := binary.LittleEndian.Uint32(p[1:])
	pos := motionPosition{seq: seq, epoch: epoch,
		x: int32(binary.LittleEndian.Uint32(p[5:])), y: int32(binary.LittleEndian.Uint32(p[9:]))}
	for {
		old := l.motion.latest.Load()
		if old == nil || old.epoch != epoch || seq <= old.seq || seq <= l.motion.lastSeq.Load() {
			return
		}
		if l.motion.latest.CompareAndSwap(old, &pos) {
			break
		}
	}
	select {
	case l.motion.wake <- struct{}{}:
	default:
	}
}

func (c *core) injectMotion(l *link) {
	k := &c.k
	for {
		select {
		case <-l.done:
			return
		case <-l.motion.wake:
		}
		k.mu.Lock()
		// Check again under the injection lock: a reliable fence or a
		// control transition may have superseded the mailbox notification.
		if pos := l.motion.latest.Load(); pos != nil && pos.seq > l.motion.lastSeq.Load() {
			l.motion.lastSeq.Store(pos.seq)
			c.injectLocked(plat.Event{Kind: plat.MouseMove, X: pos.x, Y: pos.y})
		}
		k.mu.Unlock()
	}
}
