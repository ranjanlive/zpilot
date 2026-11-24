package render

import (
	"strings"
	"testing"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
)

func msg(role llm.Role, parts ...llm.Part) session.Event {
	m := llm.Message{Role: role, Parts: parts}
	return session.Event{Kind: session.KindMessage, Message: &m}
}

func number(evs []session.Event) []session.Event {
	for i := range evs {
		evs[i].Seq = int64(i + 1)
	}
	return evs
}

func TestTranscriptReadsLikeAConversation(t *testing.T) {
	evs := number([]session.Event{
		msg(llm.RoleUser, llm.Part{Type: llm.PartText, Text: "fix the bug"}, llm.Part{Type: llm.PartMedia, Media: &llm.Media{Kind: llm.MediaImage, Name: "err.png", Size: 2048}}),
		msg(llm.RoleAssistant, llm.Part{Type: llm.PartText, Text: "Looking."}, llm.Part{Type: llm.PartToolCall, Call: &llm.ToolCall{Name: "view_file", Args: []byte(`{"path": "a.go"}`)}}),
		msg(llm.RoleUser, llm.Part{Type: llm.PartToolResult, Result: &llm.ToolResult{Name: "view_file", Output: "line1\nline2"}}, llm.Part{Type: llm.PartInfo, Text: "[info: x]"}),
		msg(llm.RoleAssistant, llm.Part{Type: llm.PartText, Text: "Fixed."}),
		{Kind: session.KindStatus, State: "error", Detail: "boom"},
		{Kind: session.KindStatus, State: "working"},
	})
	out := strings.Join(Transcript(evs, Options{}), "\n")
	for _, want := range []string{"▌you", "fix the bug", "📎 image: err.png (2 KB)", "▌agent", `▸ view_file {"path": "a.go"}`, "◂ view_file: line1", "[info: x]", "Fixed.", "── error: boom ──"} {
		if !strings.Contains(out, want) {
			t.Errorf("transcript lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "── working") {
		t.Error("routine status must be hidden outside debug mode")
	}
	if strings.Count(out, "▌you") != 1 {
		t.Error("a tool-result message is not a user turn and must not get a header")
	}
	if dbg := strings.Join(Transcript(evs, Options{Debug: true}), "\n"); !strings.Contains(dbg, "── working ──") {
		t.Error("debug mode shows every status")
	}
}

func TestToolOutputTruncationSinceAndLastLines(t *testing.T) {
	evs := number([]session.Event{
		msg(llm.RoleUser, llm.Part{Type: llm.PartToolResult, Result: &llm.ToolResult{Name: "t", Output: strings.Repeat("x", 100), IsError: true}}),
		msg(llm.RoleAssistant, llm.Part{Type: llm.PartText, Text: "later"}),
	})
	out := strings.Join(Transcript(evs, Options{ToolOutputChars: 10}), "\n")
	if !strings.Contains(out, "◂ error t:") || !strings.Contains(out, "(+90 chars)") {
		t.Fatalf("truncation: %s", out)
	}
	if got := Transcript(evs, Options{Since: 1}); len(got) != 2 || got[1] != "later" {
		t.Fatalf("since: %v", got)
	}
	lines := []string{"a", "b", "c"}
	if got := LastLines(lines, 2); len(got) != 2 || got[0] != "b" {
		t.Fatalf("last: %v", got)
	}
	if len(LastLines(lines, 0)) != 3 || len(LastLines(lines, 9)) != 3 {
		t.Fatal("0 or too-large n returns everything")
	}
}

func TestFinalAnswerSkipsToolOnlyTurns(t *testing.T) {
	evs := []session.Event{
		msg(llm.RoleAssistant, llm.Part{Type: llm.PartText, Text: "the answer"}),
		msg(llm.RoleAssistant, llm.Part{Type: llm.PartToolCall, Call: &llm.ToolCall{Name: "x"}}),
		msg(llm.RoleUser, llm.Part{Type: llm.PartText, Text: "hi"}),
	}
	if got := FinalAnswer(evs); got != "the answer" {
		t.Fatalf("got %q", got)
	}
	if FinalAnswer(nil) != "" {
		t.Fatal("empty log")
	}
}
