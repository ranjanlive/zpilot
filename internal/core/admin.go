package core

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/internal/model"
	"github.com/ranjanlive/zpilot/internal/platform"
	"github.com/ranjanlive/zpilot/internal/profile"
)

// SetAdminPassword sets the password of the built-in "admin" account,
// creating it if needed; "" disables that account.
func (s *Service) SetAdminPassword(actor, pw string) error {
	s.st.Mu.Lock()
	var cur *model.User
	for _, u := range s.st.S.Users {
		if u.Username == "admin" {
			cur = u
		}
	}
	if pw == "" {
		defer s.st.Mu.Unlock()
		if cur == nil {
			return nil
		}
		cur.Disabled = true
		s.st.Audit(actor, "user.disable", "admin", "")
		return s.st.Save()
	}
	u := model.User{Username: "admin", Name: "Administrator", Role: model.RoleAdmin}
	if cur != nil {
		u = *cur
		u.Role, u.Disabled = model.RoleAdmin, false
	}
	s.st.Mu.Unlock()
	_, err := s.SaveUser(actor, u, pw)
	return err
}

// HasAdminPassword reports whether an enabled admin can sign in with a password.
func (s *Service) HasAdminPassword() bool {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	for _, u := range s.st.S.Users {
		if !u.Disabled && u.PasswordHash != "" && u.Role == model.RoleAdmin {
			return true
		}
	}
	return false
}

// RecordAudit lets other packages (server) add audit entries.
func (s *Service) RecordAudit(actor, action, target, detail string) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	s.st.Audit(actor, action, target, detail)
}

// TestModel runs the engine's doctor against a model profile and returns its
// report. It uses a throw-away home so nothing is left behind.
func (s *Service) TestModel(ctx context.Context, id string) (ok bool, report string, err error) {
	s.st.Mu.Lock()
	m := s.st.ModelProfile(id)
	if m == nil {
		s.st.Mu.Unlock()
		return false, "", ErrNotFound
	}
	env, err := s.modelEnvLocked(m)
	auth := m.AuthStyle
	s.st.Mu.Unlock()
	if err != nil {
		return false, err.Error(), nil
	}
	home, err := os.MkdirTemp("", "zpilot-doctor-*")
	if err != nil {
		return false, "", err
	}
	defer os.RemoveAll(home)
	_ = os.WriteFile(filepath.Join(home, ".env"), []byte(profile.EnvPlaceholder), 0o600)
	if auth != "" {
		_ = os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"provider":{"auth_style":"`+auth+`"}}`), 0o600)
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.cfg.Exe, "engine", "doctor", "--home", home)
	cmd.Env = append(cleanEnv(os.Environ()), env...)
	platform.Configure(cmd)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	runErr := cmd.Run()
	text := strings.TrimSpace(out.String())
	// Keep the key out of the report no matter what the engine printed.
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "ZAGENT_API_KEY="); ok && len(v) >= 6 {
			text = strings.ReplaceAll(text, v, "<api key>")
		}
	}
	return runErr == nil, text, nil
}
