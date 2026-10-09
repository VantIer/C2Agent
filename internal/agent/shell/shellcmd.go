package shell

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"c2agent/internal/command"
)

func isWindows(osName string) bool { return osName == "Windows" }

func isMac(osName string) bool { return osName == "macOS" }

// posixQuote single-quotes a string for POSIX shells.
func posixQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var cmdVarRe = regexp.MustCompile(`%[A-Za-z_][A-Za-z0-9_]*%`)

// winQuote double-quotes a path for cmd, rejecting %VAR% patterns.
func winQuote(s string) (string, error) {
	if cmdVarRe.MatchString(s) {
		return "", fmt.Errorf("cmd cannot safely handle '%%' variable patterns in path: %q", s)
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`, nil
}

// psQuote quotes a path for a single-quoted PowerShell string (still parsed by
// cmd first when the host shell is cmd). Windows filenames may not contain
// '"' or '%', so reject them rather than risk breaking the outer quoting.
func psQuote(s string) (string, error) {
	if cmdVarRe.MatchString(s) {
		return "", fmt.Errorf("cmd cannot safely handle '%%' variable patterns in path: %q", s)
	}
	if strings.ContainsRune(s, '"') {
		return "", fmt.Errorf("invalid character in Windows path: %q", s)
	}
	return strings.ReplaceAll(s, "'", "''"), nil
}

func getCwdCmd(osName string) string {
	if isWindows(osName) {
		return `powershell -NoProfile -Command "(Get-Location).Path"`
	}
	return "pwd"
}

func listDirCmd(osName, path string) (string, error) {
	if isWindows(osName) {
		p, err := psQuote(path)
		if err != nil {
			return "", err
		}
		return "powershell -NoProfile -Command \"Get-ChildItem -Force '" + p +
			"' | Select-Object PSIsContainer,Length,Name | Format-Table -AutoSize\"", nil
	}
	return "ls -la " + posixQuote(path), nil
}

// readFileCmd builds a read_file command. Whole-file reads (empty start_line)
// read at most readFileByteCap bytes from the head on EVERY platform, so an
// enormous file cannot exhaust the controlled end's memory; the control end
// then truncates the result to 51200 characters. POSIX line ranges use `sed`
// (bounded output); Windows ranges use the same bounded OpenRead as whole reads
// and are sliced by the control end. Windows output is always base64.
func readFileCmd(osName, path, startLine, endLine string) (string, error) {
	if isWindows(osName) {
		p, err := psQuote(path)
		if err != nil {
			return "", err
		}
		start := 0
		if v := strings.TrimSpace(startLine); v != "" && v != "0" {
			n, aerr := strconv.Atoi(v)
			if aerr != nil {
				return "", fmt.Errorf("invalid start_line: %s", v)
			}
			start = n
		}
		if start <= 0 {
			// Whole-file read: bounded head read; the control end truncates.
			return openReadBase64Cmd(p, readFileByteCap), nil
		}
		end := 0
		if v := strings.TrimSpace(endLine); v != "" && v != "0" {
			if n, aerr := strconv.Atoi(v); aerr == nil {
				end = n
			}
		}
		return windowsRangeBase64Cmd(p, start, end), nil
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

// openReadBase64Cmd reads at most maxBytes from the head of a psQuoted path and
// returns a PowerShell command that base64-encodes them. It uses OpenRead (not
// ReadAllBytes) so a huge file cannot exhaust the agent's memory.
func openReadBase64Cmd(p string, maxBytes int) string {
	return "powershell -NoProfile -Command \"$fs=[IO.File]::OpenRead('" + p +
		"');try{$n=[int][Math]::Min(" + strconv.Itoa(maxBytes) + ",$fs.Length);" +
		"$b=New-Object byte[] $n;$r=$fs.Read($b,0,$n);" +
		"[Convert]::ToBase64String($b,0,$r)}finally{$fs.Close()}\""
}

// windowsRangeBase64Cmd base64-encodes lines [start, end] (end<=0 = to EOF). It
// uses [IO.File]::ReadLines, which lazily enumerates lines, so memory stays
// proportional to the requested range rather than the whole file — deep ranges
// work without reading the entire file into memory.
func windowsRangeBase64Cmd(p string, start, end int) string {
	stop := ""
	if end > 0 {
		stop = "if($i -ge " + strconv.Itoa(end) + "){break};"
	}
	return "powershell -NoProfile -Command \"[Text.StringBuilder]$o=New-Object Text.StringBuilder;$i=0;" +
		"foreach($l in [IO.File]::ReadLines('" + p + "')){$i++;" +
		"if($i -ge " + strconv.Itoa(start) + "){[void]$o.AppendLine($l)};" + stop + "};" +
		"[Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($o.ToString()))\""
}

// maxEditBytes is the largest file a shell bot can rewrite in one command: the
// base64 payload must fit the platform's command-line limit (plain bytes are
// ~3/4 of the base64 length). Larger files must be edited through a native agent.
func maxEditBytes(osName string) int {
	if isWindows(osName) {
		return winCmdLimit*3/4 - 8
	}
	return posixCmdLimit*3/4 - 8
}

// readFileWrappedCmd emits a sentinel-wrapped, size-bounded read of the whole
// file for editing. The content is base64 on BOTH platforms (so the control end
// can never mistake file content for an error message), `missing` is printed
// when path is not a regular file, and `begin`/`end` frame the payload.
func readFileWrappedCmd(osName, path, begin, end, missing string, maxBytes int) (string, error) {
	if isWindows(osName) {
		p, err := psQuote(path)
		if err != nil {
			return "", err
		}
		return "powershell -NoProfile -Command \"if(Test-Path -LiteralPath '" + p +
			"' -PathType Leaf){'" + begin + "';$fs=[IO.File]::OpenRead('" + p +
			"');try{$n=[int][Math]::Min(" + strconv.Itoa(maxBytes) + ",$fs.Length);" +
			"$b=New-Object byte[] $n;$r=$fs.Read($b,0,$n);[Convert]::ToBase64String($b,0,$r)}finally{$fs.Close()};'" + end +
			"'}else{'" + missing + "'}\"", nil
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

func makeDirCmd(osName, path string) (string, error) {
	if isWindows(osName) {
		q, err := winQuote(path)
		if err != nil {
			return "", err
		}
		return "mkdir " + q, nil
	}
	return "mkdir -p " + posixQuote(path), nil
}

func deleteCmd(osName, path string) (string, error) {
	if isWindows(osName) {
		p, err := psQuote(path)
		if err != nil {
			return "", err
		}
		return "powershell -NoProfile -Command \"Remove-Item -Recurse -Force -LiteralPath '" + p + "'\"", nil
	}
	return "rm -rf " + posixQuote(path), nil
}

func renameCmd(osName, path, newName string) (string, error) {
	if newName == "" || strings.ContainsAny(newName, `/\`) {
		return "", fmt.Errorf("new_name must be a bare name without path separators")
	}
	if isWindows(osName) {
		p, err := psQuote(path)
		if err != nil {
			return "", err
		}
		n, err := psQuote(newName)
		if err != nil {
			return "", err
		}
		return "powershell -NoProfile -Command \"Rename-Item -LiteralPath '" + p + "' -NewName '" + n + "'\"", nil
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

func remoteJoin(dir, name, osName string) string {
	if dir == "" {
		return name
	}
	sep := "/"
	if isWindows(osName) {
		sep = `\`
	}
	if strings.HasSuffix(dir, "/") || strings.HasSuffix(dir, `\`) {
		return dir + name
	}
	return dir + sep + name
}

func copyCmd(osName, src, dest string) (string, error) {
	if isWindows(osName) {
		s, err := psQuote(src)
		if err != nil {
			return "", err
		}
		d, err := psQuote(dest)
		if err != nil {
			return "", err
		}
		return "powershell -NoProfile -Command \"Copy-Item -Recurse -Force -LiteralPath '" + s + "' -Destination '" + d + "'\"", nil
	}
	return "cp -r " + posixQuote(src) + " " + posixQuote(dest), nil
}

func moveCmd(osName, src, dest string) (string, error) {
	if isWindows(osName) {
		s, err := psQuote(src)
		if err != nil {
			return "", err
		}
		d, err := psQuote(dest)
		if err != nil {
			return "", err
		}
		return "powershell -NoProfile -Command \"Move-Item -Force -LiteralPath '" + s + "' -Destination '" + d + "'\"", nil
	}
	return "mv " + posixQuote(src) + " " + posixQuote(dest), nil
}

func createFileCmd(osName, path string) (string, error) {
	if isWindows(osName) {
		p, err := psQuote(path)
		if err != nil {
			return "", err
		}
		return "powershell -NoProfile -Command \"$d=Split-Path -Parent '" + p +
			"'; if($d){[IO.Directory]::CreateDirectory($d)|Out-Null}; New-Item -ItemType File -Force -Path '" + p + "' | Out-Null\"", nil
	}
	q := posixQuote(path)
	return "mkdir -p \"$(dirname " + q + ")\"; touch " + q, nil
}

const (
	posixChunk = 100000
	// Guards against emitting a single shell command that exceeds the host
	// shell's command-line / input length limits (base64 payload bytes).
	posixCmdLimit = 900000
	winCmdLimit   = 7000 // cmd.exe's command line is ~8 KB
	// readFileByteCap bounds how many bytes a whole-file read pulls from the
	// head of a file (enough for 51200 UTF-8 characters), so an enormous file
	// cannot exhaust the controlled end's memory.
	readFileByteCap = 51200 * 4

	// Segmented transfer chunk sizes: each chunk is moved by ONE shell command,
	// so it must fit the host shell's command-line limit once base64-encoded.
	// The control end drives the offset loop; the shell bot only reads/appends.
	posixTransferChunk = 32768
	winTransferChunk   = 4096
)

// transferChunkSize is the plain-byte chunk size used for segmented upload and
// download on this platform.
func transferChunkSize(osName string) int {
	if isWindows(osName) {
		return winTransferChunk
	}
	return posixTransferChunk
}

// randToken returns a short random hex token for temporary file names.
func randToken() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "fallback"
	}
	return hex.EncodeToString(buf)
}

// base64DecodeCmd is the platform's base64-decode invocation (BSD/macOS uses -D).
func base64DecodeCmd(osName string) string {
	if isMac(osName) {
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
func writeBase64Cmd(osName, destPath, b64 string) (string, error) {
	if isWindows(osName) {
		// A single well-formed command keeps the payload within cmd.exe's ~8 KB
		// command-line limit; larger files are rejected (use a native agent).
		// There is deliberately no multi-chunk path: it could never be reached
		// within winCmdLimit and would also overflow the command line anyway.
		if len(b64) > winCmdLimit {
			return "", fmt.Errorf("file too large for a shell bot (%d base64 bytes > %d); use a native agent", len(b64), winCmdLimit)
		}
		dest, err := psQuote(destPath)
		if err != nil {
			return "", err
		}
		return "powershell -NoProfile -Command \"[IO.File]::WriteAllBytes('" + dest +
			"',[Convert]::FromBase64String('" + b64 + "'))\"", nil
	}
	if len(b64) > posixCmdLimit {
		return "", fmt.Errorf("file too large for a shell bot (%d base64 bytes > %d); use a native agent", len(b64), posixCmdLimit)
	}
	dest := posixQuote(destPath)
	// BSD (macOS) base64 decodes with -D, GNU with -d.
	b64d := "base64 -d"
	if isMac(osName) {
		b64d = "base64 -D"
	}
	cs := chunks(b64, posixChunk)
	if len(cs) == 0 {
		return "printf '' > " + dest, nil
	}
	parts := []string{"printf '%s' '" + cs[0] + "' | " + b64d + " > " + dest}
	for _, c := range cs[1:] {
		parts = append(parts, "printf '%s' '"+c+"' | "+b64d+" >> "+dest)
	}
	return strings.Join(parts, "; "), nil
}

// fileSizeCmd returns a command that prints the size of path in bytes.
func fileSizeCmd(osName, path string) (string, error) {
	if isWindows(osName) {
		p, err := psQuote(path)
		if err != nil {
			return "", err
		}
		return "powershell -NoProfile -Command \"(Get-Item -LiteralPath '" + p + "').Length\"", nil
	}
	return "wc -c < " + posixQuote(path), nil
}

// readChunkCmd returns a command that prints the base64 of `length` bytes at
// byte `offset` in path. The control end drives this with a fixed chunk-size
// loop, so memory on the shell bot is bounded regardless of file size.
func readChunkCmd(osName, path string, offset, length int64) (string, error) {
	if isWindows(osName) {
		p, err := psQuote(path)
		if err != nil {
			return "", err
		}
		n := strconv.FormatInt(length, 10)
		return "powershell -NoProfile -Command \"$fs=[IO.File]::OpenRead('" + p +
			"');try{$fs.Seek(" + strconv.FormatInt(offset, 10) + ",[IO.SeekOrigin]::Begin)|Out-Null;" +
			"$b=New-Object byte[] " + n + ";$r=$fs.Read($b,0," + n + ");" +
			"[Convert]::ToBase64String($b,0,$r)}finally{$fs.Close()}\"", nil
	}
	// tail -c +N starts at byte N (1-based); head -c L caps the chunk.
	return "tail -c +" + strconv.FormatInt(offset+1, 10) + " " + posixQuote(path) +
		" | head -c " + strconv.FormatInt(length, 10) + " | base64", nil
}

// truncateCreateCmd returns a command that creates path's parent directory (if
// needed) and truncates path to zero bytes, starting a fresh upload target.
func truncateCreateCmd(osName, path string) (string, error) {
	if isWindows(osName) {
		p, err := psQuote(path)
		if err != nil {
			return "", err
		}
		return "powershell -NoProfile -Command \"$d=Split-Path -Parent '" + p +
			"';if($d){[IO.Directory]::CreateDirectory($d)|Out-Null};" +
			"[IO.File]::WriteAllBytes('" + p + "',(New-Object byte[] 0))\"", nil
	}
	q := posixQuote(path)
	return "mkdir -p \"$(dirname " + q + ")\"; : > " + q, nil
}

// appendBase64Cmd returns a command that base64-decodes b64 and appends it to
// path (creating the file if needed). The caller sizes chunks so b64 fits the
// command line.
func appendBase64Cmd(osName, path, b64 string) (string, error) {
	if isWindows(osName) {
		if len(b64) > winCmdLimit {
			return "", fmt.Errorf("chunk too large (%d base64 bytes > %d)", len(b64), winCmdLimit)
		}
		p, err := psQuote(path)
		if err != nil {
			return "", err
		}
		return "powershell -NoProfile -Command \"$fs=[IO.File]::Open('" + p +
			"',[IO.FileMode]::Append,[IO.FileAccess]::Write,[IO.FileShare]::Read);" +
			"try{$b=[Convert]::FromBase64String('" + b64 + "');$fs.Write($b,0,$b.Length)}finally{$fs.Close()}\"", nil
	}
	if len(b64) > posixCmdLimit {
		return "", fmt.Errorf("chunk too large (%d base64 bytes > %d)", len(b64), posixCmdLimit)
	}
	return "printf '%s' '" + b64 + "' | " + base64DecodeCmd(osName) + " >> " + posixQuote(path), nil
}

// removeCmd returns a best-effort command that deletes path.
func removeCmd(osName, path string) (string, error) {
	if isWindows(osName) {
		p, err := psQuote(path)
		if err != nil {
			return "", err
		}
		return "powershell -NoProfile -Command \"Remove-Item -Force -LiteralPath '" + p + "' -ErrorAction SilentlyContinue\"", nil
	}
	return "rm -f " + posixQuote(path), nil
}

var (
	dirFileRe = regexp.MustCompile(`^(DIR|FILE)\s+\d+\s+.+$`)
	winTable  = regexp.MustCompile(`^\s*(True|False)\s+(\d*)\s*(.+?)\s*$`)
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

	win := false
	for _, l := range lines {
		if strings.Contains(l, "PSIsContainer") {
			win = true
			break
		}
	}
	if win {
		var items []string
		for _, l := range lines {
			if strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "PSIsContainer") || strings.HasPrefix(strings.TrimSpace(l), "-") {
				continue
			}
			m := winTable.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			name := strings.TrimRight(m[3], " ")
			if name == "." || name == ".." {
				continue
			}
			typ := "FILE"
			if m[1] == "True" {
				typ = "DIR"
			}
			size := m[2]
			if size == "" || typ == "DIR" {
				size = "0"
			}
			items = append(items, typ+" "+size+" "+name)
		}
		if len(items) > 0 {
			return strings.Join(items, "\n")
		}
		return output
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

// actionToShell converts an LLM action into one shell command. edit_file is
// handled entirely by Backend.editFile (sentinel-wrapped read + write) and
// never reaches this function.
func actionToShell(action string, params map[string]any, osName string) (string, error) {
	s := func(k string) string { return paramString(params[k]) }
	switch action {
	case "exec_cmd":
		return s("command"), nil
	case "get_cwd":
		return getCwdCmd(osName), nil
	case "list_dir":
		p := s("path")
		if p == "" {
			p = "."
		}
		return listDirCmd(osName, p)
	case "read_file":
		return readFileCmd(osName, s("path"), s("start_line"), s("end_line"))
	case "write_file":
		b64 := base64.StdEncoding.EncodeToString([]byte(s("content")))
		return writeBase64Cmd(osName, s("path"), b64)
	case "create_file":
		return createFileCmd(osName, s("path"))
	case "delete_file", "delete_dir":
		return deleteCmd(osName, s("path"))
	case "rename_file", "rename_dir":
		return renameCmd(osName, s("path"), s("new_name"))
	case "make_dir":
		return makeDirCmd(osName, s("path"))
	case "copy":
		return copyCmd(osName, s("src"), s("dest"))
	case "move":
		return moveCmd(osName, s("src"), s("dest"))
	default:
		return "", fmt.Errorf("unknown action: %s", action)
	}
}

// paramString renders a tool parameter for the shell command builders. It
// delegates to command.String so the control end has a single implementation.
func paramString(v any) string { return command.String(v) }

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
