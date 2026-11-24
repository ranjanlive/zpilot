//go:build !windows

package tools

import (
	"os"
	"os/exec"
	"syscall"
)

// shellCommand builds the command that runs line through the user's shell.
func shellCommand(line string) (*exec.Cmd, error) {
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	if _, err := os.Stat(sh); err != nil {
		sh = "/bin/sh"
	}
	return exec.Command(sh, "-c", line), nil
}

// ShellName describes the shell for tool descriptions.
func ShellName() string { return "the user's POSIX shell (sh -c)" }

// configureProc puts the child in its own process group so the whole tree can
// be signalled together.
func configureProc(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// A negative PID signals the whole process group.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
