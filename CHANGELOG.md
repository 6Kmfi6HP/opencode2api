# Changelog

## Unreleased

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
