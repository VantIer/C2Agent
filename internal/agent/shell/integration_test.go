package shell

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	"c2agent/internal/agent"
)

// fakeShell emulates a POSIX reverse shell: it echoes the marker line and
// returns canned output for a few commands.
func fakeShell(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Errorf("dial: %v", err)
		return
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "echo __C2AGENT_") {
			conn.Write([]byte(strings.TrimPrefix(line, "echo ") + "\n"))
			continue
		}
		switch {
		case strings.HasPrefix(line, "uname"):
			conn.Write([]byte("Linux fake\n"))
		case line == "hostname":
			conn.Write([]byte("fake-host\n"))
		case strings.HasPrefix(line, "ls"):
			conn.Write([]byte("total 8\n-rw-r--r-- 1 u u 12 Aug  1 10:00 a.txt\ndrwxr-xr-x 2 u u 4096 Aug  1 10:00 docs\n"))
		case strings.HasPrefix(line, "echo "):
			conn.Write([]byte(strings.TrimPrefix(line, "echo ") + "\n"))
		default:
			conn.Write([]byte("ok\n"))
		}
	}
}

func TestShellEndToEnd(t *testing.T) {
	reg := agent.NewRegistry()
	srv := NewServer(ServerOptions{
		Registry:      reg,
		Host:          "127.0.0.1",
		Port:          0,
		BotPrefix:     "BOT-",
		CmdTimeout:    5 * time.Second,
		TimeoutAction: "disconnect",
		QueueCapacity: 16,
	})
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()

	go fakeShell(t, srv.Addr().String())

	var ag *agent.Agent
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ag = reg.Get("BOT-001"); ag != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ag == nil {
		t.Fatal("shell bot was not registered")
	}
	if ag.OS != "Linux" || ag.Hostname != "fake-host" {
		t.Fatalf("identity mismatch: os=%q host=%q", ag.OS, ag.Hostname)
	}

	list := agent.NewJob(agent.JobAction)
	list.Action = "list_dir"
	list.Params = map[string]any{"path": "."}
	if err := ag.Enqueue(list); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	r := <-list.Result
	if r.Err != nil {
		t.Fatalf("list_dir error: %v", r.Err)
	}
	if !strings.Contains(r.Output, "FILE 12 a.txt") || !strings.Contains(r.Output, "DIR 0 docs") {
		t.Fatalf("unexpected listing: %q", r.Output)
	}

	exec := agent.NewJob(agent.JobAction)
	exec.Action = "exec_cmd"
	exec.Params = map[string]any{"command": "echo hi"}
	if err := ag.Enqueue(exec); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	er := <-exec.Result
	if er.Err != nil {
		t.Fatalf("exec_cmd error: %v", er.Err)
	}
	if strings.TrimSpace(er.Output) != "hi" {
		t.Fatalf("unexpected exec output: %q", er.Output)
	}
}
