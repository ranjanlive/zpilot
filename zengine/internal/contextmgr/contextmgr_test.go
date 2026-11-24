package contextmgr

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
)

func text(role llm.Role, s string) llm.Message {
	return llm.Message{Role: role, Parts: []llm.Part{{Type: llm.PartText, Text: s}}}
}

func call(id, name string) llm.Part {
	return llm.Part{Type: llm.PartToolCall, Call: &llm.ToolCall{ID: id, Name: name, Args: json.RawMessage(`{}`)}}
}

func result(id, name, out string, media ...llm.Media) llm.Part {
	return llm.Part{Type: llm.PartToolResult, Result: &llm.ToolResult{CallID: id, Name: name, Output: out, Media: media}}
}

func ev(kind session.Kind, turn int) session.Event { return session.Event{Kind: kind, TurnNo: turn} }

func msgEv(turn int, m llm.Message) session.Event {
	e := ev(session.KindMessage, turn)
	e.Message = &m
	return e
}

func number(events []session.Event) []session.Event {
	for i := range events {
		events[i].Seq = int64(i + 1)
	}
	return events
}

func img(blob string, size int64, path string) llm.Media {
	return llm.Media{Kind: llm.MediaImage, MIME: "image/png", Blob: blob, Size: size, Name: path, Path: path}
}

func TestBuildBasics(t *testing.T) {
	sys := ev(session.KindSystem, 0)
	sys.Text = "SYS"
	events := number([]session.Event{
		sys,
		msgEv(1, text(llm.RoleUser, "hi")),
		msgEv(1, text(llm.RoleAssistant, "hello")),
		{Kind: session.KindStatus, State: "working"}, // UI-only events never reach the model
		msgEv(2, text(llm.RoleUser, "again")),
	})
	c := Build(events)
	if c.System != "SYS" || len(c.Messages) != 3 || c.Turn != 2 || len(c.Fixes) != 0 {
		t.Fatalf("ctx: %+v", c)
	}
}

func TestBuildAppliesPruneWithoutMutatingLog(t *testing.T) {
	m := llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
		{Type: llm.PartText, Text: "look"},
		{Type: llm.PartMedia, Media: ptr(img("b1", 100, "/w/a.png"))},
	}}
	prune := ev(session.KindPrune, 1)
	prune.Blobs = []string{"b1"}
	events := number([]session.Event{msgEv(1, m), prune})
	c := Build(events)
	got := c.Messages[0].Parts[1].Media
	if got.Blob != "" || got.Path != "/w/a.png" || got.Size != 100 {
		t.Fatalf("prune must clear the payload but keep metadata: %+v", got)
	}
	if len(c.Attachments) != 0 {
		t.Fatalf("pruned attachment still listed: %+v", c.Attachments)
	}
	if events[0].Message.Parts[1].Media.Blob != "b1" {
		t.Fatal("Build mutated the event log")
	}
}

func ptr[T any](v T) *T { return &v }

func TestBuildAfterCompaction(t *testing.T) {
	comp := ev(session.KindCompaction, 2)
	comp.Compaction = &session.Compaction{Summary: "did things", KeptPrompts: []string{"latest ask"}, RemovedPaths: []string{"/w/a.png"}, Reason: "tokens"}
	events := number([]session.Event{
		msgEv(1, text(llm.RoleUser, "old")),
		msgEv(1, text(llm.RoleAssistant, "old reply")),
		comp,
		msgEv(3, text(llm.RoleUser, "new")),
	})
	c := Build(events)
	if len(c.Messages) != 2 {
		t.Fatalf("history before compaction must be gone: %d messages", len(c.Messages))
	}
	first := c.Messages[0].Text()
	for _, want := range []string{"context compacted", "did things", "/w/a.png", "latest ask", "Continue the work"} {
		if !strings.Contains(first, want) {
			t.Errorf("resume message lacks %q:\n%s", want, first)
		}
	}
	if c.Messages[1].Text() != "new" {
		t.Fatal("post-compaction message lost")
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	events := number([]session.Event{
		msgEv(1, llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "a"}, {Type: llm.PartMedia, Media: ptr(img("b", 5, "p"))}}}),
		msgEv(1, llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{call("c1", "t")}}),
	})
	a, _ := json.Marshal(Build(events))
	b, _ := json.Marshal(Build(events))
	if string(a) != string(b) {
		t.Fatal("two renders of the same log differ (breaks prompt caching)")
	}
}

func TestPendingNotes(t *testing.T) {
	n1, n2 := ev(session.KindNote, 0), ev(session.KindNote, 0)
	n1.Text, n2.Text = "one", "two"
	m := msgEv(1, text(llm.RoleUser, "x"))
	m.Consumed = []int64{1}
	events := number([]session.Event{n1, m, n2})
	pend := PendingNotes(events)
	if len(pend) != 1 || pend[0].Text != "two" {
		t.Fatalf("pending: %+v", pend)
	}
}

func TestRepairAddsMissingAndDropsOrphans(t *testing.T) {
	// assistant calls two tools, only one result present
	msgs := []llm.Message{
		text(llm.RoleUser, "go"),
		{Role: llm.RoleAssistant, Parts: []llm.Part{call("a", "t1"), call("b", "t2")}},
		{Role: llm.RoleUser, Parts: []llm.Part{result("a", "t1", "ok"), result("zzz", "ghost", "orphan")}},
	}
	out, fixes := Repair(msgs)
	if len(fixes) < 2 {
		t.Fatalf("fixes: %v", fixes)
	}
	user := out[2].Parts
	var ids []string
	for _, p := range user {
		if p.Type == llm.PartToolResult {
			ids = append(ids, p.Result.CallID)
		}
	}
	if strings.Join(ids, ",") != "b,a" { // synthetic results lead, orphan "zzz" is gone
		t.Fatalf("result ids: %v", ids)
	}
	if !user[0].Result.IsError || user[0].Result.Output != InterruptedOutput {
		t.Fatalf("synthetic result: %+v", user[0].Result)
	}
}

func TestRepairDanglingTailAndAssistantAfterCalls(t *testing.T) {
	tail := []llm.Message{text(llm.RoleUser, "go"), {Role: llm.RoleAssistant, Parts: []llm.Part{call("a", "t")}}}
	out, fixes := Repair(tail)
	if len(out) != 3 || out[2].Role != llm.RoleUser || len(fixes) != 1 {
		t.Fatalf("dangling tail: %+v %v", out, fixes)
	}
	if d := DanglingCalls(tail); len(d) != 1 || d[0].ID != "a" {
		t.Fatalf("DanglingCalls: %+v", d)
	}
	if DanglingCalls(out) != nil {
		t.Fatal("repaired tail must report no dangling calls")
	}
	two := []llm.Message{
		text(llm.RoleUser, "go"),
		{Role: llm.RoleAssistant, Parts: []llm.Part{call("a", "t")}},
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: "second assistant turn"}}},
	}
	out, _ = Repair(two)
	if len(out) != 4 || out[2].Role != llm.RoleUser {
		t.Fatalf("results must be inserted between assistant turns: %+v", out)
	}
}

func TestRepairLeavesValidHistoryAlone(t *testing.T) {
	msgs := []llm.Message{
		text(llm.RoleUser, "go"),
		{Role: llm.RoleAssistant, Parts: []llm.Part{call("a", "t")}},
		{Role: llm.RoleUser, Parts: []llm.Part{result("a", "t", "ok")}},
		text(llm.RoleAssistant, "done"),
	}
	out, fixes := Repair(msgs)
	if len(fixes) != 0 || len(out) != 4 {
		t.Fatalf("valid history changed: %v %d", fixes, len(out))
	}
	// empty messages are removed, orphan results at the start are dropped
	out, fixes = Repair([]llm.Message{{Role: llm.RoleUser}, {Role: llm.RoleUser, Parts: []llm.Part{result("q", "t", "x"), {Type: llm.PartText, Text: "keep"}}}})
	if len(out) != 1 || out[0].Text() != "keep" || len(fixes) != 2 {
		t.Fatalf("got %+v %v", out, fixes)
	}
}

func attachments() Context {
	return Context{Turn: 6, Attachments: []Attachment{
		{Blob: "old1", Size: 3 << 20, TurnNo: 1, Path: "/w/1.png"},
		{Blob: "old2", Size: 3 << 20, TurnNo: 2, Path: "/w/2.png"},
		{Blob: "mid", Size: 3 << 20, TurnNo: 4, Path: "/w/3.png"},
		{Blob: "new", Size: 3 << 20, TurnNo: 6, Path: "/w/4.png"},
	}}
}

func TestExpiredByAgeAndBatching(t *testing.T) {
	c := attachments()
	p := Policy{MaxTurnToKeepAttachment: 3, PruneBatchBytes: 4 << 20}
	// keep turns 4..6; turns 1,2 expired = 6 MiB raw = 8 MiB wire >= 4 MiB batch
	exp := ExpiredByAge(c, p, false)
	if got := strings.Join(BlobIDs(exp), ","); got != "old1,old2" {
		t.Fatalf("expired: %s", got)
	}
	// batch not reached -> deferred (cache friendly), unless forced
	p.PruneBatchBytes = 100 << 20
	if ExpiredByAge(c, p, false) != nil {
		t.Fatal("pruning must be deferred below the batch size")
	}
	if len(ExpiredByAge(c, p, true)) != 2 {
		t.Fatal("force must prune regardless of batch")
	}
	p.MaxTurnToKeepAttachment = 0
	if ExpiredByAge(c, p, true) != nil {
		t.Fatal("0 means keep until compaction")
	}
}

func TestPlanForLimit(t *testing.T) {
	c := attachments() // 4 x 3MiB raw => 4 MiB wire each
	limit := 10 << 20
	p := Policy{BodyLimit: limit, RemoveOldOnLimit: true, AutocompactOnLimit: true}

	if plan := PlanForLimit(c, 5<<20, p); len(plan.Prune) != 0 || plan.NeedCompact {
		t.Fatalf("under limit must do nothing: %+v", plan)
	}
	// body 17 MiB: removing oldest (-4) -> 13, next (-4) -> 9 <= 10: prune two, oldest first
	plan := PlanForLimit(c, 17<<20, p)
	if got := strings.Join(BlobIDs(plan.Prune), ","); got != "old1,old2" || plan.NeedCompact || plan.EstimatedAfter > limit {
		t.Fatalf("plan: %+v", plan)
	}
	// text alone too large: removal cannot help -> compact (and prune nothing)
	plan = PlanForLimit(c, 40<<20, p)
	if !plan.NeedCompact || len(plan.Prune) != 0 {
		t.Fatalf("want compaction fallback: %+v", plan)
	}
	// removal disabled -> straight to compaction
	p.RemoveOldOnLimit = false
	plan = PlanForLimit(c, 17<<20, p)
	if !plan.NeedCompact || len(plan.Prune) != 0 {
		t.Fatalf("removal disabled: %+v", plan)
	}
	// both disabled -> caller must surface an error
	p.AutocompactOnLimit = false
	if plan = PlanForLimit(c, 17<<20, p); plan.NeedCompact || len(plan.Prune) != 0 {
		t.Fatalf("both disabled: %+v", plan)
	}
}

func TestPlanForLimitDedupesRepeatedBlob(t *testing.T) {
	c := Context{Attachments: []Attachment{
		{Blob: "same", Size: 3 << 20, TurnNo: 1}, {Blob: "same", Size: 3 << 20, TurnNo: 2}, {Blob: "other", Size: 3 << 20, TurnNo: 3},
	}}
	plan := PlanForLimit(c, 12<<20, Policy{BodyLimit: 5 << 20, RemoveOldOnLimit: true})
	// removing "same" clears both copies (8 MiB wire) -> 4 MiB, under 5
	if len(plan.Prune) != 1 || plan.Prune[0].Blob != "same" || plan.EstimatedAfter != 4<<20 {
		t.Fatalf("plan: %+v", plan)
	}
}

func TestTokenEstimation(t *testing.T) {
	msgs := []llm.Message{
		text(llm.RoleUser, strings.Repeat("a", 4000)),
		{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartMedia, Media: ptr(img("b", 1000, "x"))}}},
		{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartMedia, Media: ptr(llm.Media{Kind: llm.MediaImage, Size: 1000})}}}, // pruned: free
	}
	if got := EstimateTokens("", msgs, nil); got != 1000+1000 {
		t.Fatalf("estimate: %d", got)
	}
	u := EstimateUsage("sys", msgs, nil, text(llm.RoleAssistant, strings.Repeat("b", 400)))
	if !u.Estimated || u.CompletionTokens != 100 || u.TotalTokens != u.PromptTokens+100 {
		t.Fatalf("usage: %+v", u)
	}
}

func TestCompactionHelpers(t *testing.T) {
	msgs := []llm.Message{
		text(llm.RoleUser, "first"),
		{Role: llm.RoleAssistant, Parts: []llm.Part{call("a", "t")}},
		{Role: llm.RoleUser, Parts: []llm.Part{result("a", "t", "ok")}}, // not a prompt
		text(llm.RoleUser, "[info: context compacted]\nold resume"),     // not a prompt
		text(llm.RoleUser, "second"),
		text(llm.RoleUser, "third"),
	}
	if got := strings.Join(KeptPrompts(msgs, 2), "|"); got != "second|third" {
		t.Fatalf("kept: %s", got)
	}
	if KeptPrompts(msgs, 0) != nil {
		t.Fatal("0 keeps nothing")
	}
	req := SummaryRequest(Context{Messages: msgs})
	if len(req) != len(msgs)+1 || !strings.Contains(req[len(req)-1].Text(), "Output only the summary") {
		t.Fatal("summary request must end with the instruction")
	}
	if len(msgs) != 6 {
		t.Fatal("SummaryRequest mutated its input")
	}
	c := NewCompaction(attachments(), "  sum  ", "tokens", 1, 123, 456)
	if c.Summary != "sum" || c.RemovedBlobs != 4 || len(c.RemovedPaths) != 4 || c.TokensBefore != 123 {
		t.Fatalf("compaction: %+v", c)
	}
}
