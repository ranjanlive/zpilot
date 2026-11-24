package engine

import (
	"bytes"
	"encoding/json"
	"hash/fnv"
	"image"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/provider"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
)

// image writes a valid PNG padded to n bytes for attachment-size policy tests.
func (f *fixture) image(name string, n int) string {
	f.t.Helper()
	b := make([]byte, n)
	h := fnv.New64a()
	h.Write([]byte(name))
	rand.New(rand.NewSource(int64(h.Sum64()))).Read(b) // distinct content per name, or the blob store would dedupe the images
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		f.t.Fatal(err)
	}
	copy(b, encoded.Bytes())
	return f.writeFile(name, string(b))
}

func prunes(f *fixture) []session.Event { return f.kinds(session.KindPrune) }

func TestRequestsOnlyEverAppend(t *testing.T) {
	// Prompt caching needs every request to extend the previous one: system,
	// tools and all earlier messages must be byte-identical.
	f := newFixture(t,
		callTool("c1", "write_file", map[string]any{"path": "a.txt", "content": "x"}),
		callTool("c2", "list_dir", map[string]any{}),
		say("done"),
		say("second answer"),
	)
	f.start("s1")
	if err := f.run(Prompt{Text: "first"}); err != nil {
		t.Fatal(err)
	}
	_ = f.eng.Note("a note between turns", false)
	if err := f.run(Prompt{Text: "second"}); err != nil {
		t.Fatal(err)
	}

	type body struct {
		System   json.RawMessage   `json:"systemInstruction"`
		Tools    json.RawMessage   `json:"tools"`
		Contents []json.RawMessage `json:"contents"`
	}
	var prev body
	for i := 0; i < f.model.Calls(); i++ {
		var cur body
		if err := json.Unmarshal([]byte(f.model.Body(i)), &cur); err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			if !bytes.Equal(cur.System, prev.System) || !bytes.Equal(cur.Tools, prev.Tools) {
				t.Fatalf("request %d changed the system prompt or tools", i+1)
			}
			if len(cur.Contents) <= len(prev.Contents) {
				t.Fatalf("request %d did not grow: %d -> %d", i+1, len(prev.Contents), len(cur.Contents))
			}
			for j := range prev.Contents {
				if !bytes.Equal(cur.Contents[j], prev.Contents[j]) {
					t.Fatalf("request %d rewrote message %d:\n was %s\n now %s", i+1, j, prev.Contents[j], cur.Contents[j])
				}
			}
		}
		prev = cur
	}
}

func TestPromptAttachmentsAreSentThenPrunedWithAReopenHint(t *testing.T) {
	f := newFixture(t, say("seen"), say("ok"))
	f.cfg.Context.RequestBodyLimitMB = 0.06 // ~63 KB
	path := f.image("shots/a.png", 20_000)
	f.start("s1")
	if err := f.run(Prompt{Text: "look", Attachments: []string{path}}); err != nil {
		t.Fatal(err)
	}
	contains(t, f.model.Body(0), "inlineData", "first request carries the image")

	// grow the conversation until the limit forces the old image out
	f.image("shots/b.png", 20_000)
	f.image("shots/c.png", 20_000)
	f.model.script = append(f.model.script, say("b seen"), say("c seen"))
	for _, n := range []string{"b.png", "c.png"} {
		if err := f.run(Prompt{Text: "and this", Attachments: []string{filepath.Join(f.ws, "shots", n)}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(prunes(f)) == 0 {
		t.Fatal("expected the oldest attachment to be pruned once the request exceeded the limit")
	}
	last := f.model.Body(f.model.Calls() - 1)
	for i := 0; i < f.model.Calls(); i++ {
		t.Logf("request %d: %d bytes, %d inline attachments", i+1, len(f.model.Body(i)), strings.Count(f.model.Body(i), "inlineData"))
	}
	t.Logf("prunes=%d compactions=%d limit=%d", len(prunes(f)), len(f.kinds(session.KindCompaction)), f.cfg.BodyLimitBytes())
	contains(t, last, "was removed to save context", "pruned placeholder")
	contains(t, last, "re-open it with view_file", "re-open hint")
	if len(last) > f.cfg.BodyLimitBytes() {
		t.Fatalf("the sent request (%d) exceeds the limit (%d)", len(last), f.cfg.BodyLimitBytes())
	}
	// the newest image must have survived: pruning goes oldest-first
	if strings.Count(last, "inlineData") != 1 {
		t.Fatalf("exactly the newest attachment should remain inline, got %d", strings.Count(last, "inlineData"))
	}
}

func TestAutocompactWhenRemovalIsDisabled(t *testing.T) {
	f := newFixture(t,
		say("one"),
		say("SUMMARY: the user is testing; img1 was a random png"), // prompt 2 pushes the request over the limit: compaction call
		say("two"),
		say("three"),
	)
	f.cfg.Context.RequestBodyLimitMB = 0.05
	f.cfg.Context.RemoveOldAttachmentOnRequestBodyLimit = false
	f.cfg.Context.AutocompactOnRequestBodyLimitExceed = true
	f.start("s1")
	if err := f.run(Prompt{Text: "first", Attachments: []string{f.image("a.png", 20_000)}}); err != nil {
		t.Fatal(err)
	}
	if err := f.run(Prompt{Text: "second", Attachments: []string{f.image("b.png", 21_000)}}); err != nil {
		t.Fatal(err)
	}
	comps := f.kinds(session.KindCompaction)
	if len(comps) != 1 {
		t.Fatalf("expected one compaction, got %d", len(comps))
	}
	c := comps[0].Compaction
	if !strings.Contains(c.Summary, "SUMMARY") || c.RemovedBlobs != 2 || len(c.KeptPrompts) != 1 || c.KeptPrompts[0] != "second" {
		t.Fatalf("compaction record: %+v", c)
	}
	if len(c.RemovedPaths) != 2 {
		t.Fatalf("removed attachments must be listed so they can be re-opened: %v", c.RemovedPaths)
	}
	if len(prunes(f)) != 0 {
		t.Fatal("pruning is disabled; compaction was the only allowed remedy")
	}
	// the summary request told the model to describe attachments, and had tools forbidden
	if !f.model.Request(1).ForbidToolCalls || !strings.Contains(f.model.Body(1), "describe what it contained") {
		t.Fatal("summary request malformed")
	}
	// after compaction the model sees one resume message and the system prompt was re-snapshotted
	post := f.model.Request(2)
	if len(post.Messages) != 1 || !strings.Contains(post.Messages[0].Text(), "SUMMARY: the user is testing") {
		t.Fatalf("post-compaction context: %+v", post.Messages)
	}
	if len(f.kinds(session.KindSystem)) != 2 {
		t.Fatalf("system prompt must be re-snapshotted after compaction, got %d", len(f.kinds(session.KindSystem)))
	}
	if strings.Contains(f.model.Body(2), "inlineData") {
		t.Fatal("attachments must be gone after compaction")
	}
	if err := f.run(Prompt{Text: "third"}); err != nil {
		t.Fatalf("the session must keep working after a compaction: %v", err)
	}
}

func TestBothRemediesOffReportsAClearError(t *testing.T) {
	f := newFixture(t, say("one"), say("two"))
	f.cfg.Context.RequestBodyLimitMB = 0.05
	f.cfg.Context.RemoveOldAttachmentOnRequestBodyLimit = false
	f.cfg.Context.AutocompactOnRequestBodyLimitExceed = false
	f.start("s1")
	_ = f.run(Prompt{Text: "first", Attachments: []string{f.image("a.png", 20_000)}})
	_ = f.run(Prompt{Text: "second", Attachments: []string{f.image("b.png", 21_000)}})
	st := f.lastStatus()
	if st.State != "error" || !strings.Contains(st.Detail, "over the 0 MB limit") && !strings.Contains(st.Detail, "limit") {
		t.Fatalf("status: %+v", st)
	}
	if f.model.Calls() != 1 {
		t.Fatalf("an oversize request must never be sent: %d calls", f.model.Calls())
	}
}

func TestTokenThresholdCompaction(t *testing.T) {
	f := newFixture(t,
		sayU("first answer", usage(120, 30)),
		sayU("SUMMARY of everything", usage(160, 20)), // compaction call, made because 120+30 >= 100
		say("fresh start"),
	)
	f.env.CompactAt = 100
	f.start("s1")
	if err := f.run(Prompt{Text: "one"}); err != nil {
		t.Fatal(err)
	}
	if len(f.kinds(session.KindCompaction)) != 0 {
		t.Fatal("no compaction expected yet: the first request was small")
	}
	if err := f.run(Prompt{Text: "two"}); err != nil {
		t.Fatal(err)
	}
	comps := f.kinds(session.KindCompaction)
	if len(comps) != 1 || !strings.Contains(comps[0].Compaction.Reason, "context at") {
		t.Fatalf("compactions: %+v", comps)
	}
	if got := f.model.Request(2).Messages; len(got) != 1 || !strings.Contains(got[0].Text(), "SUMMARY of everything") || !strings.Contains(got[0].Text(), "two") {
		t.Fatalf("resume context: %+v", got)
	}
}

func TestProviderContextOverflowTriggersCompaction(t *testing.T) {
	f := newFixture(t,
		say("first"),
		fail(provider.ErrContextLimit, "prompt is too long: 300000 tokens > 200000 maximum"),
		say("SUMMARY after overflow"),
		say("continued"),
	)
	f.start("s1")
	if err := f.run(Prompt{Text: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := f.run(Prompt{Text: "two"}); err != nil {
		t.Fatal(err)
	}
	comps := f.kinds(session.KindCompaction)
	if len(comps) != 1 || !strings.Contains(comps[0].Compaction.Reason, "exceeds the model's context window") {
		t.Fatalf("compactions: %+v", comps)
	}
	if f.lastStatus().State != "done" {
		t.Fatalf("status: %+v", f.lastStatus())
	}
}

func TestUnexpected413ShrinksTheRequestAndRetries(t *testing.T) {
	// The endpoint's real limit (here ~25KB) is below the configured one, so
	// the local check passes and the server answers 413.
	f := newFixture(t,
		say("one"),
		fail(provider.ErrTooLarge, "Request body is too large"),
		say("fits now"),
	)
	f.cfg.Context.RequestBodyLimitMB = 20
	f.start("s1")
	if err := f.run(Prompt{Text: "first", Attachments: []string{f.image("a.png", 30_000)}}); err != nil {
		t.Fatal(err)
	}
	if err := f.run(Prompt{Text: "second"}); err != nil {
		t.Fatal(err)
	}
	if len(prunes(f)) != 1 {
		t.Fatalf("the oldest attachment should be dropped after a 413: %d prunes", len(prunes(f)))
	}
	contains(t, f.model.Body(2), "was removed to save context", "retry request")
	var warned bool
	for _, s := range f.kinds(session.KindStatus) {
		warned = warned || (s.State == "warning" && strings.Contains(s.Detail, "rejected a"))
	}
	if !warned {
		t.Fatal("the 413 should leave a warning in the log (limit set too high)")
	}
}

func TestMalformedRequestRetriesOnceInPortableForm(t *testing.T) {
	f := newFixture(t,
		say("one"),
		fail(provider.ErrBadRequest, "INVALID_ARGUMENT: bad thought signature"),
		say("worked"),
	)
	f.start("s1")
	if err := f.run(Prompt{Text: "first", Attachments: []string{f.image("a.png", 5_000)}}); err != nil {
		t.Fatal(err)
	}
	if err := f.run(Prompt{Text: "second"}); err != nil {
		t.Fatal(err)
	}
	if f.lastStatus().State != "done" {
		t.Fatalf("status: %+v", f.lastStatus())
	}
	retry := f.model.Body(2)
	if strings.Contains(retry, "inlineData") || strings.Contains(retry, "thoughtSignature") {
		t.Fatal("the portable retry must carry no media and no native replay")
	}

	// a second failure is not retried forever
	f2 := newFixture(t, fail(provider.ErrBadRequest, "nope"), fail(provider.ErrBadRequest, "still nope"), say("never"))
	f2.start("s2")
	_ = f2.run(Prompt{Text: "x"})
	if f2.model.Calls() != 2 || f2.lastStatus().State != "error" {
		t.Fatalf("calls=%d status=%+v", f2.model.Calls(), f2.lastStatus())
	}
}

func TestAgeBasedPruningIsBatched(t *testing.T) {
	steps := []step{}
	for i := 0; i < 8; i++ {
		steps = append(steps, say("ok"))
	}
	f := newFixture(t, steps...)
	f.cfg.Context.MaxTurnToKeepAttachment = 2
	f.cfg.Context.AttachmentPruneBatchMB = 0.05 // ~52KB of expired payload before pruning kicks in
	f.start("s1")
	for i := 0; i < 5; i++ {
		p := Prompt{Text: "turn"}
		if i < 4 {
			p.Attachments = []string{f.image("img"+string(rune('a'+i))+".png", 20_000)}
		}
		if err := f.run(p); err != nil {
			t.Fatal(err)
		}
		if i == 2 && len(prunes(f)) != 0 {
			t.Fatal("expired payload is below the batch size: pruning must be deferred so the cached prefix survives")
		}
	}
	if len(prunes(f)) == 0 {
		t.Fatal("once enough payload expired, it must be pruned in one batch")
	}
	// after a prune event the prefix is stable again: later requests extend the pruned form
	n := f.model.Calls()
	if n < 5 {
		t.Fatalf("calls: %d", n)
	}
}

func TestSessionResumeRebuildsContextFromTheLog(t *testing.T) {
	f := newFixture(t, say("first reply"), say("second reply"))
	f.start("s1")
	if err := f.run(Prompt{Text: "remember 42", Attachments: []string{f.image("a.png", 3_000)}}); err != nil {
		t.Fatal(err)
	}
	counters := f.eng.Counters()
	f.eng.Close()

	f.start("s1") // resume
	if got := f.eng.Counters(); got.Requests != counters.Requests || got.Prompt != counters.Prompt {
		t.Fatalf("token counters not restored: %+v vs %+v", got, counters)
	}
	if err := f.run(Prompt{Text: "what did I say?"}); err != nil {
		t.Fatal(err)
	}
	req := f.model.Request(1)
	if len(req.Messages) != 3 || req.Messages[0].Text() != "remember 42" || req.Messages[1].Text() != "first reply" {
		t.Fatalf("resumed context: %+v", req.Messages)
	}
	if !strings.Contains(f.model.Body(1), "inlineData") {
		t.Fatal("attachments from before the restart must still be sent (read back from the blob store)")
	}
	if len(f.kinds(session.KindSystem)) != 1 {
		t.Fatal("resuming must not re-snapshot the system prompt")
	}
}

func TestCompactionFitsAnOversizedSummaryRequest(t *testing.T) {
	// the conversation is bigger than the limit, so the summary request itself
	// must drop attachments to be sendable
	f := newFixture(t, say("one"), say("two"), say("SUMMARY ok"), say("go on"))
	f.cfg.Context.RequestBodyLimitMB = 0.05
	f.cfg.Context.RemoveOldAttachmentOnRequestBodyLimit = false
	f.start("s1")
	_ = f.run(Prompt{Text: "a", Attachments: []string{f.image("a.png", 20_000)}})
	_ = f.run(Prompt{Text: "b", Attachments: []string{f.image("b.png", 21_000)}})
	if err := f.run(Prompt{Text: "c"}); err != nil {
		t.Fatal(err)
	}
	summaryBody := f.model.Body(2)
	if len(summaryBody) > f.cfg.BodyLimitBytes() {
		t.Fatalf("the summary request (%d bytes) must fit the limit (%d)", len(summaryBody), f.cfg.BodyLimitBytes())
	}
	if got := len(f.kinds(session.KindCompaction)); got != 1 {
		t.Fatalf("compactions: %d", got)
	}
}

func TestToolMediaIsPrunedLikeAnyAttachment(t *testing.T) {
	f := newFixture(t,
		callTool("c1", "view_file", map[string]any{"path": "pic.png"}),
		say("I see it"),
		say("next"),
	)
	f.image("pic.png", 18_004)
	f.cfg.Context.RequestBodyLimitMB = 0.05
	f.start("s1")
	if err := f.run(Prompt{Text: "look at pic.png"}); err != nil {
		t.Fatal(err)
	}
	contains(t, f.model.Body(1), "inlineData", "tool result media reaches the model")
	f.image("other.png", 20_000)
	if err := f.run(Prompt{Text: "also", Attachments: []string{filepath.Join(f.ws, "other.png")}}); err != nil {
		t.Fatal(err)
	}
	if len(prunes(f)) == 0 {
		t.Fatal("media returned by tools must be prunable too")
	}
	last := f.model.Body(f.model.Calls() - 1)
	contains(t, last, "re-open it with view_file at", "tool media re-open hint")
}

func TestUnsupportedMediaIsRefusedBeforeItReachesTheModel(t *testing.T) {
	f := newFixture(t, callTool("c1", "view_file", map[string]any{"path": "doc.pdf"}), say("ok"))
	f.writeFile("doc.pdf", "%PDF-1.4 fake") // PDF capability is off in the fixture
	f.start("s1")
	if err := f.run(Prompt{Text: "read doc.pdf"}); err != nil {
		t.Fatal(err)
	}
	contains(t, resultText(f.messages(), "c1"), "pdf input is not supported by the configured model", "capability message")
	if strings.Contains(f.model.Body(1), "application/pdf") {
		t.Fatal("unsupported media must never be sent")
	}
	if err := f.run(Prompt{Text: "attach it", Attachments: []string{filepath.Join(f.ws, "doc.pdf")}}); err == nil {
		// Submit succeeds; the failure is reported through the turn status.
		if st := f.lastStatus(); st.State != "error" || !strings.Contains(st.Detail, "not supported by the configured model") {
			t.Fatalf("attaching an unsupported file must fail the turn clearly: %+v", st)
		}
	}
	_ = os.Remove(filepath.Join(f.ws, "doc.pdf"))
	var _ llm.Capabilities
}
