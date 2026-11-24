package llm

import (
	"strings"
	"testing"
)

const readSchema = `{
  "type": "object",
  "properties": {
    "path":  {"type": "string"},
    "start": {"type": "integer", "minimum": 1},
    "mode":  {"type": "string", "enum": ["text", "binary"]},
    "tags":  {"type": "array", "items": {"type": "string"}}
  },
  "required": ["path"],
  "additionalProperties": false
}`

func TestValidateArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    string
		want    []string // substrings, one per expected problem
		wantNil bool
	}{
		{name: "valid", args: `{"path":"a.txt","start":3}`, wantNil: true},
		{name: "empty object missing required", args: `{}`, want: []string{`missing required argument "path"`}},
		{name: "null args treated as empty", args: `null`, want: []string{`missing required argument "path"`}},
		{name: "empty args", args: ``, want: []string{`missing required argument "path"`}},
		{name: "unknown argument lists allowed", args: `{"path":"a","bogus":1}`, want: []string{`unknown argument "bogus"`, "allowed: mode, path, start, tags"}},
		{name: "wrong type", args: `{"path":5}`, want: []string{`argument "path": expected string, got integer`}},
		{name: "float for integer", args: `{"path":"a","start":1.5}`, want: []string{`expected integer, got number`}},
		{name: "below minimum", args: `{"path":"a","start":0}`, want: []string{"below the minimum"}},
		{name: "bad enum", args: `{"path":"a","mode":"zip"}`, want: []string{"is not one of [text, binary]"}},
		{name: "array item type", args: `{"path":"a","tags":["x",2]}`, want: []string{`argument "tags[1]": expected string`}},
		{name: "invalid json", args: `{"path":`, want: []string{"not valid JSON"}},
		{name: "multiple problems", args: `{"bogus":1,"start":"x"}`, want: []string{`missing required argument "path"`, `unknown argument "bogus"`, `argument "start": expected integer`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateArgs([]byte(tc.args), []byte(readSchema))
			if tc.wantNil {
				if len(got) != 0 {
					t.Fatalf("expected no problems, got %v", got)
				}
				return
			}
			joined := strings.Join(got, " | ")
			for _, w := range tc.want {
				if !strings.Contains(joined, w) {
					t.Errorf("missing %q in problems: %s", w, joined)
				}
			}
		})
	}
}

func TestValidateArgsBrokenSchemaDoesNotBlock(t *testing.T) {
	if got := ValidateArgs([]byte(`{"a":1}`), []byte(`{not json`)); got != nil {
		t.Fatalf("broken schema must not produce problems, got %v", got)
	}
	if got := ValidateArgs([]byte(`{"a":1}`), nil); got != nil {
		t.Fatalf("empty schema must not produce problems, got %v", got)
	}
}

func TestCapabilities(t *testing.T) {
	c := Capabilities{Vision: true}
	if !c.Supports(MediaImage) || c.Supports(MediaAudio) || c.Supports(MediaPDF) || c.Supports(MediaVideo) {
		t.Fatal("capability matrix wrong")
	}
	if msg := c.UnsupportedMessage(MediaAudio); !strings.Contains(msg, "AUDIO=true") {
		t.Fatalf("unexpected message: %s", msg)
	}
	if k, ok := KindForMIME("application/pdf"); !ok || k != MediaPDF {
		t.Fatal("pdf mime")
	}
	if _, ok := KindForMIME("text/plain"); ok {
		t.Fatal("text is not media")
	}
}
