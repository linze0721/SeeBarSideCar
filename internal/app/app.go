package app

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtaci/kcp-go/v5"

	"magic/internal/keymap"
	"magic/internal/plat"
	"magic/internal/proto"
)

const (
	dialMin       = time.Second
	dialMax       = 10 * time.Second
	helloTimeout  = 10 * time.Second
	readTimeout   = 15 * time.Second // liveness without TBye (crash, network loss)
	writeTimeout  = 15 * time.Second
	boundsPoll    = 2 * time.Second
	sendQueueSize = 256
	udpBuffer     = 4 << 20
	tokenSize     = 16
	// A closing side sends TBye (KCP has no FIN) and lingers so that KCP
	// can put it on the wire, retransmitting if needed, before the session
	// goes away; byeTimeout bounds the whole goodbye.
	byeLinger  = 100 * time.Millisecond
	byeTimeout = 500 * time.Millisecond
	// A link that answers no ping within probeTimeout loses to a newer
	// link from the same peer.
	probeTimeout = time.Second
)

// pingInterval paces pings and link quality measurements; bulkPairTimeout
// bounds how long a link's two sessions wait for each other. Variables
// for tests.
var (
	pingInterval    = 5 * time.Second
	bulkPairTimeout = 5 * time.Second
)

var errBye = errors.New("peer closed the link")

var errNotConnected = errors.New("app: not connected")

// core owns the peer connection, the KVM state machine and clipboard sync.
// It implements Session for the window-sharing Feature.
type core struct {
	cfg     *Config
	p       plat.Platform
	edge    proto.Edge
	localOS string
	f       Feature

	ln *kcp.Listener

	mu     sync.Mutex
	cur    *link
	peer   *proto.Hello
	local  plat.Rect      // union of mons
	mons   []plat.Monitor // real local monitors
	virt   proto.Virtual  // applied virtual display; owned by virtualLoop
	closed bool

	pairMu sync.Mutex
	pairs  map[string]*pairing // by link token: the other session of a link being set up

	skewLogged atomic.Int64 // peer protocol version+1 last logged as incompatible, 0 if none

	vdKick chan struct{} // asks virtualLoop to recompute

	k     kvm
	clip  clipState
	files fileSync
	meter linkMeter
}

var _ Session = (*core)(nil)

// link is one authenticated connection that completed the Hello exchange.
// It has two KCP sessions to the peer: control (ctl: Hello, input, pings
// and other small messages, tuned for latency) and bulk (window layouts and
// frames, file data and clipboard images, congestion controlled), so bulk
// bytes queued inside KCP never delay input.
type link struct {
	ctl, bulk *session
	hello     *proto.Hello    // the peer's first Hello
	dialedBy  string          // Name of the side that dialed
	in        chan *proto.Msg // hook-thread input; written ahead of out
	out       chan *proto.Msg // other control messages
	bulkQ     chan *proto.Msg // bulk messages
	done      chan struct{}   // closed when the link is closed
	gone      chan struct{}   // closed once both sessions are closed
	finished  chan struct{}   // closed after teardown completed
	recvd     chan struct{}   // gets a token whenever a control message arrives
	once      sync.Once
	motion    motionLane
}

// session is one authenticated KCP session; nc counts its bytes.
type session struct {
	nc  *countConn
	pc  *proto.Conn
	kcp *kcp.UDPSession
}

func (s *session) close() { s.nc.Close() }

// bulkType reports whether messages of type t go over the bulk session.
func bulkType(t proto.Type) bool {
	// A frame depends on its preceding layout; separate KCP sessions
	// cannot preserve that order.
	return t == proto.TLayout || t == proto.TFrame || t == proto.TFileData || t == proto.TClipImage
}

func (l *link) closeSessions() {
	l.ctl.close()
	l.bulk.close()
}

// close drops the link at once; the peer notices through its read
// deadline unless it is gone anyway.
func (l *link) close() {
	l.once.Do(func() {
		close(l.done)
		l.closeSessions()
		close(l.gone)
	})
}

// bye closes the link after telling the peer, which then disconnects
// right away. It does not block; gone closes when it is done.
func (l *link) bye() {
	l.once.Do(func() {
		close(l.done)
		go func() {
			defer close(l.gone)
			t := time.AfterFunc(byeTimeout, l.closeSessions)
			defer t.Stop()
			if l.ctl.pc.Send(&proto.Msg{Type: proto.TBye}) == nil {
				time.Sleep(byeLinger)
			}
			l.closeSessions()
		}()
	})
}

// countConn counts the bytes through a session.
type countConn struct {
	net.Conn
	in, out atomic.Uint64
}

func (cc *countConn) Read(b []byte) (int, error) {
	n, err := cc.Conn.Read(b)
	cc.in.Add(uint64(n))
	return n, err
}

func (cc *countConn) Write(b []byte) (int, error) {
	n, err := cc.Conn.Write(b)
	cc.out.Add(uint64(n))
	return n, err
}

// pairing is the half of a link set up first, waiting for the other: the
// listener's control-side serve waits for the bulk session with the same
// token.
type pairing struct {
	ch chan *session // the bulk session, delivered once
}

// newCore expects a normalized cfg.
func newCore(cfg *Config, p plat.Platform) *core {
	edge, _ := cfg.EdgeValue()
	c := &core{cfg: cfg, p: p, edge: edge, localOS: p.OS(), local: p.Bounds(), mons: p.Monitors(),
		vdKick: make(chan struct{}, 1), pairs: make(map[string]*pairing)}
	c.k.heldKeys = make(map[uint16]struct{})
	c.k.heldBtns = make(map[uint8]struct{})
	return c
}

// listen binds cfg.Listen (UDP); run calls it if the caller has not.
func (c *core) listen() (net.Addr, error) {
	ln, err := kcp.ListenWithOptions(c.cfg.Listen, nil, 0, 0)
	if err != nil {
		return nil, err
	}
	ln.SetReadBuffer(udpBuffer)
	ln.SetWriteBuffer(udpBuffer)
	c.ln = ln
	return ln.Addr(), nil
}

// tuneSession sets up a KCP session for input: fast resend, no congestion
// control, small window. The listener can't tell a control session from
// a bulk one before its handshake; tuneBulk retunes the latter.
func tuneSession(s *kcp.UDPSession) {
	s.SetStreamMode(true)
	s.SetNoDelay(1, 10, 2, 1)
	s.SetWindowSize(128, 128)
	s.SetMtu(1350)
	s.SetACKNoDelay(true)
	s.SetWriteDelay(false)
}

// Control sessions alone use aggressive retransmission and duplicate PUSHes.
func tuneControl(s *kcp.UDPSession) {
	s.SetMinRTO(15)
	s.SetDupPush(1)
}

// tuneBulk sets up a KCP session for throughput on a jittery (Wi-Fi) link:
// congestion control on, no fast resend, so late packets are not resent
// spuriously.
func tuneBulk(s *kcp.UDPSession) {
	s.SetStreamMode(true)
	s.SetNoDelay(0, 20, 0, 0)
	s.SetWindowSize(256, 256)
	s.SetMtu(1350)
	s.SetWriteDelay(false)
}

// run serves until ctx ends. f may be nil.
func (c *core) run(ctx context.Context, f Feature) error {
	c.f = f
	if c.ln == nil {
		if _, err := c.listen(); err != nil {
			return err
		}
	}
	if err := c.p.Capture(c.onEvent); err != nil {
		c.ln.Close()
		return err
	}
	log.Printf("listening on %s, peer %s", c.ln.Addr(), c.cfg.Peer)

	var wg sync.WaitGroup
	wg.Go(func() { c.acceptLoop(ctx, &wg) })
	wg.Go(func() { c.dialLoop(ctx) })
	wg.Go(func() { c.boundsLoop(ctx) })
	wg.Go(func() { c.virtualLoop(ctx) })
	if *c.cfg.Clipboard {
		wg.Go(func() { c.clipLoop(ctx) })
	}

	<-ctx.Done()
	c.mu.Lock()
	c.closed = true
	cur := c.cur
	c.mu.Unlock()
	if cur != nil {
		// Before the listener closes: an accepted link sends through it.
		cur.bye()
		<-cur.gone
	}
	c.ln.Close()
	wg.Wait()

	c.k.mu.Lock()
	if c.k.timer != nil {
		c.k.timer.Stop()
	}
	c.k.mu.Unlock()
	c.p.SetGrab(false)
	return nil
}

func (c *core) acceptLoop(ctx context.Context, wg *sync.WaitGroup) {
	for {
		s, err := c.ln.AcceptKCP()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("accept: %v", err)
			}
			return
		}
		tuneSession(s)
		wg.Go(func() { c.serveAccepted(ctx, s) })
	}
}

// dialLoop keeps a connection to cfg.Peer while none exists. Dialing UDP
// always succeeds; the handshake proves the peer is there.
func (c *core) dialLoop(ctx context.Context) {
	backoff := dialMin
	for {
		if l := c.link(); l != nil {
			select {
			case <-l.finished:
				backoff = dialMin
				continue
			case <-ctx.Done():
				return
			}
		}
		// A link that fails right away (e.g. the peer's bulk session
		// misbehaves) must not turn into a reconnect storm.
		if start := time.Now(); c.dialLink(ctx) && time.Since(start) >= dialMax {
			backoff = dialMin
		}
		if ctx.Err() != nil {
			return
		}
		if c.link() != nil {
			continue // an inbound link is up (or won the duplicate race)
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, dialMax)
	}
}

func dialSession(addr string) (*kcp.UDPSession, error) {
	s, err := kcp.DialWithOptions(addr, nil, 0, 0)
	if err != nil {
		return nil, err
	}
	s.SetReadBuffer(udpBuffer)
	s.SetWriteBuffer(udpBuffer)
	return s, nil
}

// handshake authenticates nc; the caller owns nc either way.
func (c *core) handshake(ctx context.Context, nc *kcp.UDPSession, dialer bool) (*session, error) {
	stop := context.AfterFunc(ctx, func() { nc.Close() })
	defer stop()
	cc := &countConn{Conn: nc}
	pc, err := proto.Handshake(cc, c.cfg.Secret, dialer)
	if err != nil {
		return nil, err
	}
	return &session{nc: cc, pc: pc, kcp: nc}, nil
}

// dialLink dials the control session, exchanges Hellos, then dials and
// pairs the bulk session, and serves the link. Reports whether the link
// was adopted.
func (c *core) dialLink(ctx context.Context) bool {
	s, err := dialSession(c.cfg.Peer)
	if err != nil {
		log.Printf("dial %s: %v", c.cfg.Peer, err)
		return false
	}
	tuneSession(s)
	tuneControl(s)
	ctl, err := c.handshake(ctx, s, true)
	if err != nil {
		log.Printf("handshake with %s: %v", s.RemoteAddr(), err)
		s.Close()
		return false
	}
	token := make([]byte, tokenSize)
	rand.Read(token)
	peer, err := c.exchangeHello(ctx, ctl, token)
	if err != nil {
		if !errors.Is(err, errIncompatible) {
			log.Printf("hello with %s: %v", s.RemoteAddr(), err)
		}
		ctl.close()
		return false
	}
	bulk, err := c.dialBulk(ctx, token)
	if err != nil {
		log.Printf("bulk session to %s: %v", c.cfg.Peer, err)
		ctl.close()
		return false
	}
	if err := awaitReady(ctx, ctl); err != nil {
		if ctx.Err() == nil && !errors.Is(err, errBye) {
			log.Printf("pairing with %s: %v", s.RemoteAddr(), err)
		}
		ctl.close()
		bulk.close()
		return false
	}
	return c.serveLink(ctx, ctl, bulk, peer, true)
}

// The bulk Hello only queues a token locally. Wait for the listener to
// pair and adopt both sessions before announcing a connected peer.
func awaitReady(ctx context.Context, ctl *session) error {
	stop := context.AfterFunc(ctx, ctl.close)
	defer stop()
	ctl.nc.SetReadDeadline(time.Now().Add(helloTimeout))
	defer ctl.nc.SetReadDeadline(time.Time{})
	m, err := ctl.pc.Recv()
	if err != nil {
		return err
	}
	switch m.Type {
	case proto.TReady:
		return nil
	case proto.TBye:
		return errBye
	default:
		return fmt.Errorf("expected paired link confirmation, got message type %d", m.Type)
	}
}

// dialBulk opens the bulk session of the link with token.
func (c *core) dialBulk(ctx context.Context, token []byte) (*session, error) {
	s, err := dialSession(c.cfg.Peer)
	if err != nil {
		return nil, err
	}
	tuneBulk(s)
	b, err := c.handshake(ctx, s, true)
	if err != nil {
		s.Close()
		return nil, err
	}
	s.SetWriteDeadline(time.Now().Add(helloTimeout))
	err = b.pc.Send(&proto.Msg{Type: proto.THello, Hello: &proto.Hello{Token: token}})
	s.SetWriteDeadline(time.Time{})
	if err != nil {
		b.close()
		return nil, err
	}
	return b, nil
}

// exchangeHello sends our Hello (with token when we dialed) and returns
// the peer's.
func (c *core) exchangeHello(ctx context.Context, s *session, token []byte) (*proto.Hello, error) {
	stop := context.AfterFunc(ctx, func() { s.close() })
	defer stop()
	s.nc.SetDeadline(time.Now().Add(helloTimeout))
	defer s.nc.SetDeadline(time.Time{})
	h := c.hello()
	h.Token = token
	if err := s.pc.Send(&proto.Msg{Type: proto.THello, Hello: h}); err != nil {
		return nil, err
	}
	m, err := s.pc.Recv()
	if err != nil {
		return nil, err
	}
	if m.Type != proto.THello || m.Hello == nil || m.Hello.Name == "" {
		return nil, errors.New("first message is not Hello")
	}
	if err := c.checkVersion(m.Hello); err != nil {
		return nil, err
	}
	if m.Hello.Name == c.cfg.Name {
		return nil, errors.New("peer uses our own name " + m.Hello.Name + "; names must differ")
	}
	return m.Hello, nil
}

var errIncompatible = errors.New("peer runs an incompatible version; update both machines")

// checkVersion refuses a peer Hello of another protocol version, logging
// that once per peer version rather than on every attempt.
func (c *core) checkVersion(h *proto.Hello) error {
	if h.Version == proto.Version {
		c.skewLogged.Store(0)
		return nil
	}
	if mark := int64(h.Version) + 1; c.skewLogged.Swap(mark) != mark {
		log.Printf("%s runs protocol version %d, we run %d: %v", h.Name, h.Version, proto.Version, errIncompatible)
	}
	return errIncompatible
}

// serveAccepted authenticates an inbound session. Its first message tells
// what it is: a full Hello opens a link (control session), a Hello with
// only a token is the bulk session of a link being set up.
func (c *core) serveAccepted(ctx context.Context, s *kcp.UDPSession) {
	ss, err := c.handshake(ctx, s, false)
	if err != nil {
		log.Printf("handshake with %s: %v", s.RemoteAddr(), err)
		s.Close()
		return
	}
	s.SetReadDeadline(time.Now().Add(helloTimeout))
	m, err := ss.pc.Recv()
	s.SetReadDeadline(time.Time{})
	if err == nil && (m.Type != proto.THello || m.Hello == nil) {
		err = errors.New("first message is not Hello")
	}
	if err != nil {
		log.Printf("hello with %s: %v", s.RemoteAddr(), err)
		ss.close()
		return
	}
	if m.Hello.Name == "" {
		tuneBulk(s)
		c.offerBulk(ss, m.Hello.Token)
		return
	}
	tuneControl(s)
	peer, err := c.replyHello(ctx, ss, m.Hello)
	if err != nil {
		if !errors.Is(err, errIncompatible) {
			log.Printf("hello with %s: %v", s.RemoteAddr(), err)
		}
		ss.close()
		return
	}
	bulk, err := c.awaitBulk(ctx, peer.Token)
	if err != nil {
		log.Printf("bulk session from %s: %v", s.RemoteAddr(), err)
		ss.close()
		return
	}
	c.serveLink(ctx, ss, bulk, peer, false)
}

// replyHello answers a dialer's Hello with ours. A peer of another
// version gets our Hello too, so it can tell why the link is refused.
func (c *core) replyHello(ctx context.Context, s *session, peer *proto.Hello) (*proto.Hello, error) {
	stop := context.AfterFunc(ctx, func() { s.close() })
	defer stop()
	s.nc.SetWriteDeadline(time.Now().Add(helloTimeout))
	defer s.nc.SetWriteDeadline(time.Time{})
	if err := c.checkVersion(peer); err != nil {
		if s.pc.Send(&proto.Msg{Type: proto.THello, Hello: c.hello()}) == nil {
			time.Sleep(byeLinger) // KCP has no FIN: let the reply go out before the close
		}
		return nil, err
	}
	if peer.Name == c.cfg.Name {
		return nil, errors.New("peer uses our own name " + peer.Name + "; names must differ")
	}
	if len(peer.Token) != tokenSize {
		return nil, errors.New("Hello without a link token")
	}
	return peer, s.pc.Send(&proto.Msg{Type: proto.THello, Hello: c.hello()})
}

func (c *core) pairingFor(token string) *pairing {
	c.pairMu.Lock()
	defer c.pairMu.Unlock()
	p := c.pairs[token]
	if p == nil {
		p = &pairing{ch: make(chan *session, 1)}
		c.pairs[token] = p
	}
	return p
}

// offerBulk hands a bulk session to the control session with the same
// token. A bulk session nobody claims within bulkPairTimeout is closed.
func (c *core) offerBulk(b *session, token []byte) {
	if len(token) != tokenSize {
		log.Printf("bulk session from %s without a link token", b.nc.RemoteAddr())
		b.close()
		return
	}
	key := string(token)
	p := c.pairingFor(key)
	select {
	case p.ch <- b:
	default:
		b.close() // a second bulk session for the token
		return
	}
	time.AfterFunc(bulkPairTimeout, func() {
		c.pairMu.Lock()
		if c.pairs[key] == p {
			delete(c.pairs, key)
		}
		c.pairMu.Unlock()
		select {
		case b := <-p.ch:
			log.Printf("bulk session from %s matches no link; closing", b.nc.RemoteAddr())
			b.close()
		default:
		}
	})
}

// awaitBulk waits for the bulk session with token.
func (c *core) awaitBulk(ctx context.Context, token []byte) (*session, error) {
	key := string(token)
	p := c.pairingFor(key)
	defer func() {
		c.pairMu.Lock()
		if c.pairs[key] == p {
			delete(c.pairs, key)
		}
		c.pairMu.Unlock()
	}()
	t := time.NewTimer(bulkPairTimeout)
	defer t.Stop()
	select {
	case b := <-p.ch:
		return b, nil
	case <-t.C:
		return nil, errors.New("no bulk session")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// serveLink adopts the link of ctl and bulk and, if it is adopted, runs it
// until it fails. Reports whether it was adopted.
func (c *core) serveLink(ctx context.Context, ctl, bulk *session, peer *proto.Hello, dialer bool) bool {
	l := &link{
		ctl:      ctl,
		bulk:     bulk,
		hello:    peer,
		dialedBy: peer.Name,
		in:       make(chan *proto.Msg, sendQueueSize),
		out:      make(chan *proto.Msg, sendQueueSize),
		bulkQ:    make(chan *proto.Msg, sendQueueSize),
		done:     make(chan struct{}),
		gone:     make(chan struct{}),
		finished: make(chan struct{}),
		recvd:    make(chan struct{}, 1),
	}
	l.motion.wake = make(chan struct{}, 1)
	if err := ctl.kcp.SetOOBHandler(l.recvMotion); err != nil {
		log.Printf("register control motion handler: %v", err)
		l.close()
		return false
	}
	if dialer {
		l.dialedBy = c.cfg.Name
	}
	if ctx.Err() != nil || !c.adopt(l) {
		l.bye()
		return false
	}
	log.Printf("connected to %s (%s) at %s", l.hello.Name, l.hello.OS, ctl.nc.RemoteAddr())

	c.meter.attach(l)
	var wg sync.WaitGroup
	wg.Go(func() { c.writeLoop(l, !dialer) })
	wg.Go(func() { c.bulkWriteLoop(l) })
	wg.Go(func() { c.injectMotion(l) })
	if c.f != nil {
		c.f.Connected()
	}
	errc := make(chan error, 2)
	wg.Go(func() { errc <- c.bulkReadLoop(l) })
	go func() { errc <- c.readLoop(l) }()
	err := <-errc
	l.close()
	<-errc
	wg.Wait()
	if ctx.Err() == nil {
		log.Printf("disconnected from %s: %v", l.hello.Name, err)
	}

	// l stays current until teardown is done so that a replacement link is
	// not adopted (and announced Connected) before Disconnected ran.
	c.meter.detach()
	c.mu.Lock()
	c.peer = nil
	c.mu.Unlock()
	c.kickVirtual()
	c.lostPeer()
	c.filesLost()
	if c.f != nil {
		c.f.Disconnected()
	}
	c.mu.Lock()
	c.cur = nil
	c.mu.Unlock()
	close(l.finished)
	return true
}

// adopt makes l the current link. Of two links between the same peers the
// one dialed by the lexicographically smaller Name wins, so both sides
// keep the same one; between equally ranked links the newer wins (a
// dialer only redials once its link died). A winning link that no longer
// answers a ping is from a peer process that is gone (restarted before
// its goodbye arrived): the newer link replaces it.
func (c *core) adopt(l *link) bool {
	smaller := min(c.cfg.Name, l.hello.Name)
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return false
		}
		cur := c.cur
		if cur == nil {
			c.cur, c.peer = l, l.hello
			c.mu.Unlock()
			c.kickVirtual()
			return true
		}
		c.mu.Unlock()
		if cur.dialedBy == smaller && l.dialedBy != smaller && c.alive(cur) {
			return false
		}
		cur.bye()
		<-cur.finished
	}
}

// alive pings l and reports whether anything arrived on it within
// probeTimeout.
func (c *core) alive(l *link) bool {
	select {
	case <-l.recvd:
	default:
	}
	if !c.enqueueInput(l, &proto.Msg{Type: proto.TPing, Seq: uint64(time.Now().UnixNano())}, false) {
		return false
	}
	t := time.NewTimer(probeTimeout)
	defer t.Stop()
	select {
	case <-l.recvd:
		return true
	case <-l.done:
		return false
	case <-t.C:
		log.Printf("link to %s does not answer; replacing it", l.hello.Name)
		return false
	}
}

// writeLoop sends l's control queues, input first, and every pingInterval
// a ping (liveness and app-level RTT) and a link quality measurement.
func (c *core) writeLoop(l *link, confirmPair bool) {
	if confirmPair {
		// First on the control stream: no input or Hello update may
		// overtake the pairing confirmation.
		l.ctl.nc.SetWriteDeadline(time.Now().Add(helloTimeout))
		if err := l.ctl.pc.Send(&proto.Msg{Type: proto.TReady}); err != nil {
			l.close()
			return
		}
	}
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		var m *proto.Msg
		select {
		case m = <-l.in:
		default:
		}
		if m == nil {
			select {
			case m = <-l.in:
			case m = <-l.out:
			case <-t.C:
				c.meter.tick()
				m = &proto.Msg{Type: proto.TPing, Seq: uint64(time.Now().UnixNano())}
			case <-l.done:
				return
			}
		}
		l.ctl.nc.SetWriteDeadline(time.Now().Add(writeTimeout))
		if err := l.ctl.pc.Send(m); err != nil {
			l.close()
			return
		}
	}
}

// bulkWriteLoop sends l's bulk queue.
func (c *core) bulkWriteLoop(l *link) {
	for {
		select {
		case m := <-l.bulkQ:
			l.bulk.nc.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := l.bulk.pc.Send(m); err != nil {
				l.close()
				return
			}
		case <-l.done:
			return
		}
	}
}

func (c *core) readLoop(l *link) error {
	for {
		l.ctl.nc.SetReadDeadline(time.Now().Add(readTimeout))
		m, err := l.ctl.pc.Recv()
		if err != nil {
			return err
		}
		if m.Type == proto.TBye {
			return errBye
		}
		select {
		case <-l.done:
			// We are saying goodbye; the session stays open only for TBye.
			return net.ErrClosed
		default:
		}
		select {
		case l.recvd <- struct{}{}:
		default:
		}
		c.dispatch(l, m)
	}
}

// bulkReadLoop dispatches bulk messages. The bulk session may idle for
// long, so its liveness is the control session's; it only ends with an
// error or when the link closes.
func (c *core) bulkReadLoop(l *link) error {
	for {
		m, err := l.bulk.pc.Recv()
		if err != nil {
			return fmt.Errorf("bulk session: %w", err)
		}
		if !bulkType(m.Type) {
			return errors.New("bulk session: unexpected message type")
		}
		select {
		case <-l.done:
			return net.ErrClosed
		default:
		}
		c.dispatch(l, m)
	}
}

func (c *core) dispatch(l *link, m *proto.Msg) {
	switch m.Type {
	case proto.TPing:
		if m.Seq != 0 {
			// Through the input queue: it never blocks the receiver and
			// goes out ahead of bulk data, so the RTT is the link's.
			c.enqueueInput(l, &proto.Msg{Type: proto.TPong, Seq: m.Seq}, true)
		}
	case proto.TPong:
		c.meter.pong(int64(m.Seq))
	case proto.TCursor:
		c.recvCursor(m.X, m.Y, m.Seq == 1)
	case proto.THello:
		if m.Hello == nil {
			return
		}
		c.mu.Lock()
		var moved bool
		if c.cur == l {
			moved = c.peer == nil || !slices.Equal(c.peer.Monitors, m.Hello.Monitors)
			c.peer = m.Hello
		}
		c.mu.Unlock()
		if moved {
			c.kickVirtual()
		}
	case proto.TEnter:
		c.recvEnter(l, m)
	case proto.TLeave:
		c.recvLeave(l)
	case proto.TInput:
		if m.Input != nil {
			c.recvInput(l, m)
		}
	case proto.TClipboard:
		if *c.cfg.Clipboard {
			c.recvClipboard(m.Text)
		}
	case proto.TClipImage:
		if *c.cfg.Clipboard {
			c.recvClipImage(m.Image, m.Text)
		}
	case proto.TFileOffer:
		if *c.cfg.Clipboard && c.cfg.FileClipMaxMB > 0 && m.Files != nil {
			c.recvFileOffer(m.Files)
		}
	case proto.TFileGet:
		if m.Chunk != nil {
			c.recvFileGet(m.Chunk)
		}
	case proto.TFileData:
		if m.Chunk != nil {
			c.recvFileData(m.Chunk)
		}
	default:
		if c.f == nil || !c.f.Handle(m) {
			log.Printf("unhandled message type %d", m.Type)
		}
	}
}

// boundsLoop re-announces our Hello and recomputes the virtual display
// when the display layout changes.
func (c *core) boundsLoop(ctx context.Context) {
	t := time.NewTicker(boundsPoll)
	defer t.Stop()
	var blank bool // the platform reports no monitors (display asleep)
	for {
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
		b, ms := c.p.Bounds(), c.p.Monitors()
		if len(ms) == 0 {
			// A sleeping display can drop out of the list while it still
			// exists; forgetting it would remove the virtual display (and
			// the peer's) until it wakes.
			if !blank {
				log.Printf("displays: none reported (display asleep?); keeping the last known layout")
			}
			blank = true
			continue
		}
		blank = false
		c.mu.Lock()
		changed := b != c.local || !slices.Equal(ms, c.mons)
		c.local, c.mons = b, ms
		c.mu.Unlock()
		if changed {
			log.Printf("displays changed: bounds %+v, %d monitors", b, len(ms))
			c.Send(&proto.Msg{Type: proto.THello, Hello: c.hello()})
			c.kickVirtual()
		}
	}
}

func (c *core) kickVirtual() {
	select {
	case c.vdKick <- struct{}{}:
	default:
	}
}

// virtualLoop owns the virtual display. When kicked it recomputes the
// display from the local and peer monitors (none while disconnected) and
// applies changes one at a time off the hook thread, since creating a
// display can take seconds. After a change, and once per new link (its
// first Hello may predate the display), it re-sends our Hello.
func (c *core) virtualLoop(ctx context.Context) {
	var req *plat.VirtualDisplay // last request, nil if none
	var announced *link
	var why string // last logged reason there is no virtual display
	for {
		select {
		case <-c.vdKick:
		case <-ctx.Done():
			return // Run removes the display
		}
		c.mu.Lock()
		l, peer, mons, cur := c.cur, c.peer, c.mons, c.virt
		c.mu.Unlock()

		var want *plat.VirtualDisplay
		var v proto.Virtual
		if peer != nil {
			if vd, of, ok := virtualFor(mons, peer.Monitors, c.edge); ok {
				want, v = &vd, proto.Virtual{Rect: vd.Rect, Of: of}
				why = ""
			} else if w := fmt.Sprintf("%d local and %d peer monitors", len(mons), len(peer.Monitors)); w != why {
				why = w
				log.Printf("virtual display: none possible with %s (a display asleep or off?)", w)
			}
		}
		switch {
		case want == nil && req == nil:
			v = cur
		case want != nil && req != nil && *want == *req:
			v.Rect, v.Real = cur.Rect, cur.Real
		default:
			v = c.applyVirtual(want, req != nil, v)
			req = want
		}
		if v == cur && (peer == nil || l == announced) {
			continue
		}
		c.mu.Lock()
		c.virt = v
		c.mu.Unlock()
		if peer != nil && c.Send(&proto.Msg{Type: proto.THello, Hello: c.hello()}) == nil {
			announced = l
		}
	}
}

// applyVirtual creates, moves or (want nil) removes the display. v is the
// phantom for want; it becomes real when the platform applied it.
func (c *core) applyVirtual(want *plat.VirtualDisplay, had bool, v proto.Virtual) proto.Virtual {
	if !*c.cfg.VirtualDisplay {
		return v
	}
	if want == nil {
		if had {
			if _, err := c.p.SetVirtualDisplay(nil); err != nil {
				log.Printf("virtual display: remove: %v", err)
			}
			log.Printf("virtual display removed")
		}
		return v
	}
	r, err := c.p.SetVirtualDisplay(want)
	if err != nil {
		log.Printf("virtual display %+v: %v; pointer crossing only, windows cannot span", want.Rect, err)
		return v
	}
	log.Printf("virtual display at %+v mirrors peer monitor %+v", r, v.Of)
	v.Rect, v.Real = r, true
	return v
}

func (c *core) hello() *proto.Hello {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &proto.Hello{Name: c.cfg.Name, OS: c.localOS, Bounds: c.local, Monitors: c.mons, Virtual: c.virt,
		Version: proto.Version}
}

func (c *core) link() *link {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

// enqueueInput hands a hook-thread message to l's writer without ever
// blocking. When the queue is full a droppable message (MouseMove) is
// discarded; losing any other input would leave keys stuck, so the link is
// dropped instead and the peer releases everything on disconnect.
func (c *core) enqueueInput(l *link, m *proto.Msg, droppable bool) bool {
	select {
	case <-l.done:
		return false
	default:
	}
	select {
	case l.in <- m:
		return true
	default:
	}
	if !droppable {
		log.Printf("send queue full; dropping connection to %s", l.hello.Name)
		l.close()
	}
	return false
}

// enqueue waits for room in the queue of l's session for m or for the
// link to close.
func (c *core) enqueue(l *link, m *proto.Msg) bool {
	q := l.out
	if bulkType(m.Type) {
		q = l.bulkQ
	}
	select {
	case <-l.done:
		return false
	default:
	}
	select {
	case q <- m:
		return true
	case <-l.done:
		return false
	}
}

// Send implements Session.
func (c *core) Send(m *proto.Msg) error {
	l := c.link()
	if l == nil || !c.enqueue(l, m) {
		return errNotConnected
	}
	return nil
}

// Peer implements Session.
func (c *core) Peer() *proto.Hello {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peer
}

// Virtual implements Session.
func (c *core) Virtual() proto.Virtual {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.peer == nil {
		return proto.Virtual{}
	}
	return c.virt
}

// MapKey implements Session.
func (c *core) MapKey(hid uint16) uint16 {
	if !*c.cfg.SwapCmdCtrl {
		return hid
	}
	if peer := c.Peer(); peer == nil || peer.OS == c.localOS {
		return hid
	}
	return keymap.SwapCmdCtrl(hid)
}
