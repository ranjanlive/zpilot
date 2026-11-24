package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/config"
	"github.com/ranjanlive/zpilot/zengine/internal/engine"
	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/tools"
)

const toolsUsage = `usage:
  zagent tools list [--home DIR]
        list the tools the agent would have, and any third-party tools that failed to load
  zagent tools run NAME [--args JSON] [--workspace DIR] [--wait SECONDS] [--home DIR]
        run one tool exactly as the agent would (same validation, environment variables,
        output folder and notify file) and print what the model would receive`

// cmdTools lets tool authors test a tool without involving a model.
func cmdTools(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, toolsUsage)
		return exitUsage
	}
	switch args[0] {
	case "list":
		return toolsList(args[1:], stdout, stderr)
	case "run":
		return toolsRun(args[1:], stdout, stderr)
	}
	fmt.Fprintln(stderr, toolsUsage)
	return exitUsage
}

func toolsList(args []string, stdout, stderr io.Writer) int {
	fs, homeFlag := homeFlagSet("tools list", stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	home, _, cfg, ok := loadHomeForTools(*homeFlag, stderr)
	if !ok {
		return exitUsage
	}
	registry, problems := engine.DefaultRegistry(cfg, home)
	external, _ := tools.LoadExternal(home.ToolsDir())
	isExternal := map[string]bool{}
	for _, t := range external {
		isExternal[t.Spec().Name] = true
	}
	specs := map[string]llm.ToolSpec{}
	for _, s := range registry.Specs() {
		specs[s.Name] = s
	}
	names := registry.Names()
	sort.Strings(names)
	for _, n := range names {
		kind := "built-in"
		if isExternal[n] {
			kind = "external"
		}
		fmt.Fprintf(stdout, "%-14s %-9s %s\n", n, kind, firstSentence(specs[n].Description))
	}
	fmt.Fprintln(stdout, "(subagent is added by the engine at run time)")
	for _, d := range cfg.Tools.Disabled {
		fmt.Fprintf(stdout, "%-14s disabled by config.json\n", d)
	}
	for _, p := range problems {
		fmt.Fprintf(stdout, "! not loaded: %s\n", p)
	}
	if len(problems) > 0 {
		return exitFailed
	}
	return exitOK
}

func firstSentence(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if i := strings.Index(s, ". "); i > 0 {
		s = s[:i+1]
	}
	if len(s) > 90 {
		s = s[:89] + "…"
	}
	return s
}

func toolsRun(args []string, stdout, stderr io.Writer) int {
	fs, homeFlag := homeFlagSet("tools run", stderr)
	argJSON := fs.String("args", "{}", "the tool's arguments as a JSON object")
	workspace := fs.String("workspace", "", "workspace directory (default: the current directory)")
	wait := fs.Int("wait", 60, "seconds to wait for a background tool to finish")
	positional, err := parseInterspersed(fs, args)
	if err != nil || len(positional) != 1 {
		fmt.Fprintln(stderr, toolsUsage)
		return exitUsage
	}
	home, env, cfg, ok := loadHomeForTools(*homeFlag, stderr)
	if !ok {
		return exitUsage
	}
	ws, err := resolveWorkspace(*workspace)
	if err != nil {
		fmt.Fprintf(stderr, "zagent: %v\n", err)
		return exitUsage
	}
	registry, problems := engine.DefaultRegistry(cfg, home)
	for _, p := range problems {
		fmt.Fprintf(stderr, "warning: not loaded: %s\n", p)
	}

	blobs := map[string][]byte{}
	var mu sync.Mutex
	mgr := tools.NewManager()
	mgr.OnExit = func(i tools.ProcInfo) {
		note := i.Note
		if note == "" {
			note = tools.ExitNote(i)
		}
		fmt.Fprintf(stdout, "\n[wake-up note delivered to the agent] %s\n", note)
	}
	printNote := func(kind string) func(string) {
		return func(text string) {
			mu.Lock()
			defer mu.Unlock()
			fmt.Fprintf(stdout, "[%s] %s\n", kind, text)
		}
	}
	te := &tools.Env{
		Workspace: ws, Home: home, SessionID: "tool-test", SessionDir: filepath.Join(home.SessionsDir(), "tool-test"),
		Caps: env.Caps, Cfg: cfg,
		PutBlob: func(b []byte) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			id := fmt.Sprintf("%064x", len(blobs)+1)
			blobs[id] = b
			return id, nil
		},
		Approve: func(_ context.Context, r tools.ApprovalRequest) (bool, string) {
			fmt.Fprintf(stderr, "(approval for %q auto-granted in 'tools run')\n", r.Action)
			return true, ""
		},
		Notify: printNote("note, delivered with the next request"), NotifyWake: printNote("note that wakes the agent"),
		Extra: map[string]any{"procs": mgr},
	}
	defer mgr.KillAll()

	raw := json.RawMessage(*argJSON)
	if !json.Valid(raw) {
		fmt.Fprintln(stderr, "zagent: --args is not valid JSON")
		return exitUsage
	}
	start := time.Now()
	res := registry.Invoke(context.Background(), te, llm.ToolCall{ID: "call_test", Name: positional[0], Args: raw})
	fmt.Fprintf(stdout, "--- what the model receives (%s, %s) ---\n%s\n", map[bool]string{true: "ERROR", false: "ok"}[res.IsError], time.Since(start).Round(time.Millisecond), res.Output)
	for _, m := range res.Media {
		fmt.Fprintf(stdout, "--- attachment: %s %s (%s, %s) ---\n", m.Kind, m.Name, m.MIME, llm.HumanBytes(m.Size))
	}
	if mgr.Running() > 0 {
		fmt.Fprintf(stdout, "\n(waiting up to %ds for the background task…)\n", *wait)
		deadline := time.Now().Add(time.Duration(*wait) * time.Second)
		for mgr.Running() > 0 && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
		}
		time.Sleep(500 * time.Millisecond) // let the final notes and exit message print
	}
	if res.IsError {
		return exitFailed
	}
	return exitOK
}

// loadHomeForTools loads home and config without requiring a working API key:
// testing a tool needs no model.
func loadHomeForTools(homeFlag string, stderr io.Writer) (config.Home, config.Env, config.Config, bool) {
	home, err := config.ResolveHome(homeFlag)
	if err != nil {
		fmt.Fprintln(stderr, "zagent:", err)
		return home, config.Env{}, config.Config{}, false
	}
	env, cfg, err := home.Load()
	if err != nil {
		fmt.Fprintln(stderr, "zagent:", err)
		return home, env, cfg, false
	}
	return home, env, cfg, true
}
