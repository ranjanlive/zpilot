//go:build !windows

package cli

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// detach makes the child survive its parent: its own session, no controlling terminal.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

type sigWatch struct{ c chan os.Signal }

func notifySignals() sigWatch {
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	return sigWatch{c}
}

func (s sigWatch) stop() { signal.Stop(s.c) }

// openBrowser opens url in the user's default browser.
func openBrowser(url string) error {
	for _, name := range []string{"xdg-open", "open"} {
		if p, err := exec.LookPath(name); err == nil {
			return exec.Command(p, url).Start()
		}
	}
	return exec.ErrNotFound
}
