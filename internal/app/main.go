package app

import (
	"context"
	"fmt"

	"github.com/6Kmfi6HP/opencode2api/internal/domain"
)

var (
	version = "v0.4.4"
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

// Protocol DTOs live in internal/domain. Aliases keep the root package main
// surface stable while the implementation is split into packages.
type OpenAIRequest = domain.OpenAIRequest
type Message = domain.Message
type ToolCall = domain.ToolCall
type FunctionCall = domain.FunctionCall
type Tool = domain.Tool
type ToolFunction = domain.ToolFunction
type AppConfig = domain.AppConfig
type Socks5Proxy = domain.Socks5Proxy
type ClaudeRequest = domain.ClaudeRequest
type ClaudeMessage = domain.ClaudeMessage
type ClaudeContent = domain.ClaudeContent
type ClaudeTool = domain.ClaudeTool
type ClaudeResponse = domain.ClaudeResponse
type ClaudeUsage = domain.ClaudeUsage
type ResponsesAPIRequest = domain.ResponsesAPIRequest
type ResponsesTool = domain.ResponsesTool
type ReasonEffort = domain.ReasonEffort
type StoredResponseState = domain.StoredResponseState

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
