package engine

import (
	"sync"
	"time"
)

// publishTimeout bounds how long a publish waits for a full subscriber buffer
// before giving up on that subscriber, so one stalled consumer cannot block a
// session's conversation goroutine indefinitely.
const publishTimeout = 2 * time.Second

// Event is a JSON-serializable conversation event.
type Event map[string]any

type subscriber struct {
	ch        chan Event
	done      chan struct{}
	closeOnce sync.Once
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
	s := &subscriber{ch: make(chan Event, 256), done: make(chan struct{}), agentID: agentID, sessionID: sessionID}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *eventHub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
	// Signal publish to stop waiting on this subscriber; s.ch is not closed
	// because runConversation may still be publishing concurrently.
	s.closeOnce.Do(func() { close(s.done) })
}

func (h *eventHub) publish(agentID, sessionID string, ev Event) {
	// Snapshot matching subscribers under the lock, then deliver outside it so
	// a slow subscriber cannot block subscribe/unsubscribe or other publishes.
	h.mu.RLock()
	targets := make([]*subscriber, 0, len(h.subs))
	for s := range h.subs {
		if s.agentID != "" && s.agentID != agentID {
			continue
		}
		if s.sessionID != "" && s.sessionID != sessionID {
			continue
		}
		targets = append(targets, s)
	}
	h.mu.RUnlock()

	for _, s := range targets {
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
		// Fast path: buffer has room.
		select {
		case s.ch <- e:
			continue
		case <-s.done:
			continue
		default:
		}
		// Buffer full: wait briefly for the consumer to drain, then drop it.
		timer := time.NewTimer(publishTimeout)
		select {
		case s.ch <- e:
			timer.Stop()
		case <-s.done:
			timer.Stop()
		case <-timer.C:
			h.unsubscribe(s)
		}
	}
}
