package provider

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestGeminiPreflightRepairsNestedTextWithoutLosingContext(t *testing.T) {
	raw := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"view_file","id":"c1","args":{}},"thoughtSignature":"signature"}]},{"role":"user","parts":[{"functionResponse":{"name":"view_file","id":"c1","response":{"output":"image <saved>","attachment_notes":["existing"]},"parts":[{"text":"removed; reopen page.png"},{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}},{"text":"missing second.png"}]}},{"text":"continue"}]}]}`)
	fixed, err := repairGeminiToolResponseParts(raw)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Contents []struct {
			Parts []map[string]json.RawMessage `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(fixed, &body); err != nil {
		t.Fatal(err)
	}
	var response struct {
		ID       string `json:"id"`
		Response struct {
			Output string   `json:"output"`
			Notes  []string `json:"attachment_notes"`
		} `json:"response"`
		Parts []map[string]json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal(body.Contents[1].Parts[0]["functionResponse"], &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != "c1" || response.Response.Output != "image <saved>" || strings.Join(response.Response.Notes, "|") != "existing|removed; reopen page.png|missing second.png" {
		t.Fatalf("lost tool result information: %+v", response)
	}
	if len(response.Parts) != 1 || response.Parts[0]["inlineData"] == nil {
		t.Fatal("valid media must survive repair")
	}
	if !bytes.Equal(body.Contents[0].Parts[0]["thoughtSignature"], []byte(`"signature"`)) || string(body.Contents[1].Parts[1]["text"]) != `"continue"` {
		t.Fatal("unrelated context changed")
	}
	again, err := repairGeminiToolResponseParts(fixed)
	if err != nil || !bytes.Equal(fixed, again) {
		t.Fatalf("repair must be idempotent: %v", err)
	}
}

func TestGeminiPreflightRunsAfterExtraBodyAndDoesNotMutateSettings(t *testing.T) {
	const input = `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"view_file","response":{"error":"failed"},"parts":[{"text":"removed page.png"}]}}]}]}`
	var extra map[string]any
	if err := json.Unmarshal([]byte(input), &extra); err != nil {
		t.Fatal(err)
	}
	g := newGemini(t, Settings{ExtraBody: extra})
	first, err := g.Build(Request{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(first.Body), `"parts":[{"text"`) || !strings.Contains(string(first.Body), `"attachment_notes":["removed page.png"]`) || !strings.Contains(string(first.Body), `"error":"failed"`) {
		t.Fatalf("placeholder was not safely relocated: %s", first.Body)
	}
	second, err := g.Build(Request{})
	if err != nil || !bytes.Equal(first.Body, second.Body) {
		t.Fatalf("non-deterministic build: %v", err)
	}
	original, _ := json.Marshal(extra)
	if !strings.Contains(string(original), `"parts":[{"text":"removed page.png"}]`) {
		t.Fatal("build mutated provider settings")
	}
}

func TestGeminiPreflightRejectsAmbiguousNestedParts(t *testing.T) {
	for _, part := range []string{`{"text":42}`, `{"text":null}`, `{"text":"note","inlineData":{}}`, `{"fileData":{}}`, `{}`, `{"inlineData":null}`, `{"inlineData":{"mimeType":"image/png"}}`} {
		raw := []byte(`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"view_file","response":{"output":"keep"},"parts":[` + part + `]}}]}]}`)
		if _, err := repairGeminiToolResponseParts(raw); err == nil || !strings.Contains(err.Error(), "contents[0].parts[0].functionResponse.parts[0]") {
			t.Fatalf("expected precise local error for %s, got %v", part, err)
		}
	}
}
