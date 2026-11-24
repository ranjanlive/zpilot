// Command zpilot is ZPilot: a platform for building teams of AI agents
// that run on your own machine or server, talk to each other by @username,
// work on schedules and answer on Telegram or webhooks.
//
// One binary contains everything: the daemon with its web dashboard, the
// command line, the agent engine (ZEngine) and the agents' collaboration
// tools.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ranjanlive/zpilot/internal/agenttool"
	"github.com/ranjanlive/zpilot/zengine/cli"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "0.2.0-dev"

const usage = `ZPilot - build teams of AI agents that run on your machine or server

Usage: zpilot <command> [arguments]

Daemon
  serve [--listen ADDR] [--open]      run the daemon and dashboard in the foreground
        [--tls-cert F --tls-key F] [--insecure-http] [--tray]
  start [--listen ADDR]               start the daemon in the background
  stop                                stop the background daemon
  status                              show whether the daemon runs and what it is doing
  ui                                  open the dashboard in your browser
  desktop [--minimized]               desktop app: app window, tray icon, notifications

Agents and conversations
  agents list | show @a | create | update @a | delete @a | enable @a | disable @a
  send @agent "message" [--thread ID] [--wait]
  threads list [--agent @a] | show ID | delete ID
  runs list [--agent @a] | log ID | stop ID

Configuration
  models list | add | delete ID | test ID | default ID
  tools search [TEXT] | list | install ID|PATH [--allow-unverified] | remove ID
  skills list | add DIR | new NAME --description D --file F | remove NAME
  workflows list | show ID | import FILE | run ID [--input T] | runs | cancel RUN | delete ID
  schedules list | add | delete ID | run ID
  channels list | add-telegram | add-discord | add-email | add-whatsapp | add-webhook
           | allow ID SENDER | disallow ID SENDER | delete ID
  approvals list | approve ID | reject ID       (commands apr_… and manager proposals prp_…)
  users list | add NAME --role R | password NAME | role NAME --role R | delete NAME
  admin password | admin clear-password
  audit [--limit N]

Other
  engine ...                          run the agent engine directly (zagent command line)
  version

Global flags
  --data DIR      data directory (default ~/.zpilot, env ZPILOT_DATA)

Run 'zpilot <command> --help' for a command's flags.
`

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// The engine re-executes this binary for its own background service.
	cli.SelfArgs = []string{"engine"}

	data, args := extractDataFlag(args)
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return 0
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "--version":
		fmt.Fprintf(stdout, "zpilot %s (engine %s)\n", version, cli.Version())
		return 0
	case "engine":
		return cli.Run(rest, stdout, stderr)
	case "agent-tool":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "usage: zpilot agent-tool NAME")
			return 2
		}
		return agenttool.Main(rest[0], stdin, stdout)
	case "serve":
		return cmdServe(data, rest, stdout, stderr)
	case "start":
		return cmdStart(data, rest, stdout, stderr)
	case "desktop":
		return cmdDesktop(data, rest, stdout, stderr)
	}
	c := &ctx{data: data, stdin: stdin, out: stdout, err: stderr}
	handlers := map[string]func([]string) int{
		"stop": c.stop, "status": c.status, "ui": c.ui,
		"agents": c.agents, "send": c.send, "threads": c.threads, "runs": c.runs,
		"models": c.models, "tools": c.tools, "skills": c.skills, "schedules": c.schedules,
		"channels": c.channels, "approvals": c.approvals, "admin": c.admin, "audit": c.audit,
		"users": c.users, "workflows": c.workflows,
	}
	h, ok := handlers[cmd]
	if !ok {
		fmt.Fprintf(stderr, "zpilot: unknown command %q (run 'zpilot help')\n", cmd)
		return 2
	}
	return h(rest)
}

// extractDataFlag pulls --data DIR out of args wherever it appears.
func extractDataFlag(args []string) (string, []string) {
	data := os.Getenv("ZPILOT_DATA")
	if data == "" {
		data = os.Getenv("ZAUTO_DATA") // name used before the rename to ZPilot
	}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--data" && i+1 < len(args):
			data = args[i+1]
			i++
		case len(a) > 7 && a[:7] == "--data=":
			data = a[7:]
		default:
			rest = append(rest, a)
		}
	}
	if data == "" {
		if home, err := os.UserHomeDir(); err == nil {
			data = filepath.Join(home, ".zpilot")
			migrateLegacyData(data, filepath.Join(home, ".zautomation"))
		} else {
			data = ".zpilot"
		}
	}
	abs, err := filepath.Abs(data)
	if err == nil {
		data = abs
	}
	return data, rest
}

// migrateLegacyData moves the data directory used before the product was
// renamed (~/.zautomation) to the new default, once, if the new one does not
// exist yet. A failed move (for example a daemon still holding the old
// directory) leaves both untouched and is reported, never silently ignored.
func migrateLegacyData(data, legacy string) {
	if _, err := os.Stat(data); err == nil {
		return
	}
	if fi, err := os.Stat(legacy); err != nil || !fi.IsDir() {
		return
	}
	if err := os.Rename(legacy, data); err != nil {
		fmt.Fprintf(os.Stderr, "zpilot: could not move %s to %s: %v\n", legacy, data, err)
		return
	}
	fmt.Fprintf(os.Stderr, "zpilot: moved your data from %s to %s\n", legacy, data)
}
