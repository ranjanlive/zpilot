package tools

import (
	"context"
	"strings"
	"testing"
)

func TestExternalJSONOutputIsBoundedAndRejected(t *testing.T) {
	te := newEnv(t)
	installTool(t, te, "oversize", "oversize_json", func(m *Manifest) { m.Output = "json" })
	r, problems := extRegistry(t, te)
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	result := r.Invoke(context.Background(), te.Env, call("oversize", map[string]any{"name": "test"}))
	if !result.IsError || !strings.Contains(result.Output, "JSON output limit") {
		t.Fatalf("oversize JSON accepted: %s", result.Output)
	}
	var capture jsonOutput
	chunk := []byte(strings.Repeat("x", 65536))
	for range 145 {
		n, e := capture.Write(chunk)
		if e != nil || n != len(chunk) {
			t.Fatal("capture must keep draining")
		}
	}
	if len(capture.Bytes()) != maxJSONOutput || !capture.Overflowed() {
		t.Fatal("capture is not bounded")
	}
}

func TestFinishedTerminalRetentionPreservesActiveAndRecent(t *testing.T) {
	m := NewManager()
	for id := 1; id <= maxFinishedProcesses+10; id++ {
		p := &Proc{ID: id, doneCh: make(chan struct{})}
		close(p.doneCh)
		m.procs[id] = p
	}
	active := &Proc{ID: 1000, doneCh: make(chan struct{})}
	m.procs[active.ID] = active
	m.pruneFinished()
	if len(m.procs) != maxFinishedProcesses+1 {
		t.Fatal("completed terminal retention is unbounded")
	}
	if _, e := m.Get(1); e == nil {
		t.Fatal("oldest output was not evicted")
	}
	if _, e := m.Get(maxFinishedProcesses + 10); e != nil {
		t.Fatal("recent terminal was lost")
	}
	if _, e := m.Get(active.ID); e != nil {
		t.Fatal("running terminal was evicted")
	}
}
