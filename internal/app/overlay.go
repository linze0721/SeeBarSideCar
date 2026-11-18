package app

import (
	"bytes"
	"errors"
	"image"
	"image/draw"
	"image/jpeg"
	"log"
	"sync"
	"time"

	"magic/internal/plat"
	"magic/internal/proto"
)

// maxFrameSide bounds a remote frame so a bogus Frame cannot make us
// allocate an absurd buffer.
const maxFrameSide = 16384

// remoteView is one peer window spanning onto our screen. It exists while
// the peer's layout lists the window; the overlay is created on the first
// frame (no blank flash). rv.mu is never held across plat.Overlay calls:
// those may synchronize with the UI thread, which calls back into
// input/userClosed.
type remoteView struct {
	sh *Sharing
	id uint64

	buf *image.RGBA // frame buffer; only touched by the receive goroutine

	mu     sync.Mutex
	ov     plat.Overlay
	rect   plat.Rect // latest layout rect, our global coordinates
	title  string
	closed bool
	// input coalescing (plat UI thread + timer)
	move     plat.Event // latest MouseMove not yet sent
	pending  bool
	lastMove time.Time
	timer    *time.Timer
}

func spanTitle(e proto.SpanWindow) string { return e.App + " — " + e.Title }

// handleLayout reconciles the overlays with the peer's spanning windows.
func (sh *Sharing) handleLayout(layout []proto.SpanWindow) {
	type update struct {
		rv *remoteView
		e  proto.SpanWindow
	}
	var upd []update
	listed := make(map[uint64]bool, len(layout))
	sh.mu.Lock()
	for _, e := range layout {
		listed[e.ID] = true
		if sh.dismissed[e.ID] {
			continue
		}
		if rv := sh.views[e.ID]; rv != nil {
			upd = append(upd, update{rv, e})
		} else {
			sh.views[e.ID] = &remoteView{sh: sh, id: e.ID, rect: e.Rect, title: spanTitle(e)}
		}
	}
	for id := range sh.dismissed {
		if !listed[id] {
			delete(sh.dismissed, id)
		}
	}
	var gone []*remoteView
	for id, rv := range sh.views {
		if !listed[id] {
			delete(sh.views, id)
			gone = append(gone, rv)
		}
	}
	sh.mu.Unlock()
	for _, u := range upd {
		u.rv.setLayout(u.e.Rect, spanTitle(u.e))
	}
	for _, rv := range gone {
		rv.close()
	}
}

// setLayout moves/renames the overlay when the layout entry changed.
func (rv *remoteView) setLayout(rect plat.Rect, title string) {
	rv.mu.Lock()
	moved, renamed := rect != rv.rect, title != rv.title
	rv.rect, rv.title = rect, title
	ov := rv.ov
	if rv.closed {
		ov = nil
	}
	rv.mu.Unlock()
	if ov == nil {
		return // the overlay picks both up when it is created
	}
	if moved {
		ov.SetBounds(rect)
	}
	if renamed {
		ov.SetTitle(title)
	}
}

// dismiss forgets rv and tells the host to mute its window, which keeps
// it out of later layouts until it leaves the virtual display.
func (sh *Sharing) dismiss(rv *remoteView) {
	sh.mu.Lock()
	cur := sh.views[rv.id] == rv
	if cur {
		delete(sh.views, rv.id)
		sh.dismissed[rv.id] = true
	}
	sh.mu.Unlock()
	if cur {
		sh.send(&proto.Msg{Type: proto.TWinClose, WinID: rv.id})
	}
}

func (sh *Sharing) handleFrame(id uint64, f *proto.Frame) {
	sh.mu.Lock()
	rv := sh.views[id]
	sh.mu.Unlock()
	if rv == nil {
		return // stale frame of a window no longer in the layout
	}
	if err := rv.apply(f); err != nil {
		log.Printf("share: frame for window %#x: %v; closing", id, err)
		rv.close()
		sh.dismiss(rv)
		return
	}
	sh.send(&proto.Msg{Type: proto.TFrameAck, WinID: id, Seq: f.N})
}

// apply draws f into the buffer and shows it.
func (rv *remoteView) apply(f *proto.Frame) error {
	if f.Full {
		if f.W <= 0 || f.H <= 0 || f.W > maxFrameSide || f.H > maxFrameSide {
			return errors.New("bad frame size")
		}
		rv.buf = image.NewRGBA(image.Rect(0, 0, int(f.W), int(f.H)))
	}
	if rv.buf == nil {
		return nil // deltas before the first full frame: nothing to apply to
	}
	for _, t := range f.Tiles {
		img, err := jpeg.Decode(bytes.NewReader(t.JPEG))
		if err != nil {
			return err
		}
		b := img.Bounds()
		dst := image.Rect(int(t.X), int(t.Y), int(t.X)+b.Dx(), int(t.Y)+b.Dy())
		draw.Draw(rv.buf, dst, img, b.Min, draw.Src)
	}

	rv.mu.Lock()
	if rv.closed {
		rv.mu.Unlock()
		return nil
	}
	ov, rect, title := rv.ov, rv.rect, rv.title
	rv.mu.Unlock()

	if ov == nil {
		nov, err := rv.sh.p.NewOverlay(title, rect, rv.input, rv.userClosed)
		if err != nil {
			return err
		}
		rv.mu.Lock()
		closed := rv.closed
		if !closed {
			rv.ov = nov
		}
		rv.mu.Unlock()
		if closed {
			nov.Close()
			return nil
		}
		ov = nov
	}
	ov.SetFrame(rv.buf)
	return nil
}

// close closes the overlay programmatically (no closed callback).
func (rv *remoteView) close() {
	rv.mu.Lock()
	if rv.closed {
		rv.mu.Unlock()
		return
	}
	rv.closed = true
	if rv.timer != nil {
		rv.timer.Stop()
	}
	ov := rv.ov
	rv.mu.Unlock()
	if ov != nil {
		ov.Close()
	}
}

// userClosed is the plat.Overlay closed callback.
func (rv *remoteView) userClosed() {
	rv.mu.Lock()
	already := rv.closed
	rv.closed = true
	if rv.timer != nil {
		rv.timer.Stop()
	}
	rv.mu.Unlock()
	if !already {
		rv.sh.dismiss(rv)
	}
}

// input is the plat.Overlay input callback; MouseMove X,Y are our global
// coordinates, which the host maps through its virtual display.
func (rv *remoteView) input(ev plat.Event) {
	rv.mu.Lock()
	if rv.closed {
		rv.mu.Unlock()
		return
	}
	if rv.sh.s.Shadowed() {
		// The peer's own cursor is shown over its window; this input is
		// that injected cursor, not the user.
		rv.mu.Unlock()
		return
	}
	if ev.Kind == plat.MouseMove {
		rv.move = ev
		if wait := shareMoveInterval - time.Since(rv.lastMove); wait > 0 {
			if !rv.pending {
				rv.pending = true
				rv.timer = time.AfterFunc(wait, rv.flushMove)
			}
			rv.mu.Unlock()
			return
		}
		m := rv.moveMsgLocked()
		rv.mu.Unlock()
		rv.sh.send(m)
		return
	}
	if ev.Kind == plat.Key {
		ev.HID = rv.sh.s.MapKey(ev.HID)
	}
	var move *proto.Msg
	if rv.pending {
		rv.timer.Stop()
		move = rv.moveMsgLocked()
	}
	rv.mu.Unlock()
	if move != nil {
		rv.sh.send(move)
	}
	rv.sh.send(&proto.Msg{Type: proto.TWinInput, WinID: rv.id, Input: &ev})
}

func (rv *remoteView) flushMove() {
	rv.mu.Lock()
	if !rv.pending || rv.closed {
		rv.mu.Unlock()
		return
	}
	m := rv.moveMsgLocked()
	rv.mu.Unlock()
	rv.sh.send(m)
}

func (rv *remoteView) moveMsgLocked() *proto.Msg {
	rv.pending = false
	rv.lastMove = time.Now()
	ev := rv.move
	return &proto.Msg{Type: proto.TWinInput, WinID: rv.id, Input: &ev}
}
