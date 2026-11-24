# zagent

[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Platform](https://img.shields.io/badge/Platform-Windows%20%7C%20Linux%20%7C%20macOS-lightgrey)](#cross-compilation--releases)

A durable, provider-neutral **AI agent runtime** written in Go with zero runtime dependencies. Ships as a single self-contained static binary, `zagent`, capable of running headless, in an interactive terminal UI, or behind an embedded browser UI against **Gemini**, **OpenAI-compatible**, **Anthropic**, or **custom** API gateways.

---

## Highlights

- **One Engine, Three Interfaces:** The core engine owns the session and state. The headless runner, terminal UI (TUI), and web UI are lightweight views that attach over an authenticated local loopback API (`127.0.0.1`). Start an agent headless, attach from a browser, detach, and the agent continues executing uninterrupted.
- **Append-Only Event Sourcing:** Sessions are stored as durable, append-only `events.jsonl` logs accompanied by content-addressed blob storage (`blobs/`). If a crash or power cut occurs, state is deterministically reconstructed upon resume.
- **Deterministic Prefix Caching:** Conversation contexts are rendered deterministically with sorted JSON keys. Ordinary conversation turns only extend history at the end, maximizing provider-side KV-cache hits and dramatically lowering token costs.
- **Strict Context & Size Guards:** Pre-flight body-size validation ensures requests never breach provider limits (preventing HTTP 413s). Automatically prunes aged attachments and compacts long histories when approaching configurable token thresholds.
- **Self-Healing & Auto-Recovery:** Interrupted tool calls receive repair results on resume; network outages trigger exponential backoff with auto-pause; tool runtime errors are redirected back to the model as actionable feedback.
- **Extensible Tool Architecture:** Native support for file manipulation, shell command management with background task supervision, memory, skills, and subagents. Third-party tools integrate via simple JSON manifests and standard I/O.

---

## Architecture

```text
┌────────────────────────────────────────────────────────┐
│                   Interfaces                           │
│  CLI Headless  │  Terminal UI (TUI)  │  Browser WebUI  │
└──────────────────────────┬─────────────────────────────┘
                           │ Authenticated HTTP / SSE (127.0.0.1)
                           ▼
┌────────────────────────────────────────────────────────┐
│                   Engine Core                          │
│                                                        │
│  ┌──────────────────┐          ┌────────────────────┐  │
│  │   Event Log      │          │   Context Manager  │  │
│  │  (events.jsonl)  ├─────────►│  • Pruning Policy  │  │
│  │  + Blobs Store   │          │  • Compaction      │  │
│  └──────────────────┘          └─────────┬──────────┘  │
│           ▲                              │             │
│           │ Tool Results                 │ Context     │
│           │                              ▼             │
│  ┌────────┴─────────┐          ┌────────────────────┐  │
│  │   Tool Engine    │          │  Provider Adapters │  │
│  │ • Files & Shell  │          │  • Gemini          │  │
│  │ • External JSON  │          │  • OpenAI-compat   │  │
│  │ • Sub-agents     │          │  • Anthropic       │  │
│  └──────────────────┘          └─────────┬──────────┘  │
└──────────────────────────────────────────┼─────────────┘
                                           │ HTTPS Stream
                                           ▼
                                 [ Remote LLM Gateway ]
```

---

## Quick Start

### 1. Build or Download
Download a pre-compiled binary from `dist/` or build from source:

```bash
# Build the binary for your host OS
go build -o zagent ./cmd/zagent
```

### 2. Initialize Agent Home
Run `init` to scaffold your agent directory beside the binary:

```bash
./zagent init
```
This generates the agent home directory containing `.env`, `config.json`, `skills/`, `tools/`, and `sessions/`.

### 3. Configure Provider Credentials
Copy `.env.example` to `<home>/.env` and supply your credentials:

```bash
cp .env.example .env
```

Example `.env`:
```ini
PROVIDER=gemini
BASE_URL=https://generativelanguage.googleapis.com
API_KEY=your_gemini_api_key
MODEL=gemini-2.5-pro

CONTEXT_LIMIT=1000000
COMPACT_AT=80%
VISION=true
AUDIO=false
PDF=true
VIDEO=false
```

### 4. Validate Configuration
Verify connectivity, model authentication, and local paths:

```bash
./zagent doctor
```

### 5. Run Tasks

```bash
# Headless run: executes prompt, outputs final response, and exits
./zagent -p "List files in the workspace and identify the build targets"

# Terminal UI: interactive chat with live streaming and tool supervision
./zagent --ui cli

# Web UI: opens an embedded browser dashboard with live token meters
./zagent --ui web
```

---

## Command-Line Usage

```text
zagent [flags] [subcommand]

Subcommands:
  init                       Create a ready-to-run agent home
  doctor                     Verify environment, model access, and permissions
  sessions                   List all existing sessions
  show <session_id>          Print transcripts or replay session events
  tools list                 Display available built-in and external tools
  tools run <name> --args {} Test an external tool without calling a model
  version                    Display binary version and build metadata

Execution Flags:
  -p, --prompt <string>      Prompt text for headless task execution
  --prompt-file <path>       Read prompt text from a UTF-8 file
  --session <id>             Session identifier (creates new or resumes existing)
  --new                      Force the creation of a fresh session
  --home <dir>               Explicit agent home directory (default: binary directory)
  --workspace <dir>          Working directory for file and tool execution
  --ui <cli|web>             Launch interactive terminal UI or embedded web browser
  --output <lines>           Print full transcript or last N lines on exit
  --verbose                  Enable debug event logging to standard error
```

---

## Supported Providers

| `PROVIDER` | Request Endpoint | Notes |
|---|---|---|
| `gemini` | `<base>/v1beta/models/<model>:streamGenerateContent?alt=sse` | Native function calling, thinking signature replay, multimodal inputs. |
| `openai_compatible` | `<base>/v1/chat/completions` | Standard Chat Completions with SSE streaming (Ollama, vLLM, OpenRouter). |
| `anthropic` | `<base>/v1/messages` | Messages API with thinking blocks and tool use. |
| `custom` | `BASE_URL` exactly as specified | Set `API_FORMAT=gemini\|openai\|anthropic` to match wire format. |

---

## Configuration Reference

Runtime configuration is split into two layers:
1. **`.env` (Secrets & Models):** API keys, model names, context limits, and multimodal toggles.
2. **`config.json` (Runtime Policies):** Tool permissions, timeouts, retries, and network behavior.

Example `config.json`:
```json
{
  "context": {
    "max_turn_to_keep_attachment": 0,
    "request_body_limit_mb": 28,
    "autocompact_on_request_body_limit_exceed": 1,
    "remove_old_attachment_on_request_body_limit": 1
  },
  "tools": {
    "disabled": []
  },
  "permissions": {
    "run_command": "deny"
  },
  "network": {
    "max_retries": 6,
    "auto_pause": 1,
    "pause_retry_minutes": 5
  }
}
```

---

## Built-in Tools & Extensions

- **Filesystem Tools:** `view_file` (with multimodal media detection), `write_file`, `edit_file`, `list_dir`, `find_files`, `search_text`.
- **Process Management:** `run_command` executes subcommands with real-time output streaming, background execution, and cancellation via Job Objects/process groups.
- **Memory & Skills:** `memory` (workspace and permanent key-value notes), `skill` (on-demand prompt instruction overlays).
- **Subagents:** `subagent` launches isolated child agent instances in dedicated sub-sessions, returning only the summarized report to preserve parent context tokens.
- **Third-Party Tools:** Place a JSON manifest (`tools/<tool_name>/<tool_name>.json`) alongside any executable or script. See [docs/EXTERNAL_TOOLS.md](docs/EXTERNAL_TOOLS.md) for full protocol specifications.

---

## Interactive Slash Commands

When running in CLI or Web UI mode, type slash commands to inspect or alter runtime state:

- `/help` — List available commands
- `/stop` — Abort the active model turn and cancel child processes
- `/note <text>` — Queue a note to be attached to the next model request
- `/compact` — Manually trigger conversation compaction
- `/status` — View active session tokens, turns, and provider health
- `/tasks` — Inspect active background processes
- `/workspace <path>` — Switch the active working directory
- `/exit` — Detach from the session while leaving the background engine running
- `/quit` — Terminate the agent engine process cleanly

---

## Cross-Compilation & Releases

`zagent` can be cross-compiled for 6 major targets with CGO disabled:

```bash
# Windows PowerShell
.\scripts\package.ps1 -Version 0.1.1

# Linux / macOS
./scripts/package.sh 0.1.1
```

Compiled release archives and checksums are placed into `dist/`:
- `zagent-0.1.1-windows-amd64.zip`
- `zagent-0.1.1-windows-arm64.zip`
- `zagent-0.1.1-linux-amd64.tar.gz`
- `zagent-0.1.1-linux-arm64.tar.gz`
- `zagent-0.1.1-darwin-amd64.tar.gz`
- `zagent-0.1.1-darwin-arm64.tar.gz`
- `SHA256SUMS.txt`

---

## Testing & Quality Assurance

Run the comprehensive unit test suite:

```bash
# Run all unit tests
go test -v ./...

# Run static checks
go vet ./...
```

---

## License

Distributed under the **Apache License, Version 2.0**. See [LICENSE](LICENSE) for details.
