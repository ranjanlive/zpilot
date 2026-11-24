package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/contextmgr"
	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/provider"
)

// callModel sends the next request and returns a usable response. It applies
// the remedy that fits each classified failure and only gives up when none is
// left:
//
//	too large (413)     -> drop old attachments, else compact, then resend
//	context overflow    -> compact, then resend
//	malformed (400)     -> resend once in a portable, text-only form
//	empty response      -> resend (configurable), then report
//	transient failures  -> retry with backoff, then pause (see sendWithRetry)
func (e *Engine) callModel(ctx context.Context, turn int) (*provider.Response, error) {
	var (
		remedies, empties int
		degraded          bool
	)
	for {
		pr, err := e.prepare(ctx, degraded)
		if err != nil {
			return nil, err
		}
		stream := newStreamReporter(e)
		resp, err := e.sendWithRetry(ctx, pr.built, stream.sink)
		if err == nil {
			if isEmpty(resp.Message) && resp.Message.FinishReason != llm.FinishFiltered {
				empties++
				if empties <= e.cfg.Network.EmptyResponseRetries {
					e.live.publish(Live{Kind: LiveRetry, Text: "reset_stream", Detail: fmt.Sprintf("the model returned an empty response; retrying (%d/%d)", empties, e.cfg.Network.EmptyResponseRetries)})
					continue
				}
				return nil, fmt.Errorf("the model returned an empty response %d times in a row", empties)
			}
			sanitizeToolCalls(&resp.Message)
			if degraded {
				_ = e.recordStatus("warning", "the provider rejected the normal request; this response used a degraded text-only request")
			}
			e.recordUsage(resp, len(pr.built.Body), pr.view, pr.req)
			return resp, nil
		}

		pe := provider.AsError(err)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		remedies++
		switch {
		case pe.Kind == provider.ErrTooLarge && remedies <= 4:
			if err := e.shrinkAfterTooLarge(ctx, pr); err != nil {
				return nil, err
			}
		case pe.Kind == provider.ErrContextLimit && remedies <= 3:
			if err := e.compact(ctx, "the provider reported the prompt exceeds the model's context window"); err != nil {
				return nil, err
			}
		case pe.Kind == provider.ErrBadRequest && !degraded:
			degraded = true
			_ = e.recordStatus("warning", "request rejected as invalid ("+oneLineErr(pe.Message)+"); retrying with a text-only portable request")
		default:
			return nil, err
		}
	}
}

// shrinkAfterTooLarge reacts to a 413 the local size estimate did not predict
// (the endpoint's real limit is lower than configured): it targets 80% of the
// size that failed.
func (e *Engine) shrinkAfterTooLarge(ctx context.Context, pr *prepared) error {
	size := len(pr.built.Body)
	target := size * 8 / 10
	_ = e.recordStatus("warning", fmt.Sprintf("the endpoint rejected a %s request as too large; shrinking it (request_body_limit_mb may be set too high)", llm.HumanBytes(int64(size))))
	plan := contextmgr.PlanForLimit(pr.view, size, contextmgr.Policy{BodyLimit: target, RemoveOldOnLimit: true, AutocompactOnLimit: true})
	if len(plan.Prune) > 0 {
		e.prune(contextmgr.BlobIDs(plan.Prune), "the endpoint rejected the request as too large")
		return nil
	}
	return e.compact(ctx, "the endpoint rejected the request as too large and no attachments remain to drop")
}

func isEmpty(m llm.Message) bool {
	return strings.TrimSpace(m.Text()) == "" && len(m.ToolCalls()) == 0
}

func oneLineErr(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// sendWithRetry sends a built request, retrying transient failures with
// exponential backoff and jitter. When retries run out and auto_pause is on,
// the session pauses and tries again after pause_retry_minutes (or at once on
// Resume), up to max_pauses times, so an overnight outage does not lose work.
func (e *Engine) sendWithRetry(ctx context.Context, built *provider.Built, sink func(provider.Delta)) (*provider.Response, error) {
	n := e.cfg.Network
	for pauses := 0; ; pauses++ {
		var last error
		for attempt := 0; attempt <= n.MaxRetries; attempt++ {
			resp, err := e.model.Send(ctx, built, sink)
			if err == nil {
				return resp, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			pe := provider.AsError(err)
			if !pe.Retryable() {
				return nil, err
			}
			last = err
			if attempt == n.MaxRetries {
				break
			}
			d := e.retryDelay(attempt, pe.RetryAfter)
			e.live.publish(Live{Kind: LiveRetry, Text: "reset_stream", Detail: fmt.Sprintf("%s; retrying in %s (attempt %d/%d)", oneLineErr(pe.Error()), d.Round(time.Millisecond), attempt+1, n.MaxRetries)})
			if err := e.sleep(ctx, d); err != nil {
				return nil, err
			}
		}
		if !bool(n.AutoPause) || pauses >= n.MaxPauses {
			return nil, fmt.Errorf("giving up after %d retries: %w", n.MaxRetries, last)
		}
		if err := e.pause(ctx, last, pauses+1); err != nil {
			return nil, err
		}
	}
}

// pause parks the session until the retry time, a Resume() or cancellation.
func (e *Engine) pause(ctx context.Context, cause error, n int) error {
	wait := time.Duration(e.cfg.Network.PauseRetryMinutes) * time.Minute
	until := time.Now().Add(wait)
	detail := fmt.Sprintf("network trouble (%s). Paused; retrying at %s (pause %d/%d). Use resume to retry now.",
		oneLineErr(cause.Error()), until.Format("15:04:05"), n, e.cfg.Network.MaxPauses)
	e.setState(StatePaused, detail)
	_ = e.recordStatus("paused", detail)
	defer func() {
		e.setState(StateWorking, "")
		_ = e.recordStatus("working", "resumed after pause")
	}()

	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-e.resumeNow:
			cancel()
		case <-pctx.Done():
		}
	}()
	err := e.sleep(pctx, wait)
	if err != nil && ctx.Err() != nil {
		return ctx.Err() // a real cancellation, not a Resume
	}
	return nil
}

func (e *Engine) retryDelay(attempt int, retryAfter time.Duration) time.Duration {
	d := e.lastRetryDelay(attempt, retryAfter)
	// +-20% jitter so many agents do not retry in lockstep
	j := time.Duration(float64(d) * 0.2 * (jitter() - 0.5) * 2)
	// Retry-After is the provider's minimum wait, not a jitter target.
	return max(retryAfter, d+j)
}

// ---- streaming progress ----

// streamReporter turns provider deltas into live events and a running token
// estimate (bytes/4) that UIs show while the answer is still arriving.
type streamReporter struct {
	e       *Engine
	chars   int
	lastPub time.Time
	base    Counters
}

func newStreamReporter(e *Engine) *streamReporter {
	return &streamReporter{e: e, base: e.Counters()}
}

func (s *streamReporter) sink(d provider.Delta) {
	switch d.Kind {
	case provider.DeltaText:
		s.e.live.publish(Live{Kind: LiveText, Text: d.Text})
	case provider.DeltaThought:
		s.e.live.publish(Live{Kind: LiveThought, Text: d.Text})
	case provider.DeltaToolCall:
		s.e.live.publish(Live{Kind: LiveToolCall, Name: d.Text})
	}
	s.chars += len(d.Text)
	if time.Since(s.lastPub) >= 250*time.Millisecond {
		s.lastPub = time.Now()
		c := s.base
		c.Completion += s.chars / 4
		c.Estimated = true
		s.e.live.publish(Live{Kind: LiveUsage, Tokens: c})
	}
}

// sanitizeToolCalls guarantees every stored tool call has valid JSON
// arguments, whatever adapter produced it. Invalid arguments are kept as text
// in ArgsError so the model is told exactly what it sent.
func sanitizeToolCalls(m *llm.Message) {
	for i := range m.Parts {
		c := m.Parts[i].Call
		if m.Parts[i].Type != llm.PartToolCall || c == nil {
			continue
		}
		trimmed := strings.TrimSpace(string(c.Args))
		switch {
		case trimmed == "":
			c.Args = []byte(`{}`)
		case !json.Valid([]byte(trimmed)):
			if c.ArgsError == "" {
				c.ArgsError = trimmed
			}
			c.Args = []byte(`{}`)
		}
	}
}
