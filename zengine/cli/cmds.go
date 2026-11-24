package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/config"
	"github.com/ranjanlive/zpilot/zengine/internal/contextmgr"
	"github.com/ranjanlive/zpilot/zengine/internal/engine"
	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/provider"
	"github.com/ranjanlive/zpilot/zengine/internal/render"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
)

func homeFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "agent home directory")
	return fs, home
}

func cmdInit(args []string, stdout, stderr io.Writer) int {
	fs, homeFlag := homeFlagSet("init", stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	home, err := config.ResolveHome(*homeFlag)
	if err != nil {
		fmt.Fprintf(stderr, "zagent init: %v\n", err)
		return exitUsage
	}
	created, err := home.Init()
	if err != nil {
		fmt.Fprintf(stderr, "zagent init: %v\n", err)
		return exitFailed
	}
	if len(created) == 0 {
		fmt.Fprintf(stdout, "%s is already set up; nothing to create.\n", home.Root)
		return exitOK
	}
	fmt.Fprintf(stdout, "Agent home: %s\n", home.Root)
	for _, c := range created {
		fmt.Fprintf(stdout, "  created %s\n", c)
	}
	fmt.Fprintf(stdout, "\nNext: edit %s (PROVIDER, BASE_URL, API_KEY, MODEL), then run 'zagent doctor'.\n", home.EnvFile())
	return exitOK
}

func mask(s string) string {
	switch {
	case s == "":
		return "(not set)"
	case len(s) <= 8:
		return "****"
	}
	return s[:4] + "…" + s[len(s)-2:]
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	fs, homeFlag := homeFlagSet("doctor", stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	home, env, cfg, ok := loadHome(*homeFlag, stderr)
	if !ok {
		return exitUsage
	}
	settings := env.ProviderSettings(cfg)
	endpoint, _ := provider.Endpoint(settings)
	fmt.Fprintf(stdout, "Home:        %s\n", home.Root)
	fmt.Fprintf(stdout, "Provider:    %s\n", env.Provider)
	fmt.Fprintf(stdout, "Endpoint:    %s\n", endpoint)
	fmt.Fprintf(stdout, "Model:       %s\n", env.Model)
	fmt.Fprintf(stdout, "API key:     %s\n", mask(env.APIKey))
	fmt.Fprintf(stdout, "Media input: vision=%s audio=%s pdf=%s video=%s\n", yesNo(env.Caps.Vision), yesNo(env.Caps.Audio), yesNo(env.Caps.PDF), yesNo(env.Caps.Video))
	fmt.Fprintf(stdout, "Context:     limit %d tokens, compact at %d, request body limit %s\n", env.ContextLimit, env.CompactAt, llm.HumanBytes(int64(cfg.BodyLimitBytes())))
	registry, toolProblems := engine.DefaultRegistry(cfg, home)
	fmt.Fprintf(stdout, "Tools:       %s\n", strings.Join(registry.Names(), ", "))
	for _, p := range toolProblems {
		fmt.Fprintf(stdout, "  ! external tool not loaded: %s\n", p)
	}
	if len(cfg.Tools.Disabled) > 0 {
		fmt.Fprintf(stdout, "Disabled:    %s\n", strings.Join(cfg.Tools.Disabled, ", "))
	}
	fmt.Fprintf(stdout, "Commands:    run_command permission = %s\n\n", cfg.Permissions.RunCommand)

	client, err := provider.New(settings)
	if err != nil {
		fmt.Fprintf(stderr, "doctor: %v\n", err)
		return exitUsage
	}
	req := provider.Request{
		System: "You are a connectivity check.", Model: env.Model, MaxOutputTokens: 64, Caps: env.Caps,
		Messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "Reply with the single word: ok"}}}},
	}
	built, err := client.Adapter.Build(req)
	if err != nil {
		fmt.Fprintf(stderr, "doctor: cannot build a request: %v\n", err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := client.Send(ctx, built, nil)
	if err != nil {
		pe := provider.AsError(err)
		fmt.Fprintf(stdout, "Connection:  FAILED (%s) after %s\n  %s\n", pe.Kind, time.Since(start).Round(time.Millisecond), pe.Message)
		switch pe.Kind {
		case provider.ErrAuth:
			fmt.Fprintln(stdout, "  Hint: the API key was rejected. Check API_KEY, and provider.auth_style in config.json if your gateway expects a different header.")
		case provider.ErrBadRequest:
			fmt.Fprintln(stdout, "  Hint: check MODEL and BASE_URL. For custom providers BASE_URL must be the exact request URL.")
		case provider.ErrTooLarge, provider.ErrContextLimit:
			fmt.Fprintln(stdout, "  Hint: unexpected for a tiny request; check BASE_URL.")
		}
		return exitFailed
	}
	u := ""
	if resp.Usage != nil {
		u = fmt.Sprintf(", %d prompt / %d answer tokens", resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	} else {
		u = ", no usage reported (token counts will be estimated as bytes/4)"
	}
	fmt.Fprintf(stdout, "Connection:  OK in %s%s\nModel said:  %q\n", time.Since(start).Round(time.Millisecond), u, strings.TrimSpace(resp.Message.Text()))
	return exitOK
}

func cmdSessions(args []string, stdout, stderr io.Writer) int {
	fs, homeFlag := homeFlagSet("sessions", stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	home, err := config.ResolveHome(*homeFlag)
	if err != nil {
		fmt.Fprintf(stderr, "zagent sessions: %v\n", err)
		return exitUsage
	}
	metas, err := session.List(home.SessionsDir())
	if err != nil {
		fmt.Fprintf(stderr, "zagent sessions: %v\n", err)
		return exitFailed
	}
	if len(metas) == 0 {
		fmt.Fprintf(stdout, "No sessions in %s.\n", home.SessionsDir())
		return exitOK
	}
	for _, m := range metas {
		state := ""
		if li := session.ReadLock(home.SessionDir(m.ID)); li != nil {
			state = fmt.Sprintf(" [running, pid %d]", li.PID)
		}
		title := m.Title
		if title == "" {
			title = "(no prompt yet)"
		}
		fmt.Fprintf(stdout, "%-28s %s  %s%s\n    %s\n", m.ID, m.UpdatedAt.Local().Format("2006-01-02 15:04"), title, state, m.Workspace)
	}
	return exitOK
}

func cmdShow(args []string, stdout, stderr io.Writer) int {
	fs, homeFlag := homeFlagSet("show", stderr)
	var n int
	var full, debug bool
	fs.IntVar(&n, "output", 0, "show only the last N lines")
	fs.IntVar(&n, "o", 0, "shorthand for --output")
	fs.BoolVar(&full, "full", false, "do not truncate tool output")
	fs.BoolVar(&debug, "debug", false, "include usage, status and system events")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "usage: zagent show SESSION [--output N] [--full] [--debug]")
		return exitUsage
	}
	home, err := config.ResolveHome(*homeFlag)
	if err != nil {
		fmt.Fprintf(stderr, "zagent show: %v\n", err)
		return exitUsage
	}
	st, err := session.Open(home.SessionDir(positional[0]))
	if err != nil {
		fmt.Fprintf(stderr, "zagent show: %v\n", err)
		return exitFailed
	}
	defer st.Close()
	opts := render.Options{ToolOutputChars: 600, Debug: debug}
	if full {
		opts.ToolOutputChars = 0
	}
	for _, l := range render.LastLines(render.Transcript(st.Events(), opts), n) {
		fmt.Fprintln(stdout, l)
	}
	if view := contextmgr.Build(st.Events()); debug {
		fmt.Fprintf(stdout, "\n[context: %d message(s), %d inline attachment(s), turn %d]\n", len(view.Messages), len(view.Attachments), view.Turn)
	}
	return exitOK
}
