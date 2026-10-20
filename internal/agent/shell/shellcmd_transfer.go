package shell

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

func base64DecodeCmd(osName string) string {
	if osName == "macOS" {
		return "base64 -D"
	}
	return "base64 -d"
}

func chunks(s string, size int) []string {
	var out []string
	for len(s) > size {
		out = append(out, s[:size])
		s = s[size:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

// writeBase64Cmd builds a single command that base64-decodes b64 into destPath.
func writeBase64Cmd(t target, destPath, b64 string) (string, error) {
	if t.isWindows() {
		return t.finish(psWriteBase64(destPath, b64))
	}
	if len(b64) > posixCmdLimit {
		return "", fmt.Errorf("file too large for a shell bot (%d base64 bytes > %d); use a native agent", len(b64), posixCmdLimit)
	}
	dest := posixPath(destPath)
	b64d := base64DecodeCmd(t.os)
	// Create the destination's parent first, like the native agents (MkdirAll).
	prefix := "mkdir -p \"$(dirname " + dest + ")\"; "
	cs := chunks(b64, posixChunk)
	if len(cs) == 0 {
		return prefix + "printf '' > " + dest, nil
	}
	parts := []string{"printf '%s' '" + cs[0] + "' | " + b64d + " > " + dest}
	for _, c := range cs[1:] {
		parts = append(parts, "printf '%s' '"+c+"' | "+b64d+" >> "+dest)
	}
	return prefix + strings.Join(parts, "; "), nil
}

// fileSizeCmd returns a command that prints the size of path in bytes.
func fileSizeCmd(t target, path string) (string, error) {
	if t.isWindows() {
		return t.finish(psFileSize(path))
	}
	return "wc -c < " + posixPath(path), nil
}

// readChunkCmd returns a command that prints the base64 of `length` bytes at
// byte `offset` in path. The control end drives this with a fixed chunk-size
// loop, so memory on the shell bot is bounded regardless of file size.
func readChunkCmd(t target, path string, offset, length int64) (string, error) {
	if t.isWindows() {
		return t.finish(psReadChunk(path, offset, length))
	}
	// tail -c +N starts at byte N (1-based); head -c L caps the chunk.
	return "tail -c +" + strconv.FormatInt(offset+1, 10) + " " + posixPath(path) +
		" | head -c " + strconv.FormatInt(length, 10) + " | base64", nil
}

// truncateCreateCmd returns a command that creates path's parent directory (if
// needed) and truncates path to zero bytes, starting a fresh upload target.
func truncateCreateCmd(t target, path string) (string, error) {
	if t.isWindows() {
		return t.finish(psTruncateCreate(path))
	}
	q := posixPath(path)
	return "mkdir -p \"$(dirname " + q + ")\"; : > " + q, nil
}

// appendBase64Cmd returns a command that base64-decodes b64 and appends it to
// path (creating the file if needed). The caller sizes chunks so b64 fits the
// command line.
func appendBase64Cmd(t target, path, b64 string) (string, error) {
	if t.isWindows() {
		return t.finish(psAppendBase64(path, b64))
	}
	if len(b64) > posixCmdLimit {
		return "", fmt.Errorf("chunk too large (%d base64 bytes > %d)", len(b64), posixCmdLimit)
	}
	return "printf '%s' '" + b64 + "' | " + base64DecodeCmd(t.os) + " >> " + posixPath(path), nil
}

// removeCmd returns a best-effort command that deletes path (used for temp
// files); unlike deleteCmd it enforces no file/directory kind.
func removeCmd(t target, path string) (string, error) {
	if t.isWindows() {
		return t.finish(psRemove(path))
	}
	return "rm -f " + posixPath(path), nil
}

// maxEditBytes is the largest file a shell bot can rewrite in one command: the
// base64 payload must fit the host's command-line budget (plain bytes are ~1/4
// less than base64, and cmd hosts inflate that ~2x more under -EncodedCommand).
// Larger files must be edited through a native agent.
func maxEditBytes(t target) int {
	if t.isWindows() {
		if t.psHost() {
			// Direct script: plain bytes ~3/4 of the embedded base64, with room
			// for the script wrapper under winPSCmdLimit.
			return winPSCmdLimit*3/4 - 1500
		}
		// cmd host: the write goes through -EncodedCommand, which inflates the
		// script ~8/3x (UTF-16LE word * base64). Keep well inside winCmdLimit
		// including a generous path/wrapper margin.
		return 1500
	}
	return posixCmdLimit*3/4 - 8
}

// readFileWrappedCmd emits a sentinel-wrapped, size-bounded read of the whole
// file for editing. The content is base64 (so the control end can never mistake
// file content for an error message), `missing` is printed when path is not a
// regular file, and `begin`/`end` frame the payload.
func readFileWrappedCmd(t target, path, begin, end, missing string, maxBytes int) (string, error) {
	if t.isWindows() {
		return t.finish(psWrappedRead(path, begin, end, missing, maxBytes))
	}
	return "if [ -f " + posixPath(path) + " ] && [ -r " + posixPath(path) + " ]; then printf '%s\\n' '" + begin +
		"'; head -c " + strconv.Itoa(maxBytes) + " " + posixPath(path) +
		" | base64; printf '%s\\n' '" + end + "'; else printf '%s\\n' '" + missing + "'; fi", nil
}

// randSentinel returns a random per-call token used to frame a file read so
// payload content can never be confused with a framing marker or error message.
func randSentinel(prefix string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "__C2EDIT_" + prefix + "_fallback__"
	}
	return "__C2EDIT_" + prefix + "_" + hex.EncodeToString(buf) + "__"
}

// randToken returns a short random hex token for temporary file names.
func randToken() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "fallback"
	}
	return hex.EncodeToString(buf)
}
