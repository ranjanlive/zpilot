//go:build live

package provider

// Live smoke tests against a real endpoint. Run with:
//
//	ZENGINE_LIVE_URL=https://host ZENGINE_LIVE_KEY=sk-... ZENGINE_LIVE_MODEL=name \
//	  go test -tags live -run Live -v ./internal/provider/
//
// Optionally ZENGINE_LIVE_FORMATS=gemini,openai,anthropic (default: all three).

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
)

func liveSettings(t *testing.T, format string) Settings {
	t.Helper()
	url, key, model := os.Getenv("ZENGINE_LIVE_URL"), os.Getenv("ZENGINE_LIVE_KEY"), os.Getenv("ZENGINE_LIVE_MODEL")
	if url == "" || key == "" || model == "" {
		t.Skip("set ZENGINE_LIVE_URL, ZENGINE_LIVE_KEY and ZENGINE_LIVE_MODEL")
	}
	if want := os.Getenv("ZENGINE_LIVE_FORMATS"); want != "" && !strings.Contains(want, format) {
		t.Skip("format not selected")
	}
	prov := map[string]string{FormatGemini: "gemini", FormatOpenAI: "openai_compatible", FormatAnthropic: "anthropic"}[format]
	m := model
	if override := os.Getenv("ZENGINE_LIVE_MODEL_" + strings.ToUpper(format)); override != "" {
		m = override
	}
	return Settings{Provider: prov, BaseURL: url, APIKey: key, Model: m, OpenAIStreamUsage: true,
		GeminiToolMedia: "inline", StallTimeout: 90 * time.Second}
}

func liveRoundTrip(t *testing.T, format string) {
	s := liveSettings(t, format)
	c, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	ep, _ := Endpoint(s)
	t.Logf("endpoint: %s  model: %s", ep, s.Model)

	tools := []llm.ToolSpec{{Name: "add", Description: "Add two integers and return the sum.",
		Schema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"],"additionalProperties":false}`)}}
	msgs := []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "Use the add tool to compute 1234 + 8766, then tell me the result."}}}}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	// turn 1: expect a tool call
	req := Request{System: "You are a terse assistant.", Model: s.Model, Tools: tools, Messages: msgs, MaxOutputTokens: 2048}
	built, err := c.Adapter.Build(req)
	if err != nil {
		t.Fatal(err)
	}
	var streamed strings.Builder
	resp, err := c.Send(ctx, built, func(d Delta) { streamed.WriteString(d.Text) })
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	calls := resp.Message.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "add" {
		t.Fatalf("turn 1 expected one add call, got %+v (text %q)", calls, resp.Message.Text())
	}
	var args struct{ A, B int }
	if err := json.Unmarshal(calls[0].Args, &args); err != nil || args.A+args.B != 10000 {
		t.Fatalf("bad args %s (%v)", calls[0].Args, err)
	}
	t.Logf("turn 1 ok: call id=%s synthetic=%v args=%s finish=%s usage=%+v", calls[0].ID, calls[0].Synthetic, calls[0].Args, resp.Message.FinishReason, resp.Usage)

	// turn 2: send the result, expect the final answer
	msgs = append(msgs, resp.Message, llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartToolResult, Result: &llm.ToolResult{
		CallID: calls[0].ID, CallSynthetic: calls[0].Synthetic, Name: "add", Output: `{"result": 10000}`}}}})
	req.Messages = msgs
	built, err = c.Adapter.Build(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := c.Send(ctx, built, nil)
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if !strings.Contains(strings.ReplaceAll(resp2.Message.Text(), ",", ""), "10000") {
		t.Fatalf("turn 2 answer lacks 10000: %q", resp2.Message.Text())
	}
	t.Logf("turn 2 ok: %q usage=%+v", resp2.Message.Text(), resp2.Usage)
}

func TestLiveGemini(t *testing.T)    { liveRoundTrip(t, FormatGemini) }
func TestLiveOpenAI(t *testing.T)    { liveRoundTrip(t, FormatOpenAI) }
func TestLiveAnthropic(t *testing.T) { liveRoundTrip(t, FormatAnthropic) }
