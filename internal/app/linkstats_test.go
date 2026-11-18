package app

import (
	"testing"
	"time"
)

func TestLinkRates(t *testing.T) {
	prev := linkCounters{OutSegs: 1000, RetransSegs: 10, LostSegs: 5, InBytes: 1 << 20, OutBytes: 2 << 20}
	cur := linkCounters{OutSegs: 1200, RetransSegs: 18, LostSegs: 6, InBytes: 1<<20 + 10240, OutBytes: 2<<20 + 5120}
	re, lo, in, out := linkRates(prev, cur, 5*time.Second)
	if re != 4 || lo != 0.5 || in != 2 || out != 1 {
		t.Errorf("rates = retrans %v%% lost %v%% in %vKB/s out %vKB/s, want 4 0.5 2 1", re, lo, in, out)
	}
	// Nothing sent: no division by zero.
	if re, lo, _, _ := linkRates(cur, cur, time.Second); re != 0 || lo != 0 {
		t.Errorf("idle rates %v %v", re, lo)
	}
	// Counters reset (went backwards): zero, not a wrapped huge value.
	if re, _, in, _ := linkRates(cur, prev, time.Second); re != 0 || in != 0 {
		t.Errorf("reset rates %v %v", re, in)
	}
	if _, _, in, _ := linkRates(prev, cur, 0); in != 0 {
		t.Errorf("zero interval in %v", in)
	}
}

func TestLinkAppRTT(t *testing.T) {
	old := pingInterval
	pingInterval = 50 * time.Millisecond
	t.Cleanup(func() { pingInterval = old })

	a, b := pair(t, "windows", "darwin", false, testSecret)
	connected(t, a, b)
	eventually(t, "app RTT measured", func() bool {
		st := a.c.LinkStats()
		return st.Connected && st.AppRTTms > 0 && st.AppRTTMaxms >= st.AppRTTms
	})
	b.stop(t)
	eventually(t, "stats cleared", func() bool { return !a.c.LinkStats().Connected })
}
