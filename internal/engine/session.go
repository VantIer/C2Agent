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
	pending    *PendingCommand
	stop       bool
	running    bool
	authCh     chan bool
	job        *agent.Job
	runCtx     context.Context
	cancel     context.CancelFunc
}

func (s *Session) addTranscript(role, text string) {
	s.mu.Lock()
	s.transcript = append(s.transcript, TranscriptEntry{Role: role, Text: text})
	s.mu.Unlock()
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
	ID        string          `json:"id"`
	AgentID   string          `json:"agent_id"`
	Title     string          `json:"title"`
	Phase     Phase           `json:"phase"`
	Iteration int             `json:"iteration"`
	Turn      int             `json:"turn"`
	Text      string          `json:"text"`
	Pending   *PendingCommand `json:"pending,omitempty"`
	Running   bool            `json:"running"`
}

// Snapshot returns a consistent copy of the session state.
func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{
		ID: s.ID, AgentID: s.AgentID, Title: s.Title,
		Phase: s.phase, Iteration: s.iter, Turn: s.turn,
		Text: s.text, Pending: s.pending, Running: s.running,
	}
}

func (s *Session) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stop
}

func (s *Session) historyCopy() []llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]llm.Message, len(s.history))
	copy(out, s.history)
	return out
}

// runConversation executes the tool-calling loop for one user message.
func (e *Engine) runConversation(s *Session, message string) {
	defer e.finish(s)

	ag := e.registry.Get(s.AgentID)
	if ag == nil {
		e.hub.publish(s.AgentID, s.ID, Event{"type": "error", "error": "agent offline"})
		return
	}

	s.mu.Lock()
	s.history = append(s.history, llm.UserMessage(message))
	s.mu.Unlock()
	s.addTranscript("user", message)

	system := e.RenderSystemPrompt(ag.OS)
	round := 0

	for round < e.cfg.Policy.RoundLimit {
		if s.isStopped() {
			e.hub.publish(s.AgentID, s.ID, Event{"type": "stopped"})
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

		e.hub.publish(s.AgentID, s.ID, Event{"type": "answering", "iteration": round, "turn": turn})

		msgs := e.buildMessages(s, system)
		res, err := e.llm.Chat(ctx, msgs, e.tools, func(chunk string) {
			s.mu.Lock()
			s.text += chunk
			s.mu.Unlock()
			e.hub.publish(s.AgentID, s.ID, Event{"type": "chunk", "content": chunk})
		})
		if err != nil {
			if s.isStopped() {
				e.hub.publish(s.AgentID, s.ID, Event{"type": "stopped"})
				return
			}
			e.hub.publish(s.AgentID, s.ID, Event{"type": "error", "error": err.Error()})
			s.mu.Lock()
			s.history = append(s.history, llm.UserMessage("[LLM Error] "+err.Error()))
			s.mu.Unlock()
			s.addTranscript("system", "[LLM Error] "+err.Error())
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
		e.hub.publish(s.AgentID, s.ID, Event{"type": "response_done", "iteration": round, "commands": toolCallsJSON(res.ToolCalls)})

		denied := false
		for i, tc := range res.ToolCalls {
			if s.isStopped() {
				e.hub.publish(s.AgentID, s.ID, Event{"type": "stopped"})
				return
			}
			action := tc.Name
			params, perr := parseArgs(tc.Arguments)
			if perr != nil {
				e.appendTool(s, tc.ID, action, "Error: invalid arguments: "+perr.Error())
				continue
			}
			if _, known := command.SpecByName(action); !known {
				e.appendTool(s, tc.ID, action, "Error: unknown action: "+action)
				continue
			}
			if !command.CheckSafety(action, params) {
				msg := "Error: blocked by safety check"
				e.appendTool(s, tc.ID, action, msg)
				e.hub.publish(s.AgentID, s.ID, Event{"type": "execution_done", "action": action, "params": params, "result": msg})
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
				e.hub.publish(s.AgentID, s.ID, Event{"type": "auth_required", "action": action, "params": params})
				ok := e.awaitAuth(s, authCh)
				s.mu.Lock()
				s.pending = nil
				if s.authCh == authCh {
					s.authCh = nil
				}
				s.mu.Unlock()
				if s.isStopped() {
					e.hub.publish(s.AgentID, s.ID, Event{"type": "stopped"})
					return
				}
				if !ok {
					// Denied: the action is NEVER enqueued on the agent. Feed the
					// denial back as the tool result (for the model's context),
					// mark the transcript explicitly, and END the turn so the
					// model cannot immediately work around the denial.
					const deniedMsg = "Error: user denied command execution (not executed)"
					e.appendToolRaw(s, tc.ID, deniedMsg)
					s.addTranscript("system", "Denied: "+action+" (not executed)")
					for _, rest := range res.ToolCalls[i+1:] {
						e.appendToolRaw(s, rest.ID, "Error: skipped because a previous command was denied")
					}
					e.hub.publish(s.AgentID, s.ID, Event{"type": "execution_denied", "action": action, "params": params})
					denied = true
					break
				}
				round = 0
			}

			s.mu.Lock()
			s.phase = PhaseExec
			s.mu.Unlock()
			e.hub.publish(s.AgentID, s.ID, Event{"type": "executing", "action": action, "params": params})

			job := agent.NewJob(agent.JobAction)
			job.Action = action
			job.Params = params
			if err := ag.Enqueue(job); err != nil {
				e.appendTool(s, tc.ID, action, "Error: agent offline")
				continue
			}
			s.mu.Lock()
			s.job = job
			s.mu.Unlock()
			r := <-job.Result
			s.mu.Lock()
			s.job = nil
			s.mu.Unlock()

			if r.Err != nil {
				if r.Err == agent.ErrClosed {
					e.appendTool(s, tc.ID, action, "Error: agent offline")
					continue
				}
				e.hub.publish(s.AgentID, s.ID, Event{"type": "error", "error": r.Err.Error()})
				s.mu.Lock()
				s.history = append(s.history, llm.UserMessage("[Network Error] "+r.Err.Error()))
				s.mu.Unlock()
				s.addTranscript("system", "[Network Error] "+r.Err.Error())
				return
			}
			e.appendTool(s, tc.ID, action, r.Output)
			e.hub.publish(s.AgentID, s.ID, Event{"type": "execution_done", "action": action, "params": params, "result": r.Output})
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
	s.job = nil
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.mu.Unlock()
	if running {
		e.hub.publish(s.AgentID, s.ID, Event{"type": "done"})
	}
}

func (e *Engine) buildMessages(s *Session, system string) []llm.Message {
	hist := s.historyCopy()
	msgs := make([]llm.Message, 0, len(hist)+1)
	msgs = append(msgs, llm.SystemMessage(system))
	msgs = append(msgs, hist...)
	return msgs
}

func (e *Engine) appendTool(s *Session, id, label, content string) {
	if content == "" {
		content = "(no output)"
	}
	s.mu.Lock()
	s.history = append(s.history, llm.ToolMessage(content, id))
	if label != "" {
		s.transcript = append(s.transcript, TranscriptEntry{Role: "result", Text: "[" + label + "]\n" + content})
	} else {
		s.transcript = append(s.transcript, TranscriptEntry{Role: "result", Text: content})
	}
	s.mu.Unlock()
}

// appendToolRaw appends only the tool message to the LLM history (used for
// denials / skips, whose transcript rendering is handled separately).
func (e *Engine) appendToolRaw(s *Session, id, content string) {
	s.mu.Lock()
	s.history = append(s.history, llm.ToolMessage(content, id))
	s.mu.Unlock()
}

func toolCallsJSON(calls []llm.ToolCall) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		params, _ := parseArgs(c.Arguments)
		out = append(out, map[string]any{"id": c.ID, "action": c.Name, "params": params})
	}
	return out
}
