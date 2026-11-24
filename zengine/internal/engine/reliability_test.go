package engine

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"github.com/ranjanlive/zpilot/zengine/internal/contextmgr"
	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/provider"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
)

func TestSubagentResumeUsesFreshConversation(t *testing.T) {
	f := newFixture(t,
		callTool("p1", "subagent", map[string]any{"task": "FIRST_UNIQUE_TASK"}), say("FIRST_UNIQUE_REPORT"), say("done"),
		callTool("p2", "subagent", map[string]any{"task": "SECOND_UNIQUE_TASK"}), say("new report"), say("done again"),
	)
	f.start("resume")
	if e := f.run(Prompt{Text: "delegate first"}); e != nil {
		t.Fatal(e)
	}
	if e := f.eng.Close(); e != nil {
		t.Fatal(e)
	}
	f.start("resume")
	if e := f.run(Prompt{Text: "delegate second"}); e != nil {
		t.Fatal(e)
	}
	body := f.model.Body(4)
	if strings.Contains(body, "FIRST_UNIQUE_TASK") || strings.Contains(body, "FIRST_UNIQUE_REPORT") {
		t.Fatal("child reused old context")
	}
	if !strings.Contains(body, "SECOND_UNIQUE_TASK") {
		t.Fatal("child lacks its new task")
	}
	for _, id := range []string{"sub-1", "sub-2"} {
		if _, e := os.Stat(filepath.Join(f.eng.Store().Dir, "subagents", id, "events.jsonl")); e != nil {
			t.Fatal(e)
		}
	}
}

func TestIndexedContextPreservesRenderingAndPendingNotes(t *testing.T) {
	f := newFixture(t)
	f.start("index")
	store := f.eng.Store()
	note, e := store.Append(session.Event{Kind: session.KindNote, Text: "pending note"})
	if e != nil {
		t.Fatal(e)
	}
	for range 100 {
		if _, e = store.Append(session.Event{Kind: session.KindStatus, State: "done"}); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = store.Append(session.Event{Kind: session.KindMessage, TurnNo: 7, Message: &llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "old prompt"}}}}); e != nil {
		t.Fatal(e)
	}
	if _, e = store.Append(session.Event{Kind: session.KindCompaction, Compaction: &session.Compaction{Summary: "summary", KeptPrompts: []string{"old prompt"}}}); e != nil {
		t.Fatal(e)
	}
	full := contextmgr.Build(store.Events())
	indexed := contextmgr.Build(store.ContextEvents())
	if !reflect.DeepEqual(full, indexed) {
		t.Fatalf("indexed context differs: %+v / %+v", full, indexed)
	}
	if len(store.ContextEvents()) >= len(store.Events()) {
		t.Fatal("old transcript still copied for context")
	}
	if len(store.PendingNotes()) != 1 {
		t.Fatal("pre-compaction note was lost")
	}
	if _, e = store.Append(session.Event{Kind: session.KindMessage, Consumed: []int64{note.Seq}}); e != nil {
		t.Fatal(e)
	}
	if len(store.PendingNotes()) != 0 {
		t.Fatal("consumed note remains pending")
	}
}

func TestCompactionRejectsIncompleteSummary(t *testing.T) {
	for _, finish := range []llm.FinishReason{llm.FinishLength, llm.FinishFiltered} {
		t.Run(string(finish), func(t *testing.T) {
			summary := func(ctx context.Context, req provider.Request, b *provider.Built) (*provider.Response, error) {
				r, e := say("incomplete summary")(ctx, req, b)
				r.Message.FinishReason = finish
				return r, e
			}
			f := newFixture(t, say("answer"), summary, say("still answering"))
			f.start("compact")
			if e := f.run(Prompt{Text: "PRESERVE_THIS_CONSTRAINT"}); e != nil {
				t.Fatal(e)
			}
			if e := f.eng.Compact(context.Background()); e == nil {
				t.Fatal("incomplete summary accepted")
			}
			if len(f.kinds(session.KindCompaction)) != 0 {
				t.Fatal("history replaced by incomplete summary")
			}
			if e := f.run(Prompt{Text: "continue"}); e != nil {
				t.Fatal(e)
			}
			if !strings.Contains(f.model.Body(2), "PRESERVE_THIS_CONSTRAINT") {
				t.Fatal("constraint lost")
			}
		})
	}
}

func TestWorkspaceChangeRefusedDuringTurn(t *testing.T) {
	started := make(chan struct{}, 1)
	f := newFixture(t, blockUntilCancelled(started))
	f.start("workspace")
	if e := f.eng.Submit(Prompt{Text: "work"}); e != nil {
		t.Fatal(e)
	}
	<-started
	other := t.TempDir()
	if e := f.eng.SetWorkspace(other); e == nil {
		t.Fatal("active workspace changed")
	}
	oldEnv := f.eng.toolEnv
	f.eng.Stop()
	if e := f.wait(); e != nil {
		t.Fatal(e)
	}
	if e := f.eng.SetWorkspace(other); e != nil {
		t.Fatal(e)
	}
	if f.eng.Workspace() != other || oldEnv.Workspace == other {
		t.Fatal("old tool environment was mutated")
	}
}
