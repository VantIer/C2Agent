//go:build windows

package shell

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runPowerShell(t *testing.T, script string) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping PowerShell integration test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("powershell timed out\n%s", out)
	}
	if err != nil {
		t.Fatalf("powershell: %v\n%s", err, out)
	}
	return out
}

func TestPSReadLineRangePreservesBytes(t *testing.T) {
	if _, err := exec.LookPath("powershell"); err != nil {
		t.Skip("powershell not available")
	}
	dir := t.TempDir()
	cases := []struct {
		name    string
		content string
		start   int
		end     int
		want    string
	}{
		{"crlf", "l1\r\nl2\r\nl3\r\nl4", 2, 3, "l2\r\nl3\r\n"},
		{"lf", "a\nb\nc\n", 1, 2, "a\nb\n"},
		{"last-line-no-newline", "a\nb\nc", 2, 3, "b\nc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, tc.name+".txt")
			if err := os.WriteFile(p, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			script, ferr := (target{os: "Windows", env: EnvPowerShell}).finish(psReadLineRange(p, tc.start, tc.end, 8<<20))
			if ferr != nil {
				t.Fatal(ferr)
			}
			out := runPowerShell(t, script)
			got, perr := extractWrappedRange(string(out), winReadBegin, winReadEnd, winReadMissing,
				winRangeMissing, winRangeTooBig)
			if perr != "" {
				t.Fatalf("parse error: %s\noutput:\n%s", perr, out)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPSReadLineRangePastEOF(t *testing.T) {
	if _, err := exec.LookPath("powershell"); err != nil {
		t.Skip("powershell not available")
	}
	p := filepath.Join(t.TempDir(), "small.txt")
	if err := os.WriteFile(p, []byte("only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script, ferr := (target{os: "Windows", env: EnvPowerShell}).finish(psReadLineRange(p, 10, 12, 8<<20))
	if ferr != nil {
		t.Fatal(ferr)
	}
	out := runPowerShell(t, script)
	_, perr := extractWrappedRange(string(out), winReadBegin, winReadEnd, winReadMissing,
		winRangeMissing, winRangeTooBig)
	if !strings.Contains(perr, "exceeds") {
		t.Fatalf("expected range error, got %q (output %q)", perr, out)
	}
}
