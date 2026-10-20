package cli

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go"

	"c2agent/internal/agent"
	"c2agent/internal/config"
	"c2agent/internal/engine"
	"c2agent/internal/llm"
)

type fakeLLM struct{}

func (fakeLLM) Chat(context.Context, []llm.Message, []openai.ChatCompletionToolParam, func(string)) (*llm.ChatResult, error) {
	return &llm.ChatResult{Content: "ok"}, nil
}

type fakeBackend struct{}

func (fakeBackend) Execute(context.Context, string, map[string]any) (string, error) {
	return "ran", nil
}
func (fakeBackend) Upload(context.Context, string, string) (string, error) { return "up", nil }
func (fakeBackend) Download(context.Context, string, string) (string, error) {
	return "/tmp/file", nil
}
func (fakeBackend) Shutdown(context.Context) error { return nil }
func (fakeBackend) Close() error                   { return nil }

func newTestCLI(t *testing.T) (*CLI, *engine.Engine) {
	t.Helper()
	cfg := config.Default()
	reg := agent.NewRegistry()
	ag := agent.New(agent.Options{
		ID: "a1", Kind: agent.KindShell, Hostname: "host", OS: "Linux",
		Backend: fakeBackend{}, CmdTimeout: time.Second, TimeoutAction: "fail",
	})
	reg.Register(ag)
	t.Cleanup(ag.Close)
	eng := engine.NewWithClient(cfg, reg, fakeLLM{})
	return &CLI{cfg: cfg, eng: eng}, eng
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	data, _ := io.ReadAll(r)
	return string(data)
}

func TestHelpAndQuit(t *testing.T) {
	c, _ := newTestCLI(t)
	out := captureStdout(t, func() {
		if c.command("/help") {
			t.Error("/help should not exit")
		}
	})
	if !strings.Contains(out, "/agents") {
		t.Fatalf("help output missing /agents: %q", out)
	}
	if !c.command("/quit") {
		t.Fatal("/quit should exit")
	}
}

func TestAuthAndTarget(t *testing.T) {
	c, eng := newTestCLI(t)
	captureStdout(t, func() { c.command("/auth 2") })
	if eng.AuthMode() != 2 {
		t.Fatalf("auth mode = %d, want 2", eng.AuthMode())
	}

	captureStdout(t, func() { c.command("/target a1") })
	if eng.Registry().ActiveID() != "a1" {
		t.Fatalf("active agent = %q", eng.Registry().ActiveID())
	}
	if eng.ActiveSession("a1") == nil {
		t.Fatal("/target should ensure an active session")
	}
}

func TestNewUseAndExec(t *testing.T) {
	c, eng := newTestCLI(t)
	captureStdout(t, func() { c.command("/new review") })
	sessions := eng.ListSessions("a1")
	if len(sessions) != 1 || sessions[0].Title != "review" {
		t.Fatalf("unexpected sessions: %#v", sessions)
	}
	captureStdout(t, func() { c.command("/use 0") })
	if got := eng.ActiveSession("a1"); got == nil || got.ID != sessions[0].ID {
		t.Fatalf("active session = %#v", got)
	}

	out := captureStdout(t, func() { c.command("/exec echo hi") })
	if !strings.Contains(out, "ran") {
		t.Fatalf("exec output = %q", out)
	}
}
