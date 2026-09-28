//go:build !windows

package main

// On non-Windows platforms child output is byte-transparent (UTF-8), so no
// code-page decoding is needed.
func decodeConsole(b []byte) []byte { return b }
