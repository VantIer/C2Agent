package main

// Cross-platform shell command execution with a timeout.

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const forbiddenPattern = "rm -rf /"

func runCmd(command string, timeout time.Duration) string {
	if strings.TrimSpace(command) == "" {
		return "Error: Empty command"
	}
	if strings.Contains(strings.ToLower(command), forbiddenPattern) {
		return "Error: Command blocked due to safety concerns"
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("Error: Command timed out after %d seconds", int(timeout.Seconds()))
	}
	text := strings.TrimRight(string(out), "\r\n")
	if err != nil && text == "" {
		return fmt.Sprintf("Error: %v", err)
	}
	if text == "" {
		return "Command executed successfully (no output)"
	}
	return text
}
