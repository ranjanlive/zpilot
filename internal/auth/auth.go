// Package auth handles owner authentication for the HTTP API:
//
//   - a local admin token (written to <data>/endpoint.json, readable only by
//     the owner's OS account) used by the CLI and to open the dashboard
//   - an optional admin password (PBKDF2-SHA256) for remote/VPS access
//   - browser sessions (random ids in an HttpOnly, SameSite=Strict cookie)
//   - a login rate limiter
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	iterations = 600_000
	keyLen     = 32
	// SessionTTL is how long a browser session lasts without use.
	SessionTTL = 7 * 24 * time.Hour
)

// HashPassword returns "pbkdf2-sha256$<iter>$<salt>$<hash>".
func HashPassword(pw string) (string, error) {
	if len(pw) < 10 {
		return "", fmt.Errorf("use at least 10 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	k, err := pbkdf2.Key(sha256.New, pw, salt, iterations, keyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", iterations, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(k)), nil
}

// CheckPassword verifies pw against a HashPassword result in constant time.
func CheckPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// TokenEqual compares secrets in constant time.
func TokenEqual(a, b string) bool {
	return a != "" && b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Sessions is an in-memory session table (sessions end on restart).
type Sessions struct {
	mu sync.Mutex
	m  map[string]*session
}

type session struct {
	subject string // user id; "" means the local admin token
	last    time.Time
}

func NewSessions() *Sessions { return &Sessions{m: map[string]*session{}} }

// New creates a session for subject and returns its id.
func (s *Sessions) New(subject string) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	s.mu.Lock()
	s.m[id] = &session{subject: subject, last: time.Now()}
	s.mu.Unlock()
	return id
}

// Subject returns the session's subject if it is live, refreshing it.
func (s *Sessions) Subject(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.m[id]
	if !ok || time.Since(ss.last) > SessionTTL {
		delete(s.m, id)
		return "", false
	}
	ss.last = time.Now()
	return ss.subject, true
}

// Valid reports whether id is a live session.
func (s *Sessions) Valid(id string) bool { _, ok := s.Subject(id); return ok }

// EndSubject ends every session of a subject (after a password change or deletion).
func (s *Sessions) EndSubject(subject string) {
	s.mu.Lock()
	for id, ss := range s.m {
		if ss.subject == subject {
			delete(s.m, id)
		}
	}
	s.mu.Unlock()
}

// End removes a session.
func (s *Sessions) End(id string) {
	s.mu.Lock()
	delete(s.m, id)
	s.mu.Unlock()
}

// EndAll removes every session (after a password change).
func (s *Sessions) EndAll() {
	s.mu.Lock()
	s.m = map[string]*session{}
	s.mu.Unlock()
}

// Limiter slows down password guessing: after 5 failures from an address,
// each further attempt must wait, doubling up to 15 minutes.
type Limiter struct {
	mu sync.Mutex
	m  map[string]*attempts
}

type attempts struct {
	fails int
	until time.Time
}

func NewLimiter() *Limiter { return &Limiter{m: map[string]*attempts{}} }

// Allow reports whether addr may try now, and if not, how long to wait.
func (l *Limiter) Allow(addr string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.m[addr]
	if a == nil || time.Now().After(a.until) {
		return true, 0
	}
	return false, time.Until(a.until)
}

// Result records the outcome of an attempt.
func (l *Limiter) Result(addr string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ok {
		delete(l.m, addr)
		return
	}
	a := l.m[addr]
	if a == nil {
		a = &attempts{}
		l.m[addr] = a
	}
	a.fails++
	if a.fails >= 5 {
		d := time.Duration(1<<min(a.fails-5, 10)) * time.Second
		a.until = time.Now().Add(min(d, 15*time.Minute))
	}
}
