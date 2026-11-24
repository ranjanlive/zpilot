// Package render turns session events into readable text. Every interface
// (headless --output, `zagent show`, the CLI and the web view) shares it so a
// session reads the same everywhere.
package render

import (
	"fmt"
	"strings"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
)

// Options tune the transcript.
type Options struct {
	// ToolOutputChars truncates each tool result (0 = unlimited).
	ToolOutputChars int
	// Thoughts includes model reasoning text.
	Thoughts bool
	// Since renders only events with Seq greater than this.
	Since int64
	// Debug adds usage, status and system events.
	Debug bool
}

// Transcript renders events as lines of text.
func Transcript(events []session.Event, o Options) []string {
	var out []string
	add := func(format string, a ...any) {
		out = append(out, strings.Split(fmt.Sprintf(format, a...), "\n")...)
	}
	for _, ev := range events {
		if ev.Seq <= o.Since {
			continue
		}
		switch ev.Kind {
		case session.KindMessage:
			if ev.Message != nil {
				out = append(out, Message(*ev.Message, o)...)
			}
		case session.KindNote:
			// Shown when delivered (as an [info:] line inside the next message), not also when queued.
		case session.KindCompaction:
			if c := ev.Compaction; c != nil {
				add("── context compacted (%s) ──", c.Reason)
				for _, l := range strings.Split(strings.TrimSpace(c.Summary), "\n") {
					add("   %s", l)
				}
			}
		case session.KindPrune:
			add("── %d attachment(s) removed from context: %s ──", len(ev.Blobs), ev.Detail)
		case session.KindStatus:
			if o.Debug || ev.State == "error" || ev.State == "paused" || ev.State == "stopped" || ev.State == "warning" {
				add("── %s%s ──", ev.State, detail(ev.Detail))
			}
		case session.KindUsage:
			if o.Debug && ev.Usage != nil {
				add("   tokens: prompt %d, answer %d, thoughts %d%s", ev.Usage.PromptTokens, ev.Usage.CompletionTokens, ev.Usage.ThoughtTokens, estimated(ev.Usage))
			}
		case session.KindSystem:
			if o.Debug {
				add("── system prompt snapshot (%d chars) ──", len(ev.Text))
			}
		}
	}
	return out
}

func detail(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}

func estimated(u *llm.Usage) string {
	if u.Estimated {
		return " (estimated)"
	}
	return ""
}

// Message renders one message.
func Message(m llm.Message, o Options) []string {
	var out []string
	add := func(format string, a ...any) {
		out = append(out, strings.Split(fmt.Sprintf(format, a...), "\n")...)
	}
	header := "you"
	if m.Role == llm.RoleAssistant {
		header = "agent"
	}
	isToolOnly := m.Role == llm.RoleUser
	for _, p := range m.Parts {
		if p.Type != llm.PartToolResult && p.Type != llm.PartInfo {
			isToolOnly = false
		}
	}
	if !isToolOnly {
		add("▌%s", header)
	}
	for _, p := range m.Parts {
		switch p.Type {
		case llm.PartText:
			add("%s", p.Text)
		case llm.PartInfo:
			add("  %s", p.Text)
		case llm.PartMedia:
			if p.Media != nil {
				add("  📎 %s: %s (%s)", p.Media.Kind, mediaName(*p.Media), llm.HumanBytes(p.Media.Size))
			}
		case llm.PartThought:
			if o.Thoughts && p.Thought != nil && p.Thought.Text != "" {
				add("  💭 %s", oneLine(p.Thought.Text, 400))
			}
		case llm.PartToolCall:
			if p.Call != nil {
				add("  ▸ %s %s", p.Call.Name, oneLine(string(p.Call.Args), 200))
			}
		case llm.PartToolResult:
			if r := p.Result; r != nil {
				tag := "◂"
				if r.IsError {
					tag = "◂ error"
				}
				body := r.Output
				if o.ToolOutputChars > 0 && len(body) > o.ToolOutputChars {
					body = body[:o.ToolOutputChars] + fmt.Sprintf("… (+%d chars)", len(r.Output)-o.ToolOutputChars)
				}
				add("  %s %s: %s", tag, r.Name, indent(body))
				for _, md := range r.Media {
					add("    📎 %s: %s", md.Kind, mediaName(md))
				}
			}
		}
	}
	return out
}

func mediaName(m llm.Media) string {
	switch {
	case m.Name != "":
		return m.Name
	case m.Path != "":
		return m.Path
	}
	return m.MIME
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

func indent(s string) string {
	s = strings.TrimRight(s, "\n")
	return strings.ReplaceAll(s, "\n", "\n      ")
}

// LastLines returns the final n lines (all of them when n <= 0).
func LastLines(lines []string, n int) []string {
	if n <= 0 || len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}

// FinalAnswer returns the text of the last assistant message that has text.
func FinalAnswer(events []session.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Kind == session.KindMessage && ev.Message != nil && ev.Message.Role == llm.RoleAssistant {
			if t := strings.TrimSpace(ev.Message.Text()); t != "" {
				return t
			}
		}
	}
	return ""
}
