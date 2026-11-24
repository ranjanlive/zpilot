package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ranjanlive/zpilot/zengine/internal/contextmgr"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
)

func (f *fixture) cmd(line string) CommandResult {
	f.t.Helper()
	return f.eng.HandleCommand(context.Background(), line)
}

func TestNonCommandsAreNotHandled(t *testing.T) {
	f := newFixture(t)
	f.start("s1")
	for _, l := range []string{"hello", "", "  ", "/", "what about /this"} {
		if r := f.cmd(l); r.Handled {
			t.Errorf("%q must be treated as a normal prompt", l)
		}
	}
	if r := f.cmd("/nonsense"); !r.Handled || !strings.Contains(r.Reply, "Unknown command /nonsense") {
		t.Fatalf("unknown: %+v", r)
	}
}

func TestBuiltinCommands(t *testing.T) {
	f := newFixture(t)
	f.start("s1")

	help := f.cmd("/help").Reply
	for _, c := range []string{"/stop", "/note", "/compact", "/workspace", "/exit", "/new"} {
		if !strings.Contains(help, c) {
			t.Errorf("help lacks %s:\n%s", c, help)
		}
	}
	if r := f.cmd("/tools"); !strings.Contains(r.Reply, "view_file") || !strings.Contains(r.Reply, "subagent") {
		t.Fatalf("tools: %+v", r)
	}
	if r := f.cmd("/status"); !strings.Contains(r.Reply, "State: idle") || !strings.Contains(r.Reply, "Session: s1") {
		t.Fatalf("status: %+v", r)
	}
	if r := f.cmd("/tasks"); r.Reply != "No background tasks." {
		t.Fatalf("tasks: %+v", r)
	}

	// /note queues a note without waking the agent
	if r := f.cmd("/note remember the budget is 5k"); !strings.Contains(r.Reply, "Noted") {
		t.Fatalf("note: %+v", r)
	}
	pending := contextmgr.PendingNotes(f.events())
	if len(pending) != 1 || pending[0].Text != "[info: remember the budget is 5k]" || pending[0].State == "wake" {
		t.Fatalf("pending notes: %+v", pending)
	}
	if r := f.cmd("/note"); !strings.Contains(r.Reply, "Usage") {
		t.Fatalf("note without text: %+v", r)
	}

	// client-level commands only set flags; the engine itself keeps running
	if r := f.cmd("/exit"); !r.Exit || r.Shutdown {
		t.Fatalf("/exit must detach without stopping the engine: %+v", r)
	}
	if r := f.cmd("/quit"); !r.Exit || !r.Shutdown {
		t.Fatalf("/quit: %+v", r)
	}
	if r := f.cmd("/new"); !r.NewSession || !r.Exit {
		t.Fatalf("/new: %+v", r)
	}
	if r := f.cmd("/STOP"); !r.Handled {
		t.Fatalf("commands are case-insensitive: %+v", r)
	}
}

func TestWorkspaceCommandAnnouncesTheMove(t *testing.T) {
	f := newFixture(t, say("ok"))
	f.start("s1")
	system := f.eng.Store().Events()[0].Text
	other := filepath.Join(filepath.Dir(f.ws), "other")
	os.MkdirAll(other, 0o755)
	os.WriteFile(filepath.Join(other, "here.txt"), []byte("x"), 0o644)

	if r := f.cmd("/workspace " + other); !strings.Contains(r.Reply, other) {
		t.Fatalf("workspace: %+v", r)
	}
	if r := f.cmd("/workspace"); !strings.Contains(r.Reply, other) {
		t.Fatalf("show workspace: %+v", r)
	}
	if r := f.cmd("/workspace " + filepath.Join(other, "missing")); !strings.Contains(r.Reply, "not a directory") {
		t.Fatalf("bad path: %+v", r)
	}
	if f.eng.Store().Meta().Workspace != other {
		t.Fatal("meta not updated")
	}
	if err := f.run(Prompt{Text: "list it"}); err != nil {
		t.Fatal(err)
	}
	contains(t, f.model.Body(0), "the workspace changed from", "note reaches the model")
	if f.model.Request(0).System != system {
		t.Fatal("the system prompt must not be edited by a workspace change")
	}
	// tools now resolve against the new workspace
	if _, err := f.eng.toolEnv.ResolvePath("here.txt", false); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.eng.toolEnv.ResolvePath("here.txt", false); filepath.Dir(p) != other {
		t.Fatalf("resolved against the old workspace: %s", p)
	}
}

func TestCustomCommandsExpandIntoPrompts(t *testing.T) {
	f := newFixture(t, say("reviewed"), say("explained"))
	os.WriteFile(filepath.Join(f.home.CommandsDir(), "explain.md"), []byte("---\ndescription: Explain a thing simply\n---\nExplain $ARGS to a beginner, in two sentences.\n"), 0o644)
	os.WriteFile(filepath.Join(f.home.CommandsDir(), "bad name.md"), []byte("ignored"), 0o644)
	f.start("s1")

	if !strings.Contains(f.cmd("/help").Reply, "/explain") || !strings.Contains(f.cmd("/help").Reply, "Explain a thing simply") {
		t.Fatalf("help must list custom commands:\n%s", f.cmd("/help").Reply)
	}
	if strings.Contains(f.cmd("/help").Reply, "bad name") {
		t.Fatal("invalid command file names must be ignored")
	}

	// the shipped /review command has no $ARGS: arguments are appended
	if r := f.cmd("/review focus on error handling"); !strings.Contains(r.Reply, "Running /review") {
		t.Fatalf("review: %+v", r)
	}
	if err := f.wait(); err != nil {
		t.Fatal(err)
	}
	contains(t, f.model.Body(0), "Review the changes in the working tree", "review prompt")
	contains(t, f.model.Body(0), "focus on error handling", "appended arguments")

	if r := f.cmd("/explain recursion"); !strings.Contains(r.Reply, "Running /explain (Explain a thing simply)") {
		t.Fatalf("explain: %+v", r)
	}
	if err := f.wait(); err != nil {
		t.Fatal(err)
	}
	contains(t, f.model.Body(1), "Explain recursion to a beginner, in two sentences.", "$ARGS substitution")
	if strings.Contains(f.model.Body(1), "description:") {
		t.Fatal("frontmatter must not be sent to the model")
	}
}

func TestCompactCommand(t *testing.T) {
	f := newFixture(t, say("hello"), say("SUMMARY via command"))
	f.start("s1")
	if err := f.run(Prompt{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if r := f.cmd("/compact"); !strings.Contains(r.Reply, "compacted") {
		t.Fatalf("compact: %+v", r)
	}
	comps := f.kinds(session.KindCompaction)
	if len(comps) != 1 || comps[0].Compaction.Reason != "requested by the user" || !strings.Contains(comps[0].Compaction.Summary, "SUMMARY via command") {
		t.Fatalf("compaction: %+v", comps)
	}
	if st, _ := f.eng.State(); st != StateIdle {
		t.Fatalf("state after /compact: %s", st)
	}
}

func TestCompactRefusedWhileWorking(t *testing.T) {
	started := make(chan struct{}, 1)
	f := newFixture(t, blockUntilCancelled(started))
	eng := f.start("s1")
	if err := eng.Submit(Prompt{Text: "long"}); err != nil {
		t.Fatal(err)
	}
	<-started
	if r := f.cmd("/compact"); !strings.Contains(r.Reply, "working") {
		t.Fatalf("compact while working: %+v", r)
	}
	if r := f.cmd("/stop"); !strings.Contains(r.Reply, "Stopping") {
		t.Fatalf("stop: %+v", r)
	}
	if err := f.wait(); err != nil {
		t.Fatal(err)
	}
	if f.lastStatus().State != "stopped" {
		t.Fatalf("status: %+v", f.lastStatus())
	}
}
