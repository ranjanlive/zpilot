package core

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/internal/library"
	"github.com/ranjanlive/zpilot/internal/model"
	"github.com/ranjanlive/zpilot/internal/trigger"
)

// ProposeInput is a manage_propose tool call.
type ProposeInput struct {
	Caller  *model.Agent
	Session string
	Action  string          `json:"action"`
	Target  string          `json:"target"`
	Payload json.RawMessage `json:"payload"`
	Reason  string          `json:"reason"`
}

// Propose records a change for the owner to approve. Only the built-in
// manager may propose.
func (s *Service) Propose(in ProposeInput) (model.Proposal, error) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	caller := s.st.Agent(in.Caller.ID)
	if caller == nil || !caller.Builtin {
		return model.Proposal{}, forbid("only the manager can propose changes")
	}
	if !slices.Contains(model.ProposalActions, in.Action) {
		return model.Proposal{}, bad("unknown action %q", in.Action)
	}
	if strings.TrimSpace(in.Reason) == "" {
		return model.Proposal{}, bad("give a reason so the owner can decide")
	}
	if len(in.Payload) == 0 || string(in.Payload) == "null" {
		in.Payload = json.RawMessage("{}")
	}
	if len(in.Payload) > 128*1024 {
		return model.Proposal{}, bad("payload is too large")
	}
	// Dry-run validation so the manager learns about mistakes immediately.
	if err := s.validateProposalLocked(in.Action, in.Target, in.Payload); err != nil {
		return model.Proposal{}, err
	}
	threadID, _ := model.ThreadFromSession(in.Session)
	p := model.Proposal{ID: model.NewID("prp"), By: caller.Username, ThreadID: threadID, Action: in.Action,
		Target: in.Target, Payload: in.Payload, Reason: strings.TrimSpace(in.Reason), State: model.ProposalPending, Created: time.Now().UTC()}
	c := p
	s.st.S.Proposals = append(s.st.S.Proposals, &c)
	if err := s.st.Save(); err != nil {
		return p, err
	}
	s.st.Audit("@"+caller.Username, "proposal.create", in.Action, in.Target)
	s.hub.Publish("proposal", p)
	return p, nil
}

// Proposals lists proposals, pending first then newest.
func (s *Service) Proposals() []model.Proposal {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	out := []model.Proposal{}
	for _, p := range s.st.S.Proposals {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		pi, pj := out[i].State == model.ProposalPending, out[j].State == model.ProposalPending
		if pi != pj {
			return pi
		}
		return out[i].Created.After(out[j].Created)
	})
	return out
}

// DecideProposal approves (applies) or rejects a pending proposal.
func (s *Service) DecideProposal(ctx context.Context, actor, id string, approve bool) (model.Proposal, error) {
	s.st.Mu.Lock()
	p := s.st.Proposal(id)
	if p == nil {
		s.st.Mu.Unlock()
		return model.Proposal{}, ErrNotFound
	}
	if p.State != model.ProposalPending {
		s.st.Mu.Unlock()
		return *p, bad("proposal is already %s", p.State)
	}
	if !approve {
		p.State, p.Decided = model.ProposalRejected, time.Now().UTC()
		err := s.st.Save()
		s.st.Audit(actor, "proposal.reject", p.Action, p.ID)
		s.notifyManagerLocked(p)
		s.hub.Publish("proposal", p)
		c := *p
		s.st.Mu.Unlock()
		return c, err
	}
	pc := *p
	s.st.Mu.Unlock()

	// Apply outside the lock: the operations below lock on their own, and
	// installing a tool downloads from the network.
	result, err := s.applyProposal(ctx, actor, pc)

	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	p = s.st.Proposal(id)
	if p == nil {
		return pc, ErrNotFound
	}
	p.Decided = time.Now().UTC()
	if err != nil {
		p.State, p.Result = model.ProposalFailed, err.Error()
	} else {
		p.State, p.Result = model.ProposalApproved, result
	}
	_ = s.st.Save()
	s.st.Audit(actor, "proposal."+p.State, p.Action, p.ID+" "+p.Result)
	s.notifyManagerLocked(p)
	s.hub.Publish("proposal", p)
	return *p, err
}

// notifyManagerLocked tells the manager's conversation how a proposal ended.
func (s *Service) notifyManagerLocked(p *model.Proposal) {
	t := s.st.Thread(p.ThreadID)
	if t == nil {
		return
	}
	text := fmt.Sprintf("Proposal %s (%s %s) was %s by the owner.", p.ID, p.Action, p.Target, p.State)
	if p.Result != "" {
		text += " " + p.Result
	}
	_ = s.appendLocked(t, &model.Message{From: "system", FromName: "ZPilot", To: "@" + p.By, Kind: model.KindNotice, Text: text})
}

// agentPatch is the payload of create_agent / update_agent.
type agentPatch struct {
	Username    *string   `json:"username"`
	Name        *string   `json:"name"`
	Icon        *string   `json:"icon"`
	Description *string   `json:"description"`
	Instruction *string   `json:"instruction"`
	Level       *string   `json:"level"`
	ModelID     *string   `json:"model_id"`
	Workspace   *string   `json:"workspace"`
	Tools       *[]string `json:"tools"`
	Skills      *[]string `json:"skills"`
	Contacts    *[]string `json:"contacts"`
	AcceptFrom  *[]string `json:"accept_from"`
	Disabled    *bool     `json:"disabled"`
}

func (p agentPatch) apply(a *model.Agent) {
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&a.Username, p.Username)
	set(&a.Name, p.Name)
	set(&a.Icon, p.Icon)
	set(&a.Description, p.Description)
	set(&a.Instruction, p.Instruction)
	set(&a.ModelID, p.ModelID)
	set(&a.Workspace, p.Workspace)
	if p.Level != nil {
		a.Level = model.Level(*p.Level)
	}
	for _, f := range []struct {
		dst *[]string
		v   *[]string
	}{{&a.Tools, p.Tools}, {&a.Skills, p.Skills}, {&a.Contacts, p.Contacts}, {&a.AcceptFrom, p.AcceptFrom}} {
		if f.v != nil {
			*f.dst = slices.Clone(*f.v)
		}
	}
	if p.Disabled != nil {
		a.Disabled = *p.Disabled
	}
}

type schedulePayload struct {
	Name        string          `json:"name"`
	Agent       string          `json:"agent"`
	Task        string          `json:"task"`
	Trigger     trigger.Trigger `json:"trigger"`
	FreshThread bool            `json:"fresh_thread"`
}

func decodeStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return bad("payload: %v", err)
	}
	return nil
}

func (s *Service) validateProposalLocked(action, target string, raw json.RawMessage) error {
	switch action {
	case model.ActCreateAgent, model.ActUpdateAgent:
		var p agentPatch
		if err := decodeStrict(raw, &p); err != nil {
			return err
		}
		a := model.Agent{}
		self := ""
		if action == model.ActUpdateAgent {
			cur := s.st.AgentRef(model.NormalizeUsername(target))
			if cur == nil {
				return bad("target @%s does not exist", model.NormalizeUsername(target))
			}
			if cur.Builtin {
				return forbid("the manager cannot change itself")
			}
			a = *cur
			a.Contacts, a.AcceptFrom = slices.Clone(cur.Contacts), slices.Clone(cur.AcceptFrom)
			self = cur.ID
		} else if a.ModelID == "" {
			a.ModelID = s.st.S.Settings.DefaultModelID
		}
		p.apply(&a)
		return s.checkAgentLocked(&a, self)
	case model.ActDeleteAgent:
		a := s.st.AgentRef(model.NormalizeUsername(target))
		if a == nil {
			return bad("target @%s does not exist", model.NormalizeUsername(target))
		}
		if a.Builtin {
			return forbid("the manager cannot be deleted")
		}
	case model.ActCreateSchedule:
		var p schedulePayload
		if err := decodeStrict(raw, &p); err != nil {
			return err
		}
		a := s.st.AgentRef(model.NormalizeUsername(p.Agent))
		if a == nil {
			return bad("agent @%s does not exist", p.Agent)
		}
		sc := model.Schedule{Name: p.Name, AgentID: a.ID, Task: p.Task, Trigger: p.Trigger}
		if sc.Trigger.Timezone == "" {
			sc.Trigger.Timezone = "Local"
		}
		if err := sc.Validate(); err != nil {
			return bad("%v", err)
		}
	case model.ActDeleteSchedule:
		if s.st.Schedule(target) == nil {
			return bad("schedule %s does not exist", target)
		}
	case model.ActInstallTool:
		var p struct {
			ID string `json:"id"`
		}
		if err := decodeStrict(raw, &p); err != nil {
			return err
		}
		if !model.ValidSlug(p.ID) {
			return bad("invalid tool id")
		}
	}
	return nil
}

func (s *Service) applyProposal(ctx context.Context, actor string, p model.Proposal) (string, error) {
	who := actor + " (proposed by @" + p.By + ")"
	switch p.Action {
	case model.ActCreateAgent:
		var patch agentPatch
		if err := decodeStrict(p.Payload, &patch); err != nil {
			return "", err
		}
		a := model.Agent{}
		patch.apply(&a)
		v, err := s.CreateAgent(who, a)
		if err != nil {
			return "", err
		}
		return "created @" + v.Username, nil
	case model.ActUpdateAgent:
		var patch agentPatch
		if err := decodeStrict(p.Payload, &patch); err != nil {
			return "", err
		}
		cur, err := s.Agent(model.NormalizeUsername(p.Target))
		if err != nil {
			return "", err
		}
		a := cur.Agent
		patch.apply(&a)
		v, err := s.UpdateAgent(who, cur.ID, a)
		if err != nil {
			return "", err
		}
		return "updated @" + v.Username, nil
	case model.ActDeleteAgent:
		if err := s.DeleteAgent(who, model.NormalizeUsername(p.Target)); err != nil {
			return "", err
		}
		return "deleted @" + model.NormalizeUsername(p.Target), nil
	case model.ActCreateSchedule:
		var sp schedulePayload
		if err := decodeStrict(p.Payload, &sp); err != nil {
			return "", err
		}
		sc, err := s.SaveSchedule(who, model.Schedule{Name: sp.Name, AgentID: model.NormalizeUsername(sp.Agent), Task: sp.Task,
			Trigger: sp.Trigger, FreshThread: sp.FreshThread, Enabled: true})
		if err != nil {
			return "", err
		}
		return "created schedule " + sc.ID, nil
	case model.ActDeleteSchedule:
		if err := s.DeleteSchedule(who, p.Target); err != nil {
			return "", err
		}
		return "deleted schedule " + p.Target, nil
	case model.ActInstallTool:
		var tp struct {
			ID string `json:"id"`
		}
		if err := decodeStrict(p.Payload, &tp); err != nil {
			return "", err
		}
		m, err := s.InstallTool(ctx, who, tp.ID, library.InstallOptions{})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("installed %s %s", m.ID, m.Version), nil
	}
	return "", bad("unknown action")
}

// ---- tools and skills (audited wrappers over the library) -------------

// InstallTool installs a registry tool.
func (s *Service) InstallTool(ctx context.Context, actor, id string, opt library.InstallOptions) (library.ToolMeta, error) {
	m, err := s.lib.Install(ctx, id, opt)
	if err != nil {
		return m, err
	}
	s.st.Mu.Lock()
	s.st.Audit(actor, "tool.install", id, fmt.Sprintf("%s verified=%v sha256=%s", m.Version, m.Verified, m.SHA256))
	s.st.Mu.Unlock()
	s.hub.Publish("tools", nil)
	return m, nil
}

// InstallLocalTool installs a tool folder from disk.
func (s *Service) InstallLocalTool(actor, dir string) (library.ToolMeta, error) {
	m, err := s.lib.InstallLocal(dir)
	if err != nil {
		return m, bad("%v", err)
	}
	s.st.Mu.Lock()
	s.st.Audit(actor, "tool.install_local", m.ID, dir)
	s.st.Mu.Unlock()
	s.hub.Publish("tools", nil)
	return m, nil
}

// RemoveTool uninstalls a tool that no agent uses.
func (s *Service) RemoveTool(actor, id string) error {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	for _, a := range s.st.S.Agents {
		if slices.Contains(a.Tools, id) {
			return bad("@%s uses %s; remove it from the agent first", a.Username, id)
		}
	}
	if err := s.lib.RemoveTool(id); err != nil {
		return bad("%v", err)
	}
	s.st.Audit(actor, "tool.remove", id, "")
	s.hub.Publish("tools", nil)
	return nil
}

// SaveSkill creates or updates a library skill.
func (s *Service) SaveSkill(actor string, sk library.Skill) error {
	if err := s.lib.SaveSkill(sk); err != nil {
		return bad("%v", err)
	}
	s.st.Mu.Lock()
	s.st.Audit(actor, "skill.save", sk.Name, "")
	s.st.Mu.Unlock()
	s.hub.Publish("skills", nil)
	return nil
}

// RemoveSkill deletes a skill no agent uses.
func (s *Service) RemoveSkill(actor, name string) error {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	for _, a := range s.st.S.Agents {
		if slices.Contains(a.Skills, name) {
			return bad("@%s uses skill %s; remove it from the agent first", a.Username, name)
		}
	}
	if err := s.lib.RemoveSkill(name); err != nil {
		return bad("%v", err)
	}
	s.st.Audit(actor, "skill.remove", name, "")
	s.hub.Publish("skills", nil)
	return nil
}

// ---- manage_status -----------------------------------------------------

// ManageStatus answers the manager's inspection tool.
func (s *Service) ManageStatus(ctx context.Context, caller *model.Agent, what string) (any, error) {
	if !caller.Builtin {
		return nil, forbid("only the manager can inspect the installation")
	}
	switch what {
	case "agents":
		type row struct {
			Username, Name, Level, Model, Description string
			Tools, Skills, Contacts                   []string
			Disabled                                  bool
			Running, Queued                           int
		}
		var out []row
		for _, a := range s.Agents() {
			out = append(out, row{"@" + a.Username, a.Name, string(a.Level), a.ModelName, a.Description, a.Tools, a.Skills, a.Contacts, a.Disabled, a.Running, a.Queued})
		}
		return out, nil
	case "models":
		var out []map[string]any
		for _, m := range s.Models() {
			out = append(out, map[string]any{"id": m.ID, "name": m.Name, "provider": m.Provider, "model": m.Model, "has_key": m.HasKey})
		}
		return out, nil
	case "tools":
		return s.lib.Tools()
	case "skills":
		return s.lib.Skills()
	case "schedules":
		return s.Schedules(), nil
	case "channels":
		var out []map[string]any
		for _, c := range s.Channels() {
			out = append(out, map[string]any{"id": c.ID, "kind": c.Kind, "name": c.Name, "agent": "@" + c.Agent, "enabled": c.Enabled, "status": c.Status})
		}
		return out, nil
	case "runs":
		return s.Runs("", 30), nil
	case "proposals":
		return s.Proposals(), nil
	case "registry":
		reg, err := s.lib.FetchRegistry(ctx)
		if err != nil {
			return nil, err
		}
		var out []map[string]any
		for _, t := range reg.Tools {
			out = append(out, map[string]any{"id": t.ID, "name": t.Name, "version": t.Version, "category": t.Category, "description": t.Description, "installed": t.Installed})
		}
		return out, nil
	}
	return nil, bad("unknown section %q", what)
}
