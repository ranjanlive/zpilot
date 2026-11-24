package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
)

// procs returns the engine's process manager, creating one on first use so
// tools work in isolation (tests, one-shot runs).
var processInitMu sync.Mutex

func procs(env *Env) *Manager {
	processInitMu.Lock()
	defer processInitMu.Unlock()
	if env.Extra == nil {
		env.Extra = map[string]any{}
	}
	if m, ok := env.Extra["procs"].(*Manager); ok {
		return m
	}
	m := NewManager()
	env.Extra["procs"] = m
	return m
}

// ExitNote is the "[info: ...]" text announcing a finished background task.
func ExitNote(i ProcInfo) string {
	state := fmt.Sprintf("finished (exit code %d)", i.ExitCode)
	switch {
	case i.TimedOut:
		state = "was stopped because it hit its timeout"
	case i.Killed:
		state = "was killed"
	}
	return fmt.Sprintf("[info: background task #%d %s after %s: %s. Read its output with terminal(action=\"output\", id=%d).]",
		i.ID, state, i.Ended.Sub(i.Started).Round(time.Second), oneLine(i.Command, 120), i.ID)
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:alignDown(s, n)] + "…"
	}
	return s
}

func decode(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "�")
}

// ---- run_command ----

type runCommand struct{}

// Timeout disables the registry's blanket timeout: run_command manages its own
// waiting and moves slow commands to the background instead of killing them.
func (runCommand) Timeout() time.Duration { return -1 }

func (runCommand) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "run_command",
		Description: "Run a shell command (" + ShellName() + ") in the workspace and return its output and exit code. " +
			"If it is still running after wait_seconds it keeps running in the background and you are notified when it finishes; " +
			"manage it with the terminal tool. Use background=true for servers and other long-running processes.",
		Schema: schema(map[string]any{
			"command":         str("The command line to run."),
			"cwd":             str("Working directory; defaults to the workspace."),
			"wait_seconds":    num("How long to wait for completion before moving it to the background (default 30, max 600)."),
			"background":      flag("Start it and return immediately."),
			"timeout_seconds": num("Kill the command if it runs longer than this (default: no limit)."),
		}, "command"),
	}
}

func (runCommand) Run(ctx context.Context, env *Env, raw json.RawMessage) Result {
	var a struct {
		Command    string `json:"command"`
		Cwd        string `json:"cwd"`
		Wait       int    `json:"wait_seconds"`
		Background bool   `json:"background"`
		Timeout    int    `json:"timeout_seconds"`
	}
	_ = json.Unmarshal(raw, &a)
	if strings.TrimSpace(a.Command) == "" {
		return Fail("command is empty")
	}
	switch env.Cfg.Permissions.RunCommand {
	case "deny":
		return Fail("running commands is disabled (permissions.run_command is \"deny\"). Tell the user which command you need, or work with the file tools.")
	case "ask":
		if env.Approve == nil {
			return Fail("running commands needs approval, but no one is available to approve it in this mode. The user can set permissions.run_command to \"allow\" in config.json.")
		}
		ok, reason := env.Approve(ctx, ApprovalRequest{Tool: "run_command", Action: "run command", Detail: a.Command})
		if !ok {
			if reason == "" {
				reason = "the user declined"
			}
			return Fail("command not run: %s.", reason)
		}
	}
	cwd := env.Workspace
	if a.Cwd != "" {
		p, err := env.ResolvePath(a.Cwd, false)
		if err != nil {
			return Fail("%v", err)
		}
		cwd = p
	}
	wait := a.Wait
	if wait <= 0 {
		wait = 30
	}
	wait = min(wait, 600)

	m := procs(env)
	p, err := m.Start(StartOptions{
		Command: a.Command, Cwd: cwd, Timeout: time.Duration(a.Timeout) * time.Second,
		Env: []string{"ZAGENT_WORKSPACE=" + env.Workspace, "ZAGENT_HOME=" + env.Home.Root, "ZAGENT_SCRATCH=" + env.Home.ScratchDir(), "ZAGENT_SESSION=" + env.SessionID},
	})
	if err != nil {
		return Fail("could not start the command: %v", err)
	}
	if a.Background {
		p.Detach()
		time.Sleep(300 * time.Millisecond) // catch immediate failures
		return background(p, "Started in the background")
	}
	if p.Wait(ctx, time.Duration(wait)*time.Second) {
		return finished(p)
	}
	if ctx.Err() != nil { // the user stopped the agent: do not leave orphans behind
		p.Kill()
		return Fail("command cancelled")
	}
	if !p.Detach() { // finished in the instant between the wait and the detach
		return finished(p)
	}
	return background(p, fmt.Sprintf("Still running after %ds; moved to the background", wait))
}

func finished(p *Proc) Result {
	data, _ := p.Unread()
	i := p.Info()
	var sb strings.Builder
	sb.WriteString(decode(data))
	if sb.Len() > 0 && !strings.HasSuffix(sb.String(), "\n") {
		sb.WriteString("\n")
	}
	switch {
	case i.TimedOut:
		fmt.Fprintf(&sb, "[stopped: timeout reached; exit code %d]", i.ExitCode)
		return Result{Output: sb.String(), IsError: true}
	case i.Killed:
		fmt.Fprintf(&sb, "[killed; exit code %d]", i.ExitCode)
		return Result{Output: sb.String(), IsError: true}
	}
	fmt.Fprintf(&sb, "[exit code %d]", i.ExitCode)
	return Result{Output: sb.String(), IsError: i.ExitCode != 0}
}

func background(p *Proc, lead string) Result {
	data, _ := p.Unread()
	i := p.Info()
	if !i.Running { // died immediately
		var sb strings.Builder
		sb.WriteString(decode(data))
		fmt.Fprintf(&sb, "\n[exit code %d]", i.ExitCode)
		return Result{Output: strings.TrimLeft(sb.String(), "\n"), IsError: i.ExitCode != 0}
	}
	return OK("%s as task #%d.\nOutput so far:\n%s\nManage it with terminal(action=\"output\"|\"input\"|\"wait\"|\"kill\", id=%d). You will be notified when it finishes.",
		lead, i.ID, strings.TrimRight(decode(data), "\n"), i.ID)
}

// ---- terminal ----

type terminal struct{}

func (terminal) Timeout() time.Duration { return -1 }

func (terminal) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "terminal",
		Description: "Manage tasks started by run_command. Actions: list (all tasks), output (new output since you last looked; all=true for everything buffered), " +
			"input (send text to the task's stdin), wait (block until it finishes or seconds elapse), kill (stop it and its child processes).",
		Schema: schema(map[string]any{
			"action":  enum("What to do.", "list", "output", "input", "wait", "kill"),
			"id":      num("Task number (not needed for list)."),
			"input":   str("Text to send for action=input."),
			"newline": flag("Append a newline to the input (default true)."),
			"seconds": num("Maximum seconds to wait for action=wait (default 60, max 600)."),
			"all":     flag("For action=output: return everything still buffered, not just the new output."),
		}, "action"),
	}
}

func (terminal) Run(ctx context.Context, env *Env, raw json.RawMessage) Result {
	var a struct {
		Action  string `json:"action"`
		ID      int    `json:"id"`
		Input   string `json:"input"`
		Newline *bool  `json:"newline"`
		Seconds int    `json:"seconds"`
		All     bool   `json:"all"`
	}
	_ = json.Unmarshal(raw, &a)
	m := procs(env)
	if a.Action == "list" {
		infos := m.List()
		if len(infos) == 0 {
			return OK("No tasks have been started.")
		}
		var sb strings.Builder
		for _, i := range infos {
			state := "running"
			if !i.Running {
				state = fmt.Sprintf("exited %d", i.ExitCode)
				if i.Killed {
					state = "killed"
				}
				if i.TimedOut {
					state = "timed out"
				}
			}
			fmt.Fprintf(&sb, "#%d  %-10s  %s  %s\n", i.ID, state, time.Since(i.Started).Round(time.Second), oneLine(i.Command, 100))
		}
		return OK("%s", sb.String())
	}
	if a.ID == 0 {
		return Fail("action %q needs the task id (see terminal(action=\"list\")).", a.Action)
	}
	p, err := m.Get(a.ID)
	if err != nil {
		return Fail("%v", err)
	}
	switch a.Action {
	case "output":
		var data []byte
		var lost bool
		if a.All {
			data, _, lost = p.Output(0)
		} else {
			data, lost = p.Unread()
		}
		return OK("%s", statusLine(p, data, lost))
	case "input":
		if a.Input == "" {
			return Fail("input is empty")
		}
		s := a.Input
		if a.Newline == nil || *a.Newline {
			s += "\n"
		}
		if err := p.SendInput(s); err != nil {
			return Fail("cannot send input to task #%d: %v", a.ID, err)
		}
		p.Wait(ctx, 1500*time.Millisecond) // let it react before reading
		data, lost := p.Unread()
		return OK("Input sent.\n%s", statusLine(p, data, lost))
	case "wait":
		d := a.Seconds
		if d <= 0 {
			d = 60
		}
		d = min(d, 600)
		p.Wait(ctx, time.Duration(d)*time.Second)
		data, lost := p.Unread()
		return OK("%s", statusLine(p, data, lost))
	case "kill":
		p.Kill()
		p.Wait(ctx, 5*time.Second)
		data, lost := p.Unread()
		return OK("Task #%d stopped.\n%s", a.ID, statusLine(p, data, lost))
	}
	return Fail("unknown action %q", a.Action)
}

func statusLine(p *Proc, data []byte, lost bool) string {
	i := p.Info()
	var sb strings.Builder
	if lost {
		sb.WriteString("[older output was discarded: only the most recent 4 MB is kept]\n")
	}
	sb.WriteString(decode(data))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		sb.WriteString("\n")
	}
	if len(data) == 0 {
		sb.WriteString("(no new output)\n")
	}
	if i.Running {
		fmt.Fprintf(&sb, "[task #%d is running]", i.ID)
	} else {
		fmt.Fprintf(&sb, "[task #%d exited with code %d]", i.ID, i.ExitCode)
	}
	return sb.String()
}

// RegisterCommandTools adds run_command and terminal to r.
func RegisterCommandTools(r *Registry) {
	r.Register(runCommand{})
	r.Register(terminal{})
}
