//go:build !windows

package platform

import (
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

type Lock struct{ f *os.File }

func Acquire(path string) (*Lock, error) {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, fmt.Errorf("ZPilot daemon is already running: %w", e)
	}
	return &Lock{f}, nil
}
func (l *Lock) Close()                    { l.f.Close() }
func Replace(from, to string) error       { return os.Rename(from, to) }
func Configure(cmd *exec.Cmd)             { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func Track(cmd *exec.Cmd) (func(), error) { return func() {}, nil }

// TrackLimits is a no-op on Unix: limits are applied when the command is
// built (see core.ProcessEngine), because they must be set before exec.
func TrackLimits(cmd *exec.Cmd, l Limits) (func(), error) { return func() {}, nil }
func Kill(cmd *exec.Cmd) {
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
func OpenURL(url string) error {
	var c *exec.Cmd
	if _, e := exec.LookPath("xdg-open"); e == nil {
		c = exec.Command("xdg-open", url)
	} else {
		c = exec.Command("open", url)
	}
	if e := c.Start(); e != nil {
		return e
	}
	go c.Wait()
	return nil
}

// SetStartup creates an XDG autostart entry (Linux) or a LaunchAgent (macOS).
func SetStartup(name, command string, on bool) error {
	p, content := startupFile(name, command)
	if p == "" {
		return fmt.Errorf("start at login is not supported on this system")
	}
	if !on {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

// StartupEnabled reports whether the login item exists.
func StartupEnabled(name string) bool {
	p, _ := startupFile(name, "")
	_, err := os.Stat(p)
	return p != "" && err == nil
}

// StartupName is a stable login-item name for a data directory.
func StartupName(data string) string {
	h := fnv.New32a()
	h.Write([]byte(data))
	return fmt.Sprintf("zpilot-%08x", h.Sum32())
}

func startupFile(name, command string) (string, string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}
	if runtime.GOOS == "darwin" {
		args := ""
		for _, a := range splitCommand(command) {
			args += "<string>" + xmlEscape(a) + "</string>"
		}
		return filepath.Join(home, "Library", "LaunchAgents", "com."+name+".plist"),
			`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>Label</key><string>com.` + name +
				`</string><key>ProgramArguments</key><array>` + args + `</array><key>RunAtLoad</key><true/></dict></plist>`
	}
	return filepath.Join(home, ".config", "autostart", name+".desktop"),
		"[Desktop Entry]\nType=Application\nName=ZPilot\nExec=" + command + "\nX-GNOME-Autostart-enabled=true\n"
}

// splitCommand splits a command line on spaces, honouring double quotes.
func splitCommand(s string) []string {
	var out []string
	var cur strings.Builder
	q := false
	for _, r := range s {
		switch {
		case r == '"':
			q = !q
		case r == ' ' && !q:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
func Protect(b []byte) ([]byte, error)   { return b, nil }
func Unprotect(b []byte) ([]byte, error) { return b, nil }

func OpenTerminal(exe string, args []string) error {
	if _, e := exec.LookPath("x-terminal-emulator"); e == nil {
		c := exec.Command("x-terminal-emulator", append([]string{"-e", exe}, args...)...)
		if e = c.Start(); e != nil {
			return e
		}
		go c.Wait()
		return nil
	}
	return fmt.Errorf("open a terminal and run %s with --ui cli", exe)
}
