package main

import (
	"encoding/hex"
	"testing"
)

// Cross-language interop vectors: the same inputs must yield the identical
// key/nonce in the Go control end, remote-py and remote-c.
func TestDeriveMaterialInteropVector(t *testing.T) {
	token := "change-me-shared-token"
	nonce := "0123456789abcdef"
	cases := []struct {
		dir   byte
		key   string
		nonce string
	}{
		{dirC2ToAgent,
			"ea42424fcb14a1a3395d2f013866be2c1c8ab812b8ecd29790f6bda98c4a3dbe",
			"e6d636597a46b43b686f7aa7"},
		{dirAgentToC2,
			"a652964193b67a6d0d2b0787e116c2f05133a1987fb5536df1c4c1f87a64f2ad",
			"5d88656aeb8a77320e65609f"},
	}
	for _, c := range cases {
		k, n := deriveMaterial(token, nonce, c.dir)
		if got := hex.EncodeToString(k[:]); got != c.key {
			t.Errorf("dir %#x key = %s, want %s", c.dir, got, c.key)
		}
		if got := hex.EncodeToString(n[:]); got != c.nonce {
			t.Errorf("dir %#x nonce = %s, want %s", c.dir, got, c.nonce)
		}
	}
}
