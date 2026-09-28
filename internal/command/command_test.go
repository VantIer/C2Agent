package command

import (
	"testing"

	"c2agent/internal/protocol"
)

// The action table must stay in sync with the wire protocol: same code and the
// same ordered parameter names (the native backend relies on the order).
func TestSpecsMatchProtocolActions(t *testing.T) {
	for _, s := range Specs {
		a, ok := protocol.ActionByName(s.Name)
		if !ok {
			t.Errorf("action %q has no protocol code", s.Name)
			continue
		}
		if a.Code != s.Code {
			t.Errorf("%s: code mismatch: spec=%#x protocol=%#x", s.Name, s.Code, a.Code)
		}
		if len(a.Params) != len(s.Params) {
			t.Errorf("%s: param count mismatch: spec=%d protocol=%d", s.Name, len(s.Params), len(a.Params))
			continue
		}
		for i := range a.Params {
			if a.Params[i] != s.Params[i].Name {
				t.Errorf("%s: param %d mismatch: spec=%q protocol=%q", s.Name, i, s.Params[i].Name, a.Params[i])
			}
		}
	}
}

func TestCheckSafety(t *testing.T) {
	cases := []struct {
		cmd  string
		safe bool
	}{
		{"ls -la", true},
		{"rm -rf /tmp/x", true},
		{"rm -rf /", false},
		{"rm -rf /*", false},
		{"format c:", false},
		{"format  c:", false}, // extra whitespace
		{"mkfs.ext4 /dev/sda", false},
		{"dd if=/dev/zero of=/dev/sda", false},
	}
	for _, c := range cases {
		if got := CheckSafety("exec_cmd", map[string]any{"command": c.cmd}); got != c.safe {
			t.Errorf("CheckSafety(%q) = %v, want %v", c.cmd, got, c.safe)
		}
	}
	// Non-exec actions are not checked here.
	if !CheckSafety("list_dir", map[string]any{"path": "/"}) {
		t.Error("non-exec action should not be blocked by CheckSafety")
	}
}
