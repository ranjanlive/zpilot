# Changelog

All notable changes to `zagent` are documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.1] - 2026-10-15

### Fixed
- **Gemini Tool Response Serialization:** Fixed an issue where pruned tool attachment placeholders were placed into nested `functionResponse.parts` (causing Gemini HTTP 400 rejection). Pruned placeholders are now properly relocated to `functionResponse.response.attachment_notes`.
- **Preflight Request Validation:** Added `internal/provider/gemini_validate.go` to preflight wire payloads before sending. Ambiguous nested attachments produce an immediate local error with the JSON path rather than triggering remote 400 errors.
- **Prefix Cache Stability:** Tool outputs, call IDs, media blobs, and thought signatures are preserved during attachment repair without altering unmodified request bytes.

### Changed
- **Release Packaging:** Release archives now preserve Unix `0755` executable permissions when cross-compiled on Windows via the Go `package-tar` utility.
- **Documentation:** Added comprehensive root-cause analysis and recovery guide in `docs/GEMINI_ATTACHMENT_RECOVERY.md`.

---

## [0.1.0] - 2026-10-11

First public development release.

### Added
- **Multi-Provider Streaming Adapters:** Native support for Google Gemini, OpenAI-compatible (Ollama, vLLM, OpenRouter, Azure OpenAI), Anthropic Messages, and custom gateways with token usage reporting and URL normalisation.
- **Append-Only Session Logging:** Event-sourced `events.jsonl` architecture with content-addressed blob storage (`blobs/`), crash-safe load/resume, and OS-held single-writer locking (`owner.lock`).
- **Deterministic Context Management:** Byte-identical request rendering for maximum provider prefix-cache hits; per-turn attachment pruning, exact HTTP body-size guards (preventing 413s), and token-threshold compaction.
- **Network Resilience & Recovery:** Exponential backoff retry, automatic pause on network failure with delayed retry, and repair of interrupted/orphaned tool calls on session resume.
- **Native Tools:** Built-in tools for file viewing/editing, shell command execution with background task supervision, persistent/workspace memory, agent skills, and fresh-context sub-agents.
- **Third-Party Tool Protocol:** Support for external tools via JSON manifests (`tools/<name>/<name>.json`) with standard input/output streaming, execution timeouts, and progress notification channels.
- **Unified Interfaces:** Headless execution (`zagent -p`), terminal UI (`zagent --ui cli`), and browser dashboard (`zagent --ui web`) all backed by a single authenticated local control API (127.0.0.1).
- **Interactive Slash Commands:** `/help`, `/stop`, `/note`, `/compact`, `/status`, `/tasks`, `/tools`, `/workspace`, `/resume`, `/exit`, `/quit`, and custom user markdown commands.
- **Inspection & Diagnostics:** CLI commands `zagent init`, `zagent doctor`, `zagent sessions`, `zagent show`, and `zagent tools list|run`.
