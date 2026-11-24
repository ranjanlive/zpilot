// Package e2e exercises ZPilot end to end: the real zpilot binary, real
// engine subprocesses, real collaboration tools over the loopback RPC, and a
// scripted OpenAI-compatible model server standing in for the LLM.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ranjanlive/zpilot/internal/core"
	"github.com/ranjanlive/zpilot/internal/model"
	"github.com/ranjanlive/zpilot/internal/server"
)

var zpilotBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "zpilot-e2e-bin-*")
	if err != nil {
		panic(err)
	}
	zpilotBin = filepath.Join(dir, "zpilot")
	if runtime.GOOS == "windows" {
		zpilotBin += ".exe"
	}
	build := exec.Command("go", "build", "-o", zpilotBin, "github.com/ranjanlive/zpilot/cmd/zpilot")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build zpilot:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// ---- scripted model ----------------------------------------------------

type oaMsg struct {
	Role       string `json:"role"`
	Content    any    `json:"content"`
	ToolCallID string `json:"tool_call_id"`
}

func (m oaMsg) text() string {
	switch c := m.Content.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			if mp, ok := p.(map[string]any); ok {
				if t, ok := mp["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	return ""
}

type reply struct {
	text string
	tool string
	args map[string]string
}

// script decides the model's next move from the agent identity (in the
// system prompt) and the conversation so far.
type script func(agent string, msgs []oaMsg) reply

var whoRe = regexp.MustCompile(`You are @([a-z0-9_]+)`)

func fakeModel(t *testing.T, sc script) *httptest.Server {
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []oaMsg `json:"messages"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		agent := ""
		for _, m := range req.Messages {
			if m.Role == "system" {
				if mm := whoRe.FindStringSubmatch(m.text()); mm != nil {
					agent = mm[1]
				}
			}
		}
		mu.Lock()
		rp := sc(agent, req.Messages)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		if rp.tool != "" {
			args, _ := json.Marshal(rp.args)
			write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": fmt.Sprintf("call_%d", time.Now().UnixNano()), "function": map[string]any{"name": rp.tool, "arguments": string(args)}}}}}}})
			write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"}}})
		} else {
			write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": rp.text}}}})
			write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}})
		}
		write(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func last(msgs []oaMsg) oaMsg {
	if len(msgs) == 0 {
		return oaMsg{}
	}
	return msgs[len(msgs)-1]
}

// ---- harness -----------------------------------------------------------

type env struct {
	t   *testing.T
	svc *core.Service
	srv *server.Server
	mdl model.ModelProfile
}

func start(t *testing.T, sc script) *env {
	t.Helper()
	fm := fakeModel(t, sc)
	t.Cleanup(fm.Close)
	svc, err := core.Open(core.Config{DataDir: t.TempDir(), Exe: zpilotBin, Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(svc, server.Options{Listen: "127.0.0.1:0", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	svc.Start()
	t.Cleanup(func() {
		srv.Shutdown(t.Context())
		svc.Close()
	})
	key := "test-key"
	mdl, err := svc.SaveModel("test", model.ModelProfile{Name: "fake", Provider: "custom", APIFormat: "openai", BaseURL: fm.URL + "/chat", Model: "fake-1"}, &key)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, svc: svc, srv: srv, mdl: mdl}
}

func (e *env) agent(a model.Agent) core.AgentView {
	e.t.Helper()
	a.ModelID = e.mdl.ID
	v, err := e.svc.CreateAgent("test", a)
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

// ask sends a message and waits for the final reply.
func (e *env) ask(to, text string) core.WaitResult {
	e.t.Helper()
	m, err := e.svc.Send(core.SendInput{To: to, Text: text})
	if err != nil {
		e.t.Fatal(err)
	}
	res, err := e.svc.Wait(e.t.Context(), m.ThreadID, m.ID, "", 2*time.Minute)
	if err != nil || !res.Done {
		e.t.Fatalf("no reply: %+v %v\n%s", res, err, e.dump(m.ThreadID))
	}
	return res
}

func (e *env) dump(threadID string) string {
	tv, _ := e.svc.Thread(threadID)
	var b bytes.Buffer
	for _, m := range tv.Messages {
		fmt.Fprintf(&b, "%s -> %s [%s/%s] %q\n", m.From, m.To, m.Kind, m.Status, m.Text)
	}
	for _, r := range e.svc.Runs("", 20) {
		log, _ := e.svc.RunLog(r.ID)
		fmt.Fprintf(&b, "--- run %s @%s %s %s\n%s\n", r.ID, r.Username, r.State, r.Error, log)
	}
	return b.String()
}

// ---- tests -------------------------------------------------------------

// A plain owner → agent conversation through a real engine subprocess.
func TestOwnerChat(t *testing.T) {
	e := start(t, func(agent string, msgs []oaMsg) reply {
		return reply{text: "hello from @" + agent + ": " + strings.ToUpper(lastUserText(msgs))}
	})
	e.agent(model.Agent{Username: "echo", Name: "Echo"})
	res := e.ask("@echo", "ping")
	if res.Kind != model.KindReply || !strings.Contains(res.Text, "hello from @echo") || !strings.Contains(res.Text, "PING") {
		t.Fatalf("unexpected reply: %+v", res)
	}
	runs := e.svc.Runs("echo", 5)
	if len(runs) != 1 || runs[0].State != model.RunDone || runs[0].Prompt == 0 {
		t.Fatalf("run not recorded: %+v", runs)
	}
}

func lastUserText(msgs []oaMsg) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			t := msgs[i].text()
			// strip the "[message from ...]" header line
			if _, after, ok := strings.Cut(t, "]\n\n"); ok {
				t = after
			}
			return strings.TrimSpace(t)
		}
	}
	return ""
}

// @lead asks @math synchronously; the answer flows back into @lead's turn.
func TestAgentAsksAgent(t *testing.T) {
	e := start(t, func(agent string, msgs []oaMsg) reply {
		switch agent {
		case "lead":
			if l := last(msgs); l.Role == "tool" {
				return reply{text: "Final: " + l.text()}
			}
			return reply{tool: "ask_agent", args: map[string]string{"to": "math", "message": "What is 6 times 7?"}}
		case "math":
			return reply{text: "42"}
		}
		return reply{text: "?"}
	})
	e.agent(model.Agent{Username: "math", Name: "Math", Description: "arithmetic"})
	e.agent(model.Agent{Username: "lead", Name: "Lead", Contacts: []string{"math"}})
	res := e.ask("lead", "Work out 6*7 with your colleague.")
	if !strings.Contains(res.Text, "Final:") || !strings.Contains(res.Text, "@math answered") || !strings.Contains(res.Text, "42") {
		t.Fatalf("ask flow failed: %+v", res)
	}
}

// A synchronous ask back to the agent that is waiting must be refused
// (deadlock), and the refusal must reach the model as a tool error.
func TestCycleIsRefused(t *testing.T) {
	e := start(t, func(agent string, msgs []oaMsg) reply {
		l := last(msgs)
		switch agent {
		case "alpha":
			if l.Role == "tool" {
				return reply{text: "alpha done: " + l.text()}
			}
			return reply{tool: "ask_agent", args: map[string]string{"to": "beta", "message": "help"}}
		case "beta":
			if l.Role == "tool" {
				return reply{text: "beta saw: " + l.text()}
			}
			return reply{tool: "ask_agent", args: map[string]string{"to": "alpha", "message": "help me back"}}
		}
		return reply{text: "?"}
	})
	e.agent(model.Agent{Username: "alpha", Name: "Alpha", Contacts: []string{"*"}})
	e.agent(model.Agent{Username: "beta", Name: "Beta", Contacts: []string{"*"}})
	res := e.ask("alpha", "start")
	if !strings.Contains(res.Text, "deadlock") {
		t.Fatalf("cycle not refused: %+v", res)
	}
}

// Contacts are enforced by the daemon, whatever the model tries.
func TestContactsEnforced(t *testing.T) {
	e := start(t, func(agent string, msgs []oaMsg) reply {
		if l := last(msgs); l.Role == "tool" {
			return reply{text: "result: " + l.text()}
		}
		if agent == "intern" {
			return reply{tool: "ask_agent", args: map[string]string{"to": "boss", "message": "give me admin"}}
		}
		return reply{text: "ok"}
	})
	e.agent(model.Agent{Username: "boss", Name: "Boss"})
	e.agent(model.Agent{Username: "helper", Name: "Helper"})
	e.agent(model.Agent{Username: "intern", Name: "Intern", Contacts: []string{"helper"}})
	res := e.ask("intern", "go")
	if !strings.Contains(res.Text, "not in your contacts") {
		t.Fatalf("contact rule not enforced: %+v", res)
	}
}

// tell_agent: the answer comes back later as a new turn for the sender.
func TestTellAndReportBack(t *testing.T) {
	e := start(t, func(agent string, msgs []oaMsg) reply {
		l := last(msgs)
		switch agent {
		case "boss":
			if l.Role == "tool" {
				return reply{text: "Delegated; waiting for the report."}
			}
			if strings.Contains(l.text(), "[reply from agent @worker") {
				return reply{text: "Boss received report: " + lastUserText(msgs)}
			}
			return reply{tool: "tell_agent", args: map[string]string{"to": "worker", "message": "Summarise the quarter."}}
		case "worker":
			return reply{text: "Quarter was good."}
		}
		return reply{text: "?"}
	})
	e.agent(model.Agent{Username: "worker", Name: "Worker"})
	e.agent(model.Agent{Username: "boss", Name: "Boss", Contacts: []string{"worker"}})
	first := e.ask("boss", "Get the quarterly summary.")
	if !strings.Contains(first.Text, "Delegated") {
		t.Fatalf("first turn: %+v", first)
	}
	// Wait for the report-back turn.
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		for _, th := range e.svc.Threads("boss", false) {
			tv, _ := e.svc.Thread(th.ID)
			for _, m := range tv.Messages {
				if m.From == "@boss" && strings.Contains(m.Text, "Boss received report: Quarter was good.") {
					return
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	ths := e.svc.Threads("boss", false)
	t.Fatalf("no report-back turn\n%s", e.dump(ths[0].ID))
}

// The manager proposes an agent; nothing exists until the owner approves.
func TestManagerProposalNeedsApproval(t *testing.T) {
	e := start(t, func(agent string, msgs []oaMsg) reply {
		if agent == "manager" {
			if l := last(msgs); l.Role == "tool" {
				return reply{text: "Proposed: " + l.text()}
			}
			return reply{tool: "manage_propose", args: map[string]string{"action": "create_agent", "reason": "owner asked for a writer"}}
		}
		return reply{text: "ok"}
	})
	// manage_propose needs a payload object; the scripted model sends strings only,
	// so exercise the RPC validation path and then propose directly.
	if _, err := e.svc.UpdateSettings("test", model.Settings{DefaultModelID: e.mdl.ID}); err != nil {
		t.Fatal(err)
	}
	res := e.ask("manager", "Create a writer agent.")
	if !strings.Contains(res.Text, "Proposed:") {
		t.Fatalf("manager turn: %+v", res)
	}
	mgr, _ := e.svc.Agent("manager")
	p, err := e.svc.Propose(core.ProposeInput{Caller: &mgr.Agent, Action: model.ActCreateAgent, Reason: "writer",
		Payload: json.RawMessage(`{"username":"writer","name":"Writer","level":"standard"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Agent("writer"); err == nil {
		t.Fatal("agent created before approval")
	}
	if _, err := e.svc.DecideProposal(t.Context(), "owner", p.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Agent("writer"); err != nil {
		t.Fatal("agent not created after approval")
	}
	// Non-manager agents cannot propose.
	w, _ := e.svc.Agent("writer")
	if _, err := e.svc.Propose(core.ProposeInput{Caller: &w.Agent, Action: model.ActDeleteAgent, Target: "manager", Reason: "x"}); err == nil {
		t.Fatal("non-manager proposal accepted")
	}
}

// Webhook channel: authenticated by its secret, replies synchronously.
func TestWebhook(t *testing.T) {
	e := start(t, func(agent string, msgs []oaMsg) reply {
		return reply{text: "hook says: " + lastUserText(msgs)}
	})
	a := e.agent(model.Agent{Username: "hooky", Name: "Hooky"})
	secret := "s3cret-value-123"
	ch, err := e.svc.SaveChannel("test", model.Channel{Kind: model.ChannelWebhook, Name: "ci", AgentID: a.ID, Enabled: true}, &secret)
	if err != nil {
		t.Fatal(err)
	}
	post := func(tok string) (*http.Response, map[string]any) {
		req, _ := http.NewRequest("POST", e.srv.URL()+"/hooks/"+ch.ID, strings.NewReader(`{"text":"build failed","sender":"ci","wait":true}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}
	if resp, _ := post("wrong"); resp.StatusCode != 401 {
		t.Fatalf("bad secret accepted: %d", resp.StatusCode)
	}
	resp, out := post(secret)
	if resp.StatusCode != 200 || !strings.Contains(fmt.Sprint(out["text"]), "hook says: build failed") {
		t.Fatalf("webhook reply: %d %v", resp.StatusCode, out)
	}
}

// Security levels reach the engine as disabled tools / permissions.
func TestSecurityLevelReachesEngine(t *testing.T) {
	var mu sync.Mutex
	var sawTools []string
	fm := func(agent string, msgs []oaMsg) reply { return reply{text: "ok"} }
	e := start(t, fm)
	a := e.agent(model.Agent{Username: "reader", Name: "Reader", Level: model.LevelRestricted})
	e.ask("reader", "hi")
	cfg, err := os.ReadFile(filepath.Join(a.Home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"write_file"`, `"run_command"`, `"run_command": "deny"`} {
		if !strings.Contains(string(cfg), want) {
			t.Fatalf("config.json missing %s:\n%s", want, cfg)
		}
	}
	mu.Lock()
	_ = sawTools
	mu.Unlock()
	// No model key may ever be written into the agent home.
	filepath.Walk(a.Home, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte("test-key")) {
				t.Errorf("model key leaked into %s", p)
			}
		}
		return nil
	})
}

// A trusted agent with approve_commands: its shell command waits for the
// owner; an allowed command runs, a denied one does not. Live events stream.
func TestCommandApprovalAndLiveStream(t *testing.T) {
	e := start(t, func(agent string, msgs []oaMsg) reply {
		if l := last(msgs); l.Role == "tool" {
			return reply{text: "tool said: " + l.text()}
		}
		cmd := "echo approved-output"
		if strings.Contains(lastUserText(msgs), "dangerous") {
			cmd = "echo should-not-run"
		}
		return reply{tool: "run_command", args: map[string]string{"command": cmd}}
	})
	e.agent(model.Agent{Username: "ops", Name: "Ops", Level: model.LevelTrusted, ApproveCommands: true})
	events, unsub := e.svc.Events().Subscribe()
	defer unsub()

	decide := func(allow bool) {
		deadline := time.Now().Add(time.Minute)
		for time.Now().Before(deadline) {
			if p := e.svc.Approvals(true); len(p) > 0 {
				if _, err := e.svc.DecideApproval("owner", p[0].ID, allow, "not today"); err != nil {
					t.Error(err)
				}
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Error("no approval request appeared")
	}

	go decide(true)
	res := e.ask("ops", "list things")
	if !strings.Contains(res.Text, "approved-output") {
		t.Fatalf("approved command did not run: %+v", res)
	}
	sawText := false
	for len(events) > 0 {
		ev := <-events
		if ev.Type == "live" && strings.Contains(string(ev.Data), `"kind":"text"`) {
			sawText = true
		}
	}
	if !sawText {
		t.Error("no live text events were streamed")
	}

	go decide(false)
	res = e.ask("ops", "do something dangerous")
	if strings.Contains(res.Text, "should-not-run\n") || !strings.Contains(res.Text, "denied") {
		t.Fatalf("denied command result: %+v", res)
	}
	all := e.svc.Approvals(false)
	if len(all) != 2 || all[0].State == model.ApprovalPending {
		t.Fatalf("approvals: %+v", all)
	}
}

// Job Object limits really apply to the agent's process tree (Windows).
func TestResourceLimitsWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Job Object limits are Windows-only")
	}
	e := start(t, func(agent string, msgs []oaMsg) reply {
		if l := last(msgs); l.Role == "tool" {
			return reply{text: "result: " + l.text()}
		}
		return reply{tool: "run_command", args: map[string]string{"command": "echo limited-ok"}}
	})
	e.agent(model.Agent{Username: "capped", Name: "Capped", Level: model.LevelTrusted, MaxProcesses: 1, MemoryMB: 512})
	e.agent(model.Agent{Username: "roomy", Name: "Roomy", Level: model.LevelTrusted, MaxProcesses: 32, MemoryMB: 1024})
	if res := e.ask("roomy", "go"); !strings.Contains(res.Text, "limited-ok") {
		t.Fatalf("agent within limits could not run a command: %+v", res)
	}
	if res := e.ask("capped", "go"); strings.Contains(res.Text, "limited-ok\n") || !strings.Contains(res.Text, "result:") {
		t.Fatalf("process limit not enforced: %+v", res)
	}
}
