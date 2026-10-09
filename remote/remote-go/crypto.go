package main

// ChaCha20 stream cipher (RFC 7539: 256-bit key, 96-bit nonce, 32-bit counter)
// plus a net.Conn wrapper that transparently encrypts writes and decrypts
// reads. Both the key and the nonce are derived per connection from the fresh
// handshake nonce and the shared auth token (one-time key+nonce pair).

import (
	"crypto/sha256"
	"encoding/binary"
	"net"
)

// Direction bytes appended to the derivation inputs so the two directions of a
// connection never share a ChaCha20 keystream.
const (
	dirC2ToAgent byte = 0x01 // C2 -> Agent
	dirAgentToC2 byte = 0x02 // Agent -> C2
)

// deriveMaterial derives the per-connection ChaCha20 key and nonce for one
// direction from the shared auth token and the fresh handshake nonce:
//
//	key   = SHA-256(nonce || token || nonce || dir)          (32 bytes)
//	nonce = SHA-256(token || nonce || token || dir)[0:12]    (12 bytes)
func deriveMaterial(token, handshakeNonce string, dir byte) (key [32]byte, nonce [12]byte) {
	keyHash := sha256.New()
	keyHash.Write([]byte(handshakeNonce))
	keyHash.Write([]byte(token))
	keyHash.Write([]byte(handshakeNonce))
	keyHash.Write([]byte{dir})
	copy(key[:], keyHash.Sum(nil))

	nonceHash := sha256.New()
	nonceHash.Write([]byte(token))
	nonceHash.Write([]byte(handshakeNonce))
	nonceHash.Write([]byte(token))
	nonceHash.Write([]byte{dir})
	sum := nonceHash.Sum(nil)
	copy(nonce[:], sum[:12])
	return key, nonce
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	const hexc = "0123456789abcdef"
	out := make([]byte, 64)
	for i, b := range sum {
		out[i*2] = hexc[b>>4]
		out[i*2+1] = hexc[b&0xf]
	}
	return string(out)
}

type chacha20 struct {
	state     [16]uint32
	keystream [64]byte
	pos       int
}

func newChaCha20(key, nonce []byte, counter uint32) *chacha20 {
	if len(key) != 32 || len(nonce) != 12 {
		panic("chacha20: bad key/nonce size")
	}
	c := &chacha20{}
	c.state[0], c.state[1], c.state[2], c.state[3] = 0x61707865, 0x3320646e, 0x79622d32, 0x6b206574
	for i := 0; i < 8; i++ {
		c.state[4+i] = binary.LittleEndian.Uint32(key[i*4:])
	}
	c.state[12] = counter
	c.state[13] = binary.LittleEndian.Uint32(nonce[0:])
	c.state[14] = binary.LittleEndian.Uint32(nonce[4:])
	c.state[15] = binary.LittleEndian.Uint32(nonce[8:])
	return c
}

func rotl(x uint32, n uint) uint32 { return (x << n) | (x >> (32 - n)) }

func qr(a, b, c, d *uint32) {
	*a += *b
	*d ^= *a
	*d = rotl(*d, 16)
	*c += *d
	*b ^= *c
	*b = rotl(*b, 12)
	*a += *b
	*d ^= *a
	*d = rotl(*d, 8)
	*c += *d
	*b ^= *c
	*b = rotl(*b, 7)
}

func (c *chacha20) block() {
	w := c.state
	for i := 0; i < 10; i++ {
		qr(&w[0], &w[4], &w[8], &w[12])
		qr(&w[1], &w[5], &w[9], &w[13])
		qr(&w[2], &w[6], &w[10], &w[14])
		qr(&w[3], &w[7], &w[11], &w[15])
		qr(&w[0], &w[5], &w[10], &w[15])
		qr(&w[1], &w[6], &w[11], &w[12])
		qr(&w[2], &w[7], &w[8], &w[13])
		qr(&w[3], &w[4], &w[9], &w[14])
	}
	for i := 0; i < 16; i++ {
		binary.LittleEndian.PutUint32(c.keystream[i*4:], w[i]+c.state[i])
	}
	c.state[12]++
	if c.state[12] == 0 {
		c.state[13]++
	}
	c.pos = 0
}

func (c *chacha20) xorKeyStream(dst, src []byte) {
	n := len(src)
	off := 0
	for off < n {
		if c.pos == 0 || c.pos >= 64 {
			c.block()
		}
		take := 64 - c.pos
		if n-off < take {
			take = n - off
		}
		for i := 0; i < take; i++ {
			dst[off+i] = src[off+i] ^ c.keystream[c.pos+i]
		}
		c.pos += take
		off += take
	}
}

// encryptedConn wraps a net.Conn with stream encryption.
type encryptedConn struct {
	net.Conn
	tx *chacha20
	rx *chacha20
}

func newEncryptedConn(conn net.Conn, tx, rx *chacha20) *encryptedConn {
	return &encryptedConn{Conn: conn, tx: tx, rx: rx}
}

func (e *encryptedConn) Write(p []byte) (int, error) {
	enc := make([]byte, len(p))
	e.tx.xorKeyStream(enc, p)
	return e.Conn.Write(enc)
}

func (e *encryptedConn) Read(p []byte) (int, error) {
	n, err := e.Conn.Read(p)
	if n > 0 {
		e.rx.xorKeyStream(p[:n], p[:n])
	}
	return n, err
}
