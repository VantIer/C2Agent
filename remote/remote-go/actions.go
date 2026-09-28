package main

// Local file actions, semantically aligned with remote/common and remote-c.

import (
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
		return editFile(arg(params, 0), arg(params, 1), arg(params, 2), arg(params, 3), arg(params, 4))
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
	data, err := os.ReadFile(path)
	if err != nil {
		return "Error reading file: " + err.Error()
	}
	content := string(data)
	if startLine == "" || startLine == "0" {
		if len(content) > readFileLimit {
			cut := readFileLimit
			// Do not split a multi-byte UTF-8 sequence at the truncation point.
			for cut > 0 && !utf8.RuneStart(content[cut]) {
				cut--
			}
			return content[:cut]
		}
		return content
	}
	s, err := strconv.Atoi(strings.TrimSpace(startLine))
	if err != nil {
		return fmt.Sprintf("Invalid line numbers: start_line=%s, end_line=%s", startLine, endLine)
	}
	lines := splitKeepEnds(content)
	start := s - 1
	if start < 0 {
		start = 0
	}
	end := len(lines)
	if endLine != "" && endLine != "0" {
		if e, err := strconv.Atoi(strings.TrimSpace(endLine)); err == nil {
			end = e
		}
	}
	if start >= len(lines) {
		return fmt.Sprintf("Start line %s exceeds file line count (%d)", startLine, len(lines))
	}
	if end > len(lines) {
		end = len(lines)
	}
	if end < start {
		end = start
	}
	return strings.Join(lines[start:end], "")
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

func editFile(path, operation, startLine, endLine, content string) string {
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
	lines := splitKeepEnds(string(data))
	start := 0
	if strings.TrimSpace(startLine) != "" && strings.TrimSpace(startLine) != "0" {
		v, err := strconv.Atoi(strings.TrimSpace(startLine))
		if err != nil {
			return fmt.Sprintf("Invalid line numbers: start_line=%s, end_line=%s", startLine, endLine)
		}
		start = v - 1
		if start < 0 {
			start = 0
		}
	}
	end := len(lines)
	if strings.TrimSpace(endLine) != "" && strings.TrimSpace(endLine) != "0" {
		if v, err := strconv.Atoi(strings.TrimSpace(endLine)); err == nil {
			end = v
		}
	}
	switch operation {
	case "add":
		if start > len(lines) {
			start = len(lines)
		}
		lines = append(lines[:start], append([]string{content + "\n"}, lines[start:]...)...)
	case "del":
		if start >= len(lines) {
			return fmt.Sprintf("Start line %s exceeds file line count (%d)", startLine, len(lines))
		}
		if end > len(lines) {
			end = len(lines)
		}
		if end < start {
			end = start
		}
		lines = append(lines[:start], lines[end:]...)
	case "modify":
		if start >= len(lines) {
			return fmt.Sprintf("Start line %s exceeds file line count (%d)", startLine, len(lines))
		}
		if end > len(lines) {
			end = len(lines)
		}
		if end < start {
			end = start
		}
		lines = append(lines[:start], append([]string{content + "\n"}, lines[end:]...)...)
	default:
		return "Unknown operation: " + operation + ". Use 'add', 'del', or 'modify'"
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o644); err != nil {
		return "Error editing file: " + err.Error()
	}
	return fmt.Sprintf("Successfully performed %s on file: %s", operation, path)
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

func splitKeepEnds(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}
