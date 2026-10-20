package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func writeFile(path, content string) string {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "Error writing file: " + err.Error()
	}
	return "Successfully wrote to: " + path
}

// editFile replaces the single occurrence of oldText with newText, mirroring
// the control end and Zed's edit_file: it fails closed (reports an error) when
// oldText is missing or not unique, so the model re-reads or adds context
// rather than editing the wrong location. CRLF/LF differences are tolerated.
func editFile(path, oldText, newText string) string {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "File does not exist: " + path
		}
		return "Error editing file: " + err.Error()
	}
	if info.IsDir() {
		return path + " is a directory"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "Error editing file: " + err.Error()
	}
	edited, editErr := applyStringEdit(string(data), oldText, newText)
	if editErr != "" {
		return "Error: " + editErr
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		return "Error editing file: " + err.Error()
	}
	return "Successfully edited file: " + path
}

// applyStringEdit replaces the single occurrence of oldText in content with
// newText. Content and oldText are normalized to LF for the search and the
// file's original newline style is restored on write. Returns the edited
// content plus an error message ("" on success).
func applyStringEdit(content, oldText, newText string) (string, string) {
	if oldText == "" {
		return "", "old_text must not be empty"
	}
	hadCRLF := strings.Contains(content, "\r\n")
	work := strings.ReplaceAll(content, "\r\n", "\n")
	needle := strings.ReplaceAll(oldText, "\r\n", "\n")
	replacement := strings.ReplaceAll(newText, "\r\n", "\n")

	idx, count := -1, 0
	// Advance by one byte so overlapping occurrences are counted too.
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
