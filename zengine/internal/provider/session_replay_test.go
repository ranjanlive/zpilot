package provider

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ranjanlive/zpilot/zengine/internal/contextmgr"
	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
)

type replayBlobs string

func (root replayBlobs) Load(id string) ([]byte, error) {
	return os.ReadFile(filepath.Join(string(root), "blobs", id[:2], id))
}

// Opt-in read-only replay of a real session, with no provider calls or log edits.
func TestGeminiSavedSessionReplay(t *testing.T) {
	root := os.Getenv("ZENGINE_REPLAY_SESSION")
	if root == "" {
		t.Skip("set ZENGINE_REPLAY_SESSION for a read-only real-session check")
	}
	f, err := os.Open(filepath.Join(root, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 65536), 16<<20)
	var events []session.Event
	for scanner.Scan() {
		var event session.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	view := contextmgr.Build(events)
	for _, mode := range []string{"normal", "portable", "summary"} {
		messages := view.Messages
		if mode == "portable" {
			messages = view.WithPruned(contextmgr.BlobIDs(view.Attachments)).Messages
			for i := range messages {
				messages[i].Native = nil
			}
		}
		if mode == "summary" {
			messages = contextmgr.SummaryRequest(view)
			messages, _ = contextmgr.Repair(messages)
		}
		built, err := newGemini(t, Settings{}).Build(Request{Messages: messages, Blobs: replayBlobs(root), Caps: llm.Capabilities{Vision: true, Audio: true, Video: true, PDF: true}})
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(built.Body, &body); err != nil {
			t.Fatal(err)
		}
		responses := 0
		var omittedAt []int
		for contentIndex, content := range body["contents"].([]any) {
			for _, part := range content.(map[string]any)["parts"].([]any) {
				if fr, ok := part.(map[string]any)["functionResponse"].(map[string]any); ok {
					responses++
					if fr["response"].(map[string]any)["attachment_notes"] != nil {
						omittedAt = append(omittedAt, contentIndex)
					}
					if nested, ok := fr["parts"].([]any); ok {
						for _, media := range nested {
							if media.(map[string]any)["text"] != nil {
								t.Fatal("invalid nested text in saved-session replay")
							}
						}
					}
				}
			}
		}
		t.Logf("%s: %d messages, %d valid function responses, %d request bytes", mode, len(messages), responses, len(built.Body))
		t.Logf("%s: omitted tool attachments at contents indices %v", mode, omittedAt)
	}
}
