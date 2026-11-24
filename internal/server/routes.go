package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ranjanlive/zpilot/internal/core"
	"github.com/ranjanlive/zpilot/internal/library"
	"github.com/ranjanlive/zpilot/internal/model"
)

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	svc := s.svc

	// --- session ---
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil {
			s.sessions.End(c.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.who(r)
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": ok, "password_login": len(svc.Users()) > 0, "version": s.opt.Version,
			"user": map[string]string{"name": id.name, "role": id.role, "id": id.user}})
	})

	// one-time sign-in codes for links opened by the CLI or desktop app
	mux.HandleFunc("POST /api/login-code", s.admin(func(w http.ResponseWriter, r *http.Request, _ string) error {
		writeJSON(w, http.StatusOK, map[string]string{"code": s.MintLoginCode()})
		return nil
	}))

	// --- users (admin) ---
	type userIn struct {
		model.User
		Password string `json:"password"`
	}
	mux.HandleFunc("GET /api/users", s.admin(func(w http.ResponseWriter, r *http.Request, _ string) error {
		writeJSON(w, http.StatusOK, svc.Users())
		return nil
	}))
	saveUser := func(w http.ResponseWriter, r *http.Request, actor string) error {
		var in userIn
		if err := decode(r, &in); err != nil {
			return err
		}
		in.ID = r.PathValue("id")
		u, err := svc.SaveUser(actor, in.User, in.Password)
		if err != nil {
			return err
		}
		if in.Password != "" || in.Disabled {
			s.sessions.EndSubject(u.ID)
		}
		writeJSON(w, http.StatusOK, u)
		return nil
	}
	mux.HandleFunc("POST /api/users", s.admin(saveUser))
	mux.HandleFunc("PUT /api/users/{id}", s.admin(saveUser))
	mux.HandleFunc("DELETE /api/users/{id}", s.admin(func(w http.ResponseWriter, r *http.Request, actor string) error {
		if err := svc.DeleteUser(actor, r.PathValue("id")); err != nil {
			return err
		}
		s.sessions.EndSubject(r.PathValue("id"))
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))

	// --- overview & settings ---
	mux.HandleFunc("GET /api/overview", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		running, queued := svc.Active()
		pending := 0
		for _, p := range svc.Proposals() {
			if p.State == model.ProposalPending {
				pending++
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"version": s.opt.Version, "data_dir": svc.DataDir(), "running": running, "queued": queued,
			"agents": len(svc.Agents()), "models": len(svc.Models()), "pending_proposals": pending,
			"setup_complete": svc.IsSetUp(), "settings": svc.Settings(), "platform": library.Platform(),
		})
		return nil
	}))
	mux.HandleFunc("GET /api/settings", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		writeJSON(w, http.StatusOK, svc.Settings())
		return nil
	}))
	mux.HandleFunc("PUT /api/settings", s.api(func(w http.ResponseWriter, r *http.Request, actor string) error {
		var in model.Settings
		if err := decode(r, &in); err != nil {
			return err
		}
		out, err := svc.UpdateSettings(actor, in)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	}))
	mux.HandleFunc("GET /api/audit", s.admin(func(w http.ResponseWriter, r *http.Request, _ string) error {
		n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if n <= 0 || n > 1000 {
			n = 200
		}
		out, err := svc.Audit(n)
		if err != nil {
			out = []model.AuditEntry{}
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	}))

	// --- agents ---
	mux.HandleFunc("GET /api/agents", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		writeJSON(w, http.StatusOK, svc.Agents())
		return nil
	}))
	mux.HandleFunc("POST /api/agents", s.api(func(w http.ResponseWriter, r *http.Request, actor string) error {
		var in model.Agent
		if err := decode(r, &in); err != nil {
			return err
		}
		out, err := svc.CreateAgent(actor, in)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusCreated, out)
		return nil
	}))
	mux.HandleFunc("GET /api/agents/{ref}", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		out, err := svc.Agent(model.NormalizeUsername(r.PathValue("ref")))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	}))
	mux.HandleFunc("PUT /api/agents/{ref}", s.api(func(w http.ResponseWriter, r *http.Request, actor string) error {
		var in model.Agent
		if err := decode(r, &in); err != nil {
			return err
		}
		out, err := svc.UpdateAgent(actor, model.NormalizeUsername(r.PathValue("ref")), in)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	}))
	mux.HandleFunc("DELETE /api/agents/{ref}", s.api(func(w http.ResponseWriter, r *http.Request, actor string) error {
		if err := svc.DeleteAgent(actor, model.NormalizeUsername(r.PathValue("ref"))); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))
	mux.HandleFunc("POST /api/agents/{ref}/rotate-token", s.api(func(w http.ResponseWriter, r *http.Request, actor string) error {
		if err := svc.RotateAgentToken(actor, model.NormalizeUsername(r.PathValue("ref"))); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))

	// --- models ---
	type modelIn struct {
		model.ModelProfile
		APIKey *string `json:"api_key"`
	}
	mux.HandleFunc("GET /api/models", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		writeJSON(w, http.StatusOK, svc.Models())
		return nil
	}))
	saveModel := func(w http.ResponseWriter, r *http.Request, actor string) error {
		var in modelIn
		if err := decode(r, &in); err != nil {
			return err
		}
		in.ID = r.PathValue("id")
		out, err := svc.SaveModel(actor, in.ModelProfile, in.APIKey)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	}
	mux.HandleFunc("POST /api/models", s.api(saveModel))
	mux.HandleFunc("PUT /api/models/{id}", s.api(saveModel))
	mux.HandleFunc("DELETE /api/models/{id}", s.api(func(w http.ResponseWriter, r *http.Request, actor string) error {
		if err := svc.DeleteModel(actor, r.PathValue("id")); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))
	mux.HandleFunc("POST /api/models/{id}/test", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		ok, report, err := svc.TestModel(r.Context(), r.PathValue("id"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "report": report})
		return nil
	}))

	// --- threads & messages ---
	mux.HandleFunc("GET /api/threads", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		q := r.URL.Query()
		writeJSON(w, http.StatusOK, svc.Threads(q.Get("agent"), q.Get("archived") == "1"))
		return nil
	}))
	mux.HandleFunc("GET /api/threads/{id}", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		out, err := svc.Thread(r.PathValue("id"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	}))
	mux.HandleFunc("DELETE /api/threads/{id}", s.api(func(w http.ResponseWriter, r *http.Request, actor string) error {
		if err := svc.DeleteThread(actor, r.PathValue("id")); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))
	mux.HandleFunc("POST /api/threads/{id}/archive", s.op(func(w http.ResponseWriter, r *http.Request, actor string) error {
		var in struct {
			Archived bool `json:"archived"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		if err := svc.ArchiveThread(actor, r.PathValue("id"), in.Archived); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))
	mux.HandleFunc("POST /api/messages", s.op(func(w http.ResponseWriter, r *http.Request, _ string) error {
		var in core.SendInput
		if err := decode(r, &in); err != nil {
			return err
		}
		if id := identityOf(r); id.user != "" {
			in.FromName = id.name // shown in the conversation; From stays @user
		}
		m, err := svc.Send(in)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusAccepted, m)
		return nil
	}))
	mux.HandleFunc("POST /api/threads/{tid}/messages/{mid}/cancel", s.op(func(w http.ResponseWriter, r *http.Request, actor string) error {
		if err := svc.CancelMessage(actor, r.PathValue("tid"), r.PathValue("mid")); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))
	mux.HandleFunc("GET /api/threads/{tid}/messages/{mid}/wait", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		res, err := svc.Wait(r.Context(), r.PathValue("tid"), r.PathValue("mid"), "", waitTimeout(r, 60*time.Second))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))

	// --- runs ---
	mux.HandleFunc("GET /api/runs", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if n <= 0 || n > 500 {
			n = 100
		}
		writeJSON(w, http.StatusOK, svc.Runs(r.URL.Query().Get("agent"), n))
		return nil
	}))
	mux.HandleFunc("GET /api/runs/{id}/log", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		text, err := svc.RunLog(r.PathValue("id"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]string{"log": text})
		return nil
	}))
	mux.HandleFunc("POST /api/runs/{id}/stop", s.op(func(w http.ResponseWriter, r *http.Request, actor string) error {
		if err := svc.StopRun(actor, r.PathValue("id")); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))

	// --- tools ---
	lib := svc.Library()
	mux.HandleFunc("GET /api/tools", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		out, err := lib.Tools()
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	}))
	mux.HandleFunc("GET /api/registry", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		reg, err := lib.FetchRegistry(ctx)
		if err != nil {
			return &core.InputError{Msg: err.Error()}
		}
		writeJSON(w, http.StatusOK, map[string]any{"registry": reg, "platform": library.Platform()})
		return nil
	}))
	mux.HandleFunc("POST /api/tools/install", s.api(func(w http.ResponseWriter, r *http.Request, actor string) error {
		var in struct {
			ID              string `json:"id"`
			Path            string `json:"path"`
			AllowUnverified bool   `json:"allow_unverified"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		var (
			m   library.ToolMeta
			err error
		)
		if in.Path != "" {
			m, err = svc.InstallLocalTool(actor, in.Path)
		} else {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
			defer cancel()
			m, err = svc.InstallTool(ctx, actor, in.ID, library.InstallOptions{AllowUnverified: in.AllowUnverified})
			if err != nil {
				err = &core.InputError{Msg: err.Error()}
			}
		}
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, m)
		return nil
	}))
	mux.HandleFunc("DELETE /api/tools/{id}", s.api(func(w http.ResponseWriter, r *http.Request, actor string) error {
		if err := svc.RemoveTool(actor, r.PathValue("id")); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))
	mux.HandleFunc("GET /api/tools/{id}/config", s.admin(func(w http.ResponseWriter, r *http.Request, _ string) error {
		raw, err := lib.ToolConfig(r.PathValue("id"))
		if err != nil {
			return &core.InputError{Msg: err.Error()}
		}
		writeJSON(w, http.StatusOK, map[string]json.RawMessage{"config": raw})
		return nil
	}))
	mux.HandleFunc("PUT /api/tools/{id}/config", s.api(func(w http.ResponseWriter, r *http.Request, actor string) error {
		var in struct {
			Config json.RawMessage `json:"config"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		if err := lib.SetToolConfig(r.PathValue("id"), in.Config); err != nil {
			return &core.InputError{Msg: err.Error()}
		}
		svc.RecordAudit(actor, "tool.config", r.PathValue("id"), "")
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))

	// --- skills ---
	mux.HandleFunc("GET /api/skills", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		out, err := lib.Skills()
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	}))
	mux.HandleFunc("GET /api/skills/{name}", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		sk, err := lib.Skill(r.PathValue("name"), true)
		if err != nil {
			return core.ErrNotFound
		}
		writeJSON(w, http.StatusOK, sk)
		return nil
	}))
	mux.HandleFunc("PUT /api/skills/{name}", s.api(func(w http.ResponseWriter, r *http.Request, actor string) error {
		var in library.Skill
		if err := decode(r, &in); err != nil {
			return err
		}
		in.Name = r.PathValue("name")
		if err := svc.SaveSkill(actor, in); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))
	mux.HandleFunc("DELETE /api/skills/{name}", s.api(func(w http.ResponseWriter, r *http.Request, actor string) error {
		if err := svc.RemoveSkill(actor, r.PathValue("name")); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))

	// --- schedules ---
	mux.HandleFunc("GET /api/schedules", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		writeJSON(w, http.StatusOK, svc.Schedules())
		return nil
	}))
	saveSchedule := func(w http.ResponseWriter, r *http.Request, actor string) error {
		var in model.Schedule
		if err := decode(r, &in); err != nil {
			return err
		}
		in.ID = r.PathValue("id")
		out, err := svc.SaveSchedule(actor, in)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	}
	mux.HandleFunc("POST /api/schedules", s.api(saveSchedule))
	mux.HandleFunc("PUT /api/schedules/{id}", s.api(saveSchedule))
	mux.HandleFunc("DELETE /api/schedules/{id}", s.api(func(w http.ResponseWriter, r *http.Request, actor string) error {
		if err := svc.DeleteSchedule(actor, r.PathValue("id")); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))
	mux.HandleFunc("POST /api/schedules/{id}/run", s.op(func(w http.ResponseWriter, r *http.Request, actor string) error {
		m, err := svc.RunScheduleNow(actor, r.PathValue("id"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusAccepted, m)
		return nil
	}))

	// --- channels ---
	type channelIn struct {
		model.Channel
		Secret  *string `json:"secret"`
		Secret2 *string `json:"secret2"`
	}
	mux.HandleFunc("GET /api/channels", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		writeJSON(w, http.StatusOK, svc.Channels())
		return nil
	}))
	saveChannel := func(w http.ResponseWriter, r *http.Request, actor string) error {
		var in channelIn
		if err := decode(r, &in); err != nil {
			return err
		}
		in.ID = r.PathValue("id")
		out, err := svc.SaveChannel2(actor, in.Channel, in.Secret, in.Secret2)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	}
	mux.HandleFunc("POST /api/channels", s.api(saveChannel))
	mux.HandleFunc("PUT /api/channels/{id}", s.api(saveChannel))
	mux.HandleFunc("DELETE /api/channels/{id}", s.api(func(w http.ResponseWriter, r *http.Request, actor string) error {
		if err := svc.DeleteChannel(actor, r.PathValue("id")); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))
	mux.HandleFunc("POST /api/channels/{id}/allow", s.api(func(w http.ResponseWriter, r *http.Request, actor string) error {
		var in struct {
			Sender string `json:"sender"`
			Allow  bool   `json:"allow"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		if err := svc.AllowSender(actor, r.PathValue("id"), in.Sender, in.Allow); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))

	// --- proposals ---
	mux.HandleFunc("GET /api/proposals", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		writeJSON(w, http.StatusOK, svc.Proposals())
		return nil
	}))
	decide := func(approve bool) handler {
		return func(w http.ResponseWriter, r *http.Request, actor string) error {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
			defer cancel()
			p, err := svc.DecideProposal(ctx, actor, r.PathValue("id"), approve)
			if err != nil && p.ID == "" {
				return err
			}
			writeJSON(w, http.StatusOK, p)
			return nil
		}
	}
	mux.HandleFunc("POST /api/proposals/{id}/approve", s.api(decide(true)))
	mux.HandleFunc("POST /api/proposals/{id}/reject", s.api(decide(false)))

	// --- workflows ---
	mux.HandleFunc("GET /api/workflows", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		writeJSON(w, http.StatusOK, svc.Workflows())
		return nil
	}))
	mux.HandleFunc("GET /api/workflows/{id}", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		wf, err := svc.Workflow(r.PathValue("id"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, wf)
		return nil
	}))
	saveWF := func(w http.ResponseWriter, r *http.Request, actor string) error {
		var in model.Workflow
		if err := decode(r, &in); err != nil {
			return err
		}
		in.ID = r.PathValue("id")
		out, err := svc.SaveWorkflow(actor, in)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	}
	mux.HandleFunc("POST /api/workflows", s.api(saveWF))
	mux.HandleFunc("PUT /api/workflows/{id}", s.api(saveWF))
	mux.HandleFunc("DELETE /api/workflows/{id}", s.api(func(w http.ResponseWriter, r *http.Request, actor string) error {
		if err := svc.DeleteWorkflow(actor, r.PathValue("id")); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))
	mux.HandleFunc("POST /api/workflows/{id}/run", s.op(func(w http.ResponseWriter, r *http.Request, actor string) error {
		var in struct {
			Input string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		run, err := svc.RunWorkflow(actor, r.PathValue("id"), in.Input)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusAccepted, run)
		return nil
	}))
	mux.HandleFunc("GET /api/workflow-runs", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		writeJSON(w, http.StatusOK, svc.WorkflowRuns(r.URL.Query().Get("workflow"), 100))
		return nil
	}))
	mux.HandleFunc("GET /api/workflow-runs/{id}", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		run, err := svc.WorkflowRun(r.PathValue("id"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, run)
		return nil
	}))
	mux.HandleFunc("POST /api/workflow-runs/{id}/cancel", s.op(func(w http.ResponseWriter, r *http.Request, actor string) error {
		if err := svc.CancelWorkflowRun(actor, r.PathValue("id")); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))

	// --- live runs & command approvals ---
	mux.HandleFunc("GET /api/live", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		writeJSON(w, http.StatusOK, svc.Live())
		return nil
	}))
	mux.HandleFunc("GET /api/approvals", s.api(func(w http.ResponseWriter, r *http.Request, _ string) error {
		writeJSON(w, http.StatusOK, svc.Approvals(r.URL.Query().Get("pending") == "1"))
		return nil
	}))
	decideCmd := func(allow bool) handler {
		return func(w http.ResponseWriter, r *http.Request, actor string) error {
			var in struct {
				Reason string `json:"reason"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			a, err := svc.DecideApproval(actor, r.PathValue("id"), allow, in.Reason)
			if err != nil {
				return err
			}
			writeJSON(w, http.StatusOK, a)
			return nil
		}
	}
	mux.HandleFunc("POST /api/approvals/{id}/allow", s.api(decideCmd(true)))
	mux.HandleFunc("POST /api/approvals/{id}/deny", s.api(decideCmd(false)))

	// --- live events ---
	mux.HandleFunc("GET /api/events", s.api(s.events))

	// --- webhooks (authenticated by the channel's secret) ---
	mux.HandleFunc("POST /hooks/{id}", s.webhook)
	mux.HandleFunc("GET /hooks/whatsapp/{id}", s.whatsappHook)
	mux.HandleFunc("POST /hooks/whatsapp/{id}", s.whatsappHook)

	mux.Handle("/", s.static())
	return mux
}

func waitTimeout(r *http.Request, max time.Duration) time.Duration {
	sec, _ := strconv.Atoi(r.URL.Query().Get("timeout"))
	d := time.Duration(sec) * time.Second
	if d <= 0 || d > max {
		d = max
	}
	return d
}

// login signs a browser in with the admin password or the local admin token.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	if ok, wait := s.limiter.Allow(ip); !ok {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": fmt.Sprintf("too many attempts; try again in %d seconds", int(wait.Seconds())+1)})
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Token    string `json:"token"`
		Code     string `json:"code"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	subject, actor, ok := "", "owner (web)", false
	switch {
	case in.Code != "":
		ok = s.useLoginCode(in.Code)
	case in.Token != "":
		ok = authTokenEqual(in.Token, s.token)
	case in.Password != "":
		if in.Username == "" {
			in.Username = "admin"
		}
		var u model.User
		if u, ok = s.svc.Authenticate(in.Username, in.Password); ok {
			subject, actor = u.ID, "user:"+u.Username
		}
	}
	s.limiter.Result(ip, ok)
	if !ok {
		s.svc.RecordAudit("anonymous", "login.fail", ip, in.Username)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "wrong username, password or token"})
		return
	}
	id := s.sessions.New(subject)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: id, Path: "/", HttpOnly: true, Secure: r.TLS != nil,
		SameSite: http.SameSiteStrictMode, MaxAge: int((7 * 24 * time.Hour).Seconds())})
	s.svc.RecordAudit(actor, "login", ip, map[bool]string{true: "password", false: "token"}[in.Password != ""])
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// events streams server-sent events.
func (s *Server) events(w http.ResponseWriter, r *http.Request, _ string) error {
	fl, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming unsupported")
	}
	ch, unsub := s.svc.Events().Subscribe()
	defer unsub()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}
