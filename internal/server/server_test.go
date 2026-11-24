package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"github.com/ranjanlive/zpilot/internal/core"
	"github.com/ranjanlive/zpilot/internal/model"
)

type nopEngine struct{}

func (nopEngine) Run(context.Context, core.RunSpec) core.RunResult {
	return core.RunResult{Answer: "ok"}
}

func setup(t *testing.T) (*Server, *core.Service) {
	t.Helper()
	svc, err := core.Open(core.Config{DataDir: t.TempDir(), Exe: "x", Engine: nopEngine{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	srv, err := New(svc, Options{Listen: "127.0.0.1:0", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	svc.Start()
	key := "k"
	svc.SaveModel("t", model.ModelProfile{Name: "m", Provider: "gemini", Model: "x"}, &key)
	svc.CreateAgent("t", model.Agent{Username: "helper", Name: "Helper"})
	for _, u := range []struct{ name, role string }{{"vera", model.RoleViewer}, {"otto", model.RoleOperator}, {"ada", model.RoleAdmin}} {
		if _, err := svc.SaveUser("t", model.User{Username: u.name, Role: u.role}, "password-"+u.name); err != nil {
			t.Fatal(err)
		}
	}
	return srv, svc
}

type client struct {
	t    *testing.T
	base string
	c    *http.Client
}

func login(t *testing.T, srv *Server, user string) *client {
	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: srv.URL(), c: &http.Client{Jar: jar}}
	if code := c.do("POST", "/api/login", `{"username":"`+user+`","password":"password-`+user+`"}`); code != 200 {
		t.Fatalf("login %s: %d", user, code)
	}
	return c
}

func (c *client) do(method, path, body string) int {
	req, _ := http.NewRequest(method, c.base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "zpilot")
	resp, err := c.c.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestRoles(t *testing.T) {
	srv, _ := setup(t)
	msg := `{"to":"helper","text":"hi"}`
	cases := []struct {
		user, method, path, body string
		want                     int
	}{
		{"vera", "GET", "/api/agents", "", 200},
		{"vera", "POST", "/api/messages", msg, 403},
		{"vera", "GET", "/api/audit", "", 403},
		{"otto", "POST", "/api/messages", msg, 202},
		{"otto", "PUT", "/api/settings", `{}`, 403},
		{"otto", "POST", "/api/agents", `{"username":"x2","name":"X"}`, 403},
		{"otto", "GET", "/api/users", "", 403},
		{"ada", "GET", "/api/users", "", 200},
		{"ada", "POST", "/api/agents", `{"username":"x2","name":"X"}`, 201},
	}
	sessions := map[string]*client{}
	for _, c := range cases {
		cl := sessions[c.user]
		if cl == nil {
			cl = login(t, srv, c.user)
			sessions[c.user] = cl
		}
		if got := cl.do(c.method, c.path, c.body); got != c.want {
			t.Errorf("%s %s %s: got %d want %d", c.user, c.method, c.path, got, c.want)
		}
	}
}

func TestLoginFailuresAndLastAdmin(t *testing.T) {
	srv, svc := setup(t)
	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: srv.URL(), c: &http.Client{Jar: jar}}
	if code := c.do("POST", "/api/login", `{"username":"ada","password":"wrong-password"}`); code != 401 {
		t.Fatalf("wrong password: %d", code)
	}
	var ada model.User
	for _, u := range svc.Users() {
		if u.Username == "ada" {
			ada = u
		}
	}
	ada.Role = model.RoleViewer
	if _, err := svc.SaveUser("t", ada, ""); err == nil {
		t.Fatal("demoted the last admin")
	}
	b, _ := json.Marshal(svc.Users())
	if strings.Contains(string(b), "pbkdf2") {
		t.Fatal("password hashes exposed")
	}
}
