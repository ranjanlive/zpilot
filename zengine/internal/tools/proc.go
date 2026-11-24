package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

const procBufferCap = 4 << 20 // output kept per process
const maxFinishedProcesses = 128

// Proc is one command started through the manager.
type Proc struct {
	ID      int
	Command string
	Cwd     string
	Started time.Time

	mu       sync.Mutex
	buf      []byte
	base     int64 // stream offset of buf[0]
	done     bool
	exitCode int
	ended    time.Time
	killed   bool
	timedOut bool
	waitErr  error
	changed  chan struct{} // closed and replaced whenever state or output changes

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	doneCh chan struct{}

	detached   bool  // handed to the background: notify on exit
	readCursor int64 // stream offset the model has already been shown
	note       string
}

// Detach marks the process as a background task so its exit is announced. It
// returns false if the process has already finished (the caller then reports
// the result synchronously and no announcement is needed).
func (p *Proc) Detach() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		return false
	}
	p.detached = true
	return true
}

// Unread returns output the model has not seen yet and advances the cursor.
func (p *Proc) Unread() (data []byte, lost bool) {
	p.mu.Lock()
	from := p.readCursor
	p.mu.Unlock()
	data, next, lost := p.Output(from)
	p.mu.Lock()
	if next > p.readCursor {
		p.readCursor = next
	}
	p.mu.Unlock()
	return data, lost
}

// ProcInfo is a snapshot of a Proc.
type ProcInfo struct {
	ID       int
	Command  string
	Cwd      string
	Running  bool
	ExitCode int
	Killed   bool
	TimedOut bool
	Started  time.Time
	Ended    time.Time
	Bytes    int64 // total output bytes produced
	// Note is the exit announcement computed by the starter (external tools),
	// or empty to use the generic one.
	Note string
}

func (p *Proc) Info() ProcInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ProcInfo{ID: p.ID, Command: p.Command, Cwd: p.Cwd, Running: !p.done, ExitCode: p.exitCode,
		Killed: p.killed, TimedOut: p.timedOut, Started: p.Started, Ended: p.ended, Bytes: p.base + int64(len(p.buf)), Note: p.note}
}

// Write implements io.Writer for the merged stdout/stderr stream.
func (p *Proc) Write(b []byte) (int, error) {
	p.mu.Lock()
	p.buf = append(p.buf, b...)
	if over := len(p.buf) - procBufferCap; over > 0 {
		p.buf = append([]byte(nil), p.buf[over:]...)
		p.base += int64(over)
	}
	p.signalLocked()
	p.mu.Unlock()
	return len(b), nil
}

func (p *Proc) signalLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// Output returns output from stream offset `from` (use 0 for everything
// still buffered). It reports the next offset and whether older output was
// already discarded.
func (p *Proc) Output(from int64) (data []byte, next int64, lost bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	end := p.base + int64(len(p.buf))
	if from < p.base {
		from, lost = p.base, p.base > 0 && from < p.base
	}
	if from > end {
		from = end
	}
	return append([]byte(nil), p.buf[from-p.base:]...), end, lost
}

// Wait blocks until the process exits, ctx ends or d elapses. It reports
// whether the process has exited.
func (p *Proc) Wait(ctx context.Context, d time.Duration) bool {
	var timer <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-p.doneCh:
		return true
	case <-ctx.Done():
	case <-timer:
	}
	select {
	case <-p.doneCh:
		return true
	default:
		return false
	}
}

// Manager owns the processes of one engine.
type Manager struct {
	mu    sync.Mutex
	procs map[int]*Proc
	next  int
	// OnExit is called (in its own goroutine) when a process ends.
	OnExit func(ProcInfo)
}

// NewManager creates an empty manager.
func NewManager() *Manager { return &Manager{procs: map[int]*Proc{}, next: 1} }

// StartOptions configure Start.
type StartOptions struct {
	// Command is a shell command line. Argv, when set, takes precedence and is
	// executed directly with no shell involved.
	Command string
	Argv    []string
	Cwd     string
	Env     []string
	// Stdin, if non-nil, is written to the process and stdin is then closed.
	Stdin []byte
	// Stdout, if set, receives the process's stdout separately; stderr then
	// goes to the task output buffer. Otherwise both share the buffer.
	Stdout io.Writer
	// Note, if set, computes the exit announcement once the process ends.
	Note func(ProcInfo) string
	// Timeout kills the process (and its children) after this long; 0 = none.
	Timeout time.Duration
}

// Start launches a shell command.
func (m *Manager) Start(o StartOptions) (*Proc, error) {
	var cmd *exec.Cmd
	if len(o.Argv) > 0 {
		cmd = exec.Command(o.Argv[0], o.Argv[1:]...)
	} else {
		var err error
		if cmd, err = shellCommand(o.Command); err != nil {
			return nil, err
		}
	}
	cmd.Dir = o.Cwd
	cmd.Env = append(scrubEnv(os.Environ()), o.Env...)
	configureProc(cmd)

	m.mu.Lock()
	id := m.next
	m.next++
	m.mu.Unlock()

	label := o.Command
	if label == "" {
		label = strings.Join(o.Argv, " ")
	}
	p := &Proc{ID: id, Command: label, Cwd: o.Cwd, Started: time.Now(), changed: make(chan struct{}), cmd: cmd, doneCh: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = p, p // same writer: one merged, ordered stream
	if o.Stdout != nil {
		cmd.Stdout = o.Stdout
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	p.stdin = stdin
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start command: %w", err)
	}
	if o.Stdin != nil {
		go func() {
			_, _ = stdin.Write(o.Stdin)
			_ = stdin.Close()
		}()
	}
	m.mu.Lock()
	m.procs[id] = p
	m.mu.Unlock()

	var timer *time.Timer
	if o.Timeout > 0 {
		timer = time.AfterFunc(o.Timeout, func() {
			p.mu.Lock()
			p.timedOut = true
			p.mu.Unlock()
			killTree(p.cmd)
		})
	}
	go func() {
		err := cmd.Wait()
		if timer != nil {
			timer.Stop()
		}
		p.mu.Lock()
		p.done, p.ended, p.waitErr = true, time.Now(), err
		if cmd.ProcessState != nil {
			p.exitCode = cmd.ProcessState.ExitCode()
		}
		announce := p.detached
		p.signalLocked()
		p.mu.Unlock()
		if o.Note != nil {
			n := o.Note(p.Info())
			p.mu.Lock()
			p.note = n
			p.mu.Unlock()
		}
		close(p.doneCh)
		if announce && m.OnExit != nil {
			go m.OnExit(p.Info())
		}
		m.pruneFinished()
	}()
	return p, nil
}

// Retain recent completed terminals for inspection without retaining every
// command and its 4 MiB output for the lifetime of a long-running engine.
func (m *Manager) pruneFinished() {
	m.mu.Lock()
	defer m.mu.Unlock()
	var completed []int
	for id, p := range m.procs {
		select {
		case <-p.doneCh:
			completed = append(completed, id)
		default:
		}
	}
	if len(completed) <= maxFinishedProcesses {
		return
	}
	sort.Ints(completed)
	for _, id := range completed[:len(completed)-maxFinishedProcesses] {
		delete(m.procs, id)
	}
}

// scrubEnv removes the engine's own credentials (ZAGENT_API_KEY and anything
// else under the ZAGENT_ prefix that looks like a secret) from an environment
// before it is handed to a command the model started: otherwise `env` would
// hand the model the API key it is running on.
func scrubEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		up := strings.ToUpper(name)
		if strings.HasPrefix(up, "ZAGENT_") {
			secret := false
			for _, suffix := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD"} {
				secret = secret || strings.HasSuffix(up, suffix)
			}
			if secret {
				continue
			}
		}
		out = append(out, kv)
	}
	return out
}

// Get returns a process by ID.
func (m *Manager) Get(id int) (*Proc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.procs[id]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("no terminal task with id %d (use terminal_list to see tasks)", id)
}

// List returns snapshots ordered by ID.
func (m *Manager) List() []ProcInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ProcInfo, 0, len(m.procs))
	for _, p := range m.procs {
		out = append(out, p.Info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Running reports how many processes are still alive.
func (m *Manager) Running() int {
	n := 0
	for _, i := range m.List() {
		if i.Running {
			n++
		}
	}
	return n
}

// Kill terminates a process tree.
func (p *Proc) Kill() {
	p.mu.Lock()
	if p.done {
		p.mu.Unlock()
		return
	}
	p.killed = true
	p.mu.Unlock()
	killTree(p.cmd)
}

// SendInput writes to the process's stdin.
func (p *Proc) SendInput(s string) error {
	p.mu.Lock()
	done := p.done
	p.mu.Unlock()
	if done {
		return errors.New("the task has already finished")
	}
	_, err := io.WriteString(p.stdin, s)
	return err
}

// CloseStdin signals end of input.
func (p *Proc) CloseStdin() { _ = p.stdin.Close() }

// KillAll terminates every running process (engine shutdown).
func (m *Manager) KillAll() {
	m.mu.Lock()
	ps := make([]*Proc, 0, len(m.procs))
	for _, p := range m.procs {
		ps = append(ps, p)
	}
	m.mu.Unlock()
	for _, p := range ps {
		p.Kill()
	}
}
