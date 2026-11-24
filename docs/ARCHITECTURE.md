# Architecture

## Repository layout

| Path | What it is |
|---|---|
| `cmd/zpilot` | The single binary: daemon, CLI, `engine` passthrough, `agent-tool` |
| `internal/model` | Domain types and validation (agents, models, threads, messages, schedules, channels, proposals) |
| `internal/store` | `state.json` plus one file per thread, written atomically; `audit.jsonl` |
| `internal/vault` | AES-256-GCM secret store; master key sealed with DPAPI on Windows |
| `internal/library` | Tool library (ZTOOLS registry install, checksum verification, safe unzip) and skill library |
| `internal/profile` | Turns an agent profile into a ZEngine home (instruction, config, tools, skills) |
| `internal/core` | The daemon brain: message bus, dispatcher, collaboration rules, schedules, channels, proposals |
| `internal/channels` | Telegram long-polling adapter (outbound side of every channel) |
| `internal/server` | HTTP API, authentication, webhooks, the loopback agent RPC, and the embedded dashboard (`web/`) |
| `internal/agenttool` | The executables behind `list_agents`, `ask_agent`, `tell_agent`, `manage_status`, `manage_propose` |
| `internal/auth` | Password hashing (PBKDF2), sessions, login rate limiting |
| `internal/trigger`, `internal/platform` | Taken from ZSched: schedule maths, Job Objects / process groups, single-instance lock, tray |
| `zengine/` | ZEngine (from `zagent`), embedded unchanged except that `cmd/zagent` became the exported `zengine/cli` package |

## Data directory

```
~/.zpilot/
  state.json, state.json.bak    agents, models, schedules, channels, proposals, threads index, settings
  threads/<id>.json             messages of one conversation
  audit.jsonl                   append-only audit log
  secrets.json, master.key      encrypted secrets and the (sealed) key
  endpoint.json                 URL + admin token of the running daemon (owner-only)
  library/tools/<id>/           installed tools
  library/skills/<name>/        skills
  agents/<id>/home/             each agent's ZEngine home (sessions, memory, scratchpad)
  workspaces/<username>/        default agent workspaces
  runs/<run id>.log             engine output of each run
```

## Life of a message

1. A message comes in from the dashboard, the CLI, a channel, a schedule or another agent. It is stored in its thread as a **request** with status `queued`.
2. The **dispatcher** starts a request only if all of these hold:
   - the global parallel limit allows it
   - the agent's own parallel limit allows it
   - no other run of the same agent is active in the same thread (each engine session is exclusive)
   - the thread's run budget is not used up
3. **Materialise.** The agent's home is refreshed from its profile (per-agent mutex, and only changed files are rewritten).
4. **Run.** `zpilot engine --home … --workspace … --session th-<thread> --prompt-file …` starts as a child process confined in a Job Object (Windows) or a process group (Unix). The model settings and key arrive as `ZAGENT_*` environment variables. Progress lines go to the dashboard live and the full output goes to `runs/<id>.log`.
5. **Route the answer.** The answer, or the error, becomes a **reply** to the sender:
   - **The owner or a webhook:** the reply is stored and any waiters are woken.
   - **A channel:** the reply is also sent out over the channel.
   - **An agent that used `ask_agent`:** its tool call, still blocked, receives the reply.
   - **An agent that used `tell_agent`:** the reply is queued as a new turn for that agent, provided the hop and budget limits allow it.

## Collaboration rules (enforced in `core.AgentSend`)

- **Who may talk.** The caller must list the target in `contacts`, and the target must accept the caller (`accept_from`). Nobody may message themselves.
- **Hop limit.** Each request is `hop + 1` deeper than the run that sent it, and the default limit is 6.
- **Deadlock detection.** For `ask_agent`, the request's **chain** is the list of agents blocked waiting on it. Asking an agent that is already in the chain is refused with an explanation, because it would deadlock.
- **Run budget.** Each conversation has a run budget (default 40) that stops runaway loops.
- **Identity.** A collaboration tool can only act as its own agent (per-agent token) and only inside a ZPilot conversation (the session name ties it to the thread). Sub-agents cannot message other agents.

## Engine integration

ZEngine is used through its stable command-line contract:

- **Headless run:** exit codes `0` ok, `1` turn failed, `2` configuration, `3` session busy (requeued).
- **External tools:** the collaboration tools are ordinary external-tool manifests that run `zpilot agent-tool <name>`.
- **Environment overrides:** `ZAGENT_PROVIDER`, `ZAGENT_BASE_URL`, `ZAGENT_API_KEY`, `ZAGENT_MODEL`, and so on.

The only change to the engine source is that `cmd/zagent` became the package `zengine/cli`, with `Run`, `Version` and `SelfArgs`, so that `zpilot` can embed it.
