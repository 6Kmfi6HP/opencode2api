# Free-tier shaping: reference alignment (2026-11-14)

Reference: `jasonxu114514/opencode2api`, especially
`internal/gateway/opencode_prompt.go`, `toolshape.go`, `title.go`, and
`identity/request.go`.

## Reference methods worth copying

1. Render the CLI prompt, not a frozen capture. The official prompt is paired
   with a live `<env>` block: working directory, platform, UTC date, and model.
2. Treat title generation as a separate request shape. It has no tools and a
   known prompt; attaching the agent manifest turns it into a 403/429.
3. Keep caller tool schemas. Client tool names are renamed to opaque wire names
   and restored on the response path instead of being dropped.
4. Attribute free-tier quota per request. A process-wide project id lets one
   busy conversation exhaust the project bucket for every caller.
5. Keep a stable canonical session id and a plausible timestamp; omit the v1
   request header on the v2 inference lane.

## Adopted here

- `agentPreambleFor` renders the official 2.x CLI prompt (`opencode-prompt-v2.txt`)
  with a live `<env>` block, date, and model id. The environment block is cached
  per process.
- The free-tier manifest is the full official 2.x toolset (13 tools). Every
  caller tool is renamed to an opaque `clnt_` digest alias; `tool_choice` and
  conversation history are rewritten with the tool list. The response path maps
  aliases back to the caller and filters calls the caller cannot execute.
- `isTitleGenerationRequest` bypasses manifest injection while retaining the
  prompt. A title call that still gets 429 is answered locally from the first
  user turn, in JSON or SSE, instead of surfacing the cosmetic failure.
- Free-model requests mint one random 40-hex `x-opencode-project` per request;
  retries reuse the value. Key-tier and non-free requests keep the state id.
- A free chat body without an explicit ceiling gets the official
  `max_completion_tokens: 32000`; caller-supplied limits win.

## Not adopted yet

- Canonical session timestamp/date window. `newOCSessionID` already uses the
  current time and the v2 fixed-suffix layout.
- v1 prompt/toolset for the legacy `/zen` lane. The current binary routes free
  traffic through `/inference` and `/zen/go`, both of which use the 2.x names.

## Verification

- `go test ./...` and `go vet ./...` pass.
- Claude Code 2.1.325 against a local gateway with `claude-sonnet-5-5` aliased
  to `big-pickle`: Read returns the file contents and Edit rewrites the file.
  Upstream logs show the official prompt and `clnt_` aliases, and the request
  reaches 200 after the free-tier quota retry (the retry is quota, not a
  fingerprint rejection).
