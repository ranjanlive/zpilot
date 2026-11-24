package control

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/config"
	"github.com/ranjanlive/zpilot/zengine/internal/engine"
	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/provider"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
)

// scriptModel answers from a list of replies; "<block>" waits for a cancel.
type scriptModel struct {
	mu      sync.Mutex
	replies []string
	started chan struct{}
	n       int
}

func (m *scriptModel) Format() string { return "gemini" }
func (m *scriptModel) Build(req provider.Request) (*provider.Built, error) {
	return &provider.Built{Body: []byte("{}"), Format: "gemini"}, nil
}
func (m *scriptModel) Send(ctx context.Context, _ *provider.Built, sink func(provider.Delta)) (*provider.Response, error) {
	m.mu.Lock()
	var reply string
	if m.n < len(m.replies) {
		reply = m.replies[m.n]
	}
	m.n++
	m.mu.Unlock()
	if reply == "<block>" {
		select {
		case m.started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, &provider.Error{Kind: provider.ErrCanceled, Message: "cancelled"}
	}
	if sink != nil {
		sink(provider.Delta{Kind: provider.DeltaText, Text: reply})
	}
	msg := llm.Message{Role: llm.RoleAssistant, FinishReason: llm.FinishStop, Parts: []llm.Part{{Type: llm.PartText, Text: reply}}}
	return &provider.Response{Message: msg, Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}, nil
}

type rig struct {
	eng      *engine.Engine
	srv      *Server
	cli      *Client
	home     config.Home
	model    *scriptModel
	shutdown chan struct{}
}

func newRig(t *testing.T, replies ...string) *rig {
	t.Helper()
	root := t.TempDir()
	home := config.Home{Root: filepath.Join(root, "home")}
	if _, err := home.Init(); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(root, "ws")
	os.MkdirAll(ws, 0o755)
	cfg := config.Defaults()
	cfg.Permissions.RunCommand = "allow"
	model := &scriptModel{replies: replies, started: make(chan struct{}, 1)}
	eng, err := engine.New(engine.Options{Home: home, Cfg: cfg, Env: config.Env{Provider: "gemini", Model: "m", Caps: llm.Capabilities{Vision: true}},
		Workspace: ws, SessionID: "s1", Model: model, Role: "test"})
	if err != nil {
		t.Fatal(err)
	}
	eng.Start()
	r := &rig{eng: eng, home: home, model: model, shutdown: make(chan struct{}, 1)}
	r.srv, err = Serve(eng, Options{Version: "test", OnShutdown: func() { r.shutdown <- struct{}{} }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.srv.Close(); eng.Close() })
	r.cli, err = Attach(eng.Store().Dir)
	if err != nil {
		t.Fatalf("attach via the lock file: %v", err)
	}
	return r
}

func (r *rig) waitIdle(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.eng.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAuthHostAndOriginChecks(t *testing.T) {
	r := newRig(t)
	get := func(path string, mutate func(*http.Request)) int {
		req, _ := http.NewRequest("GET", "http://"+r.srv.Addr()+path, nil)
		if mutate != nil {
			mutate(req)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	bearer := func(tok string) func(*http.Request) {
		return func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+tok) }
	}
	if c := get("/api/state", nil); c != 401 {
		t.Errorf("no token: %d", c)
	}
	if c := get("/api/state", bearer("wrong")); c != 401 {
		t.Errorf("wrong token: %d", c)
	}
	if c := get("/api/state", bearer(r.srv.Token())); c != 200 {
		t.Errorf("right token: %d", c)
	}
	if c := get("/api/state?token="+r.srv.Token(), nil); c != 200 {
		t.Errorf("token in the query (EventSource): %d", c)
	}
	if c := get("/api/state", func(req *http.Request) { bearer(r.srv.Token())(req); req.Host = "evil.example:80" }); c != 403 {
		t.Errorf("DNS-rebinding host must be refused: %d", c)
	}
	if c := get("/api/state", func(req *http.Request) { bearer(r.srv.Token())(req); req.Header.Set("Origin", "https://evil.example") }); c != 403 {
		t.Errorf("foreign origin must be refused: %d", c)
	}
	if c := get("/api/state", func(req *http.Request) {
		bearer(r.srv.Token())(req)
		req.Header.Set("Origin", "http://"+r.srv.Addr())
	}); c != 200 {
		t.Errorf("same origin: %d", c)
	}
	if !strings.Contains(r.srv.URL(), "#token="+r.srv.Token()) {
		t.Error("the web URL must carry the token in the fragment")
	}
}

func TestStateAndLockEndpoint(t *testing.T) {
	r := newRig(t)
	st, err := r.cli.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Session != "s1" || st.Model != "m" || st.State != engine.StateIdle || st.Version != "test" || st.Role != "test" || st.PID != os.Getpid() {
		t.Fatalf("state: %+v", st)
	}
	li := session.ReadLock(r.eng.Store().Dir)
	if li == nil || li.Addr != r.srv.Addr() || li.Token != r.srv.Token() {
		t.Fatalf("lock endpoint: %+v", li)
	}
	if _, err := Attach(filepath.Join(t.TempDir(), "nothing")); err != ErrNotRunning {
		t.Fatalf("attach to a dead session: %v", err)
	}
}

func TestPromptStreamsAndReplays(t *testing.T) {
	r := newRig(t, "hello from the agent", "second answer")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var mu sync.Mutex
	var events []session.Event
	var liveText strings.Builder
	streamCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- r.cli.Stream(streamCtx, 0, func(e session.Event) { mu.Lock(); events = append(events, e); mu.Unlock() },
			func(l engine.Live) {
				if l.Kind == engine.LiveText {
					mu.Lock()
					liveText.WriteString(l.Text)
					mu.Unlock()
				}
			})
	}()
	if err := r.cli.Prompt(ctx, "hi there"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, e := range events {
			if e.Kind == session.KindStatus && e.State == "done" {
				return true
			}
		}
		return false
	})
	stop()
	if err := <-done; err != nil {
		t.Fatalf("a cancelled stream is not an error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if events[0].Kind != session.KindSystem || events[0].Seq != 1 {
		t.Fatalf("the stream must replay from the start, first event: %+v", events[0])
	}
	var sawUser, sawAgent bool
	for _, e := range events {
		if e.Kind == session.KindMessage && e.Message.Role == llm.RoleUser && e.Message.Text() == "hi there" {
			sawUser = true
		}
		if e.Kind == session.KindMessage && e.Message.Role == llm.RoleAssistant && e.Message.Text() == "hello from the agent" {
			sawAgent = true
		}
	}
	if !sawUser || !sawAgent {
		t.Fatalf("events missing (user=%v agent=%v)", sawUser, sawAgent)
	}
	if liveText.String() != "hello from the agent" {
		t.Fatalf("live text deltas: %q", liveText.String())
	}

	// reconnecting with the last seen seq delivers only what is new
	last := events[len(events)-1].Seq
	var more []session.Event
	var mmu sync.Mutex
	ctx2, cancel2 := context.WithCancel(ctx)
	go r.cli.Stream(ctx2, last, func(e session.Event) { mmu.Lock(); more = append(more, e); mmu.Unlock() }, nil)
	if err := r.cli.Prompt(ctx, "again"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		mmu.Lock()
		defer mmu.Unlock()
		return len(more) > 0 && more[len(more)-1].Kind == session.KindStatus && more[len(more)-1].State == "done"
	})
	cancel2()
	mmu.Lock()
	defer mmu.Unlock()
	if more[0].Seq != last+1 {
		t.Fatalf("resume must continue at seq %d, got %d", last+1, more[0].Seq)
	}
	// the history endpoint agrees with the stream
	evs, err := r.cli.Events(ctx, last)
	if err != nil || len(evs) == 0 || evs[0].Seq != last+1 {
		t.Fatalf("events since: %v %v", evs, err)
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDetachingNeverInterruptsTheAgent(t *testing.T) {
	r := newRig(t, "finished while nobody was watching")
	ctx := context.Background()
	streamCtx, detach := context.WithCancel(ctx)
	attached := make(chan struct{})
	go func() {
		r.cli.Stream(streamCtx, 0, func(e session.Event) {
			select {
			case <-attached:
			default:
				close(attached)
			}
		}, nil)
	}()
	<-attached
	if err := r.cli.Prompt(ctx, "work"); err != nil {
		t.Fatal(err)
	}
	detach() // the UI closes immediately
	r.waitIdle(t)
	events := r.eng.Store().Events()
	var final string
	for _, e := range events {
		if e.Kind == session.KindMessage && e.Message.Role == llm.RoleAssistant {
			final = e.Message.Text()
		}
	}
	if final != "finished while nobody was watching" {
		t.Fatalf("the agent must finish without a UI attached: %q", final)
	}
	// and a UI that attaches later sees the whole history
	evs, err := r.cli.Events(ctx, 0)
	if err != nil || len(evs) != len(events) {
		t.Fatalf("late attach: %d vs %d events (%v)", len(evs), len(events), err)
	}
}

func TestNotesCommandsAndShutdown(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.cli.Note(ctx, "the deadline moved to Friday"); err != nil {
		t.Fatal(err)
	}
	var notes int
	for _, e := range r.eng.Store().Events() {
		if e.Kind == session.KindNote && strings.Contains(e.Text, "Friday") {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("notes: %d", notes)
	}
	res, err := r.cli.Command(ctx, "/status")
	if err != nil || !res.Handled || !strings.Contains(res.Reply, "Session: s1") {
		t.Fatalf("command: %+v %v", res, err)
	}
	if res, _ = r.cli.Command(ctx, "/exit"); !res.Exit || res.Shutdown {
		t.Fatalf("/exit must only detach: %+v", res)
	}
	select {
	case <-r.shutdown:
		t.Fatal("/exit must not shut the engine down")
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := r.cli.Command(ctx, "/quit"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.shutdown:
	case <-time.After(5 * time.Second):
		t.Fatal("/quit must signal shutdown")
	}
	for _, bad := range []func() error{
		func() error { return r.cli.Note(ctx, "  ") },
		func() error { return r.cli.Prompt(ctx, "") },
	} {
		if err := bad(); err == nil {
			t.Fatal("empty input must be rejected")
		}
	}
}

func TestStopOverTheAPI(t *testing.T) {
	r := newRig(t, "<block>")
	ctx := context.Background()
	if err := r.cli.Prompt(ctx, "long job"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.model.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the model call never started")
	}
	st, _ := r.cli.State(ctx)
	if st.State != engine.StateWorking {
		t.Fatalf("state while working: %+v", st)
	}
	if err := r.cli.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	r.waitIdle(t)
	last := r.eng.Store().Events()
	var stopped bool
	for _, e := range last {
		stopped = stopped || (e.Kind == session.KindStatus && e.State == "stopped")
	}
	if !stopped {
		t.Fatal("the stop must be recorded")
	}
}

func TestUploadFeedsAnAttachment(t *testing.T) {
	r := newRig(t, "I see it")
	ctx := context.Background()
	local := filepath.Join(t.TempDir(), "my pic.png")
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, imageBytes.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := r.cli.Upload(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, r.home.ScratchDir()) || strings.ContainsAny(filepath.Base(path), " ?") {
		t.Fatalf("upload path: %s", path)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, imageBytes.Bytes()) {
		t.Fatalf("uploaded content: %q", b)
	}
	if err := r.cli.Prompt(ctx, "what is this?", path); err != nil {
		t.Fatal(err)
	}
	r.waitIdle(t)
	var hasMedia bool
	for _, e := range r.eng.Store().Events() {
		if e.Kind == session.KindMessage && e.Message.Role == llm.RoleUser {
			for _, p := range e.Message.Parts {
				hasMedia = hasMedia || (p.Type == llm.PartMedia && p.Media.Kind == llm.MediaImage)
			}
		}
	}
	if !hasMedia {
		t.Fatal("the uploaded file should arrive as an image attachment")
	}
}

func TestClientReportsAGoneEngine(t *testing.T) {
	r := newRig(t)
	cli := r.cli
	r.srv.Close()
	if _, err := cli.State(context.Background()); err == nil || !strings.Contains(err.Error(), ErrNotRunning.Error()) {
		t.Fatalf("a dead server must map to ErrNotRunning, got %v", err)
	}
}
