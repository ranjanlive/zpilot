package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ResolvePath turns a model-supplied path into an absolute, vetted path.
//
// Relative paths are anchored at the workspace. Access is allowed inside the
// workspace, the home scratchpad and the home skills directory (read only);
// anything else needs read_outside_workspace / write_outside_workspace in
// config.json. The home .env (API key) is never accessible. Symlinks are
// resolved before the check so a link cannot smuggle a path out of a root.
func (e *Env) ResolvePath(p string, write bool) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("path is empty")
	}
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), string(filepath.Separator)))
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(e.Workspace, p)
	}
	p = filepath.Clean(p)
	real := evalExisting(p)

	if e.isSecret(real) {
		return "", fmt.Errorf("access to %s is not allowed (it holds the agent's credentials)", p)
	}
	roots := []string{e.Workspace, e.Home.ScratchDir()}
	if !write {
		roots = append(roots, e.Home.SkillsDir(), e.Home.MemoryDir())
	}
	for _, r := range roots {
		if r != "" && within(evalExisting(r), real) {
			return p, nil
		}
	}
	if write && bool(e.Cfg.Permissions.WriteOutsideWorkspace) {
		return p, nil
	}
	if !write && (bool(e.Cfg.Permissions.ReadOutsideWorkspace) || bool(e.Cfg.Permissions.WriteOutsideWorkspace)) {
		return p, nil
	}
	verb := "read"
	flagName := "read_outside_workspace"
	if write {
		verb, flagName = "write", "write_outside_workspace"
	}
	return "", fmt.Errorf("cannot %s %s: it is outside the workspace (%s). Use a path inside the workspace or the scratchpad (%s), or set permissions.%s in config.json",
		verb, p, e.Workspace, e.Home.ScratchDir(), flagName)
}

func (e *Env) isSecret(real string) bool {
	secrets := []string{filepath.Join(e.Home.Root, ".env"), filepath.Join(e.Home.Root, "sessions")}
	for _, s := range secrets {
		if samePath(evalExisting(s), real) || within(evalExisting(s), real) {
			return true
		}
	}
	return false
}

// evalExisting resolves symlinks for the longest existing prefix of p, so a
// not-yet-created file inside a symlinked directory is still checked correctly.
func evalExisting(p string) string {
	p = filepath.Clean(p)
	cur := p
	var rest []string
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, rest[i])
			}
			return resolved
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
