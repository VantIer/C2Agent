//go:build !windows

package main

import (
	"context"
	"os/exec"
)

// shellCmd runs command through the platform shell.
func shellCmd(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", command)
}

// On non-Windows platforms child output is byte-transparent (UTF-8), so no
// code-page decoding is needed.
func decodeConsole(b []byte) []byte { return b }
