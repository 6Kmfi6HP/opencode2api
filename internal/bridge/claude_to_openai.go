package bridge

import (
	"encoding/json"
	"strings"
)

// This file holds the pure Claude Messages -> OpenAI Chat message/tool
// converters moved out of internal/app/claude.go. They are the pure dependency
// closure of the convertClaudeRequest request boundary (anthropic_protocol.go).
// None perform I/O, logging, metrics, or config reads. The app layer keeps
// same-name lowercase forwarding shells so existing *_test.go files compile
// unchanged.

// ExtractClaudeSystemText 提取 Claude system 字段的纯文本：字符串原样、
// block 数组提取 text 分片以 "\n" 连接、其它形状序列化为 JSON。
// (Moved from app/claude.go extractClaudeSystemText.)
func ExtractClaudeSystemText(system any) string {
	if system == nil {
		return ""
	}
	switch v := system.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if block, ok := item.(map[string]any); ok {
				if block["type"] == "text" {
					if text, ok := block["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// CleanJSONSchema 递归清理 JSON Schema：剥掉 $schema/title/examples 三个
// 纯注解键（上游兼容性），保留 additionalProperties/format 等约束键。
// (Moved from app/claude.go cleanJsonSchema.)
func CleanJSONSchema(schema any) any {
	m, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	clean := make(map[string]any, len(m))
	for k, v := range m {
		// Annotation-only keys are omitted for upstream compatibility. Constraint
		// keys such as additionalProperties and format are preserved.
		if k == "$schema" || k == "title" || k == "examples" {
			continue
		}
		switch child := v.(type) {
		case map[string]any:
			clean[k] = CleanJSONSchema(child)
		case []any:
			copyArray := make([]any, len(child))
			for i, elem := range child {
				copyArray[i] = CleanJSONSchema(elem)
			}
			clean[k] = copyArray
		default:
			clean[k] = v
		}
	}
	return clean
}

// ClaudeImageBlockToOpenAI 把 Anthropic image block 转为 Chat image_url part：
// url source 直接平铺；base64 source 组装 data URI（缺 media_type 回退
// image/png）。source 缺失/非法返回 (nil,false)。
// (Moved from app/claude.go claudeImageBlockToOpenAI.)
func ClaudeImageBlockToOpenAI(block map[string]any) (map[string]any, bool) {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return nil, false
	}
	srcType, _ := source["type"].(string)
	mediaType, _ := source["media_type"].(string)
	data, _ := source["data"].(string)
	url, _ := source["url"].(string)
	if srcType == "url" && url != "" {
		return map[string]any{"type": "image_url", "image_url": map[string]string{"url": url}}, true
	}
	if srcType == "base64" && data != "" {
		if mediaType == "" {
			mediaType = "image/png"
		}
		return map[string]any{
			"type": "image_url",
			"image_url": map[string]string{
				"url": "data:" + mediaType + ";base64," + data,
			},
		}, true
	}
	return nil, false
}

// ExtractClaudeContentText 提取任意 Claude content 的纯文本：字符串原样、
// block 数组提取 text 分片以 "\n" 连接、其它形状返回 ""。
// (Moved from app/claude.go extractClaudeContentText.)
func ExtractClaudeContentText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if block["type"] == "text" {
				if text, ok := block["text"].(string); ok && text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// ClaudeToOpenAIMessages 把 Claude messages + system 转为 Chat messages：
// system/system-role 并入首条 system 消息（"\n\n" 连接）；thinking 只在携带
// tool_calls 的 assistant 消息上回放为 reasoning_content（DeepSeek 兼容）；
// tool_result 转为独立 tool 消息；image/document 转为多模态 part 或文本占位。
// (Moved from app/claude.go claudeToOpenAIMessages.)
func ClaudeToOpenAIMessages(claudeMsgs []ClaudeMessage, system any) []Message {
	var systemParts []string
	if sysText := ExtractClaudeSystemText(system); sysText != "" {
		systemParts = append(systemParts, sysText)
	}

	var body []Message
	for _, msg := range claudeMsgs {
		if msg.Role == "system" {
			if text := ExtractClaudeContentText(msg.Content); text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		}
		switch content := msg.Content.(type) {
		case string:
			body = append(body, Message{Role: msg.Role, Content: content})
		case []any:
			var orderedContent []any
			var reasoningParts []string
			var toolCalls []ToolCall
			var toolResults []Message
			var followupAttachments []any
			for _, item := range content {
				block, ok := item.(map[string]any)
				if !ok {
					continue
				}
				blockType, _ := block["type"].(string)
				switch blockType {
				case "text":
					if text, ok := block["text"].(string); ok && text != "" {
						orderedContent = append(orderedContent, map[string]any{"type": "text", "text": text})
					}
				case "image":
					if part, ok := ClaudeImageBlockToOpenAI(block); ok {
						orderedContent = append(orderedContent, part)
					} else {
						// source 缺失/非法：降级为文本占位，不静默丢上下文。
						orderedContent = append(orderedContent, map[string]any{"type": "text", "text": "[image attached]"})
					}
				case "document":
					if part, ok := ClaudeDocumentBlockToOpenAI(block); ok {
						orderedContent = append(orderedContent, part)
					} else {
						orderedContent = append(orderedContent, map[string]any{"type": "text", "text": "[document attached]"})
					}
				case "thinking":
					if thinking, ok := block["thinking"].(string); ok && thinking != "" {
						reasoningParts = append(reasoningParts, thinking)
					}
				case "tool_use":
					id, _ := block["id"].(string)
					name, _ := block["name"].(string)
					var args string
					switch input := block["input"].(type) {
					case string:
						args = input
					default:
						if input != nil {
							b, _ := json.Marshal(input)
							args = string(b)
						}
					}
					if args == "" {
						args = "{}"
					}
					toolCalls = append(toolCalls, ToolCall{
						ID:   id,
						Type: "function",
						Function: FunctionCall{
							Name:      name,
							Arguments: args,
						},
					})
				case "tool_result":
					toolUseID, _ := block["tool_use_id"].(string)
					var resultText string
					var attachmentParts []any // local per-block image/document parts in original order
					switch c := block["content"].(type) {
					case string:
						resultText = c
					case []any:
						var parts []string
						for _, p := range c {
							pb, ok := p.(map[string]any)
							if !ok {
								continue
							}
							switch pb["type"] {
							case "text":
								if t, ok := pb["text"].(string); ok {
									parts = append(parts, t)
								}
							case "image":
								if part, ok := ClaudeImageBlockToOpenAI(pb); ok {
									attachmentParts = append(attachmentParts, part)
								}
							case "document":
								if part, ok := ClaudeDocumentBlockToOpenAI(pb); ok {
									attachmentParts = append(attachmentParts, part)
								}
							default:
								// 未知嵌套 block：兜底序列化 JSON 保留上下文
								// （与顶层 unknown 分支一致），不静默丢。
								if bt, _ := pb["type"].(string); bt != "" {
									if b, err := json.Marshal(pb); err == nil {
										parts = append(parts, string(b))
									}
								}
							}
						}
						resultText = strings.Join(parts, "\n")
					default:
						if c != nil {
							b, _ := json.Marshal(c)
							resultText = string(b)
						}
					}
					// Annotate based on this block's own attachments, not a
					// global accumulator, so parallel tool_results are labeled
					// independently.
					if len(attachmentParts) > 0 {
						if resultText != "" {
							resultText += "\n"
						}
						var labels []string
						for _, ap := range attachmentParts {
							if m, ok := ap.(map[string]any); ok {
								if m["type"] == "image_url" {
									labels = append(labels, "[image attached]")
								} else if m["type"] == "file" {
									labels = append(labels, "[document attached]")
								}
							}
						}
						resultText += strings.Join(labels, "\n")
						followupAttachments = append(followupAttachments, attachmentParts...)
					}
					if isError, _ := block["is_error"].(bool); isError {
						resultText = ApplyErrorPrefix(resultText)
					}
					toolResults = append(toolResults, Message{
						Role:       "tool",
						ToolCallID: toolUseID,
						Content:    resultText,
					})
				default:
					// 未知 block（server_tool_use / web_search_tool_result /
					// redacted_thinking / 其它）：序列化成 JSON 文本 part 保
					// 留上下文，不静默蒸发（对齐 claude_responses.go 的 default
					// 分支与 sub2api）；计数仍由 scanClaudeUnsupportedBlocks 记
					// 入 unsupported_blocks。
					if blockType != "" {
						if b, err := json.Marshal(block); err == nil {
							orderedContent = append(orderedContent, map[string]any{"type": "text", "text": string(b)})
						}
					}
				}
			}
			om := Message{Role: msg.Role}
			if len(orderedContent) > 0 {
				om.Content = orderedContent
			} else if len(toolCalls) == 0 {
				om.Content = ""
			}
			if len(reasoningParts) > 0 && len(toolCalls) > 0 {
				// DeepSeek 兼容（对齐 sub2api anthropicThinkingToReasoningContent）：
				// reasoning_content 只在携带 tool_calls 的 assistant 消息上回
				// 放——DeepSeek 要求产生 tool call 的那条消息带回产生它的推理；
				// 纯文本 assistant 轮的思考直接丢弃。注意 chat.go 的
				// ensureReasoningContent 在 keepReasoning 时会为所有 assistant
				// 消息补空串槽位，这里收窄写入不受影响（它只填 nil 槽位）。
				rc := strings.Join(reasoningParts, "\n")
				om.ReasoningContent = &rc
			}
			if len(toolCalls) > 0 {
				om.ToolCalls = toolCalls
			}
			// Anthropic requires tool_result blocks to precede ordinary user
			// content. Preserve that order when translating them to Chat
			// Completions' separate tool messages.
			if msg.Role == "user" {
				body = append(body, toolResults...)
				if len(followupAttachments) > 0 {
					body = append(body, Message{Role: "user", Content: followupAttachments})
				}
			}
			if len(orderedContent) > 0 || len(reasoningParts) > 0 || len(toolCalls) > 0 || len(toolResults) == 0 {
				body = append(body, om)
			}
			if msg.Role != "user" {
				body = append(body, toolResults...)
				if len(followupAttachments) > 0 {
					body = append(body, Message{Role: "user", Content: followupAttachments})
				}
			}
		default:
			b, _ := json.Marshal(content)
			body = append(body, Message{Role: msg.Role, Content: string(b)})
		}
	}

	var messages []Message
	if len(systemParts) > 0 {
		messages = append(messages, Message{Role: "system", Content: strings.Join(systemParts, "\n\n")})
	}
	messages = append(messages, body...)
	return messages
}

// ClaudeToOpenAITools 把 Claude tools 转为 Chat function tools：server tools
// （vendor type 且无 input_schema）静默跳过并记入 skipped；input_schema 经
// CleanJSONSchema 清理，缺失补空 object。
// (Moved from app/claude.go claudeToOpenAITools.)
func ClaudeToOpenAITools(claudeTools []ClaudeTool) ([]Tool, []string) {
	tools := make([]Tool, 0, len(claudeTools))
	var skipped []string
	for _, ct := range claudeTools {
		// Server tools (web_search_*, etc.) carry a vendor type and no client schema.
		// Emitting them as empty function tools would invite bogus model calls.
		if ct.Type != "" && ct.InputSchema == nil {
			skipped = append(skipped, ct.Name)
			continue
		}
		params := ct.InputSchema
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		params = CleanJSONSchema(params)
		paramsMap, ok := params.(map[string]any)
		if !ok {
			paramsMap = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, Tool{
			Type: "function",
			Function: ToolFunction{
				Name:        ct.Name,
				Description: ct.Description,
				Parameters:  paramsMap,
			},
		})
	}
	return tools, skipped
}
