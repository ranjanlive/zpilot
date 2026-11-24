//go:build windows

package cli

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

const (
	detachedProcess      = 0x00000008
	createNewProcessGrp  = 0x00000200
	createNoWindowDetach = 0x08000000
)

// detach makes the child survive its parent: no console, its own process group.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcessGrp | createNoWindowDetach,
		HideWindow:    true,
	}
}

type sigWatch struct{ c chan os.Signal }

func notifySignals() sigWatch {
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	return sigWatch{c}
}

func (s sigWatch) stop() { signal.Stop(s.c) }

// openBrowser opens url in the user's default browser.
func openBrowser(url string) error {
	// The '&' characters of a URL are meaningful to cmd; rundll32 avoids the shell.
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}
