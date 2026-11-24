package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
)

var memoryName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// MemoryDirs returns the permanent (home) and temporary (workspace) memory directories.
func MemoryDirs(env *Env) (permanent, workspace string) {
	return env.Home.MemoryDir(), filepath.Join(env.Workspace, ".agent", "memory")
}

type memoryTool struct{}

func (memoryTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "memory",
		Description: "Save and recall notes that outlive the conversation. " +
			"scope=workspace is for this project (stored in <workspace>/.agent/memory); use it freely for progress notes, decisions and facts you will need later. " +
			"scope=permanent is shared across all projects (stored in the agent home): save to it ONLY when the user explicitly asks you to remember something, or when something is critically important for every future session. " +
			"Actions: save, read, list, delete. Names are lowercase letters, digits, '.', '_' and '-'.",
		Schema: schema(map[string]any{
			"action":  enum("What to do.", "save", "read", "list", "delete"),
			"scope":   enum("Where the memory lives.", "workspace", "permanent"),
			"name":    str("Memory name, e.g. \"build-commands\"."),
			"content": str("Markdown text to store (for save). The first line should be a one-line description."),
		}, "action", "scope"),
	}
}

func (memoryTool) Run(_ context.Context, env *Env, raw json.RawMessage) Result {
	var a struct {
		Action  string `json:"action"`
		Scope   string `json:"scope"`
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	_ = json.Unmarshal(raw, &a)
	perm, ws := MemoryDirs(env)
	dir := ws
	if a.Scope == "permanent" {
		dir = perm
	}
	if a.Action == "list" {
		return listMemory(dir, a.Scope)
	}
	if !memoryName.MatchString(a.Name) {
		return Fail("name %q is invalid: use lowercase letters, digits, '.', '_' or '-' (max 64, starting with a letter or digit).", a.Name)
	}
	path := filepath.Join(dir, a.Name+".md")
	switch a.Action {
	case "save":
		if strings.TrimSpace(a.Content) == "" {
			return Fail("content is empty; nothing to save.")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return Fail("cannot create the memory directory: %v", err)
		}
		_, existed := os.Stat(path)
		if err := os.WriteFile(path, []byte(strings.TrimSpace(a.Content)+"\n"), 0o644); err != nil {
			return Fail("cannot save memory: %v", err)
		}
		verb := "Saved"
		if existed == nil {
			verb = "Updated"
		}
		return OK("%s %s memory %q (%d bytes). It is loaded into new sessions automatically.", verb, a.Scope, a.Name, len(a.Content))
	case "read":
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return Fail("no %s memory named %q. Use action=list to see what exists.", a.Scope, a.Name)
		} else if err != nil {
			return Fail("cannot read memory: %v", err)
		}
		return OK("%s", b)
	case "delete":
		if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
			return Fail("no %s memory named %q.", a.Scope, a.Name)
		} else if err != nil {
			return Fail("cannot delete memory: %v", err)
		}
		return OK("Deleted %s memory %q.", a.Scope, a.Name)
	}
	return Fail("unknown action %q", a.Action)
}

func listMemory(dir, scope string) Result {
	items := ReadMemories(dir)
	if len(items) == 0 {
		return OK("No %s memories yet.", scope)
	}
	var sb strings.Builder
	for _, it := range items {
		fmt.Fprintf(&sb, "- %s: %s\n", it.Name, it.Summary)
	}
	return OK("%s", sb.String())
}

// MemoryItem is one stored memory.
type MemoryItem struct {
	Name    string
	Summary string // first non-empty line
	Content string
}

// ReadMemories loads every memory in dir, sorted by name.
func ReadMemories(dir string) []MemoryItem {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []MemoryItem
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		content := strings.TrimSpace(string(b))
		summary := ""
		for _, l := range strings.Split(content, "\n") {
			if t := strings.TrimSpace(strings.TrimLeft(l, "# ")); t != "" {
				summary = oneLine(t, 140)
				break
			}
		}
		out = append(out, MemoryItem{Name: strings.TrimSuffix(e.Name(), ".md"), Summary: summary, Content: content})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RegisterMemoryTool adds the memory tool to r.
func RegisterMemoryTool(r *Registry) { r.Register(memoryTool{}) }
