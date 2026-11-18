package app

import (
	"context"

	"magic/internal/plat"
	"magic/internal/proto"
)

// Session is what the core (app.go) exposes to the window-sharing feature
// (share.go/viewer.go/panel.go). The core owns the connection; features
// only send through it and react to lifecycle callbacks.
type Session interface {
	// Send to the current peer; error if not connected.
	Send(m *proto.Msg) error
	// Peer returns the peer's latest Hello, or nil when disconnected.
	Peer() *proto.Hello
	// MapKey applies the Cmd/Ctrl swap when enabled and OSes differ.
	MapKey(hid uint16) uint16
	// Virtual is this machine's current virtual display (zero value when
	// disconnected). Real reports whether windows can span into it.
	Virtual() proto.Virtual
	// InjectForPeer injects input the peer aimed at one of our spanning
	// windows. The OS can echo such pointer moves as unmarked events inside
	// the virtual display; the core ignores those instead of treating them
	// as the user crossing to the peer. While we control the peer our
	// cursor is detached for relative capture, so it refuses with
	// errControlling.
	InjectForPeer(ev plat.Event) error
	// Shadowed reports whether the peer currently shows its cursor on our
	// screen over its spanning windows; overlay input is then our own
	// injected cursor, not the user, and must be dropped.
	Shadowed() bool
	// LinkStats reports the quality of the connection to the peer.
	LinkStats() LinkStats
}

// LinkStats is the connection quality: the input (control) session's KCP
// smoothed RTT and the app-level ping round trip over it (average and
// maximum over the last 30 s), and over the last ping interval the share
// of all sent KCP segments retransmitted or lost and the bulk session's
// throughput each way.
type LinkStats struct {
	Connected             bool
	RTTms                 float64
	AppRTTms, AppRTTMaxms float64
	RetransPct, LostPct   float64
	InKBps, OutKBps       float64
}

// Feature is implemented by the window-spanning subsystem (type *Sharing,
// constructed by NewSharing(cfg, plat, session)). The core calls these;
// handlers run on the core's receive goroutine and must not block on I/O
// other than Session.Send.
type Feature interface {
	// Handle consumes spanning messages (TLayout, TWinClose, TFrame,
	// TFrameAck, TWinInput); false if not one.
	Handle(m *proto.Msg) bool
	// SpanAt reports the front-most of our windows spanning onto the
	// peer's screen at (x, y) in the peer's coordinates. It runs on the
	// input hook thread and must not block.
	SpanAt(x, y float64) (uint64, bool)
	// Connected is called after Hello exchange; Disconnected on loss.
	Connected()
	Disconnected()
	// ServePanel runs the loopback status page until ctx ends.
	ServePanel(ctx context.Context) error
}
