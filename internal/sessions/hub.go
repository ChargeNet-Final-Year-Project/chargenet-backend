package sessions

import (
	"sync"

	"github.com/chargenet/chargenet-backend/internal/store"
)

// subscriberBuffer bounds SSE lag. Full buffers drop stale telemetry; the next
// tick supersedes it and the handler recovers terminal frames separately.
const subscriberBuffer = 32

// Frame is one SSE frame: a complete ChargingSession snapshot, plus whether it
// is the session's last frame.
type Frame struct {
	Session  store.Session
	Terminal bool
}

// Hub fans live session snapshots out to the SSE subscribers of one session.
//
// Only the hub closes subscriber channels, so a handler can never panic on a
// double close: a handler that is done simply unregisters.
type Hub struct {
	mu     sync.Mutex
	subs   map[string]map[*Subscription]struct{}
	closed bool
}

// NewHub builds an empty hub.
func NewHub() *Hub {
	return &Hub{subs: map[string]map[*Subscription]struct{}{}}
}

// Subscription is one client's view of a session's stream.
type Subscription struct {
	C chan Frame

	hub       *Hub
	sessionID string
	once      sync.Once
}

// Subscribe registers a listener for sessionID. The caller must Close it.
func (h *Hub) Subscribe(sessionID string) *Subscription {
	sub := &Subscription{C: make(chan Frame, subscriberBuffer), hub: h, sessionID: sessionID}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(sub.C)
		return sub
	}
	if h.subs[sessionID] == nil {
		h.subs[sessionID] = map[*Subscription]struct{}{}
	}
	h.subs[sessionID][sub] = struct{}{}
	return sub
}

// Close unregisters the subscription. It is safe to call more than once and
// never closes the channel (the hub owns that).
func (s *Subscription) Close() {
	s.once.Do(func() { s.hub.remove(s) })
}

func (h *Hub) remove(sub *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	subs := h.subs[sub.sessionID]
	delete(subs, sub)
	if len(subs) == 0 {
		delete(h.subs, sub.sessionID)
	}
}

// Publish sends a live frame to every subscriber of sessionID. A subscriber
// whose buffer is full loses the frame instead of stalling the caller.
func (h *Hub) Publish(sessionID string, frame Frame) {
	h.mu.Lock()
	subs := h.subs[sessionID]
	targets := make([]*Subscription, 0, len(subs))
	for sub := range subs {
		targets = append(targets, sub)
	}
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return
	}
	for _, sub := range targets {
		select {
		case sub.C <- frame:
		default: // client is behind; telemetry is worthless late
		}
	}
}

// Complete publishes the terminal frame and closes every subscription of the
// session. A subscriber that has already fallen behind gets its channel closed
// without the frame; the SSE handler then re-reads the settled row, so the
// client still sees the `completed` event.
func (h *Hub) Complete(sessionID string, frame Frame) {
	frame.Terminal = true

	h.mu.Lock()
	subs := h.subs[sessionID]
	delete(h.subs, sessionID)
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return
	}
	for sub := range subs {
		select {
		case sub.C <- frame:
		default:
		}
		close(sub.C)
	}
}

// Close ends every stream (process shutdown): the handlers finish their
// response and the clients fall back to polling.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for id, subs := range h.subs {
		for sub := range subs {
			close(sub.C)
		}
		delete(h.subs, id)
	}
}
