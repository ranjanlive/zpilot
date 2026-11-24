package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/config"
	"github.com/ranjanlive/zpilot/zengine/internal/contextmgr"
	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/provider"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
)

// step scripts one model call. It sees the request that was rendered, so tests
// can assert on exactly what the engine would have sent.
type step func(ctx context.Context, req provider.Request, built *provider.Built) (*provider.Response, error)

// fakeModel renders requests with the real Gemini adapter (so sizes and
// encodings are genuine) and answers from a script instead of the network.
type fakeModel struct {
	mu      sync.Mutex
	adapter provider.Adapter
	script  []step
	calls   int
	reqs    []provider.Request
	bodies  []string
	cur     provider.Request
}

func newFakeModel(t *testing.T, steps ...step) *fakeModel {
	t.Helper()
	c, err := provider.New(provider.Settings{Provider: "gemini", BaseURL: "https://fake.example", Model: "m", GeminiToolMedia: "inline"})
	if err != nil {
		t.Fatal(err)
	}
	return &fakeModel{adapter: c.Adapter, script: steps}
}

func (f *fakeModel) Format() string { return f.adapter.Format() }

func (f *fakeModel) Build(req provider.Request) (*provider.Built, error) {
	f.mu.Lock()
	f.cur = req
	f.mu.Unlock()
	return f.adapter.Build(req)
}

func (f *fakeModel) Send(ctx context.Context, b *provider.Built, sink func(provider.Delta)) (*provider.Response, error) {
	f.mu.Lock()
	idx := f.calls
	f.calls++
	req := f.cur
	f.reqs = append(f.reqs, req)
	f.bodies = append(f.bodies, string(b.Body))
	var s step
	if idx < len(f.script) {
		s = f.script[idx]
	}
	f.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("fake model: no scripted step for call #%d", idx+1)
	}
	resp, err := s(ctx, req, b)
	if err == nil && sink != nil && resp != nil {
		if t := resp.Message.Text(); t != "" {
			sink(provider.Delta{Kind: provider.DeltaText, Text: t})
		}
	}
	return resp, err
}

func (f *fakeModel) Calls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func (f *fakeModel) Request(i int) provider.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[i]
}

func (f *fakeModel) Body(i int) string { f.mu.Lock(); defer f.mu.Unlock(); return f.bodies[i] }

// ---- script helpers ----

func usage(prompt, completion int) *llm.Usage {
	return &llm.Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion}
}

func say(text string) step { return sayU(text, usage(10, 5)) }

func sayU(text string, u *llm.Usage) step {
	return func(context.Context, provider.Request, *provider.Built) (*provider.Response, error) {
		m := llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: text}}, FinishReason: llm.FinishStop}
		return &provider.Response{Message: m, Usage: u}, nil
	}
}

func callTool(id, name string, args any) step { return callToolU(id, name, args, usage(10, 5)) }

func callToolU(id, name string, args any, u *llm.Usage) step {
	return func(context.Context, provider.Request, *provider.Built) (*provider.Response, error) {
		var raw json.RawMessage
		switch a := args.(type) {
		case string:
			raw = json.RawMessage(a)
		default:
			raw, _ = json.Marshal(a)
		}
		m := llm.Message{Role: llm.RoleAssistant, FinishReason: llm.FinishToolCalls,
			Parts: []llm.Part{{Type: llm.PartToolCall, Call: &llm.ToolCall{ID: id, Name: name, Args: raw}}}}
		return &provider.Response{Message: m, Usage: u}, nil
	}
}

func fail(kind provider.ErrKind, msg string) step {
	return func(context.Context, provider.Request, *provider.Built) (*provider.Response, error) {
		return nil, &provider.Error{Kind: kind, Message: msg}
	}
}

func empty() step {
	return func(context.Context, provider.Request, *provider.Built) (*provider.Response, error) {
		return &provider.Response{Message: llm.Message{Role: llm.RoleAssistant, FinishReason: llm.FinishStop}, Usage: usage(10, 0)}, nil
	}
}

func blockUntilCancelled(started chan<- struct{}) step {
	return func(ctx context.Context, _ provider.Request, _ *provider.Built) (*provider.Response, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, &provider.Error{Kind: provider.ErrCanceled, Message: "cancelled", Cause: ctx.Err()}
	}
}

// ---- engine fixture ----

type fixture struct {
	t      *testing.T
	home   config.Home
	ws     string
	cfg    config.Config
	env    config.Env
	model  *fakeModel
	eng    *Engine
	mu     sync.Mutex
	sleeps []time.Duration
}

func newFixture(t *testing.T, steps ...step) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{t: t, home: config.Home{Root: filepath.Join(root, "home")}, ws: filepath.Join(root, "ws")}
	if _, err := f.home.Init(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.ws, 0o755); err != nil {
		t.Fatal(err)
	}
	f.cfg = config.Defaults()
	f.cfg.Permissions.RunCommand = "allow"
	f.cfg.Network.RetryInitialMS, f.cfg.Network.RetryMaxMS = 10, 80
	f.env = config.Env{Provider: "gemini", BaseURL: "https://fake.example", Model: "m", APIKey: "k", Caps: llm.Capabilities{Vision: true}}
	f.model = newFakeModel(t, steps...)
	return f
}

// start creates and starts the engine; call after adjusting cfg/env.
func (f *fixture) start(session string) *Engine {
	f.t.Helper()
	eng, err := New(Options{
		Home: f.home, Env: f.env, Cfg: f.cfg, Workspace: f.ws, SessionID: session, Model: f.model,
		Sleep: func(ctx context.Context, d time.Duration) error {
			f.mu.Lock()
			f.sleeps = append(f.sleeps, d)
			f.mu.Unlock()
			return ctx.Err()
		},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	eng.Start()
	f.eng = eng
	f.t.Cleanup(func() { eng.Close() })
	return eng
}

func (f *fixture) run(p Prompt) error {
	f.t.Helper()
	if err := f.eng.Submit(p); err != nil {
		return err
	}
	return f.wait()
}

func (f *fixture) wait() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := f.eng.WaitIdle(ctx)
	if err != nil {
		st, detail := f.eng.State()
		f.eng.mu.Lock()
		queued := f.eng.queued
		f.eng.mu.Unlock()
		f.t.Logf("WaitIdle failed: state=%s detail=%q queued=%d running_tasks=%d pending_notes=%d", st, detail, queued, f.eng.RunningTasks(), len(contextmgr.PendingNotes(f.events())))
		evs := f.events()
		for _, e := range evs[max(0, len(evs)-6):] {
			f.t.Logf("  event #%d %s state=%q detail=%q", e.Seq, e.Kind, e.State, e.Detail)
		}
	}
	return err
}

func (f *fixture) events() []session.Event { return f.eng.Store().Events() }

func (f *fixture) kinds(k session.Kind) []session.Event {
	var out []session.Event
	for _, e := range f.events() {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

func (f *fixture) lastStatus() session.Event {
	s := f.kinds(session.KindStatus)
	if len(s) == 0 {
		return session.Event{}
	}
	return s[len(s)-1]
}

func (f *fixture) messages() []llm.Message {
	var out []llm.Message
	for _, e := range f.kinds(session.KindMessage) {
		out = append(out, *e.Message)
	}
	return out
}

func (f *fixture) writeFile(rel, content string) string {
	p := filepath.Join(f.ws, rel)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// resultText finds the output of the tool result for callID in the messages.
func resultText(msgs []llm.Message, callID string) string {
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == llm.PartToolResult && p.Result.CallID == callID {
				return p.Result.Output
			}
		}
	}
	return ""
}

func contains(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%s: expected %q in:\n%s", what, needle, haystack)
	}
}
