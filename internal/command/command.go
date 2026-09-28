// Package command is the single source of truth for the actions the LLM can
// invoke: their protocol code, parameters, risk level, description and safety
// checks. The LLM tool schema is generated from this table.
package command

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"c2agent/internal/protocol"
)

// Param describes one action parameter.
type Param struct {
	Name        string
	Required    bool
	Description string
}

// Spec describes an action.
type Spec struct {
	Name        string
	Code        uint8
	LowRisk     bool
	Description string
	Params      []Param
}

// Specs lists every action in a stable order.
var Specs = []Spec{
	{"get_cwd", protocol.CmdGetCwd, true,
		"Get the absolute current working directory on the target.",
		nil},
	{"list_dir", protocol.CmdListDir, true,
		"List the contents of a directory on the target.",
		[]Param{{"path", false, "Directory path (default '.')."}}},
	{"read_file", protocol.CmdReadFile, true,
		"Read a text file on the target, optionally a line range. Whole-file reads are truncated to 51200 characters.",
		[]Param{
			{"path", true, "File path."},
			{"start_line", false, "1-based first line (0/empty = from start)."},
			{"end_line", false, "1-based last line (0/empty = to end)."},
		}},
	{"write_file", protocol.CmdWriteFile, false,
		"Create or overwrite a file with the given content.",
		[]Param{{"path", true, "File path."}, {"content", true, "Full file content."}}},
	{"create_file", protocol.CmdCreateFile, false,
		"Create an empty file.",
		[]Param{{"path", true, "File path."}}},
	{"delete_file", protocol.CmdDeleteFile, false,
		"Delete a file.",
		[]Param{{"path", true, "File path."}}},
	{"delete_dir", protocol.CmdDeleteDir, false,
		"Delete a directory recursively.",
		[]Param{{"path", true, "Directory path."}}},
	{"make_dir", protocol.CmdMakeDir, false,
		"Create a directory (parents included).",
		[]Param{{"path", true, "Directory path."}}},
	{"rename_file", protocol.CmdRenameFile, false,
		"Rename a file.",
		[]Param{{"path", true, "File path."}, {"new_name", true, "New file name (no directory)."}}},
	{"rename_dir", protocol.CmdRenameDir, false,
		"Rename a directory.",
		[]Param{{"path", true, "Directory path."}, {"new_name", true, "New directory name."}}},
	{"edit_file", protocol.CmdEditFile, false,
		"Edit a text file by inserting, deleting or replacing lines.",
		[]Param{
			{"path", true, "File path."},
			{"operation", true, "One of: add, del, modify."},
			{"start_line", false, "1-based start line."},
			{"end_line", false, "1-based end line (del/modify)."},
			{"content", false, "Content for add/modify."},
		}},
	{"copy", protocol.CmdCopy, false,
		"Copy a file or directory.",
		[]Param{{"src", true, "Source path."}, {"dest", true, "Destination path."}}},
	{"move", protocol.CmdMove, false,
		"Move a file or directory.",
		[]Param{{"src", true, "Source path."}, {"dest", true, "Destination path."}}},
	{"exec_cmd", protocol.CmdExecCmd, false,
		"Execute a shell command on the target and return its output.",
		[]Param{{"command", true, "The shell command to run."}}},
}

var byName = map[string]Spec{}

func init() {
	for _, s := range Specs {
		byName[s.Name] = s
	}
}

// SpecByName returns the spec for an action.
func SpecByName(name string) (Spec, bool) {
	s, ok := byName[name]
	return s, ok
}

// isLowRisk reports whether an action is read-only / listing.
func isLowRisk(name string) bool {
	s, ok := byName[name]
	return ok && s.LowRisk
}

// RequiresAuth decides whether an action needs explicit user authorization
// under the given auth_mode (0=N-Auto, 1=H-Auto, 2=F-Auto).
func RequiresAuth(mode int, name string) bool {
	switch mode {
	case 2:
		return false
	case 1:
		return !isLowRisk(name)
	default:
		return true
	}
}

// Forbidden substrings / patterns for shell commands.
var (
	forbiddenPatterns = []string{"format c:", "mkfs.", "dd if=/dev/zero"}
	// rm with any flags, capturing the target operand (handles separated flags
	// like `rm -r -f /` and long flags like `--recursive`).
	rmArgRe = regexp.MustCompile(`(?i)\brm\b(?:\s+-{1,2}[a-z]+)*\s+(\S+)`)
)

// CheckSafety blocks clearly destructive shell commands. This is a best-effort
// blacklist, not a security boundary.
func CheckSafety(action string, params map[string]any) bool {
	if action != "exec_cmd" {
		return true
	}
	cmd := String(params["command"])
	// Collapse runs of whitespace so e.g. "format  c:" is still caught.
	norm := strings.ToLower(strings.Join(strings.Fields(cmd), " "))
	for _, p := range forbiddenPatterns {
		if strings.Contains(norm, p) {
			return false
		}
	}
	return !rmHitsRoot(cmd)
}

// rmHitsRoot reports whether an `rm -rf` targets the filesystem root or `*`.
func rmHitsRoot(cmd string) bool {
	for _, m := range rmArgRe.FindAllStringSubmatch(cmd, -1) {
		arg := strings.Trim(m[1], `"'`)
		if arg == "/" || arg == "/*" || arg == "*" {
			return true
		}
	}
	return false
}

// String renders an action parameter value as the string sent on the wire.
func String(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}
