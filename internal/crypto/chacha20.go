// Package crypto implements the ChaCha20 stream cipher used to protect the
// native-agent connection after registration, plus a net.Conn wrapper that
// transparently encrypts writes and decrypts reads.
//
// Wire parameters (shared with remote-c / remote-py). Both the ChaCha20 key and
// the nonce are derived per connection from the fresh handshake nonce and the
// shared auth token, so every connection uses a one-time key+nonce pair:
//
//	key(dir)   = SHA-256(nonce || token || nonce || dir)          (32 bytes)
//	nonce(dir) = SHA-256(token || nonce || token || dir)[0:12]    (12 bytes)
//
// dir separates the two directions (see DirC2ToAgent / DirAgentToC2) so they
// never share a keystream.
//
// Semantics follow RFC 7539 (256-bit key, 96-bit nonce, 32-bit block counter).
package crypto

import (
	"crypto/sha256"
	"encoding/binary"
	"net"
)

// Direction bytes appended to the key/nonce derivation inputs so the two
// directions of a connection never share a ChaCha20 keystream.
const (
	DirC2ToAgent byte = 0x01 // C2 -> Agent
	DirAgentToC2 byte = 0x02 // Agent -> C2
)

// DeriveMaterial derives the per-connection ChaCha20 key and nonce for one
// direction from the shared auth token and the fresh handshake nonce:
//
//	key   = SHA-256(nonce || token || nonce || dir)          (32 bytes)
//	nonce = SHA-256(token || nonce || token || dir)[0:12]    (12 bytes)
//
// The handshake nonce is fresh per connection, so each connection gets a unique
// key+nonce pair; dir differs per direction, so the two directions never reuse
// a keystream.
func DeriveMaterial(authToken, handshakeNonce string, dir byte) (key [32]byte, nonce [12]byte) {
	keyHash := sha256.New()
	keyHash.Write([]byte(handshakeNonce))
	keyHash.Write([]byte(authToken))
	keyHash.Write([]byte(handshakeNonce))
	keyHash.Write([]byte{dir})
	copy(key[:], keyHash.Sum(nil))

	nonceHash := sha256.New()
	nonceHash.Write([]byte(authToken))
	nonceHash.Write([]byte(handshakeNonce))
	nonceHash.Write([]byte(authToken))
	nonceHash.Write([]byte{dir})
	sum := nonceHash.Sum(nil)
	copy(nonce[:], sum[:12])
	return key, nonce
}

// ChaCha20 is a stream cipher instance.
type ChaCha20 struct {
	state     [16]uint32
	keystream [64]byte
	pos       int
}

// NewChaCha20 creates a cipher with the given 32-byte key, 12-byte nonce and
// initial block counter. The fixed-size array parameters make an invalid
// key/nonce length impossible at compile time (no runtime panic).
func NewChaCha20(key [32]byte, nonce [12]byte, counter uint32) *ChaCha20 {
	c := &ChaCha20{}
	c.state[0] = 0x61707865
	c.state[1] = 0x3320646e
	c.state[2] = 0x79622d32
	c.state[3] = 0x6b206574
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

func quarterRound(a, b, c, d *uint32) {
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

func (c *ChaCha20) block() {
	w := c.state
	for i := 0; i < 10; i++ {
		quarterRound(&w[0], &w[4], &w[8], &w[12])
		quarterRound(&w[1], &w[5], &w[9], &w[13])
		quarterRound(&w[2], &w[6], &w[10], &w[14])
		quarterRound(&w[3], &w[7], &w[11], &w[15])
		quarterRound(&w[0], &w[5], &w[10], &w[15])
		quarterRound(&w[1], &w[6], &w[11], &w[12])
		quarterRound(&w[2], &w[7], &w[8], &w[13])
		quarterRound(&w[3], &w[4], &w[9], &w[14])
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

// XORKeyStream XORs src with the keystream into dst (len(dst) >= len(src)).
func (c *ChaCha20) XORKeyStream(dst, src []byte) {
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

// EncryptedConn wraps a net.Conn, encrypting all writes with tx and
// decrypting all reads with rx. Both are stream ciphers so arbitrary TCP
// segmentation is handled transparently.
type EncryptedConn struct {
	net.Conn
	tx *ChaCha20
	rx *ChaCha20
}

// NewEncryptedConn wraps conn with the given directional ciphers.
func NewEncryptedConn(conn net.Conn, tx, rx *ChaCha20) *EncryptedConn {
	return &EncryptedConn{Conn: conn, tx: tx, rx: rx}
}

// Write encrypts p and writes it.
func (e *EncryptedConn) Write(p []byte) (int, error) {
	enc := make([]byte, len(p))
	e.tx.XORKeyStream(enc, p)
	return e.Conn.Write(enc)
}

// Read reads and decrypts bytes.
func (e *EncryptedConn) Read(p []byte) (int, error) {
	n, err := e.Conn.Read(p)
	if n > 0 {
		e.rx.XORKeyStream(p[:n], p[:n])
	}
	return n, err
}
