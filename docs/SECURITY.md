# Security model

ZPilot runs AI agents that can read files, change them and, if you allow it, run commands on your machine. This document describes what protects you and where the limits are.

## Principles

1. **Permissions are enforced by code, never by prompts.**
   - An agent's security level becomes its engine configuration: tools are disabled and command or file permissions are set.
   - Contacts, hop limits and budgets are checked by the daemon.
2. **Permissions never pass between agents.**
   - A `restricted` agent asking a `trusted` agent for help does not gain anything.
   - Requests from other agents are labelled in the prompt as coming from a colleague, not from the owner. The receiving agent keeps its own instructions and limits.
3. **External input is untrusted.**
   - Channel messages are marked as untrusted in the prompt.
   - An agent that can run commands (`trusted` or `full`) cannot be connected to a channel unless you explicitly confirm `external_input_with_commands` on that agent.
4. **The manager proposes; the owner decides.** `@manager` only has read-only file access plus inspection tools. Every change it wants becomes a proposal that you approve or reject. Applied proposals are audited.

## Security levels

| Level | Files | Shell commands |
|---|---|---|
| restricted | read inside the workspace | no |
| standard | read and write inside the workspace | no |
| trusted | read and write inside the workspace | **yes**, with the daemon user's OS permissions |
| full | read and write anywhere the user can | **yes** |

To review commands before they run, turn on **Ask me before every shell command** for the agent. The engine pauses each command until an admin allows or denies it. A request nobody answers is denied after the approval timeout (30 minutes by default).

The following limits can also be set per agent:

- **Memory and process caps.** On Windows these are enforced with a Job Object. On Linux, `prlimit` sets an address-space limit.
- **Isolation (Linux, needs bubblewrap).** The whole file system becomes read-only apart from the agent's workspace, its home and `/tmp`, and the agent gets its own process namespace.

Without isolation, shell commands are **not OS-sandboxed**. A trusted agent's command can do whatever your user account can do. Use `trusted` or `full` only for agents you would trust with a terminal, and run ZPilot in a VM, container or dedicated user account for untrusted work. Container-based isolation for Windows and macOS is on the roadmap.

## Secrets

- Model API keys and channel secrets are encrypted with AES-256-GCM in `secrets.json`.
  - On Windows, the master key is sealed with DPAPI for the current user.
  - Elsewhere it is a `0600` file.
  - This protects copies and backups of the data directory. It does not protect against other code running as the same user.
- Keys reach the engine only as environment variables. The engine removes `ZAGENT_*KEY/TOKEN/SECRET/PASSWORD` from every command and tool it starts, and agents cannot read their own `.env` (it holds no secrets anyway).
- The API never returns secrets. It only reports whether one is set.

## Channels

- **Telegram, Discord and WhatsApp.** Senders are checked against an allow-list. Unknown senders are refused, audited and shown to you.
- **Email.** Mail is accepted only if the receiving server reports a DMARC pass, or a DKIM or SPF pass aligned with the sender's domain, so a forged `From:` cannot reach an agent. Auto-replies and bulk mail are ignored.
- **WhatsApp webhooks.** Every delivery must carry a valid `X-Hub-Signature-256` made with the app secret.
- **Webhooks.** Callers authenticate with a shared secret, and repeated failures are rate-limited.

## Users and roles

- **Roles.** `viewer` can read; `operator` can also talk to agents, run workflows and schedules, and stop runs; `admin` can do everything, including deciding approvals and proposals, changing configuration and managing users.
- **The last admin.** The last enabled admin account cannot be demoted, disabled or deleted.
- **Sessions.** Changing a password or disabling an account ends that user's sessions.
- **Sign-in links.** Links opened by `zpilot ui` and the desktop app carry a one-time code (single use, valid for two minutes), never the admin token.

## The HTTP API

- **Default bind and remote access.** The default address is `127.0.0.1:7700`. Listening on any other address requires an admin password and TLS (or `--insecure-http` behind a TLS proxy).
- **Owner authentication.**
  - The local admin token (in the owner-only `endpoint.json`) is used by the CLI and `zpilot ui`.
  - The password uses PBKDF2-SHA256 with 600,000 iterations and is rate-limited per IP.
  - The browser session cookie is HttpOnly and `SameSite=Strict`.
- **CSRF.** State-changing cookie requests need `X-Requested-With: zpilot` and a same-origin `Origin`.
- **DNS rebinding.** A loopback daemon answers only to loopback host names.
- **Response headers.** A strict CSP, `X-Frame-Options: DENY`, `nosniff`, and HSTS on TLS.
- **Agent RPC.** It runs on a separate loopback-only listener, and each agent authenticates with its own token (rotatable).

## Tools from ZTOOLS

- Registry downloads must match the checksum published in the release's `SHA256SUMS.txt`, or the registry's `sha256` field. A tool without a published checksum is refused unless you explicitly allow unverified installs.
- Archives are extracted with path validation (no `..`, absolute paths or drive letters) and a size cap.

## Audit

Every configuration change, login, tool install, proposal decision, refused channel sender and stopped run is appended to `audit.jsonl` and shown in the dashboard.

## Reporting a vulnerability

Please open a private security advisory on the GitHub repository rather than a public issue.
