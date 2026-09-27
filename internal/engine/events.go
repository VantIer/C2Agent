package engine

import "sync"

// Event is a JSON-serializable conversation event.
type Event map[string]any

type subscriber struct {
	ch        chan Event
	agentID   string
	sessionID string
}

type eventHub struct {
	mu   sync.RWMutex
	subs map[*subscriber]struct{}
}

func newEventHub() *eventHub {
	return &eventHub{subs: make(map[*subscriber]struct{})}
}

func (h *eventHub) subscribe(agentID, sessionID string) *subscriber {
	s := &subscriber{ch: make(chan Event, 256), agentID: agentID, sessionID: sessionID}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *eventHub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
	// Do not close s.ch: runConversation may still publish; dropped sends are
	// tolerated because ch is buffered and publish uses non-blocking sends.
}

func (h *eventHub) publish(agentID, sessionID string, ev Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs {
		if s.agentID != "" && s.agentID != agentID {
			continue
		}
		if s.sessionID != "" && s.sessionID != sessionID {
			continue
		}
		e := make(Event, len(ev)+2)
		for k, v := range ev {
			e[k] = v
		}
		if agentID != "" {
			e["agent"] = agentID
		}
		if sessionID != "" {
			e["session"] = sessionID
		}
		select {
		case s.ch <- e:
		default:
		}
	}
}
