// Package cli is the zengine command line (formerly cmd/zagent): one engine that can run headless,
// in a terminal UI, or behind a web UI, against Gemini, OpenAI-compatible and
// Anthropic APIs (or any custom endpoint).
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

// version is overridden at build time: -ldflags "-X main.version=1.2.3".
var version = "0.1.1"

// Exit codes are part of the headless contract so scripts can branch on them.
const (
	exitOK          = 0
	exitFailed      = 1   // the agent ran but the turn ended in an error
	exitUsage       = 2   // bad flags or configuration
	exitLocked      = 3   // the session is owned by another process
	exitInterrupted = 130 // stopped by the user (Ctrl-C)
)

const usageText = `zagent - a configurable agent engine

Usage:
  zagent [flags]                         run (headless with --prompt, or open a UI with --ui)
  zagent init [--home DIR]               create the home directory with template .env and config.json
  zagent doctor [--home DIR]             check configuration and test the model endpoint
  zagent sessions [--home DIR]           list sessions
  zagent show SESSION [--output N]       print a session's transcript
  zagent tools list|run NAME          list tools / run one exactly as the agent would (for tool authors)
  zagent version

Common flags:
  --home DIR          agent home (default: where the zagent binary lives; env ZAGENT_HOME)
  --workspace DIR     workspace (default: current directory)    -w
  --session [NAME]    resume or create NAME; with no name, the most recent session  -s
  --new               start a new session with a generated name
  --prompt TEXT       run this prompt, wait until the agent is done, then exit      -p
  --prompt-file FILE  read the prompt from FILE ('-' = stdin)
  --attach FILE       attach an image/audio/video/PDF (repeatable)                  -a
  --output [N]        print this run's transcript (all, or the last N lines)        -o
  --ui MODE           headless (default) | cli | web
  --verbose, --quiet  stream activity / print only the requested output
  --json              print this run's session events as JSON lines

Exit codes: 0 ok, 1 the turn failed, 2 usage or configuration error, 3 session in use, 130 interrupted.
`

// Run executes the engine command line with args (without the program name)
// and returns the process exit code. ZPilot runs it as `zpilot engine ...`.
func Run(args []string, stdout, stderr io.Writer) int { return run(args, stdout, stderr) }

// Version reports the engine version.
func Version() string { return version }

// SelfArgs is prepended to the arguments whenever the engine re-executes its
// own binary (the detached session service). The host binary sets it, e.g.
// []string{"engine"} when the engine is embedded as a subcommand.
var SelfArgs []string

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "help", "-h", "--help", "-help":
			fmt.Fprint(stdout, usageText)
			return exitOK
		case "version", "--version":
			fmt.Fprintf(stdout, "zagent %s\n", version)
			return exitOK
		case "init":
			return cmdInit(args[1:], stdout, stderr)
		case "doctor":
			return cmdDoctor(args[1:], stdout, stderr)
		case "sessions":
			return cmdSessions(args[1:], stdout, stderr)
		case "show":
			return cmdShow(args[1:], stdout, stderr)
		case "tools":
			return cmdTools(args[1:], stdout, stderr)
		}
	}
	o, err := parseOptions("zagent", args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usageText)
			return exitOK
		}
		fmt.Fprintf(stderr, "zagent: %v\n(run 'zagent help' for usage)\n", err)
		return exitUsage
	}
	return cmdRun(o, stdout, stderr)
}
