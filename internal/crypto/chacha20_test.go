package crypto

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex: %v", err)
	}
	return b
}

// RFC 7539 section 2.3.2 keystream vector.
func TestChaCha20KeystreamRFC7539(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	nonce := mustHex(t, "000000090000004a00000000")
	want := mustHex(t,
		"10f1e7e4d13b5915500fdd1fa32071c4"+
			"c7d1f4c733c068030422aa9ac3d46c4e"+
			"d2826446079faa0914c2d705d98b02a2"+
			"b5129cd1de164eb9cbd083e8a2503c4e")

	c := NewChaCha20(key, nonce, 1)
	got := make([]byte, 64)
	c.XORKeyStream(got, make([]byte, 64))
	if !bytes.Equal(got, want) {
		t.Fatalf("keystream mismatch\n got %x\nwant %x", got, want)
	}
}

// RFC 7539 section 2.4.2 encryption vector.
func TestChaCha20EncryptRFC7539(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	nonce := mustHex(t, "000000000000004a00000000")
	plaintext := []byte("Ladies and Gentlemen of the class of '99: If I could offer you only one tip for the future, sunscreen would be it.")
	want := mustHex(t,
		"6e2e359a2568f98041ba0728dd0d6981"+
			"e97e7aec1d4360c20a27afccfd9fae0b"+
			"f91b65c5524733ab8f593dabcd62b357"+
			"1639d624e65152ab8f530c359f0861d8"+
			"07ca0dbf500d6a6156a38e088a22b65e"+
			"52bc514d16ccf806818ce91ab7793736"+
			"5af90bbf74a35be6b40b8eedf2785e42"+
			"874d")

	c := NewChaCha20(key, nonce, 1)
	got := make([]byte, len(plaintext))
	c.XORKeyStream(got, plaintext)
	if !bytes.Equal(got, want) {
		t.Fatalf("ciphertext mismatch\n got %x\nwant %x", got, want)
	}
}

// Crypt is an involution: applying twice restores the plaintext, including
// across partial-block boundaries.
func TestChaCha20RoundTripAcrossBlocks(t *testing.T) {
	key, nonce := DeriveMaterial("change-me-shared-token", "0123456789abcdef", DirC2ToAgent)
	tx := NewChaCha20(key[:], nonce[:], 0)
	rx := NewChaCha20(key[:], nonce[:], 0)

	in := make([]byte, 5000)
	for i := range in {
		in[i] = byte(i * 7)
	}
	enc := make([]byte, len(in))
	tx.XORKeyStream(enc, in)

	// Decrypt in irregular chunks to simulate TCP segmentation.
	out := make([]byte, len(in))
	step := 37
	pos := 0
	for pos < len(enc) {
		end := pos + step
		if end > len(enc) {
			end = len(enc)
		}
		rx.XORKeyStream(out[pos:end], enc[pos:end])
		pos = end
	}
	if !bytes.Equal(in, out) {
		t.Fatal("round-trip mismatch across block boundary")
	}
}

// DeriveMaterial must be deterministic for the same inputs, distinct per
// direction, and distinct per handshake nonce (one-time key+nonce).
func TestDeriveMaterial(t *testing.T) {
	token := "change-me-shared-token"
	nonce := "0123456789abcdef"

	k1, n1 := DeriveMaterial(token, nonce, DirC2ToAgent)
	k2, n2 := DeriveMaterial(token, nonce, DirC2ToAgent)
	if k1 != k2 || n1 != n2 {
		t.Fatal("DeriveMaterial is not deterministic")
	}

	ktx, ntx := DeriveMaterial(token, nonce, DirC2ToAgent)
	krx, nrx := DeriveMaterial(token, nonce, DirAgentToC2)
	if ktx == krx || ntx == nrx {
		t.Fatal("directions must not share a key or nonce")
	}

	k3, n3 := DeriveMaterial(token, "fedcba9876543210", DirC2ToAgent)
	if k1 == k3 || n1 == n3 {
		t.Fatal("different handshake nonces must yield different key/nonce")
	}
}

// Cross-language interop vectors: these exact values are also produced by the
// Python (remote/common/crypto.py) and C (remote/remote-c/protocol.c)
// implementations for the same inputs; any drift breaks agent interop.
func TestDeriveMaterialInteropVector(t *testing.T) {
	token := "change-me-shared-token"
	nonce := "0123456789abcdef"
	cases := []struct {
		dir   byte
		key   string
		nonce string
	}{
		{DirC2ToAgent,
			"ea42424fcb14a1a3395d2f013866be2c1c8ab812b8ecd29790f6bda98c4a3dbe",
			"e6d636597a46b43b686f7aa7"},
		{DirAgentToC2,
			"a652964193b67a6d0d2b0787e116c2f05133a1987fb5536df1c4c1f87a64f2ad",
			"5d88656aeb8a77320e65609f"},
	}
	for _, c := range cases {
		k, n := DeriveMaterial(token, nonce, c.dir)
		if hex.EncodeToString(k[:]) != c.key {
			t.Errorf("dir %#x key mismatch\n got %s\nwant %s", c.dir, hex.EncodeToString(k[:]), c.key)
		}
		if hex.EncodeToString(n[:]) != c.nonce {
			t.Errorf("dir %#x nonce mismatch\n got %s\nwant %s", c.dir, hex.EncodeToString(n[:]), c.nonce)
		}
	}
}
