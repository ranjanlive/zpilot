package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/config"
	"github.com/ranjanlive/zpilot/zengine/internal/contextmgr"
	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/prompt"
	"github.com/ranjanlive/zpilot/zengine/internal/provider"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
	"github.com/ranjanlive/zpilot/zengine/internal/tools"
)

// Options configure an Engine.
type Options struct {
	Home      config.Home
	Env       config.Env
	Cfg       config.Config
	Workspace string
	SessionID string
	// Role is recorded in the session lock ("headless", "service", ...).
	Role string
	// SessionRoot overrides where the session directory lives (default
	// <home>/sessions). Sub-agents keep theirs inside the parent session.
	SessionRoot string
	// SystemSuffix is appended to the system prompt snapshot.
	SystemSuffix string
	// Depth is 0 for the main agent and 1 for a sub-agent (which cannot spawn more).
	Depth int

	// Model overrides the provider built from Env/Cfg (tests, embedding).
	Model Model
	// Registry overrides the default tool set.
	Registry *tools.Registry
	// Approve answers permission requests (run_command with permissions "ask").
	Approve func(ctx context.Context, req tools.ApprovalRequest) (bool, string)
	// Sleep replaces the backoff wait (tests). It must return early with
	// ctx.Err() when ctx ends.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Prompt is one unit of work for the agent.
type Prompt struct {
	Text string
	// Attachments are media file paths (images, audio, video, PDFs) sent with the prompt.
	Attachments []string
	// Auto marks an engine-initiated turn (a background task woke the agent).
	Auto bool
}

// Engine drives one session.
type Engine struct {
	opts      Options
	cfg       config.Config
	env       config.Env
	home      config.Home
	workspace string

	store    *session.Store
	lock     *session.Lock
	model    Model
	registry *tools.Registry
	toolEnv  *tools.Env
	procs    *tools.Manager
	live     bus
	sleep    func(ctx context.Context, d time.Duration) error

	mu          sync.Mutex
	workspaceMu sync.RWMutex // guards workspace and the tool environment throughout each turn
	state       State
	detail      string
	counters    Counters
	lastUsage   *llm.Usage // usage of the latest response, valid until the context is rewritten
	cancelTurn  context.CancelFunc
	closed      bool
	changed     chan struct{} // closed/replaced on every state or queue change

	queue      chan Prompt
	queued     int
	wake       chan struct{}
	resumeNow  chan struct{}
	workerStop context.CancelFunc
	workerDone chan struct{}
}

// New opens (or creates) the session and prepares the engine. It acquires the
// session lock, so a second engine on the same session fails fast.
func New(o Options) (*Engine, error) {
	if err := session.ValidateID(o.SessionID); err != nil {
		return nil, err
	}
	ws, err := filepath.Abs(o.Workspace)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(ws); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("workspace %s is not a directory", ws)
	}
	if probs := o.Cfg.Validate(); len(probs) > 0 {
		return nil, fmt.Errorf("invalid config.json: %s", strings.Join(probs, "; "))
	}
	if o.Role == "" {
		o.Role = "engine"
	}

	dir := o.Home.SessionDir(o.SessionID)
	if o.SessionRoot != "" {
		dir = filepath.Join(o.SessionRoot, o.SessionID)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	lock, err := acquire(dir, o.Role)
	if err != nil {
		return nil, err
	}
	e := &Engine{opts: o, cfg: o.Cfg, env: o.Env, home: o.Home, workspace: ws, lock: lock, sleep: o.Sleep,
		state: StateIdle, changed: make(chan struct{}), queue: make(chan Prompt, 64), wake: make(chan struct{}, 1), resumeNow: make(chan struct{}, 1)}
	if e.sleep == nil {
		e.sleep = sleepCtx
	}
	fail := func(err error) (*Engine, error) {
		lock.Release()
		if e.store != nil {
			e.store.Close()
		}
		return nil, err
	}

	var previousWorkspace string
	if st, err := session.Open(dir); err == nil {
		e.store = st
		previousWorkspace = st.Meta().Workspace
	} else if _, serr := os.Stat(filepath.Join(dir, "events.jsonl")); serr == nil {
		return fail(err) // exists but unreadable: do not paper over corruption
	} else {
		if e.store, err = session.Create(dir, session.Meta{ID: o.SessionID, Workspace: ws, Provider: o.Env.Provider, Model: o.Env.Model}); err != nil {
			return fail(err)
		}
	}

	if o.Model != nil {
		e.model = o.Model
	} else {
		client, err := provider.New(o.Env.ProviderSettings(o.Cfg))
		if err != nil {
			return fail(err)
		}
		e.model = NewModel(client)
	}

	e.procs = tools.NewManager()
	e.procs.OnExit = func(i tools.ProcInfo) {
		note := i.Note
		if note == "" {
			note = tools.ExitNote(i)
		}
		_ = e.Note(note, true)
	}
	e.registry = o.Registry
	var toolProblems []string
	if e.registry == nil {
		e.registry, toolProblems = DefaultRegistry(o.Cfg, o.Home)
	}
	if o.Depth == 0 {
		e.registry.Register(&subagentTool{parent: e})
	}
	e.toolEnv = &tools.Env{
		Workspace: ws, Home: o.Home, SessionID: o.SessionID, SessionDir: dir, Caps: o.Env.Caps, Cfg: o.Cfg,
		PutBlob: e.store.PutBlob, Approve: o.Approve,
		Notify:     func(text string) { _ = e.Note(text, false) },
		NotifyWake: func(text string) { _ = e.Note(text, true) },
		Extra:      map[string]any{"procs": e.procs},
	}

	if err := e.restore(previousWorkspace); err != nil {
		return fail(err)
	}
	for _, p := range toolProblems {
		_ = e.recordStatus("warning", "external tool not loaded: "+p)
	}
	return e, nil
}

func acquire(dir string, role string) (*session.Lock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return session.Acquire(dir, role)
}

// DefaultRegistry builds the standard tool set: the built-ins plus any
// third-party tools found in <home>/tools, honouring the enable/disable lists
// in config. Problems loading individual external tools are returned, not fatal.
func DefaultRegistry(cfg config.Config, home config.Home) (*tools.Registry, []string) {
	r := tools.NewRegistry(cfg.Tools)
	tools.RegisterFileTools(r)
	tools.RegisterCommandTools(r)
	tools.RegisterMemoryTool(r)
	tools.RegisterSkillTool(r)
	return r, tools.RegisterExternal(r, home.ToolsDir())
}

// restore brings a freshly opened session to a consistent state: snapshot the
// system prompt on first use, announce a moved workspace, answer tool calls
// left dangling by a crash, and rebuild the token counters.
func (e *Engine) restore(previousWorkspace string) error {
	events := e.store.Events()
	hasSystem := false
	for _, ev := range events {
		switch ev.Kind {
		case session.KindSystem:
			hasSystem = true
		case session.KindUsage:
			if ev.Usage != nil {
				e.counters.add(ev.Usage)
				u := *ev.Usage
				e.lastUsage = &u
			}
		}
	}
	if !hasSystem {
		if _, err := e.store.Append(session.Event{Kind: session.KindSystem, Text: e.buildSystemPrompt()}); err != nil {
			return err
		}
	}
	if previousWorkspace != "" && previousWorkspace != e.workspace {
		if _, err := e.store.Append(session.Event{Kind: session.KindNote, Text: fmt.Sprintf("[info: the workspace changed from %s to %s. Relative paths now resolve in the new workspace.]", previousWorkspace, e.workspace)}); err != nil {
			return err
		}
		_ = e.store.UpdateMeta(func(m *session.Meta) { m.Workspace = e.workspace })
	}
	view := contextmgr.BuildRaw(e.store.ContextEvents())
	if dangling := contextmgr.DanglingCalls(view.Messages); len(dangling) > 0 {
		msg := llm.Message{Role: llm.RoleUser, Parts: contextmgr.InterruptedResults(dangling)}
		if _, err := e.store.Append(session.Event{Kind: session.KindMessage, Message: &msg, TurnNo: view.Turn}); err != nil {
			return err
		}
		e.lastUsage = nil
	}
	return nil
}

func (e *Engine) buildSystemPrompt() string {
	p := prompt.Build(prompt.Inputs{Home: e.home, Workspace: e.workspace, Caps: e.env.Caps, Model: e.env.Model})
	if e.opts.SystemSuffix != "" {
		p += "\n\n" + strings.TrimSpace(e.opts.SystemSuffix) + "\n"
	}
	return p
}

// ---- lifecycle ----

// Start launches the worker that processes submitted prompts.
func (e *Engine) Start() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.workerStop != nil || e.closed {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.workerStop = cancel
	e.workerDone = make(chan struct{})
	go e.worker(ctx)
}

// Close stops the engine, kills background tasks and releases the session.
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	cancelTurn, stop, done := e.cancelTurn, e.workerStop, e.workerDone
	e.mu.Unlock()

	if cancelTurn != nil {
		cancelTurn()
	}
	if stop != nil {
		stop()
		<-done
	}
	e.procs.KillAll()
	e.setState(StateClosed, "")
	err := e.store.Close()
	e.lock.Release()
	return err
}

// Stop interrupts the running turn and discards queued prompts.
func (e *Engine) Stop() {
	e.mu.Lock()
	if e.cancelTurn != nil {
		e.cancelTurn()
	}
	e.mu.Unlock()
	for {
		select {
		case <-e.queue:
			e.mu.Lock()
			e.queued--
			e.mu.Unlock()
		default:
			e.signal()
			return
		}
	}
}

// Resume cuts short a pause and retries immediately.
func (e *Engine) Resume() {
	select {
	case e.resumeNow <- struct{}{}:
	default:
	}
}

// Submit queues a prompt. It never blocks the caller on the model.
func (e *Engine) Submit(p Prompt) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errors.New("engine is closed")
	}
	if e.workerStop == nil {
		e.mu.Unlock()
		return errors.New("engine not started")
	}
	e.queued++
	e.mu.Unlock()
	select {
	case e.queue <- p:
		e.signal()
		return nil
	default:
		e.mu.Lock()
		e.queued--
		e.mu.Unlock()
		return errors.New("too many queued prompts")
	}
}

// Note queues an "[info: ...]" note that rides on the next request. With
// wake=true an idle agent is started to react to it (background task results).
func (e *Engine) Note(text string, wake bool) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if !strings.HasPrefix(text, "[info:") {
		text = "[info: " + text + "]"
	}
	ev := session.Event{Kind: session.KindNote, Text: text}
	if wake {
		ev.State = "wake"
	}
	if _, err := e.store.Append(ev); err != nil {
		return err
	}
	if wake {
		select {
		case e.wake <- struct{}{}:
		default:
		}
	}
	e.signal()
	return nil
}

// ---- observation ----

// Store exposes the session log for UIs (read-only use expected).
func (e *Engine) Store() *session.Store { return e.store }

// SessionID returns the session's ID.
func (e *Engine) SessionID() string { return e.opts.SessionID }

// Workspace returns the absolute workspace path.
func (e *Engine) Workspace() string {
	e.workspaceMu.RLock()
	defer e.workspaceMu.RUnlock()
	return e.workspace
}

// State returns the lifecycle state and a human-readable detail.
func (e *Engine) State() (State, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state, e.detail
}

// Counters returns the token accounting.
func (e *Engine) Counters() Counters {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.counters
}

// SubscribeLive returns a channel of transient events (token deltas, state).
func (e *Engine) SubscribeLive() (<-chan Live, func()) { return e.live.subscribe(512) }

// Lock returns the session lock so the process hosting the engine can publish
// its control endpoint through it.
func (e *Engine) Lock() *session.Lock { return e.lock }

// Model returns the configured model name.
func (e *Engine) Model() string { return e.env.Model }

// CompactAtTokens exposes the compaction threshold for UIs (0 = disabled).
func (e *Engine) CompactAtTokens() int { return e.compactAt() }

// RunningTasks reports how many background tasks are alive.
func (e *Engine) RunningTasks() int { return e.procs.Running() }

// WaitIdle blocks until nothing is running, queued or pending, or ctx ends.
// While background tasks run, the engine stays non-idle so their results can
// wake the agent; waiting is bounded by agent.max_background_wait_minutes.
func (e *Engine) WaitIdle(ctx context.Context) error {
	var bgDeadline time.Time
	for {
		e.settle()
		e.mu.Lock()
		idle := (e.state == StateIdle || e.state == StateWaitingBG) && e.queued == 0
		wakeNotes := len(e.store.PendingNotes()) > 0 && e.hasWakeNote()
		ch := e.changed
		e.mu.Unlock()

		if idle && !wakeNotes {
			if e.procs.Running() == 0 {
				return nil
			}
			limit := time.Duration(e.cfg.Agent.MaxBackgroundWaitMinutes) * time.Minute
			if limit <= 0 {
				return nil
			}
			if bgDeadline.IsZero() {
				bgDeadline = time.Now().Add(limit)
			}
			if time.Now().After(bgDeadline) {
				return fmt.Errorf("gave up waiting for %d background task(s) after %s", e.procs.Running(), limit)
			}
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (e *Engine) hasWakeNote() bool {
	for _, n := range e.store.PendingNotes() {
		if n.State == "wake" {
			return true
		}
	}
	return false
}

func (e *Engine) signal() {
	e.mu.Lock()
	close(e.changed)
	e.changed = make(chan struct{})
	e.mu.Unlock()
}

func (e *Engine) setState(s State, detail string) {
	e.mu.Lock()
	changed := e.state != s || e.detail != detail
	e.state, e.detail = s, detail
	e.mu.Unlock()
	if changed {
		e.live.publish(Live{Kind: LiveState, State: s, Detail: detail})
		e.signal()
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---- worker ----

func (e *Engine) worker(ctx context.Context) {
	defer close(e.workerDone)
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-e.queue:
			e.runPrompt(ctx, p)
			e.mu.Lock()
			e.queued--
			e.mu.Unlock()
		case <-e.wake:
			if e.hasWakeNote() {
				e.runPrompt(ctx, Prompt{Auto: true})
			}
		}
		e.settle()
		e.signal()
	}
}

// settle moves a resting engine between idle and waiting-for-background-tasks
// according to what is actually running. It never disturbs an active state.
func (e *Engine) settle() {
	e.mu.Lock()
	st := e.state
	e.mu.Unlock()
	if st != StateIdle && st != StateWaitingBG {
		return
	}
	if n := e.procs.Running(); n > 0 {
		e.setState(StateWaitingBG, fmt.Sprintf("%d background task(s) running", n))
	} else {
		e.setState(StateIdle, "")
	}
}

// runPrompt executes one prompt as a cancellable turn and records how it ended.
func (e *Engine) runPrompt(parent context.Context, p Prompt) {
	e.workspaceMu.RLock()
	defer e.workspaceMu.RUnlock()
	ctx, cancel := context.WithCancel(parent)
	e.mu.Lock()
	e.cancelTurn = cancel
	e.mu.Unlock()
	defer func() {
		cancel()
		e.mu.Lock()
		e.cancelTurn = nil
		e.mu.Unlock()
	}()

	e.setState(StateWorking, "")
	_ = e.recordStatus("working", "")
	err := e.runTurn(ctx, p)
	switch {
	case err == nil:
		_ = e.recordStatus("done", "")
	case errors.Is(err, context.Canceled) || ctx.Err() != nil:
		_ = e.recordStatus("stopped", "interrupted by the user")
	default:
		_ = e.recordStatus("error", err.Error())
	}
	e.setState(StateIdle, "")
}

func (e *Engine) recordStatus(state, detail string) error {
	_, err := e.store.Append(session.Event{Kind: session.KindStatus, State: state, Detail: detail})
	return err
}

// lastStatus returns the state of the most recent terminal status event
// ("done", "error", "stopped"), skipping progress markers.
func lastStatus(events []session.Event) string {
	if ev, ok := lastStatusEvent(events); ok {
		return ev.State
	}
	return ""
}

func lastStatusDetail(events []session.Event) string {
	if ev, ok := lastStatusEvent(events); ok {
		return ev.Detail
	}
	return ""
}

func lastStatusEvent(events []session.Event) (session.Event, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Kind == session.KindStatus && ev.State != "working" && ev.State != "warning" && ev.State != "paused" {
			return ev, true
		}
	}
	return session.Event{}, false
}

// HomeScratch returns the home scratchpad directory (uploads land here).
func (e *Engine) HomeScratch() string { return e.home.ScratchDir() }
