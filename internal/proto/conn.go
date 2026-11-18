package proto

import (
	"bufio"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	magic      = "MGK1"
	maxRecord  = 1 << 16 // plaintext bytes per AEAD record
	nonceBytes = 32
)

var ErrAuth = errors.New("proto: authentication failed (secret mismatch?)")

// Conn is an authenticated, encrypted message stream. Both peers derive
// per-direction ChaCha20-Poly1305 keys from the shared secret and both
// handshake nonces; a wrong secret fails on the first record.
// Send is safe for concurrent use; Recv must be called from one goroutine.
type Conn struct {
	nc       net.Conn
	enc      *gob.Encoder
	dec      *gob.Decoder
	wmu      sync.Mutex
	w        *recordWriter
	datagram Datagram
}

// Handshake runs over nc. dialer must be true on exactly one side.
func Handshake(nc net.Conn, secret string, dialer bool) (*Conn, error) {
	_ = nc.SetDeadline(time.Now().Add(10 * time.Second))
	defer nc.SetDeadline(time.Time{})

	var mine [len(magic) + nonceBytes]byte
	copy(mine[:], magic)
	if _, err := rand.Read(mine[len(magic):]); err != nil {
		return nil, err
	}
	werr := make(chan error, 1)
	go func() { _, err := nc.Write(mine[:]); werr <- err }()
	var theirs [len(magic) + nonceBytes]byte
	if _, err := io.ReadFull(nc, theirs[:]); err != nil {
		return nil, err
	}
	if err := <-werr; err != nil {
		return nil, err
	}
	if string(theirs[:len(magic)]) != magic {
		return nil, fmt.Errorf("proto: bad magic %q", theirs[:len(magic)])
	}

	dn, ln := mine[len(magic):], theirs[len(magic):]
	if !dialer {
		dn, ln = ln, dn
	}
	salt := append(append([]byte{}, dn...), ln...)
	kdf := hkdf.New(sha256.New, []byte(secret), salt, []byte("magic v1"))
	var d2l, l2d [chacha20poly1305.KeySize]byte
	if _, err := io.ReadFull(kdf, d2l[:]); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(kdf, l2d[:]); err != nil {
		return nil, err
	}
	wk, rk := d2l, l2d
	if !dialer {
		wk, rk = l2d, d2l
	}
	wa, _ := chacha20poly1305.New(wk[:])
	ra, _ := chacha20poly1305.New(rk[:])

	w := &recordWriter{w: bufio.NewWriterSize(nc, maxRecord+64), aead: wa}
	r := &recordReader{r: bufio.NewReaderSize(nc, maxRecord+64), aead: ra}
	c := &Conn{nc: nc, w: w, enc: gob.NewEncoder(w), dec: gob.NewDecoder(r)}
	dkdf := hkdf.New(sha256.New, []byte(secret), salt, []byte("magic v1 dgram"))
	var dk [2 * chacha20poly1305.KeySize]byte
	if _, err := io.ReadFull(dkdf, dk[:]); err != nil {
		return nil, err
	}
	dw, _ := chacha20poly1305.New(dk[:chacha20poly1305.KeySize])
	dr, _ := chacha20poly1305.New(dk[chacha20poly1305.KeySize:])
	if !dialer {
		dw, dr = dr, dw
	}
	c.datagram.write, c.datagram.read = dw, dr

	// Prove the key before handing the conn out.
	go func() { werr <- c.Send(&Msg{Type: TPing}) }()
	m, err := c.Recv()
	if serr := <-werr; err == nil && serr != nil {
		err = serr
	}
	if err != nil {
		if errors.Is(err, ErrAuth) {
			return nil, ErrAuth
		}
		return nil, err
	}
	if m.Type != TPing {
		return nil, fmt.Errorf("proto: unexpected first message %d", m.Type)
	}
	return c, nil
}

func (c *Conn) Send(m *Msg) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.enc.Encode(m); err != nil {
		return err
	}
	return c.w.w.Flush()
}

func (c *Conn) Recv() (*Msg, error) {
	m := new(Msg)
	if err := c.dec.Decode(m); err != nil {
		return nil, err
	}
	return m, nil
}

func (c *Conn) Close() error         { return c.nc.Close() }
func (c *Conn) RemoteAddr() net.Addr { return c.nc.RemoteAddr() }

// Record format: u32 big-endian ciphertext length, ciphertext. Nonce is a
// per-direction counter, so records can't be replayed or reordered.

type recordWriter struct {
	w    *bufio.Writer
	aead cipher.AEAD
	ctr  uint64
	buf  []byte
}

func (rw *recordWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxRecord {
			chunk = chunk[:maxRecord]
		}
		var nonce [chacha20poly1305.NonceSize]byte
		binary.BigEndian.PutUint64(nonce[4:], rw.ctr)
		rw.ctr++
		rw.buf = rw.aead.Seal(rw.buf[:0], nonce[:], chunk, nil)
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(len(rw.buf)))
		if _, err := rw.w.Write(hdr[:]); err != nil {
			return n, err
		}
		if _, err := rw.w.Write(rw.buf); err != nil {
			return n, err
		}
		n += len(chunk)
		p = p[len(chunk):]
	}
	return n, nil
}

type recordReader struct {
	r    *bufio.Reader
	aead cipher.AEAD
	ctr  uint64
	ct   []byte
	pt   []byte // decrypted, unread remainder
}

func (rr *recordReader) Read(p []byte) (int, error) {
	for len(rr.pt) == 0 {
		var hdr [4]byte
		if _, err := io.ReadFull(rr.r, hdr[:]); err != nil {
			return 0, err
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n > maxRecord+uint32(rr.aead.Overhead()) {
			return 0, ErrAuth
		}
		if cap(rr.ct) < int(n) {
			rr.ct = make([]byte, n)
		}
		rr.ct = rr.ct[:n]
		if _, err := io.ReadFull(rr.r, rr.ct); err != nil {
			return 0, err
		}
		var nonce [chacha20poly1305.NonceSize]byte
		binary.BigEndian.PutUint64(nonce[4:], rr.ctr)
		rr.ctr++
		pt, err := rr.aead.Open(rr.ct[:0], nonce[:], rr.ct, nil)
		if err != nil {
			return 0, ErrAuth
		}
		rr.pt = pt
	}
	n := copy(p, rr.pt)
	rr.pt = rr.pt[n:]
	return n, nil
}
