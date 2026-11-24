package core

import (
	"encoding/json"
	"sync"
	"time"
)

// Event is pushed to UIs over server-sent events.
type Event struct {
	Seq  int64           `json:"seq"`
	Type string          `json:"type"` // message, thread, run, activity, agents, models, schedules, channels, proposal, settings, tools, skills
	Time time.Time       `json:"time"`
	Data json.RawMessage `json:"data"`
}

// Hub fans events out to subscribers. Slow subscribers drop events rather
// than block the daemon; the UI re-fetches state when it reconnects.
type Hub struct {
	mu   sync.Mutex
	seq  int64
	subs map[chan Event]struct{}
}

func NewHub() *Hub { return &Hub{subs: map[chan Event]struct{}{}} }

// Publish sends an event to every subscriber.
func (h *Hub) Publish(typ string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	ev := Event{Seq: h.seq, Type: typ, Time: time.Now().UTC(), Data: b}
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Subscribe returns a channel of events and a function to unsubscribe.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}
