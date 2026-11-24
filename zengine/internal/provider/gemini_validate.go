package provider

import (
	"encoding/json"
	"fmt"
)

// repairGeminiToolResponseParts checks the final wire representation, including
// extra_body overrides. Ordinary Content parts accept text, but nested
// FunctionResponse parts accept only inlineData. Repair the known placeholder
// mistake without discarding tool output, media, IDs or thought signatures.
// Unchanged requests retain their original bytes for stable prefix caching.
func repairGeminiToolResponseParts(raw []byte) ([]byte, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	var contents []map[string]json.RawMessage
	if err := json.Unmarshal(body["contents"], &contents); err != nil {
		return nil, fmt.Errorf("contents: %w", err)
	}
	changed := false
	for i, content := range contents {
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(content["parts"], &parts); err != nil {
			return nil, fmt.Errorf("contents[%d].parts: %w", i, err)
		}
		contentChanged := false
		for j, part := range parts {
			frRaw, ok := part["functionResponse"]
			if !ok {
				continue
			}
			var fr map[string]json.RawMessage
			if err := json.Unmarshal(frRaw, &fr); err != nil {
				return nil, fmt.Errorf("contents[%d].parts[%d].functionResponse: %w", i, j, err)
			}
			nestedRaw, ok := fr["parts"]
			if !ok {
				continue
			}
			var nested []map[string]json.RawMessage
			if err := json.Unmarshal(nestedRaw, &nested); err != nil {
				return nil, fmt.Errorf("contents[%d].parts[%d].functionResponse.parts: %w", i, j, err)
			}
			var notes []string
			media := make([]map[string]json.RawMessage, 0, len(nested))
			for k, p := range nested {
				path := fmt.Sprintf("contents[%d].parts[%d].functionResponse.parts[%d]", i, j, k)
				if txt, ok := p["text"]; ok && len(p) == 1 {
					var note string
					if err := json.Unmarshal(txt, &note); err != nil || string(txt) == "null" {
						return nil, fmt.Errorf("%s.text must be a string", path)
					}
					notes = append(notes, note)
					continue
				}
				if _, ok := p["inlineData"]; !ok || len(p) != 1 {
					return nil, fmt.Errorf("%s must contain only inlineData (text belongs in functionResponse.response)", path)
				}
				var blob struct {
					MIME string `json:"mimeType"`
					Data string `json:"data"`
				}
				if err := json.Unmarshal(p["inlineData"], &blob); err != nil || blob.MIME == "" || blob.Data == "" {
					return nil, fmt.Errorf("%s.inlineData requires nonempty mimeType and data", path)
				}
				media = append(media, p)
			}
			if len(notes) == 0 {
				continue
			}
			var response map[string]json.RawMessage
			if err := json.Unmarshal(fr["response"], &response); err != nil || response == nil {
				return nil, fmt.Errorf("contents[%d].parts[%d].functionResponse.response must be an object", i, j)
			}
			// Keep any existing notes, even those supplied by an extension.
			var allNotes []json.RawMessage
			if existing, ok := response["attachment_notes"]; ok {
				if err := json.Unmarshal(existing, &allNotes); err != nil {
					allNotes = []json.RawMessage{existing}
				}
			}
			for _, note := range notes {
				encoded, _ := json.Marshal(note)
				allNotes = append(allNotes, encoded)
			}
			response["attachment_notes"], _ = json.Marshal(allNotes)
			fr["response"], _ = json.Marshal(response)
			delete(fr, "parts")
			if len(media) > 0 {
				fr["parts"], _ = json.Marshal(media)
			}
			part["functionResponse"], _ = json.Marshal(fr)
			contentChanged = true
		}
		if contentChanged {
			content["parts"], _ = json.Marshal(parts)
			changed = true
		}
	}
	if !changed {
		return raw, nil
	}
	body["contents"], _ = json.Marshal(contents)
	return encodeJSON(body)
}
