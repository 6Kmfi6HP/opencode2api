# Changelog

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
