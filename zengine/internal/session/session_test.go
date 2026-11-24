package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Create(filepath.Join(t.TempDir(), "s1"), Meta{ID: "s1", Workspace: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func userMsg(text string) *llm.Message {
	return &llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: text}}}
}

func TestValidateID(t *testing.T) {
	for _, ok := range []string{"session_1", "a", "2026-10-11.a", "A-b_c.d"} {
		if err := ValidateID(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "../x", "a/b", `a\b`, "-lead", ".hidden", "sp ace", strings.Repeat("a", 129)} {
		if err := ValidateID(bad); err == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
	if id := NewID(); ValidateID(id) != nil {
		t.Fatalf("generated id invalid: %s", id)
	}
}

func TestAppendReopenPreservesOrderAndSeq(t *testing.T) {
	s := newStore(t)
	for _, txt := range []string{"a", "b", "c"} {
		if _, err := s.Append(Event{Kind: KindMessage, Message: userMsg(txt)}); err != nil {
			t.Fatal(err)
		}
	}
	dir := s.Dir
	s.Close()
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	evs := r.Events()
	if len(evs) != 3 || evs[2].Seq != 3 || evs[2].Message.Text() != "c" {
		t.Fatalf("events: %+v", evs)
	}
	ev, _ := r.Append(Event{Kind: KindMessage, Message: userMsg("d")})
	if ev.Seq != 4 {
		t.Fatalf("seq after reopen: %d", ev.Seq)
	}
	if got := r.Since(2); len(got) != 2 || got[0].Seq != 3 {
		t.Fatalf("since: %+v", got)
	}
}

func TestTornTailIsRepaired(t *testing.T) {
	s := newStore(t)
	s.Append(Event{Kind: KindMessage, Message: userMsg("kept")})
	dir := s.Dir
	s.Close()
	f, _ := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"seq":2,"kind":"message","message":{"role":"us`) // crash mid-write
	f.Close()

	r, err := Open(dir)
	if err != nil {
		t.Fatalf("torn tail must be tolerated: %v", err)
	}
	if len(r.Events()) != 1 {
		t.Fatalf("events: %d", len(r.Events()))
	}
	r.Append(Event{Kind: KindMessage, Message: userMsg("after")})
	r.Close()
	b, _ := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if bytes.Count(b, []byte("\n")) != 2 || strings.Contains(string(b), `"role":"us`+"\n") {
		t.Fatalf("file not repaired cleanly:\n%s", b)
	}
}

func TestCorruptMiddleLineIsAnError(t *testing.T) {
	s := newStore(t)
	dir := s.Dir
	s.Close()
	os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("{not json}\n{\"seq\":2,\"kind\":\"turn\"}\n"), 0o644)
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("want corruption error, got %v", err)
	}
}

func TestOpenMissing(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "nope")); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("got %v", err)
	}
	if _, err := Create(newStore(t).Dir, Meta{ID: "s1"}); err == nil {
		t.Fatal("creating over an existing session must fail")
	}
}

func TestBlobStoreDedupesAndValidates(t *testing.T) {
	s := newStore(t)
	a, err := s.PutBlob([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.PutBlob([]byte("hello"))
	if a != b || len(a) != 64 {
		t.Fatalf("not content addressed: %s %s", a, b)
	}
	got, err := s.Load(a)
	if err != nil || string(got) != "hello" {
		t.Fatalf("load: %q %v", got, err)
	}
	for _, bad := range []string{"../../etc/passwd", "short", strings.Repeat("g", 64)} {
		if _, err := s.Load(bad); err == nil {
			t.Errorf("Load(%q) must be rejected (path traversal guard)", bad)
		}
	}
}

func TestSubscribeReplaysThenStreams(t *testing.T) {
	s := newStore(t)
	s.Append(Event{Kind: KindTurn, State: "start"})
	sub := s.Subscribe(0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ev, err := sub.Next(ctx)
	if err != nil || ev.Seq != 1 {
		t.Fatalf("replay: %+v %v", ev, err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		s.Append(Event{Kind: KindStatus, State: "working"})
	}()
	ev, err = sub.Next(ctx)
	if err != nil || ev.Seq != 2 || ev.State != "working" {
		t.Fatalf("live: %+v %v", ev, err)
	}
	short, c2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer c2()
	if _, err := sub.Next(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("idle Next must honour ctx, got %v", err)
	}
}

func TestListNewestFirst(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"old", "new"} {
		s, err := Create(filepath.Join(root, id), Meta{ID: id})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(15 * time.Millisecond)
		s.Close()
	}
	metas, err := List(root)
	if err != nil || len(metas) != 2 || metas[0].ID != "new" {
		t.Fatalf("list: %+v %v", metas, err)
	}
	if m, err := List(filepath.Join(root, "absent")); err != nil || m != nil {
		t.Fatalf("absent dir: %v %v", m, err)
	}
}

func TestLockExclusionAndStaleTakeover(t *testing.T) {
	dir := t.TempDir()
	l1, err := Acquire(dir, "engine")
	if err != nil {
		t.Fatal(err)
	}
	var locked *ErrLocked
	if _, err := Acquire(dir, "other"); !errors.As(err, &locked) || locked.Info.PID != os.Getpid() {
		t.Fatalf("second acquire must report the owner, got %v", err)
	}
	l1.SetAddr("127.0.0.1:9999")
	if li := ReadLock(dir); li == nil || li.Addr != "127.0.0.1:9999" {
		t.Fatalf("addr not published: %+v", li)
	}
	l1.Release()
	if ReadLock(dir) != nil {
		t.Fatal("lock must be gone after Release")
	}
	l1.Release() // idempotent

	// a dead owner's stale heartbeat must not block forever
	stale := LockInfo{PID: 999999, Started: time.Now().Add(-time.Hour), Heartbeat: time.Now().Add(-time.Minute)}
	b, _ := json.Marshal(stale)
	os.WriteFile(lockPath(dir), b, 0o644)
	l2, err := Acquire(dir, "engine")
	if err != nil {
		t.Fatalf("stale lock must be taken over: %v", err)
	}
	l2.Release()
}
