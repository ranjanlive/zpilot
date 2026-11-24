package cli

import (
	"os"

	"golang.org/x/term"
)

// interactiveTerminal reports whether both stdin and stdout are terminals,
// which the terminal UI requires.
func interactiveTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}
