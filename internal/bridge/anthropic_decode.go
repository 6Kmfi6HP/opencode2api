package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// This file holds the pure Anthropic Messages wire decoders and the
// Anthropic -> Chat Completions non-streaming converter moved out of
// internal/app/claude.go and internal/app/chat.go. They parse a complete
// upstream Anthropic SSE body (or a single message JSON), reconstruct the
// ordered content blocks, and re-emit an OpenAI Chat Completions response.
// None perform I/O, logging, metrics, or config reads; the only injected
// dependency is the IDGen used to mint tool-call/response IDs. The app layer
// keeps same-name lowercase forwarding shells so existing *_test.go files
// compile unchanged.

// ProtocolError carries a typed upstream protocol error (e.g. an Anthropic
// error event or error body) across the bridge boundary without importing any
// app-layer error type. The app shell wraps it into its own
// anthropicProtocolError so the wire error string stays byte-identical
// ("<type>: <message>"). Errors() formats identically to the old app type so
// any direct (unwrapped) inspection still matches.
type ProtocolError struct {
	ErrType string
	Message string
}

// Error returns "<ErrType>: <Message>" when a type is present, else just the
// message — byte-identical to the app-layer anthropicProtocolError format.
func (e *ProtocolError) Error() string {
	if e.ErrType != "" {
		return e.ErrType + ": " + e.Message
	}
	return e.Message
}

// IsAnthropicFormat reports whether a body looks like an Anthropic Messages
// payload: a single {"type":"message"} JSON object, or an SSE stream whose
// first meaningful line is a known Anthropic event type.
// (Moved from app/claude.go isAnthropicFormat.)
func IsAnthropicFormat(body []byte) bool {
	var obj map[string]any
	if json.Unmarshal(body, &obj) == nil {
		if typ, _ := obj["type"].(string); typ == "message" {
			return true
		}
	}
	lines := bytes.Split(body, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		// Support "data: " prefixed SSE lines.
		if bytes.HasPrefix(line, []byte("data: ")) {
			line = bytes.TrimSpace(line[6:])
		} else if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(line[5:])
		}
		if len(line) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		typ, _ := event["type"].(string)
		switch typ {
		case "message_start", "content_block_start", "content_block_delta",
			"content_block_stop", "message_delta", "message_stop", "ping",
			"error":
			return true
		}
		return false
	}
	return false
}

// AnthropicBlockState tracks per-index content block reconstruction.
// (Moved from app/claude.go anthropicBlockState.)
type AnthropicBlockState struct {
	BlockType     string
	ID            string
	Name          string
	Signature     string
	Data          string
	TextBuilder   strings.Builder
	ThinkBuilder  strings.Builder
	JSONBuilder   strings.Builder
	InitialInput  any
	SawInputDelta bool
	Started       bool
	Stopped       bool
}

// UsageHasCompletion reports whether an upstream usage object includes output
// token accounting, which upstreams send as the terminal chunk when
// stream_options.include_usage is set.
// (Moved from app/claude.go usageHasCompletion.)
func UsageHasCompletion(usage map[string]any) bool {
	if len(usage) == 0 {
		return false
	}
	if v, ok := usage["completion_tokens"]; ok {
		if n, ok := v.(float64); ok && n >= 0 {
			return true
		}
	}
	if v, ok := usage["output_tokens"]; ok {
		if n, ok := v.(float64); ok && n >= 0 {
			return true
		}
	}
	return false
}

// ParseAnthropicSSE consumes a complete Anthropic Messages SSE body and
// returns the terminal message object plus the reconstructed content blocks.
//
// Accepted framing:
//   - standard SSE: "data: <json>", "event: <name>", comment lines starting
//     with ":" (ignored as metadata)
//
// Malformed/truncated conditions that return an error:
//   - missing message_stop
//   - error event from upstream (returned as *ProtocolError)
//   - malformed event JSON
//   - delta for an unknown/un-started index
//   - content_block_stop for an unknown/un-started index
//   - duplicate content_block_start for the same index
//   - message_stop with unclosed (not-yet-stopped) blocks
//   - malformed tool_use input JSON
//
// (Moved from app/claude.go parseAnthropicSSE.)
func ParseAnthropicSSE(body []byte) (map[string]any, []map[string]any, error) {
	lines := bytes.Split(body, []byte("\n"))
	var anthropicMsg map[string]any
	blocks := map[int]*AnthropicBlockState{}
	sawMessageStop := false
	messageStartCount := 0

	for _, rawLine := range lines {
		line := bytes.TrimSpace(rawLine)
		if len(line) == 0 {
			continue
		}
		// Standard SSE metadata lines: "event: ...", "id: ...", comment ": ..."
		if bytes.HasPrefix(line, []byte("event:")) ||
			bytes.HasPrefix(line, []byte("id:")) ||
			bytes.HasPrefix(line, []byte(":")) {
			continue
		}
		// Support "data: " prefixed SSE lines.
		if bytes.HasPrefix(line, []byte("data: ")) {
			line = bytes.TrimSpace(line[6:])
		} else if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(line[5:])
		}
		if len(line) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, nil, fmt.Errorf("malformed SSE event JSON: %w", err)
		}
		typ, _ := event["type"].(string)

		// After message_stop, only ping events and comment/metadata lines
		// are allowed. Comment/metadata lines are already filtered above.
		// Any other event is an error.
		if sawMessageStop && typ != "ping" {
			return nil, nil, fmt.Errorf("unexpected event %q after message_stop", typ)
		}

		switch typ {
		case "message_start":
			messageStartCount++
			if messageStartCount > 1 {
				return nil, nil, fmt.Errorf("multiple message_start events in SSE stream")
			}
			m, ok := event["message"].(map[string]any)
			if !ok || m == nil {
				return nil, nil, fmt.Errorf("message_start missing non-nil message object")
			}
			anthropicMsg = m
		case "content_block_start":
			if messageStartCount == 0 {
				return nil, nil, fmt.Errorf("content_block_start before message_start")
			}
			idx, ok := ExtractBlockIndex(event)
			if !ok {
				return nil, nil, fmt.Errorf("content_block_start missing valid non-negative integer index")
			}
			if existing, ok := blocks[idx]; ok && existing.Started {
				return nil, nil, fmt.Errorf("duplicate content_block_start for index %d", idx)
			}
			cb, _ := event["content_block"].(map[string]any)
			cbType, _ := cb["type"].(string)
			if cbType == "" {
				return nil, nil, fmt.Errorf("content_block_start missing content_block type")
			}
			if cbType != "text" && cbType != "thinking" && cbType != "redacted_thinking" && cbType != "tool_use" {
				return nil, nil, fmt.Errorf("content_block_start unsupported type %q", cbType)
			}
			st := &AnthropicBlockState{BlockType: cbType, Started: true}
			if cb != nil {
				if id, ok := cb["id"].(string); ok {
					st.ID = id
				}
				if name, ok := cb["name"].(string); ok {
					st.Name = name
				}
				// tool_use must have a non-empty name.
				if cbType == "tool_use" && st.Name == "" {
					return nil, nil, fmt.Errorf("tool_use content_block_start missing non-empty name")
				}
				if sig, ok := cb["signature"].(string); ok {
					st.Signature = sig
				}
				if d, ok := cb["data"].(string); ok {
					st.Data = d
				}
				// Preserve initial text if provided.
				if t, ok := cb["text"].(string); ok && t != "" {
					st.TextBuilder.WriteString(t)
				}
				if t, ok := cb["thinking"].(string); ok && t != "" {
					st.ThinkBuilder.WriteString(t)
				}
				// Preserve initial input if provided as a non-empty value.
				// In Anthropic SSE, content_block_start.input is typically {}
				// and the actual input arrives via input_json_delta partials.
				// Store separately so initial input and partial deltas are not
				// concatenated into invalid JSON.
				if input, ok := cb["input"]; ok && input != nil {
					if inputStr, ok := input.(string); ok && inputStr != "" {
						st.InitialInput = inputStr
					} else if m, ok := input.(map[string]any); ok && len(m) > 0 {
						st.InitialInput = input
					}
				}
			}
			blocks[idx] = st
		case "content_block_delta":
			if messageStartCount == 0 {
				return nil, nil, fmt.Errorf("content_block_delta before message_start")
			}
			idx, ok := ExtractBlockIndex(event)
			if !ok {
				return nil, nil, fmt.Errorf("content_block_delta missing valid non-negative integer index")
			}
			st, ok := blocks[idx]
			if !ok || !st.Started {
				return nil, nil, fmt.Errorf("content_block_delta for unknown index %d", idx)
			}
			if st.Stopped {
				return nil, nil, fmt.Errorf("content_block_delta for already-stopped index %d", idx)
			}
			delta, ok := event["delta"].(map[string]any)
			if !ok || delta == nil {
				return nil, nil, fmt.Errorf("content_block_delta for index %d missing delta object", idx)
			}
			dt, _ := delta["type"].(string)
			if dt == "" {
				return nil, nil, fmt.Errorf("content_block_delta for index %d missing delta type", idx)
			}
			switch dt {
			case "text_delta":
				if t, ok := delta["text"].(string); ok {
					st.TextBuilder.WriteString(t)
				}
			case "thinking_delta":
				if t, ok := delta["thinking"].(string); ok {
					st.ThinkBuilder.WriteString(t)
				}
			case "signature_delta":
				if sig, ok := delta["signature"].(string); ok {
					st.Signature += sig
				}
			case "input_json_delta":
				if partial, ok := delta["partial_json"].(string); ok {
					st.JSONBuilder.WriteString(partial)
					st.SawInputDelta = true
				}
			default:
				// Unknown delta type: ignore.
			}
		case "content_block_stop":
			if messageStartCount == 0 {
				return nil, nil, fmt.Errorf("content_block_stop before message_start")
			}
			idx, ok := ExtractBlockIndex(event)
			if !ok {
				return nil, nil, fmt.Errorf("content_block_stop missing valid non-negative integer index")
			}
			st, ok := blocks[idx]
			if !ok || !st.Started {
				return nil, nil, fmt.Errorf("content_block_stop for unknown index %d", idx)
			}
			if st.Stopped {
				return nil, nil, fmt.Errorf("duplicate content_block_stop for index %d", idx)
			}
			st.Stopped = true
		case "message_delta":
			if messageStartCount == 0 {
				return nil, nil, fmt.Errorf("message_delta before message_start")
			}
			if anthropicMsg == nil {
				anthropicMsg = map[string]any{}
			}
			if delta, ok := event["delta"].(map[string]any); ok {
				if stop, ok := delta["stop_reason"].(string); ok {
					anthropicMsg["stop_reason"] = stop
				}
			}
			// message_delta usage is at the event top level (Anthropic spec).
			if usage, ok := event["usage"].(map[string]any); ok {
				anthropicMsg["usage"] = MergeUsageMaps(anthropicMsg["usage"], usage)
			} else if delta, ok := event["delta"].(map[string]any); ok {
				if usage, ok := delta["usage"].(map[string]any); ok {
					anthropicMsg["usage"] = MergeUsageMaps(anthropicMsg["usage"], usage)
				}
			}
		case "message_stop":
			// Validate at message_stop time: must have message_start,
			// all blocks stopped, and non-empty stop_reason.
			if messageStartCount == 0 {
				return nil, nil, fmt.Errorf("message_stop before message_start")
			}
			for idx, st := range blocks {
				if st != nil && st.Started && !st.Stopped {
					return nil, nil, fmt.Errorf("message_stop with unclosed block at index %d", idx)
				}
			}
			stopReason, _ := anthropicMsg["stop_reason"].(string)
			if stopReason == "" {
				return nil, nil, fmt.Errorf("message_stop without stop_reason")
			}
			sawMessageStop = true
		case "error":
			errType := "api_error"
			errMsg := "upstream Anthropic error"
			if errMap, ok := event["error"].(map[string]any); ok {
				if t, ok := errMap["type"].(string); ok && t != "" {
					errType = t
				}
				if m, ok := errMap["message"].(string); ok && m != "" {
					errMsg = m
				}
			}
			return nil, nil, &ProtocolError{ErrType: errType, Message: errMsg}
		default:
			// Unknown event type: ignore.
		}
	}

	if messageStartCount == 0 {
		return nil, nil, fmt.Errorf("anthropic SSE stream missing message_start")
	}
	if !sawMessageStop {
		return nil, nil, fmt.Errorf("anthropic SSE stream ended without message_stop")
	}

	// Build ordered content blocks sorted by numeric index ascending.
	indices := make([]int, 0, len(blocks))
	for idx := range blocks {
		indices = append(indices, idx)
	}
	sort.Ints(indices)

	var contentBlocks []map[string]any
	for _, idx := range indices {
		st := blocks[idx]
		if st == nil {
			continue
		}
		switch st.BlockType {
		case "text":
			contentBlocks = append(contentBlocks, map[string]any{
				"type": "text",
				"text": st.TextBuilder.String(),
			})
		case "thinking":
			blk := map[string]any{
				"type":     "thinking",
				"thinking": st.ThinkBuilder.String(),
			}
			if st.Signature != "" {
				blk["signature"] = st.Signature
			}
			contentBlocks = append(contentBlocks, blk)
		case "redacted_thinking":
			blk := map[string]any{
				"type": "redacted_thinking",
			}
			if st.Data != "" {
				blk["data"] = st.Data
			}
			contentBlocks = append(contentBlocks, blk)
		case "tool_use":
			var input any
			if st.SawInputDelta {
				// Parse accumulated partial JSON deltas.
				inputStr := st.JSONBuilder.String()
				if inputStr != "" {
					var parsed any
					if err := json.Unmarshal([]byte(inputStr), &parsed); err != nil {
						return nil, nil, fmt.Errorf("malformed tool_use input JSON for index %d: %w", idx, err)
					}
					input = parsed
				} else {
					input = map[string]any{}
				}
			} else if st.InitialInput != nil {
				// Use initial input from content_block_start.
				if inputStr, ok := st.InitialInput.(string); ok {
					var parsed any
					if err := json.Unmarshal([]byte(inputStr), &parsed); err != nil {
						return nil, nil, fmt.Errorf("malformed tool_use initial input for index %d: %w", idx, err)
					}
					input = parsed
				} else {
					input = st.InitialInput
				}
			} else {
				input = map[string]any{}
			}
			blk := map[string]any{
				"type":  "tool_use",
				"input": input,
			}
			if st.ID != "" {
				blk["id"] = st.ID
			}
			if st.Name != "" {
				blk["name"] = st.Name
			}
			contentBlocks = append(contentBlocks, blk)
		default:
			// Unknown block type: skip.
		}
	}

	if anthropicMsg == nil {
		anthropicMsg = map[string]any{}
	}
	return anthropicMsg, contentBlocks, nil
}

// ExtractBlockIndex reads the non-negative integer "index" field of an
// Anthropic SSE event, rejecting fractional / NaN / Inf / out-of-range values
// with undefined/saturating behavior on overflow.
// (Moved from app/claude.go extractBlockIndex.)
func ExtractBlockIndex(event map[string]any) (int, bool) {
	rawIdx, ok := event["index"]
	if !ok || rawIdx == nil {
		return 0, false
	}
	f, ok := rawIdx.(float64)
	if !ok {
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	if math.Trunc(f) != f || f < 0 {
		return 0, false
	}
	// Check platform int range BEFORE converting, to avoid overflow.
	// On 64-bit: maxInt = 2^63-1; on 32-bit: maxInt = 2^31-1.
	// float64 can't represent all int64 values, so also cap at 2^53
	// (where all integers are exactly representable as float64).
	maxInt := float64(1<<(strconv.IntSize-1) - 1)
	if f > maxInt {
		return 0, false
	}
	// Also reject values above the float64 exact-integer upper bound.
	if f > float64(1<<53) {
		return 0, false
	}
	idx := int(f)
	if float64(idx) != f {
		return 0, false
	}
	return idx, true
}

// BuildOpenAIResponse assembles a Chat Completions response from a terminal
// Anthropic message plus its ordered content blocks. nowFn stamps "created";
// idGen mints toolu_ tool-call IDs (and the fallback response ID via
// NormalizeChatResponseID).
//
// Claude roundtrip: when any non-text native block exists (thinking,
// redacted_thinking, tool_use) the original ordered blocks are preserved in a
// private "_opencode2api_anthropic_content" field; convertResponse strips it
// before responding to clients. Generated tool IDs are written back to the
// source blocks so Claude roundtrip associations stay consistent.
//
// (Moved from app/chat.go buildOpenAIResponse.)
func BuildOpenAIResponse(nowFn NowFn, idGen IDGen, anthropicMsg map[string]any, contentBlocks []map[string]any, modelID string) ([]byte, error) {
	if anthropicMsg == nil {
		return nil, fmt.Errorf("no Anthropic message to convert")
	}
	now := nowFn()
	role, _ := anthropicMsg["role"].(string)
	if role == "" {
		role = "assistant"
	}
	finishReason, _ := anthropicMsg["stop_reason"].(string)
	// Anthropic 的 refusal：stop_reason 原样保留 "refusal"，并（对齐 sub2api）
	// 把文本放进 message.refusal、content 置空。
	isRefusal := finishReason == "refusal"
	finishReason = NormalizeFinishReason(finishReason)

	var textBuilder strings.Builder
	var reasoningContent string
	var toolCalls []map[string]any
	hasNonText := false

	for _, blk := range contentBlocks {
		bt, _ := blk["type"].(string)
		switch bt {
		case "text":
			if t, ok := blk["text"].(string); ok {
				textBuilder.WriteString(t)
			}
		case "thinking":
			hasNonText = true
			if t, ok := blk["thinking"].(string); ok {
				if reasoningContent != "" {
					reasoningContent += "\n"
				}
				reasoningContent += t
			}
		case "redacted_thinking":
			hasNonText = true
		case "tool_use":
			hasNonText = true
			input := blk["input"]
			if input == nil {
				input = map[string]any{}
			}
			argsJSON, _ := json.Marshal(input)
			toolID, _ := blk["id"].(string)
			if toolID == "" {
				toolID = "toolu_" + idGen("toolu_", 12)
				blk["id"] = toolID
			}
			toolName, _ := blk["name"].(string)
			toolCalls = append(toolCalls, map[string]any{
				"id":   toolID,
				"type": "function",
				"function": map[string]any{
					"name":      toolName,
					"arguments": string(argsJSON),
				},
			})
		default:
			// Unknown non-empty block type: preserve for private roundtrip.
			if bt != "" {
				hasNonText = true
			}
		}
	}

	msg := map[string]any{"role": role}

	// Determine content: if only text blocks, use a string for compatibility.
	textStr := textBuilder.String()
	if isRefusal {
		// refusal 优先：文本进 message.refusal，content 置空（对齐 sub2api）。
		// Chat->Chat 转换里 content 若为 parts 数组则保留现状，仅标注语义。
		msg["content"] = nil
		msg["refusal"] = textStr
	} else if !hasNonText {
		msg["content"] = textStr
	} else {
		if textStr != "" {
			msg["content"] = textStr
		} else {
			msg["content"] = nil
		}
	}

	if reasoningContent != "" {
		msg["reasoning_content"] = reasoningContent
	}

	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}

	// Private field for Claude roundtrip: preserves original ordered blocks
	// whenever any non-text native block exists (thinking, redacted_thinking,
	// tool_use). Generated tool IDs are written back to the blocks so that
	// Claude roundtrip associations are consistent.
	if hasNonText {
		privateBlocks := make([]map[string]any, 0, len(contentBlocks))
		for _, blk := range contentBlocks {
			privateBlocks = append(privateBlocks, blk)
		}
		msg["_opencode2api_anthropic_content"] = privateBlocks
	}

	choice := map[string]any{
		"index":         0,
		"message":       msg,
		"finish_reason": finishReason,
	}

	resp := map[string]any{
		"id":      NormalizeChatResponseID(idGen, ToString(anthropicMsg["id"])),
		"object":  "chat.completion",
		"created": now,
		"model":   modelID,
		"choices": []map[string]any{choice},
	}
	if usage, ok := anthropicMsg["usage"].(map[string]any); ok {
		resp["usage"] = AnthropicUsageToChat(usage)
	}
	result, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Chat response: %w", err)
	}
	return result, nil
}

// ConvertAnthropicMessageToOpenAI converts a single Anthropic message JSON
// (non-streaming) to Chat Completions format. Returns an error on malformed input.
// (Moved from app/chat.go convertAnthropicMessageToOpenAI.)
func ConvertAnthropicMessageToOpenAI(nowFn NowFn, idGen IDGen, msg map[string]any, modelID string) ([]byte, error) {
	if msg == nil {
		return nil, fmt.Errorf("no Anthropic message to convert")
	}
	if msg["model"] == nil {
		msg["model"] = modelID
	}
	// Direct non-stream message requires a content array.
	content, ok := msg["content"].([]any)
	if !ok {
		return nil, fmt.Errorf("anthropic message missing content array")
	}
	var contentBlocks []map[string]any
	for _, c := range content {
		block, ok := c.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("anthropic message content contains non-object block")
		}
		bt, _ := block["type"].(string)
		switch bt {
		case "text", "thinking", "redacted_thinking":
			// Supported types.
		case "tool_use":
			// tool_use must have a non-empty name.
			name, _ := block["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("tool_use block missing non-empty name")
			}
			// tool_use input must be JSON-marshalable.
			if input, exists := block["input"]; exists && input != nil {
				if _, err := json.Marshal(input); err != nil {
					return nil, fmt.Errorf("tool_use input not JSON-marshalable: %w", err)
				}
			}
		default:
			if bt == "" {
				return nil, fmt.Errorf("anthropic message content block missing type")
			}
			// Unknown non-empty block type: keep in private blocks for
			// potential roundtrip; public Chat ignores it.
		}
		contentBlocks = append(contentBlocks, block)
	}
	// Direct non-stream message requires a non-empty stop_reason.
	stopReason, _ := msg["stop_reason"].(string)
	if stopReason == "" {
		return nil, fmt.Errorf("anthropic message missing stop_reason")
	}
	return BuildOpenAIResponse(nowFn, idGen, msg, contentBlocks, modelID)
}

// ConvertAnthropicToOpenAI converts an upstream Anthropic Messages body
// (single-message JSON or complete SSE stream) to Chat Completions format.
// Returns an error if the body is malformed, truncated, or contains an error
// event (a *ProtocolError, which the app shell wraps into its own typed error).
// (Moved from app/chat.go convertAnthropicToOpenAI.)
func ConvertAnthropicToOpenAI(nowFn NowFn, idGen IDGen, body []byte, modelID string) ([]byte, error) {
	var singleMsg map[string]any
	if json.Unmarshal(body, &singleMsg) == nil {
		if typ, _ := singleMsg["type"].(string); typ == "message" {
			return ConvertAnthropicMessageToOpenAI(nowFn, idGen, singleMsg, modelID)
		}
		// Could be a single error object.
		if typ, _ := singleMsg["type"].(string); typ == "error" {
			errType := "api_error"
			errMsg := "upstream Anthropic error"
			if errMap, ok := singleMsg["error"].(map[string]any); ok {
				if t, ok := errMap["type"].(string); ok && t != "" {
					errType = t
				}
				if m, ok := errMap["message"].(string); ok && m != "" {
					errMsg = m
				}
			}
			return nil, &ProtocolError{ErrType: errType, Message: errMsg}
		}
	}
	msg, contentBlocks, err := ParseAnthropicSSE(body)
	if err != nil {
		return nil, err
	}
	if msg["model"] == nil {
		msg["model"] = modelID
	}
	return BuildOpenAIResponse(nowFn, idGen, msg, contentBlocks, modelID)
}
