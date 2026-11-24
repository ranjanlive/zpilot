# zengine project analysis

Review date: 12 October 2026. Scope: the source tree supplied in this workspace, its documentation, build scripts, and automated tests. No production files, credentials, or live model endpoints were used. Temporary reproduction tests used dummy data and were removed after execution. Application source was not changed.

## Assessment

zengine is a well-structured early agent runtime. Its strongest qualities are deterministic conversation rendering, explicit recovery policies, provider isolation, and one engine shared by multiple interfaces. The design is coherent enough to extend without a rewrite.

The highest-value next work is correctness and security hardening. Four defects were reproduced, including a file-access policy bypass. Additional source inspection identifies concurrency and lock-ownership risks. I would treat this as a development release for trusted local use, with the credential-search bypass fixed before broader distribution. Passing the current suite does not establish the guarantees at these uncovered boundaries.

The supplied folder has no Git metadata, AGENTS.md, CI workflow, or LICENSE file. Consequently this review cannot assess history, branch hygiene, or hosted CI. The changelog describes an unreleased first development version.

## Architecture and design

The runtime flow is:

```text
CLI / terminal UI / browser UI
              |
     local authenticated HTTP + SSE
              |
          Engine worker
              |
   event log -> context builder -> provider adapter -> HTTP stream
              |                                      |
       blobs / metadata                       assistant response
              |                                      |
              +------------ tool execution <---------+
```

The Go tree contains 57 production files (12,154 lines) and 21 test files (5,297 lines), with 164 Test/Fuzz/Benchmark declarations found before temporary audit tests. Most features depend on the standard library; the main third-party dependency group supports the terminal UI. Browser assets are embedded and require no JavaScript build pipeline.

The package boundaries are sound:

| Area | Assessment |
|---|---|
| `session` | Clear persistence contract, sequence numbers, content-addressed blobs, durable subscriptions. Lock ownership and EOF recovery need hardening. |
| `contextmgr` | Pure rendering and repair functions are a strong choice. Pruning and compaction are explicit events rather than hidden mutations. |
| `provider` | Canonical types plus native replay handle provider differences without contaminating the engine. Stream completion needs stronger validation. |
| `engine` | Retry, turn execution, compaction, notes, and delegation are separated into comprehensible files. Administrative mutations bypass its worker serialization. |
| `tools` | Argument validation, readable errors, panic recovery, output truncation, and process management are useful shared infrastructure. Policy enforcement is inconsistent across access paths. |
| `control` | Loopback binding, token authentication, Host/Origin checks, upload limits, and durable SSE replay show careful local API design. |
| `tui` / `webui` | Shared server API avoids separate execution engines. The browser uses DOM construction rather than injecting model HTML. Browser behavior needs dedicated tests. |

Important strengths to preserve:

- System prompt snapshots and stable tool ordering keep provider caching predictable between context changes.
- Blob storage avoids duplicating binary payloads in the event log.
- Tool mistakes are returned as actionable data, allowing the model to recover.
- Interrupted tool calls get repair results instead of leaving malformed provider history.
- The fake-model harness renders with real adapters; request-size and serialization assertions test actual behavior.
- Injectable sleep makes retries testable without waiting through real backoff intervals.
- Background processes and detached interfaces support long-running work without keeping a UI open.

The README's “requests only ever grow” statement needs qualification: pruning, compaction, degraded fallback, and context repair can change earlier rendered content. The underlying append-only log and frozen prefix between those transitions are the stronger, more precise guarantees.

## Findings requiring action

### 1. High: recursive text search bypasses protected-file checks — reproduced

Location: `internal/tools/files.go:682`, especially the direct `os.Open` at line 683 and recursive call at line 749; compare `internal/tools/paths.go`.

`search_text` validates its starting directory with `ResolvePath`, then opens discovered files directly. If the workspace contains the agent home, searching the workspace can return the home's `.env` and session-log contents even though direct `view_file` calls reject them. File symlinks encountered during traversal can also lead outside the allowed roots because the final file is not resolved again.

Reproduction: a temporary workspace enclosed a temporary home containing `API_KEY=secret`. `view_file` denied the credential file; `search_text(pattern="API_KEY")` returned that dummy credential.

Impact: protected information can enter model context despite command execution being denied. This contradicts a documented security boundary.

Recommendation: enforce path and secret checks on every visited/read file, prune protected subtrees, and test nested-home and symlink cases. Audit memory and skill helpers too: they perform direct file I/O and can follow links independently of the file-tool policy. Those additional paths were inspected but not reproduced here.

### 2. High: lock ownership can split between live engines — source inspection

Location: `internal/session/lock.go:68`, stale/unreadable removal at line 91, and `Release` at line 140.

An unreadable or stale lock is removed without confirming the owner is dead. There is a creation window between exclusive file creation and writing valid JSON where another contender can read an empty file and remove it. A process delayed for more than ten seconds can also lose its lock while still alive. Heartbeat writes and release are not fenced by a unique ownership identifier, so an old owner can overwrite or remove a replacement owner's lock.

Impact: the single-writer invariant can fail, creating conflicting event sequence numbers and unsafe simultaneous log recovery.

Recommendation: use an operating-system lock held for the process lifetime, with endpoint metadata stored separately. If leases remain, require owner identity and fencing, and do not interpret partially published metadata as permission to steal a live lock. Add multiprocess acquisition and delayed-heartbeat tests. This concurrency scenario was not dynamically reproduced.

### 3. High: a resumed parent reuses old subagent context — reproduced

Location: `internal/engine/subagent.go:84`.

Child IDs are `sub-1`, `sub-2`, etc., from an in-memory counter that resets when the parent engine is reconstructed. Child logs persist under the parent. The first delegation after restart therefore opens the previous `sub-1` session instead of starting a fresh one.

Reproduction: run one delegation, close and reopen the same parent session, then delegate a second task. The second child's first request contained the previous child's task and private report.

Impact: task isolation and the advertised fresh-context behavior break; unrelated history consumes tokens and can influence the report. Child usage totals can also be counted again from restored history, while parent totals added only in memory are not reliably reconstructed on resume.

Recommendation: generate restart-safe unique child IDs and persist delegated usage attribution in parent events. Add restart tests for both context isolation and counters.

### 4. High: incomplete streams can be accepted as successful answers — reproduced for OpenAI format

Location: `internal/provider/openai.go:255` and `mapOpenAIFinish`; similar EOF/default-stop behavior exists in the Gemini and Anthropic parsers.

The parser stops on EOF without requiring a terminal finish signal. An empty finish reason maps to normal completion. A gateway that closes an HTTP stream cleanly before the model finishes can therefore produce a nonempty partial answer that the engine marks done. Complete-looking tool arguments received before termination may also be executed.

Reproduction: feed the OpenAI adapter one content delta followed by EOF, without `finish_reason` or `[DONE]`. It returns `FinishStop` with no error.

Recommendation: track completion according to each protocol and distinguish premature EOF from a completed response. Support deliberately permissive custom gateways through explicit configuration if necessary. Test truncation between events and during tool arguments. Gemini and Anthropic behavior was established by reading the parsers, not separate dynamic reproductions.

### 5. High: administrative operations are not serialized with turns — source inspection

Locations: `internal/engine/commands.go:81`, `SetWorkspace` at line 194, `internal/engine/prepare.go:155`, and `internal/engine/engine.go:388`.

`/compact` checks state and then calls compaction outside the worker. Another request can start between those operations. The guard also permits compaction during `StatePaused`, while a paused model request is still active. `/workspace` mutates the shared tool environment while tools may be using it; `Workspace()` reads the value without locking.

Impact: model calls and context snapshots may overlap, state can be restored incorrectly, and a running tool can observe changing path roots. The read/write paths also indicate Go data races.

Recommendation: route compaction and workspace changes through the engine worker or an explicit exclusive operation boundary. Give each tool invocation an immutable environment snapshot. Add concurrent API tests and run the race detector in CI. Race detection could not run in this environment because CGO is disabled and no C compiler was found on PATH.

### 6. Medium: log replay disagrees with persisted recovery at EOF — reproduced

Location: `internal/session/store.go:79`, especially lines 103–114.

A valid JSON event without a final newline is appended to the in-memory event list, but its bytes are not included in the `good` offset. Recovery truncates it from disk anyway.

Reproduction: write one valid event without a newline. The first `Open` returns one event; close and open again, and it returns zero.

Impact: the same session can replay differently across consecutive resumes. This is an edge case around crashes or externally repaired logs, not a failure of normal newline-terminated writes.

Recommendation: either reject/discard the unterminated record before adding it to memory, or retain it and restore its newline. Keep the disk and memory recovery decision identical.

### 7. Medium: upload names collide after server restart — source inspection

Location: `internal/control/server.go:371`, generated path at line 382.

Uploads use a process-local counter and the original sanitized name in a persistent per-session directory. Restart resets the counter; uploading the same name again can reuse `1-name.png`. `os.Create` overwrites the existing file.

Impact: saved attachment paths can change contents. Old content-addressed blobs remain stable, but queued paths and later reopen operations can refer to new data.

Recommendation: use unique temporary-file creation or random identifiers with exclusive creation. Test uploading the same name across server restarts.

### 8. Medium: long-running services lack complete resource bounds — source inspection

Locations: `internal/tools/external.go:265`, `internal/tools/proc.go`, and `internal/session/store.go:215`.

JSON-output tools capture stdout in an unbounded `bytes.Buffer`; the regular process output cap does not protect this separate buffer. Completed processes remain in the manager with their retained output. All historical events remain in memory, and request preparation repeatedly copies/scans the full log even after compaction. The web interface downloads and renders the entire event history.

Recommendation: bound JSON stdout before parsing, cap retained task history and running tasks, introduce log checkpoints/indexes while preserving auditability, and page or virtualize browser transcripts. Benchmark large logs and many completed commands before making scaling claims. No load benchmark was run in this review.

## Verification and test gaps

`go test ./... -cover` passed for all packages using Go 1.27.0 on Windows. `go vet ./...` passed, and `gofmt -l cmd internal` produced no output after removal of the temporary reproduction files. Builds passed for all six advertised targets: Windows, Linux, and macOS on amd64 and arm64. Binaries were written to a temporary build directory, leaving the project's `dist` folder unchanged. Initial sandbox runs were invalidated by denied file renames and loopback connections; the successful suite ran with those restrictions removed.

| Package | Statement coverage |
|---|---:|
| CLI (`cmd/zagent`) | 23.1% |
| config | 87.5% |
| contextmgr | 86.4% |
| control | 74.0% |
| engine | 82.8% |
| llm | 67.1% |
| prompt | 98.2% |
| provider | 78.0% |
| render | 77.2% |
| session | 83.0% |
| sseutil | 96.2% |
| tools | 76.0% |
| tui | 71.4% |
| webui | 0.0% |

Coverage measures executed Go statements, not correctness or JavaScript coverage. The temporary reproduction checks passed by asserting that each suspected bug occurred; they are evidence of defects, not passing regression tests for fixed behavior.

The next tests should target protected recursive reads, competing lock owners, resume after delegation, premature stream termination, concurrent compact/workspace commands, and upload collisions after restart. Add CLI service-start/attach/exit integration tests and browser tests for reconnect, upload, streaming, stop, and slash commands. Real Linux/macOS process execution, interactive terminal operation, live model compatibility, and race detection remain unverified in this review.

## Product and maintenance priorities

1. Fix the protected-file search bypass, lock ownership, fresh-child identity, and stream completion handling before expanding features.
2. Serialize engine mutations, then add race and multiprocess tests.
3. Implement the existing approval callback end to end in the CLI and both UIs. Currently default command execution is effectively denied, as the README correctly explains. Explicit approvals would make the agent materially more useful without requiring global `allow`.
4. Add resource budgets and durable attribution for subagent usage. A round cap limits iterations, but is not a request/token/cost budget.
5. Add cross-platform CI, release smoke tests, and browser integration tests. The supplied release scripts are useful, but no CI configuration is present.
6. Decide licensing before public distribution; a missing LICENSE leaves reuse terms undefined in this snapshot. Choose an appropriate module path if publishing reusable Go packages. All implementation currently lives under `internal`, which is suitable for an application but prevents external modules from importing the engine as a library.

Keep the current package structure and fake-model harness. Targeted hardening is the appropriate next step; the evidence does not justify a wholesale architecture rewrite.
