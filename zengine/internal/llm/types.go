// Package llm defines the provider-neutral conversation model used by the
// engine. Every provider adapter converts to and from these types; the session
// log persists them. Nothing in this package knows about any wire format.
package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Role is the author of a message. Tool results travel inside user messages,
// which matches how every supported provider models them.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// PartType discriminates the payload of a Part.
type PartType string

const (
	PartText       PartType = "text"
	PartMedia      PartType = "media"
	PartThought    PartType = "thought"
	PartToolCall   PartType = "tool_call"
	PartToolResult PartType = "tool_result"
	// PartInfo is an engine-authored note ("[info: ...]") appended to a user
	// turn. It is rendered as text but kept distinct so UIs can style it.
	PartInfo PartType = "info"
)

// MediaKind classifies an attachment by the capability it requires.
type MediaKind string

const (
	MediaImage MediaKind = "image"
	MediaAudio MediaKind = "audio"
	MediaVideo MediaKind = "video"
	MediaPDF   MediaKind = "pdf"
)

// Media is an attachment. The payload bytes live in the session blob store,
// addressed by Blob (hex SHA-256); the log only carries this small reference.
type Media struct {
	Kind MediaKind `json:"kind"`
	MIME string    `json:"mime"`
	Blob string    `json:"blob,omitempty"`
	Size int64     `json:"size"`
	// Name and Path describe where the attachment came from so the model can
	// re-open it after the inline payload has been pruned or compacted away.
	Name string `json:"name,omitempty"`
	Path string `json:"path,omitempty"`
}

// ToolCall is a model request to run a tool.
type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
	// ArgsError holds the raw argument text when the model produced something
	// that is not valid JSON (typically output cut off at the token limit).
	// Args is then "{}" so the call can still be stored and replayed; the tool
	// registry reports ArgsError back to the model as the tool result.
	ArgsError string `json:"args_error,omitempty"`
	// Synthetic is true when the provider supplied no call ID and the engine
	// generated one. Adapters must not echo synthetic IDs back to the provider
	// unless the wire format requires an ID.
	Synthetic bool `json:"synthetic,omitempty"`
}

// ToolResult is the outcome of a ToolCall, always textual, optionally with
// media (for example a screenshot or a viewed image).
type ToolResult struct {
	CallID string `json:"call_id"`
	// CallSynthetic mirrors ToolCall.Synthetic for the call this answers.
	CallSynthetic bool    `json:"call_synthetic,omitempty"`
	Name          string  `json:"name"`
	Output        string  `json:"output"`
	IsError       bool    `json:"is_error,omitempty"`
	Media         []Media `json:"media,omitempty"`
}

// Thought is model reasoning that must be replayed to the provider verbatim
// when the provider requires it (signatures).
type Thought struct {
	Text      string `json:"text,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// Part is one element of a message. Exactly one payload field is set,
// according to Type.
type Part struct {
	Type    PartType    `json:"type"`
	Text    string      `json:"text,omitempty"`
	Media   *Media      `json:"media,omitempty"`
	Call    *ToolCall   `json:"call,omitempty"`
	Result  *ToolResult `json:"result,omitempty"`
	Thought *Thought    `json:"thought,omitempty"`
}

// Native preserves a provider's own encoding of an assistant message. When the
// active provider matches Format the adapter replays Raw byte-for-byte, which
// keeps thought signatures intact and the request prefix identical (cache
// friendly). On a provider switch the canonical Parts are used instead.
type Native struct {
	Format string          `json:"format"`
	Raw    json.RawMessage `json:"raw"`
}

// Message is one conversation turn.
type Message struct {
	Role   Role    `json:"role"`
	Parts  []Part  `json:"parts"`
	Native *Native `json:"native,omitempty"`
	// Usage is attached to assistant messages when the provider reported it.
	Usage *Usage `json:"usage,omitempty"`
	// FinishReason is the provider's stop reason, normalised.
	FinishReason FinishReason `json:"finish_reason,omitempty"`
}

// FinishReason is a provider-neutral stop reason.
type FinishReason string

const (
	FinishStop      FinishReason = "stop"
	FinishToolCalls FinishReason = "tool_calls"
	FinishLength    FinishReason = "length"
	FinishFiltered  FinishReason = "filtered"
	FinishOther     FinishReason = "other"
)

// Usage is token accounting for one model response.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	ThoughtTokens    int `json:"thought_tokens,omitempty"`
	CachedTokens     int `json:"cached_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens"`
	// Estimated is true when the provider reported nothing and the engine
	// approximated the counts (bytes/4).
	Estimated bool `json:"estimated,omitempty"`
}

// ToolSpec advertises a tool to the model. Schema is a JSON Schema object.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

// Capabilities declares which input media the configured model accepts.
type Capabilities struct {
	Vision bool `json:"vision"`
	Audio  bool `json:"audio"`
	PDF    bool `json:"pdf"`
	Video  bool `json:"video"`
}

// Supports reports whether the model accepts the given media kind.
func (c Capabilities) Supports(k MediaKind) bool {
	switch k {
	case MediaImage:
		return c.Vision
	case MediaAudio:
		return c.Audio
	case MediaPDF:
		return c.PDF
	case MediaVideo:
		return c.Video
	}
	return false
}

// UnsupportedMessage is the text returned to the model (as tool output) when
// it tries to use a media kind the configured model cannot accept.
func (c Capabilities) UnsupportedMessage(k MediaKind) string {
	return fmt.Sprintf("%s input is not supported by the configured model (set %s=true in .env only if the model really accepts it)",
		k, map[MediaKind]string{MediaImage: "VISION", MediaAudio: "AUDIO", MediaPDF: "PDF", MediaVideo: "VIDEO"}[k])
}

// KindForMIME maps a MIME type to a MediaKind. ok is false for non-media.
func KindForMIME(mime string) (MediaKind, bool) {
	switch {
	case len(mime) >= 6 && mime[:6] == "image/":
		return MediaImage, true
	case len(mime) >= 6 && mime[:6] == "audio/":
		return MediaAudio, true
	case len(mime) >= 6 && mime[:6] == "video/":
		return MediaVideo, true
	case mime == "application/pdf":
		return MediaPDF, true
	}
	return "", false
}

// HumanBytes formats a byte count for people: 512 B, 26.3 KB, 4.2 MB.
func HumanBytes(n int64) string {
	switch {
	case n >= 10<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// Text returns the concatenated text of all text parts.
func (m Message) Text() string {
	var sb strings.Builder
	for _, p := range m.Parts {
		if p.Type == PartText {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// ToolCalls returns the tool calls in a message, in order.
func (m Message) ToolCalls() []ToolCall {
	var calls []ToolCall
	for _, p := range m.Parts {
		if p.Type == PartToolCall && p.Call != nil {
			calls = append(calls, *p.Call)
		}
	}
	return calls
}
