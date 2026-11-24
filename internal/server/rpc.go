package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/internal/auth"
	"github.com/ranjanlive/zpilot/internal/core"
	"github.com/ranjanlive/zpilot/internal/model"
)

func authTokenEqual(a, b string) bool { return auth.TokenEqual(a, b) }

type agentHandler func(w http.ResponseWriter, r *http.Request, a *model.Agent, session string) error

// rpcRoutes serves the collaboration tools. The listener is loopback-only and
// every call carries the calling agent's own token, so an agent can only ever
// act as itself; everything it may do is decided by core.
func (s *Server) rpcRoutes() http.Handler {
	mux := http.NewServeMux()
	wrap := func(h agentHandler) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !isLoopbackHost(remoteIP(r)) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "loopback only"})
				return
			}
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			a, ok := s.svc.AgentByToken(tok)
			if !ok {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid agent token (the agent may have been deleted or its token rotated)"})
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			if err := h(w, r, a, r.Header.Get("X-ZPilot-Session")); err != nil {
				writeErr(w, err)
			}
		}
	}
	mux.HandleFunc("GET /rpc/agents", wrap(func(w http.ResponseWriter, r *http.Request, a *model.Agent, _ string) error {
		writeJSON(w, http.StatusOK, s.svc.Directory(a))
		return nil
	}))
	send := func(mode string) agentHandler {
		return func(w http.ResponseWriter, r *http.Request, a *model.Agent, session string) error {
			var in struct {
				To      string `json:"to"`
				Message string `json:"message"`
			}
			if err := decode(r, &in); err != nil {
				return err
			}
			m, err := s.svc.AgentSend(core.AgentRequest{Caller: a, Session: session, To: in.To, Text: in.Message, Mode: mode})
			if err != nil {
				return err
			}
			writeJSON(w, http.StatusAccepted, map[string]string{"thread_id": m.ThreadID, "message_id": m.ID, "to": m.To})
			return nil
		}
	}
	mux.HandleFunc("POST /rpc/ask", wrap(send(model.ModeAsk)))
	mux.HandleFunc("POST /rpc/tell", wrap(send(model.ModeTell)))
	mux.HandleFunc("GET /rpc/wait", wrap(func(w http.ResponseWriter, r *http.Request, a *model.Agent, _ string) error {
		q := r.URL.Query()
		res, err := s.svc.Wait(r.Context(), q.Get("thread"), q.Get("id"), model.AgentAddr(a.Username), waitTimeout(r, 60*time.Second))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))
	mux.HandleFunc("POST /rpc/manage/status", wrap(func(w http.ResponseWriter, r *http.Request, a *model.Agent, _ string) error {
		var in struct {
			What string `json:"what"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		out, err := s.svc.ManageStatus(ctx, a, in.What)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	}))
	mux.HandleFunc("POST /rpc/manage/propose", wrap(func(w http.ResponseWriter, r *http.Request, a *model.Agent, session string) error {
		var in core.ProposeInput
		if err := decode(r, &in); err != nil {
			return err
		}
		in.Caller, in.Session = a, session
		p, err := s.svc.Propose(in)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusCreated, p)
		return nil
	}))
	// --- engine bridge (one token per run) ---
	bridge := func(h func(w http.ResponseWriter, r *http.Request, tok string) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !isLoopbackHost(remoteIP(r)) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "loopback only"})
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
			if err := h(w, r, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); err != nil {
				writeErr(w, err)
			}
		}
	}
	mux.HandleFunc("POST /rpc/bridge/live", bridge(func(w http.ResponseWriter, r *http.Request, tok string) error {
		var evs []core.LiveEvent
		if err := decode(r, &evs); err != nil {
			return err
		}
		if err := s.svc.BridgeLive(tok, evs); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}))
	mux.HandleFunc("POST /rpc/bridge/approval", bridge(func(w http.ResponseWriter, r *http.Request, tok string) error {
		var in struct{ Tool, Action, Detail string }
		if err := decode(r, &in); err != nil {
			return err
		}
		id, err := s.svc.BridgeApproval(tok, in.Tool, in.Action, in.Detail)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": id})
		return nil
	}))
	mux.HandleFunc("POST /rpc/bridge/approval/wait", bridge(func(w http.ResponseWriter, r *http.Request, tok string) error {
		var in struct{ ID string }
		if err := decode(r, &in); err != nil {
			return err
		}
		d, err := s.svc.BridgeWaitApproval(r.Context(), tok, in.ID, 50*time.Second)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, d)
		return nil
	}))
	return mux
}

// webhook accepts {"text": "...", "sender": "...", "conversation": "...", "wait": true}
// with "Authorization: Bearer <channel secret>". With wait it returns the
// agent's reply (up to 120 s); otherwise it returns the ids to poll.
func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ch, err := s.svc.Channel(id)
	if err != nil || ch.Kind != model.ChannelWebhook || !ch.Enabled {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	ip := remoteIP(r)
	if ok, _ := s.limiter.Allow("hook:" + ip); !ok {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed attempts"})
		return
	}
	secret, err := s.svc.ChannelSecret(id)
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	good := err == nil && auth.TokenEqual(tok, secret)
	s.limiter.Result("hook:"+ip, good)
	if !good {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid secret"})
		return
	}
	var in struct {
		Text         string `json:"text"`
		Sender       string `json:"sender"`
		Conversation string `json:"conversation"`
		Wait         bool   `json:"wait"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 512<<10)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if in.Sender == "" {
		in.Sender = "webhook"
	}
	if in.Conversation == "" {
		in.Conversation = "default"
	}
	m, allowed, err := s.svc.ChannelInbound(core.Inbound{ChannelID: id, ChatID: in.Conversation, SenderID: in.Sender, SenderName: in.Sender, Text: in.Text})
	if err != nil {
		writeErr(w, err)
		return
	}
	if !allowed || m == nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "sender not allowed"})
		return
	}
	if !in.Wait {
		writeJSON(w, http.StatusAccepted, map[string]string{"thread_id": m.ThreadID, "message_id": m.ID})
		return
	}
	res, err := s.svc.Wait(r.Context(), m.ThreadID, m.ID, "", 120*time.Second)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"thread_id": m.ThreadID, "message_id": m.ID, "done": res.Done, "kind": res.Kind, "text": res.Text})
}

// WhatsApp receives inbound WhatsApp deliveries (implemented by package channels).
type WhatsApp interface {
	WhatsAppInbound(ctx context.Context, ch model.Channel, body []byte) error
}

// SetWhatsApp connects the WhatsApp adapter.
func (s *Server) SetWhatsApp(w WhatsApp) { s.wa = w }

// whatsappHook serves Meta's webhook: GET verifies the subscription, POST
// delivers messages and must carry a valid X-Hub-Signature-256.
func (s *Server) whatsappHook(w http.ResponseWriter, r *http.Request) {
	ch, err := s.svc.Channel(r.PathValue("id"))
	if err != nil || ch.Kind != model.ChannelWhatsApp || !ch.Enabled {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		if c, ok := waVerify(ch.Channel, q.Get("hub.mode"), q.Get("hub.verify_token"), q.Get("hub.challenge")); ok {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, c)
			return
		}
		http.Error(w, "verification failed", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	secret, _ := s.svc.ChannelSecret2(ch.ID)
	if !waSignature(secret, body, r.Header.Get("X-Hub-Signature-256")) {
		s.svc.RecordAudit("channel:"+ch.Name, "whatsapp.bad_signature", remoteIP(r), "")
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusOK) // acknowledge fast; Meta retries slow endpoints
	if s.wa != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_ = s.wa.WhatsAppInbound(ctx, ch.Channel, body)
		}()
	}
}
