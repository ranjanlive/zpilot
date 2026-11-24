package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/ranjanlive/zpilot/internal/channels"
	"github.com/ranjanlive/zpilot/internal/core"
	"github.com/ranjanlive/zpilot/internal/platform"
	"github.com/ranjanlive/zpilot/internal/server"
)

const defaultListen = "127.0.0.1:7700"

func cmdServe(data string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", defaultListen, "address for the dashboard and API (use 0.0.0.0:7700 on a server)")
	tlsCert := fs.String("tls-cert", "", "TLS certificate file (PEM)")
	tlsKey := fs.String("tls-key", "", "TLS private key file (PEM)")
	insecure := fs.Bool("insecure-http", false, "allow plain HTTP on a public address (only behind a TLS reverse proxy)")
	open := fs.Bool("open", false, "open the dashboard in the browser")
	tray := fs.Bool("tray", false, "show a system tray icon (Windows)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	logger := log.New(stderr, "zpilot: ", log.LstdFlags)
	logf := func(f string, a ...any) { logger.Printf(f, a...) }

	svc, err := core.Open(core.Config{DataDir: data, Log: logf})
	if err != nil {
		fmt.Fprintf(stderr, "zpilot: %v\n", err)
		return 1
	}
	defer svc.Close()

	srv, err := server.New(svc, server.Options{Listen: *listen, TLSCert: *tlsCert, TLSKey: *tlsKey, InsecureHTTP: *insecure, Version: version, Log: logf})
	if err != nil {
		fmt.Fprintf(stderr, "zpilot: %v\n", err)
		return 2
	}
	if err := srv.Start(); err != nil {
		fmt.Fprintf(stderr, "zpilot: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chm := channels.NewManager(svc, logf)
	svc.SetOutbound(chm)
	srv.SetWhatsApp(chm)
	go chm.Run(ctx)
	svc.Start()

	fmt.Fprintf(stdout, "ZPilot %s\n  data:      %s\n  dashboard: %s\n", version, svc.DataDir(), srv.URL())
	fmt.Fprintf(stdout, "  sign in:   zpilot ui   (or open the link in %s)\n", filepath.Join(svc.DataDir(), "endpoint.json"))
	if *open {
		_ = platform.OpenURL(srv.LoginURL())
	}

	stop := make(chan os.Signal, 2)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	quit := make(chan struct{})
	go watchShutdownFile(ctx, svc.DataDir(), quit)

	trayDone := make(chan struct{})
	if *tray && runtime.GOOS == "windows" {
		go func() {
			items := func() []platform.MenuItem {
				running, queued := svc.Active()
				return []platform.MenuItem{
					{Action: "open", Label: "Open dashboard"},
					{Label: fmt.Sprintf("%d running, %d queued", running, queued), Disabled: true},
					{Separator: true},
					{Action: "quit", Label: "Quit ZPilot"},
				}
			}
			_ = platform.Tray(items, func(id string) {
				switch id {
				case "open":
					_ = platform.OpenURL(srv.LoginURL())
				case "quit":
					select {
					case <-quit:
					default:
						close(quit)
					}
				}
			}, trayDone)
		}()
	}

	select {
	case <-stop:
	case <-quit:
	}
	close(trayDone)
	fmt.Fprintln(stdout, "zpilot: shutting down…")
	cancel()
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	srv.Shutdown(sctx)
	return 0
}

// watchShutdownFile lets "zpilot stop" end the daemon portably: it writes a
// stop file into the (owner-only) data directory.
func watchShutdownFile(ctx context.Context, data string, quit chan struct{}) {
	p := filepath.Join(data, "stop.request")
	_ = os.Remove(p)
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := os.Stat(p); err == nil {
				_ = os.Remove(p)
				select {
				case <-quit:
				default:
					close(quit)
				}
				return
			}
		}
	}
}

func cmdStart(data string, args []string, stdout, stderr io.Writer) int {
	if ep, err := readEndpoint(data); err == nil && ping(ep) == nil {
		fmt.Fprintf(stdout, "ZPilot is already running at %s\n", ep.URL)
		return 0
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "zpilot: %v\n", err)
		return 1
	}
	// A daemon that is still shutting down holds the data directory; wait for it.
	if _, err := os.Stat(data); err == nil && !waitUnlocked(data, 30*time.Second) {
		fmt.Fprintln(stderr, "zpilot: another ZPilot process is still using this data directory")
		return 1
	}
	if err := os.MkdirAll(data, 0o700); err != nil {
		fmt.Fprintf(stderr, "zpilot: %v\n", err)
		return 1
	}
	logPath := filepath.Join(data, "daemon.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "zpilot: %v\n", err)
		return 1
	}
	defer logf.Close()
	_ = os.Remove(filepath.Join(data, "endpoint.json"))
	cmd := exec.Command(exe, append([]string{"--data", data, "serve"}, args...)...)
	cmd.Stdout, cmd.Stderr = logf, logf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "zpilot: %v\n", err)
		return 1
	}
	_ = cmd.Process.Release()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ep, err := readEndpoint(data); err == nil && ping(ep) == nil {
			fmt.Fprintf(stdout, "ZPilot started at %s\nOpen the dashboard with: zpilot ui\n", ep.URL)
			return 0
		}
		time.Sleep(250 * time.Millisecond)
	}
	fmt.Fprintf(stderr, "zpilot: the daemon did not come up; see %s\n", logPath)
	return 1
}

// readEndpoint loads endpoint.json written by a running daemon.
func readEndpoint(data string) (server.Endpoint, error) {
	var ep server.Endpoint
	b, err := os.ReadFile(filepath.Join(data, "endpoint.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ep, errNotRunning
		}
		return ep, err
	}
	if err := json.Unmarshal(b, &ep); err != nil {
		return ep, err
	}
	return ep, nil
}

var errNotRunning = errors.New("ZPilot is not running (start it with 'zpilot start' or 'zpilot serve')")
