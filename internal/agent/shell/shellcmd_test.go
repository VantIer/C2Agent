package shell

import "testing"

// truncateRunes caps by Unicode code points, not bytes.
func TestTruncateRunesByCharacter(t *testing.T) {
	s := "你好世界" // 4 characters, 12 bytes
	if got := truncateRunes(s, 2); got != "你好" {
		t.Fatalf("truncateRunes(s,2) = %q, want %q", got, "你好")
	}
	if got := truncateRunes(s, 10); got != s {
		t.Fatalf("truncateRunes(s,10) = %q, want the whole string", got)
	}
	if got := truncateRunes(s, 0); got != "" {
		t.Fatalf("truncateRunes(s,0) = %q, want empty", got)
	}
}

// stripFileResponse removes only the framing: blank lines and prompt-looking
// content lines must survive for file reads.
func TestStripFileResponsePreservesContent(t *testing.T) {
	marker := "__C2AGENT_abc__"
	cmd := "cat 'f'"
	lines := []string{
		"user@host:~$ " + cmd, // echoed command line
		"",
		"#hash line",
		"content 1",
		"echo " + marker,
	}
	got := stripFileResponse(lines, marker, cmd)
	want := "\n#hash line\ncontent 1"
	if got != want {
		t.Fatalf("stripFileResponse = %q, want %q", got, want)
	}
}
