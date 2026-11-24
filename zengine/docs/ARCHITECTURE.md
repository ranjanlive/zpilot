# Architecture

## The log is the truth

A session is `events.jsonl`, an append-only log (plus a content-addressed `blobs/` folder for attachments). Everything else is derived from it:

```
 events.jsonl --Build()--> Context{system, messages} --adapter--> HTTP request
      ^                                                    
      |  append only: user/assistant messages, tool results, notes,
      |  prune, compaction, usage, status
```

- **Caching.** `contextmgr.Build` is a pure function of the log and adapters encode with sorted keys, so two renders of the same log are byte-identical. Because history only ever grows at the end, each request extends the previous one and provider prefix caches hit. There is a test that asserts this property across a tool loop.
- **State changes are events first.** A prune or compaction is written to the log, then the next render reflects it. Resuming a session after a crash reproduces exactly what the model last saw.
- **Provider-native replay.** An assistant message is stored both canonically and in the provider's own encoding (Gemini parts with `thoughtSignature`, Anthropic blocks with thinking signatures). When the same provider is used again the native form is replayed byte-for-byte; after a provider switch the canonical form is used.
- **Crash safety.** A torn final line is discarded and the file truncated; a corrupt middle line is a hard error. Tool calls left unanswered by a crash get a persisted "interrupted" result on resume, so the history is valid for every provider.

## One engine, many interfaces

```
 zagent -p ...   (headless)    \
 zagent --ui cli               ---->  control API (127.0.0.1, token)  ---->  Engine ---> provider
 browser (--ui web)            /      state / events / SSE stream /         tools, sessions,
                                       prompt / note / command / stop       compaction, retry
```

Every engine process hosts the control server and publishes its address and token in `sessions/<id>/lock.json`. Single-writer ownership is held separately through the OS lock on `owner.lock`; heartbeats are status metadata and do not transfer ownership. `--ui cli|web` starts a *detached service* process if the session has none, then attaches as a client. Interfaces replay history from the log, then follow a Server-Sent-Events stream (durable events with sequence numbers, plus transient token deltas), reconnecting from the last sequence they saw. Closing one never interrupts the agent.

## A turn

1. The prompt, attachments and any queued notes become one user message (notes last).
2. `prepare` renders the context and loops until the request is valid: age-based attachment pruning (batched), exact body-size check (prune oldest attachments, else compact), token-threshold compaction.
3. `callModel` sends with retry/backoff/auto-pause and applies the remedy for each classified failure: 413 shrinks and resends; a context-window error compacts; a 400 retries once in a portable text-only form; an empty reply is re-requested.
4. The response is stored, usage recorded, and tool calls run one at a time. Unknown tools, invalid or truncated arguments, unsupported media and crashes all become tool results the model can read.
5. Repeat until the model answers without tools, with a cap on rounds.

Gemini builds also preflight the encoded function-response attachments, after
`extra_body` is merged and before request-size measurement. Text placeholders
inside `functionResponse.parts` are moved to `response.attachment_notes`; media,
tool output, call IDs and thought signatures are preserved. Ambiguous nested
parts produce a local error with the exact request path instead of a provider
400. Valid requests retain their original bytes. This is a focused guard for
function-response attachment structure, not a complete remote API validator.

`Stop` cancels the in-flight request and tools but still writes a result for every call, so the log stays valid.

## Compaction

A summarisation request (the whole history plus an instruction that attachments will be lost, so each must be *described*) is sent with tool calls forbidden. It is fitted under the size limit by dropping the oldest attachments, then the oldest messages. The log gets a `compaction` event and a fresh system-prompt snapshot (the one moment the prefix may change); later renders start from the summary plus the latest user prompt.

## Sub-agents

The `subagent` tool runs a nested engine with its own session (inside the parent's), a fresh context and a restricted tool set (read-only by default, no further nesting). Only its final report returns, so media-heavy work never enters the parent's context. Stopping the parent stops the child.

## Measured behaviour this design responds to

Earlier experiments with the Google Antigravity SDK against a Gemini-format gateway shaped these choices: the endpoint rejects bodies over 32 MiB (HTTP 413) and the SDK re-sent every attachment on every turn until it hit that wall, after which its session was unusable; compaction there ignored media tokens; and history trimming could silently drop the oldest steps. zengine measures bodies exactly, prunes before sending, treats a 413 as recoverable, and never edits history silently.

## Packages

| Package | Responsibility |
|---|---|
| `llm` | Canonical messages/parts/usage, JSON-Schema argument validation |
| `provider` | Request encoding and stream decoding per API family, URL rules, error classes, stall watchdog |
| `session` | Event log, blobs, locking, listing |
| `contextmgr` | Rendering, repair, attachment/size policy, token estimates, compaction prompt |
| `tools` | Registry, path policy, file tools, process manager, shell tools, memory, skills, external tools |
| `engine` | The loop, retry/pause, compaction, notes, commands, sub-agents |
| `control` / `webui` / `tui` | Local API and its two interfaces |
| `config` / `prompt` / `render` | Files and home layout, system-prompt snapshot, transcript text |
