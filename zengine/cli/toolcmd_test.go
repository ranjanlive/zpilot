package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI2(t *testing.T, args ...string) (code int, out, errOut string) {
	t.Helper()
	var so, se bytes.Buffer
	code = run(args, &so, &se)
	return code, so.String(), se.String()
}

func TestToolsListAndRun(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	if code, _, e := runCLI2(t, "init", "--home", home); code != 0 {
		t.Fatalf("init: %d %s", code, e)
	}
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "marker.txt"), []byte("hi"), 0o644)

	code, out, _ := runCLI2(t, "tools", "list", "--home", home)
	if code != 0 || !strings.Contains(out, "view_file") || !strings.Contains(out, "built-in") {
		t.Fatalf("list: %d\n%s", code, out)
	}

	// runs a real tool through the same registry and validation the agent uses
	code, out, _ = runCLI2(t, "tools", "run", "list_dir", "--home", home, "--workspace", ws, "--args", "{}")
	if code != 0 || !strings.Contains(out, "what the model receives") || !strings.Contains(out, "marker.txt") {
		t.Fatalf("run: %d\n%s", code, out)
	}

	// flags may come before the tool name too
	code, out, _ = runCLI2(t, "tools", "run", "--home", home, "--workspace", ws, "view_file", "--args", `{"path":"marker.txt"}`)
	if code != 0 || !strings.Contains(out, "hi") {
		t.Fatalf("view_file: %d\n%s", code, out)
	}

	// the model-facing errors are shown verbatim and set the exit code
	code, out, _ = runCLI2(t, "tools", "run", "view_file", "--home", home, "--workspace", ws, "--args", `{"pth":"x"}`)
	if code != exitFailed || !strings.Contains(out, `unknown argument "pth"`) || !strings.Contains(out, "ERROR") {
		t.Fatalf("bad args: %d\n%s", code, out)
	}
	code, out, _ = runCLI2(t, "tools", "run", "nope", "--home", home, "--workspace", ws)
	if code != exitFailed || !strings.Contains(out, `unknown tool "nope"`) {
		t.Fatalf("unknown tool: %d\n%s", code, out)
	}
	if code, _, e := runCLI2(t, "tools", "run", "list_dir", "--home", home, "--args", "{nope"); code != exitUsage || !strings.Contains(e, "not valid JSON") {
		t.Fatalf("bad json: %d %s", code, e)
	}
	if code, _, _ := runCLI2(t, "tools"); code != exitUsage {
		t.Fatalf("no subcommand: %d", code)
	}
}
