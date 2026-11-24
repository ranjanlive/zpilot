// Package tools defines the tool contract, the registry that validates and
// runs calls, and the built-in tools. Every failure a model can cause (unknown
// tool, bad arguments, unsupported media, a panic in a tool) is converted into
// a tool result the model can read and act on; the engine itself never
// aborts because a model called something wrongly.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/config"
	"github.com/ranjanlive/zpilot/zengine/internal/llm"
)

// Result is what a tool returns to the model.
type Result struct {
	Output  string
	IsError bool
	Media   []llm.Media
}

// OK builds a success result.
func OK(format string, a ...any) Result { return Result{Output: fmt.Sprintf(format, a...)} }

// Fail builds an error result. The text is shown to the model, so make it say
// what was wrong and how to fix the call.
func Fail(format string, a ...any) Result {
	return Result{Output: fmt.Sprintf(format, a...), IsError: true}
}

// ApprovalRequest asks a human to allow a risky action.
type ApprovalRequest struct {
	Tool   string
	Action string // short verb phrase, e.g. "run command"
	Detail string // the command line or path
}

// Env is the runtime context handed to every tool call.
type Env struct {
	Workspace  string
	Home       config.Home
	SessionID  string
	SessionDir string
	Caps       llm.Capabilities
	Cfg        config.Config

	// PutBlob stores an attachment payload in the session and returns its ID.
	PutBlob func(data []byte) (string, error)
	// Approve asks the user for permission; nil means no human is available.
	Approve func(ctx context.Context, req ApprovalRequest) (allowed bool, reason string)
	// Notify queues an "[info: ...]" note for the next request.
	Notify func(text string)
	// NotifyWake queues a note and wakes an idle agent to react to it.
	NotifyWake func(text string)

	// Extra carries per-feature state (process manager, subagent runner, ...)
	// that tools in other files need without this package depending on them.
	Extra map[string]any
}

// Tool is one capability offered to the model.
type Tool interface {
	Spec() llm.ToolSpec
	Run(ctx context.Context, env *Env, args json.RawMessage) Result
}

// TimeoutTool lets a tool override the default execution timeout. Zero means
// "use the default"; a negative value disables the timeout (long-running
// background tools).
type TimeoutTool interface {
	Timeout() time.Duration
}

// Registry holds the tools available to a session.
type Registry struct {
	tools    map[string]Tool
	disabled map[string]bool
	enabled  map[string]bool // non-empty: allow-list
}

// NewRegistry creates an empty registry honouring the config's enable/disable lists.
func NewRegistry(cfg config.ToolsConfig) *Registry {
	r := &Registry{tools: map[string]Tool{}, disabled: map[string]bool{}, enabled: map[string]bool{}}
	for _, n := range cfg.Disabled {
		r.disabled[strings.TrimSpace(n)] = true
	}
	for _, n := range cfg.Enabled {
		r.enabled[strings.TrimSpace(n)] = true
	}
	return r
}

// Register adds or replaces a tool.
func (r *Registry) Register(t Tool) { r.tools[t.Spec().Name] = t }

func (r *Registry) active(name string) bool {
	if r.disabled[name] {
		return false
	}
	return len(r.enabled) == 0 || r.enabled[name]
}

// Subset returns a new registry holding only the named, currently active
// tools. The child shares the tool implementations, so it keeps their state.
func (r *Registry) Subset(names ...string) *Registry {
	sub := &Registry{tools: map[string]Tool{}, disabled: map[string]bool{}, enabled: map[string]bool{}}
	for _, n := range names {
		if t, ok := r.tools[n]; ok && r.active(n) {
			sub.tools[n] = t
		}
	}
	return sub
}

// Names returns the active tool names, sorted.
func (r *Registry) Names() []string {
	var out []string
	for n := range r.tools {
		if r.active(n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Specs returns the specs of active tools in a stable (sorted) order. A stable
// order matters: tool declarations are part of the cached request prefix.
func (r *Registry) Specs() []llm.ToolSpec {
	names := r.Names()
	out := make([]llm.ToolSpec, 0, len(names))
	for _, n := range names {
		out = append(out, r.tools[n].Spec())
	}
	return out
}

// Has reports whether an active tool exists.
func (r *Registry) Has(name string) bool {
	_, ok := r.tools[name]
	return ok && r.active(name)
}

// Invoke validates and runs one call. It always returns a Result.
func (r *Registry) Invoke(ctx context.Context, env *Env, call llm.ToolCall) (res Result) {
	t, ok := r.tools[call.Name]
	switch {
	case !ok:
		return Fail("unknown tool %q. Available tools: %s", call.Name, strings.Join(r.Names(), ", "))
	case !r.active(call.Name):
		return Fail("tool %q is disabled by configuration. Available tools: %s", call.Name, strings.Join(r.Names(), ", "))
	}
	spec := t.Spec()
	if call.ArgsError != "" {
		return Fail("invalid arguments for %s: they were not valid JSON, most likely because your response was cut off. You sent: %s\nCall the tool again with complete, valid JSON arguments (for large content, write it in several smaller steps).", call.Name, truncateMiddle(call.ArgsError, 300))
	}
	if problems := llm.ValidateArgs(call.Args, spec.Schema); len(problems) > 0 {
		return Fail("invalid arguments for %s:\n- %s\nExpected schema: %s", call.Name, strings.Join(problems, "\n- "), compactJSON(spec.Schema))
	}

	timeout := time.Duration(env.Cfg.Tools.DefaultTimeoutSeconds) * time.Second
	if tt, ok := t.(TimeoutTool); ok {
		switch d := tt.Timeout(); {
		case d > 0:
			timeout = d
		case d < 0:
			timeout = 0
		}
	}
	runCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	done := make(chan Result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- Fail("tool %s crashed: %v", call.Name, p)
			}
		}()
		done <- t.Run(runCtx, env, call.Args)
	}()
	select {
	case res = <-done:
	case <-runCtx.Done():
		if ctx.Err() != nil {
			return Fail("tool %s was cancelled", call.Name)
		}
		return Fail("tool %s timed out after %s. If it legitimately needs longer, run it in the background or raise tools.default_timeout_seconds.", call.Name, timeout)
	}
	res.Output = truncateMiddle(res.Output, env.Cfg.Tools.MaxOutputBytes)
	return res
}

func compactJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// truncateMiddle keeps the head and tail of long output (errors and results
// tend to sit at the end) and says how much was cut.
func truncateMiddle(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	head, tail := limit*2/3, limit-limit*2/3
	cut := len(s) - head - tail
	return s[:alignDown(s, head)] + fmt.Sprintf("\n… [%d bytes omitted; narrow the request to see them] …\n", cut) + s[alignUp(s, len(s)-tail):]
}

// alignDown / alignUp move a cut point to a UTF-8 boundary.
func alignDown(s string, i int) int {
	for i > 0 && i < len(s) && s[i]&0xC0 == 0x80 {
		i--
	}
	return i
}

func alignUp(s string, i int) int {
	for i < len(s) && s[i]&0xC0 == 0x80 {
		i++
	}
	return i
}

// schema is a tiny helper for declaring tool schemas readably.
func schema(props map[string]any, required ...string) json.RawMessage {
	m := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		m["required"] = required
	}
	b, _ := json.Marshal(m)
	return b
}

func str(desc string) map[string]any  { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any  { return map[string]any{"type": "integer", "description": desc} }
func flag(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
func enum(desc string, vals ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": vals}
}
