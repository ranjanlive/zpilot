package cli

// The bridge connects a headless engine run to a supervising process
// (ZPilot). It is opt-in: it activates only when ZAGENT_BRIDGE_URL and
// ZAGENT_BRIDGE_TOKEN are set. It does two things:
//
//   - approvals: tool calls that need permission (run_command with
//     permissions "ask", external tools with "permission": "ask") are sent to
//     the supervisor, which asks a human and answers allow/deny
//   - live events: streamed answer text, tool starts and finishes, retries and
//     state changes are forwarded so UIs can show progress as it happens
//
// The token ends in _TOKEN, so the engine's environment scrubbing removes it
// from every command and tool the agent starts.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/engine"
	"github.com/ranjanlive/zpilot/zengine/internal/tools"
)

type bridge struct {
	url, token string
	http       *http.Client
}

func newBridge() *bridge {
	u, t := strings.TrimRight(os.Getenv("ZAGENT_BRIDGE_URL"), "/"), os.Getenv("ZAGENT_BRIDGE_TOKEN")
	if u == "" || t == "" {
		return nil
	}
	return &bridge{url: u, token: t, http: &http.Client{Timeout: 70 * time.Second}}
}

func (b *bridge) post(ctx context.Context, path string, body, out any) error {
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.url+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("bridge %s: HTTP %d", path, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// approve asks the supervisor and waits for a decision (polling so no single
// HTTP request has to stay open for long).
func (b *bridge) approve(ctx context.Context, r tools.ApprovalRequest) (bool, string) {
	var created struct {
		ID string `json:"id"`
	}
	if err := b.post(ctx, "/approval", map[string]string{"tool": r.Tool, "action": r.Action, "detail": r.Detail}, &created); err != nil {
		return false, "the approval service is unreachable: " + err.Error()
	}
	for {
		var d struct {
			Done    bool   `json:"done"`
			Allowed bool   `json:"allowed"`
			Reason  string `json:"reason"`
		}
		if err := b.post(ctx, "/approval/wait", map[string]string{"id": created.ID}, &d); err != nil {
			if ctx.Err() != nil {
				return false, "stopped while waiting for approval"
			}
			time.Sleep(2 * time.Second)
			continue
		}
		if d.Done {
			return d.Allowed, d.Reason
		}
	}
}

// forwardLive batches live events and posts them every 150 ms.
func (b *bridge) forwardLive(eng *engine.Engine) (stop func()) {
	ch, cancel := eng.SubscribeLive()
	done := make(chan struct{})
	var (
		mu  sync.Mutex
		buf []engine.Live
	)
	flush := func() {
		mu.Lock()
		batch := buf
		buf = nil
		mu.Unlock()
		if len(batch) == 0 {
			return
		}
		// coalesce consecutive text deltas to keep requests small
		var out []engine.Live
		for _, ev := range batch {
			if n := len(out); n > 0 && ev.Kind == out[n-1].Kind && (ev.Kind == engine.LiveText || ev.Kind == engine.LiveThought) {
				out[n-1].Text += ev.Text
				continue
			}
			out = append(out, ev)
		}
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		_ = b.post(ctx, "/live", out, nil)
		c()
	}
	go func() {
		defer close(done)
		tick := time.NewTicker(150 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					flush()
					return
				}
				if ev.Kind == engine.LiveUsage {
					continue
				}
				mu.Lock()
				buf = append(buf, ev)
				mu.Unlock()
			case <-tick.C:
				flush()
			}
		}
	}()
	return func() { cancel(); <-done }
}
