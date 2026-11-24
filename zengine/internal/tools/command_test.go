package tools

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// sh returns a portable one-liner for the current OS.
func sh(unix, windows string) string {
	if runtime.GOOS == "windows" {
		return windows
	}
	return unix
}

func cmdEnv(t *testing.T, mode string) (*testEnv, *Registry) {
	te := newEnv(t)
	te.Cfg.Permissions.RunCommand = mode
	r := NewRegistry(te.Cfg.Tools)
	RegisterCommandTools(r)
	t.Cleanup(func() { procs(te.Env).KillAll() })
	return te, r
}

func TestRunCommandPermissions(t *testing.T) {
	ctx := context.Background()
	te, r := cmdEnv(t, "deny")
	if res := r.Invoke(ctx, te.Env, call("run_command", map[string]any{"command": "echo hi"})); !res.IsError || !strings.Contains(res.Output, "disabled") {
		t.Fatalf("deny: %+v", res)
	}
	te, r = cmdEnv(t, "ask")
	if res := r.Invoke(ctx, te.Env, call("run_command", map[string]any{"command": "echo hi"})); !res.IsError || !strings.Contains(res.Output, "needs approval") {
		t.Fatalf("ask without UI must explain how to enable: %+v", res)
	}
	var asked []ApprovalRequest
	te.Approve = func(_ context.Context, req ApprovalRequest) (bool, string) {
		asked = append(asked, req)
		return false, "not today"
	}
	if res := r.Invoke(ctx, te.Env, call("run_command", map[string]any{"command": "echo hi"})); !res.IsError || !strings.Contains(res.Output, "not today") || len(asked) != 1 || asked[0].Detail != "echo hi" {
		t.Fatalf("declined: %+v asked=%v", res, asked)
	}
	te.Approve = func(context.Context, ApprovalRequest) (bool, string) { return true, "" }
	if res := r.Invoke(ctx, te.Env, call("run_command", map[string]any{"command": "echo approved"})); res.IsError || !strings.Contains(res.Output, "approved") {
		t.Fatalf("approved: %+v", res)
	}
}

func TestRunCommandOutputExitCodeAndEnv(t *testing.T) {
	te, r := cmdEnv(t, "allow")
	ctx := context.Background()
	res := r.Invoke(ctx, te.Env, call("run_command", map[string]any{"command": "echo hello-world"}))
	if res.IsError || !strings.Contains(res.Output, "hello-world") || !strings.Contains(res.Output, "[exit code 0]") {
		t.Fatalf("echo: %+v", res)
	}
	res = r.Invoke(ctx, te.Env, call("run_command", map[string]any{"command": sh("exit 7", "exit 7")}))
	if !res.IsError || !strings.Contains(res.Output, "[exit code 7]") {
		t.Fatalf("exit code: %+v", res)
	}
	res = r.Invoke(ctx, te.Env, call("run_command", map[string]any{"command": sh("echo $ZAGENT_WORKSPACE", "echo $env:ZAGENT_WORKSPACE")}))
	if !strings.Contains(res.Output, te.Workspace) {
		t.Fatalf("env var missing: %+v", res)
	}
	res = r.Invoke(ctx, te.Env, call("run_command", map[string]any{"command": sh("pwd", "(Get-Location).Path"), "cwd": "."}))
	if !strings.Contains(strings.ToLower(res.Output), strings.ToLower(te.Workspace)) {
		t.Fatalf("cwd: %+v", res)
	}
	// stderr is merged and unicode survives
	res = r.Invoke(ctx, te.Env, call("run_command", map[string]any{"command": sh("echo 'é✓' ; echo oops 1>&2", "Write-Output 'é✓'; [Console]::Error.WriteLine('oops')")}))
	if !strings.Contains(res.Output, "é✓") || !strings.Contains(res.Output, "oops") {
		t.Fatalf("stderr/unicode: %q", res.Output)
	}
	if res = r.Invoke(ctx, te.Env, call("run_command", map[string]any{"command": "  "})); !res.IsError {
		t.Fatalf("empty command: %+v", res)
	}
	if res = r.Invoke(ctx, te.Env, call("run_command", map[string]any{"command": "echo x", "cwd": ".."})); !res.IsError || !strings.Contains(res.Output, "outside the workspace") {
		t.Fatalf("cwd escape: %+v", res)
	}
}

var sleep5 = sh("sleep 5", "Start-Sleep -Seconds 5")

func TestBackgroundTaskLifecycle(t *testing.T) {
	te, r := cmdEnv(t, "allow")
	ctx := context.Background()
	var mu sync.Mutex
	var exits []ProcInfo
	procs(te.Env).OnExit = func(i ProcInfo) { mu.Lock(); exits = append(exits, i); mu.Unlock() }

	// slow command: moved to the background after wait_seconds
	start := time.Now()
	res := r.Invoke(ctx, te.Env, call("run_command", map[string]any{"command": sh("echo started; sleep 3; echo finished", "Write-Output started; Start-Sleep -Seconds 3; Write-Output finished"), "wait_seconds": 1}))
	if res.IsError || !strings.Contains(res.Output, "moved to the background as task #1") || !strings.Contains(res.Output, "started") || time.Since(start) > 3*time.Second {
		t.Fatalf("auto-background: %+v after %s", res, time.Since(start))
	}
	list := r.Invoke(ctx, te.Env, call("terminal", map[string]any{"action": "list"}))
	if !strings.Contains(list.Output, "#1") || !strings.Contains(list.Output, "running") {
		t.Fatalf("list: %s", list.Output)
	}
	res = r.Invoke(ctx, te.Env, call("terminal", map[string]any{"action": "wait", "id": 1, "seconds": 10}))
	if !strings.Contains(res.Output, "finished") || !strings.Contains(res.Output, "exited with code 0") {
		t.Fatalf("wait: %+v", res)
	}
	if strings.Contains(res.Output, "started") {
		t.Fatalf("output must only return unseen lines, got %q", res.Output)
	}
	res = r.Invoke(ctx, te.Env, call("terminal", map[string]any{"action": "output", "id": 1}))
	if !strings.Contains(res.Output, "(no new output)") {
		t.Fatalf("cursor: %q", res.Output)
	}
	res = r.Invoke(ctx, te.Env, call("terminal", map[string]any{"action": "output", "id": 1, "all": true}))
	if !strings.Contains(res.Output, "started") || !strings.Contains(res.Output, "finished") {
		t.Fatalf("all=true: %q", res.Output)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	n := len(exits)
	mu.Unlock()
	if n != 1 || exits[0].ID != 1 || !strings.Contains(ExitNote(exits[0]), "background task #1 finished (exit code 0)") {
		t.Fatalf("exit notification: %+v", exits)
	}
}

func TestForegroundCommandsDoNotNotify(t *testing.T) {
	te, r := cmdEnv(t, "allow")
	called := make(chan ProcInfo, 1)
	procs(te.Env).OnExit = func(i ProcInfo) { called <- i }
	r.Invoke(context.Background(), te.Env, call("run_command", map[string]any{"command": "echo quick"}))
	select {
	case i := <-called:
		t.Fatalf("a foreground command must not trigger a background note: %+v", i)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestKillStopsTheWholeProcessTree(t *testing.T) {
	te, r := cmdEnv(t, "allow")
	ctx := context.Background()
	res := r.Invoke(ctx, te.Env, call("run_command", map[string]any{"command": sleep5, "background": true}))
	if res.IsError || !strings.Contains(res.Output, "task #1") {
		t.Fatalf("background: %+v", res)
	}
	start := time.Now()
	res = r.Invoke(ctx, te.Env, call("terminal", map[string]any{"action": "kill", "id": 1}))
	if !strings.Contains(res.Output, "stopped") || time.Since(start) > 4*time.Second {
		t.Fatalf("kill: %+v in %s", res, time.Since(start))
	}
	if p, _ := procs(te.Env).Get(1); p.Info().Running || !p.Info().Killed {
		t.Fatalf("state after kill: %+v", p.Info())
	}
}

func TestCommandTimeoutKills(t *testing.T) {
	te, r := cmdEnv(t, "allow")
	start := time.Now()
	res := r.Invoke(context.Background(), te.Env, call("run_command", map[string]any{"command": sleep5, "timeout_seconds": 1, "wait_seconds": 10}))
	if !res.IsError || !strings.Contains(res.Output, "timeout reached") || time.Since(start) > 4*time.Second {
		t.Fatalf("timeout: %+v after %s", res, time.Since(start))
	}
}

func TestCancelKillsForegroundCommand(t *testing.T) {
	te, r := cmdEnv(t, "allow")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	start := time.Now()
	res := r.Invoke(ctx, te.Env, call("run_command", map[string]any{"command": sleep5, "wait_seconds": 30}))
	if !res.IsError || time.Since(start) > 3*time.Second {
		t.Fatalf("cancel: %+v after %s", res, time.Since(start))
	}
	time.Sleep(300 * time.Millisecond)
	if procs(te.Env).Running() != 0 {
		t.Fatal("cancelled command left a process running")
	}
}

func TestTerminalInputAndErrors(t *testing.T) {
	te, r := cmdEnv(t, "allow")
	ctx := context.Background()
	cat := sh("cat", `$input | ForEach-Object { "got: $_" }`)
	res := r.Invoke(ctx, te.Env, call("run_command", map[string]any{"command": cat, "background": true}))
	if res.IsError {
		t.Fatalf("start: %+v", res)
	}
	res = r.Invoke(ctx, te.Env, call("terminal", map[string]any{"action": "input", "id": 1, "input": "ping"}))
	if res.IsError || !strings.Contains(res.Output, "Input sent") {
		t.Fatalf("input: %+v", res)
	}
	if runtime.GOOS != "windows" && !strings.Contains(res.Output, "ping") { // PowerShell buffers until stdin closes
		t.Fatalf("echo from cat: %q", res.Output)
	}
	for _, bad := range []map[string]any{{"action": "output"}, {"action": "output", "id": 99}, {"action": "input", "id": 1}} {
		if res = r.Invoke(ctx, te.Env, call("terminal", bad)); !res.IsError {
			t.Fatalf("expected error for %v: %+v", bad, res)
		}
	}
	if res = r.Invoke(ctx, te.Env, call("terminal", map[string]any{"action": "list"})); res.IsError {
		t.Fatalf("list: %+v", res)
	}
}

func TestOutputRingBufferDiscardsOldestAndReportsLoss(t *testing.T) {
	p := &Proc{changed: make(chan struct{})}
	chunk := strings.Repeat("x", 1<<20)
	for i := 0; i < 6; i++ {
		p.Write([]byte(chunk))
	}
	data, next, lost := p.Output(0)
	if len(data) != procBufferCap || next != 6<<20 || !lost {
		t.Fatalf("ring: len=%d next=%d lost=%v", len(data), next, lost)
	}
	if again, _, lost2 := p.Output(next); len(again) != 0 || lost2 {
		t.Fatalf("reading at the end: %d %v", len(again), lost2)
	}
}

func TestCommandsNeverSeeTheEnginesOwnCredentials(t *testing.T) {
	t.Setenv("ZAGENT_API_KEY", "sk-super-secret-value")
	t.Setenv("ZAGENT_SOME_TOKEN", "tok-secret-value")
	t.Setenv("ZAGENT_UI_THEME", "dark") // not a secret: must survive
	te, r := cmdEnv(t, "allow")
	dump := sh("env", "Get-ChildItem Env: | ForEach-Object { $_.Name + '=' + $_.Value }")
	res := r.Invoke(context.Background(), te.Env, call("run_command", map[string]any{"command": dump}))
	if strings.Contains(res.Output, "sk-super-secret-value") || strings.Contains(res.Output, "tok-secret-value") {
		t.Fatalf("the engine's credentials leaked into a command's environment:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "ZAGENT_UI_THEME=dark") || !strings.Contains(res.Output, "ZAGENT_WORKSPACE=") {
		t.Fatalf("non-secret ZAGENT_ variables and the ones the engine sets must remain:\n%s", res.Output)
	}
}

func TestScrubEnv(t *testing.T) {
	in := []string{"PATH=/bin", "ZAGENT_API_KEY=a", "zagent_access_token=b", "ZAGENT_HOME=/h", "MY_API_KEY=keep", "ZAGENT_DB_PASSWORD=p"}
	got := strings.Join(scrubEnv(in), ",")
	if got != "PATH=/bin,ZAGENT_HOME=/h,MY_API_KEY=keep" {
		t.Fatalf("scrubbed: %s", got)
	}
}
