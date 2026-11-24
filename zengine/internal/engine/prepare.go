package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/contextmgr"
	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/provider"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
)

// policy derives the attachment/size policy from config.json.
func (e *Engine) policy() contextmgr.Policy {
	c := e.cfg.Context
	return contextmgr.Policy{
		MaxTurnToKeepAttachment: c.MaxTurnToKeepAttachment,
		PruneBatchBytes:         int64(c.AttachmentPruneBatchMB * 1024 * 1024),
		BodyLimit:               e.cfg.BodyLimitBytes(),
		AutocompactOnLimit:      bool(c.AutocompactOnRequestBodyLimitExceed),
		RemoveOldOnLimit:        bool(c.RemoveOldAttachmentOnRequestBodyLimit),
	}
}

func (e *Engine) request(system string, msgs []llm.Message, forbidTools bool) provider.Request {
	return provider.Request{
		System: system, Messages: msgs, Tools: e.registry.Specs(), Model: e.env.Model,
		MaxOutputTokens: e.cfg.Agent.MaxOutputTokens, Temperature: e.cfg.Agent.Temperature,
		Caps: e.env.Caps, Blobs: e.store, ForbidToolCalls: forbidTools,
	}
}

// compactAt is the prompt size, in tokens, that triggers compaction (0 = never by tokens).
func (e *Engine) compactAt() int {
	if e.env.CompactAt > 0 {
		return e.env.CompactAt
	}
	if e.env.ContextLimit > 0 {
		return e.env.ContextLimit * 8 / 10
	}
	return 0
}

// predictedTokens estimates the prompt size of the next request: the last
// reported prompt plus its answer plus whatever was added since (estimated),
// or a pure estimate when no usage is known.
func (e *Engine) predictedTokens(view contextmgr.Context) int {
	e.mu.Lock()
	last := e.lastUsage
	e.mu.Unlock()
	if last == nil || last.Estimated {
		return contextmgr.EstimateTokens(view.System, view.Messages, e.registry.Specs())
	}
	idx := -1
	for i := len(view.Messages) - 1; i >= 0; i-- {
		if view.Messages[i].Role == llm.RoleAssistant {
			idx = i
			break
		}
	}
	return last.PromptTokens + last.CompletionTokens + contextmgr.EstimateTokens("", view.Messages[idx+1:], nil)
}

// prepared is a request ready to send.
type prepared struct {
	req   provider.Request
	built *provider.Built
	view  contextmgr.Context
}

// prepare renders the context and keeps shrinking it until the next request is
// valid and within limits. Each shrinking step is written to the log first, so
// the rendered prefix stays deterministic afterwards.
func (e *Engine) prepare(ctx context.Context, degraded bool) (*prepared, error) {
	pol := e.policy()
	for round := 0; round < 8; round++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		view := contextmgr.Build(e.store.ContextEvents())
		view = e.maybeDegrade(view, degraded)
		req := e.request(view.System, view.Messages, false)

		// 1. age-based pruning (batched, so the cached prefix rarely changes)
		if exp := contextmgr.ExpiredByAge(view, pol, false); len(exp) > 0 {
			e.prune(contextmgr.BlobIDs(exp), fmt.Sprintf("older than %d turn(s)", pol.MaxTurnToKeepAttachment))
			continue
		}
		built, err := e.model.Build(req)
		if err != nil {
			return nil, err
		}
		size := len(built.Body)

		// 2. request-size limit: drop old attachments first, compact as the fallback
		plan := contextmgr.PlanForLimit(view, size, pol)
		if len(plan.Prune) > 0 {
			e.prune(contextmgr.BlobIDs(plan.Prune), fmt.Sprintf("request %s exceeds the %s limit", llm.HumanBytes(int64(size)), llm.HumanBytes(int64(pol.BodyLimit))))
			continue
		}
		if plan.NeedCompact {
			if err := e.compact(ctx, fmt.Sprintf("request size %s over the %s limit", llm.HumanBytes(int64(size)), llm.HumanBytes(int64(pol.BodyLimit)))); err != nil {
				return nil, err
			}
			continue
		}
		if pol.BodyLimit > 0 && size > pol.BodyLimit {
			return nil, fmt.Errorf("the next request is %s, over the %s limit, and both autocompact_on_request_body_limit_exceed and remove_old_attachment_on_request_body_limit are off. Enable one of them or start a new session",
				llm.HumanBytes(int64(size)), llm.HumanBytes(int64(pol.BodyLimit)))
		}

		// 3. token threshold
		if at := e.compactAt(); at > 0 && len(view.Messages) > 2 {
			if p := e.predictedTokens(view); p >= at {
				if err := e.compact(ctx, fmt.Sprintf("context at ~%d tokens (limit %d)", p, at)); err != nil {
					return nil, err
				}
				continue
			}
		}
		return &prepared{req: req, built: built, view: view}, nil
	}
	return nil, errors.New("could not fit the conversation into a valid request after repeated pruning and compaction")
}

// maybeDegrade builds the last-resort variant used after a provider rejects a
// request as malformed: no replayed provider-native encodings and no inline
// media, only the portable text of the conversation.
func (e *Engine) maybeDegrade(view contextmgr.Context, degraded bool) contextmgr.Context {
	if !degraded {
		return view
	}
	view = view.WithPruned(contextmgr.BlobIDs(view.Attachments))
	for i := range view.Messages {
		view.Messages[i].Native = nil
	}
	return view
}

// prune removes inline payloads by recording a prune event.
func (e *Engine) prune(blobs []string, why string) {
	if len(blobs) == 0 {
		return
	}
	_, _ = e.store.Append(session.Event{Kind: session.KindPrune, Blobs: blobs, Detail: why})
	e.mu.Lock()
	e.lastUsage = nil // the prompt changed: reported usage no longer describes it
	e.mu.Unlock()
}

// Compact summarises the conversation now (the /compact command).
func (e *Engine) Compact(ctx context.Context) error {
	if !e.workspaceMu.TryLock() {
		return errors.New("the agent is working; stop it or wait before compacting")
	}
	defer e.workspaceMu.Unlock()
	e.mu.Lock()
	prev := e.state
	e.mu.Unlock()
	err := e.compact(ctx, "requested by the user")
	e.setState(prev, "")
	return err
}

// compact replaces the conversation with a model-written summary. The
// summarisation request itself is fitted under the size limit by dropping the
// oldest attachments (and, if text alone is too big, the oldest messages).
func (e *Engine) compact(ctx context.Context, reason string) error {
	e.setState(StateCompacting, reason)
	view := contextmgr.Build(e.store.ContextEvents())
	if len(view.Messages) == 0 {
		return nil
	}
	limit := e.cfg.BodyLimitBytes()
	sum := view
	var built *provider.Built
	var req provider.Request
	for attempt := 0; attempt < 12; attempt++ {
		msgs := contextmgr.SummaryRequest(sum)
		msgs, _ = contextmgr.Repair(msgs)
		req = e.request(view.System, msgs, true)
		b, err := e.model.Build(req)
		if err != nil {
			return err
		}
		built = b
		if limit <= 0 || len(b.Body) <= limit {
			break
		}
		if plan := contextmgr.PlanForLimit(sum, len(b.Body), contextmgr.Policy{BodyLimit: limit, RemoveOldOnLimit: true}); len(plan.Prune) > 0 {
			sum = sum.WithPruned(contextmgr.BlobIDs(plan.Prune))
			continue
		}
		if len(sum.Messages) <= 2 {
			break
		}
		// Text alone is too large: summarise from the more recent part of the conversation.
		cut := max(1, len(sum.Messages)/4)
		sum.Messages = append([]llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartInfo, Text: "[info: the oldest part of the conversation was left out of this summary request because it was too large]"}}}}, sum.Messages[cut:]...)
	}

	resp, err := e.sendWithRetry(ctx, built, nil)
	if err != nil {
		return fmt.Errorf("compaction failed: %w", err)
	}
	if resp.Message.FinishReason == llm.FinishLength {
		return errors.New("compaction summary was cut off; increase agent.max_output_tokens and retry compaction")
	}
	if resp.Message.FinishReason == llm.FinishFiltered || len(resp.Message.ToolCalls()) > 0 {
		return errors.New("compaction failed: the provider did not return a complete text summary")
	}
	summary := strings.TrimSpace(resp.Message.Text())
	if summary == "" {
		return errors.New("compaction failed: the model returned an empty summary")
	}
	e.recordUsage(resp, len(built.Body), view, req)

	tokensBefore := e.predictedTokens(view)
	comp := contextmgr.NewCompaction(view, summary, reason, e.cfg.Context.CompactKeepLastTurns, tokensBefore, len(built.Body))
	if _, err := e.store.Append(session.Event{Kind: session.KindCompaction, Compaction: &comp, TurnNo: view.Turn}); err != nil {
		return err
	}
	// Re-snapshot the prompt now: memory and skills may have changed, and a
	// compaction is the one moment the prefix is allowed to change.
	if _, err := e.store.Append(session.Event{Kind: session.KindSystem, Text: e.buildSystemPrompt()}); err != nil {
		return err
	}
	e.mu.Lock()
	e.lastUsage = nil
	e.mu.Unlock()
	e.live.publish(Live{Kind: LiveRetry, Detail: "context compacted: " + reason})
	return nil
}

func (e *Engine) lastRetryDelay(attempt int, retryAfter time.Duration) time.Duration {
	n := e.cfg.Network
	d := time.Duration(n.RetryInitialMS) * time.Millisecond << min(attempt, 10)
	if mx := time.Duration(n.RetryMaxMS) * time.Millisecond; d > mx {
		d = mx
	}
	if retryAfter > d {
		d = retryAfter
	}
	return d
}
