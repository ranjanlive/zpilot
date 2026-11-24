// Package store persists ZPilot's state on disk.
//
// Layout under the data directory:
//
//	state.json            agents, models, schedules, channels, proposals, threads index, settings
//	threads/<id>.json     the messages of one thread
//	audit.jsonl           append-only audit log
//
// Every write goes to a temporary file that is fsynced and then renamed over
// the target, so a crash leaves either the old or the new version, never a
// torn file. The daemon is the only writer (guarded by a lock file).
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/ranjanlive/zpilot/internal/model"
	"github.com/ranjanlive/zpilot/internal/platform"
)

// StateVersion is bumped when the on-disk format changes incompatibly.
const StateVersion = 1

// maxRuns caps the run history kept in state.json.
const maxRuns = 2000

// State is everything except thread messages.
type State struct {
	Version   int                   `json:"version"`
	Settings  model.Settings        `json:"settings"`
	Agents    []*model.Agent        `json:"agents"`
	Models    []*model.ModelProfile `json:"models"`
	Schedules []*model.Schedule     `json:"schedules"`
	Channels  []*model.Channel      `json:"channels"`
	Proposals []*model.Proposal     `json:"proposals"`
	Threads   []*model.Thread       `json:"threads"`
	Runs      []*model.Run          `json:"runs"`
	Approvals []*model.Approval     `json:"approvals"`
	Workflows []*model.Workflow     `json:"workflows"`
	WFRuns    []*model.WorkflowRun  `json:"workflow_runs"`
	// AgentTokens maps agent id to the token its tools use to call back.
	AgentTokens map[string]string `json:"agent_tokens"`
	// ChannelThreads maps "<channel id>/<external chat id>" to a thread id.
	ChannelThreads map[string]string `json:"channel_threads"`
	// AdminPassword is the legacy single-password hash (migrated to Users).
	AdminPassword string `json:"admin_password,omitempty"`
	// Users are dashboard accounts (password sign-in, roles).
	Users []*model.User `json:"users"`
}

// Store owns the files. Callers hold Store.Mu while reading or changing State
// and call Save / SaveThread before releasing it.
type Store struct {
	Dir string
	Mu  sync.Mutex
	S   *State

	threads map[string][]*model.Message // loaded thread messages
	audit   *os.File
	lock    *platform.Lock
}

// Open loads (or creates) the data directory and takes the single-writer lock.
func Open(dir string) (*Store, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{dir, filepath.Join(dir, "threads")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	lock, err := platform.Acquire(filepath.Join(dir, "daemon.lock"))
	if err != nil {
		return nil, err
	}
	st := &Store{Dir: dir, threads: map[string][]*model.Message{}, lock: lock}
	if err := st.load(); err != nil {
		lock.Close()
		return nil, err
	}
	st.audit, err = os.OpenFile(filepath.Join(dir, "audit.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		lock.Close()
		return nil, err
	}
	return st, nil
}

// Close releases the lock.
func (st *Store) Close() {
	if st.audit != nil {
		st.audit.Close()
	}
	if st.lock != nil {
		st.lock.Close()
	}
}

func (st *Store) statePath() string { return filepath.Join(st.Dir, "state.json") }

func (st *Store) load() error {
	s := &State{Version: StateVersion}
	b, err := os.ReadFile(st.statePath())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(b, s); err != nil {
			return fmt.Errorf("state.json is damaged (%v); restore it from state.json.bak", err)
		}
		if s.Version > StateVersion {
			return fmt.Errorf("state.json was written by a newer ZPilot (format %d > %d)", s.Version, StateVersion)
		}
	}
	if s.AgentTokens == nil {
		s.AgentTokens = map[string]string{}
	}
	if s.ChannelThreads == nil {
		s.ChannelThreads = map[string]string{}
	}
	s.Settings.Defaults()
	for _, a := range s.Agents {
		a.Defaults()
	}
	st.S = s
	return nil
}

// Save writes state.json, keeping the previous version as state.json.bak.
func (st *Store) Save() error {
	if len(st.S.Runs) > maxRuns {
		st.S.Runs = st.S.Runs[len(st.S.Runs)-maxRuns:]
	}
	if len(st.S.WFRuns) > 300 {
		st.S.WFRuns = st.S.WFRuns[len(st.S.WFRuns)-300:]
	}
	if len(st.S.Approvals) > 500 {
		st.S.Approvals = st.S.Approvals[len(st.S.Approvals)-500:]
	}
	st.S.Version = StateVersion
	if _, err := os.Stat(st.statePath()); err == nil {
		if b, err := os.ReadFile(st.statePath()); err == nil {
			_ = WriteFileAtomic(st.statePath()+".bak", b, 0o600)
		}
	}
	return WriteJSONAtomic(st.statePath(), st.S)
}

// ---- threads -----------------------------------------------------------

func (st *Store) threadPath(id string) string { return filepath.Join(st.Dir, "threads", id+".json") }

// Messages returns the messages of a thread (loaded on first use).
func (st *Store) Messages(threadID string) []*model.Message {
	if m, ok := st.threads[threadID]; ok {
		return m
	}
	var msgs []*model.Message
	if b, err := os.ReadFile(st.threadPath(threadID)); err == nil {
		_ = json.Unmarshal(b, &msgs)
	}
	st.threads[threadID] = msgs
	return msgs
}

// AppendMessage adds a message to its thread and persists the thread.
func (st *Store) AppendMessage(m *model.Message) error {
	msgs := append(st.Messages(m.ThreadID), m)
	st.threads[m.ThreadID] = msgs
	return st.SaveThread(m.ThreadID)
}

// SaveThread persists a thread's messages.
func (st *Store) SaveThread(threadID string) error {
	return WriteJSONAtomic(st.threadPath(threadID), st.Messages(threadID))
}

// DeleteThread removes a thread and its messages.
func (st *Store) DeleteThread(threadID string) error {
	delete(st.threads, threadID)
	for i, t := range st.S.Threads {
		if t.ID == threadID {
			st.S.Threads = append(st.S.Threads[:i], st.S.Threads[i+1:]...)
			break
		}
	}
	for k, v := range st.S.ChannelThreads {
		if v == threadID {
			delete(st.S.ChannelThreads, k)
		}
	}
	err := os.Remove(st.threadPath(threadID))
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return err
}

// FindMessage locates a message by id within a thread.
func (st *Store) FindMessage(threadID, id string) *model.Message {
	for _, m := range st.Messages(threadID) {
		if m.ID == id {
			return m
		}
	}
	return nil
}

// AllThreadIDs lists every thread, newest first.
func (st *Store) AllThreadIDs() []string {
	ts := append([]*model.Thread{}, st.S.Threads...)
	sort.Slice(ts, func(i, j int) bool { return ts[i].Updated.After(ts[j].Updated) })
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.ID
	}
	return out
}

// ---- lookups -----------------------------------------------------------

func (st *Store) Agent(id string) *model.Agent {
	for _, a := range st.S.Agents {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (st *Store) AgentByUsername(u string) *model.Agent {
	u = model.NormalizeUsername(u)
	for _, a := range st.S.Agents {
		if a.Username == u {
			return a
		}
	}
	return nil
}

// AgentRef resolves an id or @username.
func (st *Store) AgentRef(ref string) *model.Agent {
	if a := st.Agent(ref); a != nil {
		return a
	}
	return st.AgentByUsername(ref)
}

func (st *Store) ModelProfile(id string) *model.ModelProfile {
	for _, m := range st.S.Models {
		if m.ID == id {
			return m
		}
	}
	return nil
}

func (st *Store) Thread(id string) *model.Thread {
	for _, t := range st.S.Threads {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (st *Store) Schedule(id string) *model.Schedule {
	for _, s := range st.S.Schedules {
		if s.ID == id {
			return s
		}
	}
	return nil
}

func (st *Store) Channel(id string) *model.Channel {
	for _, c := range st.S.Channels {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func (st *Store) Proposal(id string) *model.Proposal {
	for _, p := range st.S.Proposals {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func (st *Store) Run(id string) *model.Run {
	for i := len(st.S.Runs) - 1; i >= 0; i-- {
		if st.S.Runs[i].ID == id {
			return st.S.Runs[i]
		}
	}
	return nil
}

// ---- audit -------------------------------------------------------------

// Audit appends an entry to audit.jsonl. Failures are reported on stderr but
// never block the operation being audited.
func (st *Store) Audit(actor, action, target, detail string) {
	e := model.AuditEntry{Time: time.Now().UTC(), Actor: actor, Action: action, Target: target, Detail: detail}
	b, _ := json.Marshal(e)
	if _, err := st.audit.Write(append(b, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "zpilot: audit log write failed: %v\n", err)
	}
}

// ReadAudit returns the last n audit entries, newest first.
func (st *Store) ReadAudit(n int) ([]model.AuditEntry, error) {
	f, err := os.Open(filepath.Join(st.Dir, "audit.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var all []model.AuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var e model.AuditEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			all = append(all, e)
		}
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	return all, sc.Err()
}

// ---- atomic files ------------------------------------------------------

// WriteJSONAtomic marshals v (indented) and writes it atomically.
func WriteJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(b, '\n'), 0o600)
}

// WriteFileAtomic writes data to path via a fsynced temporary file + rename.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	if err := platform.Replace(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}
