package shell

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	dirFileRe = regexp.MustCompile(`^(DIR|FILE)\s+\d+\s+.+$`)
	lsLine    = regexp.MustCompile(`^([dl-])[^\s]*\s+\d+\s+\S+\s+\S+\s+(\d+)\s+\S+\s+\S+\s+\S+\s+(.+?)\s*$`)
)

// formatListing normalizes raw shell listing output into "DIR <size> <name>"
// lines, matching the native agent's list_dir output.
func formatListing(output string) string {
	lines := strings.Split(output, "\n")
	nonEmpty := 0
	allDirFile := true
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		nonEmpty++
		if !dirFileRe.MatchString(strings.TrimSpace(l)) {
			allDirFile = false
		}
	}
	if nonEmpty > 0 && allDirFile {
		var out []string
		for _, l := range lines {
			if s := strings.TrimSpace(l); s != "" {
				out = append(out, s)
			}
		}
		return strings.Join(out, "\n")
	}

	var items []string
	for _, l := range lines {
		l = strings.TrimRight(l, "\r")
		if strings.TrimSpace(l) == "" || strings.HasPrefix(l, "total") {
			continue
		}
		m := lsLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		name := m[3]
		if name == "." || name == ".." {
			continue
		}
		typ := "FILE"
		size := m[2]
		if m[1] == "d" {
			typ = "DIR"
			size = "0"
		}
		items = append(items, typ+" "+size+" "+name)
	}
	if len(items) > 0 {
		return strings.Join(items, "\n")
	}
	return output
}

// truncateRunes caps s at max characters (Unicode code points), never
// splitting a UTF-8 sequence, matching the Python agent's character semantics.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}

// oneLine collapses a command to a single line because the marker-based shell
// protocol frames responses per line; it replaces newlines with spaces, so
// multi-line constructs (here-docs, multi-line scripts) are NOT supported on
// shell bots.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

var (
	promptRe     = regexp.MustCompile(`^\s*(?:\S+\s+)?\S*[$#>]\s*$`)
	markerLineRe = regexp.MustCompile(`^__C2AGENT_.*__$`)
)

// stripShellResponse removes the echoed command, the marker-echo line and
// shell prompts from a raw reverse-shell reply.
func stripShellResponse(lines []string, marker, lastCommand string) string {
	markerEcho := "echo " + marker
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.Contains(l, markerEcho) || markerLineRe.MatchString(strings.TrimSpace(l)) {
			continue
		}
		out = append(out, l)
	}
	for len(out) > 0 {
		first := out[0]
		if strings.TrimSpace(first) == "" || (lastCommand != "" && strings.Contains(first, lastCommand)) {
			out = out[1:]
			continue
		}
		break
	}
	filtered := out[:0]
	for _, l := range out {
		if !promptRe.MatchString(l) {
			filtered = append(filtered, l)
		}
	}
	out = filtered
	for len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		out = out[1:]
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// stripFileResponse removes only the framing from a file-read reply: lines
// carrying the marker echo and the echoed command line. Unlike
// stripShellResponse it never drops blank lines or lines that merely resemble
// a shell prompt, so file content is preserved verbatim.
func stripFileResponse(lines []string, marker, lastCommand string) string {
	markerEcho := "echo " + marker
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.Contains(l, markerEcho) || markerLineRe.MatchString(strings.TrimSpace(l)) {
			continue
		}
		out = append(out, l)
	}
	if len(out) > 0 && lastCommand != "" && strings.Contains(out[0], lastCommand) {
		out = out[1:]
	}
	return strings.Join(out, "\n")
}

// applyStringEdit replaces the single occurrence of oldText in content with
// newText. Matching is exact but tolerant of CRLF/LF differences: content and
// oldText are normalized to LF for the search, and the file's original newline
// style is restored on write. It mirrors the uniqueness semantics of Zed's
// edit_file: zero matches, or more than one, is reported as an error so the
// model re-reads or adds more context instead of editing the wrong location.
// It returns the edited content plus an error message ("" on success).
func applyStringEdit(content, oldText, newText string) (string, string) {
	if oldText == "" {
		return "", "old_text must not be empty"
	}
	hadCRLF := strings.Contains(content, "\r\n")
	work := strings.ReplaceAll(content, "\r\n", "\n")
	needle := strings.ReplaceAll(oldText, "\r\n", "\n")
	replacement := strings.ReplaceAll(newText, "\r\n", "\n")

	idx, count := -1, 0
	// Advance by one byte (not by len(needle)) so overlapping occurrences are
	// also counted: a needle that can overlap itself must still be unique.
	for from := 0; ; {
		j := strings.Index(work[from:], needle)
		if j < 0 {
			break
		}
		pos := from + j
		if count == 0 {
			idx = pos
		}
		count++
		from = pos + 1
	}
	switch {
	case count == 0:
		return "", "old_text not found in file; read the file again to get the exact current content."
	case count > 1:
		return "", fmt.Sprintf("old_text matched %d locations; include more surrounding context to make it unique.", count)
	}
	out := work[:idx] + replacement + work[idx+len(needle):]
	if hadCRLF {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	return out, ""
}

func decodeBase64(s string) ([]byte, bool) {
	compact := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	data, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		return nil, false
	}
	return data, true
}
