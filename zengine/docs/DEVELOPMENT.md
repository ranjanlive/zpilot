# Developer guide

For people who want to read, change or extend the engine. Users who only want to *use* zagent should start at [INSTALL.md](INSTALL.md); for how it fits together, read [ARCHITECTURE.md](ARCHITECTURE.md) first.

## Setup

```sh
git clone <repo> && cd zengine
go version                # see go.mod for the minimum
make check                # gofmt + go vet + all tests (about a minute)
make build                # ./dist/zagent
```
Dependencies are deliberately few: the standard library, plus Bubble Tea / Bubbles / Lip Gloss and `golang.org/x/term` for the terminal UI. The web UI is plain HTML/CSS/JS embedded with `go:embed` (no Node build). Cross-compiling needs no C toolchain (`CGO_ENABLED=0`).

## Map of the code

| Package | Responsibility | Start reading at |
|---|---|---|
| `cmd/zagent` | Flags, headless runner, service host, subcommands (`init doctor sessions show tools`) | `main.go`, `run.go`, `host.go` |
| `internal/engine` | The loop: `runTurn`, `callModel`, `prepare`, `compact`, retry/pause, notes, commands, sub-agents | `engine.go`, `turn.go`, `call.go`, `prepare.go` |
| `internal/contextmgr` | Pure functions: log to context, repair, pruning policy, compaction prompt | `build.go`, `policy.go`, `repair.go` |
| `internal/provider` | One adapter per API family; streaming parse; error classification; URL rules | `provider.go`, `gemini.go`, `factory.go` |
| `internal/session` | Event log, blobs, lock, listing | `store.go`, `event.go`, `lock.go` |
| `internal/tools` | Registry (validation, timeout, truncation), path policy, file/shell/memory/skill tools, process manager, external tools | `tool.go`, `files.go`, `external.go`, `proc.go` |
| `internal/llm` | Canonical types; JSON-Schema argument validator | `types.go`, `schema.go` |
| `internal/control` | HTTP/SSE server and Go client | `server.go`, `client.go` |
| `internal/tui`, `internal/webui` | The two interfaces (clients of `control`) | `tui.go`, `assets/app.js` |
| `internal/config`, `prompt`, `render`, `sseutil` | Config files and home layout; system-prompt snapshot; transcript text; SSE reader | |

## Invariants: please keep them

1. **The log is the truth and is append-only.** Never edit or delete events; express a state change as a new event (`prune`, `compaction`) and derive from it.
2. **Rendering is deterministic.** `contextmgr.Build(events)` must give identical output for identical events, and adapters must encode with stable key order. A test (`TestRequestsOnlyEverAppend`) asserts ordinary requests extend the previous one byte-for-byte between context resets; keep it green. No newly generated timestamps, random IDs or map-order effects in rendering a stored conversation.
3. **The prompt prefix is frozen.** System prompt and tool list change only at session start or compaction. New information (workspace change, finished task, user note) is an `[info: ...]` part appended to the *end* of the next user turn.
4. **A model's mistakes are data, not errors.** Unknown tool, bad arguments, unsupported media, a tool crash or timeout all become a `ToolResult` the model can read, with a message that says what to change. Only genuine engine failures abort a turn.
5. **Every tool call gets a result**, even when the turn is cancelled, so the log stays valid for every provider (`contextmgr.Repair` is the safety net, not the plan).
6. **Error text is an interface.** Messages shown to the model or the user are part of the product: concrete, actionable, no jargon or stack traces.
7. **Interfaces hold no state.** The engine and the log own everything; a UI must be reconstructible from `GET /api/events` plus the stream.

## Recipes

### Add a provider adapter
1. Implement `provider.Adapter` (`Format`, `Build`, `ParseStream`) in `internal/provider/<name>.go`. Map canonical messages to the wire format (tool results travel in user messages; media through `mediaPayload`, which already degrades unsupported/removed attachments to text), and parse the stream into an `llm.Message` with usage and finish reason. Store the provider-native assistant encoding in `Message.Native` if the API needs it replayed.
2. Add the `Format…` and `Provider…` constants and the `case` in `Settings.Format`, `Endpoint` and `New` (`factory.go`), and the auth header default in `authHeaders`.
3. Tests, following `provider_test.go`: canned stream in, expected message/usage out; request-shape and determinism checks; error classification in `classifyHTTP` if the vendor has special error bodies.
4. Add a `live_test.go` case (build tag `live`) and a section in `docs/PROVIDERS.md`.

### Add a built-in tool
1. Implement `tools.Tool` (`Spec()` with a JSON-Schema built via `schema(...)`; `Run`). Return `tools.Fail("...actionable message...")` for anything the model can fix. Optional `Timeout()` (`-1` = manages its own lifetime).
2. Register it in `engine.DefaultRegistry` and add the name to `builtinNames` in `tools/external.go` so third-party tools cannot shadow it.
3. Write tests in the style of `tools_test.go` (`newEnv(t)` builds a temp home and workspace; call through `Registry.Invoke` so validation, timeout and truncation are exercised).
4. Document it in the README tool list and `docs/CUSTOMIZING.md` if users can configure it.

### Add a `config.json` key
Struct field with a `json` tag in `internal/config/config.go`, default in `Defaults()`, a rule in `Validate()` if it can be wrong, a line in the template in `home.go`, and a row in `docs/CONFIG.md`. Unknown keys are rejected, so a missing struct field makes the key fail loudly.

### Add a slash command
Add it to `builtinCommands` and the `switch` in `engine/commands.go` (return `CommandResult`; set `Exit`/`Shutdown`/`NewSession` for client-level effects) and test it in `commands_test.go`. Both UIs get it for free because they route `/...` input to `POST /api/command`; mention it in README and `docs/CUSTOMIZING.md`.

### Add a new log event kind
Add the `Kind` and fields in `session/event.go`; decide how `contextmgr.Build` treats it (most kinds are UI-only and ignored there); render it in `render/transcript.go`, `tui/render.go` and `webui/assets/app.js`; update `docs/CONTROL_API.md`.

## Testing

```sh
go test ./...                       # everything; about a minute, no network needed
go test -count=3 ./...              # flush out flakiness (the engine is concurrent)
go test ./internal/engine -run Subagent -v
go test -tags live -run Live -v ./internal/provider/   # opt-in: real endpoint (see the file header for env vars)
```

Patterns used throughout, worth reusing:
- **Scripted fake model** (`engine/harness_test.go`): implements `engine.Model`, renders each request with the *real* adapter (so sizes and encodings are genuine) and answers from a list of steps. Asserting on `f.model.Body(i)` checks exactly what would go on the wire.
- **Helper-process tools** (`tools/external_test.go`): the test binary re-executes itself as a third-party tool, so protocol tests run real subprocesses on every OS without needing Python or Node.
- **Real HTTP** for the control API (`control/control_test.go`) on a loopback port; **model-only** tests for the TUI with a fake `API` (`tui/tui_test.go`).
- Tests must not depend on wall-clock timing beyond generous timeouts; the engine's backoff `Sleep` is injectable for exactly that reason.

## Style

- `gofmt`, `go vet` clean (`make check`); no new dependencies without discussion. Prefer the standard library.
- Comments explain *why* and the invariant being kept, not what the next line does.
- Platform-specific code lives in `_windows.go` / `_unix.go` files with build tags; verify with `GOOS=linux go vet ./...` and `GOOS=darwin go vet ./...`.
- Errors that reach a human or the model: lower-case sentence, say what to do next.

## Pitfalls we already hit

- Attachments are **content-addressed**: identical files share one blob, so pruning a blob removes *every* occurrence (including a fresh copy of the same image).
- `contextmgr.Build` repairs the history in memory; use `BuildRaw` when you need to see what is actually in the log (resume uses it to persist "interrupted" results).
- A tool's own output folder must be unique per call, or stale files from an earlier call leak into the result.
- Never close a tool's stdin late: tools that read stdin in `args_json`/`env` mode would hang until the timeout.
- `WaitIdle` must treat "waiting for background tasks" as a resting state, or a finished headless run never exits.
- Windows: `taskkill /T` is how process trees die; PowerShell 5.1 has no `&&`; Git's GNU `tar` mistakes `C:` for a host (use relative paths).
