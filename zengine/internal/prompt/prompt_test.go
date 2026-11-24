package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/config"
	"github.com/ranjanlive/zpilot/zengine/internal/llm"
)

func setup(t *testing.T) (config.Home, string) {
	t.Helper()
	root := t.TempDir()
	h := config.Home{Root: filepath.Join(root, "home")}
	if _, err := h.Init(); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(root, "ws")
	os.MkdirAll(ws, 0o755)
	return h, ws
}

func TestBuildContainsEnvironmentAndCapabilities(t *testing.T) {
	h, ws := setup(t)
	out := Build(Inputs{Home: h, Workspace: ws, Model: "gemini-x", Caps: llm.Capabilities{Vision: true, PDF: true}, Now: time.Date(2026, 10, 11, 18, 0, 0, 0, time.UTC)})
	for _, want := range []string{"You are zagent", "# Environment", ws, h.ScratchDir(), "2026-10-11", "gemini-x", "images, PDF"} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if strings.Contains(out, "# Skills") || strings.Contains(out, "# Permanent memory") {
		t.Errorf("empty sections must be omitted:\n%s", out)
	}
	out = Build(Inputs{Home: h, Workspace: ws})
	if !strings.Contains(out, "text only") {
		t.Errorf("no-capability wording missing")
	}
}

func TestBuildIsStableForIdenticalInputs(t *testing.T) {
	h, ws := setup(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	a := Build(Inputs{Home: h, Workspace: ws, Now: now})
	b := Build(Inputs{Home: h, Workspace: ws, Now: now.Add(10 * time.Hour)}) // same day => identical (cache friendly)
	if a != b {
		t.Fatal("the prompt must not change within a day")
	}
}

func TestCustomInstructionAndFallbacks(t *testing.T) {
	h, ws := setup(t)
	os.WriteFile(h.SystemFile(), []byte("You are a pirate.\n"), 0o644)
	if out := Build(Inputs{Home: h, Workspace: ws}); !strings.HasPrefix(out, "You are a pirate.") {
		t.Fatalf("custom instruction ignored: %.60s", out)
	}
	os.WriteFile(h.SystemFile(), []byte("   \n"), 0o644)
	if out := Build(Inputs{Home: h, Workspace: ws}); !strings.Contains(out, "You are zagent") {
		t.Fatal("blank file must fall back to the default")
	}
	os.Remove(h.SystemFile())
	if out := Build(Inputs{Home: h, Workspace: ws}); !strings.Contains(out, "You are zagent") {
		t.Fatal("missing file must fall back to the default")
	}
}

func TestMemoryAndSkillsAreIncluded(t *testing.T) {
	h, ws := setup(t)
	os.WriteFile(filepath.Join(h.MemoryDir(), "prefs.md"), []byte("User prefers tabs\nmore detail"), 0o644)
	os.MkdirAll(filepath.Join(ws, ".agent", "memory"), 0o755)
	os.WriteFile(filepath.Join(ws, ".agent", "memory", "notes.md"), []byte("# Build\nrun make"), 0o644)
	os.MkdirAll(filepath.Join(h.SkillsDir(), "deploy"), 0o755)
	os.WriteFile(filepath.Join(h.SkillsDir(), "deploy", "SKILL.md"), []byte("---\nname: deploy\ndescription: Use when shipping a release.\n---\nsteps"), 0o644)

	out := Build(Inputs{Home: h, Workspace: ws})
	for _, want := range []string{"# Permanent memory", "User prefers tabs", "# Workspace memory", "run make", "# Skills", "- deploy: Use when shipping a release."} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "steps") {
		t.Error("skill bodies must be loaded on demand, not inlined")
	}
	// over budget: names + summaries only
	out = Build(Inputs{Home: h, Workspace: ws, MemoryBudget: 5})
	if strings.Contains(out, "more detail") || !strings.Contains(out, "- prefs: User prefers tabs (read it with the memory tool)") {
		t.Errorf("budget not applied:\n%s", out)
	}
}
