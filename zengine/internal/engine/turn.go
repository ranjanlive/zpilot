package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ranjanlive/zpilot/zengine/internal/contextmgr"
	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/provider"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
	"github.com/ranjanlive/zpilot/zengine/internal/tools"
)

// takeNotes folds queued notes into message parts. Notes always go at the end
// of the user turn: appending keeps the cached request prefix untouched.
func (e *Engine) takeNotes() ([]llm.Part, []int64) {
	pending := e.store.PendingNotes()
	if len(pending) == 0 {
		return nil, nil
	}
	parts := make([]llm.Part, 0, len(pending))
	seqs := make([]int64, 0, len(pending))
	for _, n := range pending {
		parts = append(parts, llm.Part{Type: llm.PartInfo, Text: n.Text})
		seqs = append(seqs, n.Seq)
	}
	return parts, seqs
}

func (e *Engine) appendMessage(m llm.Message, turn int, consumed []int64) error {
	_, err := e.store.Append(session.Event{Kind: session.KindMessage, Message: &m, TurnNo: turn, Consumed: consumed})
	return err
}

// runTurn executes one prompt: user message, then model/tool rounds until the
// model answers without calling tools.
func (e *Engine) runTurn(ctx context.Context, p Prompt) error {
	view := contextmgr.Build(e.store.ContextEvents())
	turn := view.Turn

	var parts []llm.Part
	if !p.Auto {
		turn++
		if strings.TrimSpace(p.Text) != "" {
			parts = append(parts, llm.Part{Type: llm.PartText, Text: p.Text})
		}
		for _, path := range p.Attachments {
			m, err := tools.AttachFile(e.toolEnv, path)
			if err != nil {
				return err
			}
			mm := m
			parts = append(parts, llm.Part{Type: llm.PartMedia, Media: &mm})
		}
		if len(parts) == 0 {
			return errors.New("the prompt is empty")
		}
	}
	noteParts, consumed := e.takeNotes()
	parts = append(parts, noteParts...)
	if len(parts) == 0 {
		return nil
	}
	if !p.Auto {
		if _, err := e.store.Append(session.Event{Kind: session.KindTurn, State: "start", TurnNo: turn}); err != nil {
			return err
		}
		e.autoTitle(p.Text)
	}
	if err := e.appendMessage(llm.Message{Role: llm.RoleUser, Parts: parts}, turn, consumed); err != nil {
		return err
	}

	cutOffs := 0
	for round := 0; round < e.cfg.Agent.MaxToolCallTurn; round++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		resp, err := e.callModel(ctx, turn)
		if err != nil {
			return err
		}
		msg := resp.Message
		if err := e.appendMessage(msg, turn, nil); err != nil {
			return err
		}
		calls := msg.ToolCalls()
		if len(calls) == 0 {
			switch msg.FinishReason {
			case llm.FinishFiltered:
				return errors.New("the provider's safety filter blocked the response")
			case llm.FinishLength:
				cutOffs++
				if cutOffs > 3 {
					return errors.New("the model keeps hitting its output limit; raise agent.max_output_tokens or ask for a shorter answer")
				}
				// Ask the model to continue instead of silently truncating its answer.
				cont := llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartInfo, Text: "[info: your previous response was cut off by the output token limit. Continue exactly where it stopped.]"}}}
				if err := e.appendMessage(cont, turn, nil); err != nil {
					return err
				}
				continue
			}
			_, _ = e.store.Append(session.Event{Kind: session.KindTurn, State: "end", TurnNo: turn})
			return nil
		}

		results := e.runTools(ctx, calls, msg.FinishReason == llm.FinishLength)
		notes, consumed := e.takeNotes()
		if err := e.appendMessage(llm.Message{Role: llm.RoleUser, Parts: append(results, notes...)}, turn, consumed); err != nil {
			return err
		}
	}
	_, _ = e.store.Append(session.Event{Kind: session.KindTurn, State: "end", TurnNo: turn, Detail: "tool-call limit"})
	return fmt.Errorf("stopped after %d tool-call rounds (agent.max_tool_call_turn). Send another prompt to continue", e.cfg.Agent.MaxToolCallTurn)
}

func (e *Engine) autoTitle(text string) {
	if e.store.Meta().Title != "" {
		return
	}
	t := strings.Join(strings.Fields(text), " ")
	if len(t) > 80 {
		t = t[:80] + "…"
	}
	_ = e.store.UpdateMeta(func(m *session.Meta) { m.Title = t })
}

// runTools executes the model's calls one after another (a queue, not
// parallel: predictable output and one result message). Every call gets a
// result, even when the turn is interrupted, so the log stays valid.
func (e *Engine) runTools(ctx context.Context, calls []llm.ToolCall, cutOff bool) []llm.Part {
	parts := make([]llm.Part, 0, len(calls))
	for _, c := range calls {
		var res tools.Result
		switch {
		case ctx.Err() != nil:
			res = tools.Fail("not run: the user interrupted the agent before this call started")
		default:
			e.live.publish(Live{Kind: LiveToolRun, Name: c.Name, Detail: briefArgs(c.Args)})
			res = e.registry.Invoke(ctx, e.toolEnv, c)
			if res.IsError && cutOff && !json.Valid(c.Args) {
				res.Output += "\n(Your response was cut off by the output token limit, so this call's arguments are incomplete. Retry with less content, for example write a large file in several parts.)"
			}
			e.live.publish(Live{Kind: LiveToolDone, Name: c.Name, Detail: map[bool]string{true: "error", false: "ok"}[res.IsError]})
		}
		parts = append(parts, llm.Part{Type: llm.PartToolResult, Result: &llm.ToolResult{
			CallID: c.ID, CallSynthetic: c.Synthetic, Name: c.Name, Output: res.Output, IsError: res.IsError, Media: res.Media,
		}})
	}
	return parts
}

func briefArgs(args []byte) string {
	s := strings.Join(strings.Fields(string(args)), " ")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// recordUsage updates counters, the log and live displays after a response.
func (e *Engine) recordUsage(resp *provider.Response, size int, view contextmgr.Context, req provider.Request) {
	u := resp.Usage
	if u == nil || u.TotalTokens == 0 {
		u = contextmgr.EstimateUsage(req.System, req.Messages, req.Tools, resp.Message)
	}
	resp.Message.Usage = u
	e.mu.Lock()
	e.counters.add(u)
	e.lastUsage = u
	c := e.counters
	e.mu.Unlock()
	_, _ = e.store.Append(session.Event{Kind: session.KindUsage, Usage: u, RequestSize: size, TurnNo: view.Turn})
	e.live.publish(Live{Kind: LiveUsage, Tokens: c})
}
