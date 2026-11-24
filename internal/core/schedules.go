package core

import (
	"slices"
	"sort"
	"time"

	"github.com/ranjanlive/zpilot/internal/model"
	"github.com/ranjanlive/zpilot/internal/trigger"
)

// Schedules lists schedules by next run.
func (s *Service) Schedules() []model.Schedule {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	out := []model.Schedule{}
	for _, sc := range s.st.S.Schedules {
		out = append(out, *sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SaveSchedule creates (empty ID) or updates a schedule.
func (s *Service) SaveSchedule(actor string, in model.Schedule) (model.Schedule, error) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	return s.saveScheduleLocked(actor, in)
}

func (s *Service) saveScheduleLocked(actor string, in model.Schedule) (model.Schedule, error) {
	if a := s.st.AgentRef(in.AgentID); a != nil {
		in.AgentID = a.ID
	}
	if in.Trigger.Timezone == "" {
		in.Trigger.Timezone = "Local"
	}
	if err := in.Validate(); err != nil {
		return in, bad("%v", err)
	}
	if in.WorkflowID != "" {
		if s.workflowLocked(in.WorkflowID) == nil {
			return in, bad("workflow does not exist")
		}
		in.AgentID = ""
	} else if s.st.Agent(in.AgentID) == nil {
		return in, bad("agent does not exist")
	}
	now := time.Now().UTC()
	next, err := trigger.Next(in.Trigger, now, nil)
	if err != nil {
		return in, bad("%v", err)
	}
	in.NextRun = next
	in.Updated = now
	if in.ID == "" {
		in.ID = model.NewID("sch")
		in.Created = now
		c := in
		s.st.S.Schedules = append(s.st.S.Schedules, &c)
	} else {
		cur := s.st.Schedule(in.ID)
		if cur == nil {
			return in, ErrNotFound
		}
		in.Created, in.LastRun = cur.Created, cur.LastRun
		if cur.AgentID == in.AgentID && !in.FreshThread {
			in.ThreadID = cur.ThreadID
		} else {
			in.ThreadID = ""
		}
		*cur = in
	}
	if err := s.st.Save(); err != nil {
		return in, err
	}
	s.st.Audit(actor, "schedule.save", in.Name, in.Trigger.Kind)
	s.hub.Publish("schedules", nil)
	return in, nil
}

// DeleteSchedule removes a schedule.
func (s *Service) DeleteSchedule(actor, id string) error {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	sc := s.st.Schedule(id)
	if sc == nil {
		return ErrNotFound
	}
	s.st.S.Schedules = slices.DeleteFunc(s.st.S.Schedules, func(x *model.Schedule) bool { return x.ID == id })
	if err := s.st.Save(); err != nil {
		return err
	}
	s.st.Audit(actor, "schedule.delete", sc.Name, "")
	s.hub.Publish("schedules", nil)
	return nil
}

// RunScheduleNow fires a schedule immediately (without moving its next run).
func (s *Service) RunScheduleNow(actor, id string) (*model.Message, error) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	sc := s.st.Schedule(id)
	if sc == nil {
		return nil, ErrNotFound
	}
	s.st.Audit(actor, "schedule.run_now", sc.Name, "")
	return s.fireLocked(sc)
}

func (s *Service) scheduleLoop() {
	defer s.wg.Done()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-tick.C:
			s.fireDue(now.UTC())
		}
	}
}

// fireDue fires every enabled schedule whose time has come. A schedule missed
// while the daemon was down fires once on start-up, then resumes its rhythm.
func (s *Service) fireDue(now time.Time) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	changed := false
	for _, sc := range s.st.S.Schedules {
		if !sc.Enabled || sc.NextRun.IsZero() || sc.NextRun.After(now) {
			continue
		}
		if _, err := s.fireLocked(sc); err != nil {
			s.cfg.Log("schedule %s: %v", sc.Name, err)
		}
		sc.LastRun = now
		next, err := trigger.Next(sc.Trigger, now, nil)
		if err != nil || next.IsZero() {
			sc.NextRun = time.Time{}
			if sc.Trigger.Kind == "once" {
				sc.Enabled = false
			}
		} else {
			sc.NextRun = next
		}
		changed = true
	}
	if changed {
		if err := s.st.Save(); err != nil {
			s.cfg.Log("save state: %v", err)
		}
		s.hub.Publish("schedules", nil)
	}
}

func (s *Service) fireLocked(sc *model.Schedule) (*model.Message, error) {
	if sc.WorkflowID != "" {
		r, err := s.runWorkflowLocked("schedule:"+sc.Name, sc.WorkflowID, sc.Task)
		if err != nil {
			return nil, err
		}
		return &model.Message{ThreadID: r.ThreadID}, nil
	}
	a := s.st.Agent(sc.AgentID)
	if a == nil {
		return nil, bad("schedule %s: agent missing", sc.Name)
	}
	threadID := ""
	if !sc.FreshThread && sc.ThreadID != "" && s.st.Thread(sc.ThreadID) != nil {
		threadID = sc.ThreadID
	}
	m, err := s.sendLocked(SendInput{ThreadID: threadID, To: a.Username, Text: sc.Task,
		From: "schedule:" + sc.ID, FromName: sc.Name, Origin: "schedule:" + sc.ID})
	if err != nil {
		return nil, err
	}
	if !sc.FreshThread {
		sc.ThreadID = m.ThreadID
	}
	if t := s.st.Thread(m.ThreadID); t != nil && threadID == "" {
		t.Title = "⏰ " + sc.Name
	}
	return m, nil
}
