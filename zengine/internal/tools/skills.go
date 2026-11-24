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

var skillName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Skill is a folder with a SKILL.md: instructions the model loads on demand.
type Skill struct {
	Name        string
	Description string
	Dir         string
}

// ParseFrontmatter reads simple "key: value" YAML frontmatter and returns the
// metadata and the remaining body.
func ParseFrontmatter(text string) (map[string]string, string) {
	meta := map[string]string{}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return meta, text
	}
	rest := text[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return meta, text
	}
	for _, l := range strings.Split(rest[:end], "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok {
			meta[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	body := strings.TrimPrefix(rest[end+4:], "\n")
	return meta, body
}

// ListSkills scans dir for skill folders.
func ListSkills(dir string) []Skill {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "SKILL.md"))
		if err != nil {
			continue
		}
		meta, _ := ParseFrontmatter(string(b))
		name := meta["name"]
		if name == "" {
			name = e.Name()
		}
		out = append(out, Skill{Name: name, Description: meta["description"], Dir: filepath.Join(dir, e.Name())})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type skillTool struct{}

func (skillTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "skill",
		Description: "Skills are reusable instruction packs stored in the agent home. " +
			"load: read a skill's full instructions (do this before starting a task a skill describes). " +
			"list: show available skills. " +
			"create: write a new skill when you have worked out a repeatable procedure worth keeping (or the user asks for one).",
		Schema: schema(map[string]any{
			"action":      enum("What to do.", "load", "list", "create"),
			"name":        str("Skill name: lowercase letters, digits and hyphens."),
			"description": str("For create: one sentence saying WHEN to use this skill."),
			"content":     str("For create: the skill's instructions in Markdown."),
		}, "action"),
	}
}

func (skillTool) Run(_ context.Context, env *Env, raw json.RawMessage) Result {
	var a struct{ Action, Name, Description, Content string }
	_ = json.Unmarshal(raw, &a)
	dir := env.Home.SkillsDir()
	switch a.Action {
	case "list":
		skills := ListSkills(dir)
		if len(skills) == 0 {
			return OK("No skills installed.")
		}
		var sb strings.Builder
		for _, s := range skills {
			fmt.Fprintf(&sb, "- %s: %s\n", s.Name, s.Description)
		}
		return OK("%s", sb.String())
	case "load":
		if a.Name == "" {
			return Fail("name is required for load. Use action=list to see skills.")
		}
		for _, s := range ListSkills(dir) {
			if s.Name != a.Name {
				continue
			}
			b, err := os.ReadFile(filepath.Join(s.Dir, "SKILL.md"))
			if err != nil {
				return Fail("cannot read skill %q: %v", a.Name, err)
			}
			_, body := ParseFrontmatter(string(b))
			out := fmt.Sprintf("# Skill: %s\n(directory: %s)\n\n%s", s.Name, s.Dir, strings.TrimSpace(body))
			if extra := skillFiles(s.Dir); extra != "" {
				out += "\n\nOther files in this skill (open with view_file):\n" + extra
			}
			return OK("%s", out)
		}
		return Fail("no skill named %q. Use action=list to see skills.", a.Name)
	case "create":
		if !skillName.MatchString(a.Name) {
			return Fail("skill name %q is invalid: use lowercase letters, digits and hyphens (max 64).", a.Name)
		}
		if strings.TrimSpace(a.Description) == "" || strings.TrimSpace(a.Content) == "" {
			return Fail("create needs both description (when to use it) and content (the instructions).")
		}
		skillDir := filepath.Join(dir, a.Name)
		path := filepath.Join(skillDir, "SKILL.md")
		if _, err := os.Stat(path); err == nil {
			return Fail("skill %q already exists. Pick another name, or ask the user before replacing it.", a.Name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return Fail("cannot check for an existing skill: %v", err)
		}
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			return Fail("cannot create the skill directory: %v", err)
		}
		text := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n", a.Name, oneLine(a.Description, 300), strings.TrimSpace(a.Content))
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			return Fail("cannot write the skill: %v", err)
		}
		return OK("Created skill %q at %s. It is listed in the skill index of new sessions; load it now with action=load if you need it in this one.", a.Name, skillDir)
	}
	return Fail("unknown action %q", a.Action)
}

func skillFiles(dir string) string {
	var sb strings.Builder
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == "SKILL.md" {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(&sb, "- %s\n", filepath.ToSlash(rel))
		return nil
	})
	return strings.TrimRight(sb.String(), "\n")
}

// RegisterSkillTool adds the skill tool to r.
func RegisterSkillTool(r *Registry) { r.Register(skillTool{}) }
