package app

import (
	"log"
	"sync"
	"time"

	"github.com/xtaci/kcp-go/v5"
)

const (
	statsWindow = 30 * time.Second // app RTT window and log period
)

// linkCounters is a snapshot of the counters the link quality is derived
// from: KCP's segment counters (process-wide, both sessions: kcp-go keeps
// no per-session ones) and the bytes through the bulk session.
type linkCounters struct {
	OutSegs, RetransSegs, LostSegs uint64
	InBytes, OutBytes              uint64 // bulk session
}

func readCounters(bulk *countConn) linkCounters {
	s := kcp.DefaultSnmp.Copy()
	lc := linkCounters{OutSegs: s.OutSegs, RetransSegs: s.RetransSegs, LostSegs: s.LostSegs}
	if bulk != nil {
		lc.InBytes, lc.OutBytes = bulk.in.Load(), bulk.out.Load()
	}
	return lc
}

// delta is cur-prev, or 0 if the counter went backwards (reset).
func delta(prev, cur uint64) uint64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}

func percent(num, den uint64) float64 {
	if den == 0 {
		return 0
	}
	return 100 * float64(num) / float64(den)
}

// linkRates turns two counter snapshots dt apart into retransmitted and
// lost segments as a percentage of segments sent, and bulk KB/s each way.
func linkRates(prev, cur linkCounters, dt time.Duration) (retransPct, lostPct, inKBps, outKBps float64) {
	out := delta(prev.OutSegs, cur.OutSegs)
	retransPct = percent(delta(prev.RetransSegs, cur.RetransSegs), out)
	lostPct = percent(delta(prev.LostSegs, cur.LostSegs), out)
	if s := dt.Seconds(); s > 0 {
		inKBps = float64(delta(prev.InBytes, cur.InBytes)) / 1024 / s
		outKBps = float64(delta(prev.OutBytes, cur.OutBytes)) / 1024 / s
	}
	return
}

type rttSample struct {
	at  time.Time
	rtt time.Duration
}

// linkMeter tracks the quality of the current link. tick runs on the
// writer's ping ticker; pong on the receive goroutine.
type linkMeter struct {
	mu       sync.Mutex
	sess     *kcp.UDPSession // control session; nil while disconnected
	bulk     *countConn
	prev     linkCounters
	prevAt   time.Time
	loggedAt time.Time
	rates    LinkStats   // retrans/lost/KBps of the last interval
	rtts     []rttSample // app-level ping round trips within statsWindow
}

// attach starts metering l.
func (m *linkMeter) attach(l *link) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sess = l.ctl.kcp
	m.bulk = l.bulk.nc
	m.prev, m.prevAt, m.loggedAt = readCounters(m.bulk), time.Now(), time.Now()
	m.rates = LinkStats{}
	m.rtts = m.rtts[:0]
}

func (m *linkMeter) detach() {
	m.mu.Lock()
	m.sess, m.bulk = nil, nil
	m.mu.Unlock()
}

// tick closes one measuring interval and logs the link quality every
// statsWindow.
func (m *linkMeter) tick() {
	now := time.Now()
	m.mu.Lock()
	if m.sess == nil {
		m.mu.Unlock()
		return
	}
	cur := readCounters(m.bulk)
	r := &m.rates
	r.RetransPct, r.LostPct, r.InKBps, r.OutKBps = linkRates(m.prev, cur, now.Sub(m.prevAt))
	m.prev, m.prevAt = cur, now
	logNow := now.Sub(m.loggedAt) >= statsWindow
	if logNow {
		m.loggedAt = now
	}
	m.mu.Unlock()
	if logNow {
		st := m.stats()
		log.Printf("link: input rtt %.1fms (app %.1fms, max %.0fms) retrans %.1f%% bulk out %.0fKB/s in %.0fKB/s",
			st.RTTms, st.AppRTTms, st.AppRTTMaxms, st.RetransPct, st.OutKBps, st.InKBps)
	}
}

// pong records the round trip of a ping sent at sent (UnixNano).
func (m *linkMeter) pong(sent int64) {
	now := time.Now()
	rtt := now.Sub(time.Unix(0, sent))
	if rtt < 0 || rtt > statsWindow {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sess == nil {
		return
	}
	m.pruneLocked(now)
	m.rtts = append(m.rtts, rttSample{at: now, rtt: rtt})
}

func (m *linkMeter) pruneLocked(now time.Time) {
	i := 0
	for i < len(m.rtts) && now.Sub(m.rtts[i].at) > statsWindow {
		i++
	}
	m.rtts = append(m.rtts[:0], m.rtts[i:]...)
}

// stats reports the current link quality; AppRTTms is the average app
// round trip over the last statsWindow, AppRTTMaxms its maximum.
func (m *linkMeter) stats() LinkStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sess == nil {
		return LinkStats{}
	}
	st := m.rates
	st.Connected = true
	st.RTTms = float64(m.sess.GetSRTT())
	m.pruneLocked(time.Now())
	var sum time.Duration
	for _, s := range m.rtts {
		sum += s.rtt
		st.AppRTTMaxms = max(st.AppRTTMaxms, ms(s.rtt))
	}
	if len(m.rtts) > 0 {
		st.AppRTTms = ms(sum / time.Duration(len(m.rtts)))
	}
	return st
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// LinkStats implements Session.
func (c *core) LinkStats() LinkStats { return c.meter.stats() }
