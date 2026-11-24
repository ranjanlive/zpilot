package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/sseutil"
)

// geminiAdapter speaks the Gemini generateContent protocol (also served by
// many gateways under /v1beta).
type geminiAdapter struct {
	s        Settings
	endpoint string
	header   func() http.Header
}

func (g *geminiAdapter) Format() string { return FormatGemini }

func (g *geminiAdapter) Build(req Request) (*Built, error) {
	contents := make([]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleUser:
			parts := g.userParts(req, m)
			if len(parts) == 0 {
				continue
			}
			contents = append(contents, map[string]any{"role": "user", "parts": parts})
		case llm.RoleAssistant:
			if raw, ok := replayNative(m, FormatGemini); ok {
				contents = append(contents, map[string]any{"role": "model", "parts": raw})
				continue
			}
			parts := g.assistantParts(m)
			if len(parts) == 0 {
				continue
			}
			contents = append(contents, map[string]any{"role": "model", "parts": parts})
		}
	}

	body := map[string]any{"contents": contents}
	if req.System != "" {
		body["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": req.System}}}
	}
	if len(req.Tools) > 0 {
		decls := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			d := map[string]any{"name": t.Name, "description": t.Description}
			if len(t.Schema) > 0 {
				d["parametersJsonSchema"] = json.RawMessage(t.Schema)
			}
			decls = append(decls, d)
		}
		body["tools"] = []any{map[string]any{"functionDeclarations": decls}}
		if req.ForbidToolCalls {
			body["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "NONE"}}
		}
	}
	gen := map[string]any{}
	if req.MaxOutputTokens > 0 {
		gen["maxOutputTokens"] = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		gen["temperature"] = *req.Temperature
	}
	if len(gen) > 0 {
		body["generationConfig"] = gen
	}
	mergeExtra(body, g.s.ExtraBody)

	raw, err := marshalBody(body)
	if err != nil {
		return nil, fmt.Errorf("encode gemini request: %w", err)
	}
	raw, err = repairGeminiToolResponseParts(raw)
	if err != nil {
		return nil, fmt.Errorf("validate gemini request: %w", err)
	}
	return &Built{Method: http.MethodPost, URL: g.endpoint, Header: g.header(), Body: raw, Format: FormatGemini}, nil
}

func (g *geminiAdapter) inlineData(req Request, m *llm.Media) any {
	b64, ph := mediaPayload(req, m)
	if ph != "" {
		return map[string]any{"text": ph}
	}
	return map[string]any{"inlineData": map[string]any{"mimeType": m.MIME, "data": b64}}
}

// userParts renders a user-role message: function responses first (Gemini
// requires them to answer the preceding call turn), then everything else.
func (g *geminiAdapter) userParts(req Request, m llm.Message) []any {
	var responses, rest []any
	for _, p := range m.Parts {
		switch p.Type {
		case llm.PartText, llm.PartInfo:
			if p.Text != "" {
				rest = append(rest, map[string]any{"text": p.Text})
			}
		case llm.PartMedia:
			rest = append(rest, g.inlineData(req, p.Media))
		case llm.PartToolResult:
			if p.Result == nil {
				continue
			}
			r := p.Result
			out := map[string]any{"output": r.Output}
			if r.IsError {
				out = map[string]any{"error": r.Output}
			}
			fr := map[string]any{"name": r.Name, "response": out}
			if !r.CallSynthetic && r.CallID != "" {
				fr["id"] = r.CallID
			}
			if len(r.Media) > 0 {
				media := make([]any, 0, len(r.Media))
				var omitted []string
				for i := range r.Media {
					b64, placeholder := mediaPayload(req, &r.Media[i])
					if placeholder != "" {
						omitted = append(omitted, placeholder)
						continue
					}
					media = append(media, map[string]any{"inlineData": map[string]any{"mimeType": r.Media[i].MIME, "data": b64}})
				}
				// FunctionResponsePart accepts media only. Keep unavailable/pruned
				// attachment explanations in the structured response, never its parts.
				if len(omitted) > 0 {
					out["attachment_notes"] = omitted
				}
				if g.s.GeminiToolMedia == "sibling" {
					responses = append(responses, map[string]any{"functionResponse": fr})
					rest = append(rest, append([]any{map[string]any{"text": fmt.Sprintf("[media returned by %s]", r.Name)}}, media...)...)
					continue
				}
				if len(media) > 0 {
					fr["parts"] = media
				}
			}
			responses = append(responses, map[string]any{"functionResponse": fr})
		}
	}
	return append(responses, rest...)
}

func (g *geminiAdapter) assistantParts(m llm.Message) []any {
	var parts []any
	for _, p := range m.Parts {
		switch p.Type {
		case llm.PartText:
			if p.Text != "" {
				parts = append(parts, map[string]any{"text": p.Text})
			}
		case llm.PartToolCall:
			if p.Call == nil {
				continue
			}
			fc := map[string]any{"name": p.Call.Name, "args": argsOrEmpty(p.Call.Args)}
			if !p.Call.Synthetic && p.Call.ID != "" {
				fc["id"] = p.Call.ID
			}
			parts = append(parts, map[string]any{"functionCall": fc})
		}
	}
	return parts
}

func argsOrEmpty(a json.RawMessage) json.RawMessage {
	if len(strings.TrimSpace(string(a))) == 0 {
		return json.RawMessage(`{}`)
	}
	return a
}

// ---- streaming ----

type gemChunk struct {
	Candidates []struct {
		Content struct {
			Parts []json.RawMessage `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount"`
		ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
		TotalTokenCount         int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

type gemPart struct {
	Text             *string `json:"text"`
	Thought          bool    `json:"thought"`
	ThoughtSignature string  `json:"thoughtSignature"`
	FunctionCall     *struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
		ID   string          `json:"id"`
	} `json:"functionCall"`
}

// accPart is a streamed part being assembled.
type accPart struct {
	raw       json.RawMessage // verbatim, for part kinds we do not interpret
	isText    bool
	thought   bool
	text      strings.Builder
	sig       string
	callName  string
	callArgs  json.RawMessage
	callID    string
	isCall    bool
	synthetic bool
}

func (g *geminiAdapter) ParseStream(r io.Reader, sink func(Delta)) (*Response, error) {
	sr := sseutil.NewReader(r)
	var (
		parts   []*accPart
		usage   *llm.Usage
		finish  string
		blocked string
		callSeq int
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
		if data == "" || data == "[DONE]" {
			continue
		}
		var ch gemChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return nil, fmt.Errorf("decode gemini stream chunk: %w", err)
		}
		if ch.Error != nil {
			return nil, errFromStream(ch.Error.Code, ch.Error.Status, ch.Error.Message)
		}
		if ch.PromptFeedback != nil && ch.PromptFeedback.BlockReason != "" {
			blocked = ch.PromptFeedback.BlockReason
		}
		if um := ch.UsageMetadata; um != nil && um.TotalTokenCount > 0 {
			usage = &llm.Usage{
				PromptTokens: um.PromptTokenCount, CompletionTokens: um.CandidatesTokenCount,
				ThoughtTokens: um.ThoughtsTokenCount, CachedTokens: um.CachedContentTokenCount, TotalTokens: um.TotalTokenCount,
			}
		}
		for _, cand := range ch.Candidates {
			if cand.FinishReason != "" {
				finish = cand.FinishReason
			}
			for _, rawPart := range cand.Content.Parts {
				var p gemPart
				if err := json.Unmarshal(rawPart, &p); err != nil {
					return nil, fmt.Errorf("decode gemini part: %w", err)
				}
				switch {
				case p.FunctionCall != nil:
					callSeq++
					ap := &accPart{isCall: true, callName: p.FunctionCall.Name, callArgs: p.FunctionCall.Args, callID: p.FunctionCall.ID, sig: p.ThoughtSignature}
					if ap.callID == "" {
						ap.callID, ap.synthetic = newSynthID(), true
					}
					parts = append(parts, ap)
					sink(Delta{Kind: DeltaToolCall, Text: p.FunctionCall.Name})
				case p.Text != nil:
					kind := DeltaText
					if p.Thought {
						kind = DeltaThought
					}
					if *p.Text != "" {
						sink(Delta{Kind: kind, Text: *p.Text})
					}
					if n := len(parts); n > 0 && parts[n-1].isText && parts[n-1].thought == p.Thought && parts[n-1].sig == "" && p.ThoughtSignature == "" {
						parts[n-1].text.WriteString(*p.Text)
						continue
					}
					ap := &accPart{isText: true, thought: p.Thought, sig: p.ThoughtSignature}
					ap.text.WriteString(*p.Text)
					parts = append(parts, ap)
				default:
					parts = append(parts, &accPart{raw: append(json.RawMessage(nil), rawPart...)})
				}
			}
		}
	}

	msg := llm.Message{Role: llm.RoleAssistant}
	nativeParts := make([]any, 0, len(parts))
	for _, ap := range parts {
		switch {
		case ap.isCall:
			args := argsOrEmpty(ap.callArgs)
			msg.Parts = append(msg.Parts, llm.Part{Type: llm.PartToolCall, Call: &llm.ToolCall{ID: ap.callID, Name: ap.callName, Args: args, Synthetic: ap.synthetic}})
			fc := map[string]any{"name": ap.callName, "args": args}
			if !ap.synthetic {
				fc["id"] = ap.callID
			}
			np := map[string]any{"functionCall": fc}
			if ap.sig != "" {
				np["thoughtSignature"] = ap.sig
			}
			nativeParts = append(nativeParts, np)
		case ap.isText:
			txt := ap.text.String()
			np := map[string]any{"text": txt}
			if ap.thought {
				np["thought"] = true
				msg.Parts = append(msg.Parts, llm.Part{Type: llm.PartThought, Thought: &llm.Thought{Text: txt, Signature: ap.sig}})
			} else if txt != "" {
				msg.Parts = append(msg.Parts, llm.Part{Type: llm.PartText, Text: txt})
			} else if ap.sig != "" {
				msg.Parts = append(msg.Parts, llm.Part{Type: llm.PartThought, Thought: &llm.Thought{Signature: ap.sig}})
			}
			if ap.sig != "" {
				np["thoughtSignature"] = ap.sig
			}
			nativeParts = append(nativeParts, np)
		default:
			nativeParts = append(nativeParts, ap.raw)
		}
	}
	if len(nativeParts) > 0 {
		raw, err := encodeJSON(nativeParts)
		if err != nil {
			return nil, err
		}
		msg.Native = &llm.Native{Format: FormatGemini, Raw: raw}
	}
	msg.FinishReason = finishFromTools(mapGeminiFinish(finish, blocked), len(msg.ToolCalls()) > 0)
	msg.Usage = usage
	return &Response{Message: msg, Usage: usage}, nil
}

func mapGeminiFinish(fr, blocked string) llm.FinishReason {
	if blocked != "" {
		return llm.FinishFiltered
	}
	switch fr {
	case "STOP", "":
		return llm.FinishStop
	case "MAX_TOKENS":
		return llm.FinishLength
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
		return llm.FinishFiltered
	}
	return llm.FinishOther
}
