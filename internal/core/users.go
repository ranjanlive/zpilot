package core

import (
	"slices"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/internal/auth"
	"github.com/ranjanlive/zpilot/internal/model"
)

// Users lists accounts without password hashes.
func (s *Service) Users() []model.User {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	out := []model.User{}
	for _, u := range s.st.S.Users {
		c := *u
		c.PasswordHash = ""
		out = append(out, c)
	}
	return out
}

// SaveUser creates or updates an account. An empty password keeps the
// current one (and is required for new accounts).
func (s *Service) SaveUser(actor string, in model.User, password string) (model.User, error) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	in.Username = strings.ToLower(strings.TrimSpace(in.Username))
	if err := in.Validate(); err != nil {
		return in, bad("%v", err)
	}
	var cur *model.User
	for _, u := range s.st.S.Users {
		switch {
		case in.ID != "" && u.ID == in.ID:
			cur = u
		case u.Username == in.Username && (in.ID == "" || u.ID != in.ID):
			if in.ID == "" && u.Username == "admin" && in.Username == "admin" {
				cur = u // SetAdminPassword on an existing admin
				continue
			}
			return in, bad("username %s is taken", in.Username)
		}
	}
	if in.ID != "" && cur == nil {
		return in, ErrNotFound
	}
	switch {
	case password != "":
		h, err := auth.HashPassword(password)
		if err != nil {
			return in, bad("%v", err)
		}
		in.PasswordHash = h
	case cur != nil:
		in.PasswordHash = cur.PasswordHash
	default:
		return in, bad("a password is required")
	}
	if cur != nil {
		in.ID, in.Created, in.LastLogin = cur.ID, cur.Created, cur.LastLogin
		// never lock everyone out of administration
		if cur.Role == model.RoleAdmin && !cur.Disabled && (in.Role != model.RoleAdmin || in.Disabled) && s.adminCountLocked() == 1 {
			return in, bad("this is the last admin account; create another admin first")
		}
		*cur = in
	} else {
		in.ID = model.NewID("usr")
		in.Created = time.Now().UTC()
		c := in
		s.st.S.Users = append(s.st.S.Users, &c)
	}
	if err := s.st.Save(); err != nil {
		return in, err
	}
	s.st.Audit(actor, "user.save", in.Username, in.Role)
	in.PasswordHash = ""
	return in, nil
}

func (s *Service) adminCountLocked() int {
	n := 0
	for _, u := range s.st.S.Users {
		if u.Role == model.RoleAdmin && !u.Disabled {
			n++
		}
	}
	return n
}

// DeleteUser removes an account (not the last admin).
func (s *Service) DeleteUser(actor, id string) error {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	i := slices.IndexFunc(s.st.S.Users, func(u *model.User) bool { return u.ID == id || u.Username == id })
	if i < 0 {
		return ErrNotFound
	}
	u := s.st.S.Users[i]
	if u.Role == model.RoleAdmin && !u.Disabled && s.adminCountLocked() == 1 {
		return bad("this is the last admin account")
	}
	s.st.S.Users = slices.Delete(s.st.S.Users, i, i+1)
	if err := s.st.Save(); err != nil {
		return err
	}
	s.st.Audit(actor, "user.delete", u.Username, "")
	return nil
}

// Authenticate checks a username and password and returns the account.
func (s *Service) Authenticate(username, password string) (model.User, bool) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	username = strings.ToLower(strings.TrimSpace(username))
	for _, u := range s.st.S.Users {
		if u.Username == username && !u.Disabled && auth.CheckPassword(u.PasswordHash, password) {
			u.LastLogin = time.Now().UTC()
			_ = s.st.Save()
			c := *u
			c.PasswordHash = ""
			return c, true
		}
	}
	return model.User{}, false
}

// UserByID returns an enabled account.
func (s *Service) UserByID(id string) (model.User, bool) {
	s.st.Mu.Lock()
	defer s.st.Mu.Unlock()
	for _, u := range s.st.S.Users {
		if u.ID == id && !u.Disabled {
			c := *u
			c.PasswordHash = ""
			return c, true
		}
	}
	return model.User{}, false
}

// migrateUsersLocked turns the legacy single admin password into an "admin" account.
func (s *Service) migrateUsersLocked() {
	if s.st.S.AdminPassword != "" && len(s.st.S.Users) == 0 {
		s.st.S.Users = append(s.st.S.Users, &model.User{ID: model.NewID("usr"), Username: "admin", Name: "Administrator",
			Role: model.RoleAdmin, PasswordHash: s.st.S.AdminPassword, Created: time.Now().UTC()})
		s.st.S.AdminPassword = ""
	}
}
