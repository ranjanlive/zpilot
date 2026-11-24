package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/render"
	"github.com/ranjanlive/zpilot/zengine/internal/tools"
)

// readOnlyTools is what a sub-agent may use unless the caller asks for more.
var readOnlyTools = []string{"view_file", "list_dir", "find_files", "search_text"}

const subagentInstruction = `# Sub-agent role
You are a sub-agent working for another agent. You have a fresh context: you cannot see the conversation that led to this task, so rely only on the task description and the workspace.
Do the task, then reply with ONE final report that contains everything the requesting agent needs (findings, file paths, exact values, answers). It will see nothing but that report, so make it complete and compact. Do not ask questions; make reasonable assumptions and state them.`

// subagentTool delegates a task to a fresh agent. The child has its own
// session and context window, so bulky inputs (many images, long files) never
// enter the parent's context: only the child's final report comes back.
type subagentTool struct {
	parent *Engine
	n      atomic.Int64
}

func (s *subagentTool) Timeout() time.Duration { return -1 }

func (s *subagentTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "subagent",
		Description: "Delegate a self-contained task to a sub-agent that works in its own fresh context and returns only its final report. " +
			"Use it for work that would flood your context: reading many files, analysing several images, PDFs or recordings, broad searches. " +
			"The sub-agent shares your workspace but NOT your conversation, so describe the task completely and say exactly what the report must contain. " +
			"By default it can only read (view_file, list_dir, find_files, search_text); name more tools in `tools` if it must change things.",
		Schema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["task"],"properties":{` +
			`"task":{"type":"string","description":"Complete instructions for the sub-agent."},` +
			`"tools":{"type":"array","items":{"type":"string"},"description":"Tools the sub-agent may use (default: view_file, list_dir, find_files, search_text)."},` +
			`"max_rounds":{"type":"integer","minimum":1,"maximum":200,"description":"Cap on its tool-call rounds (default 40)."}}}`),
	}
}

func (s *subagentTool) Run(ctx context.Context, env *tools.Env, raw json.RawMessage) tools.Result {
	var a struct {
		Task      string   `json:"task"`
		Tools     []string `json:"tools"`
		MaxRounds int      `json:"max_rounds"`
	}
	_ = json.Unmarshal(raw, &a)
	if strings.TrimSpace(a.Task) == "" {
		return tools.Fail("task is empty: describe what the sub-agent should do.")
	}
	allowed := a.Tools
	if len(allowed) == 0 {
		allowed = readOnlyTools
	}
	var unknown []string
	var final []string
	for _, n := range allowed {
		switch {
		case n == "subagent":
			return tools.Fail("a sub-agent cannot start further sub-agents.")
		case !s.parent.registry.Has(n):
			unknown = append(unknown, n)
		default:
			final = append(final, n)
		}
	}
	if len(unknown) > 0 {
		return tools.Fail("unknown or disabled tool(s) for the sub-agent: %s. Available: %s", strings.Join(unknown, ", "), strings.Join(withoutName(s.parent.registry.Names(), "subagent"), ", "))
	}

	p := s.parent
	cfg := p.cfg
	cfg.Agent.MaxToolCallTurn = 40
	if a.MaxRounds > 0 {
		cfg.Agent.MaxToolCallTurn = a.MaxRounds
	}
	root := filepath.Join(p.store.Dir, "subagents")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return tools.Fail("cannot create child session directory: %v", err)
	}
	var id string
	for {
		id = fmt.Sprintf("sub-%d", s.n.Add(1))
		// Reserve the directory exclusively; an old conversation is never reopened.
		if err := os.Mkdir(filepath.Join(root, id), 0o755); err == nil {
			break
		} else if !os.IsExist(err) {
			return tools.Fail("cannot allocate child session: %v", err)
		}
	}
	child, err := New(Options{
		Home: p.home, Env: p.env, Cfg: cfg, Workspace: p.workspace, SessionID: id, Role: "subagent",
		SessionRoot: root, Depth: 1, SystemSuffix: subagentInstruction,
		Model: p.model, Registry: p.registry.Subset(final...), Approve: p.opts.Approve, Sleep: p.opts.Sleep,
	})
	if err != nil {
		return tools.Fail("could not start the sub-agent: %v", err)
	}
	defer child.Close()
	child.Start()
	p.live.publish(Live{Kind: LiveToolRun, Name: "subagent", Detail: "started " + id})

	// A stop of the parent turn must stop the child too.
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			child.Stop()
		case <-stopped:
		}
	}()
	defer close(stopped)

	if err := child.Submit(Prompt{Text: a.Task}); err != nil {
		return tools.Fail("could not give the task to the sub-agent: %v", err)
	}
	if err := child.WaitIdle(context.Background()); err != nil {
		return tools.Fail("sub-agent failed: %v", err)
	}
	events := child.Store().Events()
	c := child.Counters()
	p.mu.Lock()
	p.counters.Prompt += c.Prompt
	p.counters.Completion += c.Completion
	p.counters.Thought += c.Thought
	p.counters.Cached += c.Cached
	p.counters.Requests += c.Requests
	if c.Estimated {
		p.counters.Estimated = true
	}
	p.mu.Unlock()

	status := lastStatus(events)
	report := render.FinalAnswer(events)
	usage := fmt.Sprintf("[%s: %d request(s), %d prompt / %d answer tokens]", id, c.Requests, c.Prompt, c.Completion)
	switch {
	case ctx.Err() != nil:
		return tools.Fail("the sub-agent was interrupted. %s", usage)
	case status == "error":
		return tools.Fail("the sub-agent stopped with an error: %s\nPartial report: %s\n%s", lastStatusDetail(events), orNone(report), usage)
	case report == "":
		return tools.Fail("the sub-agent finished without writing a report. %s", usage)
	}
	return tools.OK("%s\n\n%s", report, usage)
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func withoutName(names []string, drop string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != drop {
			out = append(out, n)
		}
	}
	return out
}
