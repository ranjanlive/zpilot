package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/config"
	"github.com/ranjanlive/zpilot/zengine/internal/control"
	"github.com/ranjanlive/zpilot/zengine/internal/engine"
	"github.com/ranjanlive/zpilot/zengine/internal/render"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
)

// loadHome resolves the home directory and loads and validates its
// configuration. On failure it has already explained the problem to the user.
func loadHome(homeFlag string, stderr io.Writer) (config.Home, config.Env, config.Config, bool) {
	home, err := config.ResolveHome(homeFlag)
	if err != nil {
		fmt.Fprintf(stderr, "zagent: %v\n", err)
		return home, config.Env{}, config.Config{}, false
	}
	if _, err := os.Stat(home.EnvFile()); errors.Is(err, os.ErrNotExist) {
		created, ierr := home.Init()
		if ierr != nil {
			fmt.Fprintf(stderr, "zagent: cannot create the agent home %s: %v\n", home.Root, ierr)
			return home, config.Env{}, config.Config{}, false
		}
		fmt.Fprintf(stderr, "zagent: first run: created the agent home at %s (%d items).\nEdit %s (set PROVIDER, BASE_URL, API_KEY, MODEL) and run again. 'zagent doctor' tests the connection.\n",
			home.Root, len(created), home.EnvFile())
		return home, config.Env{}, config.Config{}, false
	}
	env, cfg, err := home.Load()
	if err != nil {
		fmt.Fprintf(stderr, "zagent: %v\n", err)
		return home, env, cfg, false
	}
	problems := append(env.ValidateFor(cfg), cfg.Validate()...)
	if len(problems) > 0 {
		fmt.Fprintf(stderr, "zagent: configuration problems (%s, %s):\n", home.EnvFile(), home.ConfigFile())
		for _, p := range problems {
			fmt.Fprintf(stderr, "  - %s\n", p)
		}
		return home, env, cfg, false
	}
	return home, env, cfg, true
}

func resolveWorkspace(flagVal string) (string, error) {
	if flagVal == "" {
		return os.Getwd()
	}
	return filepath.Abs(flagVal)
}

// pickSession decides which session this run uses.
func pickSession(o *options, home config.Home, workspace string) (id string, resumed bool, err error) {
	switch {
	case o.newSession:
		return session.NewID(), false, nil
	case o.sessionSet && o.session != "":
		if err := session.ValidateID(o.session); err != nil {
			return "", false, err
		}
		_, statErr := os.Stat(filepath.Join(home.SessionDir(o.session), "events.jsonl"))
		return o.session, statErr == nil, nil
	case o.sessionSet: // bare --session: the most recent session in this workspace
		metas, err := session.List(home.SessionsDir())
		if err != nil {
			return "", false, err
		}
		for _, m := range metas {
			if sameDir(m.Workspace, workspace) {
				return m.ID, true, nil
			}
		}
		return session.NewID(), false, nil
	}
	return session.NewID(), false, nil
}

func sameDir(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || (os.PathSeparator == '\\' && strings.EqualFold(a, b))
}

func readPrompt(o *options) (string, error) {
	text := o.prompt
	if o.promptFile != "" {
		var b []byte
		var err error
		if o.promptFile == "-" {
			b, err = io.ReadAll(os.Stdin)
		} else {
			b, err = os.ReadFile(o.promptFile)
		}
		if err != nil {
			return "", fmt.Errorf("cannot read the prompt file: %w", err)
		}
		if text != "" {
			text += "\n\n"
		}
		text += string(b)
	}
	if len(o.rest) > 0 && text == "" { // allow: zagent "do the thing"
		text = strings.Join(o.rest, " ")
	}
	return strings.TrimSpace(text), nil
}

// absAttachments makes attachment paths absolute so a service running in a
// different directory resolves them the same way.
func absAttachments(paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		a, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// backend is the engine as the headless runner sees it: in this process, or
// behind a running service.
type backend interface {
	submit(ctx context.Context, text string, attach []string) error
	// waitIdle returns once the agent has nothing left to do.
	waitIdle(ctx context.Context) error
	lastSeq() int64
	eventsSince(seq int64) []session.Event
	counters() engine.Counters
	stop()
	progress(o *options, stderr io.Writer) (stop func())
	close()
}

// ---- in-process backend ----

type localBackend struct{ h *host }

func (b localBackend) submit(_ context.Context, text string, attach []string) error {
	return b.h.eng.Submit(engine.Prompt{Text: text, Attachments: attach})
}
func (b localBackend) waitIdle(ctx context.Context) error    { return b.h.eng.WaitIdle(ctx) }
func (b localBackend) lastSeq() int64                        { return int64(b.h.eng.Store().Len()) }
func (b localBackend) eventsSince(seq int64) []session.Event { return b.h.eng.Store().Since(seq) }
func (b localBackend) counters() engine.Counters             { return b.h.eng.Counters() }
func (b localBackend) stop()                                 { b.h.eng.Stop() }
func (b localBackend) close()                                { b.h.close() }
func (b localBackend) progress(o *options, stderr io.Writer) func() {
	if o.quiet {
		return func() {}
	}
	live, cancel := b.h.eng.SubscribeLive()
	return pumpLive(o, stderr, func(emit func(engine.Live)) {
		for ev := range live {
			emit(ev)
		}
	}, cancel)
}

// ---- remote backend: a service already runs this session ----

type remoteBackend struct {
	c       *control.Client
	maxWait time.Duration
}

func (b remoteBackend) submit(ctx context.Context, text string, attach []string) error {
	return b.c.Prompt(ctx, text, attach...)
}

func (b remoteBackend) waitIdle(ctx context.Context) error {
	start := time.Now()
	sawBusy := false
	for {
		st, err := b.c.State(ctx)
		if err != nil {
			return err
		}
		busy := st.State == engine.StateWorking || st.State == engine.StateCompacting || st.State == engine.StatePaused
		sawBusy = sawBusy || busy
		if !busy && sawBusy && st.Tasks == 0 {
			return nil
		}
		if !busy && sawBusy && st.Tasks > 0 && time.Since(start) > b.maxWait {
			return fmt.Errorf("gave up waiting for %d background task(s)", st.Tasks)
		}
		// A prompt that was queued but not yet picked up shows as idle: give it a moment.
		if !sawBusy && time.Since(start) > 5*time.Second && !busy {
			if evs, _ := b.c.Events(ctx, st.LastSeq-1); len(evs) > 0 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (b remoteBackend) lastSeq() int64 {
	st, err := b.c.State(context.Background())
	if err != nil {
		return 0
	}
	return st.LastSeq
}
func (b remoteBackend) eventsSince(seq int64) []session.Event {
	evs, _ := b.c.Events(context.Background(), seq)
	return evs
}
func (b remoteBackend) counters() engine.Counters {
	st, _ := b.c.State(context.Background())
	return st.Counters
}
func (b remoteBackend) stop()  { _ = b.c.Stop(context.Background()) }
func (b remoteBackend) close() {}
func (b remoteBackend) progress(o *options, stderr io.Writer) func() {
	if o.quiet {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return pumpLive(o, stderr, func(emit func(engine.Live)) {
		_ = b.c.Stream(ctx, b.lastSeq(), nil, emit)
	}, cancel)
}

// pumpLive prints activity to stderr while the agent works.
func pumpLive(o *options, stderr io.Writer, source func(emit func(engine.Live)), cancel func()) func() {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		streaming := false
		source(func(ev engine.Live) {
			switch ev.Kind {
			case engine.LiveToolRun:
				if streaming {
					fmt.Fprintln(stderr)
					streaming = false
				}
				fmt.Fprintf(stderr, "  ▸ %s %s\n", ev.Name, ev.Detail)
			case engine.LiveText:
				if o.verbose {
					fmt.Fprint(stderr, ev.Text)
					streaming = true
				}
			case engine.LiveRetry:
				if ev.Detail != "" {
					fmt.Fprintf(stderr, "  ⟳ %s\n", ev.Detail)
				}
			case engine.LiveState:
				if ev.State == engine.StatePaused || ev.State == engine.StateCompacting {
					fmt.Fprintf(stderr, "  ⏸ %s %s\n", ev.State, ev.Detail)
				}
			}
		})
	}()
	return func() { once.Do(func() { cancel(); <-done }) }
}

func cmdRun(o *options, stdout, stderr io.Writer) int {
	if o.serve {
		return runService(o, stderr)
	}
	prompt, err := readPrompt(o)
	if err != nil {
		fmt.Fprintf(stderr, "zagent: %v\n", err)
		return exitUsage
	}
	if o.ui == "headless" && prompt == "" && len(o.attach) == 0 {
		fmt.Fprint(stderr, "zagent: nothing to do: give --prompt, --prompt-file or a positional prompt, or open an interface with --ui cli|web.\n(run 'zagent help' for usage)\n")
		return exitUsage
	}
	home, env, cfg, ok := loadHome(o.home, stderr)
	if !ok {
		return exitUsage
	}
	workspace, err := resolveWorkspace(o.workspace)
	if err != nil {
		fmt.Fprintf(stderr, "zagent: %v\n", err)
		return exitUsage
	}
	sessionID, resumed, err := pickSession(o, home, workspace)
	if err != nil {
		fmt.Fprintf(stderr, "zagent: %v\n", err)
		return exitUsage
	}
	attach, err := absAttachments(o.attach)
	if err != nil {
		fmt.Fprintf(stderr, "zagent: %v\n", err)
		return exitUsage
	}

	switch o.ui {
	case "web":
		return runWeb(o, home, workspace, sessionID, prompt, attach, stdout, stderr)
	case "cli":
		return runCLI(o, home, workspace, sessionID, prompt, attach, stdout, stderr)
	}

	// headless: reuse a live engine for this session, or host one right here
	var be backend
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := home.SessionDir(sessionID)
	if c, aerr := control.Attach(dir); aerr == nil {
		if _, serr := c.State(ctx); serr == nil {
			be = remoteBackend{c: c, maxWait: time.Duration(cfg.Agent.MaxBackgroundWaitMinutes) * time.Minute}
			if !o.quiet {
				fmt.Fprintf(stderr, "zagent %s: session %s is already running (pid %d); sending the prompt to it\n", version, sessionID, c.PID)
			}
		}
	}
	if be == nil {
		h, err := newHost(home, env, cfg, workspace, sessionID, "headless")
		if err != nil {
			var locked *session.ErrLocked
			if errors.As(err, &locked) {
				fmt.Fprintf(stderr, "zagent: session %q is busy: %v.\nWait for it to finish, or use another --session name.\n", sessionID, locked)
				return exitLocked
			}
			fmt.Fprintf(stderr, "zagent: %v\n", err)
			return exitUsage
		}
		be = localBackend{h}
		go func() { // a client asked this engine process to end (/quit, POST /api/shutdown)
			select {
			case <-h.quit:
				fmt.Fprintln(stderr, "zagent: shutdown requested by an attached client")
				h.eng.Stop()
				cancel()
			case <-ctx.Done():
			}
		}()
		if !o.quiet {
			verb := "new session"
			if resumed {
				verb = "resuming session"
			}
			fmt.Fprintf(stderr, "zagent %s: %s %s  (model %s, workspace %s)\n", version, verb, sessionID, env.Model, workspace)
			fmt.Fprintf(stderr, "  attach another interface any time: zagent --session %s --ui cli|web\n", sessionID)
		}
	}
	defer be.close()

	startSeq := be.lastSeq()
	stopProgress := be.progress(o, stderr)
	defer stopProgress()
	interrupted := watchInterrupt(be, cancel, stderr)

	if err := be.submit(ctx, prompt, attach); err != nil {
		fmt.Fprintf(stderr, "zagent: %v\n", err)
		return exitFailed
	}
	waitErr := be.waitIdle(ctx)
	stopProgress()

	events := be.eventsSince(startSeq)
	status := lastStatus(events)
	printResult(o, stdout, events)
	if !o.quiet {
		c := be.counters()
		fmt.Fprintf(stderr, "zagent: %s — %d request(s), context %d tokens, total %d prompt / %d answer%s\n", status.State, c.Requests, c.Context, c.Prompt, c.Completion, map[bool]string{true: " (estimated)", false: ""}[c.Estimated])
	}
	switch {
	case interrupted():
		return exitInterrupted
	case waitErr != nil && !errors.Is(waitErr, context.Canceled):
		fmt.Fprintf(stderr, "zagent: %v\n", waitErr)
		return exitFailed
	case status.State == "error":
		fmt.Fprintf(stderr, "zagent: the turn failed: %s\n", status.Detail)
		return exitFailed
	}
	return exitOK
}

func lastStatus(events []session.Event) session.Event {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == session.KindStatus && events[i].State != "working" && events[i].State != "warning" {
			return events[i]
		}
	}
	return session.Event{}
}

// printResult writes what the user asked for to stdout: the final answer, the
// run's transcript (--output [N]), or its events as JSON lines (--json).
func printResult(o *options, stdout io.Writer, run []session.Event) {
	switch {
	case o.jsonOut:
		enc := json.NewEncoder(stdout)
		for _, ev := range run {
			_ = enc.Encode(ev)
		}
	case o.outputSet:
		lines := render.Transcript(run, render.Options{ToolOutputChars: 600})
		for _, l := range render.LastLines(lines, o.outputN) {
			fmt.Fprintln(stdout, l)
		}
	default:
		if a := render.FinalAnswer(run); a != "" {
			fmt.Fprintln(stdout, a)
		}
	}
}

// watchInterrupt stops the turn on the first Ctrl-C and exits on the second.
func watchInterrupt(be backend, cancel context.CancelFunc, stderr io.Writer) func() bool {
	var (
		mu   sync.Mutex
		hits int
	)
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		for range ch {
			mu.Lock()
			hits++
			n := hits
			mu.Unlock()
			if n == 1 {
				fmt.Fprintln(stderr, "\nzagent: stopping (press Ctrl-C again to exit immediately)…")
				be.stop()
				continue
			}
			cancel()
			be.close()
			os.Exit(exitInterrupted)
		}
	}()
	return func() bool { mu.Lock(); defer mu.Unlock(); return hits > 0 }
}
