// Package contextmgr turns the session event log into the exact conversation
// sent to the model, and decides when and how that conversation shrinks.
//
// Everything here is a pure function of the log: given the same events the
// rendered context is identical, which is what keeps provider prompt caches
// warm. State changes (a prune, a compaction) are therefore recorded as events
// first and only then reflected in the next render.
package contextmgr

import (
	"fmt"
	"strings"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
)

// Attachment locates one inline payload in a rendered context.
type Attachment struct {
	Blob     string
	Name     string
	Path     string
	Size     int64
	Kind     llm.MediaKind
	TurnNo   int
	Position int // message index, oldest first
}

// Context is the rendered conversation.
type Context struct {
	System   string
	Messages []llm.Message
	// Turn is the number of user prompts since the session began.
	Turn int
	// Attachments lists inline payloads still present, oldest first.
	Attachments []Attachment
	// Fixes describes repairs applied while rendering (for logs and tests).
	Fixes []string
}

// Build renders the context from events and repairs it so every provider
// accepts it. It never mutates the events.
func Build(events []session.Event) Context {
	ctx := BuildRaw(events)
	ctx.Messages, ctx.Fixes = Repair(ctx.Messages)
	return ctx
}

// BuildRaw renders the context without repairs. The engine uses it on resume
// to find tool calls a crash left unanswered, then persists answers for them.
func BuildRaw(events []session.Event) Context {
	var ctx Context
	lastCompaction := -1
	removed := map[string]bool{}
	for i, ev := range events {
		switch ev.Kind {
		case session.KindSystem:
			ctx.System = ev.Text
		case session.KindCompaction:
			lastCompaction = i
		case session.KindPrune:
			for _, b := range ev.Blobs {
				removed[b] = true
			}
		case session.KindMessage:
			if ev.TurnNo > ctx.Turn {
				ctx.Turn = ev.TurnNo
			}
		}
	}

	start := 0
	if lastCompaction >= 0 {
		start = lastCompaction + 1
		ctx.Messages = append(ctx.Messages, ResumeMessage(*events[lastCompaction].Compaction))
	}
	for _, ev := range events[start:] {
		if ev.Kind != session.KindMessage || ev.Message == nil {
			continue
		}
		msg := cloneMessage(*ev.Message)
		pos := len(ctx.Messages)
		applyPrune(&msg, removed)
		collectAttachments(msg, ev.TurnNo, pos, &ctx.Attachments)
		ctx.Messages = append(ctx.Messages, msg)
	}
	return ctx
}

// WithPruned returns a copy of the context with the given blobs' payloads
// removed (metadata kept). It is used to fit a transient request, such as a
// compaction summary, under the size limit without touching the log.
func (c Context) WithPruned(blobs []string) Context {
	removed := make(map[string]bool, len(blobs))
	for _, b := range blobs {
		removed[b] = true
	}
	out := c
	out.Messages = make([]llm.Message, len(c.Messages))
	for i, m := range c.Messages {
		cp := cloneMessage(m)
		applyPrune(&cp, removed)
		out.Messages[i] = cp
	}
	out.Attachments = nil
	for _, a := range c.Attachments {
		if !removed[a.Blob] {
			out.Attachments = append(out.Attachments, a)
		}
	}
	return out
}

// PendingNotes returns queued notes not yet folded into a message.
func PendingNotes(events []session.Event) []session.Event {
	consumed := map[int64]bool{}
	for _, ev := range events {
		for _, s := range ev.Consumed {
			consumed[s] = true
		}
	}
	var out []session.Event
	for _, ev := range events {
		if ev.Kind == session.KindNote && !consumed[ev.Seq] {
			out = append(out, ev)
		}
	}
	return out
}

func cloneMessage(m llm.Message) llm.Message {
	out := m
	out.Parts = make([]llm.Part, len(m.Parts))
	for i, p := range m.Parts {
		cp := p
		if p.Media != nil {
			mm := *p.Media
			cp.Media = &mm
		}
		if p.Result != nil {
			r := *p.Result
			r.Media = append([]llm.Media(nil), p.Result.Media...)
			cp.Result = &r
		}
		out.Parts[i] = cp
	}
	return out
}

// applyPrune drops the payload reference of removed blobs but keeps the
// descriptive metadata, so adapters render an explanatory placeholder.
func applyPrune(m *llm.Message, removed map[string]bool) {
	for i := range m.Parts {
		p := &m.Parts[i]
		if p.Media != nil && removed[p.Media.Blob] {
			p.Media.Blob = ""
		}
		if p.Result != nil {
			for j := range p.Result.Media {
				if removed[p.Result.Media[j].Blob] {
					p.Result.Media[j].Blob = ""
				}
			}
		}
	}
}

func collectAttachments(m llm.Message, turn, pos int, out *[]Attachment) {
	add := func(md llm.Media) {
		if md.Blob != "" {
			*out = append(*out, Attachment{Blob: md.Blob, Name: md.Name, Path: md.Path, Size: md.Size, Kind: md.Kind, TurnNo: turn, Position: pos})
		}
	}
	for _, p := range m.Parts {
		if p.Media != nil {
			add(*p.Media)
		}
		if p.Result != nil {
			for _, md := range p.Result.Media {
				add(md)
			}
		}
	}
}

// ResumeMessage is the single user message that stands in for all history
// before a compaction.
func ResumeMessage(c session.Compaction) llm.Message {
	var sb strings.Builder
	sb.WriteString("[info: context compacted]\n")
	sb.WriteString("The earlier conversation was compacted to stay within the context limit. Attachments and tool outputs from before this point are no longer in context; ")
	sb.WriteString("re-open files with view_file if you need the originals.\n\n")
	sb.WriteString("## Summary of the earlier work\n")
	sb.WriteString(strings.TrimSpace(c.Summary))
	sb.WriteString("\n")
	if len(c.RemovedPaths) > 0 {
		sb.WriteString("\n## Attachments you can re-open\n")
		for _, p := range c.RemovedPaths {
			fmt.Fprintf(&sb, "- %s\n", p)
		}
	}
	if len(c.KeptPrompts) > 0 {
		sb.WriteString("\n## Recent user requests (verbatim)\n")
		for i, p := range c.KeptPrompts {
			fmt.Fprintf(&sb, "%d. %s\n", i+1, strings.TrimSpace(p))
		}
	}
	sb.WriteString("\nContinue the work from where it left off.")
	return llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: sb.String()}}}
}
