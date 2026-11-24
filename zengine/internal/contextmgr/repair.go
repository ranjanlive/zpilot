package contextmgr

import (
	"fmt"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
)

// InterruptedOutput is the tool output substituted for a call that never
// produced a result (the process died or the turn was cancelled mid-call).
const InterruptedOutput = "This tool call was interrupted before it completed (the session was stopped or restarted). Its effects are unknown: check the current state before repeating it."

// Repair makes a message list valid for every provider and returns the fixes
// it applied. A well-formed history passes through unchanged.
//
// Rules: every assistant tool call is answered by a result in the next user
// message (a synthetic "interrupted" result is added when it is missing);
// results that answer no call are dropped; empty messages are removed.
func Repair(in []llm.Message) ([]llm.Message, []string) {
	var (
		out     []llm.Message
		fixes   []string
		pending []llm.ToolCall // calls from the last assistant message awaiting results
	)
	flushPending := func() {
		if len(pending) == 0 {
			return
		}
		parts := make([]llm.Part, 0, len(pending))
		for _, c := range pending {
			parts = append(parts, syntheticResult(c))
		}
		out = append(out, llm.Message{Role: llm.RoleUser, Parts: parts})
		fixes = append(fixes, fmt.Sprintf("added interrupted results for %d unanswered tool call(s)", len(pending)))
		pending = nil
	}

	for _, m := range in {
		if len(m.Parts) == 0 && m.Native == nil {
			fixes = append(fixes, "dropped an empty message")
			continue
		}
		switch m.Role {
		case llm.RoleAssistant:
			flushPending() // an assistant turn can never directly follow unanswered calls
			out = append(out, m)
			pending = m.ToolCalls()
		case llm.RoleUser:
			if len(pending) == 0 {
				kept, dropped := dropOrphanResults(m)
				if dropped > 0 {
					fixes = append(fixes, fmt.Sprintf("dropped %d orphaned tool result(s)", dropped))
				}
				if len(kept.Parts) > 0 {
					out = append(out, kept)
				}
				continue
			}
			answered := map[string]bool{}
			var parts []llm.Part
			for _, p := range m.Parts {
				if p.Type == llm.PartToolResult && p.Result != nil {
					if !callPending(pending, p.Result.CallID) || answered[p.Result.CallID] {
						fixes = append(fixes, "dropped an orphaned tool result")
						continue
					}
					answered[p.Result.CallID] = true
				}
				parts = append(parts, p)
			}
			var missing []llm.Part
			for _, c := range pending {
				if !answered[c.ID] {
					missing = append(missing, syntheticResult(c))
				}
			}
			if len(missing) > 0 {
				fixes = append(fixes, fmt.Sprintf("added interrupted results for %d unanswered tool call(s)", len(missing)))
				parts = append(missing, parts...)
			}
			pending = nil
			m.Parts = parts
			out = append(out, m)
		}
	}
	flushPending()
	return out, fixes
}

func callPending(calls []llm.ToolCall, id string) bool {
	for _, c := range calls {
		if c.ID == id {
			return true
		}
	}
	return false
}

func syntheticResult(c llm.ToolCall) llm.Part {
	return llm.Part{Type: llm.PartToolResult, Result: &llm.ToolResult{
		CallID: c.ID, CallSynthetic: c.Synthetic, Name: c.Name, Output: InterruptedOutput, IsError: true,
	}}
}

func dropOrphanResults(m llm.Message) (llm.Message, int) {
	dropped := 0
	var parts []llm.Part
	for _, p := range m.Parts {
		if p.Type == llm.PartToolResult {
			dropped++
			continue
		}
		parts = append(parts, p)
	}
	m.Parts = parts
	return m, dropped
}

// DanglingCalls returns the tool calls at the tail of the conversation that
// have no result yet. The engine persists synthetic results for them on resume
// so later renders need no repair.
func DanglingCalls(msgs []llm.Message) []llm.ToolCall {
	if len(msgs) == 0 {
		return nil
	}
	last := msgs[len(msgs)-1]
	if last.Role != llm.RoleAssistant {
		return nil
	}
	return last.ToolCalls()
}

// InterruptedResults builds the synthetic tool-result parts for calls.
func InterruptedResults(calls []llm.ToolCall) []llm.Part {
	parts := make([]llm.Part, 0, len(calls))
	for _, c := range calls {
		parts = append(parts, syntheticResult(c))
	}
	return parts
}
