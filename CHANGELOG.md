# Changelog

## v0.10.0

- Claude to native Responses passthrough:
  - `POST /v1/messages` now converts Claude Messages to the upstream native `/responses` endpoint when the chat translation path fails (404/500/501/502) or the model is remembered as native-only, reusing the passthrough memory registry shared with `/v1/responses`.
  - Lenient conversion throughout: unsupported blocks degrade to text annotations instead of HTTP 400; assistant message parts use `output_text` (upstream rejects `input_text` on assistant messages); tool schemas are normalized so `required` covers every property key; `reasoning.effort=max` maps to `xhigh`.
  - Native Responses output (message/reasoning/function_call/apply_patch/shell) converts back to Claude content blocks for both unary and SSE streaming.
- muse-spark strict-upstream hardening (gated to `muse-spark` models only):
  - Function-call arguments with integral floats (`1000.0`) are normalized to integers outside JSON strings in both unary and streaming relay, fixing strict clients (Codex Rust `usize` parsing) while leaving visible text like `echo 1.0` untouched.
  - Responses tool schemas and reasoning effort are sanitized before native passthrough.
- Verified end to end with `launch claude` and `launch codex` on `muse-spark-1.3-contributor`, including multi-turn tool-use and shell execution.


## v0.9.1

- Fix `SSE stream ended without [DONE]` on native Responses passthrough models (`muse-spark` etc.): the streaming relay now appends a `data: [DONE]` sentinel on clean EOF when the upstream ended its `/zen/v1/responses` stream without one (same upstream quirk already tolerated in the chat translation path). Streams that already carry a sentinel are relayed verbatim with no duplicate, and empty zero-event streams still pass through untouched so upstream failures are not masked.

## v0.9.0

- Native Responses Passthrough with Memory:
  - Requests whose chat translation path fails (404/501/502/500) are automatically probed against the upstream native `/responses` endpoint; on success the model is remembered and served directly on subsequent requests.
  - Confirmed models are served by a fidelity reverse proxy that preserves upstream 4xx/5xx status codes and error bodies instead of masking client errors as 502.
  - Unified all upstream calls (`chat/completions`, `responses`) on `callOpenCodeEndpoint` with retry/rotation, context propagation, and structured attempt logging.
- Streaming Relay Quality:
  - SSE is relayed line-by-line with per-event flush, fixing the 32KB `io.Copy` buffering stall that broke typewriter streaming.
  - Tail `usage` is extracted from `response.completed`/usage events for token accounting, upstream rate-limit headers are forwarded, and completed responses persist session state for `previous_response_id` chains.
- Safer Probing & Self-Healing Registry:
  - Probing never fires on 401/403/429/400, typed conversion errors, context cancellation, or transport errors, avoiding retry storms under rate limiting or credential failure.
  - Static preset models (`muse-spark-1.3-contributor`) skip translation from the first request; the new `native_responses_models` config merges additively without clearing learned models; dynamically learned models are evicted after consecutive failures while static models are immune.

## v0.8.0

- Redesigned Admin Dashboard & Workspace Layout:
  - Upgraded Web Admin UI to a modern top-tab layout with four dedicated workspaces: Overview & Telemetry, Models & Routing, Network & Proxy, and System Settings.
  - Added Live Match Tester and quick preset buttons for interactive model alias rule evaluation.
  - Fully exposed upstream behavior controls (`prompt_cache_retention`, `cache_control_breakpoints`, `text_only_models`) and network options (`socks5_sticky`) to prevent silent config overwrites when saving via the panel.
- Advanced Model Alias Matching Rules:
  - Expanded `model_alias` with multi-pattern matching rules (`exact`, `contains`, `prefix`, `suffix`, `regex`, `wildcard`).
  - Automatically preserves and propagates bracket suffixes (e.g. `claude-3-7-sonnet[1m]` maps to target model while retaining `[1m]`).
  - Full backward compatibility for legacy key-value dictionary formats.
- Cross-Process Advisory Locking for Token Usage Statistics:
  - Implemented atomic read-modify-write synchronization for `stats.json` using OS advisory locks (`flock` on Unix-like systems and `LockFileEx` on Windows).
  - Prevents token usage and request count overwrite/loss when running the daemon server concurrently with short-lived `opencode2api launch claude|codex` instances.
- models.dev Dual-Layer Cache & Proxy Reuse:
  - Added dual-layer caching (in-memory + disk cache) for models.dev catalog data.
  - Automatically reuses active SOCKS5 proxy configurations for catalog fetches with graceful fallback to cached catalog on network timeouts or failures.
  - Periodic background refresh every 6 hours.
- Shared Configuration, Statistics, and Log Path Resolution:
  - Unified path resolution across `server` and `launch` modes with user config directory fallback (`~/.config/opencode2api`).
  - Support for `OPENCODE2API_STATS` / `OPENCODE2API_STATS_FILE` and `OPENCODE2API_LOG` / `OPENCODE2API_LOG_FILE` environment variables and CLI overrides.
- Automated Build & Release Versioning:
  - Replaced hardcoded version constants with `runtime/debug.ReadBuildInfo()` dynamic module and VCS metadata resolution (commit revision, timestamp, dirty state).
  - Added `scripts/release.sh` and `make version` / `make release` for automated SemVer release derivation, pre-flight validation, and tag deployment.

## v0.7.0

- Redesign Web Admin Control Panel:
  - Modern aesthetic with refined dark palette, glassmorphism cards, and Lucide icons.
  - Responsive multi-section layout with sidebar navigation (Overview, Proxy & Upstream, Logs, Documentation, Configuration).
  - Live metric cards for active models, requests, cache hit rates, upstream health, and response latency.
  - Real-time log streaming viewer with auto-refresh, search filter, level tags, auto-scroll toggle, and pause/resume controls.
  - Visual upstream base URLs manager, model alias editor, and rate-limit cap configuration.
  - Clean authentication modal with seamless session management.
- Unified Config File Resolution Order:
  - Consistent config path resolution across server and launch modes:
    1. `OPENCODE2API_CONFIG` environment variable
    2. Explicit `-config` / `--config` CLI flags
    3. Existing `./config.json` in the working directory (preserves backward compatibility)
    4. Platform user configuration directory: `<UserConfigDir>/opencode2api/config.json` (`~/.config/opencode2api/config.json` on Linux, `~/Library/Application Support/opencode2api/config.json` on macOS)
  - When persisting configuration in server mode, automatically create parent user config directories if they do not exist.
  - Launch mode respects the same resolution order while remaining read-only.

## v0.6.0

- Add one-click cross-platform installers: `scripts/install.sh` for Linux/macOS/FreeBSD and `scripts/install.ps1` for Windows. Both resolve the latest GitHub Release (or a pinned tag), select the correct OS/arch tarball, verify its SHA256 from `checksums.txt`, install the binary under `~/.opencode2api/bin`, and print next-step `launch claude` / `launch codex` commands. Installation, platform support, version pinning, and release-asset naming are documented in `docs/INSTALL.md` and linked from both READMEs.
- Add `opencode2api launch codex`: starts the same localhost proxy as `launch claude`, then runs the installed Codex CLI with temporary `-c` provider overrides (`model_provider`, `model_providers.opencode2api.*`, `wire_api="responses"`, and `env_key="OPENCODE2API_OPENAI_API_KEY"`) so the child process uses opencode2api through its Responses API without writing `~/.codex/config.toml`. `--model` / `-m` after `--` are extracted, forwarded via Codex's `--model`, and the same `--key` / `OPENCODE_API_KEY` / `public` resolution is used. A temporary Codex model catalog is written from the current upstream model list and passed via `-c model_catalog_json=<temp path>`; it defaults to free-tier models for `public`, includes all models for paid/tier keys, and sets `context_window`, `max_context_window`, and `auto_compact_token_limit=<ctx*0.9>` so model switching and unknown upstream model IDs do not fall back to Codex's 258K default window.
- Make `launch claude` and `launch codex` read-only for the proxy config: `launch` now loads and applies `config.json` without calling `saveConfig`, so launching a child CLI can no longer rewrite the user's persistent proxy configuration.
- `opencode2api launch claude` now supports interactive TUI model selection, 1M context window mode, and automatic compaction:
  - When `--model` is omitted, an interactive TUI lists free-tier models from the upstream catalogs, sorted by context window (largest first), with `[1m]` markers for ≥1M-context models.
  - After model selection, the context window is looked up from models.dev: ≥1M gets a `[1m]` suffix on the model ID and `CLAUDE_CODE_AUTO_COMPACT_WINDOW=ctx×0.9`; <1M gets only the auto-compact; unknown gets neither.
  - Model ID is set via five `ANTHROPIC_*_MODEL` environment variables instead of `--model`, avoiding the `[claude-code:unrecognized_model]` warning.
  - `--model` can be placed after `--` (alongside claude passthrough flags like `--dangerously-skip-permissions`) and is extracted by opencode2api instead of being forwarded to claude.
  - The `[1m]` suffix flows through `resolveModel` / `resolveModelForAuth` / `mapPublicToFreeModel` transparently (stripped for catalog lookup, re-applied on the resolved ID).
  - models.dev catalog is fetched with a cache-busting query parameter and parsed from both the top-level `models` section and the nested `providers.*.models` section (where OpenCode-specific models like `x-preview-f-free` live).
  - TUI shows only models with a free variant; paid-only models are filtered out.
- Tolerate upstream streams that end with a usage-only chunk but no `finish_reason` and no `[DONE]` (observed on `muse-spark-1.2-contributor-free`): when the turn produced output and the terminal usage chunk carried output-token accounting, the Claude and Responses stream converters now synthesize `stop` / `response.completed` instead of failing with `stream ended without finish_reason`/`server_error`. Partial EOF with no output still fails instead of fabricating a reply.
- Fix a data race in the raw-SSE wrapper used by streaming Chat conversion: `Close()` and `Read()` on `rawSSEReader` are now synchronized.

## v0.5.0

- Add `POST /v1/messages/count_tokens` with local heuristic estimation: Claude Code polls this endpoint to manage its context window and trigger auto-compaction, and previously it 404'd. The new `claudeCountTokensHandler` answers synchronously with a local estimate (text at ~4 chars/token, per-message/system/tool structural overhead, and fixed `1600`/`3000` token estimates for image/document blocks), never calling the upstream and never incurring usage. Invalid JSON or missing `model` return a protocol-shaped 400; non-POST returns 405.
- Add multi-domain upstream load balancing with session stickiness: the new `upstream_base_urls` config spreads opencode zen traffic across multiple (reversed) domains. Sessions hash-stick to one (base URL, socks5 proxy) pair so per-egress prompt caches keep hitting, rebinding to a different target on 429/5xx/transport errors like the existing proxy stickiness. Catalog fetches round-robin over all domains, logs record `base_url` per attempt, and the admin UI gains an upstream domain editor. Defaults to `["https://opencode.ai"]` when unset.

## v0.4.9

- Silence multi-modal downgrade for text-only upstream models: models matching the new configurable `text_only_models` prefixes (default `["deepseek"]`, prefix-matched case-insensitively) have image and document parts replaced with `[image attached]` / `[document attached]` text annotations before the request leaves the proxy, so DeepSeek requests with screenshots or pasted images keep working instead of failing upstream with an image-unsupported error. Applied uniformly across the Chat, Responses, and Claude protocol surfaces (single choke point at `buildUpstreamBody`); text content and part order are preserved.

## v0.4.8

- Align Messages-API `input_tokens` with Anthropic semantics: when `cache_read_input_tokens` is derived from DeepSeek/OpenAI-style counters (`prompt_cache_hit_tokens` or `prompt_tokens_details.cached_tokens`), the hit portion is subtracted from `input_tokens`, since `prompt_tokens` includes it. Clients that price input and cache reads separately no longer bill hit tokens twice (289 prompt + 256 hit now reports `input_tokens: 33`, `cache_read_input_tokens: 256`). Anthropic-style `cache_read_input_tokens` sources are left untouched.

## v0.4.7

- Fix DeepSeek cache usage accounting: `prompt_cache_hit_tokens` is cached/read input, while `prompt_cache_miss_tokens` is ordinary uncached input and is no longer counted as `cache_creation_input_tokens` in Claude usage or `cache_created_tokens` in `stats.json`. The admin stats table now also displays `cache_read_tokens` / `cache_created_tokens`.
- Harden cache usage aggregation so canonical Anthropic cache fields take precedence over DeepSeek/`cached_tokens` fallbacks and are never double-counted when multiple usage shapes are present. Add regression tests for DeepSeek and Anthropic cache semantics.

## v0.4.6

- Add session-sticky egress (`socks5_sticky`, default `true`) for round-robin proxy mode. Each session/account (paid by API token, public by Claude metadata `session_id`, otherwise a shared fallback) pins one egress proxy, so upstream per-egress prompt caches keep building up: measured 99.8% cache hit on a pinned egress vs ~0% when rotation randomly switches egress between requests. Different sessions still rotate, keeping the multi-egress distribution.
- Release sticky bindings before retrying upstream errors: free-tier 429s are rate-limited per egress IP, so the retry rotates to the next proxy (verified live: 429 on egress A → automatic rebind to egress B → retry succeeds). Transport errors and 5xx release the binding too.
- Fix rebind-after-invalidate using a deterministic hash, which always landed the same session back on the same proxy (i.e. "switch IP on error" never actually happened). Rebinding now rotates egress via an incrementing sequence mixed into the hash.
- Clear all sticky bindings when the proxy configuration changes (`active_socks5` or the proxy list), so stale bindings never point at removed egresses.

## v0.4.5

- Improve prompt-cache hit rate on the OpenCode zen upstream. Requests now inject `prompt_cache_retention: "24h"` (upstream default is ~5 min) and an Anthropic-style `cache_control: {"type":"ephemeral","ttl":"1h"}` breakpoint for models that accept it. GLM/Zhipu models, which reject unknown fields, are always skipped, and client-supplied `extra_body` values win over the injected defaults. Both behaviors are configurable via `prompt_cache_retention` (`"24h"` default, `"in_memory"`, or `"off"`) and `cache_control_breakpoints` (default `true`).
- Map DeepSeek-style `prompt_cache_hit_tokens` into Claude usage as `cache_read_input_tokens` when the standard fields are absent, so cached tokens are visible on the Messages API. `prompt_cache_miss_tokens` is ordinary uncached input, not a cache write, so it is intentionally not reported as `cache_creation_input_tokens`.
- Aggregate per-model cache accounting in `stats.json` as `cache_read_tokens` / `cache_created_tokens` across Chat, Responses, and Messages (streaming and non-streaming), and add these columns to the admin panel. For DeepSeek-style upstreams the hit rate is `cache_read_tokens / prompt_tokens`.

## v0.4.4

- Fix DeepSeek/Qwen models that emit raw DSML/XML tool-call markup (for example `<｜DSML｜tool_calls>`, `<|DSML|tool_calls>`, `<tool_calls>`, and `<tool_call>`) by converting it to standard OpenAI `tool_calls` before it reaches Chat/Claude/Responses clients. Non-streaming responses are normalized in `callOpenCodeAPI`; streaming responses are wrapped by `callOpenCodeAPIStream` so all three downstream protocols share one conversion layer. Native `tool_calls`, `usage`, `reasoning_content`, `finish_reason`, and `[DONE]` semantics are preserved.

## v0.4.3

- Fix `cache_control` counting so signature fields are not treated as cacheable message content, and ensure Responses `cached_tokens` remains present in Claude usage translation.

## v0.4.2

- Refactor project layout: split the root `main.go` monolith into `internal/app` packages, add `cmd/opencode2api` as the executable entrypoint, extract protocol DTOs into `internal/domain`, random helpers into `internal/random`, response ID normalization into `internal/ids`, and sync Makefile/Dockerfile/release script/CI paths and ldflags. HTTP behavior and CLI flags are unchanged.

- Fix `/v1/responses` `text` parameter being forwarded verbatim as upstream `response_format`. The Responses API `text` nests `type` inside `format` (`{format:{type:...}, verbosity:...}`), but upstream providers require a top-level `type` in `response_format`, causing `400 response_format: missing field type`. Added `convertResponsesTextToResponseFormat` to translate `text.format` into a valid Chat Completions `response_format` (`text` / `json_object` / `json_schema`), and drop the field entirely when it cannot be represented (unknown type, missing required `json_schema` fields, non-object `text`, or verbosity-only) so the proxy never forwards a malformed `response_format` and never surfaces a 400 for this case. Verified against the real upstream and end-to-end through the proxy.

## v0.4.1

- Add `stop_sequence` field to non-streaming Claude responses, streaming `message_start`, and streaming `message_delta` so Claude Code sees `"stop_sequence": null` consistently, matching the Anthropic Messages API contract.
- Add `stop_details` field (omitempty) to `ClaudeResponse` for forward compatibility with `refusal` stop reason.
- Extend `claudeUnsupportedBlockTypes` with 7 new server-tool/MCP block types (`code_execution_tool_use`, `code_execution_tool_result`, `mcp_tool_use`, `mcp_tool_result`, `bash_code_execution_tool_result`, `web_fetch_tool_result`, `tool_reference`) for observability; requests with these blocks are still accepted (no 400).

## v0.4.0

- Fix 19 audited protocol compatibility issues across stream integrity, native Anthropic decoding, and request compatibility adapters.
- Stream integrity: emit protocol error events (`event: error` on Claude, `response.failed` on Responses) on upstream stream errors or abnormal EOF; support 15s keepalive ping before first token; adopt single-writer SSE architecture.
- Native Anthropic decoding: strict SSE lifecycle validation, numeric index ordering for content blocks, support typed deltas (`text_delta`, `thinking_delta`, `signature_delta`, `input_json_delta`, `redacted_thinking.data`), preserve ordered blocks across Claude roundtrips via `_opencode2api_anthropic_content`, deterministic response-ID prefix normalization, and return generic `upstream_error` without error detail leakage.
- Request compatibility: `/v1/responses` `tool_result` normalization and collection; Anthropic `document` and Responses `input_file` mapping to Chat `file` parts; structure-aware file/document validation; strict temperature boundary validation (0..1 for Messages, 0..2 for Chat/Responses); safe `cache_control` and signature counting.
- Public free-model routing: automatically downgrade keyless public-tier requests for paid model IDs to `-free` variants (e.g., `deepseek-v4-flash` → `deepseek-v4-flash-free`).
- Add configurable `max_tokens` cap (global `max_tokens_cap` and per-model `max_tokens_cap_per_model`), clamping upstream requests exceeding limits, with Admin UI controls and `/api/config` support.

## v0.3.10

- Add `socks5_paid_direct` (default `false`): when an `active_socks5` proxy is set, keyed/paid upstream traffic also uses SOCKS5 unless this flag is explicitly enabled for the old paid-direct bypass.

## v0.3.9

- Stop cross-model upstream fallback for both public and keyed auth; retry transient 401/429/5xx and transport errors on the same requested model only.
- Treat upstream `CreditsError` / insufficient balance as non-retryable so Anthropic and Chat requests return the original billing error instead of silently trying other catalog models.

## v0.3.8

- Fix Claude Code `/v1/messages` streaming: wait for the OpenAI usage-only chunk after `finish_reason` before emitting `message_delta`, so token usage (and cache fields when present) is no longer dropped.

## v0.3.7

- Fix Docker image build: copy `go.sum` so lumberjack dependency resolves during multi-arch image builds.

## v0.3.6

- Claude Code `/v1/messages` → Chat upstream conversion: accept `x-api-key` (reject `sk-ant-`), merge mid-conversation `system` into one leading system message, convert `tool_result` images to follow-up `image_url`, map `tool_choice.disable_parallel_tool_use` → `parallel_tool_calls=false`, narrow `metadata.user_id` JSON to `session_id`, skip server tools without `input_schema`, and log intentional drops (`context_management`, `cache_control`, betas) in `request_plan`.
- Map Claude Code `output_config.effort` (`--effort` / `CLAUDE_CODE_EFFORT_LEVEL`) onto upstream `reasoning_effort`, and treat `thinking.type=adaptive` as enabled.
- Add structured request logging with lumberjack file rotation (default `opencode2api.log` + stdout), `request_id` tracing, protocol/upstream/stream summaries, secret redaction, and runtime `log_level` / `log_bodies` via `/api/config`.
- Restore `/v1/messages` default CoT passthrough for Claude Code (thinking off only when force-disabled or `thinking.type=disabled`), while keeping empty-reply fallbacks from v0.3.5.
- Fix reasoning effort: stop stripping `reasoning_effort` before upstream calls; forward Claude `thinking` (including `budget_tokens`) and derive effort from budget when needed.
- Hide upstream `-free` suffix in `/v1/models` responses and resolve stripped names back to free upstream IDs.

## v0.3.5

- Fix empty Claude Code / OpenAI replies when the Go gateway puts the answer in `reasoning_content` (#37635): promote to `content`/`text` when thinking is not requested, and fall back to a text block if a thinking-only stream would otherwise end empty.
- Temporarily made `/v1/messages` thinking opt-in; reverted in Unreleased because Claude Code lost visible CoT.

## Prior

- Projectized the provided Go program.
- Added Go module metadata, local build targets, and release packaging script.
- Added CI and tag-driven multi-platform release automation.
- Changed release automation to parallel matrix builds with a final publish job.
- Added README, API, configuration, deployment, release, contribution, and security docs.
- Added build metadata and `-version` flag.
