//go:build !windows

package platform

import (
	"os/exec"
	"runtime"
	"strings"
)

// Notify shows a native desktop notification: Notification Center on macOS,
// notify-send (libnotify) on Linux. It silently does nothing if neither exists.
func Notify(title, body string) {
	switch runtime.GOOS {
	case "darwin":
		q := func(s string) string { return `"` + strings.NewReplacer(`\`, `\`, `"`, `\"`).Replace(s) + `"` }
		_ = exec.Command("osascript", "-e", "display notification "+q(body)+" with title "+q(title)).Start()
	default:
		if p, err := exec.LookPath("notify-send"); err == nil {
			_ = exec.Command(p, "--app-name=ZPilot", title, body).Start()
		}
	}
}
