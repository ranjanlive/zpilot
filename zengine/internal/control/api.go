// Package control is the local API through which interfaces drive an engine.
//
// Every engine process hosts a server on 127.0.0.1; the terminal UI, the web
// UI and `zagent --prompt` against a running session are all clients of it.
// The engine does the work and the interfaces only render it, so any of them
// can attach, detach or reattach without disturbing the agent.
package control

import (
	"github.com/ranjanlive/zpilot/zengine/internal/engine"
)

// StateResponse is GET /api/state.
type StateResponse struct {
	Session   string          `json:"session"`
	Title     string          `json:"title"`
	Workspace string          `json:"workspace"`
	Model     string          `json:"model"`
	State     engine.State    `json:"state"`
	Detail    string          `json:"detail"`
	Counters  engine.Counters `json:"counters"`
	// CompactAt is the token count that triggers compaction (0 = off).
	CompactAt int    `json:"compact_at"`
	Tasks     int    `json:"tasks"`
	LastSeq   int64  `json:"last_seq"`
	PID       int    `json:"pid"`
	Role      string `json:"role"`
	Version   string `json:"version"`
}

// PromptRequest is POST /api/prompt.
type PromptRequest struct {
	Text        string   `json:"text"`
	Attachments []string `json:"attachments,omitempty"`
}

// TextRequest carries a single string (POST /api/note).
type TextRequest struct {
	Text string `json:"text"`
}

// CommandRequest is POST /api/command.
type CommandRequest struct {
	Line string `json:"line"`
}

// UploadResponse is the result of POST /api/upload.
type UploadResponse struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// ErrorResponse is the body of every non-2xx answer.
type ErrorResponse struct {
	Error string `json:"error"`
}
