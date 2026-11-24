package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/session"
)

func TestSubagentWorksInAFreshContextAndReturnsOnlyItsReport(t *testing.T) {
	f := newFixture(t,
		callTool("p1", "subagent", map[string]any{"task": "List the workspace and tell me how many files there are."}),
		callTool("k1", "list_dir", map[string]any{}), // the sub-agent's first move
		say("The workspace holds 1 file: data.txt. Report complete."),
		say("The sub-agent found 1 file."),
	)
	f.writeFile("data.txt", "x")
	f.start("s1")
	if err := f.run(Prompt{Text: "How many files do I have? Delegate the counting."}); err != nil {
		t.Fatal(err)
	}
	if f.model.Calls() != 4 {
		t.Fatalf("calls: %d", f.model.Calls())
	}
	// the child's first request carries ONLY its task: none of the parent's conversation
	child := f.model.Request(1)
	if len(child.Messages) != 1 || !strings.Contains(child.Messages[0].Text(), "List the workspace") {
		t.Fatalf("child context: %+v", child.Messages)
	}
	if strings.Contains(f.model.Body(1), "Delegate the counting") {
		t.Fatal("the parent's prompt leaked into the sub-agent")
	}
	if !strings.Contains(child.System, "Sub-agent role") {
		t.Fatal("the sub-agent must be told its role")
	}
	// the parent receives the report plus a usage line, not the child's transcript
	res := resultText(f.messages(), "p1")
	contains(t, res, "The workspace holds 1 file: data.txt", "report")
	contains(t, res, "[sub-1:", "usage line")
	if strings.Contains(res, "list_dir") {
		t.Fatal("the child's tool activity must stay in the child")
	}
	// the child is a real session, stored inside the parent's
	if _, err := os.Stat(filepath.Join(f.eng.Store().Dir, "subagents", "sub-1", "events.jsonl")); err != nil {
		t.Fatalf("child session not persisted: %v", err)
	}
	// its tokens count towards the parent's totals
	if c := f.eng.Counters(); c.Requests != 4 {
		t.Fatalf("parent counters must include the child's requests: %+v", c)
	}
	// the sub-agent cannot recurse
	for _, spec := range child.Tools {
		if spec.Name == "subagent" {
			t.Fatal("a sub-agent must not be offered the subagent tool")
		}
	}
}

func TestSubagentToolsAreRestricted(t *testing.T) {
	f := newFixture(t,
		callTool("p1", "subagent", map[string]any{"task": "try to write a file"}),
		callTool("k1", "write_file", map[string]any{"path": "x.txt", "content": "nope"}), // not in the default read-only set
		say("I could not write."),
		say("parent done"),
	)
	f.start("s1")
	if err := f.run(Prompt{Text: "go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.ws, "x.txt")); err == nil {
		t.Fatal("a read-only sub-agent wrote a file")
	}
	names := []string{}
	for _, s := range f.model.Request(1).Tools {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "find_files,list_dir,search_text,view_file" {
		t.Fatalf("default sub-agent tools: %s", got)
	}

	// asking for write access explicitly grants it
	f2 := newFixture(t,
		callTool("p1", "subagent", map[string]any{"task": "write it", "tools": []string{"write_file"}}),
		callTool("k1", "write_file", map[string]any{"path": "y.txt", "content": "yes"}),
		say("written"),
		say("parent done"),
	)
	f2.start("s2")
	if err := f2.run(Prompt{Text: "go"}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(f2.ws, "y.txt")); err != nil || string(b) != "yes" {
		t.Fatalf("explicitly granted write: %q %v", b, err)
	}
}

func TestSubagentArgumentErrorsAreToolOutput(t *testing.T) {
	f := newFixture(t,
		callTool("p1", "subagent", map[string]any{"task": "x", "tools": []string{"nonexistent"}}),
		callTool("p2", "subagent", map[string]any{"task": "x", "tools": []string{"subagent"}}),
		callTool("p3", "subagent", map[string]any{"task": "   "}),
		say("done"),
	)
	f.start("s1")
	if err := f.run(Prompt{Text: "go"}); err != nil {
		t.Fatal(err)
	}
	msgs := f.messages()
	contains(t, resultText(msgs, "p1"), "unknown or disabled tool(s) for the sub-agent: nonexistent", "unknown tool")
	contains(t, resultText(msgs, "p2"), "cannot start further sub-agents", "recursion")
	contains(t, resultText(msgs, "p3"), "task is empty", "empty task")
	if f.model.Calls() != 4 {
		t.Fatalf("no sub-agent should have run: %d model calls", f.model.Calls())
	}
}

func TestSubagentKeepsMediaOutOfTheParentContext(t *testing.T) {
	f := newFixture(t,
		callTool("p1", "subagent", map[string]any{"task": "Look at pic.png and describe it."}),
		callTool("k1", "view_file", map[string]any{"path": "pic.png"}),
		say("A red rocket on a dark background."),
		say("The sub-agent says it is a red rocket."),
	)
	f.image("pic.png", 5_004)
	f.start("s1")
	if err := f.run(Prompt{Text: "What is in pic.png?"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.model.Body(2), "inlineData") {
		t.Fatal("the sub-agent should have seen the image")
	}
	for _, i := range []int{0, 3} {
		if strings.Contains(f.model.Body(i), "inlineData") {
			t.Fatalf("parent request %d contains media: the whole point of a sub-agent is to keep it out", i+1)
		}
	}
}

func TestStoppingTheParentStopsTheSubagent(t *testing.T) {
	started := make(chan struct{}, 1)
	f := newFixture(t,
		callTool("p1", "subagent", map[string]any{"task": "long task"}),
		blockUntilCancelled(started), // the sub-agent's model call hangs
	)
	eng := f.start("s1")
	if err := eng.Submit(Prompt{Text: "go"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the sub-agent never started")
	}
	eng.Stop()
	if err := f.wait(); err != nil {
		t.Fatal(err)
	}
	if st := f.lastStatus(); st.State != "stopped" {
		t.Fatalf("status: %+v", st)
	}
	contains(t, resultText(f.messages(), "p1"), "cancelled", "the parent's call must still get a result")
	// the child shuts down on its own and releases its session lock
	childDir := filepath.Join(f.eng.Store().Dir, "subagents", "sub-1")
	deadline := time.Now().Add(10 * time.Second)
	for session.ReadLock(childDir) != nil {
		if time.Now().After(deadline) {
			t.Fatal("the sub-agent is still running after its parent was stopped")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
