package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/config"
	"github.com/ranjanlive/zpilot/zengine/internal/control"
	"github.com/ranjanlive/zpilot/zengine/internal/engine"
	"github.com/ranjanlive/zpilot/zengine/internal/webui"
)

// host is an engine plus the control server that exposes it. Every engine
// process has one, so any interface can attach to any running session.
type host struct {
	eng *engine.Engine
	srv *control.Server
	// quit is closed when a client asks the process to end.
	quit chan struct{}
	// stopBridge flushes and stops live forwarding (ZPilot runs only).
	stopBridge func()
}

func newHost(home config.Home, env config.Env, cfg config.Config, workspace, sessionID, role string) (*host, error) {
	opts := engine.Options{Home: home, Env: env, Cfg: cfg, Workspace: workspace, SessionID: sessionID, Role: role}
	br := newBridge()
	if br != nil && role == "headless" {
		opts.Approve = br.approve
	}
	eng, err := engine.New(opts)
	if err != nil {
		return nil, err
	}
	h := &host{eng: eng, quit: make(chan struct{})}
	if br != nil && role == "headless" {
		h.stopBridge = br.forwardLive(eng)
	}
	h.srv, err = control.Serve(eng, control.Options{Version: version, UI: webui.Handler(), OnShutdown: func() {
		select {
		case <-h.quit:
		default:
			close(h.quit)
		}
	}})
	if err != nil {
		eng.Close()
		return nil, err
	}
	eng.Start()
	return h, nil
}

func (h *host) close() {
	if h.stopBridge != nil {
		h.stopBridge()
	}
	h.srv.Close()
	_ = h.eng.Close()
}

// runService is the detached background process behind the interfaces
// (`zagent --serve`). It lives until a client sends /quit, the process is
// signalled, or it has been idle with nobody attached for the configured time.
func runService(o *options, stderr io.Writer) int {
	home, env, cfg, ok := loadHome(o.home, stderr)
	if !ok {
		return exitUsage
	}
	workspace, err := resolveWorkspace(o.workspace)
	if err != nil {
		fmt.Fprintf(stderr, "zagent: %v\n", err)
		return exitUsage
	}
	if o.session == "" {
		fmt.Fprintln(stderr, "zagent --serve needs --session NAME")
		return exitUsage
	}
	h, err := newHost(home, env, cfg, workspace, o.session, "service")
	if err != nil {
		fmt.Fprintf(stderr, "zagent: %v\n", err)
		return exitFailed
	}
	defer h.close()
	fmt.Fprintf(stderr, "%s zagent service for session %s at %s (pid %d)\n", time.Now().Format(time.RFC3339), o.session, h.srv.Addr(), os.Getpid())

	idleLimit := time.Duration(cfg.Service.IdleExitMinutes) * time.Minute
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	idleSince := time.Now()
	sig := notifySignals()
	defer sig.stop()
	for {
		select {
		case <-h.quit:
			fmt.Fprintln(stderr, "quit requested by a client")
			return exitOK
		case <-sig.c:
			fmt.Fprintln(stderr, "signal received, shutting down")
			return exitOK
		case <-tick.C:
			st, _ := h.eng.State()
			streams, sinceLast := h.srv.Clients()
			busy := st != engine.StateIdle || h.eng.RunningTasks() > 0 || streams > 0 || sinceLast < time.Minute
			if busy {
				idleSince = time.Now()
				continue
			}
			if idleLimit > 0 && time.Since(idleSince) >= idleLimit {
				fmt.Fprintf(stderr, "idle for %s with nobody attached; exiting\n", idleLimit)
				return exitOK
			}
		}
	}
}

// ensureService returns a client for the session's engine, starting a
// detached service process first if none is running.
func ensureService(ctx context.Context, o *options, home config.Home, workspace, sessionID string, stderr io.Writer) (*control.Client, error) {
	dir := home.SessionDir(sessionID)
	if c, err := control.Attach(dir); err == nil {
		if _, serr := c.State(ctx); serr == nil {
			return c, nil
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(filepath.Join(dir, "service.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	args := []string{"--serve", "--home", home.Root, "--workspace", workspace, "--session", sessionID}
	cmd := exec.Command(exe, append(append([]string{}, SelfArgs...), args...)...)
	cmd.Stdout, cmd.Stderr = logf, logf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the background service: %w", err)
	}
	_ = cmd.Process.Release()
	c, err := control.WaitReady(ctx, dir, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%w (see %s)", err, filepath.Join(dir, "service.log"))
	}
	return c, nil
}
