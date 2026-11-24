# Changelog

## 0.2.0 (preview), 2026-11-24

- **Command approvals.** Trusted and full agents can require approval for every shell command, through an opt-in bridge in the engine. You can decide in the chat, the inbox or the CLI, and denials carry a reason back to the agent.
- **Live streaming.** Answer text, tool calls, retries and state changes stream from the engine to the dashboard.
- **Visual workflows.** A graph canvas with parallel steps, `{{input}}` and `{{steps.<id>.output}}` templates, conditional steps, live run view and cancellation. Workflows can be scheduled or run from the CLI.
- **New channels.**
  - Discord, through a gateway bot with its own tested WebSocket client.
  - Email over IMAP/SMTP: SPF/DKIM/DMARC checking, reply threading and filtering of quoted text.
  - WhatsApp Cloud API, with verified webhook signatures.
- **Multi-user.** Viewer, operator and admin roles; password accounts; protection against removing the last admin; per-user sessions.
- **Sandboxing.** Per-agent memory and process limits (Windows Job Objects, Linux address-space limit), plus optional bubblewrap isolation on Linux.
- **Desktop app.** `zpilot desktop`: app window, Windows tray icon (status, notifications toggle, start at login), and native notifications on Windows, macOS and Linux.
- **Redesigned dashboard.**
  - Works on phones and tablets, with a command palette (`Ctrl/⌘+K`) and keyboard shortcuts.
  - Adds a Home overview, the **Needs you** inbox, inline approval cards and safe Markdown rendering.
  - Also adds browser notifications, role-aware screens and a Users page.
- **Security fixes and changes.**
  - Dashboard links now use one-time sign-in codes (two minutes, single use) instead of the long-lived admin token.
  - `zpilot stop` waits until the daemon has released its data directory, so an immediate restart works.


## 0.1.0 (preview), 2026-11-24

First release of ZPilot.

- Agent profiles: identity, @username, icon, instructions, model, workspace, skills, tools, contacts, limits.
- Four engine-enforced security levels (restricted, standard, trusted, full).
- Agent-to-agent collaboration (`list_agents`, `ask_agent`, `tell_agent`) with contacts, accept-lists, hop limits, run budgets and deadlock detection.
- Built-in `@manager` agent with owner-approved proposals.
- Channels: Telegram (long polling, allow-list, sender approval) and authenticated webhooks.
- Schedules (shared trigger engine with ZSched), with missed-run catch-up.
- ZTOOLS registry install with SHA-256 verification; skills library.
- Encrypted secret vault; keys reach the engine only through its environment.
- Web dashboard (live updates) and a complete CLI; local token sign-in, password and TLS for remote use.
- ZEngine embedded from zagent 0.1.1 (commit recorded in `zengine/UPSTREAM`).
