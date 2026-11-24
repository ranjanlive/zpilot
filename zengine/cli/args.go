package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// options are the command-line settings of a run.
type options struct {
	home       string
	workspace  string
	session    string
	sessionSet bool // --session was given (possibly without a value)
	newSession bool
	prompt     string
	promptFile string
	attach     multiFlag
	outputSet  bool
	outputN    int // 0 = complete
	ui         string
	verbose    bool
	quiet      bool
	jsonOut    bool
	serve      bool     // hidden: run as the detached background service for --session
	rest       []string // positional arguments
}

// multiFlag collects a repeatable flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

var (
	numeric   = regexp.MustCompile(`^\d+$`)
	flagStart = regexp.MustCompile(`^-{1,2}[A-Za-z]`)
)

// normalizeArgs rewrites the two optional-value flags into the --name=value
// form the flag package understands:
//
//	--session            (no value, next token is a flag or absent) -> --session=
//	--output             -> --output=all
//	--output 200 / all   -> --output=200 / --output=all
func normalizeArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--session", "-session", "-s":
			if i+1 >= len(args) || flagStart.MatchString(args[i+1]) {
				out = append(out, "--session=")
				continue
			}
		case "--output", "-output", "-o":
			if i+1 < len(args) && (numeric.MatchString(args[i+1]) || args[i+1] == "all") {
				out = append(out, "--output="+args[i+1])
				i++
			} else {
				out = append(out, "--output=all")
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

func parseOptions(name string, args []string, errOut io.Writer) (*options, error) {
	o := &options{}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&o.home, "home", "", "agent home directory (default: the directory of the zagent binary; env ZAGENT_HOME)")
	fs.StringVar(&o.workspace, "workspace", "", "workspace directory (default: the current directory)")
	fs.StringVar(&o.workspace, "w", "", "shorthand for --workspace")
	fs.Func("session", "session name; resumed if it exists, created otherwise. Without a value: the most recent session", func(v string) error {
		o.session, o.sessionSet = v, true
		return nil
	})
	fs.Func("s", "shorthand for --session", func(v string) error { o.session, o.sessionSet = v, true; return nil })
	fs.BoolVar(&o.newSession, "new", false, "start a new session with a generated name")
	fs.StringVar(&o.prompt, "prompt", "", "prompt to run; the agent works until done and then exits")
	fs.StringVar(&o.prompt, "p", "", "shorthand for --prompt")
	fs.StringVar(&o.promptFile, "prompt-file", "", "read the prompt from a file (- for stdin)")
	fs.Var(&o.attach, "attach", "attach an image, audio, video or PDF to the prompt (repeatable)")
	fs.Var(&o.attach, "a", "shorthand for --attach")
	fs.Func("output", "after the run print the transcript of it: --output (complete) or --output N (last N lines). Default: only the final answer", func(v string) error {
		o.outputSet = true
		if v == "all" || v == "" {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("--output takes a positive line count or nothing, got %q", v)
		}
		o.outputN = n
		return nil
	})
	fs.Func("o", "shorthand for --output", func(v string) error { return fs.Lookup("output").Value.Set(v) })
	fs.StringVar(&o.ui, "ui", "headless", "interface: headless | cli | web")
	fs.BoolVar(&o.verbose, "verbose", false, "stream the model's text and tool activity while it works")
	fs.BoolVar(&o.verbose, "v", false, "shorthand for --verbose")
	fs.BoolVar(&o.quiet, "quiet", false, "print nothing but the requested output")
	fs.BoolVar(&o.quiet, "q", false, "shorthand for --quiet")
	fs.BoolVar(&o.jsonOut, "json", false, "print the run's session events as JSON lines instead of text")
	fs.BoolVar(&o.serve, "serve", false, "internal: run as the background service for --session (used by --ui cli/web)")

	if err := fs.Parse(normalizeArgs(args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, err
	}
	o.rest = fs.Args()
	switch o.ui {
	case "headless", "cli", "web":
	default:
		return nil, fmt.Errorf("--ui must be headless, cli or web, got %q", o.ui)
	}
	return o, nil
}

// parseInterspersed parses flags that may appear before, between or after
// positional arguments (the flag package alone stops at the first positional).
func parseInterspersed(fs *flag.FlagSet, args []string) (positional []string, err error) {
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}
