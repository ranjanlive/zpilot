# Writing a third-party tool

A tool is **any program** plus a small JSON manifest. You can write it in Python, PowerShell, Node, Bash, Go, C#, Rust, a compiled `.exe`: anything that reads a JSON object on stdin and prints a result. This guide takes you from "hello" to a background tool that reports progress. The exact field-by-field reference is [EXTERNAL_TOOLS.md](EXTERNAL_TOOLS.md).

## 1. The smallest tool (5 minutes)

Create a folder in the agent home's `tools/` directory. The manifest must be named after the folder:

```
<home>/tools/hello_node/
    hello_node.json
    hello.js
```

`hello_node.json`
```json
{
  "name": "hello_node",
  "description": "Greet someone by name. Use when the user asks for a greeting.",
  "parameters": {
    "type": "object",
    "properties": { "name": { "type": "string", "description": "Who to greet." } },
    "required": ["name"],
    "additionalProperties": false
  },
  "command": ["node", "hello.js"],
  "output": "json"
}
```

`hello.js`
```js
let input = "";
process.stdin.on("data", (c) => (input += c)).on("end", () => {
  const args = JSON.parse(input);                       // the model's arguments, as one JSON object
  console.log(JSON.stringify({ output: `Hello, ${args.name}!` }));   // the result, as one JSON object
});
```

**Test it without a model:**
```sh
zagent tools list                                     # is it loaded? (broken manifests are listed with the reason)
zagent tools run hello_node --args '{"name":"Ada"}'   # run it exactly as the agent would
```
`tools run` uses the same validation, environment, output folder and notify file as a real call and prints "what the model receives". The agent picks the tool up in its next session.

That is a complete tool. Everything below is optional.

## 2. How the model sees your tool

The model sees only `name`, `description` and `parameters`. Treat them as the tool's interface documentation:

- **Description:** say *what it does and when to use it*, in a sentence or two. Mention important limits ("max 50 results").
- **Parameters:** a JSON Schema object. Give every property a `description`, mark `required`, set `"additionalProperties": false`, and use `enum`, `minimum`, `maximum` for constrained values. The engine **validates every call against the schema before running your program**: wrong or missing arguments never reach you, and the model gets a precise error to correct itself (for example `unknown argument "pth" (allowed: path)`). So you need no defensive parsing for types the schema already enforces.
- **Name:** lowercase letters, digits and `_`; it cannot clash with a built-in tool.

## 3. The protocol

**Input** (`"input"` in the manifest):

| Mode | The arguments arrive as |
|---|---|
| `stdin_json` (default) | One JSON object on stdin. Read it all. |
| `args_json` | The JSON object as the **last command-line argument**. Stdin is closed. |
| `env` | One variable per top-level argument, `ZAGENT_ARG_<NAME>` (upper-cased), plus `ZAGENT_ARGS_JSON` with everything. Convenient for shell scripts. |

**Output** (`"output"`):

| Mode | The result is |
|---|---|
| `text` (default) | Whatever you print to stdout/stderr. A **non-zero exit code marks the call as failed**. |
| `json` | One JSON object on stdout: `{"output": "text for the model", "is_error": false, "media": [{"path": "...", "mime": "image/png"}]}`. Only `output` is needed. Stderr is kept as diagnostics. |

Print errors the model can act on: *what failed and what to change* ("no such file: report.csv; use find_files to locate it"), not stack traces. In `json` mode set `"is_error": true`; in `text` mode exit non-zero. If your program crashes before printing, the model receives its stderr and exit code, which is also a fine way to fail.

**Environment** your program can rely on:

| Variable | Meaning |
|---|---|
| `ZAGENT_WORKSPACE` | The folder the agent is working in. **Your process also starts there** (set `"cwd": "tool_dir"` to start in the tool's folder instead). |
| `ZAGENT_OUTPUT_DIR` | **A private, empty folder for this call.** Write anything you produce here: images, reports, audio. It is created for you and never reused. Every file you leave there is listed in the result. |
| `ZAGENT_NOTIFY_FILE` | Append lines to this file to message the agent *while you run* (section 6). |
| `ZAGENT_HOME`, `ZAGENT_SCRATCH` | The agent home, and its scratchpad for temporary work. |
| `ZAGENT_SESSION` | The session name. |
| `ZAGENT_TOOL_NAME`, `ZAGENT_TOOL_DIR` | Your tool's name and folder (use `ZAGENT_TOOL_DIR` for data files shipped with it). |
| `ZAGENT_CAPS` | `vision=true,audio=false,pdf=true,video=false`: what the model can consume, so you can skip producing media it cannot use. |

Your tool's own credentials (an image-service API key, say) belong in `"env"` in the manifest or in the machine's environment. The agent's **own** API key is never in your environment.

**Paths in `command`:** a relative argument that names a file inside your tool's folder is resolved there, so `["python", "tool.py"]` works. Placeholders `{dir}`, `{workspace}`, `{home}`, `{scratch}`, `{output_dir}` are expanded anywhere in `command` and `env` values. The program is executed directly, **not through a shell**: no quoting problems, no injection; if you need a shell, make the command `["sh", "script.sh"]` or `["powershell", "-File", "script.ps1"]` yourself.

## 4. Returning files and images

Save the file in `$ZAGENT_OUTPUT_DIR`. Then either:

1. Set `"attach_outputs": true` in the manifest: every image / PDF / audio / video your tool wrote is attached to the result so the model *sees* it (when the model supports that media type and the size limits allow; otherwise the model is told why it was skipped and the file is still listed); or
2. return specific files from a JSON result: `"media": [{"path": "<absolute path>"}]`.

See `examples/tools/image_maker` (a complete image-producing tool in plain Python). In your own words to the model, say where the file is and what it contains; the path of every output file is appended automatically.

## 5. Examples in other languages

PowerShell (`output: json`, `"platforms": ["windows"]`):
```powershell
$a = [Console]::In.ReadToEnd() | ConvertFrom-Json
@{ output = "Hello, $($a.name)!" } | ConvertTo-Json -Compress
```
command: `["powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "hello.ps1"]`

Bash (`input: env`, `output: text`, `"platforms": ["linux", "darwin"]`):
```sh
#!/bin/sh
echo "Hello, $ZAGENT_ARG_NAME! Workspace: $ZAGENT_WORKSPACE"
```
command: `["sh", "hello.sh"]`

Go / Rust / C# / any compiled language: build for each OS you support and point `command` at the executable (`["mytool.exe"]`, `["mytool"]`); use `platforms` plus several manifests (`mytool_win/…`, `mytool_nix/…`) with the same `name`, each limited to its platform. Read stdin, write JSON.

## 6. Reporting progress, and long-running work

**Notes while running.** Append a line to `$ZAGENT_NOTIFY_FILE`:
```
indexing: 40% done                                    # plain text
{"text": "I need a decision on X", "wake": true}      # JSON; "wake": true also starts an idle agent
```
Notes are delivered to the agent as `[info: ...]` lines at the end of its next request. They never edit the system prompt. Write each note as a complete line ending in a newline.

**Background tools.** Set `"background": true` for work that takes minutes or hours (renders, crawls, builds, training). The call returns immediately ("started as task #N"), the agent keeps working, your notes reach it as you write them, and **when your process exits the agent is woken with the result** (your JSON `output`, plus the files in `ZAGENT_OUTPUT_DIR`), even if it had already finished its turn. The agent can read its output, wait for it, or kill it (`terminal` tool); your stdin is already closed, so do not wait for input. Use `"timeout_seconds": -1` for no time limit, or a number to cap the run (background tools are stopped at that limit and the agent is told).

Pattern for a long job:
```python
note = lambda t, wake=False: open(os.environ["ZAGENT_NOTIFY_FILE"], "a").write(json.dumps({"text": t, "wake": wake}) + "\n")
for i, chunk in enumerate(work, 1):
    process(chunk)
    note(f"{i}/{len(work)} chunks done")
print(json.dumps({"output": "finished: results in the output folder"}))
```
Try it: `zagent tools run slow_report --args '{"topic":"demo","seconds":6}'` prints the progress notes as they would be delivered, then the wake-up message.

## 7. Timeouts, permissions, platforms

- `timeout_seconds`: `0` = the engine default (120 s), `-1` = none. A foreground tool that exceeds it is killed along with its child processes and the model is told.
- `permission: "ask"` is reserved for approval before each call; interactive approval is not implemented in any interface yet, so such a tool is currently refused. Leave the default (`allow`), or keep the tool out of reach with `"tools": {"disabled": [...]}`.
- `platforms`: `windows`, `linux`, `darwin`. A manifest for another platform is silently skipped.
- The user can disable your tool at any time with `tools.disabled`.

## 8. Checklist before you share a tool

- [ ] `zagent tools list` shows it with no "not loaded" warning, and `zagent tools run` works with typical **and** bad arguments.
- [ ] The description tells the model *when* to use it; every parameter has a description; `additionalProperties` is `false`.
- [ ] It prints a useful message on failure and exits cleanly; it does not wait for stdin in `args_json`/`env` modes (the engine closes it for you, but do not prompt interactively).
- [ ] It writes files only to `ZAGENT_OUTPUT_DIR`, the workspace (if the user asked), or `ZAGENT_SCRATCH`.
- [ ] It does not print secrets, and does not need the network unless that is its purpose (say so in the description if it does).
- [ ] Long work uses `background` and notes. Quick work stays under the default timeout.
- [ ] Platform-specific? Set `platforms`. Needs an interpreter? Say so in your README (`python`, `node`, ...); the engine does not install anything.
- [ ] To distribute: share the folder (zip it); users drop it into `<home>/tools/`.

## 9. Debugging

| Symptom | Look at |
|---|---|
| Tool not offered to the model | `zagent tools list` (a "not loaded" line names the manifest problem); is it in `tools.disabled`? Is `platforms` right? |
| "invalid arguments" in the result | The model's call violated your schema; the message lists the exact problems. Loosen or clarify the schema/description. |
| "did not print valid JSON" | `output` is `json` but stdout held something else; debug prints belong on **stderr**. |
| "exited with code N" | Run it by hand: `zagent tools run NAME --args ...` shows the program's own stderr. |
| Relative file not found | Your process starts in the *workspace*; reference shipped files via `ZAGENT_TOOL_DIR` or rely on `command` path resolution. |
| Image not shown to the model | `attach_outputs` is off, the model's `VISION` flag is off, or the file is over `tools.max_attach_mb`; the result says which. |
