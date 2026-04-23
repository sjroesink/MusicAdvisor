// Package sse is a minimal Server-Sent Events hub routing per-user
// messages to any number of live connections for that user.
//
// Design: each Subscribe() call returns a new buffered channel and a
// cancel func. Publish(userID, event) writes to every live channel for
// that user with a non-blocking send — slow clients silently drop events,
// which is the right failure mode for an "invalidate and refetch" model
// where a missed event just means the client waits for the next one.
//
// The hub carries no persistent state: reconnecting clients don't
// replay history. That matches the /api/feed/stream contract, where the
// client is expected to re-GET /api/feed on every event anyway.
package sse

import (
	"sync"
	"sync/atomic"
)

// Event is what Publish ships to subscribers. Kind is an application-level
// classifier ("phase_started", "phase_finished", "card_arrived"); Data is
// a string the handler writes verbatim after "data: ".
type Event struct {
	Kind string
	Data string
}

// Hub fans messages out to any number of subscribers per user. Zero value
// is not usable; call NewHub.
type Hub struct {
	mu      sync.RWMutex
	byUser  map[string]map[int64]chan Event
	nextSub atomic.Int64
	buffer  int
}

// NewHub constructs a Hub with the given per-subscriber channel buffer.
// A small buffer (e.g. 16) absorbs bursts without blocking Publish while
// still keeping memory bounded if a client wanders off without closing.
func NewHub(buffer int) *Hub {
	if buffer <= 0 {
		buffer = 16
	}
	return &Hub{
		byUser: map[string]map[int64]chan Event{},
		buffer: buffer,
	}
}

// Subscribe registers a new channel for userID and returns it along with a
// cancel function the caller MUST invoke (normally via defer) to unregister
// and close the channel when the stream ends.
func (h *Hub) Subscribe(userID string) (<-chan Event, func()) {
	id := h.nextSub.Add(1)
	ch := make(chan Event, h.buffer)

	h.mu.Lock()
	if h.byUser[userID] == nil {
		h.byUser[userID] = map[int64]chan Event{}
	}
	h.byUser[userID][id] = ch
	h.mu.Unlock()

	cancel := func() {
		h.mu.Lock()
		if conns, ok := h.byUser[userID]; ok {
			delete(conns, id)
			if len(conns) == 0 {
				delete(h.byUser, userID)
			}
		}
		h.mu.Unlock()
		close(ch)
	}
	return ch, cancel
}

// Publish is non-blocking: a full subscriber buffer drops the event rather
// than stalling the publisher. Sync services call this from the trigger
// goroutine where blocking would be actively harmful.
func (h *Hub) Publish(userID string, ev Event) {
	h.mu.RLock()
	conns := h.byUser[userID]
	// Copy receivers before releasing the lock so a slow handler doesn't
	// hold the map lock during the send.
	targets := make([]chan Event, 0, len(conns))
	for _, ch := range conns {
		targets = append(targets, ch)
	}
	h.mu.RUnlock()

	for _, ch := range targets {
		select {
		case ch <- ev:
		default:
			// buffer full: drop. Client will catch up on next event.
		}
	}
}

// SubscriberCount returns how many live subscribers a user has. Mostly
// useful for tests; the handler doesn't need it.
func (h *Hub) SubscriberCount(userID string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byUser[userID])
}
