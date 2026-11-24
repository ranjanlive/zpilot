# Installing and setting up zagent

zagent is a single self-contained program. There is nothing to install besides the file itself: no runtime, no services, no registry entries.

## 1. Download and check

Download the archive for your system from the releases page and the `SHA256SUMS.txt` beside it.

| System | Archive |
|---|---|
| Windows 10/11 (Intel/AMD) | `zagent-<version>-windows-amd64.zip` |
| Windows on ARM | `zagent-<version>-windows-arm64.zip` |
| Linux (Intel/AMD, ARM) | `zagent-<version>-linux-amd64.tar.gz` / `-arm64.tar.gz` |
| macOS (Intel, Apple silicon) | `zagent-<version>-darwin-amd64.tar.gz` / `-arm64.tar.gz` |

Verify the download (the output must match the line for your archive in `SHA256SUMS.txt`):

```powershell
Get-FileHash .\zagent-0.1.0-windows-amd64.zip -Algorithm SHA256        # Windows
```
```sh
sha256sum zagent-0.1.0-linux-amd64.tar.gz                              # Linux   (macOS: shasum -a 256 ...)
```

## 2. Unpack: the folder *is* the agent

Unpack the archive somewhere permanent, for example `C:\Tools\zagent` or `~/zagent`. **That folder is the agent's home**: your settings, memory, skills, third-party tools and all sessions live next to the binary. To move or back up the agent, copy the folder. To use several independent agents, unpack several copies (or point one binary at different folders with `--home DIR` or the `ZAGENT_HOME` environment variable).

```
zagent/
  zagent(.exe)   README.md  CHANGELOG.md  docs/  examples/
  (created on first run)  .env  config.json  system_instruction.md  memory/  skills/  tools/  commands/  sessions/  scratchpad/
```

- **Linux / macOS:** `chmod +x zagent`. On macOS the downloaded binary is not notarised; if Gatekeeper blocks it, run `xattr -d com.apple.quarantine zagent` once.
- **Run from anywhere:** add the folder to your `PATH`, or make a shortcut/alias. The home is found from the real location of the binary, so a symlink to it works too.

## 3. Create the home and configure the model

```sh
zagent init            # creates .env, config.json, system_instruction.md and the folders
```

Open `.env` and fill in four lines (see [PROVIDERS.md](PROVIDERS.md) for ready-made examples for each service):

```ini
PROVIDER=gemini                        # gemini | openai_compatible | anthropic | custom
BASE_URL=https://generativelanguage.googleapis.com
API_KEY=your-key-here
MODEL=gemini-2.5-flash
```

Also set what the model can accept as input, so the agent never sends something the model would reject:

```ini
VISION=true      # images
AUDIO=false
PDF=false
VIDEO=false
CONTEXT_LIMIT=1000000     # the model's context window, in tokens
COMPACT_AT=80%            # summarise the conversation when it reaches this much of it
```

**Keeping the key out of files:** leave `API_KEY=` empty and set the environment variable `ZAGENT_API_KEY` instead (any `.env` setting can be given as `ZAGENT_<NAME>`). The variable wins over the file.

## 4. Check that it works

```sh
zagent doctor
```

It prints the settings it will use (including the exact request URL), lists the tools, and sends a one-word test request. Fix anything it reports before going further. Typical output ends with `Connection: OK in 1.2s`.

## 5. First tasks

```sh
cd path/to/your/project                          # the folder you are in is the agent's workspace
zagent --prompt "Summarise what this project does and list its main entry points."
```

The agent works, prints a short progress trail on stderr, and prints its final answer. Useful variations:

| Goal | Command |
|---|---|
| Name the session so you can come back | `zagent --session notes -p "..."` |
| Continue it later | `zagent --session notes -p "now add tests"` (or `zagent --session -p ...` for the latest session of this folder) |
| Give it an image, PDF, audio or video | `zagent -p "what is wrong in this screenshot?" --attach shot.png` |
| Read the whole transcript of the run | `zagent -p "..." --output` (or `--output 200` for the last 200 lines) |
| Watch or steer it in a terminal | `zagent --session notes --ui cli` |
| Watch or steer it in a browser | `zagent --session notes --ui web` |
| List / read past sessions | `zagent sessions`, `zagent show notes` |

**Shell commands are off until you turn them on.** The default `permissions.run_command` is `ask`, but interactive approval is **not implemented yet** in any interface, so `ask` currently behaves like `deny`: the agent is told it may not run commands and works with the file tools only. When you trust a task (and understand that commands run with your user's permissions), allow them in `config.json`:

```json
{ "permissions": { "run_command": "allow" } }
```

Use `"deny"` to make the refusal explicit. You can also remove the tool entirely with `"tools": {"disabled": ["run_command", "terminal"]}`.

### Using the interfaces

- `--ui web` starts the agent as a background service, prints a link and opens your browser. The link contains a private access token; it only works on your own computer.
- `--ui cli` opens a full-screen terminal view of the same session.
- In both: type to send a message. **While the agent is working, what you type becomes a note** that is attached to the end of its next request, and a **Stop** control interrupts it. `/help` lists the slash commands.
- **Closing a window never stops the agent.** `/exit` (or `Ctrl-C` when idle) leaves the interface; the agent keeps working in the background. To end the background process type `/quit`. It also exits by itself after 30 idle minutes (`service.idle_exit_minutes`).

## Updating

Replace the `zagent` binary with the new one. Your `.env`, `config.json`, sessions, memory, skills and tools are untouched. New releases may add `config.json` options with sensible defaults; see `CHANGELOG.md` and [CONFIG.md](CONFIG.md). Nothing is migrated or rewritten automatically.

## Uninstalling

Delete the folder. If a background service is still running, end it first: `zagent --session NAME --ui cli`, then `/quit`.

## Troubleshooting

| You see | Cause and fix |
|---|---|
| `first run: created the agent home` | Normal. Edit `.env`, then run again. |
| `API_KEY is required` | Set `API_KEY` (or `ZAGENT_API_KEY`). For servers without authentication set `"provider": {"auth_style": "none"}` in `config.json`. |
| `Connection: FAILED (auth)` / HTTP 401 | The key is wrong, or the service wants a different header; try `provider.auth_style` (`bearer`, `x-api-key`, `x-goog-api-key`). |
| `FAILED (bad_request)` / 404 / "model not found" | `MODEL` or `BASE_URL` is wrong. `doctor` prints the exact URL it called: compare it with your provider's documentation. For non-standard gateways use `PROVIDER=custom`, `API_FORMAT=...` and the full URL. |
| `session "x" is busy: in use by pid N` | Another zagent process owns that session. Use another session name or attach with `--ui cli`. Process death releases the OS-held lock; a slow heartbeat cannot steal a live session. |
| Agent says it "needs approval" for a command | `permissions.run_command` is `ask` and no interface can approve (headless). Set it to `allow` if you trust the task. |
| `... not supported by the configured model` in a tool result | The file type is switched off for the model (`VISION`, `PDF`, ...). Turn it on only if the model really accepts it. |
| Status shows `paused: network trouble` | The provider was unreachable after the retries. It retries on its own every 5 minutes (`/resume` retries now); nothing is lost. |
| `--ui cli needs an interactive terminal` | The terminal view requires a real terminal (not a pipe, CI job, or some IDE consoles). Use `--ui web` or `--prompt`. |
| The request was "too large" | The endpoint's body limit is below `context.request_body_limit_mb`. zagent already recovers by dropping old attachments; lower the setting to avoid the retry. |
| Odd or garbled output on Windows | Install PowerShell 7 (`pwsh`); zagent prefers it over Windows PowerShell 5.1 for the agent's shell. |
| Anything else | `zagent show SESSION --debug` prints the full event trail; `sessions/<id>/service.log` has the background service's log. |
