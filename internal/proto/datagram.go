package proto

import (
	"crypto/cipher"
	"encoding/binary"
	"sync/atomic"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	datagramSeqSize  = 8
	DatagramOverhead = datagramSeqSize + chacha20poly1305.Overhead
)

// Datagram seals and opens independent packets with the link's directional
// datagram keys. It is safe for concurrent use and does not track replays.
type Datagram struct {
	write, read cipher.AEAD
	seq         atomic.Uint64
}

func (c *Conn) Datagram() *Datagram { return &c.datagram }

// Seal appends the little-endian counter and authenticated ciphertext to dst.
// The first counter is 1; a new handshake is required before it wraps.
func (d *Datagram) Seal(dst, payload []byte) []byte {
	var nonce [chacha20poly1305.NonceSize]byte
	for {
		seq := d.seq.Load()
		if seq == ^uint64(0) {
			panic("proto: datagram counter exhausted")
		}
		if d.seq.CompareAndSwap(seq, seq+1) {
			binary.LittleEndian.PutUint64(nonce[4:], seq+1)
			break
		}
	}
	dst = append(dst, nonce[4:]...)
	return d.write.Seal(dst, nonce[:], payload, nil)
}

// Open authenticates pkt and decrypts its payload in place. The returned
// payload aliases pkt; pkt must not be shared with another caller.
func (d *Datagram) Open(pkt []byte) (seq uint64, payload []byte, err error) {
	if len(pkt) < DatagramOverhead {
		return 0, nil, ErrAuth
	}
	var nonce [chacha20poly1305.NonceSize]byte
	copy(nonce[4:], pkt[:datagramSeqSize])
	seq = binary.LittleEndian.Uint64(nonce[4:])
	if seq == 0 {
		return 0, nil, ErrAuth
	}
	ct := pkt[datagramSeqSize:]
	payload, err = d.read.Open(ct[:0], nonce[:], ct, nil)
	if err != nil {
		return 0, nil, ErrAuth
	}
	return seq, payload, nil
}
