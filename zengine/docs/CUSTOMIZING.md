# Customizing the agent

Everything here is a file in the agent home (see [INSTALL.md](INSTALL.md)), so customizing is editing files; nothing needs rebuilding.

## The system prompt

`system_instruction.md` is your prompt. `zagent init` writes a short, provider-neutral default; replace it freely: a coding assistant, a research agent, a support bot, a persona. Keep it about *behaviour*: the engine appends facts it knows, so you do not write them yourself:

- the operating system, the shell `run_command` uses, the workspace and scratchpad paths, today's date, the model name;
- which media types the model can take (from `.env`);
- permanent and workspace **memory**, and the **skills** list (below).

Tool descriptions travel separately with each tool; you do not repeat them.

**When edits take effect.** The prompt is *snapshotted* when a session starts, and again after each compaction. Editing the file affects **new sessions** (and existing ones the next time they compact), never the middle of a conversation. This is deliberate: a prompt that never changes mid-session is what lets the provider cache it. To change behaviour inside a running session, send a note (`/note use British spelling from now on`).

If the file is missing or blank, the built-in default is used.

## Memory

Memory is plain Markdown the agent can read and write with its `memory` tool.

| Scope | Where | Use |
|---|---|---|
| permanent | `<home>/memory/<name>.md` | Facts and preferences that apply everywhere. The agent saves here **only when you ask it to remember something**, or when something is critical for every future session. |
| workspace | `<workspace>/.agent/memory/<name>.md` | Notes about *this* project: build commands, decisions, progress. The agent uses it freely. |

At session start the contents are placed in the prompt (up to about 6,000 characters per scope; beyond that only names and one-line summaries are listed and the agent reads the rest on demand). The first line of a memory file is its summary, so make it a clear one-liner. You can write, edit or delete memory files by hand at any time; changes appear in **new** sessions.

Tell the agent in chat: *"remember permanently that I prefer tabs"* and it will save a permanent memory; *"remember for this project that tests run with `make check`"* saves a workspace one.

## Skills

A skill is a reusable procedure the agent loads only when a task calls for it, so it costs a line of prompt, not the whole text.

```
<home>/skills/release-notes/SKILL.md
```
```markdown
---
name: release-notes
description: Use when the user asks for release notes or a changelog entry.
---
1. Run `git log --oneline <last-tag>..HEAD`.
2. Group the commits into Added / Changed / Fixed.
3. Write them in the style of CHANGELOG.md in this repository.
```

The `description` is what the agent sees in its prompt; make it say *when* to use the skill. The body is loaded by the `skill` tool on demand, along with a list of any other files in the skill's folder (scripts, templates), which the agent can open with `view_file`. The agent can also create skills itself when you ask it to ("turn what we just did into a skill").

## Slash commands

A Markdown file in `<home>/commands/` becomes a command. `commands/review.md` becomes `/review`:

```markdown
---
description: Explain a thing simply
---
Explain $ARGS to a beginner in two sentences, with one example.
```

`/explain recursion` sends *"Explain recursion to a beginner ..."* as a normal prompt. `$ARGS` is whatever you typed after the command; without it, the text is appended to the end. The `description` shows in `/help`. Names use lowercase letters, digits, `-` and `_`. Built-in commands (`/help /stop /note /compact /status /tasks /tools /workspace /resume /exit /quit /new`) cannot be overridden.

## Tools: choose what the agent has

```json
{
  "tools": {
    "disabled": ["run_command", "terminal", "subagent"],
    "default_timeout_seconds": 120
  },
  "permissions": { "run_command": "deny", "write_outside_workspace": 0 }
}
```
- `disabled` removes tools (built-in or third-party) from the model's view entirely; `enabled` is an allow-list when you would rather name what is permitted.
- File tools stay inside the workspace and the scratchpad unless `permissions.read_outside_workspace` / `write_outside_workspace` is on. The agent's `.env` and the session logs are always off limits to them.
- Add your own tools: [TOOL_DEVELOPER_GUIDE.md](TOOL_DEVELOPER_GUIDE.md).

## Context behaviour: recipes

All in `config.json` under `context` (see [CONFIG.md](CONFIG.md)):

| You want | Set |
|---|---|
| Image-heavy work (screenshots, scans) without hitting limits | `"max_turn_to_keep_attachment": 3` so images older than three turns are dropped (in batches); the agent is told each removed file's path and can re-open it. |
| Keep every attachment as long as possible | `"max_turn_to_keep_attachment": 0` (default). Old attachments are removed only when the request would exceed the size limit. |
| Never lose detail to summaries; fail loudly instead | `"autocompact_on_request_body_limit_exceed": 0` and `COMPACT_AT` very high. An oversize request is then refused with an explanation. |
| Prefer summarising to dropping attachments | `"remove_old_attachment_on_request_body_limit": 0` |
| A small local model | lower `CONTEXT_LIMIT` and `COMPACT_AT` in `.env`; consider `compact_keep_last_turns: 2`. |

Compaction can also be triggered by hand with `/compact`.

## Sessions and workspaces

- A **session** is one conversation, stored in `sessions/<name>/`. Name them (`--session`) for work you will return to.
- The **workspace** is the folder the agent works in: the directory you start `zagent` from, or `--workspace`. `/workspace PATH` moves a running session; the agent is told, and relative paths follow.
- Sub-agents (`subagent` tool) give a task to a fresh context and bring back only a report. Ask for them when work would flood the main conversation: *"use a sub-agent to read all the PDFs in `reports/` and give me one table"*.

## Network and reliability

`network` in `config.json`: retry count and backoff, how long a silent stream is tolerated, and the pause/resume behaviour for outages (`auto_pause`, `pause_retry_minutes`, `max_pauses`). The defaults survive a typical outage unattended.

## Extra provider options

`provider.extra_body` is merged into every request, so any provider feature is reachable without code changes (thinking budgets, safety settings, `max_completion_tokens`, ...); `provider.extra_headers` adds headers; `provider.auth_style` switches the credential header. See [PROVIDERS.md](PROVIDERS.md).
