package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Home is the agent's state directory. By default it is the directory that
// contains the zagent binary, so a copied folder is a self-contained agent;
// --home or ZAGENT_HOME override it.
type Home struct{ Root string }

// ResolveHome picks the home directory: explicit flag, then ZAGENT_HOME, then
// the directory holding the running executable.
func ResolveHome(flag string) (Home, error) {
	switch {
	case flag != "":
		return abs(flag)
	case os.Getenv("ZAGENT_HOME") != "":
		return abs(os.Getenv("ZAGENT_HOME"))
	}
	exe, err := os.Executable()
	if err != nil {
		return Home{}, fmt.Errorf("cannot locate the executable to derive the home directory: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return Home{Root: filepath.Dir(exe)}, nil
}

func abs(p string) (Home, error) {
	a, err := filepath.Abs(p)
	if err != nil {
		return Home{}, err
	}
	return Home{Root: a}, nil
}

func (h Home) EnvFile() string             { return filepath.Join(h.Root, ".env") }
func (h Home) ConfigFile() string          { return filepath.Join(h.Root, "config.json") }
func (h Home) SystemFile() string          { return filepath.Join(h.Root, "system_instruction.md") }
func (h Home) MemoryDir() string           { return filepath.Join(h.Root, "memory") }
func (h Home) SkillsDir() string           { return filepath.Join(h.Root, "skills") }
func (h Home) ToolsDir() string            { return filepath.Join(h.Root, "tools") }
func (h Home) CommandsDir() string         { return filepath.Join(h.Root, "commands") }
func (h Home) SessionsDir() string         { return filepath.Join(h.Root, "sessions") }
func (h Home) ScratchDir() string          { return filepath.Join(h.Root, "scratchpad") }
func (h Home) SessionDir(id string) string { return filepath.Join(h.SessionsDir(), id) }

// Init creates the directory layout and any missing template files. Existing
// files are never overwritten. It reports what it created.
func (h Home) Init() ([]string, error) {
	var created []string
	for _, d := range []string{h.Root, h.MemoryDir(), h.SkillsDir(), h.ToolsDir(), h.CommandsDir(), h.SessionsDir(), h.ScratchDir()} {
		if _, err := os.Stat(d); errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return created, err
			}
			created = append(created, d+string(filepath.Separator))
		}
	}
	files := []struct{ path, content string }{
		{h.EnvFile(), envTemplate},
		{h.ConfigFile(), configTemplate},
		{h.SystemFile(), DefaultSystemInstruction},
		{filepath.Join(h.CommandsDir(), "review.md"), reviewCommand},
	}
	for _, f := range files {
		if _, err := os.Stat(f.path); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(f.path, []byte(f.content), 0o600); err != nil {
				return created, err
			}
			created = append(created, f.path)
		}
	}
	return created, nil
}

// Load reads and validates .env and config.json.
func (h Home) Load() (Env, Config, error) {
	env, err := LoadEnv(h.EnvFile())
	if err != nil {
		return env, Config{}, fmt.Errorf("%s: %w", h.EnvFile(), err)
	}
	cfg, err := LoadConfig(h.ConfigFile())
	if err != nil {
		return env, cfg, err
	}
	return env, cfg, nil
}

const envTemplate = `# zagent model settings. Secrets live here; everything else is in config.json.
#
# PROVIDER: gemini | openai_compatible | anthropic | custom
#   gemini / openai_compatible / anthropic: BASE_URL may be just the host. The
#   engine appends the standard path (/v1beta, /v1, ...). If you give a base
#   that already has a path (for example /v1beta/openai or /api/v3) it is used as given.
#   custom: BASE_URL is the exact URL requests are POSTed to, and API_FORMAT
#   (gemini | openai | anthropic) says which wire format it speaks.
PROVIDER=gemini
BASE_URL=
API_KEY=
MODEL=gemini-2.5-flash
# API_FORMAT=openai

# Model window in tokens, and when to auto-compact (tokens, or a percentage).
CONTEXT_LIMIT=1000000
COMPACT_AT=80%

# What the model can take as input. Only set true what the model really accepts.
VISION=true
AUDIO=false
PDF=false
VIDEO=false
`

const configTemplate = `{
  "agent": {
    "max_tool_call_turn": 200,
    "max_output_tokens": 0
  },
  "tools": {
    "disabled": [],
    "default_timeout_seconds": 120
  },
  "context": {
    "max_turn_to_keep_attachment": 0,
    "request_body_limit_mb": 28,
    "autocompact_on_request_body_limit_exceed": 1,
    "remove_old_attachment_on_request_body_limit": 1,
    "compact_keep_last_turns": 1
  },
  "network": {
    "max_retries": 6,
    "auto_pause": 1,
    "pause_retry_minutes": 5
  },
  "permissions": {
    "run_command": "ask"
  }
}
`

const reviewCommand = `Review the changes in the working tree for correctness bugs, risky edge cases and missing tests.
List findings most-severe first, each with the file and line, why it is a problem, and a concrete fix.
Do not edit any files.
`

// DefaultSystemInstruction is written to system_instruction.md on init and used
// when the file is absent. It is deliberately short and provider-neutral.
const DefaultSystemInstruction = `You are zagent, an autonomous software and research agent running on the user's machine.

Work independently toward the user's goal. Prefer doing the task over describing how you would do it, and finish with a short, accurate report of what you did and what you verified.

Principles
- Read before you edit. Make the smallest change that solves the problem and keep the surrounding style.
- Verify your work with real tool output before claiming success. If something failed or you could not check it, say so plainly.
- Never invent file contents, command output or results. If a tool returns an error, read it and adapt.
- Ask the user only when you are truly blocked by a decision that is theirs to make.

Tools
- Tool results can be errors. They explain what was wrong (unknown argument, missing field, unsupported media type); correct the call and retry.
- Large attachments are removed from the conversation over time to save context. A note tells you the original path; re-open the file if you still need it.

Notes from the system
- Lines beginning with "[info:" are notes from the engine or the user, for example a changed workspace or a finished background task. They are not part of the user's request; act on them when relevant.
`
