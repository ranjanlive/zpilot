// Package core is the ZPilot daemon's brain: it owns agents, threads and
// the message bus, decides which request runs next, runs the engine, routes
// replies (to the owner, other agents, channels) and enforces the
// collaboration rules (contacts, hop limits, cycle detection, run budgets).
//
// All state lives in a store.Store; every exported method locks it. Engine
// runs happen outside the lock.
package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ranjanlive/zpilot/internal/library"
	"github.com/ranjanlive/zpilot/internal/model"
	"github.com/ranjanlive/zpilot/internal/store"
	"github.com/ranjanlive/zpilot/internal/vault"
)

// Outbound delivers replies to external channels (implemented by package channels).
type Outbound interface {
	Send(ctx context.Context, ch *model.Channel, chatID, text string) error
	Typing(ctx context.Context, ch *model.Channel, chatID string)
}

// Config wires a Service.
type Config struct {
	DataDir string
	// Exe is the zpilot binary; it runs the engine ("zpilot engine ...") and the
	// agent collaboration tools ("zpilot agent-tool ...").
	Exe string
	// Engine runs one engine invocation. Nil uses the subprocess engine.
	Engine Engine
	Log    func(format string, args ...any)
}

// Service is the daemon core.
type Service struct {
	cfg   Config
	st    *store.Store
	vault *vault.Vault
	lib   *library.Library
	hub   *Hub

	daemonURL string // set by the HTTP server once it listens
	outbound  Outbound

	// dispatch state (guarded by st.Mu)
	queue   []queued                   // pending deliveries, oldest first
	active  map[string]*activeRun      // run id -> active run
	byConv  map[string]*activeRun      // agent id + "/" + thread id -> active run
	waiters map[string][]chan struct{} // request message id -> waiters for its reply
	agentMu map[string]*sync.Mutex     // serialises home materialisation per agent

	kick   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type queued struct{ threadID, msgID string }

type activeRun struct {
	run    *model.Run
	msg    *model.Message
	agent  *model.Agent
	cancel context.CancelFunc
	stop   bool // stopped by the owner

	bridgeToken string // authenticates the engine's bridge calls
	liveText    string // answer text streamed so far
	liveSteps   []string
	liveState   string
}

// Open loads the data directory and prepares the service (call Start to run it).
func Open(cfg Config) (*Service, error) {
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	if cfg.Exe == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		cfg.Exe = exe
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	cfg.DataDir = st.Dir
	v, err := vault.Open(st.Dir)
	if err != nil {
		st.Close()
		return nil, err
	}
	if cfg.Engine == nil {
		cfg.Engine = &ProcessEngine{Exe: cfg.Exe}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		cfg: cfg, st: st, vault: v, lib: library.New(st.Dir), hub: NewHub(),
		active: map[string]*activeRun{}, byConv: map[string]*activeRun{},
		waiters: map[string][]chan struct{}{}, agentMu: map[string]*sync.Mutex{},
		kick: make(chan struct{}, 1), ctx: ctx, cancel: cancel,
	}
	if err := s.recover(); err != nil {
		st.Close()
		return nil, err
	}
	return s, nil
}

// DataDir is the absolute data directory.
func (s *Service) DataDir() string { return s.cfg.DataDir }

// Library exposes the tool and skill library.
func (s *Service) Library() *library.Library { return s.lib }

// Vault exposes the secret store.
func (s *Service) Vault() *vault.Vault { return s.vault }

// Events exposes the event hub.
func (s *Service) Events() *Hub { return s.hub }

// SetDaemonURL tells the service how collaboration tools reach the API.
func (s *Service) SetDaemonURL(u string) {
	s.st.Mu.Lock()
	s.daemonURL = u
	s.st.Mu.Unlock()
}

// SetOutbound connects channel delivery.
func (s *Service) SetOutbound(o Outbound) {
	s.st.Mu.Lock()
	s.outbound = o
	s.st.Mu.Unlock()
}

// Start runs the dispatcher and scheduler.
func (s *Service) Start() {
	s.wg.Add(2)
	go s.dispatchLoop()
	go s.scheduleLoop()
	s.wake()
}

// Close stops new work, stops running engines and releases the data directory.
func (s *Service) Close() {
	s.cancel()
	s.st.Mu.Lock()
	for _, ar := range s.active {
		ar.cancel()
	}
	s.st.Mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		s.cfg.Log("shutdown: some runs did not stop in time")
	}
	s.st.Close()
}

// recover runs at startup: requests that were running when the daemon
// stopped are marked failed (never blindly re-run: they may have had side
// effects), and queued requests are re-queued.
func (s *Service) recover() error {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	if err := s.ensureManagerLocked(); err != nil {
		return err
	}
	s.migrateUsersLocked()
	for _, a := range s.st.S.Approvals {
		if a.State == model.ApprovalPending {
			a.State, a.Reason, a.Decided = model.ApprovalExpired, "the daemon restarted", time.Now().UTC()
		}
	}
	for _, r := range s.st.S.Runs {
		if r.State == model.RunRunning {
			r.State = model.RunInterrupted
			r.Error = "the daemon stopped while this run was in progress"
			r.Ended = time.Now().UTC()
		}
	}
	ids := s.st.AllThreadIDs()
	for i := len(ids) - 1; i >= 0; i-- { // oldest thread first
		tid := ids[i]
		changed := false
		for _, m := range s.st.Messages(tid) {
			if !model.IsAgentAddr(m.To) {
				continue
			}
			switch m.Status {
			case model.StatusRunning:
				m.Status = model.StatusFailed
				m.Detail = "interrupted"
				m.Updated = time.Now().UTC()
				changed = true
				s.routeResultLocked(m, &model.Message{Kind: model.KindError,
					Text: "This request was interrupted because ZPilot stopped while it was running. It was not retried automatically, because it may already have had side effects. Send it again if needed."})
			case model.StatusQueued:
				s.queue = append(s.queue, queued{tid, m.ID})
			}
		}
		if changed {
			if err := s.st.SaveThread(tid); err != nil {
				return err
			}
		}
	}
	return s.st.Save()
}

// ensureManagerLocked creates the built-in manager agent on first start.
func (s *Service) ensureManagerLocked() error {
	if s.st.AgentByUsername(model.ManagerUsername) != nil {
		return nil
	}
	now := time.Now().UTC()
	a := &model.Agent{
		ID: model.NewID("agt"), Username: model.ManagerUsername, Name: "Manager", Icon: "🧭", Color: "#0ea5e9",
		Description: "Built-in administrator. Inspects the installation and proposes new agents, schedules and tool installs for the owner to approve.",
		Instruction: "You are the manager of this ZPilot installation. Help the owner design, organise and troubleshoot their agents and workflows. Be concise and concrete.",
		Level:       model.LevelRestricted, Contacts: []string{"*"}, Builtin: true, Created: now, Updated: now,
		ModelID: s.st.S.Settings.DefaultModelID,
	}
	a.Defaults()
	s.st.S.Agents = append(s.st.S.Agents, a)
	s.st.S.AgentTokens[a.ID] = model.NewToken()
	s.st.Audit("system", "agent.create", "@manager", "built-in manager created")
	return nil
}

// wake nudges the dispatcher.
func (s *Service) wake() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// ---- errors ------------------------------------------------------------

// ErrNotFound is returned for unknown ids.
var ErrNotFound = errors.New("not found")

// InputError is a problem with the caller's request (HTTP 400).
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }

func bad(format string, a ...any) error { return &InputError{Msg: fmt.Sprintf(format, a...)} }

// ForbiddenError is a refused action (HTTP 403).
type ForbiddenError struct{ Msg string }

func (e *ForbiddenError) Error() string { return e.Msg }

func forbid(format string, a ...any) error { return &ForbiddenError{Msg: fmt.Sprintf(format, a...)} }

// ---- settings ----------------------------------------------------------

// Settings returns installation settings.
func (s *Service) Settings() model.Settings {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	return s.st.S.Settings
}

// UpdateSettings validates and saves settings.
func (s *Service) UpdateSettings(actor string, in model.Settings) (model.Settings, error) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	in.Defaults()
	if err := in.Validate(); err != nil {
		return in, bad("%v", err)
	}
	if in.DefaultModelID != "" && s.st.ModelProfile(in.DefaultModelID) == nil {
		return in, bad("default model %s does not exist", in.DefaultModelID)
	}
	s.st.S.Settings = in
	// The manager follows the default model until someone picks another.
	if m := s.st.AgentByUsername(model.ManagerUsername); m != nil && m.ModelID == "" {
		m.ModelID = in.DefaultModelID
	}
	if err := s.st.Save(); err != nil {
		return in, err
	}
	s.st.Audit(actor, "settings.update", "", "")
	s.hub.Publish("settings", in)
	s.wake()
	return in, nil
}

// Audit returns recent audit entries.
func (s *Service) Audit(n int) ([]model.AuditEntry, error) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	return s.st.ReadAudit(n)
}

// IsSetUp reports whether at least one model exists (used by the UI's onboarding).
func (s *Service) IsSetUp() bool {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	return len(s.st.S.Models) > 0
}

// runLogPath is where a run's engine output is kept.
func (s *Service) runLogPath(runID string) string {
	return filepath.Join(s.cfg.DataDir, "runs", runID+".log")
}
