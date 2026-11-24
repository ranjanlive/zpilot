package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/config"
)

// runWeb makes sure the session has a running engine, prints the browser URL
// (the access token rides in the fragment) and opens it. The engine keeps
// running in the background after this command returns.
func runWeb(o *options, home config.Home, workspace, sessionID, prompt string, attach []string, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := ensureService(ctx, o, home, workspace, sessionID, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "zagent: %v\n", err)
		return exitFailed
	}
	if prompt != "" || len(attach) > 0 {
		if err := c.Prompt(ctx, prompt, attach...); err != nil {
			fmt.Fprintf(stderr, "zagent: could not send the prompt: %v\n", err)
			return exitFailed
		}
	}
	url := c.WebURL()
	fmt.Fprintf(stdout, "Session %s is running (pid %d).\nOpen: %s\n", sessionID, c.PID, url)
	fmt.Fprintf(stdout, "Leave it running in the background; stop it with: zagent --session %s --ui cli  then /quit\n", sessionID)
	if err := openBrowser(url); err != nil {
		fmt.Fprintf(stderr, "(could not open a browser automatically: %v)\n", err)
	}
	return exitOK
}

// runCLI attaches the terminal interface. Implemented in tui.go.
func runCLI(o *options, home config.Home, workspace, sessionID, prompt string, attach []string, stdout, stderr io.Writer) int {
	if !interactiveTerminal() {
		fmt.Fprintln(stderr, "zagent: --ui cli needs an interactive terminal (stdin and stdout must be a TTY). In scripts use --prompt (headless) or --ui web.")
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := ensureService(ctx, o, home, workspace, sessionID, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "zagent: %v\n", err)
		return exitFailed
	}
	if prompt != "" || len(attach) > 0 {
		if err := c.Prompt(ctx, prompt, attach...); err != nil {
			fmt.Fprintf(stderr, "zagent: could not send the prompt: %v\n", err)
			return exitFailed
		}
	}
	return runTUI(c, sessionID, stdout, stderr)
}
