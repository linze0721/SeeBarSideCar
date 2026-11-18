package kcp

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"slices"
	"testing"
	"time"
)

// Use the PacketConn interface to observe actual UDP writes, including on Linux.
type forkPacketConn struct {
	net.PacketConn
	beforeWrite func([]byte)
}

func (c forkPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.beforeWrite(p)
	return c.PacketConn.WriteTo(p, addr)
}

func forkLoopback(t *testing.T, block BlockCrypt, beforeClientWrite func([]byte)) (client, server *UDPSession) {
	t.Helper()
	listener, err := ListenWithOptions("127.0.0.1:0", block, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if beforeClientWrite != nil {
		conn = forkPacketConn{conn, beforeClientWrite}
	}
	// Exercise the full conversation ID range, including the upstream FEC OOB seqid.
	client, err = NewConn3(0xffffffff, listener.Addr(), block, 0, 0, conn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	deadline := time.Now().Add(5 * time.Second)
	listener.SetDeadline(deadline)
	client.SetDeadline(deadline)
	client.SetNoDelay(1, 10, 2, 1)
	if _, err := client.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	server, err = listener.AcceptKCP()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	server.SetDeadline(deadline)
	server.SetNoDelay(1, 10, 2, 1)
	var first [1]byte
	if _, err := io.ReadFull(server, first[:]); err != nil || first[0] != 1 {
		t.Fatalf("bootstrap: got %x, %v", first, err)
	}
	return client, server
}

func TestForkOOBWithoutFEC(t *testing.T) {
	for _, tc := range []struct {
		name     string
		newCrypt func([]byte) (BlockCrypt, error)
		maxSize  int
	}{
		{"plain", nil, 1194},
		{"CFB", NewAESBlockCrypt, 1174},
		{"AEAD", NewAESGCMCrypt, 1166},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var block BlockCrypt
			if tc.newCrypt != nil {
				var err error
				block, err = tc.newCrypt(make([]byte, 16))
				if err != nil {
					t.Fatal(err)
				}
			}
			client, server := forkLoopback(t, block, nil)
			clientOOB, serverOOB := make(chan []byte, 3), make(chan []byte, 3)
			for i, sess := range []*UDPSession{client, server} {
				if !sess.SetMtu(1200) || sess.GetOOBMaxSize() != tc.maxSize {
					t.Fatalf("want usable MTU 1200 with OOB max %d, got max %d", tc.maxSize, sess.GetOOBMaxSize())
				}
				ch := []chan []byte{clientOOB, serverOOB}[i]
				if err := sess.SetOOBHandler(func(p []byte) {
					select {
					case ch <- bytes.Clone(p):
					default:
					}
				}); err != nil {
					t.Fatal(err)
				}
				if err := sess.SendOOB(make([]byte, tc.maxSize+1)); err == nil {
					t.Fatal("accepted oversized OOB payload")
				}
			}

			stream := bytes.Repeat([]byte("reliable KCP stream\x00"), 1024)
			streamDone := make(chan error, 1)
			go func() {
				for p := stream; len(p) > 0; {
					end := min(len(p), 512)
					if _, err := client.Write(p[:end]); err != nil {
						streamDone <- err
						return
					}
					p = p[end:]
				}
				streamDone <- nil
			}()
			packets := [][]byte{nil, {0x5a}, bytes.Repeat([]byte{0xf3}, tc.maxSize)}
			for _, packet := range packets {
				for _, sess := range []*UDPSession{client, server} {
					if err := sess.SendOOB(packet); err != nil {
						t.Fatal(err)
					}
				}
			}
			got := make([]byte, len(stream))
			if _, err := io.ReadFull(server, got); err != nil || !bytes.Equal(got, stream) {
				t.Fatalf("reliable stream was not delivered intact: %v", err)
			}
			if err := <-streamDone; err != nil {
				t.Fatal(err)
			}
			for i, ch := range []chan []byte{clientOOB, serverOOB} {
				missing := slices.Clone(packets)
				for range packets {
					select {
					case p := <-ch:
						j := slices.IndexFunc(missing, func(want []byte) bool { return bytes.Equal(p, want) })
						if j < 0 {
							t.Fatalf("peer %d received unexpected OOB payload of length %d", i, len(p))
						}
						missing = slices.Delete(missing, j, j+1)
					case <-time.After(time.Second):
						t.Fatalf("peer %d missing %d OOB packets", i, len(missing))
					}
				}
			}
		})
	}
}

func TestForkDupPushDatagrams(t *testing.T) {
	sent := make(chan []byte, 16)
	client, server := forkLoopback(t, nil, func(p []byte) {
		select {
		case sent <- bytes.Clone(p):
		default:
		}
	})
	pop := func() []byte {
		t.Helper()
		select {
		case p := <-sent:
			if len(p) < IKCP_OVERHEAD {
				t.Fatalf("unexpected short KCP datagram: %x", p)
			}
			return p
		case <-time.After(time.Second):
			t.Fatal("missing KCP datagram")
			return nil
		}
	}
	pop()                  // Bootstrap PUSH, sent before duplication is enabled.
	client.SetMinRTO(2000) // A retransmission must not look like a duplicate.
	server.SetMinRTO(2000)
	client.SetDupPush(1)
	client.SetACKNoDelay(true)
	server.SetACKNoDelay(true)

	if _, err := client.Write([]byte("p")); err != nil {
		t.Fatal(err)
	}
	original, duplicate := pop(), pop()
	if original[4] != IKCP_CMD_PUSH || !bytes.Equal(original, duplicate) {
		t.Fatalf("expected two identical PUSH datagrams; got %x and %x", original, duplicate)
	}
	var one [1]byte
	if _, err := io.ReadFull(server, one[:]); err != nil || one[0] != 'p' {
		t.Fatalf("read duplicated PUSH: %q, %v", one, err)
	}
	if _, err := server.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if ack := pop(); len(ack) != IKCP_OVERHEAD || ack[4] != IKCP_CMD_ACK {
		t.Fatalf("expected one pure ACK datagram, got %x", ack)
	}
	select {
	case p := <-sent:
		t.Fatalf("unexpected extra datagram: %x", p)
	case <-time.After(40 * time.Millisecond):
	}
}

func TestForkFlushMarksPackedPushDatagrams(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
		want []bool
	}{
		{"shared ACK and PUSH datagram", 1, []bool{true}},
		{"ACK in its own datagram", IKCP_MTU_DEF - IKCP_OVERHEAD + 1, []bool{false, true, true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []bool
			local := newKCP(1, func(_ []byte, _ int, hasPush bool) { got = append(got, hasPush) })
			local.NoDelay(1, 10, 0, 1)
			peer := NewKCP(1, func(buf []byte, size int) {
				if ret := local.Input(buf[:size], IKCP_PACKET_REGULAR, false); ret != 0 {
					t.Fatalf("input: %d", ret)
				}
			})
			peer.NoDelay(1, 10, 0, 1)
			peer.Send([]byte{1})
			peer.Update()
			local.Send(make([]byte, tc.size))
			local.Update()
			if !slices.Equal(got, tc.want) {
				t.Fatalf("datagram PUSH flags: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestForkMinRTOWithLoopbackSamples(t *testing.T) {
	for _, floorFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("floor_first=%t", floorFirst), func(t *testing.T) {
			client, server := forkLoopback(t, nil, func(p []byte) {
				if len(p) == IKCP_OVERHEAD && p[4] == IKCP_CMD_ACK {
					time.Sleep(time.Millisecond) // Ensure nonzero RTT samples at millisecond resolution.
				}
			})
			client.SetACKNoDelay(true)
			const floor = 15
			if floorFirst {
				server.SetMinRTO(floor)
			}
			server.SetNoDelay(1, 10, 2, 1)
			if !floorFirst {
				server.SetMinRTO(floor)
			}
			var one [1]byte
			reachedFloor := false
			for i := range 32 {
				if _, err := server.Write([]byte{byte(i)}); err != nil {
					t.Fatal(err)
				}
				if _, err := io.ReadFull(client, one[:]); err != nil || one[0] != byte(i) {
					t.Fatalf("read %d: got %x, %v", i, one, err)
				}
				time.Sleep(2 * time.Millisecond)
				if rto := server.GetRTO(); rto < floor {
					t.Fatalf("RTO %d fell below %d ms", rto, floor)
				} else if rto == floor {
					reachedFloor = true
				}
			}
			if !reachedFloor || server.GetSRTT() <= 0 {
				t.Fatalf("expected estimator to reach custom floor %d ms: RTO %d, SRTT %d", floor, server.GetRTO(), server.GetSRTT())
			}
		})
	}
}
