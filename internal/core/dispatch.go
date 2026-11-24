package core

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ranjanlive/zpilot/internal/model"
	"github.com/ranjanlive/zpilot/internal/platform"
	"github.com/ranjanlive/zpilot/internal/profile"
)

func (s *Service) dispatchLoop() {
	defer s.wg.Done()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.kick:
		case <-tick.C:
		}
		s.dispatchOnce()
	}
}

// dispatchOnce starts every queued request that may run now. Rules: a global
// parallel limit, a per-agent limit, and at most one run per agent per thread
// (the engine session is exclusive).
func (s *Service) dispatchOnce() {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	if s.ctx.Err() != nil {
		return
	}
	perAgent := map[string]int{}
	for _, ar := range s.active {
		perAgent[ar.agent.ID]++
	}
	var keep []queued
	for i, q := range s.queue {
		if len(s.active) >= s.st.S.Settings.GlobalParallel {
			keep = append(keep, s.queue[i:]...)
			break
		}
		m := s.st.FindMessage(q.threadID, q.msgID)
		if m == nil || m.Status != model.StatusQueued {
			continue // canceled or gone
		}
		a := s.st.AgentByUsername(strings.TrimPrefix(m.To, "@"))
		if a == nil {
			s.failLocked(m, "the agent no longer exists")
			continue
		}
		if a.Disabled {
			keep = append(keep, q) // resumes when re-enabled
			continue
		}
		if perAgent[a.ID] >= a.MaxParallel || s.byConv[a.ID+"/"+q.threadID] != nil {
			keep = append(keep, q)
			continue
		}
		t := s.st.Thread(q.threadID)
		if t == nil {
			continue
		}
		if t.Runs >= t.MaxRuns {
			m.Status = model.StatusRejected
			m.Detail = "budget"
			s.updateLocked(m)
			s.routeResultLocked(m, &model.Message{Kind: model.KindNotice,
				Text: fmt.Sprintf("Not run: this conversation reached its budget of %d agent runs. Start a new conversation or raise the limit in Settings.", t.MaxRuns)})
			continue
		}
		if err := s.startLocked(a, t, m); err != nil {
			s.failLocked(m, err.Error())
			continue
		}
		perAgent[a.ID]++
	}
	s.queue = keep
	if err := s.st.Save(); err != nil {
		s.cfg.Log("save state: %v", err)
	}
}

// failLocked fails a request before it ran and tells the sender why.
func (s *Service) failLocked(m *model.Message, why string) {
	m.Status = model.StatusFailed
	m.Detail = why
	s.updateLocked(m)
	s.routeResultLocked(m, &model.Message{Kind: model.KindError, Text: "Could not run: " + why})
}

func (s *Service) startLocked(a *model.Agent, t *model.Thread, m *model.Message) error {
	mp := s.st.ModelProfile(a.ModelID)
	if mp == nil {
		return fmt.Errorf("@%s has no model configured; choose one on its settings page", a.Username)
	}
	env, err := s.modelEnvLocked(mp)
	if err != nil {
		return err
	}
	if s.daemonURL == "" {
		return fmt.Errorf("the daemon API is not ready yet")
	}
	agentCopy := *a
	agentCopy.Contacts = slices.Clone(a.Contacts)
	agentCopy.Tools = slices.Clone(a.Tools)
	agentCopy.Skills = slices.Clone(a.Skills)

	now := time.Now().UTC()
	run := &model.Run{ID: model.NewID("run"), AgentID: a.ID, Username: a.Username, ThreadID: t.ID, MessageID: m.ID, State: model.RunRunning, Started: now}
	s.st.S.Runs = append(s.st.S.Runs, run)
	t.Runs++
	m.Status = model.StatusRunning
	m.RunID = run.ID
	s.updateLocked(m)

	ctx, cancel := context.WithCancel(s.ctx)
	ar := &activeRun{run: run, msg: m, agent: &agentCopy, cancel: cancel, bridgeToken: model.NewToken()}
	env = append(env, "ZAGENT_BRIDGE_URL="+s.daemonURL+"/rpc/bridge", "ZAGENT_BRIDGE_TOKEN="+ar.bridgeToken)
	s.active[run.ID] = ar
	s.byConv[a.ID+"/"+t.ID] = ar

	spec := RunSpec{
		Session: t.SessionName(),
		Prompt:  s.headerLocked(m) + "\n\n" + m.Text,
		Env:     env,
		Timeout: time.Duration(a.TimeoutMinutes) * time.Minute,
		LogPath: s.runLogPath(run.ID),
		Limits:  platform.Limits{MemoryMB: a.MemoryMB, MaxProcesses: a.MaxProcesses},
		Isolate: a.Isolate,
	}
	opts := profile.Options{DataDir: s.cfg.DataDir, Library: s.lib, Exe: s.cfg.Exe, DaemonURL: s.daemonURL,
		AgentToken: s.st.S.AgentTokens[a.ID], AuthStyle: mp.AuthStyle}
	mu := s.agentMu[a.ID]
	if mu == nil {
		mu = &sync.Mutex{}
		s.agentMu[a.ID] = mu
	}
	s.hub.Publish("run", run)

	var typingCh *model.Channel
	var typingChat string
	out := s.outbound
	if out != nil && strings.HasPrefix(m.From, "channel:") {
		typingCh, typingChat = s.channelForAddr(m.From)
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		if typingCh != nil {
			go func() {
				tick := time.NewTicker(4 * time.Second)
				defer tick.Stop()
				for {
					out.Typing(ctx, typingCh, typingChat)
					select {
					case <-ctx.Done():
						return
					case <-tick.C:
					}
				}
			}()
		}
		mu.Lock()
		paths, warnings, err := profile.Materialize(&agentCopy, opts)
		mu.Unlock()
		var res RunResult
		if err != nil {
			res = RunResult{Err: fmt.Errorf("prepare agent home: %w", err), ExitCode: -1}
		} else {
			if len(warnings) > 0 {
				_ = os.MkdirAll(paths.Root, 0o700)
				s.activity(run.ID, "⚠ "+strings.Join(warnings, "; "))
			}
			spec.Home, spec.Workspace = paths.Home, paths.Workspace
			res = s.cfg.Engine.Run(ctx, spec)
		}
		s.finish(ar, res)
	}()
	return nil
}

// activity publishes one progress line of a run.
func (s *Service) activity(runID, line string) {
	s.hub.Publish("activity", map[string]string{"run_id": runID, "line": line})
}

// finish records the outcome of a run and routes the answer.
func (s *Service) finish(ar *activeRun, res RunResult) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	delete(s.active, ar.run.ID)
	delete(s.byConv, ar.agent.ID+"/"+ar.msg.ThreadID)
	s.expireApprovalsLocked(ar.run.ID)

	run := ar.run
	run.Ended = time.Now().UTC()
	run.ExitCode = res.ExitCode
	run.Prompt, run.Answer, run.Requests = res.Prompt, res.Answered, res.Requests
	m := s.st.FindMessage(ar.msg.ThreadID, ar.msg.ID)
	if m == nil {
		m = ar.msg
	}

	if res.ExitCode == exitLocked && !ar.stop && s.ctx.Err() == nil {
		// Someone else holds the engine session (e.g. the owner attached a
		// terminal). Put the request back and try again shortly.
		run.State, run.Error = model.RunFailed, "session busy; requeued"
		if t := s.st.Thread(m.ThreadID); t != nil && t.Runs > 0 {
			t.Runs--
		}
		m.Status = model.StatusQueued
		s.updateLocked(m)
		go func() {
			select {
			case <-time.After(5 * time.Second):
			case <-s.ctx.Done():
			}
			s.st.Mu.Lock()
			s.queue = append(s.queue, queued{m.ThreadID, m.ID})
			s.st.Mu.Unlock()
			s.wake()
		}()
		s.hub.Publish("run", run)
		_ = s.st.Save()
		return
	}

	reply := &model.Message{Kind: model.KindReply, Text: res.Answer}
	switch {
	case ar.stop:
		run.State, run.Error = model.RunStopped, "stopped by the owner"
		reply.Kind, reply.Text = model.KindNotice, "The run was stopped by the owner."
	case s.ctx.Err() != nil:
		run.State, run.Error = model.RunInterrupted, "the daemon is shutting down"
		reply.Kind, reply.Text = model.KindError, "The run was interrupted because ZPilot is shutting down. It was not retried automatically."
	case res.TimedOut:
		run.State, run.Error = model.RunTimeout, fmt.Sprintf("exceeded %d minutes", ar.agent.TimeoutMinutes)
		reply.Kind, reply.Text = model.KindError, fmt.Sprintf("The run took longer than the %d-minute limit and was stopped.", ar.agent.TimeoutMinutes)
	case res.Err != nil:
		run.State, run.Error = model.RunFailed, res.Err.Error()
		reply.Kind, reply.Text = model.KindError, "The run failed: "+res.Err.Error()
	case res.ExitCode == exitOK:
		run.State = model.RunDone
		if reply.Text == "" {
			reply.Kind, reply.Text = model.KindNotice, "(The agent finished without a written answer.)"
		}
	default:
		run.State = model.RunFailed
		run.Error = lastLines(res.Tail, 6)
		reply.Kind = model.KindError
		why := map[int]string{exitFailed: "the turn failed", exitUsage: "the engine rejected its configuration"}[res.ExitCode]
		if why == "" {
			why = fmt.Sprintf("the engine exited with code %d", res.ExitCode)
		}
		reply.Text = fmt.Sprintf("The run failed (%s).\n\n%s", why, lastLines(res.Tail, 6))
		if res.Answer != "" {
			reply.Text = res.Answer + "\n\n---\n" + reply.Text
		}
	}
	if run.State == model.RunDone {
		m.Status = model.StatusDone
	} else {
		m.Status = model.StatusFailed
		m.Detail = run.State
	}
	s.updateLocked(m)
	s.routeResultLocked(m, reply)
	s.hub.Publish("run", run)
	if err := s.st.Save(); err != nil {
		s.cfg.Log("save state: %v", err)
	}
	s.wake()
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ---- runs --------------------------------------------------------------

// Runs lists recent runs, newest first; agent filters by username.
func (s *Service) Runs(agent string, limit int) []model.Run {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	agent = model.NormalizeUsername(agent)
	out := []model.Run{}
	for i := len(s.st.S.Runs) - 1; i >= 0 && len(out) < limit; i-- {
		r := s.st.S.Runs[i]
		if agent == "" || r.Username == agent {
			out = append(out, *r)
		}
	}
	return out
}

// RunLog returns the engine log of a run.
func (s *Service) RunLog(id string) (string, error) {
	s.st.Mu.Lock()
	r := s.st.Run(id)
	s.st.Mu.Unlock()
	if r == nil {
		return "", ErrNotFound
	}
	b, err := os.ReadFile(s.runLogPath(id))
	if err != nil {
		return "", nil
	}
	const max = 512 * 1024
	if len(b) > max {
		b = append([]byte("…(earlier output trimmed)\n"), b[len(b)-max:]...)
	}
	return string(b), nil
}

// StopRun stops a running engine (and everything it started).
func (s *Service) StopRun(actor, id string) error {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	ar := s.active[id]
	if ar == nil {
		return bad("run %s is not running", id)
	}
	ar.stop = true
	ar.cancel()
	s.st.Audit(actor, "run.stop", "@"+ar.agent.Username, id)
	return nil
}

// Active reports the number of running and queued requests.
func (s *Service) Active() (running, queued int) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	return len(s.active), len(s.queue)
}
