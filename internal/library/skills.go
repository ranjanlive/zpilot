package library

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ranjanlive/zpilot/internal/model"
	"github.com/ranjanlive/zpilot/internal/store"
)

// Skill is a folder with SKILL.md. The engine loads a skill on demand, using
// the description from the frontmatter to decide when.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body,omitempty"`
	Files       int    `json:"files"`
}

// Skills lists the skill library.
func (l *Library) Skills() ([]Skill, error) {
	ents, err := os.ReadDir(l.SkillsDir())
	if err != nil {
		return nil, err
	}
	out := []Skill{}
	for _, e := range ents {
		if e.IsDir() && model.ValidSlug(e.Name()) {
			if s, err := l.Skill(e.Name(), false); err == nil {
				out = append(out, s)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Skill reads one skill; withBody includes the SKILL.md body.
func (l *Library) Skill(name string, withBody bool) (Skill, error) {
	if !model.ValidSlug(name) {
		return Skill{}, fmt.Errorf("invalid skill name %q", name)
	}
	dir := filepath.Join(l.SkillsDir(), name)
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return Skill{}, fmt.Errorf("skill %s not found", name)
	}
	meta, body := parseFrontmatter(string(b))
	s := Skill{Name: name, Description: meta["description"]}
	if withBody {
		s.Body = body
	}
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			s.Files++
		}
		return nil
	})
	return s, nil
}

// SkillDir is the folder of a skill.
func (l *Library) SkillDir(name string) string { return filepath.Join(l.SkillsDir(), name) }

// SaveSkill creates or updates a skill's SKILL.md.
func (l *Library) SaveSkill(s Skill) error {
	if !model.ValidSlug(s.Name) {
		return fmt.Errorf("skill name must be lowercase letters, digits, - or _ (got %q)", s.Name)
	}
	desc := strings.TrimSpace(strings.ReplaceAll(s.Description, "\n", " "))
	if desc == "" {
		return fmt.Errorf("description is required: it tells the agent when to use the skill")
	}
	if strings.TrimSpace(s.Body) == "" {
		return fmt.Errorf("instructions are required")
	}
	content := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n", s.Name, desc, strings.TrimSpace(s.Body))
	return store.WriteFileAtomic(filepath.Join(l.SkillDir(s.Name), "SKILL.md"), []byte(content), 0o644)
}

// ImportSkill copies a skill folder (with SKILL.md) into the library.
func (l *Library) ImportSkill(src string) (Skill, error) {
	name := filepath.Base(filepath.Clean(src))
	if !model.ValidSlug(name) {
		return Skill{}, fmt.Errorf("folder name %q is not a valid skill name", name)
	}
	if _, err := os.Stat(filepath.Join(src, "SKILL.md")); err != nil {
		return Skill{}, fmt.Errorf("%s has no SKILL.md", src)
	}
	dst := l.SkillDir(name)
	_ = os.RemoveAll(dst)
	if err := CopyDir(src, dst); err != nil {
		return Skill{}, err
	}
	return l.Skill(name, false)
}

// RemoveSkill deletes a skill from the library.
func (l *Library) RemoveSkill(name string) error {
	if _, err := l.Skill(name, false); err != nil {
		return err
	}
	return os.RemoveAll(l.SkillDir(name))
}

// parseFrontmatter reads simple "key: value" frontmatter (same rules as the engine).
func parseFrontmatter(text string) (map[string]string, string) {
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
	for _, line := range strings.Split(rest[:end], "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			meta[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	body := rest[end+4:]
	return meta, strings.TrimLeft(body, "\n")
}
