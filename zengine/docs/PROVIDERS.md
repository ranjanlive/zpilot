# Provider cookbook

Copy the block for your service into `.env` (`zagent init` creates it) and run `zagent doctor`. `doctor` prints the exact URL it will call, which is the quickest way to see whether a `BASE_URL` is interpreted as you meant.

**How `BASE_URL` is read.** For `gemini`, `openai_compatible` and `anthropic` give just the host, and the standard path is added. If your base already contains a path (`/v1beta/openai`, `/api/v1`, `/openai/v1`) it is used exactly as written and only the final segment is added. For `custom` the value is the **exact** URL requests are POSTed to.

| Provider | Request URL built from `BASE_URL` |
|---|---|
| `gemini` | `<base>/v1beta/models/<MODEL>:streamGenerateContent?alt=sse` |
| `openai_compatible` | `<base>/v1/chat/completions` if the base has no path, else `<base>/chat/completions` |
| `anthropic` | `<base>/v1/messages` if the base has no path, else `<base>/messages` |
| `custom` | exactly `BASE_URL` |

> **Tested vs. documented.** The three wire formats were exercised end to end (streaming, tool calls, tool results, usage) against a Gemini-format gateway that also exposes OpenAI- and Anthropic-format endpoints. The vendor-specific examples below follow each vendor's public API conventions but were **not** run against those vendors; if one needs a tweak, `provider.extra_headers`, `provider.extra_body` and `provider.auth_style` in `config.json` cover most differences.

## Google Gemini (official API)

```ini
PROVIDER=gemini
BASE_URL=https://generativelanguage.googleapis.com
API_KEY=...
MODEL=gemini-2.5-flash
CONTEXT_LIMIT=1000000
VISION=true
AUDIO=true
PDF=true
VIDEO=true
```
Thinking options and other Gemini settings go into `config.json` under `provider.extra_body`, for example `{"provider": {"extra_body": {"generationConfig": {"thinkingConfig": {"thinkingBudget": 2048}}}}}`.

## A Gemini-compatible gateway or proxy

Same as above with your gateway's host. This is the setup the engine was developed against:
```ini
PROVIDER=gemini
BASE_URL=https://api.example.com
API_KEY=...
MODEL=gemini-3.8-flash-high
```
Gateways often cap the request size (commonly 32 MiB). Keep `context.request_body_limit_mb` a little below it (the default 28 suits a 32 MiB limit).

## OpenAI

```ini
PROVIDER=openai_compatible
BASE_URL=https://api.openai.com
API_KEY=sk-...
MODEL=gpt-4.1
CONTEXT_LIMIT=1000000
VISION=true
PDF=true            # sent as a file part
AUDIO=false         # input_audio accepts only wav and mp3
VIDEO=false         # the chat API takes no video
```
If the API rejects `stream_options` or wants `max_completion_tokens`, set `"provider": {"openai_stream_usage": 0}` and/or pass `{"provider": {"extra_body": {"max_completion_tokens": 4096}}}`.

## Anthropic

```ini
PROVIDER=anthropic
BASE_URL=https://api.anthropic.com
API_KEY=sk-ant-...
MODEL=claude-sonnet-4-5
CONTEXT_LIMIT=200000
COMPACT_AT=80%
VISION=true
PDF=true            # sent as a document block
AUDIO=false
VIDEO=false
```
Prompt-cache breakpoints are placed automatically (`provider.prompt_cache`). Extended thinking: `{"provider": {"extra_body": {"thinking": {"type": "enabled", "budget_tokens": 4096}}}}` (thinking blocks are preserved and replayed as the API requires).

## OpenRouter and other OpenAI-compatible gateways

The base must include the version segment when the gateway uses one:
```ini
PROVIDER=openai_compatible
BASE_URL=https://openrouter.ai/api/v1
API_KEY=sk-or-...
MODEL=anthropic/claude-sonnet-4.5
```

## Local models (Ollama, LM Studio, llama.cpp server, vLLM)

No key needed. A bare host gets `/v1` added.
```ini
PROVIDER=openai_compatible
BASE_URL=http://localhost:11434          # Ollama; LM Studio is usually http://localhost:1234
MODEL=llama3.1
CONTEXT_LIMIT=32768                       # be honest about the context you actually loaded
COMPACT_AT=70%
VISION=false
```
```json
{ "provider": { "auth_style": "none" } }
```
Small local models handle fewer tools well: disable what you do not need with `"tools": {"disabled": ["skill", "subagent"]}`.

## Azure OpenAI and other URL-shaped gateways

Use `custom`, give the full URL (deployment and `api-version` included) and put the credential header yourself:
```ini
PROVIDER=custom
API_FORMAT=openai
BASE_URL=https://myres.openai.azure.com/openai/deployments/gpt-4o/chat/completions?api-version=2024-10-21
MODEL=gpt-4o
API_KEY=unused
```
```json
{ "provider": { "auth_style": "none", "extra_headers": { "api-key": "YOUR-AZURE-KEY" } } }
```

## Gemini through its OpenAI-compatible endpoint

```ini
PROVIDER=openai_compatible
BASE_URL=https://generativelanguage.googleapis.com/v1beta/openai
API_KEY=...
MODEL=gemini-2.5-flash
```

## Choosing values

- `CONTEXT_LIMIT` should be the real window of the model *as served*. `COMPACT_AT` leaves room for the answer and for tool output; 70-85% is typical.
- Switch a media flag on only when the model accepts that input; otherwise the agent tells the model "not supported" when it tries, rather than sending data that would fail.
- If your provider reports no token usage, counts are estimated (bytes divided by 4) and shown with a `~`; compaction then relies on the estimate.
