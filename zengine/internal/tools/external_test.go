package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
)

// TestExternalHelper is not a test: it is the third-party tool. The tests
// below register this very test binary as an external tool and drive it
// through the same manifest/stdin/env protocol real tools use.
func TestExternalHelper(t *testing.T) {
	mode := os.Getenv("ZTOOL_MODE")
	if mode == "" {
		t.Skip("helper process only")
	}
	stdin, _ := io.ReadAll(os.Stdin)
	out := os.Getenv("ZAGENT_OUTPUT_DIR")
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	switch mode {
	case "oversize_json":
		fmt.Print(`{"output":"`)
		for range 145 {
			fmt.Print(strings.Repeat("x", 64*1024))
		}
		fmt.Print(`"}`)
	case "echo":
		fmt.Printf("stdin=%s\n", strings.TrimSpace(string(stdin)))
		for _, k := range []string{"ZAGENT_WORKSPACE", "ZAGENT_SESSION", "ZAGENT_TOOL_NAME", "ZAGENT_CAPS", "CUSTOM_FLAG"} {
			fmt.Printf("%s=%s\n", k, os.Getenv(k))
		}
		fmt.Printf("output_dir_exists=%v\n", dirExists(out))
		cwd, _ := os.Getwd()
		fmt.Printf("cwd=%s\n", cwd)
	case "argsjson":
		fmt.Printf("last_arg=%s\n", os.Args[len(os.Args)-1])
	case "envargs":
		fmt.Printf("name=%s all=%s\n", os.Getenv("ZAGENT_ARG_NAME"), os.Getenv("ZAGENT_ARGS_JSON"))
	case "files":
		_ = os.WriteFile(filepath.Join(out, "chart.png"), imageBytes.Bytes(), 0o644)
		_ = os.WriteFile(filepath.Join(out, "notes.txt"), []byte("n"), 0o644)
		fmt.Println("saved two files")
	case "json":
		p := filepath.Join(out, "result.png")
		_ = os.WriteFile(p, imageBytes.Bytes(), 0o644)
		b, _ := json.Marshal(map[string]any{"output": "rendered ok", "media": []map[string]string{{"path": p}}})
		fmt.Println(string(b))
	case "json_error":
		fmt.Println(`{"output":"could not render: bad input","is_error":true}`)
	case "badjson":
		fmt.Println("this is not json")
	case "notify":
		f, _ := os.OpenFile(os.Getenv("ZAGENT_NOTIFY_FILE"), os.O_APPEND|os.O_WRONLY, 0o644)
		fmt.Fprintln(f, "halfway there")
		time.Sleep(500 * time.Millisecond)
		fmt.Fprintln(f, `{"text":"need your attention","wake":true}`)
		time.Sleep(500 * time.Millisecond)
		f.Close()
		fmt.Println("finished notifying")
	case "sleep":
		time.Sleep(10 * time.Second)
	case "bg":
		time.Sleep(700 * time.Millisecond)
		fmt.Println(`{"output":"background render complete"}`)
	case "argvs":
		fmt.Println(strings.Join(os.Args[2:], "|"))
	case "crash":
		fmt.Fprintln(os.Stderr, "Traceback: ModuleNotFoundError: no module named widgets")
		os.Exit(2)
	case "fail":
		fmt.Fprintln(os.Stderr, "boom: missing dependency")
		os.Exit(3)
	}
	os.Exit(0)
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// installTool writes a manifest for the helper into <home>/tools/<name>/.
func installTool(t *testing.T, te *testEnv, name, mode string, mutate func(*Manifest)) {
	t.Helper()
	m := Manifest{
		Name: name, Description: "test tool " + name,
		Parameters: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`),
		Command:    []string{os.Args[0], "-test.run=^TestExternalHelper$"},
		Env:        map[string]string{"ZTOOL_MODE": mode, "CUSTOM_FLAG": "{dir}"},
	}
	if mutate != nil {
		mutate(&m)
	}
	dir := filepath.Join(te.Home.ToolsDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, name+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func extRegistry(t *testing.T, te *testEnv) (*Registry, []string) {
	t.Helper()
	r := NewRegistry(te.Cfg.Tools)
	problems := RegisterExternal(r, te.Home.ToolsDir())
	t.Cleanup(func() { procs(te.Env).KillAll() })
	return r, problems
}

func TestLoadExternalValidation(t *testing.T) {
	te := newEnv(t)
	good := func(m *Manifest) {}
	installTool(t, te, "good_tool", "echo", good)
	installTool(t, te, "view_file", "echo", good) // reserved
	installTool(t, te, "no_desc", "echo", func(m *Manifest) { m.Description = " " })
	installTool(t, te, "no_cmd", "echo", func(m *Manifest) { m.Command = nil })
	installTool(t, te, "bad_input", "echo", func(m *Manifest) { m.Input = "carrier_pigeon" })
	installTool(t, te, "bad_name_x", "echo", func(m *Manifest) { m.Name = "Bad-Name" })
	installTool(t, te, "bad_schema", "echo", func(m *Manifest) { m.Parameters = json.RawMessage(`{"type":"array"}`) })
	installTool(t, te, "other_os", "echo", func(m *Manifest) { m.Platforms = []string{"plan9"} })
	installTool(t, te, "this_os", "echo", func(m *Manifest) { m.Platforms = []string{runtime.GOOS} })
	os.MkdirAll(filepath.Join(te.Home.ToolsDir(), "no_manifest"), 0o755) // a folder without a manifest is ignored
	os.WriteFile(filepath.Join(te.Home.ToolsDir(), "broken", "x"), nil, 0o644)
	os.MkdirAll(filepath.Join(te.Home.ToolsDir(), "broken"), 0o755)
	os.WriteFile(filepath.Join(te.Home.ToolsDir(), "broken", "broken.json"), []byte(`{"name": "broken", "typo_field": 1}`), 0o644)

	r, problems := extRegistry(t, te)
	if got := strings.Join(r.Names(), ","); got != "good_tool,this_os" {
		t.Fatalf("loaded tools: %s", got)
	}
	all := strings.Join(problems, "\n")
	for _, want := range []string{"reserved for a built-in", "description is required", "command is required", `input must be`, `name "Bad-Name" is invalid`, `"type": "object"`, "bad manifest"} {
		if !strings.Contains(all, want) {
			t.Errorf("problems lack %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "other_os") || strings.Contains(all, "no_manifest") {
		t.Errorf("platform-filtered and manifest-less folders are not problems:\n%s", all)
	}
}

func TestExternalToolProtocol(t *testing.T) {
	te := newEnv(t)
	installTool(t, te, "echoer", "echo", nil)
	r, problems := extRegistry(t, te)
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	res := r.Invoke(context.Background(), te.Env, call("echoer", map[string]any{"name": "zed"}))
	if res.IsError {
		t.Fatalf("run: %+v", res)
	}
	for _, want := range []string{`stdin={"name":"zed"}`, "ZAGENT_WORKSPACE=" + te.Workspace, "ZAGENT_SESSION=s1", "ZAGENT_TOOL_NAME=echoer", "ZAGENT_CAPS=vision=true,audio=false,pdf=true,video=false", "output_dir_exists=true"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output lacks %q:\n%s", want, res.Output)
		}
	}
	if !strings.Contains(res.Output, filepath.Join(te.Home.ToolsDir(), "echoer")) {
		t.Errorf("{dir} placeholder not expanded in env:\n%s", res.Output)
	}
	if !strings.Contains(strings.ToLower(res.Output), "cwd="+strings.ToLower(te.Workspace)) {
		t.Errorf("default cwd must be the workspace:\n%s", res.Output)
	}
	// the manifest schema is enforced by the registry before anything runs
	res = r.Invoke(context.Background(), te.Env, call("echoer", map[string]any{"nme": "x"}))
	if !res.IsError || !strings.Contains(res.Output, `missing required argument "name"`) {
		t.Fatalf("schema validation: %+v", res)
	}
}

func TestExternalInputModes(t *testing.T) {
	te := newEnv(t)
	installTool(t, te, "argmode", "argsjson", func(m *Manifest) { m.Input = "args_json" })
	installTool(t, te, "envmode", "envargs", func(m *Manifest) { m.Input = "env" })
	r, _ := extRegistry(t, te)
	res := r.Invoke(context.Background(), te.Env, call("argmode", map[string]any{"name": "q"}))
	if !strings.Contains(res.Output, `last_arg={"name":"q"}`) {
		t.Fatalf("args_json: %+v", res)
	}
	res = r.Invoke(context.Background(), te.Env, call("envmode", map[string]any{"name": "q"}))
	if !strings.Contains(res.Output, `name=q all={"name":"q"}`) {
		t.Fatalf("env: %+v", res)
	}
}

func TestExternalExitCodesAndStderr(t *testing.T) {
	te := newEnv(t)
	installTool(t, te, "failing", "fail", nil)
	r, _ := extRegistry(t, te)
	res := r.Invoke(context.Background(), te.Env, call("failing", map[string]any{"name": "x"}))
	if !res.IsError || !strings.Contains(res.Output, "boom: missing dependency") || !strings.Contains(res.Output, "[tool exited with code 3]") {
		t.Fatalf("failure must carry stderr and the exit code: %+v", res)
	}
}

func TestExternalJSONOutputAndMedia(t *testing.T) {
	te := newEnv(t)
	installTool(t, te, "renderer", "json", func(m *Manifest) { m.Output = "json" })
	installTool(t, te, "renderer_err", "json_error", func(m *Manifest) { m.Output = "json" })
	installTool(t, te, "renderer_bad", "badjson", func(m *Manifest) { m.Output = "json" })
	r, _ := extRegistry(t, te)
	ctx := context.Background()

	res := r.Invoke(ctx, te.Env, call("renderer", map[string]any{"name": "x"}))
	if res.IsError || !strings.Contains(res.Output, "rendered ok") || len(res.Media) != 1 || res.Media[0].Kind != llm.MediaImage {
		t.Fatalf("json result with media: %+v", res)
	}
	if !strings.Contains(res.Output, "Files written by the tool") {
		t.Fatalf("files in the output directory must be listed: %s", res.Output)
	}
	// the model cannot take images: the media is dropped with an explanation, the result still arrives
	te.Caps = llm.Capabilities{}
	res = r.Invoke(ctx, te.Env, call("renderer", map[string]any{"name": "x"}))
	if res.IsError || len(res.Media) != 0 || !strings.Contains(res.Output, "could not attach") || !strings.Contains(res.Output, "not supported by the configured model") {
		t.Fatalf("capability handling: %+v", res)
	}
	te.Caps = llm.Capabilities{Vision: true}

	if res = r.Invoke(ctx, te.Env, call("renderer_err", map[string]any{"name": "x"})); !res.IsError || !strings.Contains(res.Output, "could not render") {
		t.Fatalf("is_error: %+v", res)
	}
	if res = r.Invoke(ctx, te.Env, call("renderer_bad", map[string]any{"name": "x"})); !res.IsError || !strings.Contains(res.Output, "did not print valid JSON") {
		t.Fatalf("bad json: %+v", res)
	}
}

func TestExternalAttachOutputs(t *testing.T) {
	te := newEnv(t)
	installTool(t, te, "charter", "files", func(m *Manifest) { m.AttachOutputs = true })
	installTool(t, te, "charter_plain", "files", nil)
	r, _ := extRegistry(t, te)
	res := r.Invoke(context.Background(), te.Env, call("charter", map[string]any{"name": "x"}))
	if len(res.Media) != 1 || res.Media[0].Name != "chart.png" || !strings.Contains(res.Output, "notes.txt") {
		t.Fatalf("attach_outputs: %+v", res)
	}
	res = r.Invoke(context.Background(), te.Env, call("charter_plain", map[string]any{"name": "x"}))
	if len(res.Media) != 0 || !strings.Contains(res.Output, "chart.png") {
		t.Fatalf("without attach_outputs files are listed, not attached: %+v", res)
	}
}

func TestExternalTimeout(t *testing.T) {
	te := newEnv(t)
	installTool(t, te, "slowpoke", "sleep", func(m *Manifest) { m.TimeoutSeconds = 1 })
	r, _ := extRegistry(t, te)
	start := time.Now()
	res := r.Invoke(context.Background(), te.Env, call("slowpoke", map[string]any{"name": "x"}))
	if !res.IsError || !strings.Contains(res.Output, "timed out") || time.Since(start) > 6*time.Second {
		t.Fatalf("timeout: %+v after %s", res, time.Since(start))
	}
	time.Sleep(300 * time.Millisecond)
	if procs(te.Env).Running() != 0 {
		t.Fatal("a timed-out tool must not be left running")
	}
}

func TestExternalPermissionAsk(t *testing.T) {
	te := newEnv(t)
	installTool(t, te, "careful", "echo", func(m *Manifest) { m.Permission = "ask" })
	r, _ := extRegistry(t, te)
	ctx := context.Background()
	if res := r.Invoke(ctx, te.Env, call("careful", map[string]any{"name": "x"})); !res.IsError || !strings.Contains(res.Output, "needs approval") {
		t.Fatalf("no approver: %+v", res)
	}
	var asked ApprovalRequest
	te.Approve = func(_ context.Context, req ApprovalRequest) (bool, string) { asked = req; return false, "nope" }
	if res := r.Invoke(ctx, te.Env, call("careful", map[string]any{"name": "x"})); !res.IsError || !strings.Contains(res.Output, "nope") || asked.Tool != "careful" {
		t.Fatalf("declined: %+v asked=%+v", res, asked)
	}
	te.Approve = func(context.Context, ApprovalRequest) (bool, string) { return true, "" }
	if res := r.Invoke(ctx, te.Env, call("careful", map[string]any{"name": "x"})); res.IsError {
		t.Fatalf("approved: %+v", res)
	}
}

func TestExternalNotifyFileReachesTheSessionWhileRunning(t *testing.T) {
	te := newEnv(t)
	var mu sync.Mutex
	var plain, woke []string
	te.Notify = func(s string) { mu.Lock(); plain = append(plain, s); mu.Unlock() }
	te.NotifyWake = func(s string) { mu.Lock(); woke = append(woke, s); mu.Unlock() }
	installTool(t, te, "chatty", "notify", nil)
	r, _ := extRegistry(t, te)
	res := r.Invoke(context.Background(), te.Env, call("chatty", map[string]any{"name": "x"}))
	if res.IsError || !strings.Contains(res.Output, "finished notifying") {
		t.Fatalf("run: %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(plain) != 1 || plain[0] != "halfway there" || len(woke) != 1 || woke[0] != "need your attention" {
		t.Fatalf("notes: plain=%v woke=%v", plain, woke)
	}
}

func TestExternalBackgroundTool(t *testing.T) {
	te := newEnv(t)
	installTool(t, te, "renderbg", "bg", func(m *Manifest) { m.Output = "json"; m.Background = true })
	r, _ := extRegistry(t, te)
	exits := make(chan ProcInfo, 1)
	procs(te.Env).OnExit = func(i ProcInfo) { exits <- i }

	start := time.Now()
	res := r.Invoke(context.Background(), te.Env, call("renderbg", map[string]any{"name": "x"}))
	if res.IsError || !strings.Contains(res.Output, "background task #1") || time.Since(start) > 3*time.Second {
		t.Fatalf("a background tool returns at once: %+v after %s", res, time.Since(start))
	}
	if !strings.Contains(r.Specs()[0].Description, "Runs in the background") {
		t.Fatal("the model must be told the tool is asynchronous")
	}
	select {
	case i := <-exits:
		if !strings.Contains(i.Note, "background tool renderbg (task #1) finished") || !strings.Contains(i.Note, "background render complete") {
			t.Fatalf("exit note: %q", i.Note)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no exit announcement")
	}
}

func TestExternalToolDisabledByConfig(t *testing.T) {
	te := newEnv(t)
	te.Cfg.Tools.Disabled = []string{"echoer"}
	installTool(t, te, "echoer", "echo", nil)
	r, _ := extRegistry(t, te)
	if r.Has("echoer") {
		t.Fatal("tools.disabled applies to external tools too")
	}
	if res := r.Invoke(context.Background(), te.Env, call("echoer", map[string]any{"name": "x"})); !res.IsError || !strings.Contains(res.Output, "disabled by configuration") {
		t.Fatalf("%+v", res)
	}
}

func TestExternalRelativeArgumentsResolveAgainstTheToolDirectory(t *testing.T) {
	te := newEnv(t)
	installTool(t, te, "pathy", "argvs", func(m *Manifest) {
		m.Command = append(m.Command, "data.txt", "-flag", "missing.txt", "{dir}")
	})
	dir := filepath.Join(te.Home.ToolsDir(), "pathy")
	if err := os.WriteFile(filepath.Join(dir, "data.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, _ := extRegistry(t, te)
	res := r.Invoke(context.Background(), te.Env, call("pathy", map[string]any{"name": "x"}))
	if res.IsError {
		t.Fatalf("run: %+v", res)
	}
	line := strings.Split(strings.TrimSpace(strings.Split(res.Output, "\n")[0]), "|")
	// os.Args[2:] after the helper's own -test.run flag: [resolved data.txt, -flag, missing.txt, {dir} expanded]
	if len(line) < 4 || line[len(line)-4] != filepath.Join(dir, "data.txt") || line[len(line)-3] != "-flag" || line[len(line)-2] != "missing.txt" || line[len(line)-1] != dir {
		t.Fatalf("argument resolution: %q", line)
	}
}

func TestExternalCrashBeforeOutputShowsTheRealError(t *testing.T) {
	te := newEnv(t)
	installTool(t, te, "crasher", "crash", func(m *Manifest) { m.Output = "json" })
	r, _ := extRegistry(t, te)
	res := r.Invoke(context.Background(), te.Env, call("crasher", map[string]any{"name": "x"}))
	if !res.IsError || !strings.HasPrefix(res.Output, "Traceback: ModuleNotFoundError") || !strings.Contains(res.Output, "[tool crasher exited with code 2]") {
		t.Fatalf("the tool's own error must lead, then the exit code: %q", res.Output)
	}
}

func TestEveryExternalCallGetsItsOwnOutputFolder(t *testing.T) {
	te := newEnv(t)
	installTool(t, te, "charter", "files", nil)
	r, _ := extRegistry(t, te)
	seen := map[string]bool{}
	for i := 0; i < 5; i++ { // back to back: same second, and (via a fresh registry) the same per-process counter
		reg, _ := extRegistry(t, te)
		res := reg.Invoke(context.Background(), te.Env, call("charter", map[string]any{"name": "x"}))
		for _, line := range strings.Split(res.Output, "\n") {
			if strings.Contains(line, "chart.png") {
				dir := filepath.Dir(strings.TrimSpace(line))
				if seen[dir] {
					t.Fatalf("output folder %s was reused: files from an earlier call would leak into this result", dir)
				}
				seen[dir] = true
			}
		}
	}
	_ = r
	if len(seen) != 5 {
		t.Fatalf("expected 5 distinct output folders, got %d", len(seen))
	}
}
