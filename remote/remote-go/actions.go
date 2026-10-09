package main

// Local file actions, semantically aligned with remote/common and remote-c.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func arg(params []string, i int) string {
	if i < len(params) {
		return params[i]
	}
	return ""
}

func runAction(cmd uint8, params []string, cmdTimeout time.Duration) string {
	switch cmd {
	case cmdGetCwd:
		return getCwd()
	case cmdListDir:
		return listDir(arg(params, 0))
	case cmdMakeDir:
		return makeDir(arg(params, 0))
	case cmdCreateFile:
		return createFile(arg(params, 0))
	case cmdDeleteDir:
		return deletePath(arg(params, 0), true)
	case cmdDeleteFile:
		return deletePath(arg(params, 0), false)
	case cmdRenameDir, cmdRenameFile:
		return renamePath(arg(params, 0), arg(params, 1))
	case cmdReadFile:
		return readFile(arg(params, 0), arg(params, 1), arg(params, 2))
	case cmdWriteFile:
		return writeFile(arg(params, 0), arg(params, 1))
	case cmdEditFile:
		return editFile(arg(params, 0), arg(params, 1), arg(params, 2))
	case cmdCopy:
		return copyPath(arg(params, 0), arg(params, 1))
	case cmdMove:
		return movePath(arg(params, 0), arg(params, 1))
	case cmdExecCmd:
		return runCmd(arg(params, 0), cmdTimeout)
	}
	return fmt.Sprintf("Error: Unknown cmd: %#x", cmd)
}

func getCwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "Error getting cwd: " + err.Error()
	}
	if abs, err := filepath.Abs(wd); err == nil {
		return abs
	}
	return wd
}

func listDir(path string) string {
	if path == "" {
		path = "."
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "Path does not exist: " + path
		}
		return "Error listing directory: " + err.Error()
	}
	if !info.IsDir() {
		return path + " is a file"
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "Error listing directory: " + err.Error()
	}
	var b strings.Builder
	for _, e := range entries {
		typ := "FILE"
		size := int64(0)
		if e.IsDir() {
			typ = "DIR"
		} else if fi, err := e.Info(); err == nil {
			size = fi.Size()
		}
		fmt.Fprintf(&b, "%-6s %12d %s\n", typ, size, e.Name())
	}
	out := strings.TrimRight(b.String(), "\n")
	if out == "" {
		return "Empty directory"
	}
	return out
}

func makeDir(path string) string {
	if _, err := os.Stat(path); err == nil {
		return "Directory already exists: " + path
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "Error creating directory: " + err.Error()
	}
	return "Successfully created directory: " + path
}

func createFile(path string) string {
	if _, err := os.Stat(path); err == nil {
		return "File already exists: " + path
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "Error creating file: " + err.Error()
	}
	_ = f.Close()
	return "Successfully created file: " + path
}

func deletePath(path string, wantDir bool) string {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "Path does not exist: " + path
		}
		return "Error deleting: " + err.Error()
	}
	if info.IsDir() != wantDir {
		kind := "file"
		if wantDir {
			kind = "directory"
		}
		return "Error: not a " + kind + ": " + path
	}
	if err := os.RemoveAll(path); err != nil {
		return "Error deleting: " + err.Error()
	}
	return "Successfully deleted: " + path
}

func renamePath(path, newName string) string {
	if newName == "" || strings.ContainsAny(newName, `/\`) {
		return "Error: new_name must be a bare name without path separators"
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "Path does not exist: " + path
		}
		return "Error renaming: " + err.Error()
	}
	newPath := filepath.Join(filepath.Dir(path), newName)
	if _, err := os.Stat(newPath); err == nil {
		return "Target name already exists: " + newName
	}
	if err := os.Rename(path, newPath); err != nil {
		return "Error renaming: " + err.Error()
	}
	return fmt.Sprintf("Successfully renamed: %s -> %s", path, newName)
}

func readFile(path, startLine, endLine string) string {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "File does not exist: " + path
		}
		return "Error reading file: " + err.Error()
	}
	if info.IsDir() {
		return path + " is a directory"
	}
	if startLine == "" || startLine == "0" {
		return readWholeLimited(path)
	}
	s, err := strconv.Atoi(strings.TrimSpace(startLine))
	if err != nil {
		return fmt.Sprintf("Invalid line numbers: start_line=%s, end_line=%s", startLine, endLine)
	}
	start := s - 1
	if start < 0 {
		start = 0
	}
	end := 0 // 0 = to end of file
	if endLine != "" && endLine != "0" {
		if e, err := strconv.Atoi(strings.TrimSpace(endLine)); err == nil {
			end = e
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return "Error reading file: " + err.Error()
	}
	defer f.Close()

	// Stream line by line, keeping only the requested range, so a huge file is
	// never loaded into memory in full.
	r := bufio.NewReader(f)
	var b strings.Builder
	lineNo := 0
	reachedStart := false
	for {
		line, rerr := r.ReadString('\n')
		if len(line) > 0 {
			lineNo++
			if lineNo > start {
				reachedStart = true
				if end > 0 && lineNo > end {
					break
				}
				b.WriteString(line)
			}
		}
		if rerr != nil {
			break
		}
	}
	if !reachedStart {
		return fmt.Sprintf("Start line %s exceeds file line count (%d)", startLine, lineNo)
	}
	return b.String()
}

// readWholeLimited reads at most enough bytes to cover readFileLimit characters
// (4 bytes per character in the worst case), so an enormous file cannot exhaust
// memory, then truncates to readFileLimit characters (not bytes).
func readWholeLimited(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "Error reading file: " + err.Error()
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(readFileLimit)*4+4))
	if err != nil {
		return "Error reading file: " + err.Error()
	}
	s := string(data)
	if utf8.RuneCountInString(s) <= readFileLimit {
		return s
	}
	n := 0
	for i := range s {
		if n == readFileLimit {
			return s[:i]
		}
		n++
	}
	return s
}

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

func copyPath(src, dest string) string {
	info, err := os.Stat(src)
	if err != nil {
		return "Source not found: " + src
	}
	if info.IsDir() {
		if err := copyTree(src, dest); err != nil {
			return "Error copying: " + err.Error()
		}
	} else {
		if dir := filepath.Dir(dest); dir != "" && dir != "." {
			_ = os.MkdirAll(dir, 0o755)
		}
		if err := copyFile(src, dest); err != nil {
			return "Error copying: " + err.Error()
		}
	}
	return fmt.Sprintf("Successfully copied: %s -> %s", src, dest)
}

func movePath(src, dest string) string {
	if _, err := os.Stat(src); err != nil {
		return "Source not found: " + src
	}
	if dir := filepath.Dir(dest); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	if err := os.Rename(src, dest); err == nil {
		return fmt.Sprintf("Successfully moved: %s -> %s", src, dest)
	}
	// Cross-device fallback: copy then remove.
	info, _ := os.Stat(src)
	if info != nil && info.IsDir() {
		if err := copyTree(src, dest); err != nil {
			return "Error moving: " + err.Error()
		}
		if err := os.RemoveAll(src); err != nil {
			return "Error moving: " + err.Error()
		}
	} else {
		if err := copyFile(src, dest); err != nil {
			return "Error moving: " + err.Error()
		}
		if err := os.Remove(src); err != nil {
			return "Error moving: " + err.Error()
		}
	}
	return fmt.Sprintf("Successfully moved: %s -> %s", src, dest)
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func copyTree(src, dest string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dest)
	}
	if err := os.MkdirAll(dest, info.Mode()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dest, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
