package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
)

type memBlobs map[string][]byte

func testPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func (m memBlobs) Load(id string) ([]byte, error) {
	if b, ok := m[id]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("blob %s not found", id)
}

func TestEndpointNormalisation(t *testing.T) {
	tests := []struct {
		name string
		s    Settings
		want string
		err  string
	}{
		{"gemini default host", Settings{Provider: "gemini", Model: "gemini-3.8-flash-high"}, "https://generativelanguage.googleapis.com/v1beta/models/gemini-3.8-flash-high:streamGenerateContent?alt=sse", ""},
		{"gemini gateway no path", Settings{Provider: "gemini", BaseURL: "https://api.zyreca.com", Model: "m"}, "https://api.zyreca.com/v1beta/models/m:streamGenerateContent?alt=sse", ""},
		{"gemini trailing slash", Settings{Provider: "gemini", BaseURL: "https://h.example/", Model: "m"}, "https://h.example/v1beta/models/m:streamGenerateContent?alt=sse", ""},
		{"gemini explicit v1", Settings{Provider: "gemini", BaseURL: "https://h.example/v1", Model: "m"}, "https://h.example/v1/models/m:streamGenerateContent?alt=sse", ""},
		{"gemini models/ prefix stripped", Settings{Provider: "gemini", BaseURL: "https://h.example", Model: "models/m"}, "https://h.example/v1beta/models/m:streamGenerateContent?alt=sse", ""},
		{"openai bare host gets /v1", Settings{Provider: "openai_compatible", BaseURL: "https://api.zyreca.com", Model: "m"}, "https://api.zyreca.com/v1/chat/completions", ""},
		{"openai with /v1", Settings{Provider: "openai_compatible", BaseURL: "https://h.example/v1/", Model: "m"}, "https://h.example/v1/chat/completions", ""},
		{"openai custom prefix respected", Settings{Provider: "openai_compatible", BaseURL: "https://h.example/api/v3", Model: "m"}, "https://h.example/api/v3/chat/completions", ""},
		{"openai gemini-compat prefix", Settings{Provider: "openai_compatible", BaseURL: "https://h.example/v1beta/openai", Model: "m"}, "https://h.example/v1beta/openai/chat/completions", ""},
		{"openai full path kept", Settings{Provider: "openai_compatible", BaseURL: "https://h.example/v1/chat/completions", Model: "m"}, "https://h.example/v1/chat/completions", ""},
		{"anthropic default host", Settings{Provider: "anthropic", Model: "m"}, "https://api.anthropic.com/v1/messages", ""},
		{"anthropic gateway", Settings{Provider: "anthropic", BaseURL: "https://api.zyreca.com", Model: "m"}, "https://api.zyreca.com/v1/messages", ""},
		{"custom exact", Settings{Provider: "custom", APIFormat: "openai", BaseURL: "https://h.example/weird/path?x=1"}, "https://h.example/weird/path?x=1", ""},
		{"custom needs format", Settings{Provider: "custom", BaseURL: "https://h.example/x"}, "", "API_FORMAT"},
		{"custom needs absolute url", Settings{Provider: "custom", APIFormat: "openai", BaseURL: "h.example/x"}, "", "absolute URL"},
		{"openai needs base", Settings{Provider: "openai_compatible", Model: "m"}, "", "BASE_URL is required"},
		{"unknown provider", Settings{Provider: "bogus"}, "", "unknown provider"},
		{"gemini needs model", Settings{Provider: "gemini", BaseURL: "https://h.example"}, "", "MODEL is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Endpoint(tc.s)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("want error containing %q, got %v (url %q)", tc.err, err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestAuthHeaders(t *testing.T) {
	h := authHeaders(Settings{APIKey: "k"}, FormatGemini)
	if h.Get("x-goog-api-key") != "k" || h.Get("Authorization") != "" {
		t.Fatalf("gemini headers: %v", h)
	}
	h = authHeaders(Settings{APIKey: "k"}, FormatOpenAI)
	if h.Get("Authorization") != "Bearer k" {
		t.Fatalf("openai headers: %v", h)
	}
	h = authHeaders(Settings{APIKey: "k"}, FormatAnthropic)
	if h.Get("x-api-key") != "k" || h.Get("anthropic-version") == "" {
		t.Fatalf("anthropic headers: %v", h)
	}
	h = authHeaders(Settings{APIKey: "k", AuthStyle: "bearer"}, FormatGemini)
	if h.Get("Authorization") != "Bearer k" || h.Get("x-goog-api-key") != "" {
		t.Fatalf("auth override: %v", h)
	}
	h = authHeaders(Settings{AuthStyle: "none", ExtraHeaders: map[string]string{"X-Team": "a"}}, FormatOpenAI)
	if h.Get("Authorization") != "" || h.Get("X-Team") != "a" {
		t.Fatalf("extra headers: %v", h)
	}
}

func TestClassifyHTTP(t *testing.T) {
	tests := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{413, `{"error":{"message":"Request body is too large"}}`, ErrTooLarge},
		{400, `Request body is too large`, ErrTooLarge},
		{429, `slow down`, ErrRateLimit},
		{429, `{"error":{"status":"RESOURCE_EXHAUSTED","message":"Too many concurrent requests for this API key"}}`, ErrRateLimit},
		{500, `oops`, ErrServer},
		{503, `unavailable`, ErrServer},
		{529, `overloaded`, ErrServer},
		{401, `bad key`, ErrAuth},
		{403, `nope`, ErrAuth},
		{400, `This model's maximum context length is 128000 tokens`, ErrContextLimit},
		{400, `prompt is too long: 250000 tokens > 200000 maximum`, ErrContextLimit},
		{400, `invalid argument`, ErrBadRequest},
		{404, `model not found`, ErrBadRequest},
	}
	for _, tc := range tests {
		if got := classifyHTTP(tc.status, http.Header{}, []byte(tc.body)); got.Kind != tc.want {
			t.Errorf("%d %q: got %s want %s", tc.status, tc.body, got.Kind, tc.want)
		}
	}
	h := http.Header{}
	h.Set("Retry-After", "7")
	if e := classifyHTTP(429, h, nil); e.RetryAfter != 7*time.Second || !e.Retryable() {
		t.Fatalf("retry-after not parsed: %+v", e)
	}
	if (&Error{Kind: ErrTooLarge}).Retryable() || (&Error{Kind: ErrAuth}).Retryable() {
		t.Fatal("too_large and auth must not be blindly retryable")
	}
}

// ---- Gemini ----

const geminiStream = `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Let me "}]}}]}

data: {"candidates":[{"content":{"role":"model","parts":[{"text":"check."}]}}]}

data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"add","args":{"a":1,"b":2}},"thoughtSignature":"SIG1"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":20,"totalTokenCount":35}}

`

func newGemini(t *testing.T, s Settings) *geminiAdapter {
	t.Helper()
	s.Provider = "gemini"
	if s.Model == "" {
		s.Model = "m"
	}
	if s.BaseURL == "" {
		s.BaseURL = "https://h.example"
	}
	c, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	return c.Adapter.(*geminiAdapter)
}

func TestGeminiParseStream(t *testing.T) {
	g := newGemini(t, Settings{})
	var deltas []string
	resp, err := g.ParseStream(strings.NewReader(geminiStream), func(d Delta) { deltas = append(deltas, string(d.Kind)+":"+d.Text) })
	if err != nil {
		t.Fatal(err)
	}
	m := resp.Message
	if m.Text() != "Let me check." {
		t.Fatalf("text not merged: %q", m.Text())
	}
	calls := m.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "add" || !calls[0].Synthetic || !strings.HasPrefix(calls[0].ID, "call_") {
		t.Fatalf("calls: %+v", calls)
	}
	if string(calls[0].Args) != `{"a":1,"b":2}` {
		t.Fatalf("args: %s", calls[0].Args)
	}
	if m.FinishReason != llm.FinishToolCalls {
		t.Fatalf("finish: %s", m.FinishReason)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 10 || resp.Usage.ThoughtTokens != 20 || resp.Usage.TotalTokens != 35 {
		t.Fatalf("usage: %+v", resp.Usage)
	}
	if m.Native == nil || !strings.Contains(string(m.Native.Raw), `"thoughtSignature":"SIG1"`) {
		t.Fatalf("native replay lost the signature: %v", m.Native)
	}
	if strings.Join(deltas, "|") != "text:Let me |text:check.|tool_call:add" {
		t.Fatalf("deltas: %v", deltas)
	}
}

func TestGeminiSignatureOnlyPartIsKeptSeparate(t *testing.T) {
	g := newGemini(t, Settings{})
	stream := `data: {"candidates":[{"content":{"parts":[{"text":"Hello"},{"text":"","thoughtSignature":"S"}]},"finishReason":"STOP"}]}` + "\n\n"
	resp, err := g.ParseStream(strings.NewReader(stream), func(Delta) {})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resp.Message.Native.Raw); got != `[{"text":"Hello"},{"text":"","thoughtSignature":"S"}]` {
		t.Fatalf("native: %s", got)
	}
	if resp.Message.Text() != "Hello" {
		t.Fatalf("text: %q", resp.Message.Text())
	}
}

func TestGeminiBuildRoundTrip(t *testing.T) {
	g := newGemini(t, Settings{})
	blobs := memBlobs{"img": testPNG(t)}
	call := llm.ToolCall{ID: "call_1", Name: "view_file", Args: json.RawMessage(`{"path":"a.png"}`), Synthetic: true}
	req := Request{
		System: "sys", Model: "m", MaxOutputTokens: 100, Blobs: blobs,
		Caps:  llm.Capabilities{Vision: true},
		Tools: []llm.ToolSpec{{Name: "view_file", Description: "d", Schema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "look"}}},
			{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, Call: &call}},
				Native: &llm.Native{Format: FormatGemini, Raw: json.RawMessage(`[{"functionCall":{"name":"view_file","args":{"path":"a.png"}},"thoughtSignature":"SIG"}]`)}},
			{Role: llm.RoleUser, Parts: []llm.Part{
				{Type: llm.PartInfo, Text: "[info: workspace changed]"},
				{Type: llm.PartToolResult, Result: &llm.ToolResult{CallID: "call_1", CallSynthetic: true, Name: "view_file", Output: "ok",
					Media: []llm.Media{{Kind: llm.MediaImage, MIME: "image/png", Blob: "img", Size: 7, Name: "a.png"}}}},
			}},
		},
	}
	built, err := g.Build(req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(built.Body, &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	contents := body["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents: %d", len(contents))
	}
	model := contents[1].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if model["thoughtSignature"] != "SIG" {
		t.Fatalf("native signature not replayed: %v", model)
	}
	user := contents[2].(map[string]any)["parts"].([]any)
	fr := user[0].(map[string]any)["functionResponse"].(map[string]any)
	if fr["name"] != "view_file" {
		t.Fatalf("functionResponse must come first: %v", user)
	}
	if _, hasID := fr["id"]; hasID {
		t.Fatal("synthetic call IDs must not be echoed to Gemini")
	}
	if fr["parts"] == nil {
		t.Fatalf("tool media should be inline in functionResponse.parts: %v", fr)
	}
	if user[1].(map[string]any)["text"] != "[info: workspace changed]" {
		t.Fatalf("info part missing: %v", user)
	}
	if body["systemInstruction"] == nil || body["tools"] == nil {
		t.Fatalf("system/tools missing: %v", body)
	}
	if built.Header.Get("x-goog-api-key") != "" {
		// no key configured in this test; header must simply not be set
		t.Fatal("unexpected key header")
	}
	// Determinism: identical input must produce byte-identical bodies (cache friendliness).
	again, _ := g.Build(req)
	if string(again.Body) != string(built.Body) {
		t.Fatal("Build is not deterministic")
	}
}

func TestGeminiUnsupportedAndRemovedMediaBecomeText(t *testing.T) {
	g := newGemini(t, Settings{})
	req := Request{Model: "m", Caps: llm.Capabilities{Vision: true}, Blobs: memBlobs{},
		Messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{
			{Type: llm.PartMedia, Media: &llm.Media{Kind: llm.MediaAudio, MIME: "audio/wav", Blob: "x", Name: "a.wav"}},
			{Type: llm.PartMedia, Media: &llm.Media{Kind: llm.MediaImage, MIME: "image/png", Name: "gone.png", Path: "/w/gone.png", Size: 9}},
		}}}}
	built, err := g.Build(req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(built.Body)
	if strings.Contains(s, "inlineData") {
		t.Fatalf("no inlineData expected: %s", s)
	}
	if !strings.Contains(s, "does not accept audio input") || !strings.Contains(s, "re-open it with view_file at /w/gone.png") {
		t.Fatalf("placeholders missing: %s", s)
	}
}

func TestGeminiForbidToolCallsAndExtraBody(t *testing.T) {
	g := newGemini(t, Settings{ExtraBody: map[string]any{"generationConfig": map[string]any{"thinkingConfig": map[string]any{"thinkingLevel": "high"}}}})
	temp := 0.2
	built, _ := g.Build(Request{Model: "m", ForbidToolCalls: true, MaxOutputTokens: 50, Temperature: &temp,
		Tools:    []llm.ToolSpec{{Name: "t", Schema: json.RawMessage(`{}`)}},
		Messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "x"}}}}})
	s := string(built.Body)
	for _, want := range []string{`"mode":"NONE"`, `"thinkingLevel":"high"`, `"maxOutputTokens":50`, `"temperature":0.2`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
}

// ---- OpenAI ----

const openAIStream = `data: {"choices":[{"delta":{"role":"assistant","reasoning_content":"think "}}]}

data: {"choices":[{"delta":{"content":"Hi "}}]}

data: {"choices":[{"delta":{"content":"there"}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"add","arguments":"{\"a\":"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1,\"b\":2}"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"name":"sub","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":8,"completion_tokens":259,"total_tokens":267,"completion_tokens_details":{"reasoning_tokens":248},"prompt_tokens_details":{"cached_tokens":3}}}

data: [DONE]

`

func newOpenAI(t *testing.T, s Settings) *openAIAdapter {
	t.Helper()
	s.Provider, s.Model, s.BaseURL = "openai_compatible", "m", "https://h.example"
	c, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	return c.Adapter.(*openAIAdapter)
}

func TestOpenAIParseStream(t *testing.T) {
	o := newOpenAI(t, Settings{})
	resp, err := o.ParseStream(strings.NewReader(openAIStream), func(Delta) {})
	if err != nil {
		t.Fatal(err)
	}
	m := resp.Message
	if m.Text() != "Hi there" {
		t.Fatalf("text: %q", m.Text())
	}
	calls := m.ToolCalls()
	if len(calls) != 2 || calls[0].ID != "call_a" || string(calls[0].Args) != `{"a":1,"b":2}` {
		t.Fatalf("calls: %+v", calls)
	}
	if !calls[1].Synthetic || calls[1].Name != "sub" {
		t.Fatalf("second call: %+v", calls[1])
	}
	if resp.Usage.PromptTokens != 8 || resp.Usage.ThoughtTokens != 248 || resp.Usage.CompletionTokens != 11 || resp.Usage.CachedTokens != 3 {
		t.Fatalf("usage: %+v", resp.Usage)
	}
	if m.FinishReason != llm.FinishToolCalls {
		t.Fatalf("finish: %s", m.FinishReason)
	}
	hasThought := false
	for _, p := range m.Parts {
		hasThought = hasThought || p.Type == llm.PartThought
	}
	if !hasThought {
		t.Fatal("reasoning not captured")
	}
}

func TestOpenAIBuildToolRoundTrip(t *testing.T) {
	o := newOpenAI(t, Settings{OpenAIStreamUsage: true})
	call := llm.ToolCall{ID: "call_a", Name: "add", Args: json.RawMessage(`{"a":1}`)}
	req := Request{System: "sys", Model: "m", Caps: llm.Capabilities{Vision: true}, Blobs: memBlobs{"i": testPNG(t)},
		Tools: []llm.ToolSpec{{Name: "add", Description: "d", Schema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "go"}}},
			{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, Call: &call}}},
			{Role: llm.RoleUser, Parts: []llm.Part{
				{Type: llm.PartToolResult, Result: &llm.ToolResult{CallID: "call_a", Name: "add", Output: "3",
					Media: []llm.Media{{Kind: llm.MediaImage, MIME: "image/png", Blob: "i", Size: 1}}}},
				{Type: llm.PartInfo, Text: "[info: x]"},
			}},
		}}
	built, err := o.Build(req)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages      []map[string]any `json:"messages"`
		StreamOptions map[string]any   `json:"stream_options"`
	}
	if err := json.Unmarshal(built.Body, &body); err != nil {
		t.Fatal(err)
	}
	roles := []string{}
	for _, m := range body.Messages {
		roles = append(roles, m["role"].(string))
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool,user" {
		t.Fatalf("roles: %v", roles)
	}
	if body.Messages[3]["tool_call_id"] != "call_a" {
		t.Fatalf("tool message: %v", body.Messages[3])
	}
	trailing := body.Messages[4]["content"].([]any)
	if trailing[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("tool media should arrive in a trailing user message: %v", trailing)
	}
	if body.StreamOptions["include_usage"] != true {
		t.Fatal("stream usage not requested")
	}
}

func TestOpenAINativeReplayDropsReasoning(t *testing.T) {
	o := newOpenAI(t, Settings{})
	raw := json.RawMessage(`{"role":"assistant","content":"hi","reasoning_content":"secret thoughts"}`)
	built, _ := o.Build(Request{Model: "m", Messages: []llm.Message{
		{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "x"}}},
		{Role: llm.RoleAssistant, Native: &llm.Native{Format: FormatOpenAI, Raw: raw}},
	}})
	if strings.Contains(string(built.Body), "secret thoughts") {
		t.Fatal("reasoning leaked into replay")
	}
	o2 := newOpenAI(t, Settings{OpenAIReplayReasoning: true})
	built2, _ := o2.Build(Request{Model: "m", Messages: []llm.Message{
		{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "x"}}},
		{Role: llm.RoleAssistant, Native: &llm.Native{Format: FormatOpenAI, Raw: raw}},
	}})
	if !strings.Contains(string(built2.Body), "secret thoughts") {
		t.Fatal("opt-in replay of reasoning failed")
	}
}

func TestOpenAIAudioAndPDFParts(t *testing.T) {
	o := newOpenAI(t, Settings{})
	req := Request{Model: "m", Caps: llm.Capabilities{Audio: true, PDF: true, Video: true}, Blobs: memBlobs{"a": []byte("1"), "p": []byte("2"), "v": []byte("3")},
		Messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{
			{Type: llm.PartMedia, Media: &llm.Media{Kind: llm.MediaAudio, MIME: "audio/wav", Blob: "a"}},
			{Type: llm.PartMedia, Media: &llm.Media{Kind: llm.MediaAudio, MIME: "audio/flac", Blob: "a", Name: "x.flac"}},
			{Type: llm.PartMedia, Media: &llm.Media{Kind: llm.MediaPDF, MIME: "application/pdf", Blob: "p", Name: "d.pdf"}},
			{Type: llm.PartMedia, Media: &llm.Media{Kind: llm.MediaVideo, MIME: "video/mp4", Blob: "v", Name: "c.mp4"}},
		}}}}
	built, _ := o.Build(req)
	s := string(built.Body)
	for _, want := range []string{`"input_audio"`, `"format":"wav"`, `OpenAI-format APIs accept only wav and mp3`, `"file_data":"data:application/pdf;base64,Mg=="`, `not supported by the OpenAI-format API`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
}

// ---- Anthropic ----

const anthropicStream = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":25,"cache_read_input_tokens":100,"cache_creation_input_tokens":0,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"SIGX"}}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Adding"}}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"add","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1,"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"b\":2}"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

`

func newAnthropic(t *testing.T, s Settings) *anthropicAdapter {
	t.Helper()
	s.Provider, s.Model, s.BaseURL = "anthropic", "m", "https://h.example"
	c, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	return c.Adapter.(*anthropicAdapter)
}

func TestAnthropicParseStream(t *testing.T) {
	a := newAnthropic(t, Settings{})
	resp, err := a.ParseStream(strings.NewReader(anthropicStream), func(Delta) {})
	if err != nil {
		t.Fatal(err)
	}
	m := resp.Message
	if m.Text() != "Adding" {
		t.Fatalf("text: %q", m.Text())
	}
	calls := m.ToolCalls()
	if len(calls) != 1 || calls[0].ID != "toolu_1" || string(calls[0].Args) != `{"a":1,"b":2}` {
		t.Fatalf("calls: %+v", calls)
	}
	if m.FinishReason != llm.FinishToolCalls {
		t.Fatalf("finish: %s", m.FinishReason)
	}
	if resp.Usage.PromptTokens != 125 || resp.Usage.CachedTokens != 100 || resp.Usage.CompletionTokens != 42 || resp.Usage.TotalTokens != 167 {
		t.Fatalf("usage: %+v", resp.Usage)
	}
	raw := string(m.Native.Raw)
	if !strings.Contains(raw, `"signature":"SIGX"`) || !strings.Contains(raw, `"type":"tool_use"`) {
		t.Fatalf("native: %s", raw)
	}
}

func TestAnthropicBuildMergesTurnsAndCaches(t *testing.T) {
	a := newAnthropic(t, Settings{PromptCache: true})
	call := llm.ToolCall{ID: "toolu_1", Name: "add", Args: json.RawMessage(`{}`)}
	req := Request{System: "sys", Model: "m",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "a"}}},
			{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "b"}}}, // adjacent user turns must merge
			{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, Call: &call}}},
			{Role: llm.RoleUser, Parts: []llm.Part{
				{Type: llm.PartText, Text: "[info: n]"},
				{Type: llm.PartToolResult, Result: &llm.ToolResult{CallID: "toolu_1", Name: "add", Output: "3", IsError: true}},
			}},
		}}
	built, err := a.Build(req)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		MaxTokens int `json:"max_tokens"`
		System    []map[string]any
		Messages  []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		}
	}
	if err := json.Unmarshal(built.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.MaxTokens != 8192 {
		t.Fatalf("max_tokens default: %d", body.MaxTokens)
	}
	if len(body.Messages) != 3 || body.Messages[0].Role != "user" || len(body.Messages[0].Content) != 2 {
		t.Fatalf("turns not merged: %+v", body.Messages)
	}
	last := body.Messages[2].Content
	if last[0]["type"] != "tool_result" || last[0]["is_error"] != true {
		t.Fatalf("tool_result must lead the user turn: %v", last)
	}
	if last[len(last)-1]["cache_control"] == nil || body.System[0]["cache_control"] == nil {
		t.Fatalf("cache breakpoints missing: %v / %v", last, body.System)
	}
	if built.Header.Get("anthropic-version") == "" {
		t.Fatal("anthropic-version header missing")
	}
}

// ---- Client end to end ----

func TestClientSendStreamsAndClassifiesErrors(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b := new(strings.Builder)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		gotBody = b.String()
		switch r.URL.Query().Get("mode") {
		case "413":
			w.WriteHeader(413)
			fmt.Fprint(w, `{"error":{"message":"Request body is too large"}}`)
		case "429":
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(429)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, openAIStream)
		}
	}))
	defer srv.Close()

	mk := func(mode string) *Client {
		c, err := New(Settings{Provider: "custom", APIFormat: "openai", BaseURL: srv.URL + "/x?mode=" + mode, APIKey: "KEY", Model: "m", OpenAIStreamUsage: true})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	req := Request{Model: "m", Messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "hi"}}}}}

	c := mk("ok")
	built, _ := c.Adapter.Build(req)
	var text strings.Builder
	resp, err := c.Send(context.Background(), built, func(d Delta) {
		if d.Kind == DeltaText {
			text.WriteString(d.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "Hi there" || resp.Message.Text() != "Hi there" || gotAuth != "Bearer KEY" || !strings.Contains(gotBody, `"stream":true`) {
		t.Fatalf("send: text=%q auth=%q body=%s", text.String(), gotAuth, gotBody)
	}

	c = mk("413")
	built, _ = c.Adapter.Build(req)
	if _, err := c.Send(context.Background(), built, nil); AsError(err).Kind != ErrTooLarge {
		t.Fatalf("want too_large, got %v", err)
	}
	c = mk("429")
	built, _ = c.Adapter.Build(req)
	_, err = c.Send(context.Background(), built, nil)
	if pe := AsError(err); pe.Kind != ErrRateLimit || pe.RetryAfter != 3*time.Second {
		t.Fatalf("want rate_limit with Retry-After, got %+v", pe)
	}
}

func TestClientStallWatchdog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select { // then go silent until the client gives up
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	c, _ := New(Settings{Provider: "custom", APIFormat: "openai", BaseURL: srv.URL, Model: "m", StallTimeout: 150 * time.Millisecond})
	built, _ := c.Adapter.Build(Request{Model: "m", Messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "x"}}}}})
	start := time.Now()
	_, err := c.Send(context.Background(), built, nil)
	if pe := AsError(err); pe.Kind != ErrStalled || !pe.Retryable() {
		t.Fatalf("want stalled, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("watchdog too slow")
	}
}

func TestClientCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	c, _ := New(Settings{Provider: "custom", APIFormat: "openai", BaseURL: srv.URL, Model: "m"})
	built, _ := c.Adapter.Build(Request{Model: "m", Messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "x"}}}}})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	if _, err := c.Send(ctx, built, nil); AsError(err).Kind != ErrCanceled {
		t.Fatalf("want canceled, got %v", err)
	}
}
