# Configuration reference

The agent home contains two configuration files. `.env` says **which model** and **what it can consume**; `config.json` controls **how the engine behaves**. Both are created by `zagent init`. Every `config.json` key is optional (defaults below) and **unknown keys are rejected**, so a typo fails loudly instead of silently doing nothing. Any flag-like value accepts `true`/`false` or `1`/`0`.

## `.env`

Standard `KEY=VALUE` lines; `#` comments; values may be quoted. Any key can instead come from the process environment as `ZAGENT_<KEY>` (for example `ZAGENT_API_KEY`), which wins over the file, so a secret never has to be written to disk.

| Key | Meaning |
|---|---|
| `PROVIDER` | `gemini`, `openai_compatible`, `anthropic` or `custom`. |
| `API_FORMAT` | Only for `custom`: `gemini`, `openai` or `anthropic`. |
| `BASE_URL` | Host (or host + path). Standard paths are added for the three families; for `custom` it is the exact request URL. |
| `API_KEY` | Credential. Required unless `provider.auth_style` is `none`. |
| `MODEL` | Model name as the API expects it. |
| `CONTEXT_LIMIT` | The model's window in tokens (`1000000`, `128k`, `1m`). Used for the UI meter and the default compaction point. |
| `COMPACT_AT` | When the prompt reaches this many tokens (or `NN%` of `CONTEXT_LIMIT`) the conversation is compacted before the next request. Unset: 80% of `CONTEXT_LIMIT`. |
| `VISION` `AUDIO` `PDF` `VIDEO` | What the model accepts as input. If a tool or user tries a type that is off, the model is told so (as tool output) and nothing is sent. |

## `config.json`

### `agent`
| Key | Default | Meaning |
|---|---|---|
| `max_tool_call_turn` | 200 | Model-to-tools-to-model rounds allowed inside one prompt. |
| `max_output_tokens` | 0 | Cap on a response (0 = provider default; Anthropic uses `provider.anthropic_max_tokens`). |
| `temperature` | unset | Sampling temperature. |
| `max_background_wait_minutes` | 60 | After the model finishes, a headless run waits this long for running background tasks (their results wake the agent). |

### `tools`
| Key | Default | Meaning |
|---|---|---|
| `disabled` | `[]` | Tool names to remove, built-in or external (`["run_command", "subagent"]`). Removed tools are not advertised to the model. |
| `enabled` | `[]` | If non-empty, an allow-list: only these tools load. |
| `default_timeout_seconds` | 120 | Per-call timeout (tools that manage their own lifetime, such as `run_command`, and external tools with `timeout_seconds` are exempt). |
| `max_output_bytes` | 65536 | A single tool result is cut to this size (head and tail kept). |
| `max_view_bytes` | 2 MiB | How much of a text file `view_file` loads per call. |
| `max_attach_mb` | 20 | Largest single media attachment. |

### `context`
| Key | Default | Meaning |
|---|---|---|
| `max_turn_to_keep_attachment` | 0 | Inline attachments older than this many user turns are pruned. **0 keeps them until compaction.** |
| `attachment_prune_batch_mb` | 4 | Age-based pruning waits until this much expired payload exists, then prunes it all at once. Every prune changes the cached prefix, so batching keeps cache hits high. |
| `request_body_limit_mb` | 28 | The largest request body the endpoint accepts (measured exactly before each send). |
| `autocompact_on_request_body_limit_exceed` | 1 | If the next request would exceed the limit, compact the conversation. |
| `remove_old_attachment_on_request_body_limit` | 1 | If it would exceed the limit, drop the **oldest attachments first**. When both flags are on, removal is tried first and compaction is the fallback. If both are off, an oversize request is refused with a clear error rather than sent. |
| `compact_keep_last_turns` | 1 | Recent user prompts carried over verbatim (text only) after a compaction. |

### `network`
| Key | Default | Meaning |
|---|---|---|
| `max_retries` | 6 | Retries per request for transient failures (429, 5xx, resets, stalls). A server `Retry-After` is honoured. |
| `retry_initial_ms` / `retry_max_ms` | 1000 / 30000 | Exponential backoff with ±20% jitter. |
| `stall_timeout_seconds` | 120 | Abort and retry a stream that goes silent this long. |
| `auto_pause` | 1 | When retries are exhausted, **pause the session** and try again later instead of failing. |
| `pause_retry_minutes` | 5 | Wait between pauses (`/resume` retries immediately). |
| `max_pauses` | 12 | Pauses before giving up. |
| `empty_response_retries` | 2 | Re-send when the model returns nothing. |

### `permissions`
| Key | Default | Meaning |
|---|---|---|
| `run_command` | `ask` | `ask` / `allow` / `deny`. The engine supports an approval callback, but no interface implements interactive approval yet, so `ask` currently refuses (with a message saying how to enable commands) exactly like `deny`. Set `allow` to let the agent run commands. |
| `write_outside_workspace` | 0 | Let file tools write outside the workspace and scratchpad. |
| `read_outside_workspace` | 0 | Let file tools read outside them. The home `.env` and session logs stay off limits regardless. |

### `provider`
| Key | Default | Meaning |
|---|---|---|
| `auth_style` | family default | `bearer`, `x-api-key`, `x-goog-api-key` or `none`. |
| `extra_headers` | `{}` | Added to every request. |
| `extra_body` | `{}` | **Deep-merged into every request body**: the escape hatch for provider features (thinking budgets, safety settings, ...). |
| `gemini_tool_media` | `inline` | Where media returned by a tool goes: inside `functionResponse` (`inline`, Gemini 3) or beside it (`sibling`). |
| `openai_stream_usage` | 1 | Ask for usage in the final stream chunk (turn off for servers that reject the option). |
| `openai_replay_reasoning` | 0 | Send reasoning text back on assistant turns. |
| `prompt_cache` | 1 | Place Anthropic `cache_control` breakpoints. |
| `anthropic_version` | `2023-06-01` | The `anthropic-version` header. |
| `anthropic_max_tokens` | 8192 | Messages API requires `max_tokens`. |

### `service`
| Key | Default | Meaning |
|---|---|---|
| `idle_exit_minutes` | 30 | The background service behind `--ui cli/web` exits after this long idle with nothing attached (0 = never). |

## Home layout

```
<home>/
  zagent(.exe)             the binary (the home defaults to its folder)
  .env  config.json        configuration
  system_instruction.md    your system prompt (default written by init)
  memory/*.md              permanent memory, loaded into every session
  skills/<name>/SKILL.md   skills (name + description in the prompt; body loaded on demand)
  tools/<name>/<name>.json third-party tools
  commands/<name>.md       custom slash commands
  scratchpad/              temporary work, tool output, uploads
  sessions/<id>/           events.jsonl, blobs/, meta.json, lock.json, service.log
<workspace>/.agent/memory/ project-local memory
```
