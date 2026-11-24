package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/session"
	"github.com/ranjanlive/zpilot/zengine/internal/tools"
)

// CommandResult is the outcome of a slash command.
type CommandResult struct {
	// Handled is false when the line is not a command (send it as a prompt).
	Handled bool `json:"handled"`
	// Reply is text to show the user.
	Reply string `json:"reply,omitempty"`
	// Exit asks the client to leave: /exit detaches an interface but never
	// stops the engine, which keeps working headless.
	Exit bool `json:"exit,omitempty"`
	// Shutdown asks the engine process itself to end (/quit).
	Shutdown bool `json:"shutdown,omitempty"`
	// NewSession asks the client to switch to a fresh session (/new).
	NewSession bool `json:"new_session,omitempty"`
}

var commandName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// builtinCommands documents the commands every interface supports.
var builtinCommands = []struct{ name, help string }{
	{"help", "show this list"},
	{"stop", "interrupt the agent's current work (the engine keeps running)"},
	{"note <text>", "send a note to the agent; it is attached to the end of the next request"},
	{"compact", "summarise the conversation now to free context"},
	{"status", "show state, tokens and background tasks"},
	{"tasks", "list background tasks"},
	{"tools", "list the tools the agent can use"},
	{"workspace <path>", "move the agent to another workspace (it is told, the system prompt is untouched)"},
	{"resume", "retry now while paused for network trouble"},
	{"exit", "leave this interface; the agent keeps working in the background"},
	{"quit", "stop the agent process and leave"},
	{"new", "start a new session"},
}

// HandleCommand runs a slash command line. Lines that do not start with "/"
// are not commands. It never blocks on the model for longer than the command
// itself needs (/compact waits for its summary).
func (e *Engine) HandleCommand(ctx context.Context, line string) CommandResult {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "/") || line == "/" {
		return CommandResult{}
	}
	name, args, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	name, args = strings.ToLower(name), strings.TrimSpace(args)
	r := CommandResult{Handled: true}

	switch name {
	case "help", "?":
		r.Reply = e.helpText()
	case "stop":
		e.Stop()
		r.Reply = "Stopping the agent's current work."
	case "resume":
		e.Resume()
		r.Reply = "Retrying now."
	case "note":
		if args == "" {
			r.Reply = "Usage: /note <text>"
			break
		}
		if err := e.Note(args, false); err != nil {
			r.Reply = "Could not save the note: " + err.Error()
			break
		}
		r.Reply = "Noted. It will be attached to the end of the agent's next request."
	case "compact":
		if st, _ := e.State(); st == StateWorking || st == StateCompacting {
			r.Reply = "The agent is working; use /stop first, or wait for it to finish."
			break
		}
		if err := e.Compact(ctx); err != nil {
			r.Reply = "Compaction failed: " + err.Error()
			break
		}
		r.Reply = "Conversation compacted."
	case "status":
		r.Reply = e.statusText()
	case "tasks":
		r.Reply = e.tasksText()
	case "tools":
		r.Reply = "Tools: " + strings.Join(e.registry.Names(), ", ")
	case "workspace":
		if args == "" {
			r.Reply = "Workspace: " + e.Workspace()
			break
		}
		if err := e.SetWorkspace(args); err != nil {
			r.Reply = "Cannot change the workspace: " + err.Error()
			break
		}
		r.Reply = "Workspace is now " + e.Workspace()
	case "exit":
		r.Exit = true
		r.Reply = "Leaving; the agent keeps running in the background."
	case "quit":
		r.Shutdown = true
		r.Exit = true
		r.Reply = "Stopping the agent."
	case "new":
		r.NewSession = true
		r.Exit = true
	default:
		prompt, desc, ok := e.customCommand(name, args)
		if !ok {
			r.Reply = fmt.Sprintf("Unknown command /%s. Type /help for the list.", name)
			break
		}
		if err := e.Submit(Prompt{Text: prompt}); err != nil {
			r.Reply = "Could not start /" + name + ": " + err.Error()
			break
		}
		r.Reply = "Running /" + name
		if desc != "" {
			r.Reply += " (" + desc + ")"
		}
	}
	return r
}

func (e *Engine) helpText() string {
	var sb strings.Builder
	sb.WriteString("Commands:\n")
	for _, c := range builtinCommands {
		fmt.Fprintf(&sb, "  /%-18s %s\n", c.name, c.help)
	}
	if custom := e.customCommandNames(); len(custom) > 0 {
		sb.WriteString("Custom commands (from the home's commands folder):\n")
		for _, n := range custom {
			_, desc, _ := e.customCommand(n, "")
			fmt.Fprintf(&sb, "  /%-18s %s\n", n, desc)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func (e *Engine) statusText() string {
	st, detail := e.State()
	c := e.Counters()
	est := ""
	if c.Estimated {
		est = " (estimated)"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "State: %s", st)
	if detail != "" {
		fmt.Fprintf(&sb, " (%s)", detail)
	}
	fmt.Fprintf(&sb, "\nSession: %s   Workspace: %s", e.SessionID(), e.Workspace())
	fmt.Fprintf(&sb, "\nContext: %d tokens%s", c.Context, est)
	if at := e.compactAt(); at > 0 {
		fmt.Fprintf(&sb, " (compacts at %d)", at)
	}
	fmt.Fprintf(&sb, "\nTotals: %d request(s), %d prompt / %d answer / %d thought tokens, %d cached", c.Requests, c.Prompt, c.Completion, c.Thought, c.Cached)
	if n := e.RunningTasks(); n > 0 {
		fmt.Fprintf(&sb, "\nBackground tasks running: %d", n)
	}
	return sb.String()
}

func (e *Engine) tasksText() string {
	infos := e.procs.List()
	if len(infos) == 0 {
		return "No background tasks."
	}
	var sb strings.Builder
	for _, i := range infos {
		state := "running"
		if !i.Running {
			state = fmt.Sprintf("exited %d", i.ExitCode)
		}
		fmt.Fprintf(&sb, "#%d %-10s %s  %s\n", i.ID, state, time.Since(i.Started).Round(time.Second), i.Command)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// SetWorkspace moves the agent to another directory. The system prompt keeps
// its original workspace text (it is part of the cached prefix); the model is
// told through a note instead, and the move is recorded in the session.
func (e *Engine) SetWorkspace(path string) error {
	if !e.workspaceMu.TryLock() {
		return fmt.Errorf("the agent is working; stop it or wait before changing the workspace")
	}
	defer e.workspaceMu.Unlock()
	if !filepath.IsAbs(path) {
		path = filepath.Join(e.workspace, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("%s is not a directory", abs)
	}
	e.mu.Lock()
	old := e.workspace
	if old == abs {
		e.mu.Unlock()
		return nil
	}
	e.workspace = abs
	previousEnv := e.toolEnv
	nextEnv := *previousEnv
	nextEnv.Workspace = abs
	e.toolEnv = &nextEnv // background and timed-out tools retain their original environment
	e.mu.Unlock()
	if err := e.store.UpdateMeta(func(m *session.Meta) { m.Workspace = abs }); err != nil {
		e.mu.Lock()
		e.workspace, e.toolEnv = old, previousEnv
		e.mu.Unlock()
		return err
	}
	return e.Note(fmt.Sprintf("[info: the workspace changed from %s to %s. Relative paths now resolve in the new workspace.]", old, abs), false)
}

// ---- custom commands ----

func (e *Engine) customCommandNames() []string {
	entries, err := os.ReadDir(e.home.CommandsDir())
	if err != nil {
		return nil
	}
	var names []string
	for _, ent := range entries {
		n := strings.TrimSuffix(ent.Name(), ".md")
		if !ent.IsDir() && strings.HasSuffix(ent.Name(), ".md") && commandName.MatchString(n) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// customCommand expands <home>/commands/<name>.md into a prompt. The file may
// start with frontmatter ("description: ..."), and may use $ARGS for whatever
// the user typed after the command; without $ARGS the text is appended.
func (e *Engine) customCommand(name, args string) (prompt, description string, ok bool) {
	if !commandName.MatchString(name) {
		return "", "", false
	}
	b, err := os.ReadFile(filepath.Join(e.home.CommandsDir(), name+".md"))
	if err != nil {
		return "", "", false
	}
	meta, body := tools.ParseFrontmatter(string(b))
	body = strings.TrimSpace(body)
	if body == "" {
		return "", "", false
	}
	if strings.Contains(body, "$ARGS") {
		body = strings.ReplaceAll(body, "$ARGS", args)
	} else if args != "" {
		body += "\n\n" + args
	}
	return body, meta["description"], true
}
