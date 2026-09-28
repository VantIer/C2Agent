package agent

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Registry tracks connected agents from both controlled-end families.
type Registry struct {
	mu       sync.RWMutex
	agents   map[string]*Agent
	activeID string
	botSeq   int
	onRemove []func(*Agent)
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{agents: make(map[string]*Agent)}
}

// AddRemoveListener registers fn to be called when an agent is unregistered.
func (r *Registry) AddRemoveListener(fn func(*Agent)) {
	r.mu.Lock()
	r.onRemove = append(r.onRemove, fn)
	r.mu.Unlock()
}

// Register adds an agent, replacing (and closing) any prior connection with
// the same id.
func (r *Registry) Register(a *Agent) {
	r.mu.Lock()
	old := r.agents[a.ID]
	r.agents[a.ID] = a
	if r.activeID == "" {
		r.activeID = a.ID
	}
	r.mu.Unlock()

	if old != nil && old != a {
		old.Close()
	}
}

// UnregisterAgent removes a specific agent instance. It is a no-op if that
// instance is no longer the registered one (e.g. a reconnect already replaced
// it), so a closing connection can never evict its own successor.
func (r *Registry) UnregisterAgent(a *Agent) {
	if a == nil {
		return
	}
	r.mu.Lock()
	if r.agents[a.ID] != a {
		r.mu.Unlock()
		return
	}
	delete(r.agents, a.ID)
	if r.activeID == a.ID {
		r.activeID = firstByOrder(r.agents)
	}
	listeners := append([]func(*Agent){}, r.onRemove...)
	r.mu.Unlock()

	a.Close()
	for _, fn := range listeners {
		fn(a)
	}
}

// Get returns the agent with the given id, or nil.
func (r *Registry) Get(id string) *Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.agents[id]
}

// List returns all connected agents in a stable order (connection time, then
// id). Go map iteration is randomized, so callers must not rely on raw order.
func (r *Registry) List() []*Agent {
	r.mu.RLock()
	out := make([]*Agent, 0, len(r.agents))
	for _, a := range r.agents {
		out = append(out, a)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ConnectedAt.Equal(out[j].ConnectedAt) {
			return out[i].ConnectedAt.Before(out[j].ConnectedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// firstByOrder returns the agent that comes first under the List ordering.
func firstByOrder(m map[string]*Agent) string {
	first := ""
	var firstAt time.Time
	for id, a := range m {
		if first == "" || a.ConnectedAt.Before(firstAt) ||
			(a.ConnectedAt.Equal(firstAt) && id < first) {
			first = id
			firstAt = a.ConnectedAt
		}
	}
	return first
}

// Active returns the currently active agent, or nil.
func (r *Registry) Active() *Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.activeID == "" {
		return nil
	}
	return r.agents[r.activeID]
}

// ActiveID returns the active agent id (possibly empty).
func (r *Registry) ActiveID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.activeID
}

// SetActive switches the active agent; false if the id is unknown.
func (r *Registry) SetActive(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.agents[id]; !ok {
		return false
	}
	r.activeID = id
	return true
}

// TouchHB refreshes an agent's heartbeat record.
func (r *Registry) TouchHB(id string) {
	if a := r.Get(id); a != nil {
		a.TouchHB()
	}
}

// NextBotID allocates a sequential shell-bot id (e.g. BOT-001).
func (r *Registry) NextBotID(prefix string) string {
	if prefix == "" {
		prefix = "BOT-"
	}
	r.mu.Lock()
	r.botSeq++
	n := r.botSeq
	r.mu.Unlock()
	return fmt.Sprintf("%s%03d", prefix, n)
}
