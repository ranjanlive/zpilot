package core

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/internal/model"
)

// Workflows run as ordinary agent messages in one thread per run. A step's
// request is sent from "workflow:<run id>:<step id>"; when its reply comes
// back (routeResultLocked → workflowReplyLocked) the run advances.

const wfSenderPrefix = "workflow:"

// Workflows lists workflow definitions.
func (s *Service) Workflows() []model.Workflow {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	out := []model.Workflow{}
	for _, w := range s.st.S.Workflows {
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Service) workflowLocked(id string) *model.Workflow {
	for _, w := range s.st.S.Workflows {
		if w.ID == id {
			return w
		}
	}
	return nil
}

// Workflow returns one definition.
func (s *Service) Workflow(id string) (model.Workflow, error) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	w := s.workflowLocked(id)
	if w == nil {
		return model.Workflow{}, ErrNotFound
	}
	return *w, nil
}

// SaveWorkflow validates and stores a workflow.
func (s *Service) SaveWorkflow(actor string, in model.Workflow) (model.Workflow, error) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	for i := range in.Steps {
		in.Steps[i].Agent = model.NormalizeUsername(in.Steps[i].Agent)
		if in.Steps[i].DependsOn == nil {
			in.Steps[i].DependsOn = []string{}
		}
	}
	if err := in.Validate(); err != nil {
		return in, bad("%v", err)
	}
	for _, st := range in.Steps {
		if a := s.st.AgentByUsername(st.Agent); a == nil {
			return in, bad("step %s: there is no agent @%s", st.ID, st.Agent)
		}
	}
	now := time.Now().UTC()
	in.Updated = now
	if in.ID == "" {
		in.ID = model.NewID("wf")
		in.Created = now
		c := in
		s.st.S.Workflows = append(s.st.S.Workflows, &c)
	} else {
		cur := s.workflowLocked(in.ID)
		if cur == nil {
			return in, ErrNotFound
		}
		in.Created = cur.Created
		*cur = in
	}
	if err := s.st.Save(); err != nil {
		return in, err
	}
	s.st.Audit(actor, "workflow.save", in.Name, fmt.Sprintf("%d steps", len(in.Steps)))
	s.hub.Publish("workflows", nil)
	return in, nil
}

// DeleteWorkflow removes a workflow that no schedule uses.
func (s *Service) DeleteWorkflow(actor, id string) error {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	w := s.workflowLocked(id)
	if w == nil {
		return ErrNotFound
	}
	for _, sc := range s.st.S.Schedules {
		if sc.WorkflowID == id {
			return bad("schedule %q runs this workflow; delete it first", sc.Name)
		}
	}
	s.st.S.Workflows = slices.DeleteFunc(s.st.S.Workflows, func(x *model.Workflow) bool { return x.ID == id })
	if err := s.st.Save(); err != nil {
		return err
	}
	s.st.Audit(actor, "workflow.delete", w.Name, "")
	s.hub.Publish("workflows", nil)
	return nil
}

// WorkflowRuns lists runs, newest first (optionally for one workflow).
func (s *Service) WorkflowRuns(workflowID string, limit int) []model.WorkflowRun {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	out := []model.WorkflowRun{}
	for i := len(s.st.S.WFRuns) - 1; i >= 0 && len(out) < limit; i-- {
		r := s.st.S.WFRuns[i]
		if workflowID == "" || r.WorkflowID == workflowID {
			out = append(out, copyRun(r))
		}
	}
	return out
}

func copyRun(r *model.WorkflowRun) model.WorkflowRun {
	c := *r
	c.Steps = map[string]*model.StepRun{}
	for k, v := range r.Steps {
		sv := *v
		c.Steps[k] = &sv
	}
	return c
}

func (s *Service) wfRunLocked(id string) *model.WorkflowRun {
	for i := len(s.st.S.WFRuns) - 1; i >= 0; i-- {
		if s.st.S.WFRuns[i].ID == id {
			return s.st.S.WFRuns[i]
		}
	}
	return nil
}

// WorkflowRun returns one run.
func (s *Service) WorkflowRun(id string) (model.WorkflowRun, error) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	r := s.wfRunLocked(id)
	if r == nil {
		return model.WorkflowRun{}, ErrNotFound
	}
	return copyRun(r), nil
}

// RunWorkflow starts a run with the given input.
func (s *Service) RunWorkflow(actor, id, input string) (model.WorkflowRun, error) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	return s.runWorkflowLocked(actor, id, input)
}

func (s *Service) runWorkflowLocked(actor, id, input string) (model.WorkflowRun, error) {
	w := s.workflowLocked(id)
	if w == nil {
		return model.WorkflowRun{}, ErrNotFound
	}
	if len(input) > maxMessageBytes {
		return model.WorkflowRun{}, bad("input is too large")
	}
	t := s.newThreadLocked("⚙ "+w.Name, "workflow:"+w.ID)
	// a workflow needs at least one run per step, plus room for agents that collaborate
	t.MaxRuns = max(t.MaxRuns, len(w.Steps)*4)
	r := &model.WorkflowRun{ID: model.NewID("wfr"), WorkflowID: w.ID, Name: w.Name, Input: input, ThreadID: t.ID,
		State: model.WFRunning, Steps: map[string]*model.StepRun{}, Graph: slices.Clone(w.Steps), By: actor, Started: time.Now().UTC()}
	for _, st := range w.Steps {
		r.Steps[st.ID] = &model.StepRun{State: model.WFPending}
	}
	s.st.S.WFRuns = append(s.st.S.WFRuns, r)
	s.st.Audit(actor, "workflow.run", w.Name, r.ID)
	s.advanceLocked(r)
	if err := s.st.Save(); err != nil {
		return copyRun(r), err
	}
	return copyRun(r), nil
}

// CancelWorkflowRun stops a run: pending steps are canceled, queued step
// messages are canceled, and running steps are stopped.
func (s *Service) CancelWorkflowRun(actor, id string) error {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	r := s.wfRunLocked(id)
	if r == nil {
		return ErrNotFound
	}
	if r.State != model.WFRunning {
		return bad("the run is already %s", r.State)
	}
	r.State, r.Ended = model.WFCanceled, time.Now().UTC()
	for _, sr := range r.Steps {
		switch sr.State {
		case model.WFPending:
			sr.State = model.WFCanceled
		case model.WFRunning:
			if m := s.st.FindMessage(r.ThreadID, sr.MessageID); m != nil {
				if m.Status == model.StatusQueued {
					s.queue = slices.DeleteFunc(s.queue, func(q queued) bool { return q.msgID == m.ID })
					m.Status, m.Detail = model.StatusCanceled, "workflow canceled"
					s.updateLocked(m)
				} else if ar := s.active[m.RunID]; ar != nil {
					ar.stop = true
					ar.cancel()
				}
			}
			sr.State, sr.Ended = model.WFCanceled, time.Now().UTC()
		}
	}
	s.st.Audit(actor, "workflow.cancel", r.Name, r.ID)
	s.hub.Publish("workflow_run", copyRun(r))
	return s.st.Save()
}

// advanceLocked starts every step whose dependencies are finished, applies
// conditions, and finishes the run when nothing is left.
func (s *Service) advanceLocked(r *model.WorkflowRun) {
	if r.State != model.WFRunning {
		return
	}
	outputs := map[string]string{}
	for id, sr := range r.Steps {
		outputs[id] = sr.Output
	}
	progressed := true
	for progressed {
		progressed = false
		for _, st := range r.Graph {
			sr := r.Steps[st.ID]
			if sr.State != model.WFPending {
				continue
			}
			ready, blocked := true, false
			for _, d := range st.DependsOn {
				switch r.Steps[d].State {
				case model.WFDone, model.WFSkipped:
				case model.WFFailed, model.WFCanceled:
					blocked = true
				default:
					ready = false
				}
			}
			now := time.Now().UTC()
			if blocked {
				sr.State, sr.Error, sr.Ended = model.WFCanceled, "an earlier step failed", now
				progressed = true
				continue
			}
			if !ready {
				continue
			}
			if c := st.When; c != nil {
				hit := strings.Contains(strings.ToLower(outputs[c.Step]), strings.ToLower(c.Contains))
				if r.Steps[c.Step].State == model.WFSkipped || hit == c.Not {
					sr.State, sr.Ended = model.WFSkipped, now
					progressed = true
					continue
				}
			}
			prompt := model.RenderPrompt(st.Prompt, r.Input, outputs)
			m, err := s.sendLocked(SendInput{ThreadID: r.ThreadID, To: st.Agent, Text: prompt,
				From: wfSenderPrefix + r.ID + ":" + st.ID, FromName: r.Name + " · " + st.ID, Origin: "workflow:" + r.WorkflowID})
			if err != nil {
				sr.State, sr.Error, sr.Ended = model.WFFailed, err.Error(), now
				progressed = true
				continue
			}
			sr.State, sr.MessageID, sr.Started = model.WFRunning, m.ID, now
		}
	}
	done, failed := true, false
	for _, sr := range r.Steps {
		switch sr.State {
		case model.WFPending, model.WFRunning:
			done = false
		case model.WFFailed:
			failed = true
		}
	}
	if done {
		r.State, r.Ended = model.WFDone, time.Now().UTC()
		if failed {
			r.State = model.WFFailed
		}
		s.notifyWorkflowEndLocked(r)
	}
	s.hub.Publish("workflow_run", copyRun(r))
}

// workflowReplyLocked records a step's result. It reports whether req
// belonged to a workflow.
func (s *Service) workflowReplyLocked(req *model.Message, res *model.Message) bool {
	rest, ok := strings.CutPrefix(req.From, wfSenderPrefix)
	if !ok {
		return false
	}
	runID, stepID, _ := strings.Cut(rest, ":")
	r := s.wfRunLocked(runID)
	if r == nil || r.Steps[stepID] == nil {
		return true
	}
	sr := r.Steps[stepID]
	if sr.State != model.WFRunning {
		return true
	}
	sr.Ended = time.Now().UTC()
	if res.Kind == model.KindReply {
		sr.State, sr.Output = model.WFDone, res.Text
	} else {
		sr.State, sr.Error = model.WFFailed, res.Text
	}
	s.advanceLocked(r)
	_ = s.st.Save()
	return true
}

// notifyWorkflowEndLocked appends a summary to the run's thread.
func (s *Service) notifyWorkflowEndLocked(r *model.WorkflowRun) {
	t := s.st.Thread(r.ThreadID)
	if t == nil {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Workflow %q %s in %s.\n", r.Name, r.State, r.Ended.Sub(r.Started).Round(time.Second))
	ids := slices.Sorted(maps.Keys(r.Steps))
	for _, id := range ids {
		fmt.Fprintf(&b, "\n• %s: %s", id, r.Steps[id].State)
		if r.Steps[id].Error != "" {
			fmt.Fprintf(&b, " (%s)", clip(oneLine(r.Steps[id].Error), 160))
		}
	}
	_ = s.appendLocked(t, &model.Message{From: "system", FromName: "ZPilot", To: model.AddrUser, Kind: model.KindNotice, Text: b.String()})
}
