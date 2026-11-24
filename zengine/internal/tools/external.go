package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
)

// Manifest describes a third-party tool: <home>/tools/<name>/<name>.json.
//
// The tool is any program: an .exe, a script run through an interpreter, a
// shell wrapper. It is executed directly from Command (no shell), receives the
// call arguments as JSON, and talks back through stdout, files and a notify
// file. See docs/EXTERNAL_TOOLS.md.
type Manifest struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	// Command is the argv to execute. Placeholders: {dir} (the tool's
	// directory), {workspace}, {home}, {scratch}, {output_dir}. A first element
	// that exists inside the tool's directory is resolved against it.
	Command []string `json:"command"`
	// Input chooses how arguments arrive: "stdin_json" (default), "args_json"
	// (the JSON object is appended as the last argument) or "env" (one
	// ZAGENT_ARG_<NAME> variable per top-level argument).
	Input string `json:"input"`
	// Output: "text" (default; stdout and stderr become the result, a non-zero
	// exit code marks an error) or "json" (stdout is a result object, see
	// Result).
	Output string `json:"output"`
	// Cwd: "workspace" (default) or "tool_dir".
	Cwd string            `json:"cwd"`
	Env map[string]string `json:"env"`
	// TimeoutSeconds: 0 = the engine default, -1 = no timeout (long-running tools).
	TimeoutSeconds int `json:"timeout_seconds"`
	// Background runs the tool as a background task: the call returns at once
	// and the agent is woken with the result when it exits.
	Background bool `json:"background"`
	// Permission: "allow" (default) or "ask" (a human approves each call).
	Permission string `json:"permission"`
	// AttachOutputs attaches media files the tool wrote to its output
	// directory (images, audio, PDFs the model supports) to the result.
	AttachOutputs bool `json:"attach_outputs"`
	// Platforms restricts loading to listed GOOS values (empty = all).
	Platforms []string `json:"platforms"`
}

// ExternalResult is the JSON a tool prints on stdout when Output is "json".
type ExternalResult struct {
	Output  string `json:"output"`
	IsError bool   `json:"is_error"`
	// Media lists files to attach to the result.
	Media []struct {
		Path string `json:"path"`
		MIME string `json:"mime"`
	} `json:"media"`
}

var toolName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)

// builtinNames cannot be shadowed by external tools.
var builtinNames = map[string]bool{
	"view_file": true, "write_file": true, "edit_file": true, "list_dir": true, "find_files": true, "search_text": true,
	"run_command": true, "terminal": true, "memory": true, "skill": true, "subagent": true,
}

// LoadExternal scans dir for tool manifests. Problems with individual tools
// are reported, never fatal: one broken tool must not take the agent down.
func LoadExternal(dir string) (loaded []Tool, problems []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		toolDir := filepath.Join(dir, e.Name())
		path := filepath.Join(toolDir, e.Name()+".json")
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			problems = append(problems, fmt.Sprintf("tool %s: %v", e.Name(), err))
			continue
		}
		var m Manifest
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&m); err != nil {
			problems = append(problems, fmt.Sprintf("tool %s: bad manifest: %v", e.Name(), err))
			continue
		}
		if m.Name == "" {
			m.Name = e.Name()
		}
		t, err := newExternal(m, toolDir)
		if err != nil {
			problems = append(problems, fmt.Sprintf("tool %s: %v", e.Name(), err))
			continue
		}
		if t == nil { // not for this platform
			continue
		}
		if seen[m.Name] {
			problems = append(problems, fmt.Sprintf("tool %s: duplicate name %q", e.Name(), m.Name))
			continue
		}
		seen[m.Name] = true
		loaded = append(loaded, t)
	}
	sort.Slice(loaded, func(i, j int) bool { return loaded[i].Spec().Name < loaded[j].Spec().Name })
	return loaded, problems
}

func newExternal(m Manifest, dir string) (*externalTool, error) {
	if !toolName.MatchString(m.Name) {
		return nil, fmt.Errorf("name %q is invalid (lowercase letters, digits and '_', starting with a letter, max 48)", m.Name)
	}
	if builtinNames[m.Name] {
		return nil, fmt.Errorf("name %q is reserved for a built-in tool", m.Name)
	}
	if strings.TrimSpace(m.Description) == "" {
		return nil, errors.New("description is required: it is what tells the model when to use the tool")
	}
	if len(m.Command) == 0 || strings.TrimSpace(m.Command[0]) == "" {
		return nil, errors.New("command is required")
	}
	if len(m.Platforms) > 0 {
		ok := false
		for _, p := range m.Platforms {
			ok = ok || p == runtime.GOOS
		}
		if !ok {
			return nil, nil
		}
	}
	switch m.Input {
	case "", "stdin_json", "args_json", "env":
	default:
		return nil, fmt.Errorf(`input must be "stdin_json", "args_json" or "env", got %q`, m.Input)
	}
	switch m.Output {
	case "", "text", "json":
	default:
		return nil, fmt.Errorf(`output must be "text" or "json", got %q`, m.Output)
	}
	switch m.Cwd {
	case "", "workspace", "tool_dir":
	default:
		return nil, fmt.Errorf(`cwd must be "workspace" or "tool_dir", got %q`, m.Cwd)
	}
	switch m.Permission {
	case "", "allow", "ask":
	default:
		return nil, fmt.Errorf(`permission must be "allow" or "ask", got %q`, m.Permission)
	}
	if len(m.Parameters) == 0 {
		m.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	var sch map[string]any
	if err := json.Unmarshal(m.Parameters, &sch); err != nil {
		return nil, fmt.Errorf("parameters is not a valid JSON Schema object: %v", err)
	}
	if t, _ := sch["type"].(string); t != "object" {
		return nil, errors.New(`parameters must be a JSON Schema with "type": "object"`)
	}
	return &externalTool{m: m, dir: dir}, nil
}

type externalTool struct {
	m   Manifest
	dir string
	seq atomic.Int64
}

func (t *externalTool) Spec() llm.ToolSpec {
	desc := t.m.Description
	if t.m.Background {
		desc += " (Runs in the background: the call returns at once and you are notified with the result when it finishes.)"
	}
	return llm.ToolSpec{Name: t.m.Name, Description: desc, Schema: t.m.Parameters}
}

// Timeout implements TimeoutTool. Background and unlimited tools manage their
// own lifetime; others use the manifest value or the engine default.
func (t *externalTool) Timeout() time.Duration {
	switch {
	case t.m.Background, t.m.TimeoutSeconds < 0:
		return -1
	case t.m.TimeoutSeconds > 0:
		return time.Duration(t.m.TimeoutSeconds) * time.Second
	}
	return 0
}

func (t *externalTool) Run(ctx context.Context, env *Env, raw json.RawMessage) Result {
	if t.m.Permission == "ask" {
		if env.Approve == nil {
			return Fail("tool %s needs approval, but no one is available to approve it in this mode.", t.m.Name)
		}
		ok, why := env.Approve(ctx, ApprovalRequest{Tool: t.m.Name, Action: "run tool " + t.m.Name, Detail: briefJSON(raw)})
		if !ok {
			if why == "" {
				why = "the user declined"
			}
			return Fail("tool %s was not run: %s.", t.m.Name, why)
		}
	}

	n := t.seq.Add(1)
	outDir := filepath.Join(env.Home.ScratchDir(), "tool-output", sanitize(env.SessionID), fmt.Sprintf("%s-%x-%d", t.m.Name, time.Now().UnixNano(), n))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return Fail("cannot create the tool's output directory: %v", err)
	}
	notifyFile := filepath.Join(outDir, ".notify")
	if err := os.WriteFile(notifyFile, nil, 0o644); err != nil {
		return Fail("cannot create the tool's notify file: %v", err)
	}

	argv, err := t.argv(env, outDir, raw)
	if err != nil {
		return Fail("%v", err)
	}
	cwd := env.Workspace
	if t.m.Cwd == "tool_dir" {
		cwd = t.dir
	}
	environment := []string{
		"ZAGENT_WORKSPACE=" + env.Workspace, "ZAGENT_HOME=" + env.Home.Root, "ZAGENT_SCRATCH=" + env.Home.ScratchDir(),
		"ZAGENT_SESSION=" + env.SessionID, "ZAGENT_TOOL_NAME=" + t.m.Name, "ZAGENT_TOOL_DIR=" + t.dir,
		"ZAGENT_OUTPUT_DIR=" + outDir, "ZAGENT_NOTIFY_FILE=" + notifyFile,
		fmt.Sprintf("ZAGENT_CAPS=vision=%t,audio=%t,pdf=%t,video=%t", env.Caps.Vision, env.Caps.Audio, env.Caps.PDF, env.Caps.Video),
	}
	for k, v := range t.m.Env {
		environment = append(environment, k+"="+expand(v, t.dir, env, outDir))
	}
	// A non-nil (even empty) slice makes the manager close stdin after writing,
	// so a tool that reads stdin in args_json/env mode sees EOF instead of hanging.
	stdin := []byte{}
	switch t.m.Input {
	case "", "stdin_json":
		stdin = normalizedArgs(raw)
	case "env":
		environment = append(environment, "ZAGENT_ARGS_JSON="+string(normalizedArgs(raw)))
		environment = append(environment, argEnv(raw)...)
	}

	var stdout jsonOutput
	var sink io.Writer
	if t.m.Output == "json" {
		sink = &stdout
	}
	m := procs(env)
	timeout := time.Duration(0)
	if t.m.TimeoutSeconds > 0 && t.m.Background {
		timeout = time.Duration(t.m.TimeoutSeconds) * time.Second
	}
	p, err := m.Start(StartOptions{
		Argv: argv, Cwd: cwd, Env: environment, Stdin: stdin, Stdout: sink, Timeout: timeout,
		Note: func(i ProcInfo) string { return t.exitNote(env, i, &stdout, outDir) },
	})
	if err != nil {
		return Fail("could not start %s: %v", t.m.Name, err)
	}

	watchCtx, stopWatch := context.WithCancel(context.Background())
	var watchDone sync.WaitGroup
	watchDone.Add(1)
	go func() {
		defer watchDone.Done()
		tailNotes(watchCtx, notifyFile, env)
	}()
	finishWatch := func() { stopWatch(); watchDone.Wait() }

	if t.m.Background {
		p.Detach()
		// The watcher outlives this call; it ends when the process does.
		go func() {
			<-p.doneCh
			time.Sleep(300 * time.Millisecond) // let the final lines land
			finishWatch()
		}()
		time.Sleep(300 * time.Millisecond)
		if !p.Info().Running {
			return t.collect(env, p, &stdout, outDir)
		}
		return OK("Started tool %s as background task #%d. Output files will be written to %s. You will be notified when it finishes; manage it with terminal(action=\"output\"|\"wait\"|\"kill\", id=%d).",
			t.m.Name, p.ID, outDir, p.ID)
	}

	done := p.Wait(ctx, 0)
	if !done {
		p.Kill()
		p.Wait(context.Background(), 5*time.Second)
		finishWatch()
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Fail("tool %s timed out and was stopped.", t.m.Name)
		}
		return Fail("tool %s was cancelled.", t.m.Name)
	}
	time.Sleep(50 * time.Millisecond)
	finishWatch()
	return t.collect(env, p, &stdout, outDir)
}

func (t *externalTool) argv(env *Env, outDir string, raw json.RawMessage) ([]string, error) {
	argv := make([]string, len(t.m.Command))
	for i, a := range t.m.Command {
		argv[i] = expand(a, t.dir, env, outDir)
	}
	// A relative argument naming a file inside the tool's directory (the
	// executable, a script, a data file) is addressed there, so manifests can
	// say ["python", "tool.py"] while the process itself runs in the workspace.
	for i, a := range argv {
		if filepath.IsAbs(a) || strings.HasPrefix(a, "-") {
			continue
		}
		if st, err := os.Stat(filepath.Join(t.dir, a)); err == nil && !st.IsDir() {
			argv[i] = filepath.Join(t.dir, a)
		}
	}
	if t.m.Input == "args_json" {
		argv = append(argv, string(normalizedArgs(raw)))
	}
	return argv, nil
}

func expand(s, toolDir string, env *Env, outDir string) string {
	r := strings.NewReplacer("{dir}", toolDir, "{workspace}", env.Workspace, "{home}", env.Home.Root, "{scratch}", env.Home.ScratchDir(), "{output_dir}", outDir)
	return r.Replace(s)
}

func normalizedArgs(raw json.RawMessage) []byte {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return []byte("{}")
	}
	return raw
}

func argEnv(raw json.RawMessage) []string {
	var m map[string]any
	if json.Unmarshal(normalizedArgs(raw), &m) != nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		var v string
		switch x := m[k].(type) {
		case string:
			v = x
		default:
			b, _ := json.Marshal(x)
			v = string(b)
		}
		out = append(out, "ZAGENT_ARG_"+strings.ToUpper(sanitizeEnvName(k))+"="+v)
	}
	return out
}

func sanitizeEnvName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, s)
}

func briefJSON(raw json.RawMessage) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// collect turns a finished process into a Result.
func (t *externalTool) collect(env *Env, p *Proc, stdout *jsonOutput, outDir string) Result {
	info := p.Info()
	logs, _ := p.Unread()
	res := Result{}
	switch {
	case info.TimedOut:
		res = Fail("tool %s timed out and was stopped.\n%s", t.m.Name, decode(logs))
	case info.Killed:
		res = Fail("tool %s was stopped.\n%s", t.m.Name, decode(logs))
	case t.m.Output == "json":
		if stdout.Overflowed() {
			res = Fail("tool %s produced more than the 8 MiB JSON output limit; reduce its output or write large results to files", t.m.Name)
		} else {
			res = t.parseJSON(env, stdout.Bytes(), logs, info)
		}
	default:
		out := strings.TrimRight(decode(logs), "\n")
		res = Result{Output: out, IsError: info.ExitCode != 0}
		if info.ExitCode != 0 {
			res.Output = strings.TrimSpace(fmt.Sprintf("%s\n[tool exited with code %d]", out, info.ExitCode))
		}
	}
	if res.Output == "" && !res.IsError {
		res.Output = fmt.Sprintf("Tool %s finished with no output.", t.m.Name)
	}
	files, media := t.outputFiles(env, outDir)
	if len(files) > 0 {
		res.Output += "\nFiles written by the tool:\n" + strings.Join(files, "\n")
	}
	res.Media = append(res.Media, media...)
	return res
}

func (t *externalTool) parseJSON(env *Env, out, logs []byte, info ProcInfo) Result {
	var r ExternalResult
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		if info.ExitCode != 0 { // it failed before producing a result: its own error message is the useful part
			return Result{Output: strings.TrimSpace(decode(logs) + fmt.Sprintf("\n[tool %s exited with code %d]", t.m.Name, info.ExitCode)), IsError: true}
		}
		return Result{Output: fmt.Sprintf("Tool %s exited normally but printed nothing on stdout, although its manifest says the output is JSON.\n%s", t.m.Name, decode(logs)), IsError: true}
	}
	if err := json.Unmarshal(trimmed, &r); err != nil {
		return Result{Output: fmt.Sprintf("Tool %s did not print valid JSON on stdout (%v). It printed:\n%s", t.m.Name, err, truncateMiddle(string(trimmed), 2000)), IsError: true}
	}
	res := Result{Output: r.Output, IsError: r.IsError || info.ExitCode != 0}
	var attachedBytes int64
	for i, m := range r.Media {
		if i >= 16 {
			res.Output += "\n(additional media omitted: maximum 16 attachments per tool result)"
			break
		}
		if info, err := os.Stat(m.Path); err == nil && attachedBytes+info.Size() > 32<<20 {
			res.Output += "\n(media omitted: total attachment size exceeds 32 MiB)"
			continue
		}
		media, err := mediaFromFile(env, m.Path, m.MIME)
		if err != nil {
			res.Output += "\n(could not attach " + m.Path + ": " + err.Error() + ")"
			continue
		}
		res.Media = append(res.Media, media)
		attachedBytes += media.Size
	}
	return res
}

// outputFiles lists files the tool wrote into its output directory and,
// when AttachOutputs is set, attaches the media the model can take.
func (t *externalTool) outputFiles(env *Env, outDir string) (listing []string, media []llm.Media) {
	var attachedBytes int64
	_ = filepath.WalkDir(outDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == ".notify" {
			return nil
		}
		listing = append(listing, "  "+p)
		if t.m.AttachOutputs && len(media) < 16 {
			if info, err := d.Info(); err == nil && attachedBytes+info.Size() > 32<<20 {
				return nil
			}
			if m, err := mediaFromFile(env, p, ""); err == nil {
				media = append(media, m)
				attachedBytes += m.Size
			}
		}
		if len(listing) >= 200 {
			listing = append(listing, "  … additional output files omitted (limit 200)")
			return io.EOF
		}
		return nil
	})
	sort.Strings(listing)
	return listing, media
}

// mediaFromFile attaches a file the tool produced. It may live outside the
// workspace (the tool's output directory), so it is checked against the media
// capabilities and size limits directly instead of the workspace path policy.
func mediaFromFile(env *Env, path, mimeHint string) (llm.Media, error) {
	f, err := os.Open(path)
	if err != nil {
		return llm.Media{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return llm.Media{}, errors.New("not a file")
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	mt := mimeHint
	if mt == "" {
		mt = detectMIME(path, head[:n])
	}
	kind, ok := llm.KindForMIME(mt)
	if !ok {
		return llm.Media{}, fmt.Errorf("%s is not an image, audio, video or PDF file", mt)
	}
	res := viewMedia(env, path, f, info, kind, mt)
	if res.IsError || len(res.Media) == 0 {
		return llm.Media{}, errors.New(res.Output)
	}
	return res.Media[0], nil
}

// exitNote builds the announcement for a finished background run.
func (t *externalTool) exitNote(env *Env, i ProcInfo, stdout *jsonOutput, outDir string) string {
	state := fmt.Sprintf("finished (exit code %d)", i.ExitCode)
	switch {
	case i.TimedOut:
		state = "was stopped at its timeout"
	case i.Killed:
		state = "was stopped"
	}
	summary := ""
	if t.m.Output == "json" {
		var r ExternalResult
		if stdout.Overflowed() {
			summary = "JSON output exceeded the 8 MiB limit; result rejected"
		} else if json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &r) == nil {
			summary = r.Output
		}
	}
	files, _ := t.outputFiles(env, outDir)
	var sb strings.Builder
	fmt.Fprintf(&sb, "[info: background tool %s (task #%d) %s after %s.", t.m.Name, i.ID, state, i.Ended.Sub(i.Started).Round(time.Second))
	if summary != "" {
		fmt.Fprintf(&sb, " Result: %s", oneLine(summary, 400))
	}
	if len(files) > 0 {
		fmt.Fprintf(&sb, " Files: %s", strings.TrimSpace(strings.Join(files, ", ")))
	}
	fmt.Fprintf(&sb, " Read its full output with terminal(action=\"output\", id=%d).]", i.ID)
	return sb.String()
}

// tailNotes forwards lines a tool appends to its notify file as notes. A line
// is either plain text or JSON {"text": "...", "wake": true}. It keeps polling
// until ctx ends, then drains once more so nothing written at the very end is lost.
func tailNotes(ctx context.Context, path string, env *Env) {
	var offset int64
	var partial string
	read := func() {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return
		}
		data, _ := io.ReadAll(io.LimitReader(f, 64*1024))
		offset += int64(len(data))
		sc := bufio.NewScanner(strings.NewReader(partial + string(data)))
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var lines []string
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		partial = ""
		if len(data) > 0 && data[len(data)-1] != '\n' && len(lines) > 0 {
			partial = lines[len(lines)-1] // an unfinished line: wait for the rest
			if len(partial) > 64*1024 {
				partial = ""
			} // bound unfinished notify lines
			lines = lines[:len(lines)-1]
		}
		for _, l := range lines {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			text, wake := l, false
			var j struct {
				Text string `json:"text"`
				Wake bool   `json:"wake"`
			}
			if strings.HasPrefix(l, "{") && json.Unmarshal([]byte(l), &j) == nil && j.Text != "" {
				text, wake = j.Text, j.Wake
			}
			if wake && env.NotifyWake != nil {
				env.NotifyWake(text)
			} else if env.Notify != nil {
				env.Notify(text)
			}
		}
	}
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			read()
		case <-ctx.Done():
			read()
			return
		}
	}
}

// RegisterExternal loads external tools into r and returns any problems.
func RegisterExternal(r *Registry, dir string) []string {
	loaded, problems := LoadExternal(dir)
	for _, t := range loaded {
		r.Register(t)
	}
	return problems
}
