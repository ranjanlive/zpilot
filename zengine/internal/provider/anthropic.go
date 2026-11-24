package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"sort"
	"strings"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/sseutil"
)

// anthropicAdapter speaks the Anthropic Messages protocol.
type anthropicAdapter struct {
	s        Settings
	endpoint string
	header   func() http.Header
}

func (a *anthropicAdapter) Format() string { return FormatAnthropic }

func (a *anthropicAdapter) Build(req Request) (*Built, error) {
	msgs := a.messages(req)

	maxTokens := req.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = a.s.AnthropicMaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = 8192 // Messages API makes max_tokens mandatory
	}
	body := map[string]any{"model": req.Model, "max_tokens": maxTokens, "messages": msgs, "stream": true}
	if req.System != "" {
		sys := map[string]any{"type": "text", "text": req.System}
		if a.s.PromptCache {
			sys["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		body["system"] = []any{sys}
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tool := map[string]any{"name": t.Name, "description": t.Description}
			if len(t.Schema) > 0 {
				tool["input_schema"] = json.RawMessage(t.Schema)
			} else {
				tool["input_schema"] = map[string]any{"type": "object"}
			}
			tools = append(tools, tool)
		}
		body["tools"] = tools
		if req.ForbidToolCalls {
			body["tool_choice"] = map[string]any{"type": "none"}
		}
	}
	mergeExtra(body, a.s.ExtraBody)
	raw, err := marshalBody(body)
	if err != nil {
		return nil, fmt.Errorf("encode anthropic request: %w", err)
	}
	return &Built{Method: http.MethodPost, URL: a.endpoint, Header: a.header(), Body: raw, Format: FormatAnthropic}, nil
}

// messages renders canonical messages, merging adjacent same-role turns
// (the Messages API requires strict alternation) and placing a cache
// breakpoint on the final block.
func (a *anthropicAdapter) messages(req Request) []any {
	type turn struct {
		role   string
		blocks []any
	}
	var turns []*turn
	push := func(role string, blocks []any) {
		if len(blocks) == 0 {
			return
		}
		if n := len(turns); n > 0 && turns[n-1].role == role {
			turns[n-1].blocks = append(turns[n-1].blocks, blocks...)
			return
		}
		turns = append(turns, &turn{role: role, blocks: blocks})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleUser:
			push("user", a.userBlocks(req, m))
		case llm.RoleAssistant:
			if raw, ok := replayNative(m, FormatAnthropic); ok {
				var blocks []json.RawMessage
				if json.Unmarshal(raw, &blocks) == nil {
					bs := make([]any, len(blocks))
					for i, b := range blocks {
						bs[i] = b
					}
					push("assistant", bs)
					continue
				}
			}
			push("assistant", a.assistantBlocks(m))
		}
	}
	out := make([]any, 0, len(turns))
	for i, t := range turns {
		blocks := t.blocks
		if a.s.PromptCache && i == len(turns)-1 {
			blocks = withCacheControl(blocks)
		}
		out = append(out, map[string]any{"role": t.role, "content": blocks})
	}
	return out
}

// withCacheControl marks the last block ephemeral without mutating the input.
func withCacheControl(blocks []any) []any {
	if len(blocks) == 0 {
		return blocks
	}
	cp := append([]any(nil), blocks...)
	last := cp[len(cp)-1]
	var m map[string]any
	switch v := last.(type) {
	case map[string]any:
		m = make(map[string]any, len(v)+1)
		maps.Copy(m, v)
	case json.RawMessage:
		if json.Unmarshal(v, &m) != nil {
			return blocks
		}
	default:
		return blocks
	}
	m["cache_control"] = map[string]any{"type": "ephemeral"}
	cp[len(cp)-1] = m
	return cp
}

func (a *anthropicAdapter) mediaBlock(req Request, m *llm.Media) any {
	b64, ph := mediaPayload(req, m)
	if ph != "" {
		return map[string]any{"type": "text", "text": ph}
	}
	src := map[string]any{"type": "base64", "media_type": m.MIME, "data": b64}
	switch m.Kind {
	case llm.MediaImage:
		return map[string]any{"type": "image", "source": src}
	case llm.MediaPDF:
		return map[string]any{"type": "document", "source": src}
	}
	return map[string]any{"type": "text", "text": fmt.Sprintf("[%s attachment %q omitted: the Anthropic Messages API accepts only images and PDFs]", m.Kind, attachmentLabel(m))}
}

func (a *anthropicAdapter) userBlocks(req Request, m llm.Message) []any {
	var results, rest []any
	for _, p := range m.Parts {
		switch p.Type {
		case llm.PartToolResult:
			if p.Result == nil {
				continue
			}
			r := p.Result
			var content any = r.Output
			if len(r.Media) > 0 {
				cb := []any{map[string]any{"type": "text", "text": r.Output}}
				for i := range r.Media {
					cb = append(cb, a.mediaBlock(req, &r.Media[i]))
				}
				content = cb
			}
			blk := map[string]any{"type": "tool_result", "tool_use_id": r.CallID, "content": content}
			if r.IsError {
				blk["is_error"] = true
			}
			results = append(results, blk)
		case llm.PartText, llm.PartInfo:
			if p.Text != "" {
				rest = append(rest, map[string]any{"type": "text", "text": p.Text})
			}
		case llm.PartMedia:
			rest = append(rest, a.mediaBlock(req, p.Media))
		}
	}
	return append(results, rest...) // tool_result blocks must come first
}

func (a *anthropicAdapter) assistantBlocks(m llm.Message) []any {
	var blocks []any
	for _, p := range m.Parts {
		switch p.Type {
		case llm.PartText:
			if p.Text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
			}
		case llm.PartToolCall:
			if p.Call == nil {
				continue
			}
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": p.Call.ID, "name": p.Call.Name, "input": argsOrEmpty(p.Call.Args)})
		}
	}
	return blocks
}

// ---- streaming ----

type anEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage anUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		Signature string          `json:"signature"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Data      string          `json:"data"`
		Input     json.RawMessage `json:"input"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *anUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
}

type anBlock struct {
	kind      string // text | thinking | redacted_thinking | tool_use
	text      strings.Builder
	signature string
	id, name  string
	input     strings.Builder
	redacted  string
}

func (a *anthropicAdapter) ParseStream(r io.Reader, sink func(Delta)) (*Response, error) {
	sr := sseutil.NewReader(r)
	blocks := map[int]*anBlock{}
	var (
		stop            string
		in, out, cr, cc int
		sawUsage        bool
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
		var e anEvent
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return nil, fmt.Errorf("decode anthropic stream event: %w", err)
		}
		switch e.Type {
		case "error":
			if e.Error != nil {
				code := 500
				switch e.Error.Type {
				case "rate_limit_error":
					code = 429
				case "overloaded_error":
					code = 529
				case "invalid_request_error":
					code = 400
				case "authentication_error", "permission_error":
					code = 401
				}
				return nil, errFromStream(code, e.Error.Type, e.Error.Message)
			}
		case "message_start":
			if e.Message != nil {
				sawUsage = true
				in, cr, cc, out = e.Message.Usage.InputTokens, e.Message.Usage.CacheReadTokens, e.Message.Usage.CacheCreationTokens, e.Message.Usage.OutputTokens
			}
		case "content_block_start":
			if e.ContentBlock == nil {
				continue
			}
			b := &anBlock{kind: e.ContentBlock.Type, id: e.ContentBlock.ID, name: e.ContentBlock.Name, redacted: e.ContentBlock.Data}
			b.text.WriteString(e.ContentBlock.Text)
			b.text.WriteString(e.ContentBlock.Thinking)
			b.signature = e.ContentBlock.Signature
			if len(e.ContentBlock.Input) > 0 && string(e.ContentBlock.Input) != "{}" {
				b.input.Write(e.ContentBlock.Input)
			}
			blocks[e.Index] = b
			if b.kind == "tool_use" {
				sink(Delta{Kind: DeltaToolCall, Text: b.name})
			}
		case "content_block_delta":
			b := blocks[e.Index]
			if b == nil || e.Delta == nil {
				continue
			}
			switch e.Delta.Type {
			case "text_delta":
				b.text.WriteString(e.Delta.Text)
				sink(Delta{Kind: DeltaText, Text: e.Delta.Text})
			case "thinking_delta":
				b.text.WriteString(e.Delta.Thinking)
				sink(Delta{Kind: DeltaThought, Text: e.Delta.Thinking})
			case "signature_delta":
				b.signature += e.Delta.Signature
			case "input_json_delta":
				b.input.WriteString(e.Delta.PartialJSON)
			}
		case "message_delta":
			if e.Delta != nil && e.Delta.StopReason != "" {
				stop = e.Delta.StopReason
			}
			if e.Usage != nil {
				sawUsage = true
				if e.Usage.OutputTokens > 0 {
					out = e.Usage.OutputTokens
				}
				if e.Usage.InputTokens > 0 {
					in = e.Usage.InputTokens
				}
			}
		}
	}

	idx := make([]int, 0, len(blocks))
	for i := range blocks {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	msg := llm.Message{Role: llm.RoleAssistant}
	native := make([]any, 0, len(idx))
	hasCalls := false
	for _, i := range idx {
		b := blocks[i]
		switch b.kind {
		case "text":
			if b.text.Len() > 0 {
				msg.Parts = append(msg.Parts, llm.Part{Type: llm.PartText, Text: b.text.String()})
				native = append(native, map[string]any{"type": "text", "text": b.text.String()})
			}
		case "thinking":
			msg.Parts = append(msg.Parts, llm.Part{Type: llm.PartThought, Thought: &llm.Thought{Text: b.text.String(), Signature: b.signature}})
			native = append(native, map[string]any{"type": "thinking", "thinking": b.text.String(), "signature": b.signature})
		case "redacted_thinking":
			native = append(native, map[string]any{"type": "redacted_thinking", "data": b.redacted})
		case "tool_use":
			hasCalls = true
			args, argsErr := normaliseArgs(b.input.String())
			msg.Parts = append(msg.Parts, llm.Part{Type: llm.PartToolCall, Call: &llm.ToolCall{ID: b.id, Name: b.name, Args: args, ArgsError: argsErr}})
			native = append(native, map[string]any{"type": "tool_use", "id": b.id, "name": b.name, "input": args})
		}
	}
	if len(native) > 0 {
		raw, err := encodeJSON(native)
		if err != nil {
			return nil, err
		}
		msg.Native = &llm.Native{Format: FormatAnthropic, Raw: raw}
	}
	var usage *llm.Usage
	if sawUsage {
		prompt := in + cr + cc
		usage = &llm.Usage{PromptTokens: prompt, CompletionTokens: out, CachedTokens: cr, TotalTokens: prompt + out}
	}
	msg.FinishReason = finishFromTools(mapAnthropicStop(stop), hasCalls)
	msg.Usage = usage
	return &Response{Message: msg, Usage: usage}, nil
}

func mapAnthropicStop(s string) llm.FinishReason {
	switch s {
	case "end_turn", "stop_sequence", "":
		return llm.FinishStop
	case "tool_use":
		return llm.FinishToolCalls
	case "max_tokens":
		return llm.FinishLength
	case "refusal":
		return llm.FinishFiltered
	}
	return llm.FinishOther
}
