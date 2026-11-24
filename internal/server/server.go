// Package server is ZPilot's HTTP layer:
//
//   - the owner API and the embedded dashboard on the main listener
//     (127.0.0.1:7700 by default; any address with --listen for VPS use)
//   - webhook endpoints (/hooks/<channel id>) on the main listener
//   - the agent RPC on a separate loopback-only listener, used by the
//     collaboration tools that agents run (ask_agent, tell_agent, ...)
//
// Owner requests authenticate with the local admin token (Authorization:
// Bearer) or a browser session cookie obtained by logging in with that token
// or the admin password. Cookie requests that change state must carry
// X-Requested-With: zpilot, which a cross-site page cannot send without a CORS
// preflight we never grant.
package server

import (
	"context"
	"crypto/tls"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ranjanlive/zpilot/internal/auth"
	"github.com/ranjanlive/zpilot/internal/core"
	"github.com/ranjanlive/zpilot/internal/model"
	"github.com/ranjanlive/zpilot/internal/store"
)

//go:embed web
var webFS embed.FS

// Options configure the server.
type Options struct {
	Listen       string // main listener, e.g. 127.0.0.1:7700
	TLSCert      string
	TLSKey       string
	InsecureHTTP bool // allow plain HTTP on a non-loopback address (behind a TLS proxy)
	Version      string
	Log          func(string, ...any)
}

// Endpoint is written to <data>/endpoint.json for the CLI.
type Endpoint struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
	RPC   string `json:"rpc"`
}

// Server serves the API.
type Server struct {
	svc      *core.Service
	opt      Options
	token    string
	sessions *auth.Sessions
	limiter  *auth.Limiter
	loopback bool // the main listener is loopback-only
	main     *http.Server
	rpc      *http.Server
	url      string
	wa       WhatsApp

	codeMu sync.Mutex
	codes  map[string]time.Time // one-time login codes -> expiry
}

// New prepares a server. Start begins listening.
func New(svc *core.Service, opt Options) (*Server, error) {
	if opt.Log == nil {
		opt.Log = func(string, ...any) {}
	}
	host, _, err := net.SplitHostPort(opt.Listen)
	if err != nil {
		return nil, fmt.Errorf("--listen %q: %w", opt.Listen, err)
	}
	s := &Server{svc: svc, opt: opt, token: randomToken(), sessions: auth.NewSessions(), limiter: auth.NewLimiter(), codes: map[string]time.Time{}}
	s.loopback = isLoopbackHost(host)
	tlsOn := opt.TLSCert != "" || opt.TLSKey != ""
	if tlsOn && (opt.TLSCert == "" || opt.TLSKey == "") {
		return nil, errors.New("give both --tls-cert and --tls-key")
	}
	if !s.loopback {
		if !svc.HasAdminPassword() {
			return nil, errors.New("listening on a non-loopback address requires an admin password: run 'zpilot admin password' first")
		}
		if !tlsOn && !opt.InsecureHTTP {
			return nil, errors.New("listening on a non-loopback address requires TLS (--tls-cert/--tls-key), or --insecure-http when a TLS reverse proxy sits in front")
		}
	}
	return s, nil
}

// Start opens both listeners and writes endpoint.json.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.opt.Listen)
	if err != nil {
		return err
	}
	rpcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		ln.Close()
		return err
	}
	scheme := "http"
	if s.opt.TLSCert != "" {
		scheme = "https"
	}
	addr := ln.Addr().(*net.TCPAddr)
	host := s.opt.Listen[:strings.LastIndex(s.opt.Listen, ":")]
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	s.url = fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(strings.Trim(host, "[]"), fmt.Sprint(addr.Port)))
	rpcURL := "http://" + rpcLn.Addr().String()
	s.svc.SetDaemonURL(rpcURL)

	s.main = &http.Server{Handler: s.secure(s.routes()), ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 2 * time.Minute}
	s.rpc = &http.Server{Handler: s.rpcRoutes(), ReadHeaderTimeout: 15 * time.Second}
	go func() {
		var err error
		if s.opt.TLSCert != "" {
			s.main.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			err = s.main.ServeTLS(ln, s.opt.TLSCert, s.opt.TLSKey)
		} else {
			err = s.main.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.opt.Log("http: %v", err)
		}
	}()
	go func() {
		if err := s.rpc.Serve(rpcLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.opt.Log("rpc: %v", err)
		}
	}()
	return store.WriteJSONAtomic(filepath.Join(s.svc.DataDir(), "endpoint.json"),
		Endpoint{URL: s.url, Token: s.token, PID: os.Getpid(), RPC: rpcURL})
}

// URL is the dashboard base URL.
func (s *Server) URL() string { return s.url }

// LoginURL opens the dashboard and signs the browser in with a one-time code.
func (s *Server) LoginURL() string { return s.url + "/#code=" + s.MintLoginCode() }

// MintLoginCode returns a single-use sign-in code valid for two minutes, so
// links that open the dashboard never contain the long-lived admin token.
func (s *Server) MintLoginCode() string {
	code := randomToken()[:32]
	s.codeMu.Lock()
	defer s.codeMu.Unlock()
	now := time.Now()
	for c, exp := range s.codes {
		if now.After(exp) {
			delete(s.codes, c)
		}
	}
	s.codes[code] = now.Add(2 * time.Minute)
	return code
}

// useLoginCode consumes a code; it reports whether it was valid.
func (s *Server) useLoginCode(code string) bool {
	s.codeMu.Lock()
	defer s.codeMu.Unlock()
	for c, exp := range s.codes {
		if auth.TokenEqual(c, code) {
			delete(s.codes, c)
			return time.Now().Before(exp)
		}
	}
	return false
}

// Shutdown stops both listeners and removes endpoint.json.
func (s *Server) Shutdown(ctx context.Context) {
	if s.main != nil {
		_ = s.main.Shutdown(ctx)
	}
	if s.rpc != nil {
		_ = s.rpc.Shutdown(ctx)
	}
	_ = os.Remove(filepath.Join(s.svc.DataDir(), "endpoint.json"))
}

// ---- middleware --------------------------------------------------------

func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		// DNS-rebinding guard: a loopback daemon only answers to loopback names.
		if s.loopback {
			host := r.Host
			if hh, _, err := net.SplitHostPort(host); err == nil {
				host = hh
			}
			if !isLoopbackHost(host) {
				http.Error(w, "forbidden host", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

const sessionCookie = "zpilot_session"

// identity is who is calling the owner API.
type identity struct {
	actor string // for the audit log
	name  string // display name for messages
	role  string
	user  string // user id ("" = local admin token)
}

// who authenticates a request: the local admin token (CLI), or a browser
// session that belongs to the token owner or to a user account.
func (s *Server) who(r *http.Request) (identity, bool) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if auth.TokenEqual(strings.TrimPrefix(h, "Bearer "), s.token) {
			return identity{actor: "owner (cli)", name: "Owner", role: model.RoleAdmin}, true
		}
		return identity{}, false
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return identity{}, false
	}
	subject, ok := s.sessions.Subject(c.Value)
	if !ok {
		return identity{}, false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if r.Header.Get("X-Requested-With") != "zpilot" {
			return identity{}, false
		}
		if o := r.Header.Get("Origin"); o != "" {
			u, err := url.Parse(o)
			if err != nil || u.Host != r.Host {
				return identity{}, false
			}
		}
	}
	if subject == "" {
		return identity{actor: "owner (web)", name: "Owner", role: model.RoleAdmin}, true
	}
	u, ok := s.svc.UserByID(subject)
	if !ok {
		return identity{}, false
	}
	return identity{actor: "user:" + u.Username, name: nonEmptyStr(u.Name, u.Username), role: u.Role, user: u.ID}, true
}

func nonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

type handler func(w http.ResponseWriter, r *http.Request, actor string) error

// api wraps an owner endpoint. Reading needs the viewer role; anything else
// needs admin unless the route is registered with op (operator).
func (s *Server) api(h handler) http.HandlerFunc {
	return s.guard("", h)
}

// op marks an endpoint that operators may use (talking to agents, stopping runs).
func (s *Server) op(h handler) http.HandlerFunc { return s.guard(model.RoleOperator, h) }

// admin forces the admin role even for reads (secrets, audit, users).
func (s *Server) admin(h handler) http.HandlerFunc { return s.guard(model.RoleAdmin, h) }

func (s *Server) guard(need string, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.who(r)
		if !ok {
			// Drain a small body so the client reads our 401 instead of a reset.
			_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
			return
		}
		if need == "" {
			need = model.RoleAdmin
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				need = model.RoleViewer
			}
		}
		if model.RoleRank(id.role) < model.RoleRank(need) {
			_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "your role (" + id.role + ") cannot do this"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		ctx := context.WithValue(r.Context(), identityKey{}, id)
		if err := h(w, r.WithContext(ctx), id.actor); err != nil {
			writeErr(w, err)
		}
	}
}

type identityKey struct{}

func identityOf(r *http.Request) identity {
	id, _ := r.Context().Value(identityKey{}).(identity)
	return id
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var in *core.InputError
	var fb *core.ForbiddenError
	var mbe *http.MaxBytesError
	switch {
	case errors.Is(err, core.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case errors.As(err, &in):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": in.Msg})
	case errors.As(err, &fb):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": fb.Msg})
	case errors.As(err, &mbe):
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request too large"})
	case errors.Is(err, context.Canceled):
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return &core.InputError{Msg: "request body is empty"}
		}
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return err
		}
		return &core.InputError{Msg: "invalid JSON: " + err.Error()}
	}
	return nil
}

// ---- static UI ---------------------------------------------------------

func (s *Server) static() http.Handler {
	sub, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// ---- helpers -----------------------------------------------------------

func isLoopbackHost(h string) bool {
	h = strings.Trim(h, "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func remoteIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func randomToken() string { return model.NewToken() }
