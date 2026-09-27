package shell

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"c2agent/internal/command"
)

func isWindows(osName string) bool { return osName == "Windows" }

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
// cmd first when the host shell is cmd).
func psQuote(s string) (string, error) {
	if cmdVarRe.MatchString(s) {
		return "", fmt.Errorf("cmd cannot safely handle '%%' variable patterns in path: %q", s)
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

func readFileCmd(osName, path, startLine, endLine string) (string, error) {
	if isWindows(osName) {
		p, err := psQuote(path)
		if err != nil {
			return "", err
		}
		return "powershell -NoProfile -Command \"[Convert]::ToBase64String([IO.File]::ReadAllBytes('" + p + "'))\"", nil
	}
	start := strings.TrimSpace(startLine)
	if start == "" || start == "0" {
		return "cat " + posixQuote(path), nil
	}
	s, err := strconv.Atoi(start)
	if err != nil || s < 1 {
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
	return "mv " + posixQuote(path) + " " + posixQuote(newName), nil
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
		return "powershell -NoProfile -Command \"New-Item -ItemType File -Force -Path '" + p + "' | Out-Null\"", nil
	}
	return "touch " + posixQuote(path), nil
}

const (
	posixChunk = 100000
	winChunk   = 8000
)

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
		dest, err := psQuote(destPath)
		if err != nil {
			return "", err
		}
		cs := chunks(b64, winChunk)
		if len(cs) == 0 {
			return "powershell -NoProfile -Command \"[IO.File]::WriteAllBytes('" + dest + "',[byte[]]@())\"", nil
		}
		var stmts []string
		stmts = append(stmts, "[IO.File]::WriteAllBytes('"+dest+"',[Convert]::FromBase64String('"+cs[0]+"'))")
		for _, c := range cs[1:] {
			stmts = append(stmts, "[IO.File]::AppendAllBytes('"+dest+"',[Convert]::FromBase64String('"+c+"'))")
		}
		return "powershell -NoProfile -Command \"" + strings.Join(stmts, ";") + "\"", nil
	}
	dest := posixQuote(destPath)
	cs := chunks(b64, posixChunk)
	if len(cs) == 0 {
		return "printf '' > " + dest, nil
	}
	parts := []string{"printf '%s' '" + cs[0] + "' | base64 -d > " + dest}
	for _, c := range cs[1:] {
		parts = append(parts, "printf '%s' '"+c+"' | base64 -d >> "+dest)
	}
	return strings.Join(parts, "; "), nil
}

// readBase64Cmd builds a command whose output is the base64 of srcPath.
func readBase64Cmd(osName, srcPath string) (string, error) {
	if isWindows(osName) {
		src, err := psQuote(srcPath)
		if err != nil {
			return "", err
		}
		return "powershell -NoProfile -Command \"[Convert]::ToBase64String([IO.File]::ReadAllBytes('" + src + "'))\"", nil
	}
	return "base64 " + posixQuote(srcPath), nil
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
// handled by the backend (read/modify/write) and returns ErrHandled.
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

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func atoiSafe(s string) (int, error) { return strconv.Atoi(strings.TrimSpace(s)) }

var promptRe = regexp.MustCompile(`^\s*(?:\S.*)?[$#>]\s*$`)

// stripShellResponse removes the echoed command, the marker-echo line and
// shell prompts from a raw reverse-shell reply.
func stripShellResponse(lines []string, marker, lastCommand string) string {
	markerEcho := "echo " + marker
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if !strings.Contains(l, markerEcho) {
			out = append(out, l)
		}
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

// applyEdit performs add/del/modify line editing, mirroring the native agent.
func applyEdit(content, operation, startLine, endLine, newContent string) (string, bool) {
	lines := splitKeepEnds(content)
	start := 0
	if strings.TrimSpace(startLine) != "" && strings.TrimSpace(startLine) != "0" {
		v, err := atoiSafe(startLine)
		if err != nil {
			return "", false
		}
		start = v - 1
		if start < 0 {
			start = 0
		}
	}
	end := len(lines)
	if strings.TrimSpace(endLine) != "" && strings.TrimSpace(endLine) != "0" {
		if v, err := atoiSafe(endLine); err == nil {
			end = v
		}
	}
	switch operation {
	case "add":
		if start > len(lines) {
			start = len(lines)
		}
		lines = append(lines[:start], append([]string{newContent + "\n"}, lines[start:]...)...)
	case "del":
		if start >= len(lines) {
			return "", false
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
			return "", false
		}
		if end > len(lines) {
			end = len(lines)
		}
		if end < start {
			end = start
		}
		lines = append(lines[:start], append([]string{newContent + "\n"}, lines[end:]...)...)
	default:
		return "", false
	}
	return strings.Join(lines, ""), true
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
