package core

import (
	"crypto/subtle"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/internal/model"
	"github.com/ranjanlive/zpilot/internal/profile"
)

// AgentView is an agent plus derived, read-only facts for UIs.
type AgentView struct {
	model.Agent
	Address   string `json:"address"`
	Home      string `json:"home"`
	Workspace string `json:"workspace_path"`
	Running   int    `json:"running"`
	Queued    int    `json:"queued"`
	Channels  int    `json:"channels"`
	ModelName string `json:"model_name,omitempty"`
}

func (s *Service) viewLocked(a *model.Agent) AgentView {
	p := profile.For(s.cfg.DataDir, a)
	v := AgentView{Agent: *a, Address: model.AgentAddr(a.Username), Home: p.Home, Workspace: p.Workspace}
	for _, ar := range s.active {
		if ar.agent.ID == a.ID {
			v.Running++
		}
	}
	for _, q := range s.queue {
		if m := s.st.FindMessage(q.threadID, q.msgID); m != nil && m.To == v.Address {
			v.Queued++
		}
	}
	for _, c := range s.st.S.Channels {
		if c.AgentID == a.ID {
			v.Channels++
		}
	}
	if m := s.st.ModelProfile(a.ModelID); m != nil {
		v.ModelName = m.Name
	}
	return v
}

// Agents lists agents, manager first then by username.
func (s *Service) Agents() []AgentView {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	out := make([]AgentView, 0, len(s.st.S.Agents))
	for _, a := range s.st.S.Agents {
		out = append(out, s.viewLocked(a))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Builtin != out[j].Builtin {
			return out[i].Builtin
		}
		return out[i].Username < out[j].Username
	})
	return out
}

// Agent returns one agent by id or @username.
func (s *Service) Agent(ref string) (AgentView, error) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	a := s.st.AgentRef(ref)
	if a == nil {
		return AgentView{}, ErrNotFound
	}
	return s.viewLocked(a), nil
}

// checkAgentLocked validates a profile against what exists.
func (s *Service) checkAgentLocked(a *model.Agent, selfID string) error {
	a.Defaults()
	if err := a.Validate(); err != nil {
		return bad("%v", err)
	}
	if o := s.st.AgentByUsername(a.Username); o != nil && o.ID != selfID {
		return bad("@%s is already taken", a.Username)
	}
	if a.ModelID != "" && s.st.ModelProfile(a.ModelID) == nil {
		return bad("model %s does not exist", a.ModelID)
	}
	for _, c := range append(slices.Clone(a.Contacts), a.AcceptFrom...) {
		if c == "*" || c == a.Username {
			continue
		}
		if s.st.AgentByUsername(c) == nil {
			return bad("@%s is not an agent", c)
		}
	}
	if slices.Contains(a.Contacts, a.Username) {
		return bad("an agent cannot list itself as a contact")
	}
	installed := map[string]bool{}
	if ts, err := s.lib.Tools(); err == nil {
		for _, t := range ts {
			installed[t.ID] = true
		}
	}
	for _, t := range a.Tools {
		if !installed[t] {
			return bad("tool %s is not installed (install it from the Tools page first)", t)
		}
	}
	for _, sk := range a.Skills {
		if _, err := s.lib.Skill(sk, false); err != nil {
			return bad("skill %s is not in the library", sk)
		}
	}
	if a.Workspace != "" {
		if fi, err := os.Stat(a.Workspace); err != nil || !fi.IsDir() {
			return bad("workspace %s does not exist or is not a folder", a.Workspace)
		}
	}
	if a.Level.RunsCommands() && !a.ExternalInputWithCommands && !a.ApproveCommands {
		for _, c := range s.st.S.Channels {
			if c.AgentID == selfID && c.Enabled {
				return bad("@%s receives messages from the %s channel %q; an agent reachable from outside cannot run commands unless you confirm external_input_with_commands", a.Username, c.Kind, c.Name)
			}
		}
	}
	return nil
}

// CreateAgent adds an agent.
func (s *Service) CreateAgent(actor string, a model.Agent) (AgentView, error) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	a.ID = model.NewID("agt")
	a.Builtin = false
	if a.ModelID == "" {
		a.ModelID = s.st.S.Settings.DefaultModelID
	}
	if err := s.checkAgentLocked(&a, a.ID); err != nil {
		return AgentView{}, err
	}
	now := time.Now().UTC()
	a.Created, a.Updated = now, now
	s.st.S.Agents = append(s.st.S.Agents, &a)
	s.st.S.AgentTokens[a.ID] = model.NewToken()
	if err := s.st.Save(); err != nil {
		return AgentView{}, err
	}
	s.st.Audit(actor, "agent.create", "@"+a.Username, string(a.Level))
	s.hub.Publish("agents", nil)
	return s.viewLocked(&a), nil
}

// UpdateAgent replaces an agent's editable fields.
func (s *Service) UpdateAgent(actor, ref string, in model.Agent) (AgentView, error) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	cur := s.st.AgentRef(ref)
	if cur == nil {
		return AgentView{}, ErrNotFound
	}
	in.ID, in.Builtin, in.Created = cur.ID, cur.Builtin, cur.Created
	if cur.Builtin {
		in.Username = cur.Username // the manager keeps its handle
	}
	if err := s.checkAgentLocked(&in, cur.ID); err != nil {
		return AgentView{}, err
	}
	in.Updated = time.Now().UTC()
	detail := ""
	if in.Level != cur.Level {
		detail = fmt.Sprintf("level %s -> %s", cur.Level, in.Level)
	}
	renamed := in.Username != cur.Username
	old := cur.Username
	*cur = in
	if renamed { // keep other agents' contact lists pointing at the renamed agent
		for _, o := range s.st.S.Agents {
			for i, c := range o.Contacts {
				if c == old {
					o.Contacts[i] = in.Username
				}
			}
			for i, c := range o.AcceptFrom {
				if c == old {
					o.AcceptFrom[i] = in.Username
				}
			}
		}
	}
	if err := s.st.Save(); err != nil {
		return AgentView{}, err
	}
	s.st.Audit(actor, "agent.update", "@"+in.Username, detail)
	s.hub.Publish("agents", nil)
	s.wake()
	return s.viewLocked(cur), nil
}

// DeleteAgent removes an agent. Its home (sessions) is kept on disk under
// agents/<id> so nothing is lost by accident; the workspace is never touched.
func (s *Service) DeleteAgent(actor, ref string) error {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	a := s.st.AgentRef(ref)
	if a == nil {
		return ErrNotFound
	}
	if a.Builtin {
		return forbid("the built-in manager cannot be deleted (you can disable it)")
	}
	for _, ar := range s.active {
		if ar.agent.ID == a.ID {
			return bad("@%s is running; stop its runs first", a.Username)
		}
	}
	for _, c := range s.st.S.Channels {
		if c.AgentID == a.ID {
			return bad("@%s is connected to channel %q; remove or reassign the channel first", a.Username, c.Name)
		}
	}
	for _, sc := range s.st.S.Schedules {
		if sc.AgentID == a.ID {
			return bad("@%s has schedule %q; delete it first", a.Username, sc.Name)
		}
	}
	s.st.S.Agents = slices.DeleteFunc(s.st.S.Agents, func(x *model.Agent) bool { return x.ID == a.ID })
	delete(s.st.S.AgentTokens, a.ID)
	for _, o := range s.st.S.Agents {
		o.Contacts = slices.DeleteFunc(o.Contacts, func(c string) bool { return c == a.Username })
		o.AcceptFrom = slices.DeleteFunc(o.AcceptFrom, func(c string) bool { return c == a.Username })
	}
	// cancel its queued requests
	for _, q := range slices.Clone(s.queue) {
		if m := s.st.FindMessage(q.threadID, q.msgID); m != nil && m.To == model.AgentAddr(a.Username) {
			s.cancelQueuedLocked(m, "the agent was deleted")
		}
	}
	if err := s.st.Save(); err != nil {
		return err
	}
	s.st.Audit(actor, "agent.delete", "@"+a.Username, "")
	s.hub.Publish("agents", nil)
	return nil
}

// RotateAgentToken invalidates an agent's tool token (the next run gets a new one).
func (s *Service) RotateAgentToken(actor, ref string) error {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	a := s.st.AgentRef(ref)
	if a == nil {
		return ErrNotFound
	}
	s.st.S.AgentTokens[a.ID] = model.NewToken()
	s.st.Audit(actor, "agent.rotate_token", "@"+a.Username, "")
	return s.st.Save()
}

// AgentByToken authenticates a collaboration tool call.
func (s *Service) AgentByToken(tok string) (*model.Agent, bool) {
	if tok == "" {
		return nil, false
	}
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	for id, t := range s.st.S.AgentTokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(tok)) == 1 {
			if a := s.st.Agent(id); a != nil {
				c := *a
				return &c, true
			}
		}
	}
	return nil, false
}

// ---- models ------------------------------------------------------------

// Models lists model profiles (without keys).
func (s *Service) Models() []model.ModelProfile {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	out := make([]model.ModelProfile, 0, len(s.st.S.Models))
	for _, m := range s.st.S.Models {
		c := *m
		c.HasKey = s.vault.Has(m.KeyRef)
		out = append(out, c)
	}
	return out
}

// SaveModel creates or updates a model profile. A nil key leaves the stored
// key unchanged; an empty string removes it.
func (s *Service) SaveModel(actor string, in model.ModelProfile, key *string) (model.ModelProfile, error) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	in.Name = strings.TrimSpace(in.Name)
	in.BaseURL = strings.TrimSpace(in.BaseURL)
	if err := in.Validate(); err != nil {
		return in, bad("%v", err)
	}
	now := time.Now().UTC()
	var cur *model.ModelProfile
	if in.ID != "" {
		if cur = s.st.ModelProfile(in.ID); cur == nil {
			return in, ErrNotFound
		}
		in.Created, in.KeyRef = cur.Created, cur.KeyRef
	} else {
		in.ID = model.NewID("mdl")
		in.Created = now
		in.KeyRef = "model/" + in.ID
	}
	in.Updated = now
	if key != nil {
		if err := s.vault.Set(in.KeyRef, strings.TrimSpace(*key)); err != nil {
			return in, err
		}
	}
	in.HasKey = s.vault.Has(in.KeyRef)
	if cur != nil {
		*cur = in
	} else {
		c := in
		s.st.S.Models = append(s.st.S.Models, &c)
	}
	if s.st.S.Settings.DefaultModelID == "" {
		s.st.S.Settings.DefaultModelID = in.ID
		for _, a := range s.st.S.Agents {
			if a.ModelID == "" {
				a.ModelID = in.ID
			}
		}
	}
	if err := s.st.Save(); err != nil {
		return in, err
	}
	s.st.Audit(actor, "model.save", in.Name, in.Provider+" "+in.Model)
	s.hub.Publish("models", nil)
	s.wake()
	return in, nil
}

// DeleteModel removes a profile that no agent uses.
func (s *Service) DeleteModel(actor, id string) error {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	m := s.st.ModelProfile(id)
	if m == nil {
		return ErrNotFound
	}
	for _, a := range s.st.S.Agents {
		if a.ModelID == id {
			return bad("@%s uses this model; switch it to another model first", a.Username)
		}
	}
	s.st.S.Models = slices.DeleteFunc(s.st.S.Models, func(x *model.ModelProfile) bool { return x.ID == id })
	_ = s.vault.Delete(m.KeyRef)
	if s.st.S.Settings.DefaultModelID == id {
		s.st.S.Settings.DefaultModelID = ""
		if len(s.st.S.Models) > 0 {
			s.st.S.Settings.DefaultModelID = s.st.S.Models[0].ID
		}
	}
	if err := s.st.Save(); err != nil {
		return err
	}
	s.st.Audit(actor, "model.delete", m.Name, "")
	s.hub.Publish("models", nil)
	return nil
}

// modelEnvLocked returns the engine environment for a model profile.
func (s *Service) modelEnvLocked(m *model.ModelProfile) ([]string, error) {
	key, err := s.vault.Get(m.KeyRef)
	if err != nil {
		return nil, err
	}
	if key == "" && m.AuthStyle != "none" {
		return nil, fmt.Errorf("model %q has no API key; add one on the Models page", m.Name)
	}
	env := []string{
		"ZAGENT_PROVIDER=" + m.Provider,
		"ZAGENT_BASE_URL=" + m.BaseURL,
		"ZAGENT_API_KEY=" + key,
		"ZAGENT_MODEL=" + m.Model,
		"ZAGENT_VISION=" + boolStr(m.Vision),
		"ZAGENT_AUDIO=" + boolStr(m.Audio),
		"ZAGENT_PDF=" + boolStr(m.PDF),
		"ZAGENT_VIDEO=" + boolStr(m.Video),
	}
	if m.APIFormat != "" {
		env = append(env, "ZAGENT_API_FORMAT="+m.APIFormat)
	}
	if m.ContextLimit > 0 {
		env = append(env, fmt.Sprintf("ZAGENT_CONTEXT_LIMIT=%d", m.ContextLimit))
	}
	if m.CompactAt != "" {
		env = append(env, "ZAGENT_COMPACT_AT="+m.CompactAt)
	}
	return env, nil
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
