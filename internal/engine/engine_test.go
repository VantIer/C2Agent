package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go"

	"c2agent/internal/agent"
	"c2agent/internal/config"
	"c2agent/internal/llm"
)

type errLLM struct{}

func (errLLM) Chat(context.Context, []llm.Message, []openai.ChatCompletionToolParam, func(string)) (*llm.ChatResult, error) {
	return nil, errors.New("boom")
}

// An LLM error must still emit a terminal "done" event (otherwise clients that
// wait for it, e.g. the CLI, would block forever).
func TestErrorEmitsDone(t *testing.T) {
	reg := newTestAgent(t, &fakeBackend{})
	cfg := config.Default()
	cfg.Policy.AuthMode = 2
	eng := NewWithClient(cfg, reg, errLLM{})
	s, _ := eng.NewSession("a1", "")
	ch, _, unsub := eng.Subscribe("", s.ID)
	defer unsub()
	if err := eng.BeginChat(s.ID, "hi"); err != nil {
		t.Fatal(err)
	}
	sawLLMError := false
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-ch:
			switch ev["type"] {
			case "llm_error":
				sawLLMError = true
			case "error":
				t.Fatal("LLM failure must not use the generic 'error' event")
			case "done":
				if !sawLLMError {
					t.Fatal("llm_error must be emitted before the terminal 'done'")
				}
				return
			}
		case <-deadline:
			t.Fatal("no terminal 'done' event after an LLM error")
		}
	}
}

type fakeBackend struct {
	mu      sync.Mutex
	actions []string
}

func (f *fakeBackend) Execute(_ context.Context, action string, _ map[string]any) (string, error) {
	f.mu.Lock()
	f.actions = append(f.actions, action)
	f.mu.Unlock()
	return "OUT:" + action, nil
}
func (f *fakeBackend) Upload(context.Context, string, string) (string, error) { return "up", nil }
func (f *fakeBackend) Download(context.Context, string, string) (string, error) {
	return "dl", nil
}
func (f *fakeBackend) Shutdown(context.Context) error { return nil }
func (f *fakeBackend) Close() error                   { return nil }

func (f *fakeBackend) called(action string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.actions {
		if a == action {
			return true
		}
	}
	return false
}

type fakeLLM struct {
	mu    sync.Mutex
	turns []*llm.ChatResult
	idx   int
}

func (f *fakeLLM) Chat(context.Context, []llm.Message, []openai.ChatCompletionToolParam, func(string)) (*llm.ChatResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idx < len(f.turns) {
		r := f.turns[f.idx]
		f.idx++
		return r, nil
	}
	return &llm.ChatResult{Content: "done", Raw: llm.AssistantMessage("done", nil)}, nil
}

func toolTurn(content, id, name, args string) *llm.ChatResult {
	calls := []llm.ToolCall{{ID: id, Name: name, Arguments: args}}
	return &llm.ChatResult{Content: content, ToolCalls: calls, Raw: llm.AssistantMessage(content, calls)}
}

func newTestAgent(t *testing.T, br *fakeBackend) *agent.Registry {
	t.Helper()
	reg := agent.NewRegistry()
	ag := agent.New(agent.Options{ID: "a1", Kind: agent.KindShell, OS: "Linux", Backend: br})
	reg.Register(ag)
	return reg
}

func waitIdle(t *testing.T, eng *Engine, id string) Snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snap := eng.GetSession(id).Snapshot()
		if !snap.Running {
			return snap
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("session did not become idle")
	return Snapshot{}
}

func TestConversationExecutesTool(t *testing.T) {
	br := &fakeBackend{}
	reg := newTestAgent(t, br)
	cfg := config.Default()
	cfg.Policy.AuthMode = 2
	cfg.Policy.RoundLimit = 5
	cfg.LLM.SystemPrompt = "os={system_name}"
	el := &fakeLLM{turns: []*llm.ChatResult{
		toolTurn("", "c1", "list_dir", `{"path":"."}`),
		{Content: "done", Raw: llm.AssistantMessage("done", nil)},
	}}
	eng := NewWithClient(cfg, reg, el)
	s, err := eng.NewSession("a1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.BeginChat(s.ID, "list files"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, eng, s.ID)

	if !br.called("list_dir") {
		t.Fatal("list_dir was not executed on the agent")
	}
	// user + tool result + assistant(done)
	if got := len(eng.GetTranscript(s.ID)); got < 3 {
		t.Fatalf("expected >=3 transcript entries, got %d", got)
	}
}

func TestAuthDenied(t *testing.T) {
	br := &fakeBackend{}
	reg := newTestAgent(t, br)
	cfg := config.Default()
	cfg.Policy.AuthMode = 0 // N-Auto: everything needs auth
	cfg.Policy.RoundLimit = 5
	el := &fakeLLM{turns: []*llm.ChatResult{
		toolTurn("", "c1", "exec_cmd", `{"command":"id"}`),
	}}
	eng := NewWithClient(cfg, reg, el)
	s, _ := eng.NewSession("a1", "")
	if err := eng.BeginChat(s.ID, "run id"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if eng.GetSession(s.ID).Snapshot().Pending != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if eng.GetSession(s.ID).Snapshot().Pending == nil {
		t.Fatal("expected a pending authorization")
	}
	if !eng.SubmitAuth(s.ID, false) {
		t.Fatal("SubmitAuth returned false")
	}
	waitIdle(t, eng, s.ID)
	if br.called("exec_cmd") {
		t.Fatal("denied command must not execute")
	}
	if el.idx != 1 {
		t.Fatalf("denial must end the turn: expected 1 LLM call, got %d", el.idx)
	}
	marked := false
	for _, tr := range eng.GetTranscript(s.ID) {
		if strings.Contains(tr.Text, "Denied") {
			marked = true
		}
	}
	if !marked {
		t.Fatal("transcript does not record the denial")
	}
}

// The snapshot must expose the pending command with lowercase JSON keys so
// the web UI can render the authorization modal after re-attaching.
func TestSnapshotPendingJSON(t *testing.T) {
	s := &Session{
		ID: "s1", AgentID: "a1",
		pending: &PendingCommand{ToolCallID: "c1", Action: "exec_cmd", Params: map[string]any{"command": "id"}},
	}
	data, err := json.Marshal(s.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	txt := string(data)
	if !strings.Contains(txt, `"action":"exec_cmd"`) || !strings.Contains(txt, `"command":"id"`) {
		t.Fatalf("pending not serialized with lowercase keys: %s", txt)
	}
}

// An execution failure (timeout / disconnect) must be reported as
// "execution_error" so the UI renders it as command output, not as an AI
// conversation message, and must never use the generic "error" event.
type errBackend struct{ fakeBackend }

func (b *errBackend) Execute(context.Context, string, map[string]any) (string, error) {
	return "", agent.NewNetworkError("boom")
}

func TestExecutionErrorEmitsEvent(t *testing.T) {
	reg := agent.NewRegistry()
	reg.Register(agent.New(agent.Options{ID: "a1", Kind: agent.KindShell, OS: "Linux", Backend: &errBackend{}}))
	cfg := config.Default()
	cfg.Policy.AuthMode = 2
	cfg.Policy.RoundLimit = 5
	el := &fakeLLM{turns: []*llm.ChatResult{
		toolTurn("", "c1", "exec_cmd", `{"command":"sleep 100"}`),
	}}
	eng := NewWithClient(cfg, reg, el)
	s, _ := eng.NewSession("a1", "")
	ch, _, unsub := eng.Subscribe("", s.ID)
	defer unsub()
	if err := eng.BeginChat(s.ID, "go"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-ch:
			switch ev["type"] {
			case "execution_error":
				if ev["error"] == nil {
					t.Fatal("execution_error without an error field")
				}
				// The failure must be persisted as a result entry so a page
				// refresh replays the same thing (and never "[Network Error]").
				tr := eng.GetTranscript(s.ID)
				found := false
				for _, e := range tr {
					if e.Role == "result" && strings.Contains(e.Text, "Error: network error: boom") {
						found = true
					}
					if strings.Contains(e.Text, "[Network Error]") {
						t.Fatalf("stale network-error transcript entry: %q", e.Text)
					}
				}
				if !found {
					t.Fatalf("execution error not persisted as a result: %+v", tr)
				}
				return
			case "error":
				t.Fatal("job failure must not use the generic 'error' event")
			}
		case <-deadline:
			t.Fatal("no execution_error event emitted")
		}
	}
}

// An offline agent must be reported as "agent_error" (rendered like a command
// result), never as the generic "error" conversation message, and the turn's
// user message plus the error must be persisted for history replay.
func TestAgentOfflineEmitsAgentError(t *testing.T) {
	br := &fakeBackend{}
	reg := newTestAgent(t, br)
	cfg := config.Default()
	cfg.Policy.AuthMode = 2
	eng := NewWithClient(cfg, reg, &fakeLLM{})
	s, err := eng.NewSession("a1", "")
	if err != nil {
		t.Fatal(err)
	}
	reg.UnregisterAgent(reg.Get("a1")) // agent goes offline before the turn

	ch, _, unsub := eng.Subscribe("", s.ID)
	defer unsub()
	if err := eng.BeginChat(s.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-ch:
			switch ev["type"] {
			case "agent_error":
				tr := eng.GetTranscript(s.ID)
				var hasUser, hasErr bool
				for _, e := range tr {
					if e.Role == "user" && e.Text == "hello" {
						hasUser = true
					}
					if e.Role == "result" && strings.Contains(e.Text, "agent offline") {
						hasErr = true
					}
				}
				if !hasUser || !hasErr {
					t.Fatalf("transcript missing user/error entries: %+v", tr)
				}
				return
			case "error":
				t.Fatal("agent offline must not use the generic 'error' event")
			}
		case <-deadline:
			t.Fatal("no agent_error event emitted")
		}
	}
}

// A disconnected agent must not take its sessions down: they are kept so a
// reconnect with the same id can resume them (session resilience).
func TestSessionsSurviveAgentDisconnect(t *testing.T) {
	reg := newTestAgent(t, &fakeBackend{})
	cfg := config.Default()
	eng := NewWithClient(cfg, reg, &fakeLLM{})
	s, err := eng.NewSession("a1", "")
	if err != nil {
		t.Fatal(err)
	}
	reg.UnregisterAgent(reg.Get("a1"))
	time.Sleep(50 * time.Millisecond)
	if eng.GetSession(s.ID) == nil {
		t.Fatal("session was removed when its agent disconnected")
	}
}

func TestMultiSessionSameAgent(t *testing.T) {
	br := &fakeBackend{}
	reg := newTestAgent(t, br)
	cfg := config.Default()
	cfg.Policy.AuthMode = 2
	cfg.Policy.RoundLimit = 5
	el := &fakeLLM{turns: []*llm.ChatResult{
		toolTurn("", "c1", "list_dir", `{"path":"."}`),
		toolTurn("", "c2", "list_dir", `{"path":"."}`),
		{Content: "done", Raw: llm.AssistantMessage("done", nil)},
		{Content: "done", Raw: llm.AssistantMessage("done", nil)},
	}}
	eng := NewWithClient(cfg, reg, el)
	s1, _ := eng.NewSession("a1", "one")
	s2, _ := eng.NewSession("a1", "two")

	if err := eng.BeginChat(s1.ID, "hi1"); err != nil {
		t.Fatal(err)
	}
	if err := eng.BeginChat(s2.ID, "hi2"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, eng, s1.ID)
	waitIdle(t, eng, s2.ID)

	if !br.called("list_dir") {
		t.Fatal("list_dir not executed")
	}
	if len(eng.ListSessions("a1")) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(eng.ListSessions("a1")))
	}
}
