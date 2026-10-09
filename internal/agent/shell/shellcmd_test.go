package shell

import (
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf16"
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
	linux := target{os: "Linux", env: EnvBash}
	size, err := fileSizeCmd(linux, "/tmp/f")
	if err != nil || size != "wc -c < '/tmp/f'" {
		t.Fatalf("fileSizeCmd = %q, %v", size, err)
	}
	rc, err := readChunkCmd(linux, "/tmp/f", 32768, 4096)
	if err != nil || rc != "tail -c +32769 '/tmp/f' | head -c 4096 | base64" {
		t.Fatalf("readChunkCmd = %q, %v", rc, err)
	}
	ac, err := appendBase64Cmd(linux, "/tmp/f", "AAAA")
	if err != nil || ac != "printf '%s' 'AAAA' | base64 -d >> '/tmp/f'" {
		t.Fatalf("appendBase64Cmd = %q, %v", ac, err)
	}
	tc, err := truncateCreateCmd(linux, "/tmp/f")
	if err != nil || tc != "mkdir -p \"$(dirname '/tmp/f')\"; : > '/tmp/f'" {
		t.Fatalf("truncateCreateCmd = %q, %v", tc, err)
	}
}

// A PowerShell host runs the script directly (no launcher, so '$' is evaluated
// exactly once); a cmd host receives it base64-encoded via -EncodedCommand so
// cmd.exe quoting and PowerShell expansion cannot corrupt it.
func TestWindowsEnvWrappers(t *testing.T) {
	ps := target{os: "Windows", env: EnvPowerShell}
	cmd, err := readFileCmd(ps, `C:\a$b.txt`, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "[IO.File]::OpenRead(") {
		t.Fatalf("PowerShell read command missing OpenRead: %q", cmd)
	}
	if strings.HasPrefix(cmd, "powershell ") {
		t.Fatalf("PowerShell host must not spawn a child launcher: %q", cmd)
	}

	cmdT := target{os: "Windows", env: EnvCmd}
	encCmd, err := readFileCmd(cmdT, `C:\a$b.txt`, "", "")
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "powershell -NoProfile -EncodedCommand "
	if !strings.HasPrefix(encCmd, prefix) {
		t.Fatalf("cmd host must use -EncodedCommand: %q", encCmd)
	}
	u16, derr := base64.StdEncoding.DecodeString(strings.TrimPrefix(encCmd, prefix))
	if derr != nil {
		t.Fatalf("encoded command is not valid base64: %v", derr)
	}
	script := decodeUTF16LE(t, u16)
	if !strings.Contains(script, "[IO.File]::OpenRead(") {
		t.Fatalf("decoded script missing OpenRead: %q", script)
	}
	// A literal '$' stays inside a single-quoted PowerShell string, so cmd and
	// PowerShell hosts both keep it verbatim.
	if !strings.Contains(script, "'C:\\a$b.txt'") {
		t.Fatalf("path with '$' not single-quoted: %q", script)
	}
}

// sliceLines must preserve original line terminators (unlike a ReadLines +
// AppendLine round trip, which rewrites LF files to CRLF).
func TestSliceLinesPreservesTerminators(t *testing.T) {
	if got := sliceLines("a\nb\nc", "2", "0"); got != "b\nc" {
		t.Fatalf("LF range = %q, want %q", got, "b\nc")
	}
	if got := sliceLines("a\r\nb\r\nc\r\n", "1", "2"); got != "a\r\nb\r\n" {
		t.Fatalf("CRLF range = %q, want %q", got, "a\r\nb\r\n")
	}
	if got := sliceLines("a\nb\nc\n", "2", "3"); got != "b\nc\n" {
		t.Fatalf("full range = %q, want %q", got, "b\nc\n")
	}
	if got := sliceLines("only", "2", "0"); !strings.HasPrefix(got, "Start line 2 exceeds") {
		t.Fatalf("out-of-range should report an error, got %q", got)
	}
}

func decodeUTF16LE(t *testing.T, b []byte) string {
	t.Helper()
	if len(b)%2 != 0 {
		t.Fatalf("odd UTF-16LE byte length: %d", len(b))
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u))
}

func TestFormatListingWindows(t *testing.T) {
	header := "PSIsContainer Length Name\n------------- ------ ----"
	if got := formatListing(header); got != "Empty directory" {
		t.Fatalf("header-only Windows listing = %q, want %q", got, "Empty directory")
	}
	table := header + "\n        False   12 a.txt\n         True        docs"
	got := formatListing(table)
	if !strings.Contains(got, "FILE 12 a.txt") || !strings.Contains(got, "DIR 0 docs") {
		t.Fatalf("Windows listing = %q", got)
	}
}

func TestDeleteCmdKindEnforced(t *testing.T) {
	linux := target{os: "Linux", env: EnvBash}
	if c, _ := deleteCmd(linux, "/tmp/d", false); !strings.Contains(c, "-d '/tmp/d'") || !strings.Contains(c, "rm -f '/tmp/d'") {
		t.Fatalf("delete_file command = %q", c)
	}
	if c, _ := deleteCmd(linux, "/tmp/f", true); !strings.Contains(c, "-f '/tmp/f'") || !strings.Contains(c, "rm -rf '/tmp/f'") {
		t.Fatalf("delete_dir command = %q", c)
	}
	win := target{os: "Windows", env: EnvCmd}
	c, _ := deleteCmd(win, `C:\x`, false)
	const prefix = "powershell -NoProfile -EncodedCommand "
	if !strings.HasPrefix(c, prefix) {
		t.Fatalf("windows delete_file command not encoded: %q", c)
	}
	u16, derr := base64.StdEncoding.DecodeString(strings.TrimPrefix(c, prefix))
	if derr != nil {
		t.Fatal(derr)
	}
	if !strings.Contains(decodeUTF16LE(t, u16), "PathType Container") {
		t.Fatalf("windows delete_file missing kind check: %q", decodeUTF16LE(t, u16))
	}
}

func TestRenameCmdRejectsPathSeparators(t *testing.T) {
	linux := target{os: "Linux", env: EnvBash}
	for _, name := range []string{"", "a/b", `a\b`, "../x"} {
		if _, err := renameCmd(linux, "/tmp/x", name); err == nil {
			t.Fatalf("renameCmd accepted unsafe new_name %q", name)
		}
	}
	if _, err := renameCmd(linux, "/tmp/x", "y"); err != nil {
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
