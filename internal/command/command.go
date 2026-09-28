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

// actionMeta carries the control-end-only metadata for an action. The wire
// code, name and ordered parameter list live in protocol.Actions, which is the
// single source of truth, so they can never drift from the protocol.
type actionMeta struct {
	lowRisk     bool
	description string
	required    map[string]bool
	paramDesc   map[string]string
}

func meta(lowRisk bool, description string, required []string, paramDesc map[string]string) actionMeta {
	req := make(map[string]bool, len(required))
	for _, r := range required {
		req[r] = true
	}
	return actionMeta{lowRisk: lowRisk, description: description, required: req, paramDesc: paramDesc}
}

var actionMetas = map[string]actionMeta{
	"get_cwd": meta(true,
		"Get the absolute current working directory on the target.", nil, nil),
	"list_dir": meta(true,
		"List the contents of a directory on the target.", nil,
		map[string]string{"path": "Directory path (default '.')."}),
	"read_file": meta(true,
		"Read a text file on the target, optionally a line range. Whole-file reads are truncated to 51200 characters.",
		[]string{"path"},
		map[string]string{
			"path":       "File path.",
			"start_line": "1-based first line (0/empty = from start).",
			"end_line":   "1-based last line (0/empty = to end).",
		}),
	"write_file": meta(false,
		"Create or overwrite a file with the given content.", []string{"path", "content"},
		map[string]string{"path": "File path.", "content": "Full file content."}),
	"create_file": meta(false,
		"Create an empty file.", []string{"path"},
		map[string]string{"path": "File path."}),
	"delete_file": meta(false,
		"Delete a file.", []string{"path"},
		map[string]string{"path": "File path."}),
	"delete_dir": meta(false,
		"Delete a directory recursively.", []string{"path"},
		map[string]string{"path": "Directory path."}),
	"make_dir": meta(false,
		"Create a directory (parents included).", []string{"path"},
		map[string]string{"path": "Directory path."}),
	"rename_file": meta(false,
		"Rename a file.", []string{"path", "new_name"},
		map[string]string{"path": "File path.", "new_name": "New file name (no directory)."}),
	"rename_dir": meta(false,
		"Rename a directory.", []string{"path", "new_name"},
		map[string]string{"path": "Directory path.", "new_name": "New directory name."}),
	"edit_file": meta(false,
		"Edit a text file by inserting, deleting or replacing lines.", []string{"path", "operation"},
		map[string]string{
			"path":       "File path.",
			"operation":  "One of: add, del, modify.",
			"start_line": "1-based start line.",
			"end_line":   "1-based end line (del/modify).",
			"content":    "Content for add/modify.",
		}),
	"copy": meta(false,
		"Copy a file or directory.", []string{"src", "dest"},
		map[string]string{"src": "Source path.", "dest": "Destination path."}),
	"move": meta(false,
		"Move a file or directory.", []string{"src", "dest"},
		map[string]string{"src": "Source path.", "dest": "Destination path."}),
	"exec_cmd": meta(false,
		"Execute a shell command on the target and return its output.", []string{"command"},
		map[string]string{"command": "The shell command to run."}),
}

// Specs lists every action, derived from the wire protocol action table so the
// code / name / ordered parameters are defined in exactly one place.
var Specs []Spec

var byName = map[string]Spec{}

func init() {
	Specs = make([]Spec, 0, len(protocol.Actions))
	for _, a := range protocol.Actions {
		m := actionMetas[a.Name]
		params := make([]Param, len(a.Params))
		for i, name := range a.Params {
			params[i] = Param{Name: name, Required: m.required[name], Description: m.paramDesc[name]}
		}
		Specs = append(Specs, Spec{
			Name:        a.Name,
			Code:        a.Code,
			LowRisk:     m.lowRisk,
			Description: m.description,
			Params:      params,
		})
	}
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
