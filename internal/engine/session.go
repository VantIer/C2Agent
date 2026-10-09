package engine

import (
	"context"
	"sync"
	"time"

	"c2agent/internal/agent"
	"c2agent/internal/command"
	"c2agent/internal/llm"
)

// Phase is the conversation phase of a session.
type Phase string

const (
	PhaseIdle     Phase = "idle"
	PhaseLLM      Phase = "llm"
	PhaseExec     Phase = "exec"
	PhaseAuthWait Phase = "auth_wait"
)

// PendingCommand is an action awaiting user authorization.
type PendingCommand struct {
	ToolCallID string         `json:"tool_call_id"`
	Action     string         `json:"action"`
	Params     map[string]any `json:"params"`
}

// TranscriptEntry is one displayable line of a session's conversation.
type TranscriptEntry struct {
	Role string `json:"role"` // user | assistant | result | system
	Text string `json:"text"`
}

// Session is one independent conversation bound to an agent.
type Session struct {
	ID        string
	AgentID   string
	Title     string
	CreatedAt time.Time

	mu         sync.Mutex
	history    []llm.Message
	transcript []TranscriptEntry
	phase      Phase
	iter       int
	turn       int
	text       string
	seq        uint64
	pending    *PendingCommand
	stop       bool
	running    bool
	authCh     chan bool
	job        *agent.Job
	runCtx     context.Context
	cancel     context.CancelFunc
	done       chan struct{}
}

func (s *Session) addTranscript(role, text string) uint64 {
	s.mu.Lock()
	s.transcript = append(s.transcript, TranscriptEntry{Role: role, Text: text})
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	return seq
}

func (s *Session) transcriptCopy() []TranscriptEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TranscriptEntry, len(s.transcript))
	copy(out, s.transcript)
	return out
}

// Snapshot is a copy of session state for UI replay.
type Snapshot struct {
	ID         string            `json:"id"`
	AgentID    string            `json:"agent_id"`
	Title      string            `json:"title"`
	Phase      Phase             `json:"phase"`
	Iteration  int               `json:"iteration"`
	Turn       int               `json:"turn"`
	Text       string            `json:"text"`
	Seq        uint64            `json:"seq"`
	Pending    *PendingCommand   `json:"pending,omitempty"`
	Running    bool              `json:"running"`
	Transcript []TranscriptEntry `json:"transcript"`
}

// Snapshot returns a consistent copy of the session state.
func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	tr := make([]TranscriptEntry, len(s.transcript))
	copy(tr, s.transcript)
	return Snapshot{
		ID: s.ID, AgentID: s.AgentID, Title: s.Title,
		Phase: s.phase, Iteration: s.iter, Turn: s.turn,
		Text: s.text, Pending: s.pending, Running: s.running,
		Transcript: tr, Seq: s.seq,
	}
}

// publishNow assigns the next event sequence and broadcasts ev.
func (e *Engine) publishNow(s *Session, ev Event) {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	ev["seq"] = seq
	e.hub.publish(s.AgentID, s.ID, ev)
}

// publish broadcasts ev carrying a sequence already assigned under the session
// lock (used when the state mutation and the seq increment are atomic).
func (e *Engine) publish(s *Session, seq uint64, ev Event) {
	ev["seq"] = seq
	e.hub.publish(s.AgentID, s.ID, ev)
}

func (s *Session) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stop
}

// Phase returns the current conversation phase (cheap status accessor).
func (s *Session) Phase() Phase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase
}

// Running reports whether a conversation is currently in progress (cheap
// status accessor that does not copy the transcript like Snapshot does).
func (s *Session) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *Session) historyCopy() []llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]llm.Message, len(s.history))
	copy(out, s.history)
	return out
}

// persistPartial moves any in-progress assistant text into the transcript, so
// a refresh replays the same partial content the user already saw (used when a
// turn ends abnormally: stop or LLM error).
func (e *Engine) persistPartial(s *Session) {
	s.mu.Lock()
	partial := s.text
	s.text = ""
	s.mu.Unlock()
	if partial != "" {
		s.addTranscript("assistant", partial)
	}
}

// markStopped records a stop in the transcript (partial text + a system marker)
// and then broadcasts the stopped event, keeping the live view and the
// replayed view identical.
func (e *Engine) markStopped(s *Session) {
	e.persistPartial(s)
	s.addTranscript("system", "[Stopped by user]")
	e.publishNow(s, Event{"type": "stopped"})
}

// runConversation executes the tool-calling loop for one user message. The
// user message itself is persisted by BeginChat before this goroutine starts.
func (e *Engine) runConversation(s *Session) {
	defer e.finish(s)

	ag := e.registry.Get(s.AgentID)
	if ag == nil {
		const msg = "Error: agent offline"
		seq := s.addTranscript("result", msg)
		e.publish(s, seq, Event{"type": "agent_error", "error": "agent offline"})
		return
	}

	system := e.RenderSystemPrompt(ag.OS)
	round := 0

	for round < e.cfg.Policy.RoundLimit {
		if s.isStopped() {
			e.markStopped(s)
			return
		}
		round++
		s.mu.Lock()
		s.iter = round
		s.turn++
		s.text = ""
		s.pending = nil
		s.phase = PhaseLLM
		turn := s.turn
		ctx := s.runCtx
		s.mu.Unlock()

		e.publishNow(s, Event{"type": "answering", "iteration": round, "turn": turn})

		msgs := e.buildMessages(s, system)
		res, err := e.llm.Chat(ctx, msgs, e.tools, func(chunk string) {
			s.mu.Lock()
			s.text += chunk
			s.seq++
			seq := s.seq
			s.mu.Unlock()
			e.publish(s, seq, Event{"type": "chunk", "content": chunk})
		})
		if err != nil {
			if s.isStopped() {
				e.markStopped(s)
				return
			}
			e.persistPartial(s)
			seq := s.addTranscript("system", "[LLM Error] "+err.Error())
			e.publish(s, seq, Event{"type": "llm_error", "error": err.Error()})
			return
		}

		if res == nil {
			e.persistPartial(s)
			seq := s.addTranscript("system", "[LLM Error] empty response")
			e.publish(s, seq, Event{"type": "llm_error", "error": "empty response"})
			return
		}

		s.mu.Lock()
		s.history = append(s.history, res.Raw)
		s.text = ""
		s.mu.Unlock()
		if res.Content != "" {
			s.addTranscript("assistant", res.Content)
		}

		if len(res.ToolCalls) == 0 {
			break
		}
		e.publishNow(s, Event{"type": "response_done", "iteration": round, "commands": toolCallsJSON(res.ToolCalls)})

		denied := false
		for i, tc := range res.ToolCalls {
			if s.isStopped() {
				for _, rest := range res.ToolCalls[i:] {
					e.appendToolRaw(s, rest.ID, "Error: aborted before execution")
				}
				e.markStopped(s)
				return
			}
			action := tc.Name
			params, perr := parseArgs(tc.Arguments)
			if perr != nil {
				msg := "Error: invalid arguments: " + perr.Error()
				seq := e.appendTool(s, tc.ID, action, params, msg)
				e.publish(s, seq, Event{"type": "execution_done", "action": action, "params": params, "result": msg})
				continue
			}
			if _, known := command.SpecByName(action); !known {
				msg := "Error: unknown action: " + action
				seq := e.appendTool(s, tc.ID, action, params, msg)
				e.publish(s, seq, Event{"type": "execution_done", "action": action, "params": params, "result": msg})
				continue
			}
			if !command.CheckSafety(action, params) {
				msg := "Error: blocked by safety check"
				seq := e.appendTool(s, tc.ID, action, params, msg)
				e.publish(s, seq, Event{"type": "execution_done", "action": action, "params": params, "result": msg})
				continue
			}

			if e.RequiresAuth(action) {
				// Register the decision channel BEFORE announcing the request so
				// a fast client cannot submit a decision that is then lost.
				authCh := make(chan bool, 1)
				s.mu.Lock()
				s.pending = &PendingCommand{ToolCallID: tc.ID, Action: action, Params: params}
				s.phase = PhaseAuthWait
				s.authCh = authCh
				s.mu.Unlock()
				e.publishNow(s, Event{"type": "auth_required", "action": action, "params": params})
				ok := e.awaitAuth(s, authCh)
				s.mu.Lock()
				s.pending = nil
				if s.authCh == authCh {
					s.authCh = nil
				}
				s.mu.Unlock()
				if s.isStopped() {
					for _, rest := range res.ToolCalls[i:] {
						e.appendToolRaw(s, rest.ID, "Error: aborted before execution")
					}
					e.markStopped(s)
					return
				}
				if !ok {
					// Denied: the action is NEVER enqueued on the agent. Feed the
					// denial back as the tool result (for the model's context),
					// mark the transcript explicitly, and END the turn so the
					// model cannot immediately work around the denial.
					const deniedMsg = "Error: user denied command execution (not executed)"
					e.appendToolRaw(s, tc.ID, deniedMsg)
					seq := s.addTranscript("system", "Denied: "+action+" (not executed)")
					for _, rest := range res.ToolCalls[i+1:] {
						e.appendToolRaw(s, rest.ID, "Error: skipped because a previous command was denied")
					}
					e.publish(s, seq, Event{"type": "execution_denied", "action": action, "params": params})
					denied = true
					break
				}
				round = 0
			}

			s.mu.Lock()
			s.phase = PhaseExec
			s.mu.Unlock()
			e.publishNow(s, Event{"type": "executing", "action": action, "params": params})

			job := agent.NewJob(agent.JobAction)
			job.Action = action
			job.Params = params
			if err := ag.Enqueue(job); err != nil {
				msg := "Error: agent offline"
				seq := e.appendTool(s, tc.ID, action, params, msg)
				e.publish(s, seq, Event{"type": "execution_done", "action": action, "params": params, "result": msg})
				continue
			}
			s.mu.Lock()
			s.job = job
			s.mu.Unlock()
			var r agent.JobResult
			r.Output, r.Err = waitJob(ag, job)
			s.mu.Lock()
			s.job = nil
			s.mu.Unlock()

			if r.Err != nil {
				if r.Err == agent.ErrCancelled {
					// The user stopped the session (or the job was cancelled):
					// report it as a stop, not as an execution failure.
					e.publishNow(s, Event{"type": "stopped"})
					return
				}
				if r.Err == agent.ErrClosed {
					msg := "Error: agent offline"
					seq := e.appendTool(s, tc.ID, action, params, msg)
					e.publish(s, seq, Event{"type": "execution_done", "action": action, "params": params, "result": msg})
					continue
				}
				// Persist the failure as this tool call's result so history
				// replay matches the live view, and answer the remaining tool
				// calls so the assistant/tool message sequence stays valid.
				seq := e.appendTool(s, tc.ID, action, params, "Error: "+r.Err.Error())
				for _, rest := range res.ToolCalls[i+1:] {
					e.appendToolRaw(s, rest.ID, "Error: skipped because the agent connection failed")
				}
				e.publish(s, seq, Event{"type": "execution_error", "action": action, "params": params, "error": r.Err.Error()})
				return
			}
			seq := e.appendTool(s, tc.ID, action, params, r.Output)
			e.publish(s, seq, Event{"type": "execution_done", "action": action, "params": params, "result": r.Output})
		}
		if denied {
			break
		}
	}
}

// finish marks a session idle and emits exactly one terminal "done" event,
// regardless of how the conversation ended (normal / stopped / error).
func (e *Engine) finish(s *Session) {
	s.mu.Lock()
	running := s.running
	s.running = false
	s.phase = PhaseIdle
	s.stop = false
	s.pending = nil
	s.job = nil
	runDone := s.done
	s.done = nil
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.mu.Unlock()
	if running {
		e.publishNow(s, Event{"type": "done"})
	}
	if runDone != nil {
		close(runDone)
	}
}

func (e *Engine) buildMessages(s *Session, system string) []llm.Message {
	hist := s.historyCopy()
	msgs := make([]llm.Message, 0, len(hist)+1)
	msgs = append(msgs, llm.SystemMessage(system))
	msgs = append(msgs, hist...)
	return msgs
}

// actionDetail renders the parameter detail shown next to an action's name,
// matching the live web/CLI rendering (command first, then path). Persisting
// it in the transcript means a page refresh replays the same text instead of
// dropping the detail.
func actionDetail(params map[string]any) string {
	if v, ok := params["command"].(string); ok && v != "" {
		return " [" + v + "]"
	}
	if v, ok := params["path"].(string); ok && v != "" {
		return " [" + v + "]"
	}
	return ""
}

func (e *Engine) appendTool(s *Session, id, label string, params map[string]any, content string) uint64 {
	if content == "" {
		content = "(no output)"
	}
	text := "[" + label + "]" + actionDetail(params) + "\n" + content
	s.mu.Lock()
	s.history = append(s.history, llm.ToolMessage(content, id))
	s.transcript = append(s.transcript, TranscriptEntry{Role: "result", Text: text})
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	return seq
}

// appendToolRaw appends only the tool message to the LLM history (used for
// denials / skips, whose transcript rendering is handled separately).
func (e *Engine) appendToolRaw(s *Session, id, content string) uint64 {
	s.mu.Lock()
	s.history = append(s.history, llm.ToolMessage(content, id))
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	return seq
}

func toolCallsJSON(calls []llm.ToolCall) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		params, _ := parseArgs(c.Arguments)
		out = append(out, map[string]any{"id": c.ID, "action": c.Name, "params": params})
	}
	return out
}
