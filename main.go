package main

import (
	"context"
	"fmt"
)

var (
	version = "v0.4.0"
	commit  = "none"
	date    = "unknown"
)

func versionString() string {
	return fmt.Sprintf("opencode2api %s (commit=%s, date=%s)", version, commit, date)
}

// ======================== 结构化日志 ========================

type contextKey string

const reqIDKey contextKey = "request_id"

func getReqID(ctx context.Context) string {
	if id, ok := ctx.Value(reqIDKey).(string); ok {
		return id
	}
	return ""
}

// ======================== 管理面板认证 ========================

type OpenAIRequest struct {
	Model           string         `json:"model"`
	Messages        []Message      `json:"messages"`
	Stream          bool           `json:"stream"`
	Temperature     *float64       `json:"temperature,omitempty"`
	MaxTokens       *int           `json:"max_tokens,omitempty"`
	TopP            *float64       `json:"top_p,omitempty"`
	Thinking        any            `json:"thinking,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
	ExtraBody       map[string]any `json:"extra_body,omitempty"`
	Tools           []Tool         `json:"tools,omitempty"`
	ToolChoice      any            `json:"tool_choice,omitempty"`
}

type Message struct {
	Role             string     `json:"role,omitempty"`
	Content          any        `json:"content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	Name             string     `json:"name,omitempty"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type AppConfig struct {
	ModelAlias           map[string]string `json:"model_alias"`
	ReasoningEffortMap   map[string]string `json:"reasoning_effort_map"`
	ForceDisableThinking bool              `json:"force_disable_thinking"`
	// MaxTokensCap is the global default upper bound for max_tokens sent
	// upstream. 0 (default) means no global cap. Per-model values in
	// MaxTokensCapPerModel take precedence.
	MaxTokensCap int `json:"max_tokens_cap,omitempty"`
	// MaxTokensCapPerModel overrides the global cap for specific models.
	// A value of 0 for a model disables the cap for that model.
	MaxTokensCapPerModel map[string]int `json:"max_tokens_cap_per_model,omitempty"`
	Socks5Proxies        []Socks5Proxy  `json:"socks5_proxies,omitempty"`
	ActiveSocks5         string         `json:"active_socks5,omitempty"`
	// Socks5PaidDirect controls whether keyed/paid upstream calls bypass SOCKS5.
	// Omitted or false (default): all traffic uses the active proxy.
	// true: paid/keyed traffic goes direct; only public/free uses SOCKS5.
	Socks5PaidDirect bool `json:"socks5_paid_direct,omitempty"`
}

// ======================== Claude Messages API 类型 ========================

type ClaudeRequest struct {
	Model             string          `json:"model"`
	Messages          []ClaudeMessage `json:"messages"`
	System            any             `json:"system,omitempty"`
	MaxTokens         *int            `json:"max_tokens,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	TopK              *int            `json:"top_k,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	Tools             []ClaudeTool    `json:"tools,omitempty"`
	ToolChoice        any             `json:"tool_choice,omitempty"`
	StopSequences     []string        `json:"stop_sequences,omitempty"`
	Metadata          any             `json:"metadata,omitempty"`
	Thinking          any             `json:"thinking,omitempty"`
	OutputConfig      any             `json:"output_config,omitempty"`
	ContextManagement any             `json:"context_management,omitempty"`
}

type ClaudeMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type ClaudeContent struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     any    `json:"input,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"`
}

type ClaudeTool struct {
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	InputSchema  any    `json:"input_schema"`
	Type         string `json:"type,omitempty"`
	CacheControl any    `json:"cache_control,omitempty"`
}

type ClaudeResponse struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Role         string          `json:"role"`
	Content      []ClaudeContent `json:"content"`
	Model        string          `json:"model"`
	StopReason   string          `json:"stop_reason"`
	StopSequence *string         `json:"stop_sequence"`
	StopDetails  any             `json:"stop_details,omitempty"`
	Usage        ClaudeUsage     `json:"usage,omitempty"`
}

type ClaudeUsage map[string]any

// ======================== Responses API 类型 ========================

type ResponsesAPIRequest struct {
	Model              string          `json:"model"`
	Input              any             `json:"input"`
	Messages           []Message       `json:"messages,omitempty"`
	Instructions       string          `json:"instructions,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	MaxTokens          *int            `json:"max_output_tokens,omitempty"`
	TopP               *float64        `json:"top_p,omitempty"`
	FrequencyPenalty   *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty    *float64        `json:"presence_penalty,omitempty"`
	Reasoning          ReasonEffort    `json:"reasoning,omitempty"`
	Include            []string        `json:"include,omitempty"`
	Store              *bool           `json:"store,omitempty"`
	Tools              []ResponsesTool `json:"tools,omitempty"`
	ToolChoice         any             `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls,omitempty"`
	Stop               any             `json:"stop,omitempty"`
	User               string          `json:"user,omitempty"`
	StreamOptions      any             `json:"stream_options,omitempty"`
	Metadata           any             `json:"metadata,omitempty"`
	Text               any             `json:"text,omitempty"`
	Truncation         string          `json:"truncation,omitempty"`
	ServiceTier        string          `json:"service_tier,omitempty"`
	PromptCacheKey     string          `json:"prompt_cache_key,omitempty"`
	SafetyIdentifier   any             `json:"safety_identifier,omitempty"`
	TopLogprobs        *int            `json:"top_logprobs,omitempty"`
}

type ResponsesTool struct {
	Type            string         `json:"type"`
	Name            string         `json:"name,omitempty"`
	Description     string         `json:"description,omitempty"`
	Parameters      map[string]any `json:"parameters,omitempty"`
	Function        *ToolFunction  `json:"function,omitempty"`
	ServerLabel     string         `json:"server_label,omitempty"`
	ServerURL       string         `json:"server_url,omitempty"`
	ConnectorID     string         `json:"connector_id,omitempty"`
	Authorization   string         `json:"authorization,omitempty"`
	AllowedTools    []string       `json:"allowed_tools,omitempty"`
	RequireApproval any            `json:"require_approval,omitempty"`
}

type ReasonEffort struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
	Mode    string `json:"mode,omitempty"`
}

type StoredResponseState struct {
	Model        string          `json:"model"`
	Instructions string          `json:"instructions,omitempty"`
	Tools        []ResponsesTool `json:"tools,omitempty"`
	ToolChoice   any             `json:"tool_choice,omitempty"`
	Output       []any           `json:"output,omitempty"`
}

// ======================== 配置管理 ========================

// ======================== Thinking/Reasoning 判断 ========================

// ======================== 消息处理 ========================
// normalizeContent 是 dumb pipe 透传：保留 string 与 []any 两种入参形状
// （其它非常规类型走 json.Marshal 兜底），不解析或过滤任何 multimodal part。
// 能力协商由 opencode 客户端 + 上游负责；这里既不"硬降级"也不"补全"。

// ======================== Anthropic 格式兼容 ========================

// anthropicBlockState tracks per-index content block reconstruction.

// mergeUsageMaps merges src into dst. Anthropic usage values are snapshots /
// cumulative: a field present in src always replaces the value in dst (including
// 0). Nested maps are recursively merged. Fields absent from src are retained.

// parseAnthropicSSE reconstructs content blocks from Anthropic SSE events.
// It manages parallel blocks by index, handles text_delta, thinking_delta,
// signature_delta, and input_json_delta. Returns the reconstructed message,
// ordered content blocks (sorted by numeric index ascending), and an error
// if the stream is malformed/truncated.
//
// Supported line formats:
//   - raw JSON per line (one event per line)
//   - standard SSE: "data: <json>", "event: <name>", comment lines starting
//     with ":" (ignored as metadata)
//
// Malformed/truncated conditions that return an error:
//   - missing message_stop
//   - error event from upstream
//   - malformed event JSON
//   - delta for an unknown/un-started index
//   - content_block_stop for an unknown/un-started index
//   - duplicate content_block_start for the same index
//   - message_stop with unclosed (not-yet-stopped) blocks
//   - malformed tool_use input JSON

// extractBlockIndex extracts a non-negative integer index from an SSE event.
// Returns false if the index is missing, not a JSON number, is a float with
// a fractional part, is negative, NaN, Inf, or exceeds the platform's int range.
// The platform maxInt is checked BEFORE the int(f) conversion to avoid
// undefined/saturating behavior on overflow.

// deterministicResponseID returns a deterministic response ID for the given
// prefix and upstream ID. If id already has the prefix (with a non-empty
// suffix), it is kept as-is. Otherwise, a stable hex digest derived from
// sha256(id) is appended to the prefix so the same input always produces the
// same output. An empty id gets a random suffix (callers should cache).

// normalizeChatResponseID ensures a Chat response ID has the chatcmpl- prefix.

// normalizeResponsesID ensures a Responses response ID has the resp_ prefix.

// normalizeClaudeMessageID ensures a Claude message ID has the msg_ prefix.

// buildOpenAIResponse constructs a Chat Completions response from an Anthropic
// message and ordered content blocks. Text blocks are concatenated into a
// content string (preserving original order), thinking goes to
// reasoning_content, tool_use blocks populate tool_calls. A private field
// _opencode2api_anthropic_content preserves the original ordered blocks for
// Claude roundtrip; convertResponse strips it before responding to clients.

// convertAnthropicMessageToOpenAI converts a native Anthropic message JSON
// (non-streaming) to Chat Completions format. Returns an error on malformed input.

// convertAnthropicToOpenAI detects whether the upstream body is a native
// Anthropic message (JSON) or SSE stream, and converts it to Chat Completions.
// Returns an error if the body is malformed, truncated, or contains an error event.

// ======================== 响应清理 ========================

// promoteMisplacedReasoning moves reasoning_content into content when upstream
// put the visible answer in reasoning_content (opencode-go #37635). Only runs
// when content is empty and the chunk has no tool_calls, so genuine CoT that
// precedes tool calls is left alone when keepReasoning is true.

// convertStreamChunkWithUsage 转换流式 chunk 并同时提取 usage，避免二次解析

// ======================== 认证层级 ========================

// 只认 sk- 开头的 opencode key；Anthropic sk-ant-* 不能转发上游。

// isFreeModel 判断模型是否属于免费模型（以 -free 结尾）

// publicFacingModelID strips the upstream "-free" suffix for client-visible catalogs.

// mapPublicToFreeModel downgrades paid model IDs to their "-free" variants for
// keyless (public tier) requests, so deepseek-v4-flash reaches the free tier as
// deepseek-v4-flash-free instead of failing upstream with a missing key. Keyed
// tiers keep the exact requested model; models without a known free variant are
// left untouched.

// anthropicProtocolError is a typed error that carries Anthropic error
// type/message through a local protocol conversion failure. Use errors.As
// to extract it; do not parse error strings.

// writeUpstreamError writes a protocol-shaped error response for each
// downstream protocol (chat, claude, responses). Only local Anthropic
// protocol conversion errors (anthropicProtocolError via errors.As) expose
// the upstream type/message; all other errors (transport, build, etc.) get
// a generic "upstream_error" type with a stable safe message. The error's
// Error() string is never exposed. Invalid HTTP status (0, etc.) is
// normalized to 502.

// ======================== 安全响应头过滤 ========================

// ======================== Chat Completions Handler ========================

// ======================== Models Handler ========================

// ======================== Claude Messages API ========================

// claudeDocumentBlockToOpenAI maps an Anthropic document content block to a
// Chat Completions file content part. It supports source.type=base64
// (media_type, default application/pdf) and source.type=url. A filename is
// preserved from the block/title when available; no protocol ID is generated.
// Returns (nil, false) when the document lacks a usable payload so the caller
// can surface a structured 400 instead of serializing the wrapper as text.

// countCacheControlInValue counts cache_control breakpoints on content
// blocks within system, message content, and tool_result content arrays. It
// recurses into all values except input_schema and tool_use input, so a
// property named cache_control inside a schema or input object is not
// falsely counted as a breakpoint.

// countClaudeThinkingSignatures counts non-empty signature fields on
// thinking content blocks at the top level of each message's content array.
// Only actual message content blocks are counted — not nested values inside
// tool_use input or other objects that happen to have type:"thinking" and a
// signature key. The signature content itself is never recorded; only the
// count is exposed in request_plan for observability. These signatures have
// no Chat Completions equivalent and are dropped upstream.

// claudeUnsupportedBlockTypes lists Anthropic content block types that are
// dropped without a structured upstream representation. document is handled
// as a best-effort file part (see claudeDocumentBlockToOpenAI) and is not
// listed here so it is not counted as unsupported.

// ======================== Responses API ========================

// convertResponsesTextToResponseFormat translates the Responses API `text`
// parameter ({format:{type:...}, verbosity:...}) into the Chat Completions
// `response_format` shape ({type:...}) that upstream providers require.
//
// Returns nil when no representable format can be built (unknown type,
// missing required json_schema fields, or a non-object text value) so the
// caller can omit response_format instead of sending a malformed object that
// upstream would reject with a 400.

// includeHas reports whether the include array contains the given key.

// toolResultOutputKind marks the output item types that carry a tool/function
// output payload. tool_result is the Anthropic-style alias accepted by the
// Responses entrypoint in addition to the standard *_call_output types.

// normalizeToolResultOutput is the single helper that extracts a textual
// output from a tool/function output item. It prefers the standard `output`
// field; for Anthropic-style tool_result it reads `content` when `output` is
// absent. content supports a string, a string array, or an array of
// {type:"text"|"input_text"|"output_text", text:"..."} blocks joined by
// newlines in original order. The boolean reports whether a payload was
// present (an empty string is a legitimate provided output).

// joinToolResultContent renders an Anthropic tool_result content value to text.

// validateClaudeDocumentBlocks scans Anthropic Messages content for document
// blocks that lack a usable source payload. It inspects top-level message
// content blocks and nested tool_result.content blocks, but never descends
// into tool_use input, document source, schemas, or arbitrary domain data.

// validateClaudeDocumentBlocksContent inspects a content array's top-level
// blocks. For tool_result blocks, it recurses into the tool_result's own
// content array (which may contain document blocks). It never recurses into
// tool_use input or other arbitrary map values.

// validateResponsesFileItems scans Responses input for input_file content
// parts that are recognized as file inputs but lack any usable payload. It
// inspects top-level items and message content arrays only — it
// never descends into function/tool arguments, nested tool_use.input, file
// payload objects, metadata, or arbitrary maps. Returns a non-empty message
// when a malformed file item is found.

// validateResponsesFileItem validates a single top-level input item or a
// content part within a message content array. File validation applies only
// to official input paths: top-level input_file items and message content
// arrays. Output/tool_result content arrays are not validated for file
// inputs — they use text shapes only (normalizeToolResultOutput supports
// strings and text/input_text/output_text blocks).

// responsesInputFileToFile extracts a Chat Completions file object from a
// Responses input_file content part. It accepts the official common flat
// fields (file_data, file_id, file_url, filename) as well as a nested
// input_file object. Only known fields are selected — unknown/extension
// fields are never copied. file_url (nested or flat) maps to Chat
// file.file_data on a best-effort basis. Empty strings are not valid
// payloads. The official Chat file object has only file_data/file_id/filename.
// Returns (file, true) when a usable payload exists; (nil, false) otherwise.

// ======================== Responses Stream Handler ========================

// ======================== Admin 管理页面 ========================

// ======================== Main ========================
