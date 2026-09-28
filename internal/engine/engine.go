// Package engine orchestrates multi-session LLM conversations, authorization,
// round limits and event broadcasting on top of the agent registry.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/openai/openai-go"

	"c2agent/internal/agent"
	"c2agent/internal/command"
	"c2agent/internal/config"
	"c2agent/internal/llm"
)

// LLMChatter is the subset of llm.Client used by the engine.
type LLMChatter interface {
	Chat(ctx context.Context, messages []llm.Message, tools []openai.ChatCompletionToolParam, onContent func(string)) (*llm.ChatResult, error)
}

// Engine owns sessions and drives conversations.
type Engine struct {
	cfg      *config.Config
	registry *agent.Registry
	llm      LLMChatter
	tools    []openai.ChatCompletionToolParam
	hub      *eventHub

	mu       sync.RWMutex
	sessions map[string]*Session
	byAgent  map[string]map[string]*Session
	active   map[string]string
	seq      uint64

	authMu   sync.Mutex
	authMode int
}

// New creates an engine using the official OpenAI-compatible client.
func New(cfg *config.Config, reg *agent.Registry) *Engine {
	client := llm.New(cfg.LLM.APIBase, cfg.LLM.APIKey, cfg.LLM.Model, cfg.LLM.Temperature, cfg.LLM.Stream)
	return NewWithClient(cfg, reg, client)
}

// NewWithClient creates an engine with an injected chat client (for tests).
func NewWithClient(cfg *config.Config, reg *agent.Registry, client LLMChatter) *Engine {
	return &Engine{
		cfg:      cfg,
		registry: reg,
		llm:      client,
		tools:    llm.BuildTools(),
		hub:      newEventHub(),
		sessions: map[string]*Session{},
		byAgent:  map[string]map[string]*Session{},
		active:   map[string]string{},
		authMode: cfg.Policy.AuthMode,
	}
}

// Registry exposes the underlying agent registry.
func (e *Engine) Registry() *agent.Registry { return e.registry }

// NewSession creates a session bound to an agent.
func (e *Engine) NewSession(agentID, title string) (*Session, error) {
	if e.registry.Get(agentID) == nil {
		return nil, fmt.Errorf("no such agent: %s", agentID)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.newSessionLocked(agentID, title)
}

// newSessionLocked creates a session; e.mu must be held.
func (e *Engine) newSessionLocked(agentID, title string) (*Session, error) {
	if len(e.byAgent[agentID]) >= e.cfg.Policy.MaxSessionsPerAgent {
		return nil, fmt.Errorf("session limit reached for agent %s", agentID)
	}
	e.seq++
	id := fmt.Sprintf("s%d-%d", time.Now().UnixNano(), e.seq)
	if title == "" {
		title = fmt.Sprintf("Session %d", len(e.byAgent[agentID])+1)
	}
	s := &Session{ID: id, AgentID: agentID, Title: title, CreatedAt: time.Now(), phase: PhaseIdle}
	e.sessions[id] = s
	if e.byAgent[agentID] == nil {
		e.byAgent[agentID] = map[string]*Session{}
	}
	e.byAgent[agentID][id] = s
	if e.active[agentID] == "" {
		e.active[agentID] = id
	}
	return s, nil
}

// EnsureSession returns the active session of an agent, creating one if none.
// The lookup and creation are atomic so concurrent callers cannot create
// duplicate sessions.
func (e *Engine) EnsureSession(agentID string) (*Session, error) {
	if e.registry.Get(agentID) == nil {
		return nil, fmt.Errorf("no such agent: %s", agentID)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if id := e.active[agentID]; id != "" {
		if s := e.sessions[id]; s != nil {
			return s, nil
		}
	}
	return e.newSessionLocked(agentID, "")
}

// GetSession returns a session by id.
func (e *Engine) GetSession(id string) *Session {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.sessions[id]
}

// ListSessions returns an agent's sessions, oldest first.
func (e *Engine) ListSessions(agentID string) []*Session {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]*Session, 0, len(e.byAgent[agentID]))
	for _, s := range e.byAgent[agentID] {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// ActiveSession returns the active session of an agent.
func (e *Engine) ActiveSession(agentID string) *Session {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.sessions[e.active[agentID]]
}

// SetActiveSession switches the active session of an agent.
func (e *Engine) SetActiveSession(agentID, sessionID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.byAgent[agentID][sessionID]; !ok {
		return false
	}
	e.active[agentID] = sessionID
	return true
}

// CloseSession stops and removes a session.
func (e *Engine) CloseSession(id string) {
	s := e.GetSession(id)
	if s == nil {
		return
	}
	e.stopSession(s)
	if !e.waitRun(s, 5*time.Second) {
		// The conversation goroutine did not stop in time; keep the session
		// rather than deleting it out from under itself.
		return
	}
	e.mu.Lock()
	delete(e.sessions, id)
	if m := e.byAgent[s.AgentID]; m != nil {
		delete(m, id)
		if e.active[s.AgentID] == id {
			e.active[s.AgentID] = ""
			var oldest *Session
			for _, cand := range m {
				if oldest == nil || cand.CreatedAt.Before(oldest.CreatedAt) ||
					(cand.CreatedAt.Equal(oldest.CreatedAt) && cand.ID < oldest.ID) {
					oldest = cand
				}
			}
			if oldest != nil {
				e.active[s.AgentID] = oldest.ID
			}
		}
	}
	e.mu.Unlock()
}

// BeginChat starts the conversation loop for a session with a user message.
func (e *Engine) BeginChat(sessionID, message string) error {
	s := e.GetSession(sessionID)
	if s == nil {
		return fmt.Errorf("no such session: %s", sessionID)
	}
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("session is already running")
	}
	if s.stop {
		s.mu.Unlock()
		return fmt.Errorf("session is stopped; reset it first")
	}
	s.running = true
	s.runCtx, s.cancel = context.WithCancel(context.Background())
	s.done = make(chan struct{})
	s.mu.Unlock()
	go e.runConversation(s, message)
	return nil
}

// Stop interrupts a running conversation.
func (e *Engine) Stop(sessionID string) {
	if s := e.GetSession(sessionID); s != nil {
		e.stopSession(s)
	}
}

func (e *Engine) stopSession(s *Session) {
	s.mu.Lock()
	s.stop = true
	cancel := s.cancel
	job := s.job
	authCh := s.authCh
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if job != nil {
		job.Cancel()
	}
	if authCh != nil {
		select {
		case authCh <- false:
		default:
		}
	}
}

// ResetConversation clears a session and re-arms it.
func (e *Engine) ResetConversation(sessionID string) bool {
	s := e.GetSession(sessionID)
	if s == nil {
		return false
	}
	e.stopSession(s)
	if !e.waitRun(s, 5*time.Second) {
		// The conversation goroutine did not stop in time; do not clear its
		// state from under it.
		return false
	}
	s.mu.Lock()
	s.history = nil
	s.transcript = nil
	s.stop = false
	s.phase = PhaseIdle
	s.iter = 0
	s.turn = 0
	s.text = ""
	s.pending = nil
	s.mu.Unlock()
	return true
}

// waitRun waits until a session's running goroutine (if any) finishes. It
// returns false if the timeout elapses first, so callers can avoid mutating a
// session that may still be driven by runConversation.
func (e *Engine) waitRun(s *Session, timeout time.Duration) bool {
	s.mu.Lock()
	runDone := s.done
	s.mu.Unlock()
	if runDone == nil {
		return true
	}
	select {
	case <-runDone:
		return true
	case <-time.After(timeout):
		return false
	}
}

// SubmitAuth delivers a user's authorization decision for a waiting session.
func (e *Engine) SubmitAuth(sessionID string, ok bool) bool {
	s := e.GetSession(sessionID)
	if s == nil {
		return false
	}
	s.mu.Lock()
	ch := s.authCh
	s.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- ok:
		return true
	default:
		return false
	}
}

// authWaitTimeout bounds how long a session waits for an authorization
// decision before treating it as denied.
const authWaitTimeout = 5 * time.Minute

// awaitAuth blocks until the user decides (ch), the mode no longer requires
// authorization, the session stops, or the wait times out.
func (e *Engine) awaitAuth(s *Session, ch chan bool) bool {
	s.mu.Lock()
	ctx := s.runCtx
	s.mu.Unlock()
	deadline := time.NewTimer(authWaitTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(400 * time.Millisecond)
	defer tick.Stop()
	for {
		s.mu.Lock()
		pc := s.pending
		stopped := s.stop
		s.mu.Unlock()
		if stopped || pc == nil {
			return false
		}
		if !command.RequiresAuth(e.AuthMode(), pc.Action) {
			return true
		}
		select {
		case ok := <-ch:
			return ok
		case <-tick.C:
		case <-deadline.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// AuthMode returns the current authorization mode.
func (e *Engine) AuthMode() int {
	e.authMu.Lock()
	defer e.authMu.Unlock()
	return e.authMode
}

// SetAuthMode switches the authorization mode.
func (e *Engine) SetAuthMode(mode int) {
	if mode < 0 || mode > 2 {
		return
	}
	e.authMu.Lock()
	e.authMode = mode
	e.authMu.Unlock()
}

// RequiresAuth reports whether an action needs authorization right now.
func (e *Engine) RequiresAuth(action string) bool {
	return command.RequiresAuth(e.AuthMode(), action)
}

// RenderSystemPrompt fills {system_name} with the target OS.
func (e *Engine) RenderSystemPrompt(osName string) string {
	if osName == "" {
		osName = "Unknown"
	}
	return strings.ReplaceAll(e.cfg.LLM.SystemPrompt, "{system_name}", osName)
}

// GetTranscript returns the display transcript of a session.
func (e *Engine) GetTranscript(sessionID string) []TranscriptEntry {
	s := e.GetSession(sessionID)
	if s == nil {
		return nil
	}
	return s.transcriptCopy()
}

// Subscribe registers an event subscriber (empty filters match everything).
// The returned done channel is closed if the hub drops the subscriber (e.g. a
// stalled consumer), so callers can stop waiting instead of blocking forever.
func (e *Engine) Subscribe(agentID, sessionID string) (<-chan Event, <-chan struct{}, func()) {
	sub := e.hub.subscribe(agentID, sessionID)
	return sub.ch, sub.done, func() { e.hub.unsubscribe(sub) }
}

func (e *Engine) resolve(agentID string) *agent.Agent {
	if agentID == "" {
		return e.registry.Active()
	}
	return e.registry.Get(agentID)
}

func (e *Engine) wait(ag *agent.Agent, j *agent.Job) (string, error) {
	if err := ag.Enqueue(j); err != nil {
		return "", err
	}
	return waitJob(ag, j)
}

// waitJob blocks for a job's result. It prefers a result that is already
// available, and only treats the agent's close as an error when no result was
// (about to be) delivered.
func waitJob(ag *agent.Agent, j *agent.Job) (string, error) {
	select {
	case r := <-j.Result:
		return r.Output, r.Err
	default:
	}
	select {
	case r := <-j.Result:
		return r.Output, r.Err
	case <-ag.Done():
		select {
		case r := <-j.Result:
			return r.Output, r.Err
		default:
			return "", agent.ErrClosed
		}
	}
}

// RunAction executes a high-level action on an agent (session-independent).
func (e *Engine) RunAction(agentID, action string, params map[string]any) (string, error) {
	ag := e.resolve(agentID)
	if ag == nil {
		return "", errors.New("no active agent")
	}
	j := agent.NewJob(agent.JobDirect)
	j.Action = action
	j.Params = params
	return e.wait(ag, j)
}

// ExecDirect runs a shell command on an agent (session-independent).
func (e *Engine) ExecDirect(agentID, cmd string) (string, error) {
	ag := e.resolve(agentID)
	if ag == nil {
		return "", errors.New("no active agent")
	}
	j := agent.NewJob(agent.JobDirect)
	j.Action = "exec_cmd"
	j.Params = map[string]any{"command": cmd}
	return e.wait(ag, j)
}

// Upload sends a local file to an agent.
func (e *Engine) Upload(agentID, localPath, destPath string) (string, error) {
	ag := e.resolve(agentID)
	if ag == nil {
		return "", errors.New("no active agent")
	}
	j := agent.NewJob(agent.JobUpload)
	j.LocalPath = localPath
	j.DestPath = destPath
	return e.wait(ag, j)
}

// Download fetches a remote file into destDir (default cfg download dir).
func (e *Engine) Download(agentID, srcPath, destDir string) (string, error) {
	ag := e.resolve(agentID)
	if ag == nil {
		return "", errors.New("no active agent")
	}
	if destDir == "" {
		d, err := e.cfg.DlDir()
		if err != nil {
			return "", err
		}
		destDir = d
	}
	j := agent.NewJob(agent.JobDownload)
	j.SrcPath = srcPath
	j.DownloadDir = destDir
	return e.wait(ag, j)
}

// ShutdownAgent asks an agent to terminate.
func (e *Engine) ShutdownAgent(agentID string) error {
	ag := e.resolve(agentID)
	if ag == nil {
		return errors.New("no active agent")
	}
	j := agent.NewJob(agent.JobShutdown)
	_, err := e.wait(ag, j)
	return err
}

func parseArgs(raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}
