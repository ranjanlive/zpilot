// Package prompt composes the system prompt snapshot for a session.
//
// The snapshot is taken when a session starts and again after a compaction,
// then stored in the session log. Edits to system_instruction.md, new memories
// or new skills made mid-session therefore never alter the prompt prefix; they
// reach the model as "[info: ...]" notes or through tools instead.
package prompt

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/config"
	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/tools"
)

// Inputs are everything the prompt depends on.
type Inputs struct {
	Home      config.Home
	Workspace string
	Caps      llm.Capabilities
	Model     string
	Now       time.Time
	// MemoryBudget caps how many characters of memory text are inlined per scope.
	MemoryBudget int
}

// Build renders the system prompt.
func Build(in Inputs) string {
	if in.MemoryBudget <= 0 {
		in.MemoryBudget = 6000
	}
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	var sb strings.Builder
	sb.WriteString(strings.TrimSpace(instruction(in.Home)))
	sb.WriteString("\n\n# Environment\n")
	fmt.Fprintf(&sb, "- Operating system: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&sb, "- Shell for run_command: %s\n", tools.ShellName())
	fmt.Fprintf(&sb, "- Workspace (your working directory; relative paths resolve here): %s\n", in.Workspace)
	fmt.Fprintf(&sb, "- Scratchpad for temporary files: %s\n", in.Home.ScratchDir())
	fmt.Fprintf(&sb, "- Agent home (skills and permanent memory live here): %s\n", in.Home.Root)
	fmt.Fprintf(&sb, "- Today's date: %s\n", in.Now.Format("2006-01-02"))
	if in.Model != "" {
		fmt.Fprintf(&sb, "- Model: %s\n", in.Model)
	}
	fmt.Fprintf(&sb, "- Media you can take as input: %s\n", capabilityList(in.Caps))

	permanent, workspace := tools.MemoryDirs(&tools.Env{Home: in.Home, Workspace: in.Workspace})
	writeMemory(&sb, "Permanent memory (applies to every project)", permanent, in.MemoryBudget)
	writeMemory(&sb, "Workspace memory (this project)", workspace, in.MemoryBudget)

	if skills := tools.ListSkills(in.Home.SkillsDir()); len(skills) > 0 {
		sb.WriteString("\n# Skills\nLoad a skill with the skill tool before starting a task it describes.\n")
		for _, s := range skills {
			fmt.Fprintf(&sb, "- %s: %s\n", s.Name, s.Description)
		}
	}
	return sb.String()
}

func instruction(h config.Home) string {
	b, err := os.ReadFile(h.SystemFile())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return config.DefaultSystemInstruction
	}
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return config.DefaultSystemInstruction
	}
	return string(b)
}

func capabilityList(c llm.Capabilities) string {
	var on []string
	for _, p := range []struct {
		ok   bool
		name string
	}{{c.Vision, "images"}, {c.PDF, "PDF"}, {c.Audio, "audio"}, {c.Video, "video"}} {
		if p.ok {
			on = append(on, p.name)
		}
	}
	if len(on) == 0 {
		return "text only (view_file will refuse images, PDFs, audio and video)"
	}
	return strings.Join(on, ", ") + " (others are refused by view_file)"
}

func writeMemory(sb *strings.Builder, title, dir string, budget int) {
	items := tools.ReadMemories(dir)
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(sb, "\n# %s\n", title)
	used := 0
	for _, it := range items {
		if used+len(it.Content) <= budget {
			fmt.Fprintf(sb, "## %s\n%s\n", it.Name, it.Content)
			used += len(it.Content)
		} else {
			fmt.Fprintf(sb, "- %s: %s (read it with the memory tool)\n", it.Name, it.Summary)
		}
	}
}
