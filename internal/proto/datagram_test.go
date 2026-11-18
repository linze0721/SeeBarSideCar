package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
)

func TestDatagramRoundTrip(t *testing.T) {
	a, b, errA, errB := pair(t, "same-secret-123", "same-secret-123")
	if errA != nil || errB != nil {
		t.Fatal(errA, errB)
	}
	for _, peers := range [][2]*Conn{{a, b}, {b, a}} {
		sender, receiver := peers[0].Datagram(), peers[1].Datagram()
		for wantSeq := uint64(1); wantSeq <= 4; wantSeq++ {
			want := []byte{42, byte(wantSeq), 255}
			pkt := sender.Seal([]byte{0xfe}, want)
			if pkt[0] != 0xfe || binary.LittleEndian.Uint64(pkt[1:]) != wantSeq {
				t.Fatalf("bad prefix or counter: %x", pkt)
			}
			pkt = pkt[1:]
			if _, _, err := sender.Open(bytes.Clone(pkt)); !errors.Is(err, ErrAuth) {
				t.Fatalf("wrong direction: %v", err)
			}
			for _, at := range []int{0, 8, len(pkt) - 1} {
				bad := bytes.Clone(pkt)
				bad[at] ^= 0x80
				if _, _, err := receiver.Open(bad); !errors.Is(err, ErrAuth) {
					t.Fatalf("tampered byte %d: %v", at, err)
				}
			}
			seq, payload, err := receiver.Open(pkt)
			if err != nil || seq != wantSeq || !bytes.Equal(payload, want) {
				t.Fatalf("open = (%d, %x, %v), want (%d, %x, nil)", seq, payload, err, wantSeq, want)
			}
		}
	}
}

func TestDatagramConcurrentSeal(t *testing.T) {
	a, b, errA, errB := pair(t, "same-secret-123", "same-secret-123")
	if errA != nil || errB != nil {
		t.Fatal(errA, errB)
	}
	const n = 128
	pkts := make(chan []byte, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { pkts <- a.Datagram().Seal(nil, []byte{byte(i)}) })
	}
	wg.Wait()
	close(pkts)
	var seenSeq, seenPayload [n]bool
	for pkt := range pkts {
		seq, payload, err := b.Datagram().Open(pkt)
		if err != nil || seq < 1 || seq > n || seenSeq[seq-1] ||
			len(payload) != 1 || payload[0] >= n || seenPayload[payload[0]] {
			t.Fatalf("invalid or duplicate datagram: seq=%d payload=%x err=%v", seq, payload, err)
		}
		seenSeq[seq-1], seenPayload[payload[0]] = true, true
	}
}
