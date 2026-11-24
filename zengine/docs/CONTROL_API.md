# Control API and on-disk formats

Every engine process (`zagent -p ...`, or the background service behind `--ui cli|web`) runs an HTTP server on the loopback interface. The bundled terminal and web UIs are ordinary clients of it, and so can yours be: a desktop app, an editor plugin, a chat bridge. The Go client is `internal/control/client.go`; the server is `internal/control/server.go`.

## Finding and authenticating

The engine publishes its endpoint in the session's lock file, `<home>/sessions/<id>/lock.json` (readable only by you):

```json
{ "pid": 1234, "started": "2026-10-11T22:35:39Z", "heartbeat": "2026-10-11T22:36:01Z",
  "role": "service", "addr": "127.0.0.1:61458", "token": "b1e8...c6531" }
```

- `lock.json` contains discovery metadata, refreshed every 2 s. The separate `owner.lock` is held exclusively by the OS for the engine's lifetime. A delayed heartbeat cannot transfer ownership; process death releases the OS lock. Clients should reconnect or wait when metadata is temporarily unavailable rather than delete ownership files.
- Send the token as `Authorization: Bearer <token>` (or `?token=` for `EventSource`, which cannot set headers).
- The server also requires `Host` to be `127.0.0.1:<port>`, `localhost:<port>` or `[::1]:<port>` (DNS-rebinding defence) and rejects any request whose `Origin` header is not the same host. Non-browser clients send no `Origin` and are unaffected.
- Browser URL for the bundled UI: `http://127.0.0.1:<port>/#token=<token>` (the fragment is never sent to a server).

Errors are `{"error": "message"}` with a 4xx/5xx status.

## Endpoints

| Method and path | Body / query | Result |
|---|---|---|
| `GET /api/state` | | `state` object (below) |
| `GET /api/events?since=N` | `since`: last seen `seq` (default 0) | JSON array of log events with `seq > N` |
| `GET /api/stream?since=N` | `since` or `Last-Event-ID` | Server-Sent Events (below) |
| `POST /api/prompt` | `{"text": "...", "attachments": ["/abs/path.png"]}` | `202 {"status":"queued"}`; queued behind running work |
| `POST /api/note` | `{"text": "..."}` | `202`; attached to the end of the next request |
| `POST /api/command` | `{"line": "/status"}` | command result `{handled, reply, exit, shutdown, new_session}` |
| `POST /api/stop` | | interrupts current work; discards queued prompts |
| `POST /api/resume` | | retries now when paused for network trouble |
| `POST /api/shutdown` | | ends the engine process |
| `POST /api/upload?name=file.png` | raw file body (max 200 MB) | `{"path": "...", "size": N}`: a path inside the scratchpad you can pass in `attachments` |
| `GET /api/blob/{id}` | | an attachment's bytes (images inline; other types as downloads) |

`state`:
```json
{ "session": "notes", "title": "first prompt text", "workspace": "/w", "model": "m",
  "state": "idle|working|compacting|paused|waiting_bg|closed", "detail": "",
  "counters": { "context": 4464, "prompt": 15645, "completion": 261, "thought": 295, "cached": 0, "requests": 4, "estimated": false },
  "compact_at": 800000, "tasks": 0, "last_seq": 57, "pid": 1234, "role": "service", "version": "0.1.0" }
```
`counters.context` is the size of the latest prompt (what the model holds now); the others accumulate over the session. `estimated` is true when any figure was estimated (bytes/4).

## Event stream

`GET /api/stream` replays the log from `since`, then follows it live, and also carries transient events. Two event names:

```
id: 42
event: event
data: { ...a log event, see below... }

event: live
data: { "kind": "text", "text": "partial answer", "tokens": {...}, "at": "..." }
```

`event` frames have increasing `seq` ids; a reconnecting client passes the last one and misses nothing. `live` frames are **not** stored (the durable result arrives later as an `event`) and carry:

| `kind` | Fields | Meaning |
|---|---|---|
| `text` | `text` | a streamed piece of the answer |
| `thought` | `text` | a streamed piece of reasoning |
| `tool_call` | `name` | the model started a tool call |
| `tool_run` / `tool_done` | `name`, `detail` | a tool started / finished (`detail` is a short argument summary or `ok`/`error`) |
| `state` | `state`, `detail` | the engine's state changed |
| `usage` | `tokens` | live counters; during streaming `completion` includes an estimate and `estimated` is true |
| `retry` | `detail`, `text` | a failure is being retried or compaction happened; `text == "reset_stream"` means discard the partial answer shown so far |

Keep-alive comments (`: ping`) arrive every 15 s.

## The session log: `events.jsonl`

One JSON object per line, append-only, `seq` starting at 1. A torn final line (crash mid-write) is discarded on load.

```json
{"seq": 5, "time": "2026-10-11T17:04:11Z", "kind": "message", "turn_no": 1, "message": {...}, "consumed": [3]}
```

| `kind` | Fields | Meaning |
|---|---|---|
| `system` | `text` | The system-prompt snapshot. Written at start and after each compaction; the **last** one in the log is in force. |
| `message` | `message`, `turn_no`, `consumed` | A conversation message. `consumed` lists the `note` events folded into it. |
| `note` | `text`, `state` | A queued `[info: ...]` note; `state: "wake"` means it should wake an idle agent. Delivered as an `info` part of a later message. |
| `compaction` | `compaction{summary, kept_prompts, reason, removed_paths, ...}` | Everything before this event is replaced by the summary. |
| `prune` | `blobs[]`, `detail` | Attachment payloads removed from context (the files stay on disk). |
| `usage` | `usage`, `request_size` | Token accounting of one model request. |
| `status` | `state`, `detail` | `working`, `done`, `error`, `stopped`, `paused`, `warning`. |
| `turn` | `state` (`start`/`end`), `turn_no` | Boundaries of a user prompt. |

A **message**:
```json
{ "role": "user|assistant",
  "parts": [
    {"type": "text", "text": "..."},
    {"type": "media", "media": {"kind": "image|audio|video|pdf", "mime": "image/png", "blob": "<sha256>", "size": 1234, "name": "a.png", "path": "/abs/a.png"}},
    {"type": "info", "text": "[info: ...]"},
    {"type": "thought", "thought": {"text": "...", "signature": "..."}},
    {"type": "tool_call", "call": {"id": "...", "name": "view_file", "args": {"path": "a.go"}, "args_error": "...", "synthetic": false}},
    {"type": "tool_result", "result": {"call_id": "...", "name": "view_file", "output": "...", "is_error": false, "media": [ ... ]}}
  ],
  "native": {"format": "gemini|openai|anthropic", "raw": [ ... provider-native encoding ... ]},
  "usage": {...}, "finish_reason": "stop|tool_calls|length|filtered|other" }
```
Tool results travel inside `user` messages. `native` is the exact provider encoding of an assistant message, replayed verbatim when the same provider is used again (it carries thought signatures). `args_error` holds raw text when the model produced arguments that were not valid JSON.

## Other files in a session folder

| Path | Content |
|---|---|
| `blobs/<2 hex>/<sha256>` | Attachment payloads, content-addressed (identical files are stored once). Referenced by `media.blob`. |
| `meta.json` | `id`, `title`, `workspace`, `provider`, `model`, `created_at`, `updated_at`. |
| `lock.json` | Live-engine discovery metadata and authenticated endpoint (above). |
| `owner.lock` | OS-held single-writer ownership; its file can remain after release. |
| `service.log` | Output of the background service. |
| `subagents/<id>/` | The session of each sub-agent, in the same format. |

The log is safe to read while the engine runs (append-only; ignore a partial last line). Never write to it from outside: the engine is its only writer.

## Example (shell)

```sh
LOCK=~/zagent/sessions/notes/lock.json
ADDR=$(jq -r .addr $LOCK); TOKEN=$(jq -r .token $LOCK)
curl -s -H "Authorization: Bearer $TOKEN" http://$ADDR/api/state
curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -d '{"text":"summarise TODO.md"}' http://$ADDR/api/prompt
curl -N -H "Authorization: Bearer $TOKEN" "http://$ADDR/api/stream?since=0"
```
