# ZPilot

**Build teams of AI agents that run on your own machine or server.**

ZPilot lets you create as many agents as you like, each with its own name, `@username`, icon, instructions, skills, tools, model, workspace and security level. Agents talk to each other by `@username`, work on schedules, answer on Telegram or webhooks, and a built-in `@manager` agent helps you run the whole setup. You get one binary with a web dashboard and a full CLI. It runs on a laptop or on a headless VPS.

```
zpilot desktop        # start everything: daemon, app window, tray icon, notifications
zpilot ui             # or just open the dashboard in your browser, already signed in
zpilot send @lead "Research the top 3 Go web frameworks and draft a comparison"
```

ZPilot is part of the Zyreca stack:

| Project | Role |
|---|---|
| **ZEngine** | The agent runtime. It is embedded here as `zengine/`, from the `zagent` repository. |
| **ZPilot** | This repository: agents, collaboration, channels, schedules, dashboard. |
| **ZSched** | A standalone scheduler and supervisor. ZPilot reuses its trigger and process code. |
| **ZTOOLS** | The tool registry. Install any tool with one click or `zpilot tools install <id>`. |

---

## Features

- **Agents as profiles.** Each agent has a name, `@username`, icon and colour, description, instructions, model, workspace, skills, tools, contacts and limits.
- **Engine-enforced security levels.** `restricted`, `standard`, `trusted` and `full` are written into each agent's engine configuration. A prompt cannot talk its way past them.
- **Approve each command.** Trusted agents can require your approval for every shell command. Requests appear in the chat, the **Needs you** inbox, browser notifications and desktop notifications. Allow or deny with an optional reason, which the agent sees.
- **Live streaming.** Answers stream in word by word, and you see tool calls as they happen.
- **Agent-to-agent collaboration.**
  - `ask_agent` waits for a colleague's answer.
  - `tell_agent` hands work off, and the reply comes back later as a new turn.
  - `list_agents` shows who an agent may contact.
  - The daemon enforces contacts, accept-lists, hop limits, run budgets and deadlock detection.
- **Visual workflows.** Build pipelines on a canvas:
  - steps run in parallel when their inputs are ready
  - a step can use earlier results with `{{steps.<id>.output}}`
  - a step can run only when an earlier output contains certain text
  - you can run a workflow on demand, on a schedule or from the CLI, and watch it live
- **Built-in `@manager`.** It inspects your setup and *proposes* agents, schedules and tool installs. Nothing changes until you approve.
- **Channels:**
  - **Telegram** (long polling, no public URL needed)
  - **Discord** (gateway bot: answers DMs, and messages in servers when mentioned)
  - **Email** (IMAP/SMTP for Gmail, Outlook or any provider; mail that fails SPF/DKIM/DMARC is dropped)
  - **WhatsApp** (official Cloud API, with signed webhooks)
  - **Webhooks**

  Unknown senders are refused and listed so you can allow them with one click.
- **Schedules** for agents or workflows. They use the same trigger code as ZSched. A run missed while the daemon was down fires once at start-up.
- **ZTOOLS.** Browse and install tools from the registry. Downloads are checked against published SHA-256 sums.
- **Skills library.** Reusable instruction packs you can attach to any agent.
- **Sandboxing.** Per-agent memory and process limits are enforced with Job Objects on Windows and a memory limit on Linux. On Linux, an agent can also be isolated so the whole file system is read-only apart from its workspace (needs bubblewrap).
- **Multiple users** with three roles:
  - **viewer**: reads everything
  - **operator**: also talks to agents, runs workflows and stops runs
  - **admin**: everything, including settings, approvals and users
- **Desktop app.** `zpilot desktop` opens an app window, puts an icon in the system tray (Windows) and shows native notifications on Windows, macOS and Linux. It can also start at login.
- **Responsive dashboard.**
  - It works on phones and tablets.
  - Press `Ctrl/⌘+K` for the command palette, or `g` followed by a letter to jump to a page.
  - Light and dark themes are included.
- **Crash-safe.** Each agent session is an append-only event log. A run that was interrupted is never re-run blindly.
- **Observability.** You get run history with token counts, per-run logs and a full audit log.

## Install

You need **Go 1.26+** to build from source.

```bash
git clone https://github.com/ranjanlive/zpilot
cd zpilot
go build -o zpilot ./cmd/zpilot          # Windows: go build -o zpilot.exe ./cmd/zpilot
```

To cross-compile release binaries for Windows, Linux and macOS, run `./scripts/build.sh` or `scripts\build.ps1`.

## Quick start

```bash
zpilot start                       # runs in the background; data in ~/.zpilot
zpilot ui                          # opens the dashboard

# or from the CLI:
zpilot models add --name "Gemini Flash" --provider gemini --model gemini-2.5-flash   # prompts for the key
zpilot agents create --username researcher --name "Researcher" --description "Finds and summarises sources"
zpilot agents create --username lead --name "Team Lead" --contacts researcher
zpilot send @lead "Find three recent articles on WebAssembly and summarise them"
```

In the dashboard, **Conversations** is where you chat. Type `@` to pick an agent. When an agent pulls in a colleague, the agent-to-agent messages appear inline as dashed bubbles.

### Running on a VPS

```bash
zpilot admin password              # create the "admin" account (min. 10 characters)
zpilot serve --listen 0.0.0.0:7700 --tls-cert cert.pem --tls-key key.pem
# or behind a TLS reverse proxy (Caddy, nginx):
zpilot serve --listen 127.0.0.1:7700        # proxy https://agents.example.com -> 127.0.0.1:7700
```

Add more people with `zpilot users add NAME --role operator` or on the **Users** page.

ZPilot refuses to listen on a public address unless an admin account exists and TLS is configured. A reverse proxy counts as TLS if you pass `--insecure-http`. A sample systemd unit is in [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md).

---

## How it works

```
 Dashboard / CLI / Telegram / Webhooks / Schedules
                    │
                    ▼
        ┌──────────────────────────┐   loopback RPC (per-agent token)
        │  zpilot daemon (core)    │◄──────────────────────────────┐
        │  agents · threads · bus  │                               │
        │  dispatcher · policies   │                               │
        └──────────┬───────────────┘                               │
                   │ one engine process per run                    │
                   ▼                                               │
   zpilot engine --home agents/<id>/home --session th-<thread>  ────┘
   (ZEngine: model loop, file tools, sub-agents, memory, skills,
    external tools, incl. ask_agent / tell_agent)
```

- Each agent profile is **materialised** into a ZEngine home: `system_instruction.md`, a `config.json` derived from the security level, `skills/` and `tools/`.
- Model credentials go to the engine **only as environment variables**, and the engine removes them from every command and tool it starts. API keys are never written into agent folders, and they are encrypted at rest in the vault.
- A conversation (thread) gives each participating agent its own engine session, so every agent keeps its own context.

More detail is in [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md). The threat model is in [`docs/SECURITY.md`](docs/SECURITY.md).

## CLI reference

```
zpilot desktop [--minimized] | serve | start | stop | status | ui
zpilot agents list | show @a | create | update @a | delete @a | enable @a | disable @a
zpilot send @agent "message" [--thread ID] [--wait=false]
zpilot threads list | show ID | delete ID
zpilot runs list | log ID | stop ID
zpilot workflows list | show ID | import FILE | run ID [--input T] | runs | cancel RUN | delete ID
zpilot models list | add | delete ID | test ID | default ID
zpilot tools search [TEXT] | list | install ID|PATH | remove ID
zpilot skills list | add DIR | new NAME | remove NAME
zpilot schedules list | add | delete ID | run ID
zpilot channels list | add-telegram | add-discord | add-email | add-whatsapp | add-webhook | allow ID SENDER | delete ID
zpilot approvals list | approve ID | deny ID [--reason T]
zpilot users list | add NAME --role R | password NAME | role NAME --role R | delete NAME
zpilot admin password | clear-password
zpilot audit
zpilot engine ...        # the raw ZEngine command line
```

Global flag: `--data DIR` (default `~/.zpilot`, or the `ZPILOT_DATA` environment variable).

## Development

```bash
go test ./...                      # includes end-to-end tests with real engine subprocesses
go vet ./...
```

The end-to-end suite (`internal/e2e`) builds `zpilot`, runs real engine subprocesses against a scripted OpenAI-compatible model, and checks the main flows:

- an owner chatting with an agent
- one agent asking another
- a refused cycle
- enforced contacts
- tell plus report-back
- manager proposals needing approval
- webhooks
- security levels reaching the engine
- no API key ever written to disk

## Status

The current version is **0.2.0 (preview)**. Next on the roadmap:

- a native tray icon on macOS and Linux (notifications already work there)
- Slack and Microsoft Teams channels
- workflow import and export in the dashboard
- container-based isolation for Windows and macOS

## License

Apache License 2.0. See [LICENSE](LICENSE). ZEngine (`zengine/`) is included under the same license.
