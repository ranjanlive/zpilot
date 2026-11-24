# zagent documentation

Pick the path that matches what you want to do.

## I want to use zagent

| Read | To learn |
|---|---|
| [INSTALL.md](INSTALL.md) | Download, unpack, configure, first run, the three interfaces, updating, troubleshooting |
| [PROVIDERS.md](PROVIDERS.md) | Ready-made `.env` blocks for Gemini, OpenAI, Anthropic, OpenRouter, local models, Azure, custom gateways |
| [CUSTOMIZING.md](CUSTOMIZING.md) | Your own system prompt, memory, skills, slash commands, which tools the agent has, context behaviour recipes |
| [CONFIG.md](CONFIG.md) | Every `.env` and `config.json` setting, with defaults, and the home folder layout |

## I want to extend zagent with my own tool

| Read | To learn |
|---|---|
| [TOOL_DEVELOPER_GUIDE.md](TOOL_DEVELOPER_GUIDE.md) | Tutorial: build, test and share a tool in any language; progress notes and background tools; checklist |
| [EXTERNAL_TOOLS.md](EXTERNAL_TOOLS.md) | The manifest and protocol reference, environment variables, result format |
| `../examples/tools/` | Working tools: JSON in/out (Python), PowerShell, Node, an image generator, a background job with progress notes |

Test a tool without a model: `zagent tools list`, `zagent tools run NAME --args '{...}'`.

## I want to understand or change the engine

| Read | To learn |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) | How and why it works: the event log, cache stability, turns, compaction, interfaces, sub-agents |
| [DEVELOPMENT.md](DEVELOPMENT.md) | Code map, invariants, recipes (add a provider / tool / config key / command), testing patterns, pitfalls |
| [CONTROL_API.md](CONTROL_API.md) | The local HTTP/SSE API and the on-disk session formats, for building another interface |
| [../CONTRIBUTING.md](../CONTRIBUTING.md) | How to report problems and send changes |
| [RELEASING.md](RELEASING.md) | How a release is built and what to check; versioning and compatibility promises |

## Quick reference

```
zagent init | doctor | sessions | show SESSION | tools list | tools run NAME | version
zagent -p "task" [--session NAME] [--attach FILE] [--output [N]] [--json]      headless
zagent --session NAME --ui cli | web                                              interfaces
/help /stop /note /compact /status /tasks /tools /workspace /resume /exit /quit /new
```
