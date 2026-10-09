package shell

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestExtractWrappedEdit(t *testing.T) {
	begin, end, missing := "__B__", "__E__", "__M__"
	payload := "hello\nworld\n"

	okOut := begin + "\n" + base64.StdEncoding.EncodeToString([]byte(payload)) + "\n" + end
	if got, errMsg := extractWrappedEdit(okOut, begin, end, missing); errMsg != "" || got != payload {
		t.Fatalf("success case: got=%q err=%q", got, errMsg)
	}

	// Content that looks like an error must be preserved verbatim.
	looksLikeError := "Error: pretend\n"
	errOut := begin + "\n" + base64.StdEncoding.EncodeToString([]byte(looksLikeError)) + "\n" + end
	if got, errMsg := extractWrappedEdit(errOut, begin, end, missing); errMsg != "" || got != looksLikeError {
		t.Fatalf("error-looking content: got=%q err=%q", got, errMsg)
	}

	if _, errMsg := extractWrappedEdit(missing, begin, end, missing); errMsg == "" {
		t.Fatal("expected a missing-file error")
	}
	if _, errMsg := extractWrappedEdit(begin+"\nAAAA", begin, end, missing); errMsg == "" {
		t.Fatal("expected a malformed-response error")
	}
	if _, errMsg := extractWrappedEdit(begin+"\n!!!!\n"+end, begin, end, missing); errMsg == "" {
		t.Fatal("expected a decode error")
	}
}

func TestApplyStringEditOverlappingIsAmbiguous(t *testing.T) {
	// "aa" occurs twice (overlapping) in "aaa"; must be rejected.
	if _, errMsg := applyStringEdit("aaa", "aa", "b"); errMsg == "" {
		t.Fatal("expected an error for an overlapping non-unique old_text")
	}
}

func TestStreamingCommandBuilders(t *testing.T) {
	size, err := fileSizeCmd("Linux", "/tmp/f")
	if err != nil || size != "wc -c < '/tmp/f'" {
		t.Fatalf("fileSizeCmd = %q, %v", size, err)
	}
	rc, err := readChunkCmd("Linux", "/tmp/f", 32768, 4096)
	if err != nil || rc != "tail -c +32769 '/tmp/f' | head -c 4096 | base64" {
		t.Fatalf("readChunkCmd = %q, %v", rc, err)
	}
	ac, err := appendBase64Cmd("Linux", "/tmp/f", "AAAA")
	if err != nil || ac != "printf '%s' 'AAAA' | base64 -d >> '/tmp/f'" {
		t.Fatalf("appendBase64Cmd = %q, %v", ac, err)
	}
	tc, err := truncateCreateCmd("Linux", "/tmp/f")
	if err != nil || tc != "mkdir -p \"$(dirname '/tmp/f')\"; : > '/tmp/f'" {
		t.Fatalf("truncateCreateCmd = %q, %v", tc, err)
	}
}

func TestWindowsRangeReadUsesLazyReadLines(t *testing.T) {
	cmd := windowsRangeBase64Cmd("C:\\f.txt", 10, 20)
	if !strings.Contains(cmd, "[IO.File]::ReadLines(") || !strings.Contains(cmd, "$i -ge 10") {
		t.Fatalf("windowsRangeBase64Cmd does not use lazy ReadLines: %q", cmd)
	}
}

func TestRenameCmdRejectsPathSeparators(t *testing.T) {
	for _, name := range []string{"", "a/b", `a\b`, "../x"} {
		if _, err := renameCmd("Linux", "/tmp/x", name); err == nil {
			t.Fatalf("renameCmd accepted unsafe new_name %q", name)
		}
	}
	if _, err := renameCmd("Linux", "/tmp/x", "y"); err != nil {
		t.Fatalf("renameCmd rejected a valid name: %v", err)
	}
}

func TestApplyStringEditUniqueReplace(t *testing.T) {
	// A multi-line replacement must not gain a spurious trailing newline
	// (the old line-number implementation appended "\n" and duplicated lines).
	content := "a\nb\nc\n"
	out, errMsg := applyStringEdit(content, "b\n", "x\ny\n")
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if out != "a\nx\ny\nc\n" {
		t.Fatalf("got %q, want %q", out, "a\nx\ny\nc\n")
	}
}

func TestApplyStringEditDelete(t *testing.T) {
	out, errMsg := applyStringEdit("keep\ndrop\nkeep2\n", "drop\n", "")
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if out != "keep\nkeep2\n" {
		t.Fatalf("got %q, want %q", out, "keep\nkeep2\n")
	}
}

func TestApplyStringEditNotFound(t *testing.T) {
	if _, errMsg := applyStringEdit("a\nb\n", "zzz", "y"); errMsg == "" {
		t.Fatal("expected an error for a missing old_text")
	}
}

func TestApplyStringEditAmbiguous(t *testing.T) {
	if _, errMsg := applyStringEdit("x\nx\n", "x\n", "y\n"); errMsg == "" {
		t.Fatal("expected an error for a non-unique old_text")
	}
}

func TestApplyStringEditEmptyOldText(t *testing.T) {
	if _, errMsg := applyStringEdit("a\n", "", "y"); errMsg == "" {
		t.Fatal("expected an error for an empty old_text")
	}
}

// A CRLF file must still match an LF old_text and keep CRLF on write.
func TestApplyStringEditCRLFTolerant(t *testing.T) {
	out, errMsg := applyStringEdit("a\r\nb\r\nc\r\n", "b\n", "x\n")
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if out != "a\r\nx\r\nc\r\n" {
		t.Fatalf("got %q, want %q", out, "a\r\nx\r\nc\r\n")
	}
}

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
