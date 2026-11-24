package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ranjanlive/zpilot/internal/core"
	"github.com/ranjanlive/zpilot/internal/model"
	"github.com/ranjanlive/zpilot/internal/platform"
	"github.com/ranjanlive/zpilot/internal/server"
	"golang.org/x/term"
)

// ctx is the state shared by CLI commands.
type ctx struct {
	data  string
	stdin io.Reader
	out   io.Writer
	err   io.Writer
}

// ---- API client --------------------------------------------------------

type apiError struct {
	code int
	msg  string
}

func (e *apiError) Error() string { return e.msg }

func ping(ep server.Endpoint) error {
	c := &http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequest("GET", ep.URL+"/api/overview", nil)
	req.Header.Set("Authorization", "Bearer "+ep.Token)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *ctx) call(method, path string, body, out any) error {
	ep, err := readEndpoint(c.data)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ep.URL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+ep.Token)
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 15 * time.Minute}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w (%v)", errNotRunning, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return &apiError{resp.StatusCode, e.Error}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// fail prints an error and returns exit code 1.
func (c *ctx) fail(err error) int {
	fmt.Fprintf(c.err, "zpilot: %v\n", err)
	return 1
}

func (c *ctx) table(header string, rows [][]string) {
	tw := tabwriter.NewWriter(c.out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

func (c *ctx) printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Fprintln(c.out, string(b))
}

func newFlags(name string, c *ctx) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(c.err)
	return fs
}

// parse lets flags appear before or after positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, bool) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, false
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, true
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

func readFileArg(path string) (string, error) {
	if path == "-" {
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	}
	b, err := os.ReadFile(path)
	return string(b), err
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "in " + (-d).Round(time.Minute).String()
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.Local().Format("2006-01-02")
}

// ---- daemon ------------------------------------------------------------

func (c *ctx) stop(args []string) int {
	ep, err := readEndpoint(c.data)
	if err != nil || ping(ep) != nil {
		fmt.Fprintln(c.out, "ZPilot is not running.")
		return 0
	}
	if err := os.WriteFile(filepath.Join(c.data, "stop.request"), []byte("stop"), 0o600); err != nil {
		return c.fail(err)
	}
	// The API goes away first; the process then stops its runs and releases
	// the data directory. Wait for that, so an immediate restart works.
	if waitUnlocked(c.data, 40*time.Second) {
		fmt.Fprintln(c.out, "ZPilot stopped.")
		return 0
	}
	return c.fail(errors.New("the daemon did not stop within 40 seconds"))
}

func (c *ctx) status(args []string) int {
	var ov struct {
		Version  string `json:"version"`
		DataDir  string `json:"data_dir"`
		Running  int    `json:"running"`
		Queued   int    `json:"queued"`
		Agents   int    `json:"agents"`
		Models   int    `json:"models"`
		Pending  int    `json:"pending_proposals"`
		Platform string `json:"platform"`
	}
	if err := c.call("GET", "/api/overview", nil, &ov); err != nil {
		fmt.Fprintln(c.out, err)
		return 3
	}
	ep, _ := readEndpoint(c.data)
	fmt.Fprintf(c.out, "ZPilot %s is running (pid %d)\n  dashboard  %s\n  data       %s\n  agents     %d   models %d\n  activity   %d running, %d queued\n",
		ov.Version, ep.PID, ep.URL, ov.DataDir, ov.Agents, ov.Models, ov.Running, ov.Queued)
	if ov.Pending > 0 {
		fmt.Fprintf(c.out, "  approvals  %d waiting (zpilot approvals list)\n", ov.Pending)
	}
	if ov.Models == 0 {
		fmt.Fprintln(c.out, "\nNext step: add a model with 'zpilot models add' or in the dashboard.")
	}
	return 0
}

func (c *ctx) ui(args []string) int {
	ep, err := readEndpoint(c.data)
	if err != nil || ping(ep) != nil {
		return c.fail(errNotRunning)
	}
	var lc struct {
		Code string `json:"code"`
	}
	if err := c.call("POST", "/api/login-code", nil, &lc); err != nil {
		return c.fail(err)
	}
	link := ep.URL + "/#code=" + lc.Code
	if err := platform.OpenURL(link); err != nil {
		fmt.Fprintf(c.out, "Open this link in your browser (it signs you in):\n%s\n", link)
		return 0
	}
	fmt.Fprintf(c.out, "Opened %s\n", ep.URL)
	return 0
}

// ---- agents ------------------------------------------------------------

func (c *ctx) agents(args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list", "ls":
		var as []core.AgentView
		if err := c.call("GET", "/api/agents", nil, &as); err != nil {
			return c.fail(err)
		}
		var rows [][]string
		for _, a := range as {
			state := "ready"
			if a.Disabled {
				state = "disabled"
			} else if a.Running > 0 {
				state = fmt.Sprintf("running %d", a.Running)
			}
			rows = append(rows, []string{a.Icon + " @" + a.Username, a.Name, string(a.Level), nonEmpty(a.ModelName, "(no model)"), strings.Join(a.Tools, ","), state})
		}
		c.table("AGENT\tNAME\tLEVEL\tMODEL\tTOOLS\tSTATE", rows)
		return 0
	case "show":
		if len(args) < 2 {
			return c.fail(errors.New("usage: zpilot agents show @agent"))
		}
		var a core.AgentView
		if err := c.call("GET", "/api/agents/"+url.PathEscape(model.NormalizeUsername(args[1])), nil, &a); err != nil {
			return c.fail(err)
		}
		c.printJSON(a)
		return 0
	case "create", "update":
		return c.agentSave(args[0], args[1:])
	case "delete", "rm":
		if len(args) < 2 {
			return c.fail(errors.New("usage: zpilot agents delete @agent"))
		}
		if err := c.call("DELETE", "/api/agents/"+url.PathEscape(model.NormalizeUsername(args[1])), nil, nil); err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.out, "Deleted @%s (its sessions remain on disk; its workspace is untouched).\n", model.NormalizeUsername(args[1]))
		return 0
	case "enable", "disable":
		if len(args) < 2 {
			return c.fail(fmt.Errorf("usage: zpilot agents %s @agent", args[0]))
		}
		var a core.AgentView
		ref := url.PathEscape(model.NormalizeUsername(args[1]))
		if err := c.call("GET", "/api/agents/"+ref, nil, &a); err != nil {
			return c.fail(err)
		}
		a.Disabled = args[0] == "disable"
		if err := c.call("PUT", "/api/agents/"+ref, a.Agent, nil); err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.out, "@%s %sd.\n", a.Username, args[0])
		return 0
	}
	return c.fail(fmt.Errorf("unknown agents command %q", args[0]))
}

func (c *ctx) agentSave(verb string, args []string) int {
	fs := newFlags("agents "+verb, c)
	username := fs.String("username", "", "handle without @ (lowercase, digits, _)")
	name := fs.String("name", "", "display name")
	icon := fs.String("icon", "", "emoji")
	desc := fs.String("description", "", "what the agent does (shown to other agents)")
	instr := fs.String("instruction", "", "system instruction text")
	instrFile := fs.String("instruction-file", "", "read the system instruction from a file ('-' = stdin)")
	level := fs.String("level", "", "restricted | standard | trusted | full")
	modelID := fs.String("model", "", "model profile id")
	ws := fs.String("workspace", "", "absolute workspace folder (default: <data>/workspaces/<username>)")
	var tools, skills, contacts listFlag
	fs.Var(&tools, "tools", "comma-separated tool ids")
	fs.Var(&skills, "skills", "comma-separated skill names")
	fs.Var(&contacts, "contacts", "comma-separated agents it may message (* = all)")
	pos, ok := parse(fs, args)
	if !ok {
		return 2
	}
	var a model.Agent
	path := "/api/agents"
	method := "POST"
	if verb == "update" {
		if len(pos) < 1 {
			return c.fail(errors.New("usage: zpilot agents update @agent [flags]"))
		}
		var cur core.AgentView
		path += "/" + url.PathEscape(model.NormalizeUsername(pos[0]))
		if err := c.call("GET", path, nil, &cur); err != nil {
			return c.fail(err)
		}
		a, method = cur.Agent, "PUT"
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["username"] {
		a.Username = *username
	}
	if set["name"] {
		a.Name = *name
	}
	if set["icon"] {
		a.Icon = *icon
	}
	if set["description"] {
		a.Description = *desc
	}
	if set["instruction"] {
		a.Instruction = *instr
	}
	if *instrFile != "" {
		t, err := readFileArg(*instrFile)
		if err != nil {
			return c.fail(err)
		}
		a.Instruction = t
	}
	if set["level"] {
		a.Level = model.Level(*level)
	}
	if set["model"] {
		a.ModelID = *modelID
	}
	if set["workspace"] {
		a.Workspace = *ws
	}
	if set["tools"] {
		a.Tools = tools
	}
	if set["skills"] {
		a.Skills = skills
	}
	if set["contacts"] {
		a.Contacts = contacts
	}
	if verb == "create" && a.Name == "" {
		a.Name = a.Username
	}
	var outv core.AgentView
	if err := c.call(method, path, a, &outv); err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.out, "%s @%s (%s, level %s).\n", map[string]string{"create": "Created", "update": "Updated"}[verb], outv.Username, outv.Name, outv.Level)
	return 0
}

// ---- send / threads / runs ---------------------------------------------

func (c *ctx) send(args []string) int {
	fs := newFlags("send", c)
	thread := fs.String("thread", "", "continue this thread")
	wait := fs.Bool("wait", true, "wait for the reply and print it")
	pos, ok := parse(fs, args)
	if !ok {
		return 2
	}
	if len(pos) < 2 {
		return c.fail(errors.New(`usage: zpilot send @agent "message" [--thread ID] [--wait=false]`))
	}
	text := strings.Join(pos[1:], " ")
	if text == "-" {
		b, _ := io.ReadAll(c.stdin)
		text = string(b)
	}
	var m model.Message
	if err := c.call("POST", "/api/messages", map[string]string{"to": pos[0], "text": text, "thread_id": *thread}, &m); err != nil {
		return c.fail(err)
	}
	if !*wait {
		fmt.Fprintf(c.out, "Queued %s in thread %s\n", m.ID, m.ThreadID)
		return 0
	}
	fmt.Fprintf(c.err, "→ %s (thread %s) … waiting for the reply\n", m.To, m.ThreadID)
	for {
		var w core.WaitResult
		if err := c.call("GET", fmt.Sprintf("/api/threads/%s/messages/%s/wait?timeout=55", m.ThreadID, m.ID), nil, &w); err != nil {
			return c.fail(err)
		}
		if !w.Done {
			continue
		}
		fmt.Fprintln(c.out, w.Text)
		if w.Kind == model.KindReply {
			return 0
		}
		return 1
	}
}

func (c *ctx) threads(args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list", "ls":
		fs := newFlags("threads list", c)
		agent := fs.String("agent", "", "only threads with this agent")
		if _, ok := parse(fs, args[1:]); !ok {
			return 2
		}
		var ts []model.Thread
		if err := c.call("GET", "/api/threads?agent="+url.QueryEscape(*agent), nil, &ts); err != nil {
			return c.fail(err)
		}
		var rows [][]string
		for _, t := range ts {
			rows = append(rows, []string{t.ID, clipStr(t.Title, 40), "@" + strings.Join(t.Agents, " @"), fmt.Sprint(t.Runs), ago(t.Updated)})
		}
		c.table("THREAD\tTITLE\tAGENTS\tRUNS\tUPDATED", rows)
		return 0
	case "show":
		if len(args) < 2 {
			return c.fail(errors.New("usage: zpilot threads show ID"))
		}
		var tv core.ThreadView
		if err := c.call("GET", "/api/threads/"+url.PathEscape(args[1]), nil, &tv); err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.out, "%s  (%s)\n\n", tv.Title, tv.ID)
		for _, m := range tv.Messages {
			status := ""
			if m.Status != "" && m.Status != model.StatusDone {
				status = " [" + m.Status + "]"
			}
			fmt.Fprintf(c.out, "%s  %s → %s  %s%s\n%s\n\n", m.Created.Local().Format("Jan 02 15:04"), m.From, m.To, m.Kind, status, indent(m.Text))
		}
		return 0
	case "delete", "rm":
		if len(args) < 2 {
			return c.fail(errors.New("usage: zpilot threads delete ID"))
		}
		if err := c.call("DELETE", "/api/threads/"+url.PathEscape(args[1]), nil, nil); err != nil {
			return c.fail(err)
		}
		fmt.Fprintln(c.out, "Deleted.")
		return 0
	}
	return c.fail(fmt.Errorf("unknown threads command %q", args[0]))
}

func (c *ctx) runs(args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list", "ls":
		fs := newFlags("runs list", c)
		agent := fs.String("agent", "", "only this agent")
		if _, ok := parse(fs, args[1:]); !ok {
			return 2
		}
		var rs []model.Run
		if err := c.call("GET", "/api/runs?agent="+url.QueryEscape(*agent), nil, &rs); err != nil {
			return c.fail(err)
		}
		var rows [][]string
		for _, r := range rs {
			dur := "-"
			if !r.Ended.IsZero() {
				dur = r.Ended.Sub(r.Started).Round(time.Second).String()
			}
			rows = append(rows, []string{r.ID, "@" + r.Username, r.State, dur, fmt.Sprintf("%d/%d", r.Prompt, r.Answer), ago(r.Started)})
		}
		c.table("RUN\tAGENT\tSTATE\tDURATION\tTOKENS (IN/OUT)\tSTARTED", rows)
		return 0
	case "log":
		if len(args) < 2 {
			return c.fail(errors.New("usage: zpilot runs log ID"))
		}
		var l struct {
			Log string `json:"log"`
		}
		if err := c.call("GET", "/api/runs/"+url.PathEscape(args[1])+"/log", nil, &l); err != nil {
			return c.fail(err)
		}
		fmt.Fprint(c.out, l.Log)
		return 0
	case "stop":
		if len(args) < 2 {
			return c.fail(errors.New("usage: zpilot runs stop ID"))
		}
		if err := c.call("POST", "/api/runs/"+url.PathEscape(args[1])+"/stop", nil, nil); err != nil {
			return c.fail(err)
		}
		fmt.Fprintln(c.out, "Stopping.")
		return 0
	}
	return c.fail(fmt.Errorf("unknown runs command %q", args[0]))
}

// ---- models ------------------------------------------------------------

func (c *ctx) models(args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list", "ls":
		var ms []model.ModelProfile
		if err := c.call("GET", "/api/models", nil, &ms); err != nil {
			return c.fail(err)
		}
		var st model.Settings
		_ = c.call("GET", "/api/settings", nil, &st)
		var rows [][]string
		for _, m := range ms {
			def := ""
			if m.ID == st.DefaultModelID {
				def = "default"
			}
			rows = append(rows, []string{m.ID, m.Name, m.Provider, m.Model, map[bool]string{true: "set", false: "MISSING"}[m.HasKey || m.AuthStyle == "none"], def})
		}
		c.table("ID\tNAME\tPROVIDER\tMODEL\tKEY\t", rows)
		return 0
	case "add":
		fs := newFlags("models add", c)
		name := fs.String("name", "", "display name")
		provider := fs.String("provider", "gemini", "gemini | openai_compatible | anthropic | custom")
		base := fs.String("base-url", "", "API base URL (required for openai_compatible and custom)")
		mdl := fs.String("model", "", "model id, e.g. gemini-2.5-flash")
		format := fs.String("api-format", "", "for custom: gemini | openai | anthropic")
		ctxLimit := fs.Int("context-limit", 0, "context window in tokens")
		vision := fs.Bool("vision", true, "the model accepts images")
		pdf := fs.Bool("pdf", false, "the model accepts PDFs")
		keyEnv := fs.String("key-env", "", "read the API key from this environment variable instead of prompting")
		if _, ok := parse(fs, args[1:]); !ok {
			return 2
		}
		key, err := c.readSecret("API key", *keyEnv)
		if err != nil {
			return c.fail(err)
		}
		in := map[string]any{"name": nonEmpty(*name, *mdl), "provider": *provider, "base_url": *base, "model": *mdl,
			"api_format": *format, "context_limit": *ctxLimit, "vision": *vision, "pdf": *pdf, "api_key": key}
		var m model.ModelProfile
		if err := c.call("POST", "/api/models", in, &m); err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.out, "Added model %s (%s). Test it with: zpilot models test %s\n", m.Name, m.ID, m.ID)
		return 0
	case "delete", "rm":
		if len(args) < 2 {
			return c.fail(errors.New("usage: zpilot models delete ID"))
		}
		if err := c.call("DELETE", "/api/models/"+url.PathEscape(args[1]), nil, nil); err != nil {
			return c.fail(err)
		}
		fmt.Fprintln(c.out, "Deleted.")
		return 0
	case "test":
		if len(args) < 2 {
			return c.fail(errors.New("usage: zpilot models test ID"))
		}
		var r struct {
			OK     bool   `json:"ok"`
			Report string `json:"report"`
		}
		if err := c.call("POST", "/api/models/"+url.PathEscape(args[1])+"/test", nil, &r); err != nil {
			return c.fail(err)
		}
		fmt.Fprintln(c.out, r.Report)
		if !r.OK {
			return 1
		}
		return 0
	case "default":
		if len(args) < 2 {
			return c.fail(errors.New("usage: zpilot models default ID"))
		}
		var st model.Settings
		if err := c.call("GET", "/api/settings", nil, &st); err != nil {
			return c.fail(err)
		}
		st.DefaultModelID = args[1]
		if err := c.call("PUT", "/api/settings", st, nil); err != nil {
			return c.fail(err)
		}
		fmt.Fprintln(c.out, "Default model updated.")
		return 0
	}
	return c.fail(fmt.Errorf("unknown models command %q", args[0]))
}

// readSecret reads a secret from an env var, or prompts without echo.
func (c *ctx) readSecret(label, envName string) (string, error) {
	if envName != "" {
		v := os.Getenv(envName)
		if v == "" {
			return "", fmt.Errorf("environment variable %s is empty", envName)
		}
		return v, nil
	}
	if f, ok := c.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprintf(c.err, "%s (input hidden): ", label)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(c.err)
		return strings.TrimSpace(string(b)), err
	}
	line, err := bufio.NewReader(c.stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// ---- tools & skills ----------------------------------------------------

func (c *ctx) tools(args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list", "ls":
		var ts []map[string]any
		if err := c.call("GET", "/api/tools", nil, &ts); err != nil {
			return c.fail(err)
		}
		var rows [][]string
		for _, t := range ts {
			rows = append(rows, []string{fmt.Sprint(t["id"]), fmt.Sprint(t["version"]), fmt.Sprint(t["source"]), fmt.Sprint(t["verified"]), clipStr(fmt.Sprint(t["description"]), 60)})
		}
		c.table("TOOL\tVERSION\tSOURCE\tVERIFIED\tDESCRIPTION", rows)
		return 0
	case "search":
		var r struct {
			Registry struct {
				Tools []map[string]any `json:"tools"`
			} `json:"registry"`
			Platform string `json:"platform"`
		}
		if err := c.call("GET", "/api/registry", nil, &r); err != nil {
			return c.fail(err)
		}
		q := strings.ToLower(strings.Join(args[1:], " "))
		var rows [][]string
		for _, t := range r.Registry.Tools {
			hay := strings.ToLower(fmt.Sprint(t["id"], t["name"], t["description"], t["tags"]))
			if q != "" && !strings.Contains(hay, q) {
				continue
			}
			inst := ""
			if v, _ := t["installed_version"].(string); v != "" {
				inst = "installed " + v
			}
			rows = append(rows, []string{fmt.Sprint(t["id"]), fmt.Sprint(t["version"]), fmt.Sprint(t["category"]), inst, clipStr(fmt.Sprint(t["description"]), 60)})
		}
		c.table("TOOL\tVERSION\tCATEGORY\t\tDESCRIPTION", rows)
		fmt.Fprintf(c.out, "\nInstall with: zpilot tools install ID   (platform %s)\n", r.Platform)
		return 0
	case "install":
		fs := newFlags("tools install", c)
		unverified := fs.Bool("allow-unverified", false, "install even if no checksum is published")
		pos, ok := parse(fs, args[1:])
		if !ok || len(pos) < 1 {
			return c.fail(errors.New("usage: zpilot tools install ID|PATH [--allow-unverified]"))
		}
		body := map[string]any{"id": pos[0], "allow_unverified": *unverified}
		if fi, err := os.Stat(pos[0]); err == nil && fi.IsDir() {
			abs, _ := filepath.Abs(pos[0])
			body = map[string]any{"path": abs}
		}
		var m map[string]any
		if err := c.call("POST", "/api/tools/install", body, &m); err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.out, "Installed %v %v (verified: %v). Add it to an agent with: zpilot agents update @agent --tools %v\n", m["id"], m["version"], m["verified"], m["id"])
		return 0
	case "remove", "rm", "uninstall":
		if len(args) < 2 {
			return c.fail(errors.New("usage: zpilot tools remove ID"))
		}
		if err := c.call("DELETE", "/api/tools/"+url.PathEscape(args[1]), nil, nil); err != nil {
			return c.fail(err)
		}
		fmt.Fprintln(c.out, "Removed.")
		return 0
	}
	return c.fail(fmt.Errorf("unknown tools command %q", args[0]))
}

func (c *ctx) skills(args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list", "ls":
		var ss []map[string]any
		if err := c.call("GET", "/api/skills", nil, &ss); err != nil {
			return c.fail(err)
		}
		var rows [][]string
		for _, s := range ss {
			rows = append(rows, []string{fmt.Sprint(s["name"]), clipStr(fmt.Sprint(s["description"]), 80)})
		}
		c.table("SKILL\tDESCRIPTION", rows)
		return 0
	case "new":
		fs := newFlags("skills new", c)
		desc := fs.String("description", "", "when the agent should use this skill")
		file := fs.String("file", "", "instructions file ('-' = stdin)")
		pos, ok := parse(fs, args[1:])
		if !ok || len(pos) < 1 || *file == "" {
			return c.fail(errors.New("usage: zpilot skills new NAME --description D --file F"))
		}
		body, err := readFileArg(*file)
		if err != nil {
			return c.fail(err)
		}
		if err := c.call("PUT", "/api/skills/"+url.PathEscape(pos[0]), map[string]string{"description": *desc, "body": body}, nil); err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.out, "Saved skill %s.\n", pos[0])
		return 0
	case "add":
		if len(args) < 2 {
			return c.fail(errors.New("usage: zpilot skills add DIR   (a folder containing SKILL.md)"))
		}
		b, err := os.ReadFile(filepath.Join(args[1], "SKILL.md"))
		if err != nil {
			return c.fail(err)
		}
		name := filepath.Base(filepath.Clean(args[1]))
		meta, body := frontmatter(string(b))
		if err := c.call("PUT", "/api/skills/"+url.PathEscape(name), map[string]string{"description": meta["description"], "body": body}, nil); err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.out, "Added skill %s (SKILL.md only; copy supporting files into <data>/library/skills/%s if it has any).\n", name, name)
		return 0
	case "remove", "rm":
		if len(args) < 2 {
			return c.fail(errors.New("usage: zpilot skills remove NAME"))
		}
		if err := c.call("DELETE", "/api/skills/"+url.PathEscape(args[1]), nil, nil); err != nil {
			return c.fail(err)
		}
		fmt.Fprintln(c.out, "Removed.")
		return 0
	}
	return c.fail(fmt.Errorf("unknown skills command %q", args[0]))
}

func frontmatter(text string) (map[string]string, string) {
	meta := map[string]string{}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return meta, text
	}
	head, body, ok := strings.Cut(rest, "\n---")
	if !ok {
		return meta, text
	}
	for _, l := range strings.Split(head, "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok {
			meta[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return meta, strings.TrimLeft(body, "\n")
}

// ---- schedules ---------------------------------------------------------

func (c *ctx) schedules(args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list", "ls":
		var ss []model.Schedule
		if err := c.call("GET", "/api/schedules", nil, &ss); err != nil {
			return c.fail(err)
		}
		var as []core.AgentView
		_ = c.call("GET", "/api/agents", nil, &as)
		names := map[string]string{}
		for _, a := range as {
			names[a.ID] = "@" + a.Username
		}
		var rows [][]string
		for _, s := range ss {
			en := "on"
			if !s.Enabled {
				en = "off"
			}
			next := "-"
			if !s.NextRun.IsZero() {
				next = s.NextRun.Local().Format("Mon Jan 02 15:04")
			}
			rows = append(rows, []string{s.ID, s.Name, names[s.AgentID], s.Trigger.Kind, en, next})
		}
		c.table("ID\tNAME\tAGENT\tTRIGGER\tENABLED\tNEXT RUN", rows)
		return 0
	case "add":
		fs := newFlags("schedules add", c)
		name := fs.String("name", "", "schedule name")
		agent := fs.String("agent", "", "@agent")
		task := fs.String("task", "", "what to ask the agent")
		every := fs.Duration("every", 0, "run at a fixed interval, e.g. 2h")
		daily := fs.String("daily", "", "run every day at HH:MM")
		weekly := fs.String("weekly", "", "days for weekly runs: mon,wed,fri (use with --time)")
		at := fs.String("time", "09:00", "time of day for --weekly")
		once := fs.String("at", "", "run once at an RFC 3339 time, e.g. 2026-12-01T09:00:00+05:30")
		tz := fs.String("timezone", "Local", "IANA timezone, e.g. Asia/Kolkata")
		fresh := fs.Bool("fresh", false, "start a new conversation each run")
		if _, ok := parse(fs, args[1:]); !ok {
			return 2
		}
		sc := model.Schedule{Name: *name, AgentID: model.NormalizeUsername(*agent), Task: *task, Enabled: true, FreshThread: *fresh}
		sc.Trigger.Timezone = *tz
		switch {
		case *every > 0:
			sc.Trigger.Kind, sc.Trigger.EverySeconds = "interval", int64(every.Seconds())
		case *daily != "":
			sc.Trigger.Kind, sc.Trigger.Time = "daily", *daily
		case *weekly != "":
			sc.Trigger.Kind, sc.Trigger.Time = "weekly", *at
			for _, d := range strings.Split(*weekly, ",") {
				i := weekdayIndex(d)
				if i < 0 {
					return c.fail(fmt.Errorf("unknown weekday %q", d))
				}
				sc.Trigger.Days = append(sc.Trigger.Days, i)
			}
		case *once != "":
			t, err := time.Parse(time.RFC3339, *once)
			if err != nil {
				return c.fail(fmt.Errorf("--at: %v", err))
			}
			sc.Trigger.Kind, sc.Trigger.At = "once", t
		default:
			return c.fail(errors.New("choose a trigger: --every, --daily, --weekly or --at"))
		}
		var out model.Schedule
		if err := c.call("POST", "/api/schedules", sc, &out); err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.out, "Created schedule %s; next run %s.\n", out.ID, out.NextRun.Local().Format("Mon Jan 02 15:04"))
		return 0
	case "delete", "rm":
		if len(args) < 2 {
			return c.fail(errors.New("usage: zpilot schedules delete ID"))
		}
		if err := c.call("DELETE", "/api/schedules/"+url.PathEscape(args[1]), nil, nil); err != nil {
			return c.fail(err)
		}
		fmt.Fprintln(c.out, "Deleted.")
		return 0
	case "run":
		if len(args) < 2 {
			return c.fail(errors.New("usage: zpilot schedules run ID"))
		}
		var m model.Message
		if err := c.call("POST", "/api/schedules/"+url.PathEscape(args[1])+"/run", nil, &m); err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.out, "Started in thread %s.\n", m.ThreadID)
		return 0
	}
	return c.fail(fmt.Errorf("unknown schedules command %q", args[0]))
}

func weekdayIndex(d string) int {
	for i, n := range []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"} {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(d)), n) {
			return i
		}
	}
	return -1
}

// ---- channels ----------------------------------------------------------

func (c *ctx) channels(args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list", "ls":
		var cs []core.ChannelView
		if err := c.call("GET", "/api/channels", nil, &cs); err != nil {
			return c.fail(err)
		}
		var rows [][]string
		for _, ch := range cs {
			rows = append(rows, []string{ch.ID, ch.Kind, ch.Name, "@" + ch.Agent, fmt.Sprint(ch.Enabled), strings.Join(ch.Allowed, ","), ch.Status})
			for _, r := range ch.Rejected {
				rows = append(rows, []string{"", "", "  refused:", r.ID, r.Name, "", "allow with: zpilot channels allow " + ch.ID + " " + r.ID})
			}
		}
		c.table("ID\tKIND\tNAME\tAGENT\tENABLED\tALLOWED\tSTATUS", rows)
		return 0
	case "add-telegram", "add-webhook", "add-discord", "add-email", "add-whatsapp":
		kind := strings.TrimPrefix(args[0], "add-")
		fs := newFlags("channels "+args[0], c)
		name := fs.String("name", "", "channel name")
		agent := fs.String("agent", "", "@agent that answers")
		var allow listFlag
		fs.Var(&allow, "allow", "ids allowed to talk to the agent (telegram/discord ids, e-mail addresses or @domain, phone numbers)")
		keyEnv := fs.String("secret-env", "", "read the token / password / secret from this environment variable")
		secret2Env := fs.String("app-secret-env", "", "whatsapp: read the app secret from this environment variable")
		settings := map[string]*string{}
		for _, k := range map[string][]string{
			"email":    {"address", "username", "imap_host", "imap_port", "smtp_host", "smtp_port", "smtp_tls", "poll_seconds"},
			"whatsapp": {"phone_number_id", "verify_token", "graph_version"},
		}[kind] {
			settings[k] = fs.String(strings.ReplaceAll(k, "_", "-"), "", k)
		}
		if _, ok := parse(fs, args[1:]); !ok {
			return 2
		}
		secret := ""
		if kind == model.ChannelWebhook && *keyEnv == "" {
			secret = model.NewToken()
		} else {
			label := map[string]string{"telegram": "Bot token from @BotFather", "discord": "Discord bot token", "email": "Mailbox password (Gmail: app password)", "whatsapp": "WhatsApp access token"}[kind]
			s, err := c.readSecret(label, *keyEnv)
			if err != nil {
				return c.fail(err)
			}
			secret = s
		}
		set := map[string]string{}
		for k, v := range settings {
			if *v != "" {
				set[k] = *v
			}
		}
		in := map[string]any{"kind": kind, "name": nonEmpty(*name, kind), "agent_id": model.NormalizeUsername(*agent), "enabled": true, "allowed": []string(allow), "secret": secret, "settings": set}
		if kind == model.ChannelWhatsApp {
			if set["verify_token"] == "" {
				set["verify_token"] = model.NewToken()[:24]
			}
			s2, err := c.readSecret("WhatsApp app secret", *secret2Env)
			if err != nil {
				return c.fail(err)
			}
			in["secret2"] = s2
		}
		var ch core.ChannelView
		if err := c.call("POST", "/api/channels", in, &ch); err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.out, "Created %s channel %s for @%s.\n", kind, ch.ID, ch.Agent)
		if kind == model.ChannelWebhook {
			ep, _ := readEndpoint(c.data)
			fmt.Fprintf(c.out, "\nPOST %s/hooks/%s\nAuthorization: Bearer %s\nBody: {\"text\": \"...\", \"sender\": \"name\", \"conversation\": \"id\", \"wait\": true}\n\nKeep the secret safe; it is shown only now.\n", ep.URL, ch.ID, secret)
		} else if kind == model.ChannelWhatsApp {
			ep, _ := readEndpoint(c.data)
			fmt.Fprintf(c.out, "\nIn the Meta app dashboard set the webhook to:\n  Callback URL: <your public https URL>/hooks/whatsapp/%s   (this machine: %s)\n  Verify token: %s\nand subscribe to the 'messages' field.\n", ch.ID, ep.URL, ch.Settings["verify_token"])
		} else if len(allow) == 0 {
			fmt.Fprintln(c.out, "Nobody is allowed yet: send a message, then run 'zpilot channels list' and allow your id.")
		}
		return 0
	case "allow", "disallow":
		if len(args) < 3 {
			return c.fail(fmt.Errorf("usage: zpilot channels %s CHANNEL_ID SENDER_ID", args[0]))
		}
		if err := c.call("POST", "/api/channels/"+url.PathEscape(args[1])+"/allow", map[string]any{"sender": args[2], "allow": args[0] == "allow"}, nil); err != nil {
			return c.fail(err)
		}
		fmt.Fprintln(c.out, "Updated.")
		return 0
	case "delete", "rm":
		if len(args) < 2 {
			return c.fail(errors.New("usage: zpilot channels delete ID"))
		}
		if err := c.call("DELETE", "/api/channels/"+url.PathEscape(args[1]), nil, nil); err != nil {
			return c.fail(err)
		}
		fmt.Fprintln(c.out, "Deleted.")
		return 0
	}
	return c.fail(fmt.Errorf("unknown channels command %q", args[0]))
}

// ---- approvals ---------------------------------------------------------

func (c *ctx) approvals(args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list", "ls":
		var cmds []model.Approval
		if err := c.call("GET", "/api/approvals?pending=1", nil, &cmds); err != nil {
			return c.fail(err)
		}
		var ps []model.Proposal
		if err := c.call("GET", "/api/proposals", nil, &ps); err != nil {
			return c.fail(err)
		}
		n := 0
		for _, a := range cmds {
			n++
			fmt.Fprintf(c.out, "%s  [command]  @%s wants to %s, %s\n%s\n\n", a.ID, a.Username, a.Action, ago(a.Created), indent(a.Detail))
		}
		for _, p := range ps {
			if p.State != model.ProposalPending && len(args) < 2 {
				continue
			}
			n++
			fmt.Fprintf(c.out, "%s  [%s]  %s %s  by @%s, %s\n  why: %s\n  payload: %s\n", p.ID, p.State, p.Action, p.Target, p.By, ago(p.Created), p.Reason, clipStr(string(p.Payload), 400))
			if p.Result != "" {
				fmt.Fprintf(c.out, "  result: %s\n", p.Result)
			}
			fmt.Fprintln(c.out)
		}
		if n == 0 {
			fmt.Fprintln(c.out, "Nothing is waiting for you.")
		}
		return 0
	case "approve", "allow", "reject", "deny":
		fs := newFlags("approvals "+args[0], c)
		reason := fs.String("reason", "", "reason the agent sees (command approvals)")
		pos, ok := parse(fs, args[1:])
		if !ok || len(pos) < 1 {
			return c.fail(fmt.Errorf("usage: zpilot approvals %s ID [--reason TEXT]", args[0]))
		}
		yes := args[0] == "approve" || args[0] == "allow"
		id := pos[0]
		if strings.HasPrefix(id, "apr_") {
			verb := map[bool]string{true: "allow", false: "deny"}[yes]
			var a model.Approval
			if err := c.call("POST", "/api/approvals/"+url.PathEscape(id)+"/"+verb, map[string]string{"reason": *reason}, &a); err != nil {
				return c.fail(err)
			}
			fmt.Fprintf(c.out, "%s: @%s %s\n", a.State, a.Username, clipStr(a.Detail, 80))
			return 0
		}
		verb := map[bool]string{true: "approve", false: "reject"}[yes]
		var p model.Proposal
		if err := c.call("POST", "/api/proposals/"+url.PathEscape(id)+"/"+verb, nil, &p); err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.out, "%s: %s %s\n", p.State, p.Action, p.Result)
		if p.State == model.ProposalFailed {
			return 1
		}
		return 0
	}
	return c.fail(fmt.Errorf("unknown approvals command %q", args[0]))
}

// ---- users -------------------------------------------------------------

func (c *ctx) users(args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list", "ls":
		var us []model.User
		if err := c.call("GET", "/api/users", nil, &us); err != nil {
			return c.fail(err)
		}
		var rows [][]string
		for _, u := range us {
			st := "active"
			if u.Disabled {
				st = "disabled"
			}
			rows = append(rows, []string{u.Username, u.Name, u.Role, st, ago(u.LastLogin)})
		}
		c.table("USER\tNAME\tROLE\tSTATUS\tLAST SIGN-IN", rows)
		return 0
	case "add", "password", "role":
		fs := newFlags("users "+args[0], c)
		name := fs.String("name", "", "display name")
		role := fs.String("role", "", "viewer | operator | admin")
		pos, ok := parse(fs, args[1:])
		if !ok || len(pos) < 1 {
			return c.fail(fmt.Errorf("usage: zpilot users %s USERNAME [--name N] [--role R]", args[0]))
		}
		var us []model.User
		if err := c.call("GET", "/api/users", nil, &us); err != nil {
			return c.fail(err)
		}
		var cur *model.User
		for i := range us {
			if us[i].Username == strings.ToLower(pos[0]) {
				cur = &us[i]
			}
		}
		u := model.User{Username: pos[0], Role: model.RoleOperator}
		if cur != nil {
			u = *cur
		} else if args[0] != "add" {
			return c.fail(fmt.Errorf("no user %q", pos[0]))
		}
		if *name != "" {
			u.Name = *name
		}
		if *role != "" {
			u.Role = *role
		}
		pw := ""
		if args[0] != "role" {
			p1, err := c.readSecret("Password for "+u.Username+" (min 10 characters)", "")
			if err != nil {
				return c.fail(err)
			}
			pw = p1
		}
		body := map[string]any{"username": u.Username, "name": u.Name, "role": u.Role, "disabled": u.Disabled, "password": pw}
		method, path := "POST", "/api/users"
		if cur != nil {
			method, path = "PUT", "/api/users/"+url.PathEscape(cur.ID)
		}
		if err := c.call(method, path, body, nil); err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.out, "Saved %s (%s).\n", u.Username, u.Role)
		return 0
	case "delete", "rm":
		if len(args) < 2 {
			return c.fail(errors.New("usage: zpilot users delete USERNAME"))
		}
		if err := c.call("DELETE", "/api/users/"+url.PathEscape(strings.ToLower(args[1])), nil, nil); err != nil {
			return c.fail(err)
		}
		fmt.Fprintln(c.out, "Deleted.")
		return 0
	}
	return c.fail(fmt.Errorf("unknown users command %q", args[0]))
}

// ---- workflows ---------------------------------------------------------

func (c *ctx) workflows(args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list", "ls":
		var ws []model.Workflow
		if err := c.call("GET", "/api/workflows", nil, &ws); err != nil {
			return c.fail(err)
		}
		var rows [][]string
		for _, w := range ws {
			var agents []string
			for _, s := range w.Steps {
				if !slices.Contains(agents, "@"+s.Agent) {
					agents = append(agents, "@"+s.Agent)
				}
			}
			rows = append(rows, []string{w.ID, w.Name, fmt.Sprint(len(w.Steps)), strings.Join(agents, " ")})
		}
		c.table("ID\tNAME\tSTEPS\tAGENTS", rows)
		return 0
	case "show":
		if len(args) < 2 {
			return c.fail(errors.New("usage: zpilot workflows show ID"))
		}
		var w model.Workflow
		if err := c.call("GET", "/api/workflows/"+url.PathEscape(args[1]), nil, &w); err != nil {
			return c.fail(err)
		}
		c.printJSON(w)
		return 0
	case "import":
		if len(args) < 2 {
			return c.fail(errors.New("usage: zpilot workflows import FILE.json   (the JSON from 'workflows show', without id to create a new one)"))
		}
		b, err := readFileArg(args[1])
		if err != nil {
			return c.fail(err)
		}
		var w model.Workflow
		if err := json.Unmarshal([]byte(b), &w); err != nil {
			return c.fail(err)
		}
		method, path := "POST", "/api/workflows"
		if w.ID != "" {
			method, path = "PUT", "/api/workflows/"+url.PathEscape(w.ID)
		}
		var out model.Workflow
		if err := c.call(method, path, w, &out); err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.out, "Saved workflow %s (%s).\n", out.Name, out.ID)
		return 0
	case "run":
		fs := newFlags("workflows run", c)
		input := fs.String("input", "", "input for {{input}} ('-' = stdin)")
		wait := fs.Bool("wait", true, "wait for the run to finish and print the results")
		pos, ok := parse(fs, args[1:])
		if !ok || len(pos) < 1 {
			return c.fail(errors.New("usage: zpilot workflows run ID [--input TEXT] [--wait=false]"))
		}
		in := *input
		if in == "-" {
			b, _ := io.ReadAll(c.stdin)
			in = string(b)
		}
		var r model.WorkflowRun
		if err := c.call("POST", "/api/workflows/"+url.PathEscape(pos[0])+"/run", map[string]string{"input": in}, &r); err != nil {
			return c.fail(err)
		}
		fmt.Fprintf(c.err, "Started run %s (conversation %s)\n", r.ID, r.ThreadID)
		if !*wait {
			return 0
		}
		for r.State == model.WFRunning {
			time.Sleep(time.Second)
			if err := c.call("GET", "/api/workflow-runs/"+url.PathEscape(r.ID), nil, &r); err != nil {
				return c.fail(err)
			}
		}
		for _, st := range r.Graph {
			sr := r.Steps[st.ID]
			fmt.Fprintf(c.out, "── %s (@%s): %s\n", st.ID, st.Agent, sr.State)
			if sr.Output != "" {
				fmt.Fprintln(c.out, indent(sr.Output))
			}
			if sr.Error != "" {
				fmt.Fprintln(c.out, indent("error: "+sr.Error))
			}
		}
		fmt.Fprintf(c.out, "\nRun %s.\n", r.State)
		if r.State != model.WFDone {
			return 1
		}
		return 0
	case "runs":
		var rs []model.WorkflowRun
		if err := c.call("GET", "/api/workflow-runs", nil, &rs); err != nil {
			return c.fail(err)
		}
		var rows [][]string
		for _, r := range rs {
			rows = append(rows, []string{r.ID, r.Name, r.State, ago(r.Started), r.By})
		}
		c.table("RUN\tWORKFLOW\tSTATE\tSTARTED\tBY", rows)
		return 0
	case "cancel":
		if len(args) < 2 {
			return c.fail(errors.New("usage: zpilot workflows cancel RUN_ID"))
		}
		if err := c.call("POST", "/api/workflow-runs/"+url.PathEscape(args[1])+"/cancel", nil, nil); err != nil {
			return c.fail(err)
		}
		fmt.Fprintln(c.out, "Canceled.")
		return 0
	case "delete", "rm":
		if len(args) < 2 {
			return c.fail(errors.New("usage: zpilot workflows delete ID"))
		}
		if err := c.call("DELETE", "/api/workflows/"+url.PathEscape(args[1]), nil, nil); err != nil {
			return c.fail(err)
		}
		fmt.Fprintln(c.out, "Deleted.")
		return 0
	}
	return c.fail(fmt.Errorf("unknown workflows command %q", args[0]))
}

// ---- admin & audit -----------------------------------------------------

func (c *ctx) admin(args []string) int {
	if len(args) == 0 || (args[0] != "password" && args[0] != "clear-password") {
		return c.fail(errors.New("usage: zpilot admin password | zpilot admin clear-password"))
	}
	pw := ""
	if args[0] == "password" {
		p1, err := c.readSecret("New dashboard password (min 10 characters)", "")
		if err != nil {
			return c.fail(err)
		}
		if f, ok := c.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			p2, _ := c.readSecret("Repeat", "")
			if p1 != p2 {
				return c.fail(errors.New("the passwords differ"))
			}
		}
		pw = p1
	}
	// The password lives in state.json, which only the daemon writes. If it
	// is not running, open the data directory directly.
	if ep, err := readEndpoint(c.data); err == nil && ping(ep) == nil {
		return c.fail(errors.New("stop the daemon first ('zpilot stop'), then set the password, then start it again"))
	}
	svc, err := core.Open(core.Config{DataDir: c.data})
	if err != nil {
		return c.fail(err)
	}
	defer svc.Close()
	if err := svc.SetAdminPassword("owner (cli)", pw); err != nil {
		return c.fail(err)
	}
	if pw == "" {
		fmt.Fprintln(c.out, "Password removed; only local token sign-in remains.")
	} else {
		fmt.Fprintln(c.out, "Password set. You can now sign in to the dashboard from other machines (with TLS).")
	}
	return 0
}

func (c *ctx) audit(args []string) int {
	fs := newFlags("audit", c)
	limit := fs.Int("limit", 50, "entries")
	if _, ok := parse(fs, args); !ok {
		return 2
	}
	var es []model.AuditEntry
	if err := c.call("GET", fmt.Sprintf("/api/audit?limit=%d", *limit), nil, &es); err != nil {
		return c.fail(err)
	}
	sort.SliceStable(es, func(i, j int) bool { return es[i].Time.Before(es[j].Time) })
	var rows [][]string
	for _, e := range es {
		rows = append(rows, []string{e.Time.Local().Format("Jan 02 15:04:05"), e.Actor, e.Action, e.Target, clipStr(e.Detail, 60)})
	}
	c.table("TIME\tACTOR\tACTION\tTARGET\tDETAIL", rows)
	return 0
}

// ---- small helpers -----------------------------------------------------

func nonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func clipStr(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n    ")
}

// waitUnlocked waits until no daemon holds the data directory's lock.
func waitUnlocked(data string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if l, err := platform.Acquire(filepath.Join(data, "daemon.lock")); err == nil {
			l.Close()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}
