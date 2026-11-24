package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/sseutil"
)

// openAIAdapter speaks the OpenAI Chat Completions protocol, which is also
// implemented by most gateways and local inference servers.
type openAIAdapter struct {
	s        Settings
	endpoint string
	header   func() http.Header
}

func (o *openAIAdapter) Format() string { return FormatOpenAI }

func (o *openAIAdapter) Build(req Request) (*Built, error) {
	msgs := make([]any, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": req.System})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleUser:
			msgs = append(msgs, o.userMessages(req, m)...)
		case llm.RoleAssistant:
			if raw, ok := replayNative(m, FormatOpenAI); ok {
				msgs = append(msgs, o.nativeAssistant(raw))
				continue
			}
			if am := o.assistantMessage(m); am != nil {
				msgs = append(msgs, am)
			}
		}
	}

	body := map[string]any{"model": req.Model, "messages": msgs, "stream": true}
	if o.s.OpenAIStreamUsage {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if req.MaxOutputTokens > 0 {
		body["max_tokens"] = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			fn := map[string]any{"name": t.Name, "description": t.Description}
			if len(t.Schema) > 0 {
				fn["parameters"] = json.RawMessage(t.Schema)
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		body["tools"] = tools
		if req.ForbidToolCalls {
			body["tool_choice"] = "none"
		}
	}
	mergeExtra(body, o.s.ExtraBody)
	raw, err := marshalBody(body)
	if err != nil {
		return nil, fmt.Errorf("encode openai request: %w", err)
	}
	return &Built{Method: http.MethodPost, URL: o.endpoint, Header: o.header(), Body: raw, Format: FormatOpenAI}, nil
}

// contentPart renders one media attachment as a chat-completions content part.
func (o *openAIAdapter) contentPart(req Request, m *llm.Media) any {
	b64, ph := mediaPayload(req, m)
	if ph != "" {
		return map[string]any{"type": "text", "text": ph}
	}
	switch m.Kind {
	case llm.MediaImage:
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + m.MIME + ";base64," + b64}}
	case llm.MediaAudio:
		format := audioFormat(m.MIME)
		if format == "" {
			return map[string]any{"type": "text", "text": fmt.Sprintf("[audio attachment %q omitted: OpenAI-format APIs accept only wav and mp3 audio, got %s]", attachmentLabel(m), m.MIME)}
		}
		return map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": b64, "format": format}}
	case llm.MediaPDF:
		name := m.Name
		if name == "" {
			name = "document.pdf"
		}
		return map[string]any{"type": "file", "file": map[string]any{"filename": name, "file_data": "data:application/pdf;base64," + b64}}
	}
	return map[string]any{"type": "text", "text": fmt.Sprintf("[%s attachment %q omitted: not supported by the OpenAI-format API]", m.Kind, attachmentLabel(m))}
}

func audioFormat(mime string) string {
	switch strings.ToLower(mime) {
	case "audio/wav", "audio/x-wav", "audio/wave", "audio/vnd.wave":
		return "wav"
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	}
	return ""
}

// userMessages renders a canonical user message. Tool results become `tool`
// role messages (which must directly follow the assistant's tool_calls);
// media returned by tools cannot ride inside them, so it is delivered in a
// trailing user message.
func (o *openAIAdapter) userMessages(req Request, m llm.Message) []any {
	var out []any
	var toolMedia, rest []any
	for _, p := range m.Parts {
		switch p.Type {
		case llm.PartToolResult:
			if p.Result == nil {
				continue
			}
			r := p.Result
			content := r.Output
			if r.IsError {
				content = "ERROR: " + content
			}
			out = append(out, map[string]any{"role": "tool", "tool_call_id": r.CallID, "content": content})
			if len(r.Media) > 0 {
				toolMedia = append(toolMedia, map[string]any{"type": "text", "text": fmt.Sprintf("Media returned by tool call %s (%s):", r.CallID, r.Name)})
				for i := range r.Media {
					toolMedia = append(toolMedia, o.contentPart(req, &r.Media[i]))
				}
			}
		case llm.PartText, llm.PartInfo:
			if p.Text != "" {
				rest = append(rest, map[string]any{"type": "text", "text": p.Text})
			}
		case llm.PartMedia:
			rest = append(rest, o.contentPart(req, p.Media))
		}
	}
	trailing := append(toolMedia, rest...)
	if len(trailing) == 0 {
		return out
	}
	allText := true
	for _, t := range trailing {
		if tm, ok := t.(map[string]any); !ok || tm["type"] != "text" {
			allText = false
			break
		}
	}
	if allText {
		var sb strings.Builder
		for i, t := range trailing {
			if i > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(t.(map[string]any)["text"].(string))
		}
		return append(out, map[string]any{"role": "user", "content": sb.String()})
	}
	return append(out, map[string]any{"role": "user", "content": trailing})
}

func (o *openAIAdapter) assistantMessage(m llm.Message) any {
	var text strings.Builder
	var calls []any
	for _, p := range m.Parts {
		switch p.Type {
		case llm.PartText:
			text.WriteString(p.Text)
		case llm.PartToolCall:
			if p.Call == nil {
				continue
			}
			calls = append(calls, map[string]any{
				"id": p.Call.ID, "type": "function",
				"function": map[string]any{"name": p.Call.Name, "arguments": string(argsOrEmpty(p.Call.Args))},
			})
		}
	}
	if text.Len() == 0 && len(calls) == 0 {
		return nil
	}
	msg := map[string]any{"role": "assistant", "content": text.String()}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	return msg
}

// nativeAssistant replays a stored assistant message, dropping reasoning
// fields unless the user opted in (most servers reject them as input).
func (o *openAIAdapter) nativeAssistant(raw json.RawMessage) any {
	if o.s.OpenAIReplayReasoning {
		return raw
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	delete(m, "reasoning_content")
	delete(m, "reasoning")
	return m
}

// ---- streaming ----

type oaChunk struct {
	Choices []struct {
		Delta struct {
			Content          *string `json:"content"`
			ReasoningContent *string `json:"reasoning_content"`
			Reasoning        *string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CachedTokens           int `json:"cached_tokens"`
		CompletionTokenDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type oaCall struct {
	id, name string
	args     strings.Builder
}

func (o *openAIAdapter) ParseStream(r io.Reader, sink func(Delta)) (*Response, error) {
	sr := sseutil.NewReader(r)
	var (
		text, reasoning strings.Builder
		calls           = map[int]*oaCall{}
		finish          string
		usage           *llm.Usage
	)
	for {
		ev, err := sr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		data := strings.TrimSpace(ev.Data)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var ch oaChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return nil, fmt.Errorf("decode openai stream chunk: %w", err)
		}
		if ch.Error != nil {
			code := 500
			if c, ok := ch.Error.Code.(float64); ok {
				code = int(c)
			}
			return nil, errFromStream(code, ch.Error.Type, ch.Error.Message)
		}
		if u := ch.Usage; u != nil && u.TotalTokens > 0 {
			usage = &llm.Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens, CachedTokens: u.CachedTokens}
			if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
				usage.CachedTokens = u.PromptTokensDetails.CachedTokens
			}
			if u.CompletionTokenDetails != nil {
				usage.ThoughtTokens = u.CompletionTokenDetails.ReasoningTokens
				// OpenAI counts reasoning inside completion tokens; report text-only completion.
				if usage.CompletionTokens >= usage.ThoughtTokens {
					usage.CompletionTokens -= usage.ThoughtTokens
				}
			}
		}
		for _, c := range ch.Choices {
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
			d := c.Delta
			if d.Content != nil && *d.Content != "" {
				text.WriteString(*d.Content)
				sink(Delta{Kind: DeltaText, Text: *d.Content})
			}
			for _, rs := range []*string{d.ReasoningContent, d.Reasoning} {
				if rs != nil && *rs != "" {
					reasoning.WriteString(*rs)
					sink(Delta{Kind: DeltaThought, Text: *rs})
				}
			}
			for _, tc := range d.ToolCalls {
				cur := calls[tc.Index]
				if cur == nil {
					cur = &oaCall{}
					calls[tc.Index] = cur
				}
				if tc.ID != "" {
					cur.id = tc.ID
				}
				if tc.Function.Name != "" {
					if cur.name == "" {
						sink(Delta{Kind: DeltaToolCall, Text: tc.Function.Name})
					}
					cur.name += tc.Function.Name
				}
				cur.args.WriteString(tc.Function.Arguments)
			}
		}
	}

	msg := llm.Message{Role: llm.RoleAssistant}
	if reasoning.Len() > 0 {
		msg.Parts = append(msg.Parts, llm.Part{Type: llm.PartThought, Thought: &llm.Thought{Text: reasoning.String()}})
	}
	if text.Len() > 0 {
		msg.Parts = append(msg.Parts, llm.Part{Type: llm.PartText, Text: text.String()})
	}
	idx := make([]int, 0, len(calls))
	for i := range calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	nativeCalls := make([]any, 0, len(idx))
	for _, i := range idx {
		c := calls[i]
		synthetic := false
		if c.id == "" {
			c.id, synthetic = newSynthID(), true
		}
		args, argsErr := normaliseArgs(c.args.String())
		msg.Parts = append(msg.Parts, llm.Part{Type: llm.PartToolCall, Call: &llm.ToolCall{ID: c.id, Name: c.name, Args: args, ArgsError: argsErr, Synthetic: synthetic}})
		nativeCalls = append(nativeCalls, map[string]any{"id": c.id, "type": "function", "function": map[string]any{"name": c.name, "arguments": string(args)}})
	}
	native := map[string]any{"role": "assistant", "content": text.String()}
	if reasoning.Len() > 0 {
		native["reasoning_content"] = reasoning.String()
	}
	if len(nativeCalls) > 0 {
		native["tool_calls"] = nativeCalls
	}
	if raw, err := encodeJSON(native); err == nil && (text.Len() > 0 || len(nativeCalls) > 0) {
		msg.Native = &llm.Native{Format: FormatOpenAI, Raw: raw}
	}
	msg.FinishReason = finishFromTools(mapOpenAIFinish(finish), len(nativeCalls) > 0)
	msg.Usage = usage
	return &Response{Message: msg, Usage: usage}, nil
}

func mapOpenAIFinish(fr string) llm.FinishReason {
	switch fr {
	case "stop", "":
		return llm.FinishStop
	case "tool_calls", "function_call":
		return llm.FinishToolCalls
	case "length":
		return llm.FinishLength
	case "content_filter":
		return llm.FinishFiltered
	}
	return llm.FinishOther
}
