# Changelog

## Unreleased

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
