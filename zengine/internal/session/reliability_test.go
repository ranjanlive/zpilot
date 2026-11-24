package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestValidTailSurvivesReopenAndAppend(t *testing.T) {
	s := newStore(t)
	dir := s.Dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte(`{"kind":"note","text":"kept"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 2; round++ {
		r, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Events()) != 1 || r.Events()[0].Text != "kept" {
			t.Fatal("valid tail was lost")
		}
		if round == 1 {
			if _, err = r.Append(Event{Kind: KindNote, Text: "next"}); err != nil {
				t.Fatal(err)
			}
		}
		if err = r.Close(); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if len(r.Events()) != 2 || r.Events()[1].Seq != 2 {
		t.Fatal("append merged with unterminated tail")
	}
}

func TestOwnershipSurvivesStaleAndMalformedDiscovery(t *testing.T) {
	dir := t.TempDir()
	owner, err := Acquire(dir, "first")
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	for _, data := range [][]byte{[]byte(`{`), mustLockJSON(t, LockInfo{PID: os.Getpid(), Heartbeat: time.Now().Add(-time.Hour)})} {
		if err = os.WriteFile(lockPath(dir), data, 0600); err != nil {
			t.Fatal(err)
		}
		second, e := Acquire(dir, "contender")
		if second != nil {
			second.Release()
			t.Fatal("live ownership stolen")
		}
		var locked *ErrLocked
		if !errors.As(e, &locked) {
			t.Fatalf("want ErrLocked, got %v", e)
		}
	}
}
func mustLockJSON(t *testing.T, v LockInfo) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func TestConcurrentAcquireAndRelease(t *testing.T) {
	dir := t.TempDir()
	owner, e := Acquire(dir, "first")
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, e := Acquire(dir, "other")
			if l != nil {
				l.Release()
				t.Error("concurrent acquire succeeded")
			}
			var locked *ErrLocked
			if !errors.As(e, &locked) {
				t.Errorf("unexpected error %v", e)
			}
		}()
	}
	wg.Wait()
	for range 12 {
		wg.Add(1)
		go func() { defer wg.Done(); owner.Release() }()
	}
	wg.Wait()
	next, e := Acquire(dir, "next")
	if e != nil {
		t.Fatal(e)
	}
	defer next.Release()
	owner.Release()
	if _, e = Acquire(dir, "third"); e == nil {
		t.Fatal("old Release removed new ownership")
	}
}

func TestOwnerProcessHelper(t *testing.T) {
	dir := os.Getenv("ZENGINE_LOCK_TEST_HOME")
	if dir == "" {
		t.Skip("helper process")
	}
	l, e := Acquire(dir, "child")
	if e != nil {
		os.Exit(2)
	}
	defer l.Release()
	if e = os.WriteFile(filepath.Join(dir, "ready"), nil, 0600); e != nil {
		os.Exit(3)
	}
	select {}
}
func TestLockReleasedByProcessDeath(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOwnerProcessHelper$")
	cmd.Env = append(os.Environ(), "ZENGINE_LOCK_TEST_HOME="+dir)
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	for {
		if _, e := os.Stat(filepath.Join(dir, "ready")); e == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("child did not acquire lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if l, e := Acquire(dir, "parent"); e == nil {
		l.Release()
		t.Fatal("cross-process ownership was not exclusive")
	}
	if e := cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = cmd.Wait()
	l, e := Acquire(dir, "after crash")
	if e != nil {
		t.Fatal(e)
	}
	l.Release()
}
