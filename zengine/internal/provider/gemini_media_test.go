package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
)

func TestGeminiToolMediaPlaceholdersStayOutsideMediaParts(t *testing.T) {
	for _, mode := range []string{"inline", "sibling"} {
		for _, scenario := range []string{"pruned", "missing", "unsupported", "corrupt", "mixed"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				media := llm.Media{Kind: llm.MediaImage, MIME: "image/png", Name: "screen.png", Path: "screen.png", Blob: "bad"}
				blobs := memBlobs{"good": testPNG(t), "bad": []byte{0xff, 0xfe, 0x89, 0, 'P', 0}}
				caps := llm.Capabilities{Vision: true}
				switch scenario {
				case "pruned":
					media.Blob = ""
				case "missing":
					media.Blob = "missing"
				case "unsupported":
					caps.Vision = false
				}
				attachments := []llm.Media{media}
				if scenario == "mixed" {
					valid := media
					valid.Blob = "good"
					attachments = append(attachments, valid)
				}
				g := newGemini(t, Settings{GeminiToolMedia: mode})
				built, err := g.Build(Request{Caps: caps, Blobs: blobs, Messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartToolResult, Result: &llm.ToolResult{Name: "view_file", Output: "image", Media: attachments}}}}}})
				if err != nil {
					t.Fatal(err)
				}
				var body map[string]any
				if err := json.Unmarshal(built.Body, &body); err != nil {
					t.Fatal(err)
				}
				parts := body["contents"].([]any)[0].(map[string]any)["parts"].([]any)
				fr := parts[0].(map[string]any)["functionResponse"].(map[string]any)
				if fr["response"].(map[string]any)["attachment_notes"] == nil {
					t.Fatal("missing attachment explanation")
				}
				if nested, ok := fr["parts"].([]any); ok {
					for _, part := range nested {
						if part.(map[string]any)["inlineData"] == nil || part.(map[string]any)["text"] != nil {
							t.Fatal("invalid FunctionResponsePart")
						}
					}
				} else if mode == "inline" && scenario == "mixed" {
					t.Fatal("valid media dropped")
				}
				if scenario != "mixed" && strings.Contains(string(built.Body), "inlineData") {
					t.Fatal("unavailable image sent")
				}
			})
		}
	}
}
