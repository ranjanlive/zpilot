package cli

import (
	"fmt"
	"io"

	"github.com/ranjanlive/zpilot/zengine/internal/control"
	"github.com/ranjanlive/zpilot/zengine/internal/tui"
)

// runTUI shows the terminal interface for a running engine. Leaving it never
// stops the agent: it keeps working in its background process.
func runTUI(c *control.Client, sessionID string, stdout, stderr io.Writer) int {
	newSession, err := tui.Run(c)
	if err != nil {
		fmt.Fprintf(stderr, "zagent: %v\n", err)
		return exitFailed
	}
	if newSession {
		fmt.Fprintln(stdout, "Start a new session with: zagent --new --ui cli")
		return exitOK
	}
	fmt.Fprintf(stdout, "Detached. Session %s keeps running in the background; reattach with: zagent --session %s --ui cli\n", sessionID, sessionID)
	return exitOK
}
