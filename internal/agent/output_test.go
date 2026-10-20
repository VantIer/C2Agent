package agent

import (
	"strings"
	"testing"
	"unicode/utf8"

	"c2agent/internal/limits"
)

func TestTruncateOutput(t *testing.T) {
	if got := TruncateOutput("short"); got != "short" {
		t.Fatalf("short output changed: %q", got)
	}

	long := strings.Repeat("a", limits.MaxOutputBytes+1024)
	got := TruncateOutput(long)
	if len(got) > limits.MaxOutputBytes {
		t.Fatalf("truncated output is %d bytes, want <= %d", len(got), limits.MaxOutputBytes)
	}
	if !strings.HasSuffix(got, "[... output truncated at 8 MiB]") {
		t.Fatal("missing truncation marker")
	}

	// The cut must keep valid UTF-8 when it lands inside a multi-byte rune.
	multi := strings.Repeat("界", limits.MaxOutputBytes/3+100)
	got = TruncateOutput(multi)
	if !utf8.ValidString(got) {
		t.Fatal("truncated output is not valid UTF-8")
	}
	if !strings.HasSuffix(got, "[... output truncated at 8 MiB]") {
		t.Fatal("missing truncation marker for UTF-8 output")
	}
}
