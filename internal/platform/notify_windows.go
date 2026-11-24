//go:build windows

package platform

// Notify shows a native notification. On Windows it is a tray balloon,
// which Windows 10/11 present as a toast; the tray must be running.
func Notify(title, body string) { Balloon(title, body) }
