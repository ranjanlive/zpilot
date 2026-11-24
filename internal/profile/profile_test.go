package profile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ranjanlive/zpilot/internal/library"
	"github.com/ranjanlive/zpilot/internal/model"
)

func cfgOf(t *testing.T, l model.Level) map[string]any {
	t.Helper()
	b, err := EngineConfig(&model.Agent{Level: l, MaxToolRounds: 50, TimeoutMinutes: 10}, "")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func disabled(m map[string]any) []string {
	var out []string
	for _, v := range m["tools"].(map[string]any)["disabled"].([]any) {
		out = append(out, v.(string))
	}
	return out
}

func TestLevelsMapToEngineConfig(t *testing.T) {
	r := cfgOf(t, model.LevelRestricted)
	for _, tool := range []string{"write_file", "edit_file", "run_command", "terminal"} {
		if !slices.Contains(disabled(r), tool) {
			t.Errorf("restricted: %s not disabled", tool)
		}
	}
	s := cfgOf(t, model.LevelStandard)
	if slices.Contains(disabled(s), "write_file") || !slices.Contains(disabled(s), "run_command") {
		t.Errorf("standard: %v", disabled(s))
	}
	tr := cfgOf(t, model.LevelTrusted)
	p := tr["permissions"].(map[string]any)
	if len(disabled(tr)) != 0 || p["run_command"] != "allow" || p["write_outside_workspace"] != false {
		t.Errorf("trusted: %v", tr)
	}
	f := cfgOf(t, model.LevelFull)["permissions"].(map[string]any)
	if f["write_outside_workspace"] != true || f["read_outside_workspace"] != true {
		t.Errorf("full: %v", f)
	}
	if _, err := EngineConfig(&model.Agent{Level: "root"}, ""); err == nil {
		t.Error("unknown level accepted")
	}
}

func TestMaterialize(t *testing.T) {
	data := t.TempDir()
	lib := library.New(data)
	lib.SaveSkill(library.Skill{Name: "style", Description: "Use for writing", Body: "Short sentences."})
	a := &model.Agent{ID: "agt_1", Username: "writer", Name: "Writer", Level: model.LevelStandard, Skills: []string{"style", "missing"},
		Contacts: []string{"editor"}, MaxToolRounds: 10, TimeoutMinutes: 5}
	p, warn, err := Materialize(a, Options{DataDir: data, Library: lib, Exe: "/bin/zpilot", DaemonURL: "http://127.0.0.1:9", AgentToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if len(warn) != 1 || !strings.Contains(warn[0], "missing") {
		t.Fatalf("warnings: %v", warn)
	}
	sys, _ := os.ReadFile(filepath.Join(p.Home, "system_instruction.md"))
	for _, want := range []string{"You are @writer", "ask_agent", "Security level: standard", "cannot run shell commands"} {
		if !strings.Contains(string(sys), want) {
			t.Errorf("system instruction lacks %q", want)
		}
	}
	for _, f := range []string{"skills/style/SKILL.md", "tools/ask_agent/ask_agent.json", "tools/list_agents/list_agents.json", ".env"} {
		if _, err := os.Stat(filepath.Join(p.Home, f)); err != nil {
			t.Errorf("%s missing", f)
		}
	}
	if _, err := os.Stat(filepath.Join(p.Home, "tools", "manage_propose")); err == nil {
		t.Error("manager tool given to a normal agent")
	}
	// removing contacts removes the collaboration tools
	a.Contacts = nil
	Materialize(a, Options{DataDir: data, Library: lib, Exe: "/bin/zpilot", DaemonURL: "http://127.0.0.1:9", AgentToken: "tok"})
	if _, err := os.Stat(filepath.Join(p.Home, "tools", "ask_agent")); err == nil {
		t.Error("collaboration tools kept without contacts")
	}
}
