//go:build !windows

package main

import (
	"context"
	"os/exec"
	"syscall"
)

// shellCmd runs command through the platform shell.
func shellCmd(ctx context.Context, command string) *exec.Cmd {
	c := exec.CommandContext(ctx, "sh", "-c", command)
	// Put the child in its own process group so killTree can kill the whole
	// tree (including background grandchildren).
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return c
}

// killTree kills the command's whole process group.
func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

// On non-Windows platforms child output is byte-transparent (UTF-8), so no
// code-page decoding is needed.
func decodeConsole(b []byte) []byte { return b }
