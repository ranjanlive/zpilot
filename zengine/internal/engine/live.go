package engine

import (
	"sync"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
)

// State is the engine's coarse lifecycle state, shown by every interface.
type State string

const (
	StateIdle       State = "idle"
	StateWorking    State = "working"
	StatePaused     State = "paused"     // network trouble; waiting to retry
	StateWaitingBG  State = "waiting_bg" // model finished, background tasks still running
	StateCompacting State = "compacting"
	StateClosed     State = "closed"
)

// LiveKind labels transient events that are not stored in the session log
// (token deltas); durable facts are in the log and reach UIs through it.
type LiveKind string

const (
	LiveText     LiveKind = "text"      // streamed answer text
	LiveThought  LiveKind = "thought"   // streamed reasoning text
	LiveToolCall LiveKind = "tool_call" // the model started emitting a tool call
	LiveToolRun  LiveKind = "tool_run"  // a tool began executing
	LiveToolDone LiveKind = "tool_done"
	LiveState    LiveKind = "state"
	LiveUsage    LiveKind = "usage"
	LiveRetry    LiveKind = "retry"
)

// Live is one transient event.
type Live struct {
	Kind   LiveKind  `json:"kind"`
	Text   string    `json:"text,omitempty"`
	Name   string    `json:"name,omitempty"` // tool name
	State  State     `json:"state,omitempty"`
	Detail string    `json:"detail,omitempty"`
	Tokens Counters  `json:"tokens"`
	At     time.Time `json:"at"`
}

// Counters is the token accounting shown live in UIs.
type Counters struct {
	// Context is the size of the prompt of the latest request (what the model
	// is holding right now).
	Context int `json:"context"`
	// Prompt, Completion, Thought and Cached accumulate over the session.
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Thought    int `json:"thought"`
	Cached     int `json:"cached"`
	Requests   int `json:"requests"`
	// Estimated is true when any contribution had to be estimated (bytes/4).
	Estimated bool `json:"estimated"`
}

func (c *Counters) add(u *llm.Usage) {
	c.Requests++
	c.Context = u.PromptTokens
	c.Prompt += u.PromptTokens
	c.Completion += u.CompletionTokens
	c.Thought += u.ThoughtTokens
	c.Cached += u.CachedTokens
	if u.Estimated {
		c.Estimated = true
	}
}

type bus struct {
	mu   sync.Mutex
	subs map[int]chan Live
	next int
}

// subscribe returns a channel of live events and a cancel function. A slow
// consumer drops the oldest deltas rather than blocking the engine; durable
// state is always recoverable from the log.
func (b *bus) subscribe(buffer int) (<-chan Live, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs == nil {
		b.subs = map[int]chan Live{}
	}
	id := b.next
	b.next++
	ch := make(chan Live, buffer)
	b.subs[id] = ch
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
	}
}

func (b *bus) publish(ev Live) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
			select { // full: drop the oldest, keep the newest
			case <-ch:
			default:
			}
			select {
			case ch <- ev:
			default:
			}
		}
	}
}
