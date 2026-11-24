//go:build windows

package tools

import (
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
)

const (
	createNoWindow        = 0x08000000
	createNewProcessGroup = 0x00000200
)

// shellCommand runs line in PowerShell (preferring PowerShell 7 when
// installed), forcing UTF-8 so output is not mangled by the OEM code page.
// It falls back to cmd.exe when PowerShell is unavailable.
func shellCommand(line string) (*exec.Cmd, error) {
	for _, name := range []string{"pwsh.exe", "powershell.exe"} {
		if path, err := exec.LookPath(name); err == nil {
			script := "[Console]::OutputEncoding=[System.Text.Encoding]::UTF8; $OutputEncoding=[System.Text.Encoding]::UTF8; " + line
			return exec.Command(path, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script), nil
		}
	}
	if path, err := exec.LookPath("cmd.exe"); err == nil {
		cmd := exec.Command(path)
		// cmd.exe parses its own command line; pass it verbatim.
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: fmt.Sprintf(`cmd.exe /D /S /C "chcp 65001 >nul & %s"`, line)}
		return cmd, nil
	}
	return nil, fmt.Errorf("no shell found (looked for pwsh, powershell and cmd)")
}

// ShellName describes the shell for tool descriptions.
func ShellName() string {
	if _, err := exec.LookPath("pwsh.exe"); err == nil {
		return "PowerShell 7 (pwsh); chain commands with ';' or '&&'"
	}
	if _, err := exec.LookPath("powershell.exe"); err == nil {
		return "Windows PowerShell 5.1 (no '&&' or '||'; chain with ';')"
	}
	return "cmd.exe"
}

func configureProc(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow | createNewProcessGroup
}

// killTree terminates the process and all of its descendants.
func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	k := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	k.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	if err := k.Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}
