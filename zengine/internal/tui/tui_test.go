package tui

import (
	"context"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ranjanlive/zpilot/zengine/internal/control"
	"github.com/ranjanlive/zpilot/zengine/internal/engine"
	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
)

type fakeAPI struct {
	mu       sync.Mutex
	state    control.StateResponse
	events   []session.Event
	prompts  []string
	attached [][]string
	notes    []string
	commands []string
	stops    int
	reply    map[string]engine.CommandResult
}

func (f *fakeAPI) State(context.Context) (control.StateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}
func (f *fakeAPI) Events(context.Context, int64) ([]session.Event, error) { return f.events, nil }
func (f *fakeAPI) Prompt(_ context.Context, text string, a ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, text)
	f.attached = append(f.attached, a)
	return nil
}
func (f *fakeAPI) Note(_ context.Context, t string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = append(f.notes, t)
	return nil
}
func (f *fakeAPI) Command(_ context.Context, line string) (engine.CommandResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, line)
	if r, ok := f.reply[line]; ok {
		return r, nil
	}
	return engine.CommandResult{Handled: true, Reply: "ok"}, nil
}
func (f *fakeAPI) Stop(context.Context) error   { f.mu.Lock(); f.stops++; f.mu.Unlock(); return nil }
func (f *fakeAPI) Resume(context.Context) error { return nil }
func (f *fakeAPI) Stream(ctx context.Context, _ int64, _ func(session.Event), _ func(engine.Live)) error {
	<-ctx.Done()
	return nil
}

func msgEvent(seq int64, m llm.Message) session.Event {
	return session.Event{Seq: seq, Kind: session.KindMessage, Message: &m}
}

func part(t llm.PartType, text string) llm.Part { return llm.Part{Type: t, Text: text} }

func newModel(t *testing.T, api *fakeAPI) *Model {
	t.Helper()
	m := New(api)
	m.send = func(tea.Msg) {}
	t.Cleanup(m.cancel)
	var next tea.Model = m
	next, _ = next.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return next.(*Model)
}

func update(m *Model, msg tea.Msg) tea.Cmd {
	_, cmd := m.Update(msg)
	return cmd
}

func typeText(m *Model, s string) {
	m.ta.SetValue(s)
}

func enter(m *Model) tea.Msg {
	cmd := update(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		return nil
	}
	return cmd()
}

func history() []session.Event {
	call := &llm.ToolCall{ID: "c1", Name: "view_file", Args: []byte(`{"path":"a.go"}`)}
	return []session.Event{
		msgEvent(1, llm.Message{Role: llm.RoleUser, Parts: []llm.Part{part(llm.PartText, "fix the bug in a.go")}}),
		msgEvent(2, llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{part(llm.PartText, "I'll look at **a.go** first."), {Type: llm.PartToolCall, Call: call}}}),
		msgEvent(3, llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartToolResult, Result: &llm.ToolResult{CallID: "c1", Name: "view_file", Output: "line1\nline2\nline3\nline4\nline5\nline6"}}}}),
		msgEvent(4, llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{part(llm.PartText, "Found it: an off-by-one.")}}),
	}
}

func loaded(m *Model, st control.StateResponse, evs []session.Event) {
	update(m, loadedMsg{state: st, events: evs})
}

func TestHistoryRendersAndToolResultsAttachToTheirCall(t *testing.T) {
	api := &fakeAPI{}
	m := newModel(t, api)
	loaded(m, control.StateResponse{Session: "s1", Title: "fix a.go", Model: "gem", State: engine.StateIdle, CompactAt: 800000,
		Counters: engine.Counters{Context: 4400, Prompt: 16000, Completion: 500}}, history())

	view := m.View()
	for _, want := range []string{"fix the bug in a.go", "Found it: an off-by-one.", "view_file", "done", "ctx 4.4k/800k", "↑16k", "idle"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	if m.seq != 4 {
		t.Fatalf("seq: %d", m.seq)
	}
	// the tool block was updated in place, not duplicated
	tools := 0
	for _, b := range m.blocks {
		if b.kind == "tool" {
			tools++
			if !b.tool.done || b.tool.isErr {
				t.Fatalf("tool block state: %+v", b.tool)
			}
		}
	}
	if tools != 1 {
		t.Fatalf("tool blocks: %d", tools)
	}
	// an already-seen event is ignored (the stream replays from the last seq)
	before := len(m.blocks)
	update(m, eventMsg(history()[0]))
	if len(m.blocks) != before {
		t.Fatal("duplicate event rendered twice")
	}
}

func TestIdleInputIsAPromptBusyInputIsANote(t *testing.T) {
	api := &fakeAPI{}
	m := newModel(t, api)
	loaded(m, control.StateResponse{Session: "s1", State: engine.StateIdle}, nil)

	typeText(m, "do the thing")
	enter(m)
	if len(api.prompts) != 1 || api.prompts[0] != "do the thing" || len(api.notes) != 0 {
		t.Fatalf("idle: prompts=%v notes=%v", api.prompts, api.notes)
	}
	if m.ta.Value() != "" {
		t.Fatal("the input must clear after sending")
	}

	update(m, liveMsg{Kind: engine.LiveState, State: engine.StateWorking})
	if !m.busy() {
		t.Fatal("working state must be busy")
	}
	typeText(m, "also check the tests")
	enter(m)
	if len(api.notes) != 1 || api.notes[0] != "also check the tests" || len(api.prompts) != 1 {
		t.Fatalf("busy: prompts=%v notes=%v", api.prompts, api.notes)
	}
	if !strings.Contains(m.View(), "Enter sends a note") {
		t.Fatal("the footer must say input becomes a note while the agent works")
	}
	typeText(m, "   ")
	if msg := enter(m); msg != nil || len(api.notes) != 1 {
		t.Fatal("blank input must send nothing")
	}
}

func TestSlashCommandsRunOnTheEngineAndExitDetaches(t *testing.T) {
	api := &fakeAPI{reply: map[string]engine.CommandResult{
		"/status":  {Handled: true, Reply: "State: idle"},
		"/exit":    {Handled: true, Exit: true, Reply: "Leaving"},
		"/new":     {Handled: true, NewSession: true, Exit: true},
		"/usr/bin": {Handled: false},
	}}
	m := newModel(t, api)
	loaded(m, control.StateResponse{Session: "s1", State: engine.StateIdle}, nil)

	typeText(m, "/status")
	msg := enter(m)
	update(m, msg)
	if !strings.Contains(m.View(), "State: idle") || len(api.prompts) != 0 {
		t.Fatalf("command reply must be shown and never sent as a prompt:\n%s", m.View())
	}

	typeText(m, "/usr/bin is where it lives")
	// not a command the engine knows: it falls through to a normal prompt
	api.reply["/usr/bin is where it lives"] = engine.CommandResult{Handled: false}
	enter(m)
	if len(api.prompts) != 1 {
		t.Fatalf("unhandled slash text must become a prompt: %v", api.prompts)
	}

	typeText(m, "/exit")
	msg = enter(m)
	cmd := update(m, msg)
	if !m.quitting || cmd == nil {
		t.Fatal("/exit must quit the UI")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("expected a quit command")
	}
	if len(api.commands) < 2 {
		t.Fatalf("commands: %v", api.commands)
	}

	m2 := newModel(t, &fakeAPI{reply: api.reply})
	loaded(m2, control.StateResponse{Session: "s1", State: engine.StateIdle}, nil)
	typeText(m2, "/new")
	update(m2, enter(m2))
	if m2.banner != "new" {
		t.Fatal("/new must be reported to the caller")
	}
}

func TestInterruptKeys(t *testing.T) {
	api := &fakeAPI{}
	m := newModel(t, api)
	loaded(m, control.StateResponse{Session: "s1", State: engine.StateIdle}, nil)

	// idle: Ctrl-C leaves the UI (detaches), it does not stop anything
	_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if _, ok := cmd().(tea.QuitMsg); !ok || api.stops != 0 {
		t.Fatalf("idle ctrl-c: stops=%d", api.stops)
	}

	m2 := newModel(t, api)
	loaded(m2, control.StateResponse{Session: "s1", State: engine.StateWorking}, nil)
	// working: Ctrl-C and Esc stop the agent and keep the UI open
	for _, k := range []tea.KeyMsg{{Type: tea.KeyCtrlC}, {Type: tea.KeyEsc}} {
		_, cmd = m2.handleKey(k)
		if cmd == nil {
			t.Fatal("expected a stop command")
		}
		cmd()
	}
	if api.stops != 2 || m2.quitting {
		t.Fatalf("working: stops=%d quitting=%v", api.stops, m2.quitting)
	}
}

func TestStreamingPreviewIsReplacedByTheDurableMessage(t *testing.T) {
	api := &fakeAPI{}
	m := newModel(t, api)
	loaded(m, control.StateResponse{Session: "s1", State: engine.StateIdle}, nil)
	update(m, liveMsg{Kind: engine.LiveState, State: engine.StateWorking})
	update(m, liveMsg{Kind: engine.LiveText, Text: "Partial ans"})
	update(m, liveMsg{Kind: engine.LiveText, Text: "wer so far"})
	if !strings.Contains(m.View(), "Partial answer so far") {
		t.Fatalf("live text must show while streaming:\n%s", m.View())
	}
	if !strings.Contains(m.View(), "writing") {
		t.Fatal("the status line must say what the agent is doing")
	}
	update(m, eventMsg(msgEvent(1, llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{part(llm.PartText, "Final answer.")}})))
	if m.stream != "" {
		t.Fatal("the streamed preview must be dropped once the message is stored")
	}
	v := m.View()
	if strings.Contains(v, "Partial answer so far") || !strings.Contains(v, "Final answer.") {
		t.Fatalf("view after the durable message:\n%s", v)
	}
	// a retry resets the half-streamed text
	update(m, liveMsg{Kind: engine.LiveText, Text: "half"})
	update(m, liveMsg{Kind: engine.LiveRetry, Text: "reset_stream", Detail: "retrying in 2s"})
	if m.stream != "" || m.banner != "retrying in 2s" {
		t.Fatalf("retry: stream=%q banner=%q", m.stream, m.banner)
	}
}

func TestAttachSendsWithTheNextPromptOnly(t *testing.T) {
	api := &fakeAPI{}
	m := newModel(t, api)
	loaded(m, control.StateResponse{Session: "s1", State: engine.StateIdle}, nil)
	typeText(m, "/attach "+t.TempDir())
	enter(m)
	if len(m.attach) != 1 {
		t.Fatalf("attachments: %v (blocks %+v)", m.attach, m.blocks)
	}
	typeText(m, "look at this")
	enter(m)
	if len(api.attached) != 1 || len(api.attached[0]) != 1 {
		t.Fatalf("attachments sent: %v", api.attached)
	}
	typeText(m, "and now text only")
	enter(m)
	if len(api.attached) != 2 || len(api.attached[1]) != 0 {
		t.Fatalf("attachments must not repeat: %v", api.attached)
	}
	typeText(m, "/attach /definitely/not/here.png")
	enter(m)
	if len(m.attach) != 0 || m.blocks[len(m.blocks)-1].kind != "error" {
		t.Fatal("a missing file must be reported, not queued")
	}
}

func TestPausedClosedAndHeaderStates(t *testing.T) {
	m := newModel(t, &fakeAPI{})
	loaded(m, control.StateResponse{Session: "s1", State: engine.StatePaused, Detail: "network trouble (down)"}, nil)
	if v := m.View(); !strings.Contains(v, "paused") || !strings.Contains(v, "/resume") {
		t.Fatalf("paused view:\n%s", v)
	}
	update(m, streamEnd{})
	v := m.View()
	if !strings.Contains(v, "engine stopped") || !strings.Contains(v, "reopen it") {
		t.Fatalf("closed view:\n%s", v)
	}
	// estimated token counts are marked
	update(m, liveMsg{Kind: engine.LiveUsage, Tokens: engine.Counters{Context: 10, Prompt: 2000, Completion: 100, Estimated: true}})
	if !strings.Contains(m.View(), "↑~2.0k") {
		t.Fatalf("estimates must be marked with ~:\n%s", m.View())
	}
}

func TestFormatMarkdown(t *testing.T) {
	out := formatMarkdown("# Title\nsome `code` and **bold** text\n```go\nx := 1\n```\nafter")
	for _, want := range []string{"Title", "code", "bold", "x := 1", "after"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q: %q", want, out)
		}
	}
	if strings.Contains(out, "```") || strings.Contains(out, "**") || strings.Contains(out, "`code`") {
		t.Errorf("markup must be consumed: %q", out)
	}
}
