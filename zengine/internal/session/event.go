// Package session persists a conversation as an append-only event log plus a
// content-addressed blob store for attachments. The log is the single source
// of truth: the model context, the UIs and resume all derive from it, which is
// what keeps requests byte-identical from turn to turn.
package session

import (
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
)

// Kind labels an event.
type Kind string

const (
	// KindSystem snapshots the system prompt. It is written when a session
	// starts and again after each compaction; mid-session edits to the prompt
	// files never reach the model, which keeps the cached prefix stable.
	KindSystem Kind = "system"
	// KindMessage is one conversation message (user, assistant, tool results).
	KindMessage Kind = "message"
	// KindNote is a queued "[info: ...]" note waiting to ride on the next request.
	KindNote Kind = "note"
	// KindCompaction replaces everything before it with a summary.
	KindCompaction Kind = "compaction"
	// KindPrune records attachments whose inline payload was removed.
	KindPrune Kind = "prune"
	// KindUsage records the token accounting of one model request (UI only).
	KindUsage Kind = "usage"
	// KindStatus records lifecycle changes for UIs: working, paused, retrying, done, error.
	KindStatus Kind = "status"
	// KindTurn marks the start and end of a user prompt (UI and analytics).
	KindTurn Kind = "turn"
)

// Event is one line of events.jsonl.
type Event struct {
	Seq  int64     `json:"seq"`
	Time time.Time `json:"time"`
	Kind Kind      `json:"kind"`

	// TurnNo is the user-turn counter in force when the event was written.
	TurnNo int `json:"turn_no,omitempty"`

	Message *llm.Message `json:"message,omitempty"`
	// Consumed lists the note events folded into this message.
	Consumed []int64 `json:"consumed,omitempty"`

	// Text carries the system prompt (KindSystem), the note (KindNote) or a
	// status line (KindStatus).
	Text string `json:"text,omitempty"`

	Compaction *Compaction `json:"compaction,omitempty"`
	// Blobs lists removed attachment IDs (KindPrune).
	Blobs []string `json:"blobs,omitempty"`

	Usage       *llm.Usage `json:"usage,omitempty"`
	RequestSize int        `json:"request_size,omitempty"`

	// State and Detail describe KindStatus / KindTurn events.
	State  string `json:"state,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Compaction is the payload of a KindCompaction event.
type Compaction struct {
	Summary string `json:"summary"`
	// KeptPrompts are recent user prompts carried over verbatim as text.
	KeptPrompts   []string `json:"kept_prompts,omitempty"`
	Reason        string   `json:"reason"`
	TokensBefore  int      `json:"tokens_before,omitempty"`
	BytesBefore   int      `json:"bytes_before,omitempty"`
	RemovedBlobs  int      `json:"removed_blobs,omitempty"`
	RemovedPaths  []string `json:"removed_paths,omitempty"`
	MessagesAfter int      `json:"messages_after,omitempty"`
}

// Meta is meta.json: a small index record so session lists need not parse logs.
type Meta struct {
	ID        string    `json:"id"`
	Title     string    `json:"title,omitempty"`
	Workspace string    `json:"workspace"`
	Provider  string    `json:"provider,omitempty"`
	Model     string    `json:"model,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Version   int       `json:"version"`
}
