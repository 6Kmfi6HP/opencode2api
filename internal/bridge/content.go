package bridge

import (
	"encoding/json"
	"strings"
)

// This file holds the pure content / tool-result / file-item conversion
// helpers moved out of internal/app (the Responses input/content paths and the
// Claude document/file validation paths). They perform no I/O, logging,
// metrics, or config reads. The app layer keeps same-name lowercase forwarding
// shells so existing *_test.go files compile unchanged.

// ResponsesInputToMessages converts a Responses `input` payload plus optional
// `instructions` into the canonical Chat message list.
// (Moved from app/responses.go responsesInputToMessages.)
func ResponsesInputToMessages(input any, instructions string) []Message {
	var messages []Message
	if instructions != "" {
		messages = append(messages, Message{Role: "system", Content: instructions})
	}
	switch v := input.(type) {
	case string:
		messages = append(messages, Message{Role: "user", Content: v})
	case []any:
		functionOutputs := CollectFunctionOutputs(v)
		// Pre-collect call IDs present in this input array so output items
		// whose matching call is also present are not independently appended
		// (the call branch emits the paired tool message). Standalone outputs
		// (e.g. previous-response-id replay) have no matching call and still
		// append independently. Uses the same call-ID extraction rule as the
		// call branch: call_id → id → nested tool_use.id.
		callIDsPresent := map[string]bool{}
		for _, item := range v {
			elem, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch elem["type"] {
			case "function_call", "tool_call", "apply_patch_call", "shell_call":
				cid, _ := elem["call_id"].(string)
				if cid == "" {
					cid, _ = elem["id"].(string)
				}
				if cid == "" {
					if tu, ok := elem["tool_use"].(map[string]any); ok {
						cid, _ = tu["id"].(string)
					}
				}
				if cid != "" {
					callIDsPresent[cid] = true
				}
			}
		}
		for _, item := range v {
			switch elem := item.(type) {
			case string:
				messages = append(messages, Message{Role: "user", Content: elem})
			case map[string]any:
				itemType, _ := elem["type"].(string)
				switch itemType {
				case "function_call", "tool_call", "apply_patch_call", "shell_call":
					callID, _ := elem["call_id"].(string)
					if callID == "" {
						callID, _ = elem["id"].(string)
					}
					name, _ := elem["name"].(string)
					if name == "" {
						switch itemType {
						case "apply_patch_call":
							name = "apply_patch"
						case "shell_call":
							name = "shell"
						}
					}
					args, _ := elem["arguments"].(string)
					if name == "" {
						if tu, ok := elem["tool_use"].(map[string]any); ok {
							name, _ = tu["name"].(string)
							callID, _ = tu["id"].(string)
							if a, ok := tu["arguments"].(string); ok {
								args = a
							} else if inp, ok := tu["input"]; ok {
								b, _ := json.Marshal(inp)
								args = string(b)
							}
						}
					}
					if args == "" {
						args = BuildBuiltInToolCallArguments(itemType, elem)
					}
					if args == "" {
						args = "{}"
					}
					messages = append(messages, Message{
						Role:    "assistant",
						Content: "",
						ToolCalls: []ToolCall{{
							ID:   callID,
							Type: "function",
							Function: FunctionCall{
								Name:      name,
								Arguments: args,
							},
						}},
					})
					if callID != "" {
						// Map presence (not value=="") decides whether a payload
						// was provided: an empty string is a legitimate output.
						output, hasOutput := functionOutputs[callID]
						if !hasOutput {
							output = "[tool output missing]"
						}
						messages = append(messages, Message{Role: "tool", ToolCallID: callID, Content: output})
					}
				case "function_call_output", "tool_result", "apply_patch_call_output", "shell_call_output":
					callID, _ := elem["call_id"].(string)
					if callID == "" {
						callID, _ = elem["tool_use_id"].(string)
					}
					if callID != "" {
						// If the matching call item is also present in this
						// input array, skip independent emission — the call
						// branch will emit the paired assistant+tool messages,
						// preventing a leading duplicate tool message when
						// output precedes call. Standalone outputs (no matching
						// call, e.g. previous-response-id replay) still append
						// independently.
						if callIDsPresent[callID] {
							continue
						}
						// Map presence (not value=="") decides whether a payload
						// was provided: an empty string is a legitimate output.
						output, hasOutput := functionOutputs[callID]
						if !hasOutput {
							// Fallback for items not collected (e.g. output
							// field absent on a standard *_call_output). Use
							// the single normalizer so Anthropic-style content
							// is honored and the raw tool_result wrapper JSON
							// is never emitted.
							text, present := NormalizeToolResultOutput(elem)
							if present {
								output = text
								hasOutput = true
							}
						}
						if !hasOutput {
							output = "[tool output missing]"
						}
						messages = append(messages, Message{Role: "tool", ToolCallID: callID, Content: output})
					}
					continue
				case "reasoning":
					// 只保留 summary 文本（summary[*].text）作为 ReasoningContent；
					// signature 与 encrypted_content 绑定发起方且非明文，不回放为
					// 文本（摘要为空时即丢弃该条目，不注入任何原文 JSON）。
					if text := ExtractTextFromContentParts(elem["summary"]); text != "" {
						messages = append(messages, Message{Role: "assistant", Content: "", ReasoningContent: &text})
					}
					continue
				case "message", "":
					role := "user"
					if r, ok := elem["role"].(string); ok && r != "" {
						role = r
					}
					if role == "developer" {
						role = "system"
					}
					content := ResponsesContentToMessageContent(elem["content"])
					messages = append(messages, Message{Role: role, Content: content})
				case "input_file":
					// Top-level input_file item (file upload). Map to a user
					// message carrying a structured file part. Malformed items
					// (no payload) are rejected earlier by the handler, so a
					// failure here is dropped rather than serialized as text.
					if file, ok := ResponsesInputFileToFile(elem); ok {
						messages = append(messages, Message{
							Role:    "user",
							Content: []any{map[string]any{"type": "file", "file": file}},
						})
					}
					continue
				default:
					// 未知 item 类型（含 item_reference、服务端专有 item 等）静默跳过：
					// 不把原始 JSON 注入 role:user 文本污染上下文。
					continue
				}
			default:
				// 数组内裸非字符串原子（null/数字/布尔等）丢弃，不转成文本消息。
				continue
			}
		}
	default:
		b, _ := json.Marshal(v)
		messages = append(messages, Message{Role: "user", Content: string(b)})
	}
	return messages
}

// toolResultOutputKind marks the output item types that carry a tool/function
// output payload. tool_result is the Anthropic-style alias accepted by the
// Responses entrypoint in addition to the standard *_call_output types.
var toolResultOutputKind = map[string]struct{}{
	"function_call_output":    {},
	"apply_patch_call_output": {},
	"shell_call_output":       {},
	"tool_result":             {},
}

// CollectFunctionOutputs 收集 Responses input 数组里的 tool 输出（call_id → 文本）。
// (Moved from app/responses.go.)
func CollectFunctionOutputs(items []any) map[string]string {
	outputs := map[string]string{}
	for _, item := range items {
		elem, ok := item.(map[string]any)
		if !ok {
			continue
		}
		itemType, _ := elem["type"].(string)
		if _, ok := toolResultOutputKind[itemType]; !ok {
			continue
		}
		// Standard Responses items use call_id; Anthropic-style tool_result
		// uses tool_use_id when call_id is absent.
		callID, _ := elem["call_id"].(string)
		if callID == "" {
			callID, _ = elem["tool_use_id"].(string)
		}
		if callID == "" {
			continue
		}
		text, present := NormalizeToolResultOutput(elem)
		if present {
			outputs[callID] = text
		}
		// When no payload is present, the key is left absent so the caller
		// surfaces "[tool output missing]" — the raw wrapper JSON is never
		// stored as the output.
	}
	return outputs
}

// NormalizeToolResultOutput is the single helper that extracts a textual
// output from a tool/function output item. It prefers the standard `output`
// field; for Anthropic-style tool_result it reads `content` when `output` is
// absent. content supports a string, a string array, or an array of
// {type:"text"|"input_text"|"output_text", text:"..."} blocks joined by
// newlines in original order. The boolean reports whether a payload was
// present (an empty string is a legitimate provided output).
// (Moved from app/responses.go.)
func NormalizeToolResultOutput(elem map[string]any) (string, bool) {
	var text string
	present := false
	// Standard `output` field takes priority.
	if v, ok := elem["output"]; ok && v != nil {
		switch s := v.(type) {
		case string:
			text = s
		default:
			b, _ := json.Marshal(v)
			text = string(b)
		}
		present = true
	} else if c, ok := elem["content"]; ok && c != nil {
		// Anthropic-style tool_result uses `content`.
		text = JoinToolResultContent(c)
		present = true
	}
	if !present {
		return "", false
	}
	// Apply is_error prefix here so the collected map already carries error
	// semantics, independent of call/output ordering in the array.
	if isError, _ := elem["is_error"].(bool); isError {
		text = ApplyErrorPrefix(text)
	}
	return text, true
}

// JoinToolResultContent renders an Anthropic tool_result content value to text.
// (Moved from app/responses.go.)
func JoinToolResultContent(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			pb, ok := p.(map[string]any)
			if !ok {
				if s, ok := p.(string); ok {
					parts = append(parts, s)
				}
				continue
			}
			switch pb["type"] {
			case "text", "input_text", "output_text":
				if t, ok := pb["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		if c != nil {
			b, _ := json.Marshal(c)
			return string(b)
		}
		return ""
	}
}

// ParseJSONString parses a JSON string into a value, returning nil on empty
// input or parse error. (Moved from app/responses.go.)
func ParseJSONString(input string) any {
	var parsed any
	if input == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(input), &parsed); err != nil {
		return nil
	}
	return parsed
}

// BuildBuiltInToolCallArguments 把 apply_patch_call / shell_call 内联字段组装为
// JSON arguments 字符串。 (Moved from app/responses.go.)
func BuildBuiltInToolCallArguments(itemType string, elem map[string]any) string {
	if arguments, ok := elem["arguments"].(string); ok && arguments != "" {
		return arguments
	}

	payload := map[string]any{}
	switch itemType {
	case "apply_patch_call":
		if input, ok := elem["input"].(string); ok && input != "" {
			payload["input"] = input
		}
		if operation, ok := elem["operation"]; ok && operation != nil {
			payload["operation"] = operation
		}
	case "shell_call":
		for _, key := range []string{"command", "timeout_ms", "working_directory", "max_output_tokens"} {
			if value, ok := elem[key]; ok && value != nil {
				payload[key] = value
			}
		}
	}
	if len(payload) == 0 {
		payload = elem
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// BuildResponseToolCallItem 把一个 Chat ToolCall 组装为 Responses output item。
// (Moved from app/responses.go.)
func BuildResponseToolCallItem(tc ToolCall, outputType string) map[string]any {
	switch outputType {
	case "apply_patch_call":
		item := map[string]any{
			"id":      "apc_" + tc.ID,
			"type":    outputType,
			"status":  "completed",
			"call_id": tc.ID,
		}
		if parsed, ok := ParseJSONString(tc.Function.Arguments).(map[string]any); ok {
			for key, value := range parsed {
				item[key] = value
			}
		} else if tc.Function.Arguments != "" {
			item["arguments"] = tc.Function.Arguments
		}
		return item
	case "shell_call":
		item := map[string]any{
			"id":      "shc_" + tc.ID,
			"type":    outputType,
			"status":  "completed",
			"call_id": tc.ID,
		}
		if parsed, ok := ParseJSONString(tc.Function.Arguments).(map[string]any); ok {
			for key, value := range parsed {
				item[key] = value
			}
		} else if tc.Function.Arguments != "" {
			item["arguments"] = tc.Function.Arguments
		}
		return item
	default:
		return map[string]any{
			"id":        "fc_" + tc.ID,
			"type":      "function_call",
			"status":    "completed",
			"arguments": tc.Function.Arguments,
			"call_id":   tc.ID,
			"name":      tc.Function.Name,
		}
	}
}

// CloneJSONValue deep-copies a JSON-shaped value via marshal/unmarshal,
// returning the input unchanged on error. (Moved from app/responses.go.)
func CloneJSONValue[T any](value T) T {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var cloned T
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		return value
	}
	return cloned
}

// ExtractTextFromContentParts 从 content（字符串或 parts 数组）提取
// input_text/output_text 文本。 (Moved from app/responses.go.)
func ExtractTextFromContentParts(content any) string {
	parts, ok := content.([]any)
	if !ok {
		if s, ok := content.(string); ok {
			return s
		}
		return ""
	}
	var texts []string
	for _, p := range parts {
		if part, ok := p.(map[string]any); ok {
			if part["type"] == "input_text" || part["type"] == "output_text" {
				if t, ok := part["text"].(string); ok {
					texts = append(texts, t)
				}
			}
		}
	}
	return strings.Join(texts, "\n")
}

// ConvertResponsesContentPart 转换单个 Responses content part 为 Chat part。
// (Moved from app/responses.go.)
func ConvertResponsesContentPart(part map[string]any) (map[string]any, bool) {
	partType, _ := part["type"].(string)
	switch partType {
	case "input_text", "output_text", "text":
		text, _ := part["text"].(string)
		if text == "" {
			return nil, false
		}
		return map[string]any{
			"type": "text",
			"text": text,
		}, true
	case "input_image":
		imageURL, _ := part["image_url"].(string)
		if imageURL == "" {
			return nil, false
		}
		imageURLValue := map[string]any{
			"url": imageURL,
		}
		if detail, ok := part["detail"].(string); ok && detail != "" {
			imageURLValue["detail"] = detail
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": imageURLValue,
		}, true
	case "input_file":
		file, ok := ResponsesInputFileToFile(part)
		if !ok {
			return nil, false
		}
		return map[string]any{"type": "file", "file": file}, true
	default:
		return nil, false
	}
}

// ResponsesContentToMessageContent 转换 Responses content 为 Chat message content。
// (Moved from app/responses.go.)
func ResponsesContentToMessageContent(content any) any {
	if content == nil {
		return nil
	}
	if s, ok := content.(string); ok {
		return s
	}

	parts, ok := content.([]any)
	if !ok {
		b, err := json.Marshal(content)
		if err != nil {
			return nil
		}
		return string(b)
	}

	convertedParts := make([]any, 0, len(parts))
	texts := make([]string, 0, len(parts))
	onlyTextParts := true

	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		convertedPart, ok := ConvertResponsesContentPart(part)
		if !ok {
			text := ExtractTextFromContentParts([]any{part})
			if text == "" {
				b, err := json.Marshal(part)
				if err != nil {
					continue
				}
				text = string(b)
			}
			convertedParts = append(convertedParts, map[string]any{
				"type": "text",
				"text": text,
			})
			texts = append(texts, text)
			continue
		}

		if convertedPart["type"] != "text" {
			onlyTextParts = false
		}
		if text, ok := convertedPart["text"].(string); ok && text != "" {
			texts = append(texts, text)
		}
		convertedParts = append(convertedParts, convertedPart)
	}

	if len(convertedParts) == 0 {
		return ""
	}
	if onlyTextParts {
		return strings.Join(texts, "\n")
	}
	return convertedParts
}

// ChatContentToResponsesContent 转换 Chat content 为 Responses output content
// parts，并返回拼接出的可见文本。 (Moved from app/responses.go.)
func ChatContentToResponsesContent(content any) ([]any, string) {
	switch v := content.(type) {
	case nil:
		return nil, ""
	case string:
		if v == "" {
			return nil, ""
		}
		return []any{map[string]any{
			"type":        "output_text",
			"text":        v,
			"annotations": []any{},
			"logprobs":    []any{},
		}}, v
	case []any:
		parts := make([]any, 0, len(v))
		texts := make([]string, 0, len(v))
		for _, rawPart := range v {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			partType, _ := part["type"].(string)
			switch partType {
			case "text", "input_text", "output_text":
				text, _ := part["text"].(string)
				if text == "" {
					continue
				}
				annotations, ok := part["annotations"]
				if !ok {
					annotations = []any{}
				}
				logprobs, ok := part["logprobs"]
				if !ok {
					logprobs = []any{}
				}
				texts = append(texts, text)
				parts = append(parts, map[string]any{
					"type":        "output_text",
					"text":        text,
					"annotations": annotations,
					"logprobs":    logprobs,
				})
			}
		}
		return parts, strings.Join(texts, "\n")
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, ""
		}
		text := string(b)
		return []any{map[string]any{
			"type":        "output_text",
			"text":        text,
			"annotations": []any{},
			"logprobs":    []any{},
		}}, text
	}
}

// ValidateClaudeDocumentBlocks 校验 Claude messages 中所有 document block 均有
// 可用 source；返回首个错误消息，无错返回 ""。
// (Moved from app/responses.go validateClaudeDocumentBlocks.)
func ValidateClaudeDocumentBlocks(msgs []ClaudeMessage) string {
	for _, msg := range msgs {
		if m := validateClaudeDocumentBlocksContent(msg.Content); m != "" {
			return m
		}
	}
	return ""
}

// validateClaudeDocumentBlocksContent 递归校验 content 数组中的 document /
// tool_result 嵌套 document block。
// (Moved from app/responses.go.)
func validateClaudeDocumentBlocksContent(content any) string {
	blocks, ok := content.([]any)
	if !ok {
		return ""
	}
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		bt, _ := block["type"].(string)
		if bt == "document" {
			if _, ok := ClaudeDocumentBlockToOpenAI(block); !ok {
				return "document is missing a usable source payload"
			}
		}
		if bt == "tool_result" {
			// tool_result content is itself a content array that may contain
			// document blocks. Recurse into it, but not into any other fields.
			if m := validateClaudeDocumentBlocksContent(block["content"]); m != "" {
				return m
			}
		}
	}
	return ""
}

// ValidateResponsesFileItems 校验 Responses input 数组里的 file item，返回首个
// 错误消息。 (Moved from app/responses.go.)
func ValidateResponsesFileItems(input any) string {
	switch v := input.(type) {
	case []any:
		for _, item := range v {
			if msg := validateResponsesFileItem(item); msg != "" {
				return msg
			}
		}
	}
	return ""
}

// validateResponsesFileItem validates a single top-level input item or a
// content part within a message content array. File validation applies only
// to official input paths: top-level input_file items and message content
// arrays. Output/tool_result content arrays are not validated for file
// inputs — they use text shapes only (NormalizeToolResultOutput supports
// strings and text/input_text/output_text blocks).
// (Moved from app/responses.go.)
func validateResponsesFileItem(item any) string {
	elem, ok := item.(map[string]any)
	if !ok {
		return ""
	}
	itemType, _ := elem["type"].(string)
	// Top-level input_file item or input_file content part.
	if itemType == "input_file" {
		if _, ok := ResponsesInputFileToFile(elem); !ok {
			return "input_file is missing file_data, file_id, and file_url"
		}
		return ""
	}
	// For message items, recurse into the content array (content parts).
	if itemType == "message" || itemType == "" {
		if content, ok := elem["content"].([]any); ok {
			for _, part := range content {
				if msg := validateResponsesFileItem(part); msg != "" {
					return msg
				}
			}
		}
		return ""
	}
	// All other item types (function_call, tool_call, tool_result,
	// apply_patch_call, shell_call, reasoning, *_call_output, etc.) are not
	// inspected — their arguments/input/content fields are not file inputs.
	return ""
}

// ResponsesInputFileToFile 把 Responses input_file item/part 提取为 Chat
// `file` 对象。 返回 (file, true) 当存在可用 payload，否则 (nil, false)。
// (Moved from app/responses.go.)
func ResponsesInputFileToFile(part map[string]any) (map[string]any, bool) {
	file := map[string]any{}

	// Helper: read a non-empty string from a map by key.
	nonEmptyStr := func(m map[string]any, key string) (string, bool) {
		if v, ok := m[key].(string); ok && v != "" {
			return v, true
		}
		return "", false
	}

	// Nested input_file object: {"type":"input_file","input_file":{...}}.
	// Only select known fields; do not wholesale-copy.
	if nested, ok := part["input_file"].(map[string]any); ok {
		if v, ok := nonEmptyStr(nested, "file_data"); ok {
			file["file_data"] = v
		}
		if v, ok := nonEmptyStr(nested, "file_id"); ok {
			file["file_id"] = v
		}
		// nested file_url maps to file.file_data (best-effort).
		if v, ok := nonEmptyStr(nested, "file_url"); ok {
			file["file_data"] = v
		}
		if v, ok := nonEmptyStr(nested, "filename"); ok {
			file["filename"] = v
		}
	}

	// Flat fields take priority over nested values.
	if v, ok := nonEmptyStr(part, "file_data"); ok {
		file["file_data"] = v
	}
	if v, ok := nonEmptyStr(part, "file_id"); ok {
		file["file_id"] = v
	}
	// Flat file_url maps to file.file_data (best-effort).
	if v, ok := nonEmptyStr(part, "file_url"); ok {
		file["file_data"] = v
	}
	if v, ok := nonEmptyStr(part, "filename"); ok {
		file["filename"] = v
	}

	// A usable payload requires at least one of file_data / file_id.
	if _, hasData := file["file_data"]; !hasData {
		if _, hasID := file["file_id"]; !hasID {
			return nil, false
		}
	}
	return file, true
}

// ClaudeDocumentBlockToOpenAI 把 Claude document block 转为 Chat `file` part。
// (Moved from app/claude.go claudeDocumentBlockToOpenAI.)
func ClaudeDocumentBlockToOpenAI(block map[string]any) (map[string]any, bool) {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return nil, false
	}
	srcType, _ := source["type"].(string)
	mediaType, _ := source["media_type"].(string)
	if mediaType == "" {
		mediaType = "application/pdf"
	}
	data, _ := source["data"].(string)
	url, _ := source["url"].(string)

	file := map[string]any{}
	if filename, ok := block["filename"].(string); ok && filename != "" {
		file["filename"] = filename
	} else if title, ok := block["title"].(string); ok && title != "" {
		file["filename"] = title
	}

	switch srcType {
	case "base64":
		if data == "" {
			return nil, false
		}
		file["file_data"] = "data:" + mediaType + ";base64," + data
		return map[string]any{"type": "file", "file": file}, true
	case "url":
		if url == "" {
			return nil, false
		}
		file["file_data"] = url
		return map[string]any{"type": "file", "file": file}, true
	}
	return nil, false
}
