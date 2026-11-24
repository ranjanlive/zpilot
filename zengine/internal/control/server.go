package control

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/engine"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
)

const maxUpload = 200 << 20 // bytes

// Options configure a Server.
type Options struct {
	// Version is reported in /api/state.
	Version string
	// UI serves the web interface at "/" (nil = API only).
	UI http.Handler
	// OnShutdown is called when a client asks the engine process to end.
	OnShutdown func()
}

// Server exposes one engine over HTTP on the loopback interface.
type Server struct {
	eng   *engine.Engine
	opts  Options
	ln    net.Listener
	srv   *http.Server
	token string

	streams  atomic.Int64
	lastSeen atomic.Int64 // unix nanoseconds of the last authenticated request
	uploadN  atomic.Int64
}

// Serve starts the server on a random loopback port and publishes the
// endpoint and token in the session lock so clients can find it.
func Serve(eng *engine.Engine, opts Options) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start control server: %w", err)
	}
	var tok [24]byte
	if _, err := rand.Read(tok[:]); err != nil {
		ln.Close()
		return nil, err
	}
	s := &Server{eng: eng, opts: opts, ln: ln, token: hex.EncodeToString(tok[:])}
	s.touch()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.api(s.handleState))
	mux.HandleFunc("GET /api/events", s.api(s.handleEvents))
	mux.HandleFunc("GET /api/stream", s.api(s.handleStream))
	mux.HandleFunc("POST /api/prompt", s.api(s.handlePrompt))
	mux.HandleFunc("POST /api/note", s.api(s.handleNote))
	mux.HandleFunc("POST /api/command", s.api(s.handleCommand))
	mux.HandleFunc("POST /api/stop", s.api(s.handleStop))
	mux.HandleFunc("POST /api/resume", s.api(s.handleResume))
	mux.HandleFunc("POST /api/shutdown", s.api(s.handleShutdown))
	mux.HandleFunc("POST /api/upload", s.api(s.handleUpload))
	mux.HandleFunc("GET /api/blob/{id}", s.api(s.handleBlob))
	if opts.UI != nil {
		mux.Handle("/", s.guardHost(opts.UI))
	}
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()

	if lock := eng.Lock(); lock != nil {
		lock.SetEndpoint(s.Addr(), s.token)
	}
	return s, nil
}

// Addr is the host:port the server listens on.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Token is the secret clients must present.
func (s *Server) Token() string { return s.token }

// URL returns the browser URL for the web UI, carrying the token in the
// fragment (fragments are never sent to servers or logged).
func (s *Server) URL() string { return "http://" + s.Addr() + "/#token=" + s.token }

// Close shuts the server down.
func (s *Server) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
	_ = s.srv.Close()
}

// Clients reports how many live streams are attached and how long ago the
// last authenticated request was.
func (s *Server) Clients() (streams int, sinceLast time.Duration) {
	return int(s.streams.Load()), time.Since(time.Unix(0, s.lastSeen.Load()))
}

func (s *Server) touch() { s.lastSeen.Store(time.Now().UnixNano()) }

// ---- security ----

// allowedHost reports whether the Host header names this server. Rejecting
// anything else blocks DNS-rebinding, where a hostile page makes the browser
// resolve its own name to 127.0.0.1.
func (s *Server) allowedHost(host string) bool {
	_, port, err := net.SplitHostPort(s.Addr())
	if err != nil {
		return false
	}
	for _, h := range []string{"127.0.0.1:" + port, "localhost:" + port, "[::1]:" + port} {
		if strings.EqualFold(host, h) {
			return true
		}
	}
	return false
}

func (s *Server) guardHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowedHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authorized(r *http.Request) bool {
	tok := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok = strings.TrimPrefix(h, "Bearer ")
	} else {
		tok = r.URL.Query().Get("token") // EventSource cannot set headers
	}
	return subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) == 1
}

// api wraps a handler with host, origin and token checks.
func (s *Server) api(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.allowedHost(r.Host) {
			writeErr(w, http.StatusForbidden, "forbidden host")
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			u, err := url.Parse(o)
			if err != nil || !s.allowedHost(u.Host) {
				writeErr(w, http.StatusForbidden, "forbidden origin")
				return
			}
		}
		if !s.authorized(r) {
			writeErr(w, http.StatusUnauthorized, "missing or wrong token")
			return
		}
		s.touch()
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return false
	}
	return true
}

// ---- handlers ----

func (s *Server) state() StateResponse {
	st, detail := s.eng.State()
	meta := s.eng.Store().Meta()
	role := ""
	if li := session.ReadLock(s.eng.Store().Dir); li != nil {
		role = li.Role
	}
	return StateResponse{
		Session: s.eng.SessionID(), Title: meta.Title, Workspace: s.eng.Workspace(), Model: s.eng.Model(),
		State: st, Detail: detail, Counters: s.eng.Counters(), CompactAt: s.eng.CompactAtTokens(),
		Tasks: s.eng.RunningTasks(), LastSeq: int64(s.eng.Store().Len()), PID: os.Getpid(), Role: role, Version: s.opts.Version,
	}
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.state())
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	evs := s.eng.Store().Since(since)
	if evs == nil {
		evs = []session.Event{}
	}
	writeJSON(w, http.StatusOK, evs)
}

// handleStream is a Server-Sent Events feed: durable log events (replayed
// from `since`, then live) plus transient live events. A reconnecting client
// passes the last seq it saw, via ?since= or Last-Event-ID, and misses nothing.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		if n, err := strconv.ParseInt(id, 10, 64); err == nil && n > since {
			since = n
		}
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	s.streams.Add(1)
	defer s.streams.Add(-1)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	events := make(chan session.Event, 64)
	sub := s.eng.Store().Subscribe(since)
	go func() {
		defer close(events)
		for {
			ev, err := sub.Next(ctx)
			if err != nil {
				return
			}
			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	live, stopLive := s.eng.SubscribeLive()
	defer stopLive()

	send := func(name string, id int64, v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return true
		}
		if id > 0 {
			fmt.Fprintf(w, "id: %d\n", id)
		}
		_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
		fl.Flush()
		return err == nil
	}
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case ev, ok := <-events:
			if !ok || !send("event", ev.Seq, ev) {
				return
			}
		case l, ok := <-live:
			if !ok || !send("live", 0, l) {
				return
			}
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
			s.touch()
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	var req PromptRequest
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Text) == "" && len(req.Attachments) == 0 {
		writeErr(w, http.StatusBadRequest, "the prompt is empty")
		return
	}
	if err := s.eng.Submit(engine.Prompt{Text: req.Text, Attachments: req.Attachments}); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

func (s *Server) handleNote(w http.ResponseWriter, r *http.Request) {
	var req TextRequest
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeErr(w, http.StatusBadRequest, "the note is empty")
		return
	}
	if err := s.eng.Note(req.Text, false); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "noted"})
}

func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	var req CommandRequest
	if !decode(w, r, &req) {
		return
	}
	res := s.eng.HandleCommand(r.Context(), req.Line)
	writeJSON(w, http.StatusOK, res)
	if res.Shutdown && s.opts.OnShutdown != nil {
		go s.opts.OnShutdown()
	}
}

func (s *Server) handleStop(w http.ResponseWriter, _ *http.Request) {
	s.eng.Stop()
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopping"})
}

func (s *Server) handleResume(w http.ResponseWriter, _ *http.Request) {
	s.eng.Resume()
	writeJSON(w, http.StatusOK, map[string]string{"status": "resuming"})
}

func (s *Server) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "shutting down"})
	if s.opts.OnShutdown != nil {
		go s.opts.OnShutdown()
	}
}

// handleUpload stores an attachment (raw body, ?name=file.png) in the home
// scratchpad, where the agent may read it, and returns its path.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.URL.Query().Get("name"))
	if name == "" || name == "." || name == string(filepath.Separator) {
		writeErr(w, http.StatusBadRequest, "?name= is required")
		return
	}
	dir := filepath.Join(s.eng.HomeScratch(), "uploads", s.eng.SessionID())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	path := filepath.Join(dir, fmt.Sprintf("%d-%s", s.uploadN.Add(1), sanitizeName(name)))
	f, err := os.Create(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, maxUpload))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("upload exceeds %d MB", maxUpload>>20))
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, UploadResponse{Path: path, Size: n})
}

// handleBlob serves an attachment stored in the session so UIs can show
// images. Only image types are rendered inline; everything else downloads, and
// nosniff stops a browser from guessing a more dangerous type.
func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	data, err := s.eng.Store().Load(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such attachment")
		return
	}
	ct := http.DetectContentType(data)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if strings.HasPrefix(ct, "image/") && ct != "image/svg+xml" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment")
	}
	_, _ = w.Write(data)
}

func sanitizeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}
