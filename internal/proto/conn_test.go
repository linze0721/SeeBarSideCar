package proto

import (
	"net"
	"strings"
	"testing"

	"magic/internal/plat"
)

func pair(t *testing.T, secretA, secretB string) (*Conn, *Conn, error, error) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	type res struct {
		c   *Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := Handshake(b, secretB, false)
		if err != nil {
			b.Close()
		}
		ch <- res{c, err}
	}()
	ca, errA := Handshake(a, secretA, true)
	if errA != nil {
		a.Close()
	}
	rb := <-ch
	return ca, rb.c, errA, rb.err
}

func TestHandshakeRoundTrip(t *testing.T) {
	ca, cb, errA, errB := pair(t, "same-secret-123", "same-secret-123")
	if errA != nil || errB != nil {
		t.Fatal(errA, errB)
	}
	// Larger than one AEAD record, both directions.
	big := strings.Repeat("x", 3*maxRecord+7)
	go ca.Send(&Msg{Type: TClipboard, Text: big})
	m, err := cb.Recv()
	if err != nil || m.Text != big {
		t.Fatalf("clipboard: err=%v len=%d", err, len(m.Text))
	}
	go cb.Send(&Msg{Type: TInput, Input: &plat.Event{Kind: plat.Key, HID: 4, Down: true}})
	m, err = ca.Recv()
	if err != nil || m.Input == nil || m.Input.HID != 4 || !m.Input.Down {
		t.Fatalf("input: %v %+v", err, m)
	}
}

func TestHandshakeWrongSecret(t *testing.T) {
	_, _, errA, errB := pair(t, "secret-one-123", "secret-two-456")
	if errA == nil || errB == nil {
		t.Fatalf("expected both sides to fail, got %v / %v", errA, errB)
	}
}
