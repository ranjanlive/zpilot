package provider

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
)

// mediaPayload returns the base64 payload for a media part, or a text
// placeholder explaining why the payload cannot be sent. Adapters call this so
// a missing blob or an unsupported kind degrades to text instead of failing
// the whole request.
func mediaPayload(req Request, m *llm.Media) (b64 string, placeholder string) {
	if m == nil {
		return "", "[empty attachment]"
	}
	if !req.Caps.Supports(m.Kind) {
		return "", fmt.Sprintf("[%s attachment %q omitted: the configured model does not accept %s input]", m.Kind, attachmentLabel(m), m.Kind)
	}
	if m.Blob == "" || req.Blobs == nil {
		return "", fmt.Sprintf("[%s attachment %q was removed to save context (%s, %d bytes)%s]", m.Kind, attachmentLabel(m), m.MIME, m.Size, reopenHint(m))
	}
	data, err := req.Blobs.Load(m.Blob)
	if err != nil {
		return "", fmt.Sprintf("[%s attachment %q could not be loaded: %v]", m.Kind, attachmentLabel(m), err)
	}
	if m.Kind == llm.MediaImage {
		if err := llm.ValidateImageData(m.MIME, data); err != nil {
			return "", fmt.Sprintf("[image attachment %q omitted: %v%s]", attachmentLabel(m), err, reopenHint(m))
		}
	}
	return base64.StdEncoding.EncodeToString(data), ""
}

func attachmentLabel(m *llm.Media) string {
	if m.Name != "" {
		return m.Name
	}
	if m.Path != "" {
		return m.Path
	}
	return m.Blob
}

func reopenHint(m *llm.Media) string {
	if m.Path != "" {
		return "; re-open it with view_file at " + m.Path
	}
	return ""
}

// replayNative returns the provider-native encoding of an assistant message if
// it was produced by the same format.
func replayNative(m llm.Message, format string) (json.RawMessage, bool) {
	if m.Native != nil && m.Native.Format == format && len(m.Native.Raw) > 0 {
		return m.Native.Raw, true
	}
	return nil, false
}

// textOf flattens text-like parts (text and info) of a message.
func textOf(parts []llm.Part) string {
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == llm.PartText || p.Type == llm.PartInfo {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// normaliseArgs makes tool-call arguments storable. Valid JSON passes
// through; anything else (usually a cut-off response) becomes "{}" with the raw
// text preserved so the model can be told exactly what went wrong.
func normaliseArgs(raw string) (args json.RawMessage, argsErr string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return json.RawMessage(`{}`), ""
	}
	if json.Valid([]byte(raw)) {
		return json.RawMessage(raw), ""
	}
	return json.RawMessage(`{}`), raw
}

var (
	synthCounter atomic.Uint64
	synthPrefix  = randomHex(3)
)

// newSynthID returns a tool-call ID that is unique across the process. It is
// only used when a provider sends no ID; the value is stored in the session
// log, so replays stay deterministic.
func newSynthID() string { return fmt.Sprintf("call_%s%d", synthPrefix, synthCounter.Add(1)) }

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// finishFromTools upgrades a plain stop to tool_calls when the model asked for tools.
func finishFromTools(fr llm.FinishReason, hasCalls bool) llm.FinishReason {
	if hasCalls && (fr == llm.FinishStop || fr == "") {
		return llm.FinishToolCalls
	}
	if fr == "" {
		return llm.FinishStop
	}
	return fr
}

func errFromStream(code int, status, msg string) *Error {
	e := classifyHTTP(code, nil, []byte(msg)) // nil Header is safe: Get on a nil map returns ""
	if e.Message == "" {
		e.Message = status
	}
	return e
}
