package shell

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"c2agent/internal/command"
	"c2agent/internal/limits"
)

func getCwdCmd(t target) (string, error) {
	if t.isWindows() {
		return t.finish(psGetCwd())
	}
	return "pwd", nil
}

func listDirCmd(t target, path string) (string, error) {
	if t.isWindows() {
		return t.finish(psListDir(path))
	}
	return "ls -la " + posixPath(path), nil
}

// clampLineEnd bounds an inclusive, 1-based line range to ReadFileLines.
func clampLineEnd(start, end int) int {
	if end == 0 || end < start || end > start+limits.ReadFileLines-1 {
		return start + limits.ReadFileLines - 1
	}
	return end
}

// readFileCmd builds a read_file command. Windows whole-file reads return a
// bounded base64 head; Windows line ranges are extracted remotely by byte
// offset so arbitrary ranges work without loading the file. POSIX line ranges
// use sed (bounded by the 512-line clamp); whole-file reads are capped by the
// control end.
func readFileCmd(t target, path, startLine, endLine string) (string, error) {
	if t.isWindows() {
		s := strings.TrimSpace(startLine)
		if s == "" || s == "0" {
			return readFileWrappedCmd(t, path, winReadBegin, winReadEnd, winReadMissing, readFileByteCap)
		}
		start, err := strconv.Atoi(s)
		if err != nil || start < 1 {
			return "", fmt.Errorf("invalid start_line: %s", startLine)
		}
		end := 0
		if e := strings.TrimSpace(endLine); e != "" && e != "0" {
			if n, err := strconv.Atoi(e); err == nil && n > 0 {
				end = n
			}
		}
		end = clampLineEnd(start, end)
		// Base64 expands by 4/3; leave room for the newline and framing lines.
		maxPlain := limits.MaxOutputBytes*3/4 - 4096
		return t.finish(psReadLineRange(path, start, end, maxPlain))
	}
	start := strings.TrimSpace(startLine)
	if start == "" || start == "0" {
		return "head -c " + strconv.Itoa(readFileByteCap) + " " + posixPath(path), nil
	}
	s, err := strconv.Atoi(start)
	if err != nil {
		return "", fmt.Errorf("invalid start_line: %s", start)
	}
	if s < 1 {
		s = 1
	}
	end := 0
	if e := strings.TrimSpace(endLine); e != "" && e != "0" {
		if n, err := strconv.Atoi(e); err == nil && n > 0 {
			end = n
		}
	}
	end = clampLineEnd(s, end)
	return "sed -n '" + strconv.Itoa(s) + "," + strconv.Itoa(end) + "p' " + posixPath(path), nil
}

func makeDirCmd(t target, path string) (string, error) {
	if t.isWindows() {
		return t.finish(psMakeDir(path))
	}
	return "mkdir -p " + posixPath(path), nil
}

func deleteCmd(t target, path string, wantDir bool) (string, error) {
	if t.isWindows() {
		return t.finish(psDelete(path, wantDir))
	}
	q := posixPath(path)
	if wantDir {
		return "if [ -f " + q + " ]; then echo " + posixQuote("Error: "+path+" is a file") + "; else rm -rf " + q + "; fi", nil
	}
	return "if [ -d " + q + " ]; then echo " + posixQuote("Error: "+path+" is a directory") + "; else rm -f " + q + "; fi", nil
}

func renameCmd(t target, path, newName string) (string, error) {
	if t.isWindows() {
		body, err := psRename(path, newName)
		if err != nil {
			return "", err
		}
		return t.finish(body)
	}
	if newName == "" || strings.ContainsAny(newName, `/\`) {
		return "", fmt.Errorf("new_name must be a bare name without path separators")
	}
	return "mv " + posixPath(path) + " " + posixPath(posixSibling(path, newName)), nil
}

// posixSibling joins newName onto path's parent directory (both treated as
// absolute/relative paths, only '/' separator). Mirrors the native agents,
// which rename in place rather than moving to the current directory.

func copyCmd(t target, src, dest string) (string, error) {
	if remotePathInside(src, dest, t) {
		return "", fmt.Errorf("destination is inside source: %s", dest)
	}
	if t.isWindows() {
		return t.finish(psCopy(src, dest))
	}
	// Create the destination's parent first, like the native agents (MkdirAll).
	return "mkdir -p \"$(dirname " + posixPath(dest) + ")\"; cp -r " + posixPath(src) + " " + posixPath(dest), nil
}

func moveCmd(t target, src, dest string) (string, error) {
	if remotePathInside(src, dest, t) {
		return "", fmt.Errorf("destination is inside source: %s", dest)
	}
	if t.isWindows() {
		return t.finish(psMove(src, dest))
	}
	return "mkdir -p \"$(dirname " + posixPath(dest) + ")\"; mv " + posixPath(src) + " " + posixPath(dest), nil
}

// remotePathInside reports whether child is parent or below it. Paths are
// normalized with the remote separator rules (Windows is case-insensitive);
// this is a lexical guard for copy/move, not a filesystem resolution.

func createFileCmd(t target, path string) (string, error) {
	if t.isWindows() {
		return t.finish(psCreateFile(path))
	}
	q := posixPath(path)
	return "mkdir -p \"$(dirname " + q + ")\"; touch " + q, nil
}

// base64DecodeCmd is the platform's base64-decode invocation (BSD/macOS uses -D).

func actionToShell(action string, params map[string]any, t target) (string, error) {
	s := func(k string) string { return paramString(params[k]) }
	switch action {
	case "exec_cmd":
		// Passed through verbatim: on a cmd.exe host the model is told the
		// environment is "cmd", on a PowerShell host "PowerShell", so it can
		// emit the right syntax (see the rendered system prompt).
		return s("command"), nil
	case "get_cwd":
		return getCwdCmd(t)
	case "list_dir":
		p := s("path")
		if p == "" {
			p = "."
		}
		return listDirCmd(t, p)
	case "read_file":
		return readFileCmd(t, s("path"), s("start_line"), s("end_line"))
	case "write_file":
		b64 := base64.StdEncoding.EncodeToString([]byte(s("content")))
		return writeBase64Cmd(t, s("path"), b64)
	case "create_file":
		return createFileCmd(t, s("path"))
	case "delete_file":
		return deleteCmd(t, s("path"), false)
	case "delete_dir":
		return deleteCmd(t, s("path"), true)
	case "rename_file", "rename_dir":
		return renameCmd(t, s("path"), s("new_name"))
	case "make_dir":
		return makeDirCmd(t, s("path"))
	case "copy":
		return copyCmd(t, s("src"), s("dest"))
	case "move":
		return moveCmd(t, s("src"), s("dest"))
	default:
		return "", fmt.Errorf("unknown action: %s", action)
	}
}

// paramString renders a tool parameter for the shell command builders. It
// delegates to command.String so the control end has a single implementation.
func paramString(v any) string { return command.String(v) }

// isWholeFileRead reports whether a read_file request covers the whole file.
func isWholeFileRead(params map[string]any) bool {
	s := paramString(params["start_line"])
	return strings.TrimSpace(s) == "" || strings.TrimSpace(s) == "0"
}
