package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/contextmgr"
	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/provider"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
)

func TestSimpleTurn(t *testing.T) {
	f := newFixture(t, say("Hello there"))
	f.start("s1")
	if err := f.run(Prompt{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	msgs := f.messages()
	if len(msgs) != 2 || msgs[0].Role != llm.RoleUser || msgs[1].Text() != "Hello there" {
		t.Fatalf("messages: %+v", msgs)
	}
	if st := f.lastStatus(); st.State != "done" {
		t.Fatalf("status: %+v", st)
	}
	c := f.eng.Counters()
	if c.Requests != 1 || c.Prompt != 10 || c.Completion != 5 || c.Context != 10 {
		t.Fatalf("counters: %+v", c)
	}
	if len(f.kinds(session.KindUsage)) != 1 {
		t.Fatal("usage event missing")
	}
	if got := f.model.Request(0).System; !strings.Contains(got, "You are zagent") {
		t.Fatalf("system prompt not sent: %.80s", got)
	}
	if meta := f.eng.Store().Meta(); meta.Title != "hi" {
		t.Fatalf("auto title: %q", meta.Title)
	}
}

func TestToolRoundTrip(t *testing.T) {
	f := newFixture(t,
		callTool("c1", "write_file", map[string]any{"path": "out/a.txt", "content": "hello"}),
		callTool("c2", "view_file", map[string]any{"path": "out/a.txt"}),
		say("done"),
	)
	f.start("s1")
	if err := f.run(Prompt{Text: "make a file"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(f.ws, "out", "a.txt"))
	if err != nil || string(b) != "hello" {
		t.Fatalf("file: %q %v", b, err)
	}
	msgs := f.messages()
	if len(msgs) != 6 { // user, call, result, call, result, final
		t.Fatalf("expected 6 messages, got %d", len(msgs))
	}
	contains(t, resultText(msgs, "c2"), "hello", "view_file result")
	// the second request must carry the first result back to the model
	contains(t, f.model.Body(1), "Wrote 5 bytes", "request 2 body")
	if f.model.Calls() != 3 {
		t.Fatalf("calls: %d", f.model.Calls())
	}
}

func TestInvalidToolCallsComeBackAsToolOutput(t *testing.T) {
	f := newFixture(t,
		callTool("c1", "no_such_tool", map[string]any{}),
		callTool("c2", "view_file", map[string]any{"pth": "x"}),
		callTool("c3", "view_file", `{"path": 5}`),
		callTool("c4", "view_file", `{"path":`), // truncated JSON
		say("giving up"),
	)
	f.start("s1")
	if err := f.run(Prompt{Text: "go"}); err != nil {
		t.Fatalf("a model mistake must never abort the turn: %v", err)
	}
	msgs := f.messages()
	contains(t, resultText(msgs, "c1"), `unknown tool "no_such_tool". Available tools:`, "unknown tool")
	contains(t, resultText(msgs, "c2"), `unknown argument "pth"`, "bad arg name")
	contains(t, resultText(msgs, "c2"), `missing required argument "path"`, "missing arg")
	contains(t, resultText(msgs, "c3"), "expected string", "wrong type")
	contains(t, resultText(msgs, "c4"), "not valid JSON", "truncated json")
	for _, id := range []string{"c1", "c2", "c3", "c4"} {
		for _, m := range msgs {
			for _, p := range m.Parts {
				if p.Type == llm.PartToolResult && p.Result.CallID == id && !p.Result.IsError {
					t.Fatalf("%s should be flagged as an error result", id)
				}
			}
		}
	}
	if st := f.lastStatus(); st.State != "done" {
		t.Fatalf("status: %+v", st)
	}
}

func TestDisabledToolsAreNotOfferedOrRunnable(t *testing.T) {
	f := newFixture(t, callTool("c1", "run_command", map[string]any{"command": "echo hi"}), say("ok"))
	f.cfg.Tools.Disabled = []string{"run_command"}
	f.start("s1")
	if err := f.run(Prompt{Text: "go"}); err != nil {
		t.Fatal(err)
	}
	for _, spec := range f.model.Request(0).Tools {
		if spec.Name == "run_command" {
			t.Fatal("a disabled tool must not be advertised to the model")
		}
	}
	contains(t, resultText(f.messages(), "c1"), "disabled by configuration", "disabled call")
}

func TestEmptyResponsesAreRetriedThenReported(t *testing.T) {
	f := newFixture(t, empty(), say("recovered"))
	f.start("s1")
	if err := f.run(Prompt{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	msgs := f.messages()
	if msgs[len(msgs)-1].Text() != "recovered" || f.model.Calls() != 2 {
		t.Fatalf("calls=%d last=%q", f.model.Calls(), msgs[len(msgs)-1].Text())
	}
	if len(msgs) != 2 {
		t.Fatalf("empty responses must not be stored in the log: %d messages", len(msgs))
	}

	f2 := newFixture(t, empty(), empty(), empty())
	f2.start("s2")
	_ = f2.run(Prompt{Text: "hi"})
	if st := f2.lastStatus(); st.State != "error" || !strings.Contains(st.Detail, "empty response") {
		t.Fatalf("persistent empties: %+v", st)
	}
}

func TestRetryBackoffHonoursRetryAfter(t *testing.T) {
	f := newFixture(t,
		fail(provider.ErrServer, "boom"),
		func(context.Context, provider.Request, *provider.Built) (*provider.Response, error) {
			return nil, &provider.Error{Kind: provider.ErrRateLimit, Message: "slow", RetryAfter: 5 * time.Second}
		},
		say("finally"),
	)
	f.start("s1")
	if err := f.run(Prompt{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sleeps) != 2 {
		t.Fatalf("sleeps: %v", f.sleeps)
	}
	if f.sleeps[0] > 20*time.Millisecond || f.sleeps[0] < 5*time.Millisecond {
		t.Fatalf("first backoff ~10ms (+-20%%), got %s", f.sleeps[0])
	}
	if f.sleeps[1] < 5*time.Second {
		t.Fatalf("Retry-After must win over the computed backoff, got %s", f.sleeps[1])
	}
}

func TestAutoPauseThenResume(t *testing.T) {
	f := newFixture(t,
		fail(provider.ErrNetwork, "down"), fail(provider.ErrNetwork, "down"), // attempt 0 and 1 (max_retries=1) -> pause
		fail(provider.ErrNetwork, "down"), fail(provider.ErrNetwork, "down"), // after the first pause -> pause again
		say("back online"),
	)
	f.cfg.Network.MaxRetries, f.cfg.Network.MaxPauses, f.cfg.Network.PauseRetryMinutes = 1, 3, 7
	f.start("s1")
	if err := f.run(Prompt{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if f.lastStatus().State != "done" {
		t.Fatalf("status: %+v", f.lastStatus())
	}
	paused := 0
	for _, s := range f.kinds(session.KindStatus) {
		if s.State == "paused" {
			paused++
			contains(t, s.Detail, "Paused; retrying at", "pause detail")
		}
	}
	if paused != 2 {
		t.Fatalf("expected 2 pauses recorded, got %d", paused)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	long := 0
	for _, d := range f.sleeps {
		if d == 7*time.Minute {
			long++
		}
	}
	if long != 2 {
		t.Fatalf("pause waits of pause_retry_minutes: %v", f.sleeps)
	}
}

func TestGivesUpAfterMaxPauses(t *testing.T) {
	steps := make([]step, 20)
	for i := range steps {
		steps[i] = fail(provider.ErrServer, "still down")
	}
	f := newFixture(t, steps...)
	f.cfg.Network.MaxRetries, f.cfg.Network.MaxPauses = 0, 2
	f.start("s1")
	_ = f.run(Prompt{Text: "hi"})
	if st := f.lastStatus(); st.State != "error" || !strings.Contains(st.Detail, "giving up") {
		t.Fatalf("status: %+v", st)
	}
	if f.model.Calls() != 3 { // initial + one retry per pause
		t.Fatalf("calls: %d", f.model.Calls())
	}
}

func TestAuthErrorsAreNotRetried(t *testing.T) {
	f := newFixture(t, fail(provider.ErrAuth, "bad key"), say("never"))
	f.start("s1")
	_ = f.run(Prompt{Text: "hi"})
	if f.model.Calls() != 1 || f.lastStatus().State != "error" {
		t.Fatalf("calls=%d status=%+v", f.model.Calls(), f.lastStatus())
	}
}

func TestResumeNowCutsThePauseShort(t *testing.T) {
	f := newFixture(t, fail(provider.ErrNetwork, "down"), say("ok"))
	f.cfg.Network.MaxRetries, f.cfg.Network.PauseRetryMinutes = 0, 60
	root := f.home.Root
	_ = root
	// a real, blocking sleep for the pause so Resume has something to interrupt
	eng, err := New(Options{Home: f.home, Env: f.env, Cfg: f.cfg, Workspace: f.ws, SessionID: "s1", Model: f.model})
	if err != nil {
		t.Fatal(err)
	}
	eng.Start()
	defer eng.Close()
	f.eng = eng
	if err := eng.Submit(Prompt{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if st, _ := eng.State(); st == StatePaused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("engine never paused")
		}
		time.Sleep(10 * time.Millisecond)
	}
	eng.Resume()
	if err := f.wait(); err != nil {
		t.Fatal(err)
	}
	if f.lastStatus().State != "done" {
		t.Fatalf("status: %+v", f.lastStatus())
	}
}

func TestMaxToolCallRounds(t *testing.T) {
	steps := make([]step, 10)
	for i := range steps {
		steps[i] = callTool("c", "list_dir", map[string]any{})
	}
	f := newFixture(t, steps...)
	f.cfg.Agent.MaxToolCallTurn = 3
	f.start("s1")
	_ = f.run(Prompt{Text: "loop forever"})
	if st := f.lastStatus(); st.State != "error" || !strings.Contains(st.Detail, "3 tool-call rounds") {
		t.Fatalf("status: %+v", st)
	}
	if f.model.Calls() != 3 {
		t.Fatalf("calls: %d", f.model.Calls())
	}
}

func TestOutputLimitTriggersContinuation(t *testing.T) {
	cut := func(context.Context, provider.Request, *provider.Built) (*provider.Response, error) {
		return &provider.Response{Message: llm.Message{Role: llm.RoleAssistant, FinishReason: llm.FinishLength,
			Parts: []llm.Part{{Type: llm.PartText, Text: "part one, "}}}, Usage: usage(10, 5)}, nil
	}
	f := newFixture(t, cut, say("part two."))
	f.start("s1")
	if err := f.run(Prompt{Text: "write"}); err != nil {
		t.Fatal(err)
	}
	contains(t, f.model.Body(1), "cut off by the output token limit", "continuation request")
}

func TestNotesAreAppendedAtTheEndAndConsumedOnce(t *testing.T) {
	f := newFixture(t, say("one"), say("two"))
	eng := f.start("s1")
	if err := eng.Note("remember the deadline", false); err != nil {
		t.Fatal(err)
	}
	if err := f.run(Prompt{Text: "first"}); err != nil {
		t.Fatal(err)
	}
	user := f.messages()[0]
	if len(user.Parts) != 2 || user.Parts[0].Text != "first" || user.Parts[1].Type != llm.PartInfo || user.Parts[1].Text != "[info: remember the deadline]" {
		t.Fatalf("note must follow the prompt text: %+v", user.Parts)
	}
	if err := f.run(Prompt{Text: "second"}); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(f.model.Body(1), "remember the deadline"); n != 1 {
		t.Fatalf("the note must appear exactly once in later requests, got %d", n)
	}
	if len(contextmgr.PendingNotes(f.events())) != 0 {
		t.Fatal("note still pending after being consumed")
	}
}

func TestNotesRideOnToolResultMessages(t *testing.T) {
	var eng *Engine
	f := newFixture(t,
		func(ctx context.Context, req provider.Request, b *provider.Built) (*provider.Response, error) {
			_ = eng.Note("the build server is back", false) // arrives while the model is working
			return callTool("c1", "list_dir", map[string]any{})(ctx, req, b)
		},
		say("ok"),
	)
	eng = f.start("s1")
	if err := f.run(Prompt{Text: "go"}); err != nil {
		t.Fatal(err)
	}
	contains(t, f.model.Body(1), "the build server is back", "note in second request")
	msgs := f.messages()
	toolMsg := msgs[2]
	last := toolMsg.Parts[len(toolMsg.Parts)-1]
	if last.Type != llm.PartInfo {
		t.Fatalf("note must come after the tool results: %+v", toolMsg.Parts)
	}
}

func TestStopInterruptsAndKeepsTheLogValid(t *testing.T) {
	started := make(chan struct{}, 1)
	f := newFixture(t, blockUntilCancelled(started), say("after the stop"))
	eng := f.start("s1")
	if err := eng.Submit(Prompt{Text: "long job"}); err != nil {
		t.Fatal(err)
	}
	<-started
	eng.Stop()
	if err := f.wait(); err != nil {
		t.Fatal(err)
	}
	if st := f.lastStatus(); st.State != "stopped" {
		t.Fatalf("status: %+v", st)
	}
	// the session is still usable, and the dangling user prompt is harmless
	if err := f.run(Prompt{Text: "next"}); err != nil {
		t.Fatal(err)
	}
	if f.lastStatus().State != "done" {
		t.Fatalf("status after next: %+v", f.lastStatus())
	}
	view := contextmgr.Build(f.events())
	if len(view.Fixes) != 0 {
		t.Fatalf("log needed repairs: %v", view.Fixes)
	}
}

func TestStopDuringToolsStillAnswersEveryCall(t *testing.T) {
	two := func(context.Context, provider.Request, *provider.Built) (*provider.Response, error) {
		m := llm.Message{Role: llm.RoleAssistant, FinishReason: llm.FinishToolCalls, Parts: []llm.Part{
			{Type: llm.PartToolCall, Call: &llm.ToolCall{ID: "a", Name: "run_command", Args: []byte(`{"command":"` + sleepCmd() + `","wait_seconds":30}`)}},
			{Type: llm.PartToolCall, Call: &llm.ToolCall{ID: "b", Name: "list_dir", Args: []byte(`{}`)}},
		}}
		return &provider.Response{Message: m, Usage: usage(5, 5)}, nil
	}
	f := newFixture(t, two, say("unused"))
	eng := f.start("s1")
	if err := eng.Submit(Prompt{Text: "go"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return eng.RunningTasks() > 0 })
	eng.Stop()
	if err := f.wait(); err != nil {
		t.Fatal(err)
	}
	msgs := f.messages()
	if resultText(msgs, "a") == "" || resultText(msgs, "b") == "" {
		t.Fatalf("every call needs a result after a stop: %+v", msgs)
	}
	contains(t, resultText(msgs, "b"), "interrupted", "second call")
	if view := contextmgr.Build(f.events()); len(view.Fixes) != 0 {
		t.Fatalf("repairs needed: %v", view.Fixes)
	}
	if eng.RunningTasks() != 0 {
		t.Fatal("stop left a process running")
	}
}

func sleepCmd() string {
	if os.PathSeparator == '\\' {
		return "Start-Sleep -Seconds 20"
	}
	return "sleep 20"
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestResumeRepairsToolCallsLeftDangling(t *testing.T) {
	f := newFixture(t, say("hello again"))
	// craft a session whose last message is an assistant tool call (crash right after the model answered)
	dir := f.home.SessionDir("crashed")
	st, err := session.Create(dir, session.Meta{ID: "crashed", Workspace: f.ws})
	if err != nil {
		t.Fatal(err)
	}
	st.Append(session.Event{Kind: session.KindSystem, Text: "SYS"})
	u := llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "do it"}}}
	a := llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, Call: &llm.ToolCall{ID: "x1", Name: "run_command", Args: []byte(`{"command":"rm"}`)}}}}
	st.Append(session.Event{Kind: session.KindMessage, Message: &u, TurnNo: 1})
	st.Append(session.Event{Kind: session.KindMessage, Message: &a, TurnNo: 1})
	st.Close()

	f.start("crashed")
	msgs := f.messages()
	if len(msgs) != 3 || msgs[2].Role != llm.RoleUser {
		t.Fatalf("a result message must be appended on resume: %+v", msgs)
	}
	r := msgs[2].Parts[0].Result
	if r.CallID != "x1" || !r.IsError || !strings.Contains(r.Output, "interrupted") {
		t.Fatalf("synthetic result: %+v", r)
	}
	if err := f.run(Prompt{Text: "continue"}); err != nil {
		t.Fatal(err)
	}
	if view := contextmgr.Build(f.events()); len(view.Fixes) != 0 {
		t.Fatalf("repairs still needed: %v", view.Fixes)
	}
}

func TestSessionLockPreventsTwoEngines(t *testing.T) {
	f := newFixture(t, say("x"))
	f.start("shared")
	_, err := New(Options{Home: f.home, Env: f.env, Cfg: f.cfg, Workspace: f.ws, SessionID: "shared", Model: f.model})
	var locked *session.ErrLocked
	if !errors.As(err, &locked) {
		t.Fatalf("second engine must fail with ErrLocked, got %v", err)
	}
	f.eng.Close()
	eng2, err := New(Options{Home: f.home, Env: f.env, Cfg: f.cfg, Workspace: f.ws, SessionID: "shared", Model: f.model})
	if err != nil {
		t.Fatalf("lock must be released on Close: %v", err)
	}
	eng2.Close()
}

func TestWorkspaceChangeIsAnnouncedNotRewrittenIntoThePrompt(t *testing.T) {
	f := newFixture(t, say("one"), say("two"))
	f.start("s1")
	if err := f.run(Prompt{Text: "first"}); err != nil {
		t.Fatal(err)
	}
	system1 := f.model.Request(0).System
	f.eng.Close()

	ws2 := filepath.Join(filepath.Dir(f.ws), "ws2")
	os.MkdirAll(ws2, 0o755)
	f.ws = ws2
	f.start("s1")
	if err := f.run(Prompt{Text: "second"}); err != nil {
		t.Fatal(err)
	}
	if system2 := f.model.Request(1).System; system2 != system1 {
		t.Fatal("the system prompt must not change mid-session (cache!)")
	}
	contains(t, f.model.Body(1), "the workspace changed from", "workspace note")
	if f.eng.Store().Meta().Workspace != ws2 {
		t.Fatal("meta workspace not updated")
	}
}

func TestBackgroundTaskWakesAnIdleAgent(t *testing.T) {
	f := newFixture(t,
		callTool("c1", "run_command", map[string]any{"command": bgCmd(), "background": true}),
		say("started it, waiting"),
		say("I saw the task finish"), // runs because the exit note wakes the agent
	)
	f.start("s1")
	if err := f.run(Prompt{Text: "run something in the background"}); err != nil {
		t.Fatal(err)
	}
	contains(t, f.model.Body(2), "background task #1 finished (exit code 0)", "wake request")
	msgs := f.messages()
	if msgs[len(msgs)-1].Text() != "I saw the task finish" {
		t.Fatalf("last message: %q", msgs[len(msgs)-1].Text())
	}
}

func bgCmd() string {
	if os.PathSeparator == '\\' {
		return "Start-Sleep -Seconds 1; Write-Output done"
	}
	return "sleep 1; echo done"
}
