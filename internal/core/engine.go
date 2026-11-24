package core

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ranjanlive/zpilot/internal/platform"
)

// RunSpec is one engine invocation.
type RunSpec struct {
	Home      string
	Workspace string
	Session   string
	Prompt    string
	Env       []string // ZAGENT_* model settings
	Timeout   time.Duration
	LogPath   string
	// OnActivity receives engine progress lines (tool calls, retries).
	OnActivity func(line string)
	// Limits and isolation for the run and everything it starts.
	Limits  platform.Limits
	Isolate bool
}

// RunResult is what came back.
type RunResult struct {
	Answer   string
	ExitCode int
	Err      error // could not start, timed out, ...
	TimedOut bool
	Prompt   int
	Answered int
	Requests int
	Tail     string // last lines of stderr, for error messages
}

// Engine runs the agent engine.
type Engine interface {
	Run(ctx context.Context, spec RunSpec) RunResult
}

// Engine exit codes (the ZEngine headless contract).
const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
	exitLocked = 3
)

// ProcessEngine runs "zpilot engine ..." as a child process, confined so that
// stopping it also stops everything the agent started (Job Objects on
// Windows, process groups elsewhere).
type ProcessEngine struct{ Exe string }

var countersRe = regexp.MustCompile(`(\d+) request\(s\), context \d+ tokens, total (\d+) prompt / (\d+) answer`)

func (p *ProcessEngine) Run(ctx context.Context, spec RunSpec) RunResult {
	var res RunResult
	promptFile, err := os.CreateTemp("", "zpilot-prompt-*.txt")
	if err != nil {
		res.Err = err
		return res
	}
	defer os.Remove(promptFile.Name())
	if _, err := promptFile.WriteString(spec.Prompt); err != nil {
		promptFile.Close()
		res.Err = err
		return res
	}
	promptFile.Close()

	if spec.Timeout <= 0 {
		spec.Timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	args := []string{"engine", "--home", spec.Home, "--workspace", spec.Workspace, "--session", spec.Session, "--prompt-file", promptFile.Name()}
	argv, err := sandboxArgv(append([]string{p.Exe}, args...), spec)
	if err != nil {
		res.Err = err
		return res
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = spec.Workspace
	cmd.Env = append(cleanEnv(os.Environ()), spec.Env...)
	platform.Configure(cmd)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	stderrR, stderrW := io.Pipe()
	cmd.Stderr = stderrW

	var logw io.Writer = io.Discard
	if spec.LogPath != "" {
		_ = os.MkdirAll(filepath.Dir(spec.LogPath), 0o700)
		if f, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			defer f.Close()
			logw = f
		}
	}

	if err := cmd.Start(); err != nil {
		stderrW.Close()
		res.Err = fmt.Errorf("start engine: %w", err)
		return res
	}
	release, terr := platform.TrackLimits(cmd, spec.Limits)
	if terr == nil && release != nil {
		defer release()
	}

	tail := newTail(40)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderrR)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			fmt.Fprintln(logw, line)
			tail.add(line)
			if m := countersRe.FindStringSubmatch(line); m != nil {
				res.Requests, _ = strconv.Atoi(m[1])
				res.Prompt, _ = strconv.Atoi(m[2])
				res.Answered, _ = strconv.Atoi(m[3])
			}
			if spec.OnActivity != nil {
				if t := strings.TrimSpace(line); strings.HasPrefix(t, "▸") || strings.HasPrefix(t, "⟳") || strings.HasPrefix(t, "⏸") {
					spec.OnActivity(t)
				}
			}
		}
		io.Copy(io.Discard, stderrR)
	}()

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait(); stderrW.Close() }()

	var err2 error
	select {
	case err2 = <-waitErr:
	case <-ctx.Done():
		platform.Kill(cmd)
		err2 = <-waitErr
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			res.TimedOut = true
		}
		res.Err = ctx.Err()
	}
	wg.Wait()
	res.Tail = tail.String()
	res.Answer = strings.TrimSpace(stdout.String())
	fmt.Fprintf(logw, "---- answer ----\n%s\n", res.Answer)
	var ee *exec.ExitError
	switch {
	case err2 == nil:
		res.ExitCode = 0
	case errors.As(err2, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		if res.Err == nil {
			res.Err = err2
		}
		res.ExitCode = -1
	}
	return res
}

// cleanEnv drops inherited ZAGENT_* variables so the daemon's environment can
// never leak a different model or key into an agent.
func cleanEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(strings.ToUpper(kv), "ZAGENT_") {
			out = append(out, kv)
		}
	}
	return out
}

type tailBuf struct {
	n     int
	lines []string
	mu    sync.Mutex
}

func newTail(n int) *tailBuf { return &tailBuf{n: n} }

func (t *tailBuf) add(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, s)
	if len(t.lines) > t.n {
		t.lines = t.lines[len(t.lines)-t.n:]
	}
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "\n")
}

// sandboxArgv wraps the engine command for Linux limits and isolation.
// Windows limits are applied through the Job Object instead.
func sandboxArgv(argv []string, spec RunSpec) ([]string, error) {
	if runtime.GOOS != "linux" {
		if spec.Isolate {
			return nil, fmt.Errorf("file-system isolation is available on Linux (with bubblewrap) only; turn it off for this agent")
		}
		return argv, nil
	}
	if spec.Isolate {
		bwrap, err := exec.LookPath("bwrap")
		if err != nil {
			return nil, fmt.Errorf("isolation needs bubblewrap (install the 'bubblewrap' package) or turn isolation off for this agent")
		}
		tmp := os.TempDir()
		wrapped := []string{bwrap, "--die-with-parent", "--new-session", "--unshare-pid", "--unshare-ipc",
			"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/run",
			"--bind", tmp, tmp, "--bind", spec.Workspace, spec.Workspace, "--bind", spec.Home, spec.Home, "--"}
		argv = append(wrapped, argv...)
	}
	if spec.Limits.MemoryMB > 0 {
		prlimit, err := exec.LookPath("prlimit")
		if err != nil {
			return nil, fmt.Errorf("the memory limit needs prlimit (util-linux)")
		}
		bytes := strconv.FormatInt(int64(spec.Limits.MemoryMB)<<20, 10)
		argv = append([]string{prlimit, "--as=" + bytes, "--"}, argv...)
	}
	return argv, nil
}
