package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ranjanlive/zpilot/internal/model"
	"github.com/ranjanlive/zpilot/internal/platform"
	"github.com/ranjanlive/zpilot/internal/server"
)

// cmdDesktop is the desktop companion: it makes sure the daemon runs, opens
// the dashboard in its own app window, shows a tray icon (Windows) and turns
// important events into native notifications.
func cmdDesktop(data string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("desktop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	minimized := fs.Bool("minimized", false, "start in the tray without opening the window (used at login)")
	listen := fs.String("listen", defaultListen, "address for the daemon if it has to be started")
	quiet := fs.Bool("no-notifications", false, "do not show desktop notifications")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ep, err := readEndpoint(data)
	if err != nil || ping(ep) != nil {
		if code := cmdStart(data, []string{"--listen", *listen}, io.Discard, stderr); code != 0 {
			return code
		}
		if ep, err = readEndpoint(data); err != nil {
			fmt.Fprintf(stderr, "zpilot: %v\n", err)
			return 1
		}
	}
	d := &desktop{data: data, ep: ep, out: stdout, err: stderr, notify: !*quiet}
	if !*minimized {
		d.openWindow()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.watchEvents(ctx)

	stop := make(chan os.Signal, 2)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	quit := make(chan struct{})
	var quitOnce sync.Once
	doQuit := func() { quitOnce.Do(func() { close(quit) }) }
	trayDone := make(chan struct{})
	if runtime.GOOS == "windows" {
		go func() {
			err := platform.Tray(d.menu, func(action string) {
				switch action {
				case "open":
					d.openWindow()
				case "notify":
					d.mu.Lock()
					d.notify = !d.notify
					d.mu.Unlock()
				case "startup":
					d.toggleStartup()
				case "hide":
					doQuit() // close the tray; the daemon keeps working
				case "quit":
					d.stopDaemon()
					doQuit()
				}
			}, trayDone)
			if err != nil {
				fmt.Fprintf(stderr, "zpilot: tray: %v\n", err)
			}
		}()
	} else {
		fmt.Fprintf(stdout, "ZPilot is running at %s. Press Ctrl+C to stop desktop notifications (the daemon keeps running; 'zpilot stop' stops it).\n", ep.URL)
	}
	select {
	case <-stop:
	case <-quit:
	}
	close(trayDone)
	return 0
}

type desktop struct {
	data string
	ep   server.Endpoint
	out  io.Writer
	err  io.Writer

	mu      sync.Mutex
	notify  bool
	status  string
	lastNot time.Time
}

func (d *desktop) api(method, path string, out any) error {
	req, _ := http.NewRequest(method, d.ep.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+d.ep.Token)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// menu builds the tray menu (called each time it opens).
func (d *desktop) menu() []platform.MenuItem {
	var ov struct {
		Running int `json:"running"`
		Queued  int `json:"queued"`
		Pending int `json:"pending_proposals"`
	}
	status := "ZPilot is not responding"
	if err := d.api("GET", "/api/overview", &ov); err == nil {
		var apr []model.Approval
		_ = d.api("GET", "/api/approvals?pending=1", &apr)
		status = fmt.Sprintf("%d running · %d queued", ov.Running, ov.Queued)
		if n := len(apr) + ov.Pending; n > 0 {
			status += fmt.Sprintf(" · %d need you", n)
		}
	}
	d.mu.Lock()
	notify := d.notify
	d.mu.Unlock()
	return []platform.MenuItem{
		{Action: "open", Label: "Open ZPilot"},
		{Label: status, Disabled: true},
		{Separator: true},
		{Action: "notify", Label: "Notifications", Checked: notify},
		{Action: "startup", Label: "Start at login", Checked: platform.StartupEnabled(platform.StartupName(d.data))},
		{Separator: true},
		{Action: "hide", Label: "Close tray (keep agents running)"},
		{Action: "quit", Label: "Quit ZPilot"},
	}
}

func (d *desktop) toggleStartup() {
	name := platform.StartupName(d.data)
	exe, err := os.Executable()
	if err != nil {
		return
	}
	on := !platform.StartupEnabled(name)
	cmd := fmt.Sprintf(`"%s" --data "%s" desktop --minimized`, exe, d.data)
	if err := platform.SetStartup(name, cmd, on); err != nil {
		platform.Notify("ZPilot", "Could not change start at login: "+err.Error())
		return
	}
	if on {
		platform.Notify("ZPilot", "ZPilot will start when you sign in.")
	}
}

func (d *desktop) stopDaemon() {
	_ = os.WriteFile(filepath.Join(d.data, "stop.request"), []byte("stop"), 0o600)
}

// openWindow shows the dashboard in a chromeless app window (Edge, Chrome or
// Chromium in --app mode with its own profile), falling back to the default
// browser. Each window signs in with a fresh one-time code.
func (d *desktop) openWindow() {
	var lc struct {
		Code string `json:"code"`
	}
	link := d.ep.URL
	if err := d.apiPost("/api/login-code", &lc); err == nil {
		link += "/#code=" + lc.Code
	}
	if b := findAppBrowser(); b != "" {
		profile := filepath.Join(d.data, "desktop-ui")
		cmd := exec.Command(b, "--app="+link, "--user-data-dir="+profile, "--no-first-run", "--no-default-browser-check",
			"--window-size=1320,880", "--class=ZPilot", "--disable-features=Translate")
		if err := cmd.Start(); err == nil {
			go cmd.Wait()
			return
		}
	}
	if err := platform.OpenURL(link); err != nil {
		fmt.Fprintf(d.out, "Open %s in your browser.\n", link)
	}
}

func (d *desktop) apiPost(path string, out any) error {
	req, _ := http.NewRequest("POST", d.ep.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+d.ep.Token)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// findAppBrowser locates a Chromium-based browser that supports --app.
func findAppBrowser() string {
	var candidates []string
	switch runtime.GOOS {
	case "windows":
		for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
			base := os.Getenv(env)
			if base == "" {
				continue
			}
			candidates = append(candidates,
				filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`),
				filepath.Join(base, `Google\Chrome\Application\chrome.exe`),
				filepath.Join(base, `BraveSoftware\Brave-Browser\Application\brave.exe`))
		}
	case "darwin":
		candidates = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
		}
	default:
		for _, n := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge", "brave-browser"} {
			if p, err := exec.LookPath(n); err == nil {
				candidates = append(candidates, p)
			}
		}
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}

// watchEvents follows the daemon's event stream and raises notifications.
func (d *desktop) watchEvents(ctx context.Context) {
	agents := map[string]string{} // username -> display name
	for ctx.Err() == nil {
		var list []struct {
			Username string `json:"username"`
			Name     string `json:"name"`
			Icon     string `json:"icon"`
		}
		if d.api("GET", "/api/agents", &list) == nil {
			for _, a := range list {
				agents[a.Username] = strings.TrimSpace(a.Icon + " " + a.Name)
			}
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", d.ep.URL+"/api/events", nil)
		req.Header.Set("Authorization", "Bearer "+d.ep.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			if resp != nil {
				resp.Body.Close()
			}
			if ep, err := readEndpoint(d.data); err == nil {
				d.ep = ep // the daemon may have restarted on another token
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 4<<20)
		for sc.Scan() {
			line, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			var ev struct {
				Type string          `json:"type"`
				Data json.RawMessage `json:"data"`
			}
			if json.Unmarshal([]byte(line), &ev) == nil {
				d.onEvent(ev.Type, ev.Data, agents)
			}
		}
		resp.Body.Close()
	}
}

func (d *desktop) onEvent(typ string, data json.RawMessage, agents map[string]string) {
	name := func(addr string) string {
		u := strings.TrimPrefix(addr, "@")
		if n := agents[u]; n != "" {
			return n
		}
		return addr
	}
	var title, body string
	switch typ {
	case "approval":
		var a model.Approval
		if json.Unmarshal(data, &a) == nil && a.State == model.ApprovalPending {
			title, body = "🔐 Approval needed: "+name(a.Username), a.Action+": "+a.Detail
		}
	case "proposal":
		var p model.Proposal
		if json.Unmarshal(data, &p) == nil && p.State == model.ProposalPending {
			title, body = "🧭 Proposal from "+name(p.By), strings.ReplaceAll(p.Action, "_", " ")+" "+p.Target+" — "+p.Reason
		}
	case "message":
		var m model.Message
		if json.Unmarshal(data, &m) == nil && m.To == model.AddrUser && m.Status == "" {
			switch m.Kind {
			case model.KindReply:
				title, body = name(m.From)+" replied", m.Text
			case model.KindError:
				title, body = "⚠ "+name(m.From)+" ran into a problem", m.Text
			case model.KindNotice:
				if m.From == "system" {
					title, body = "ZPilot", m.Text
				}
			}
		}
	case "channel_error":
		var e map[string]string
		if json.Unmarshal(data, &e) == nil {
			title, body = "⚠ Channel error", e["error"]
		}
	}
	if title == "" {
		return
	}
	d.mu.Lock()
	enabled := d.notify
	recent := time.Since(d.lastNot) < 2*time.Second && typ == "message"
	if enabled && !recent {
		d.lastNot = time.Now()
	}
	d.mu.Unlock()
	if !enabled || recent {
		return
	}
	body = strings.Join(strings.Fields(body), " ")
	if r := []rune(body); len(r) > 220 {
		body = string(r[:219]) + "…"
	}
	platform.Notify(title, body)
}
