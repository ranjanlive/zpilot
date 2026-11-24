// Package tui is the terminal interface. Like the web UI it is a view over an
// engine's control API: it renders the session log and live events, and sends
// prompts, notes and commands back. Closing it never stops the agent.
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
)

var (
	dimStyle   = lipgloss.NewStyle().Faint(true)
	userStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("63")).Bold(true)
	agentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	toolStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	warnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	codeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("180"))
	boldStyle  = lipgloss.NewStyle().Bold(true)
	infoStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true)
)

// block is one renderable unit of the conversation. Tool blocks are updated in
// place when their result arrives.
type block struct {
	kind   string // text | tool | note | divider | system | error | warn
	who    string // "you" / "agent" for text blocks
	text   string
	tool   *toolBlock
	nolimt bool
}

type toolBlock struct {
	name, args, output string
	done, isErr        bool
	media              []string
}

// renderBlock turns a block into styled, width-wrapped text.
func renderBlock(b block, width int) string {
	wrap := func(s string) string {
		if width <= 0 {
			return s
		}
		return lipgloss.NewStyle().Width(width).Render(s)
	}
	switch b.kind {
	case "text":
		head := userStyle.Render("▌ you")
		if b.who == "agent" {
			head = agentStyle.Render("▌ agent")
		}
		return head + "\n" + wrap(formatMarkdown(b.text))
	case "tool":
		t := b.tool
		mark := toolStyle.Render("▸")
		state := dimStyle.Render("running…")
		switch {
		case t.done && t.isErr:
			state, mark = errStyle.Render("error"), errStyle.Render("▸")
		case t.done:
			state, mark = okStyle.Render("done"), okStyle.Render("▸")
		}
		line := fmt.Sprintf("%s %s %s  %s", mark, boldStyle.Render(t.name), dimStyle.Render(oneLine(t.args, max(10, width-len(t.name)-16))), state)
		if t.done && t.output != "" {
			line += "\n" + dimStyle.Render(wrapIndent(firstLines(t.output, 4), width, "    "))
		}
		for _, m := range t.media {
			line += "\n    " + dimStyle.Render("📎 "+m)
		}
		return line
	case "note":
		return infoStyle.Render(wrap("ⓘ " + b.text))
	case "divider":
		return dimStyle.Render(wrap("── " + b.text + " ──"))
	case "error":
		return errStyle.Render(wrap("✖ " + b.text))
	case "warn":
		return warnStyle.Render(wrap("⚠ " + b.text))
	}
	return wrap(b.text)
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if n > 0 && len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n… (+%d more lines)", len(lines)-n)
}

func wrapIndent(s string, width int, indent string) string {
	w := width - len(indent)
	if w < 20 {
		w = 20
	}
	var out []string
	for _, l := range strings.Split(s, "\n") {
		out = append(out, indent+lipgloss.NewStyle().Width(w).Render(l))
	}
	return strings.Join(out, "\n")
}

// formatMarkdown applies the small subset of markdown that matters in a
// terminal: fenced code, inline code, bold and headings. Everything else is
// left as plain text.
func formatMarkdown(s string) string {
	var out []string
	inFence := false
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		switch {
		case inFence:
			out = append(out, codeStyle.Render("  "+line))
		case strings.HasPrefix(line, "#"):
			out = append(out, boldStyle.Render(strings.TrimSpace(strings.TrimLeft(line, "#"))))
		default:
			out = append(out, inlineMarkdown(line))
		}
	}
	return strings.Join(out, "\n")
}

func inlineMarkdown(line string) string {
	var sb strings.Builder
	for i := 0; i < len(line); {
		switch {
		case line[i] == '`':
			if j := strings.IndexByte(line[i+1:], '`'); j >= 0 {
				sb.WriteString(codeStyle.Render(line[i+1 : i+1+j]))
				i += j + 2
				continue
			}
		case strings.HasPrefix(line[i:], "**"):
			if j := strings.Index(line[i+2:], "**"); j >= 0 {
				sb.WriteString(boldStyle.Render(inlineMarkdown(line[i+2 : i+2+j])))
				i += j + 4
				continue
			}
		}
		sb.WriteByte(line[i])
		i++
	}
	return sb.String()
}

// blocksForMessage converts a message into blocks, registering tool calls so
// later results can update them.
func (m *Model) blocksForMessage(msg llm.Message) []block {
	var out []block
	if msg.Role == llm.RoleUser {
		var texts []string
		var notes []string
		for _, p := range msg.Parts {
			switch p.Type {
			case llm.PartText:
				texts = append(texts, p.Text)
			case llm.PartMedia:
				if p.Media != nil {
					texts = append(texts, fmt.Sprintf("📎 %s: %s (%s)", p.Media.Kind, nameOf(*p.Media), llm.HumanBytes(p.Media.Size)))
				}
			case llm.PartInfo:
				notes = append(notes, trimInfo(p.Text))
			case llm.PartToolResult:
				m.applyResult(p.Result)
			}
		}
		if len(texts) > 0 && !strings.HasPrefix(texts[0], "[info: context compacted]") {
			out = append(out, block{kind: "text", who: "you", text: strings.Join(texts, "\n")})
		}
		for _, n := range notes {
			out = append(out, block{kind: "note", text: n})
		}
		return out
	}
	var text []string
	for _, p := range msg.Parts {
		switch p.Type {
		case llm.PartText:
			text = append(text, p.Text)
		case llm.PartToolCall:
			if p.Call != nil {
				tb := &toolBlock{name: p.Call.Name, args: string(p.Call.Args)}
				m.tools[p.Call.ID] = tb
			}
		}
	}
	if t := strings.TrimSpace(strings.Join(text, "\n")); t != "" {
		out = append(out, block{kind: "text", who: "agent", text: t})
	}
	for _, p := range msg.Parts {
		if p.Type == llm.PartToolCall && p.Call != nil {
			out = append(out, block{kind: "tool", tool: m.tools[p.Call.ID]})
		}
	}
	return out
}

func (m *Model) applyResult(r *llm.ToolResult) {
	if r == nil {
		return
	}
	t := m.tools[r.CallID]
	if t == nil {
		m.blocks = append(m.blocks, block{kind: "note", text: r.Name + ": " + oneLine(r.Output, 200)})
		return
	}
	t.done, t.isErr, t.output = true, r.IsError, r.Output
	for _, md := range r.Media {
		t.media = append(t.media, fmt.Sprintf("%s %s", md.Kind, nameOf(md)))
	}
}

func nameOf(m llm.Media) string {
	switch {
	case m.Name != "":
		return m.Name
	case m.Path != "":
		return m.Path
	}
	return m.MIME
}

func trimInfo(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[info:")
	s = strings.TrimSuffix(s, "]")
	return strings.TrimSpace(s)
}

// blocksForEvent converts a durable event into blocks.
func (m *Model) blocksForEvent(ev session.Event) []block {
	switch ev.Kind {
	case session.KindMessage:
		if ev.Message != nil {
			return m.blocksForMessage(*ev.Message)
		}
	case session.KindNote:
		return nil // rendered once, when delivered inside the next message
	case session.KindCompaction:
		if c := ev.Compaction; c != nil {
			return []block{{kind: "divider", text: "context compacted: " + c.Reason}}
		}
	case session.KindPrune:
		return []block{{kind: "divider", text: fmt.Sprintf("%d attachment(s) removed from context", len(ev.Blobs))}}
	case session.KindStatus:
		switch ev.State {
		case "error":
			return []block{{kind: "error", text: ev.Detail}}
		case "warning", "paused", "stopped":
			return []block{{kind: "warn", text: ev.State + ": " + ev.Detail}}
		}
	}
	return nil
}
