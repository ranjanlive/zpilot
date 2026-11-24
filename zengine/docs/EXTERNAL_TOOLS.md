# Third-party tools

A tool is a folder in `<home>/tools/` containing a manifest named after the folder and whatever it runs:

```
tools/
  word_count/
    word_count.json      <- the manifest
    word_count.py        <- any script, or an .exe / binary
```

Tools load at session start. A tool with a broken manifest is skipped and reported (as a warning in the session and by `zagent doctor`), never fatal. The tool is run **directly from `command` with no shell**, so arguments cannot be used for command injection.

## Manifest

```json
{
  "name": "word_count",
  "description": "Count words and list the most frequent ones. Use when the user wants text statistics.",
  "parameters": {
    "type": "object",
    "properties": { "text": { "type": "string" }, "top": { "type": "integer", "minimum": 1 } },
    "required": ["text"],
    "additionalProperties": false
  },
  "command": ["python", "word_count.py"],
  "input": "stdin_json",
  "output": "json",
  "timeout_seconds": 30
}
```

| Field | Meaning |
|---|---|
| `name` | Lowercase letters, digits, `_`. Defaults to the folder name. Built-in names are reserved. |
| `description` | **Required.** It is what tells the model when to use the tool. |
| `parameters` | A JSON Schema object. The engine validates every call against it *before* running anything and, on a mistake, returns the exact problems (unknown or missing argument, wrong type) to the model as the tool result. |
| `command` | The argv. Placeholders: `{dir}` (the tool folder), `{workspace}`, `{home}`, `{scratch}`, `{output_dir}`. A relative argument that names a file inside the tool folder (the script, a data file) is resolved there, so `["python", "word_count.py"]` works while the process runs in the workspace. |
| `input` | `stdin_json` (default): the arguments object on stdin. `args_json`: it is appended as the last argument (stdin is closed). `env`: one `ZAGENT_ARG_<NAME>` variable per argument, plus `ZAGENT_ARGS_JSON`. |
| `output` | `text` (default): stdout and stderr are the result; a non-zero exit code marks it as an error. `json`: stdout is one JSON object (below). |
| `cwd` | `workspace` (default) or `tool_dir`. |
| `env` | Extra environment variables (placeholders expand). |
| `timeout_seconds` | 0 = engine default, **-1 = no timeout** (hours-long tools). |
| `background` | Run as a background task (below). |
| `permission` | `allow` (default) or `ask` (each call needs approval). Interactive approval is not implemented in any interface yet, so an `ask` tool is currently refused with an explanatory message; leave it `allow`, or disable the tool in `config.json` when you do not want it available. |
| `attach_outputs` | Media files the tool writes to its output folder are attached to the result for the model to see (subject to the model's media capabilities). |
| `platforms` | Restrict to `windows`, `linux`, `darwin`. |

### JSON result

```json
{ "output": "text shown to the model", "is_error": false,
  "media": [ { "path": "C:/.../chart.png", "mime": "image/png" } ] }
```

If a tool crashes before printing, the model gets the tool's own error text followed by the exit code. A media file the model cannot take (capability off, too large) is dropped with an explanation, the rest of the result still arrives.

## What the tool receives

| Variable | Value |
|---|---|
| `ZAGENT_WORKSPACE` | The agent's current workspace. |
| `ZAGENT_HOME` / `ZAGENT_SCRATCH` | The agent home / its scratchpad. |
| `ZAGENT_SESSION` | The session name. |
| `ZAGENT_TOOL_NAME` / `ZAGENT_TOOL_DIR` | This tool. |
| `ZAGENT_OUTPUT_DIR` | **A private folder for this call.** Save generated files here (an image generator writes its picture here). Everything written is listed in the result. |
| `ZAGENT_NOTIFY_FILE` | Append lines here to message the running agent (below). |
| `ZAGENT_CAPS` | `vision=true,audio=false,pdf=true,video=false`: what the model can consume. |

The engine's own credentials are never in a tool's environment.

## Talking to the agent while running

Append a line to `$ZAGENT_NOTIFY_FILE`:

```
indexing 40% done                                  # plain text: delivered as a note
{"text": "need your decision on X", "wake": true}  # JSON: wake: true also starts an idle agent
```

Notes are appended to the end of the agent's next request as `[info: ...]` lines. They never edit the system prompt, so prompt caching is unaffected.

## Background tools

With `"background": true` the call returns at once with a task number; the agent carries on. Progress lines written to the notify file reach it as notes; when the process exits the agent is **woken** with a note containing the result and the files produced, even if it had finished its turn. Manage tasks with the built-in `terminal` tool (read output, wait, kill; a tool's stdin is closed after its arguments are delivered, so it cannot be sent further input). A headless run stays alive for background tasks up to `agent.max_background_wait_minutes`.

## Examples

The root [TOOLS catalogue](../../TOOLS/README.md) contains maintained third-party tools. [browser_automation](../../TOOLS/browser_automation/README.md) runs Playwright website workflows, produces reports/screenshots, and includes a local demo and real-agent integration tests. Use the catalogue installer to copy selected tools into the agent home.

`examples/tools/word_count` (JSON in/out) and `examples/tools/slow_report` (background, progress notes, output file). Copy a folder into `<home>/tools/` and run `zagent doctor`.
