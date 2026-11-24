package core

import (
	"context"
	"crypto/subtle"
	"slices"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/internal/model"
)

// LiveEvent is one engine progress event forwarded by the bridge.
type LiveEvent struct {
	Kind   string `json:"kind"` // text, thought, tool_call, tool_run, tool_done, state, retry
	Text   string `json:"text,omitempty"`
	Name   string `json:"name,omitempty"`
	State  string `json:"state,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// LiveSnapshot is what a UI needs to show a run that is in progress.
type LiveSnapshot struct {
	RunID    string   `json:"run_id"`
	ThreadID string   `json:"thread_id"`
	Username string   `json:"username"`
	Text     string   `json:"text"`
	Steps    []string `json:"steps"`
	State    string   `json:"state"`
}

const (
	maxLiveText  = 64 << 10
	maxLiveSteps = 40
)

// bridgeRun finds the active run a bridge token belongs to.
func (s *Service) bridgeRunLocked(tok string) *activeRun {
	if tok == "" {
		return nil
	}
	for _, ar := range s.active {
		if subtle.ConstantTimeCompare([]byte(ar.bridgeToken), []byte(tok)) == 1 {
			return ar
		}
	}
	return nil
}

// BridgeLive records live events from a run and fans them out to UIs.
func (s *Service) BridgeLive(tok string, evs []LiveEvent) error {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	ar := s.bridgeRunLocked(tok)
	if ar == nil {
		return forbid("unknown run")
	}
	for _, ev := range evs {
		switch ev.Kind {
		case "text":
			if len(ar.liveText) < maxLiveText {
				ar.liveText += ev.Text
			}
		case "tool_run":
			ar.liveSteps = append(ar.liveSteps, strings.TrimSpace(ev.Name+" "+ev.Detail))
			if len(ar.liveSteps) > maxLiveSteps {
				ar.liveSteps = ar.liveSteps[len(ar.liveSteps)-maxLiveSteps:]
			}
			// A new tool round means the text so far was preamble, not the answer.
			ar.liveText = ""
		case "state":
			ar.liveState = ev.State
		case "retry":
			if ev.Detail != "" {
				ar.liveSteps = append(ar.liveSteps, "⟳ "+ev.Detail)
			}
		}
		s.hub.Publish("live", map[string]any{"run_id": ar.run.ID, "thread_id": ar.msg.ThreadID, "username": ar.agent.Username,
			"kind": ev.Kind, "text": ev.Text, "name": ev.Name, "detail": ev.Detail, "state": ev.State})
	}
	return nil
}

// Live returns snapshots of every running run.
func (s *Service) Live() []LiveSnapshot {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	out := []LiveSnapshot{}
	for _, ar := range s.active {
		out = append(out, LiveSnapshot{RunID: ar.run.ID, ThreadID: ar.msg.ThreadID, Username: ar.agent.Username,
			Text: ar.liveText, Steps: slices.Clone(ar.liveSteps), State: ar.liveState})
	}
	return out
}

// ---- approvals ---------------------------------------------------------

// BridgeApproval creates a pending approval for a running engine.
func (s *Service) BridgeApproval(tok, tool, action, detail string) (string, error) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	ar := s.bridgeRunLocked(tok)
	if ar == nil {
		return "", forbid("unknown run")
	}
	a := &model.Approval{ID: model.NewID("apr"), RunID: ar.run.ID, AgentID: ar.agent.ID, Username: ar.agent.Username,
		ThreadID: ar.msg.ThreadID, Tool: tool, Action: action, Detail: clip(detail, 8000), State: model.ApprovalPending, Created: time.Now().UTC()}
	s.st.S.Approvals = append(s.st.S.Approvals, a)
	_ = s.st.Save()
	s.st.Audit("@"+a.Username, "approval.request", a.Tool, clip(oneLine(a.Detail), 200))
	s.hub.Publish("approval", a)
	return a.ID, nil
}

// approvalLocked finds an approval.
func (s *Service) approvalLocked(id string) *model.Approval {
	for i := len(s.st.S.Approvals) - 1; i >= 0; i-- {
		if s.st.S.Approvals[i].ID == id {
			return s.st.S.Approvals[i]
		}
	}
	return nil
}

// ApprovalDecision is returned to the waiting engine.
type ApprovalDecision struct {
	Done    bool   `json:"done"`
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// BridgeWaitApproval waits up to d for the owner's decision.
func (s *Service) BridgeWaitApproval(ctx context.Context, tok, id string, d time.Duration) (ApprovalDecision, error) {
	deadline := time.Now().Add(d)
	for {
		s.st.Mu.Lock()
		ar := s.bridgeRunLocked(tok)
		a := s.approvalLocked(id)
		if ar == nil || a == nil || a.RunID != ar.run.ID {
			s.st.Mu.Unlock()
			return ApprovalDecision{}, forbid("unknown approval")
		}
		timeout := time.Duration(s.st.S.Settings.ApprovalTimeoutMin) * time.Minute
		if a.State == model.ApprovalPending && time.Since(a.Created) > timeout {
			s.decideLocked(a, model.ApprovalExpired, "system", "no decision within "+timeout.String())
		}
		if a.State != model.ApprovalPending {
			res := ApprovalDecision{Done: true, Allowed: a.State == model.ApprovalAllowed, Reason: approvalReason(a)}
			s.st.Mu.Unlock()
			return res, nil
		}
		ch := make(chan struct{})
		s.waiters[id] = append(s.waiters[id], ch)
		s.st.Mu.Unlock()
		left := min(time.Until(deadline), 15*time.Second)
		if time.Until(deadline) <= 0 {
			s.dropWaiter(id, ch)
			return ApprovalDecision{}, nil
		}
		select {
		case <-ch:
		case <-time.After(left):
			s.dropWaiter(id, ch)
		case <-ctx.Done():
			s.dropWaiter(id, ch)
			return ApprovalDecision{}, ctx.Err()
		}
	}
}

func approvalReason(a *model.Approval) string {
	switch a.State {
	case model.ApprovalAllowed:
		return "approved by the owner"
	case model.ApprovalExpired:
		return "the owner did not decide in time, so it was not run"
	}
	if a.Reason != "" {
		return "the owner denied it: " + a.Reason
	}
	return "the owner denied it"
}

// Approvals lists approvals, pending first then newest.
func (s *Service) Approvals(onlyPending bool) []model.Approval {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	out := []model.Approval{}
	for i := len(s.st.S.Approvals) - 1; i >= 0; i-- {
		a := s.st.S.Approvals[i]
		if onlyPending && a.State != model.ApprovalPending {
			continue
		}
		out = append(out, *a)
	}
	slices.SortStableFunc(out, func(x, y model.Approval) int {
		px, py := x.State == model.ApprovalPending, y.State == model.ApprovalPending
		switch {
		case px && !py:
			return -1
		case py && !px:
			return 1
		}
		return 0
	})
	return out
}

// DecideApproval allows or denies a pending command.
func (s *Service) DecideApproval(actor, id string, allow bool, reason string) (model.Approval, error) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	a := s.approvalLocked(id)
	if a == nil {
		return model.Approval{}, ErrNotFound
	}
	if a.State != model.ApprovalPending {
		return *a, bad("this request was already %s", a.State)
	}
	state := model.ApprovalDenied
	if allow {
		state = model.ApprovalAllowed
	}
	s.decideLocked(a, state, actor, strings.TrimSpace(reason))
	return *a, nil
}

func (s *Service) decideLocked(a *model.Approval, state, actor, reason string) {
	a.State, a.By, a.Reason, a.Decided = state, actor, clip(reason, 500), time.Now().UTC()
	_ = s.st.Save()
	s.st.Audit(actor, "approval."+state, "@"+a.Username, clip(oneLine(a.Detail), 200))
	s.hub.Publish("approval", a)
	s.notifyWaitersLocked(a.ID)
}

// expireApprovalsLocked closes approvals of a run that ended.
func (s *Service) expireApprovalsLocked(runID string) {
	for _, a := range s.st.S.Approvals {
		if a.RunID == runID && a.State == model.ApprovalPending {
			s.decideLocked(a, model.ApprovalExpired, "system", "the run ended")
		}
	}
}
