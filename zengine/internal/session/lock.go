package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	heartbeatEvery = 2 * time.Second
	staleAfter     = 10 * time.Second
)

// LockInfo describes the process that owns a session.
type LockInfo struct {
	PID       int       `json:"pid"`
	Started   time.Time `json:"started"`
	Heartbeat time.Time `json:"heartbeat"`
	Role      string    `json:"role,omitempty"` // free-form: "engine", "headless", ...
	// Addr is where the owner serves its control API, so clients can attach.
	Addr string `json:"addr,omitempty"`
	// Token authenticates clients of that API. The lock file is private to the user.
	Token string `json:"token,omitempty"`
}

// ErrLocked means another live process drives the session.
type ErrLocked struct{ Info LockInfo }

func (e *ErrLocked) Error() string {
	return fmt.Sprintf("session is in use by pid %d since %s (heartbeat %s ago)",
		e.Info.PID, e.Info.Started.Local().Format("15:04:05"), time.Since(e.Info.Heartbeat).Round(time.Second))
}

// Lock holds an OS-owned exclusive claim on a session. Heartbeats publish
// status only; process death releases ownership even when metadata survives.
type Lock struct {
	path        string
	mu          sync.Mutex
	writeMu     sync.Mutex
	owner       *ownerLock
	releaseOnce sync.Once
	info        LockInfo
	stop        chan struct{}
	done        chan struct{}
}

func lockPath(dir string) string { return filepath.Join(dir, "lock.json") }

// ReadLock reads discovery metadata. A stale heartbeat is still live when the
// owner holds the OS lock. Acquire is the authority for session exclusion.
func ReadLock(dir string) *LockInfo {
	b, err := os.ReadFile(lockPath(dir))
	if err != nil {
		return nil
	}
	var li LockInfo
	if json.Unmarshal(b, &li) != nil {
		return nil
	}
	if time.Since(li.Heartbeat) > staleAfter {
		owner, err := acquireOwner(filepath.Join(dir, "owner.lock"))
		if err == nil {
			owner.close()
			return nil
		}
	}
	return &li
}

// Acquire claims the session or returns *ErrLocked.
func Acquire(dir, role string) (*Lock, error) {
	path := lockPath(dir)
	owner, err := acquireOwner(filepath.Join(dir, "owner.lock"))
	if err != nil {
		if ownerBusy(err) {
			info := LockInfo{}
			if b, readErr := os.ReadFile(path); readErr == nil {
				_ = json.Unmarshal(b, &info)
			}
			return nil, &ErrLocked{Info: info}
		}
		return nil, err
	}
	now := time.Now().UTC()
	info := LockInfo{PID: os.Getpid(), Started: now, Heartbeat: now, Role: role}
	b, _ := json.Marshal(info)
	if err := atomicWritePerm(path, b, 0o600); err != nil {
		owner.close()
		return nil, err
	}
	l := &Lock{path: path, info: info, owner: owner, stop: make(chan struct{}), done: make(chan struct{})}
	go l.beat()
	return l, nil
}

// SetAddr publishes the owner's control address in the lock file.
func (l *Lock) SetAddr(addr string) { l.SetEndpoint(addr, "") }

// SetEndpoint publishes the control address and its access token.
func (l *Lock) SetEndpoint(addr, token string) {
	l.mu.Lock()
	l.info.Addr, l.info.Token = addr, token
	l.mu.Unlock()
	l.write()
}

func (l *Lock) write() {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	select {
	case <-l.stop:
		return
	default:
	}
	l.mu.Lock()
	l.info.Heartbeat = time.Now().UTC()
	b, _ := json.Marshal(l.info)
	l.mu.Unlock()
	// Windows can refuse a rename while a reader has the file open; retry briefly.
	for i := 0; i < 5; i++ {
		if atomicWritePerm(l.path, b, 0o600) == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (l *Lock) beat() {
	defer close(l.done)
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			l.write()
		case <-l.stop:
			return
		}
	}
}

// Release stops the heartbeat and removes the lock file.
func (l *Lock) Release() {
	l.releaseOnce.Do(func() {
		close(l.stop)
		<-l.done
		l.writeMu.Lock()
		defer l.writeMu.Unlock()
		_ = os.Remove(l.path)
		l.owner.close()
	})
}
