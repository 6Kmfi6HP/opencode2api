package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// ======================== Claude -> Responses 原生转换（lenient） ========================
//
// 目标：Claude Messages 客户端（如 launch claude）在上游 chat 通道不可用、
// 但原生 responses 可用时，仍能工作。转换原则：尽可能支持，不支持的字段
// 做 best-effort 降级，绝不因为“不支持”返回 400。
//
// 降级规则：
//   - 未知 block（server_tool_use / web_search_* / mcp_* / code_execution_* 等）
//     序列化为文本保留上下文，不报错。
//   - image/document 无可用 source 时降级为 "[image attached]" /
//     "[document attached]" 文本，不报错。
//   - tool_use 缺 name 时跳过该 block；tool_result 缺 tool_use_id 时降级为
//     user 文本，不报错。
//   - redacted_thinking 无文本等价物，静默丢弃，不报错。
//   - top_k / cache_control / signature / context_management 等无对应物，
//     直接丢弃并记入 request_plan 日志，不报错。

// claudeMessagesToResponsesInput 把 Claude messages + system 转为
// Responses 的 instructions + input 数组。永不返回错误。
func claudeMessagesToResponsesInput(msgs []ClaudeMessage, system any) (string, []any) {
	var instructionParts []string
	if sysText := extractClaudeSystemText(system); sysText != "" {
		instructionParts = append(instructionParts, sysText)
	}
	input := []any{} // 非 nil 空数组，避免上游对 null 的严格校验

	flushText := func(role string, parts []any) {
		if len(parts) == 0 {
			return
		}
		input = append(input, map[string]any{
			"type":    "message",
			"role":    role,
			"content": parts,
		})
	}

	for _, msg := range msgs {
		// system role 消息并入 instructions，不产生 input item。
		if msg.Role == "system" {
			if text := extractClaudeContentText(msg.Content); text != "" {
				instructionParts = append(instructionParts, text)
			}
			continue
		}
		role := msg.Role
		if role != "user" && role != "assistant" && role != "developer" && role != "system" {
			role = "user"
		}
		if role == "developer" {
			// Responses 侧已有 developer->system 归一习惯，这里保留原值，
			// 上游忽略未知 role 时仍有文本可读；不报错。
		}

		switch content := msg.Content.(type) {
		case string:
			if content == "" {
				continue
			}
			input = append(input, map[string]any{
				"type":    "message",
				"role":    role,
				"content": []any{responsesTextPart(role, content)},
			})
		case []any:
			var pending []any
			flushPending := func() {
				if len(pending) > 0 {
					flushText(role, pending)
					pending = nil
				}
			}
			for _, item := range content {
				block, ok := item.(map[string]any)
				if !ok {
					continue
				}
				bt, _ := block["type"].(string)
				switch bt {
				case "text":
					if text, ok := block["text"].(string); ok && text != "" {
						pending = append(pending, responsesTextPart(role, text))
					}
				case "image":
					if role == "assistant" {
						// assistant 消息不允许 input_image，降级为文本标注。
						pending = append(pending, responsesTextPart(role, "[image attached]"))
					} else if part, ok := claudeImageBlockToOpenAI(block); ok {
						// {type:image_url, image_url:{url}} -> Responses input_image
						url := ""
						if m, ok := part["image_url"].(map[string]string); ok {
							url = m["url"]
						} else if m, ok := part["image_url"].(map[string]any); ok {
							url, _ = m["url"].(string)
						}
						if url != "" {
							pending = append(pending, map[string]any{"type": "input_image", "image_url": url})
						} else {
							pending = append(pending, responsesTextPart(role, "[image attached]"))
						}
					} else {
						pending = append(pending, responsesTextPart(role, "[image attached]"))
					}
				case "document":
					if role == "assistant" {
						pending = append(pending, responsesTextPart(role, "[document attached]"))
					} else if part, ok := claudeDocumentBlockToOpenAI(block); ok {
						if fm, ok := part["file"].(map[string]any); ok {
							item := map[string]any{"type": "input_file"}
							for k, v := range fm {
								item[k] = v
							}
							pending = append(pending, item)
						} else {
							pending = append(pending, responsesTextPart(role, "[document attached]"))
						}
					} else {
						pending = append(pending, responsesTextPart(role, "[document attached]"))
					}
				case "thinking":
					flushPending()
					thinking, _ := block["thinking"].(string)
					if thinking == "" {
						continue
					}
					input = append(input, map[string]any{
						"type":    "reasoning",
						"summary": []any{map[string]any{"type": "summary_text", "text": thinking}},
					})
				case "redacted_thinking":
					// 无文本等价物，丢弃，不报错。
					continue
				case "tool_use":
					flushPending()
					id, _ := block["id"].(string)
					name, _ := block["name"].(string)
					if name == "" {
						// 缺 name 无法映射为 function_call，跳过该 block。
						continue
					}
					if id == "" {
						id = "toolu_" + randomString(12)
					}
					args := "{}"
					if rawInput, exists := block["input"]; exists && rawInput != nil {
						if s, ok := rawInput.(string); ok && s != "" {
							args = s
						} else {
							if b, err := json.Marshal(rawInput); err == nil && len(b) > 0 {
								args = string(b)
							}
						}
					}
					input = append(input, map[string]any{
						"type":      "function_call",
						"call_id":   id,
						"name":      name,
						"arguments": args,
					})
				case "tool_result":
					flushPending()
					toolUseID, _ := block["tool_use_id"].(string)
					text := claudeToolResultToText(block)
					if isErr, _ := block["is_error"].(bool); isErr {
						text = applyErrorPrefix(text)
					}
					if toolUseID == "" {
						// 缺 ID 无法配对，降级为普通 user 文本，保留上下文。
						if text == "" {
							continue
						}
						input = append(input, map[string]any{
							"type":    "message",
							"role":    "user",
							"content": []any{map[string]any{"type": "input_text", "text": text}},
						})
						continue
					}
					input = append(input, map[string]any{
						"type":    "function_call_output",
						"call_id": toolUseID,
						"output":  text,
					})
				case "":
					// 空 type：尝试按 role+content 兜底为文本。
					if text := extractClaudeContentText([]any{block}); text != "" {
						pending = append(pending, responsesTextPart(role, text))
					}
				default:
					// 未知 block（含 server_tool_use / web_search_* / mcp_* /
					// code_execution_* / search_result / container_upload 等）：
					// 尝试提取文本，提不出则序列化 JSON 保留上下文，不报错。
					if text := extractClaudeContentText([]any{block}); text != "" {
						pending = append(pending, responsesTextPart(role, text))
						continue
					}
					// 嵌套 content 数组里可能还有文本（如 web_search_tool_result）。
					if c, ok := block["content"]; ok && c != nil {
						if text := joinToolResultContent(c); text != "" {
							pending = append(pending, responsesTextPart(role, text))
							continue
						}
					}
					if b, err := json.Marshal(block); err == nil {
						pending = append(pending, responsesTextPart(role, string(b)))
					}
				}
			}
			flushPending()
		default:
			// 非常规 content 形状：序列化为文本，不报错。
			if content == nil {
				continue
			}
			if b, err := json.Marshal(content); err == nil {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    role,
					"content": []any{responsesTextPart(role, string(b))},
				})
			}
		}
	}

	instructions := strings.Join(instructionParts, "\n\n")
	return instructions, input
}

// claudeToolResultToText 提取 tool_result 的文本，图片/文档附件转为标注。
func claudeToolResultToText(block map[string]any) string {
	switch c := block["content"].(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			pb, ok := p.(map[string]any)
			if !ok {
				if s, ok := p.(string); ok && s != "" {
					parts = append(parts, s)
				}
				continue
			}
			switch pb["type"] {
			case "text":
				if t, ok := pb["text"].(string); ok && t != "" {
					parts = append(parts, t)
				}
			case "image":
				parts = append(parts, "[image attached]")
			case "document":
				parts = append(parts, "[document attached]")
			default:
				// 未知嵌套 block：尝试文本，兜底 JSON。
				if t, ok := pb["text"].(string); ok && t != "" {
					parts = append(parts, t)
				} else if b, err := json.Marshal(pb); err == nil {
					parts = append(parts, string(b))
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		if b, err := json.Marshal(c); err == nil {
			return string(b)
		}
		return ""
	}
}

// isMuseSparkModel 报告是否为 muse-spark 系模型（大小写不敏感子串匹配）。
// 目前仅该系列上游对原生 responses 做严格校验（required 全覆盖、effort 白名单、
// 整数参数拒绝浮点），归一化只对它生效，避免影响其它模型的现有行为。
func isMuseSparkModel(modelID string) bool {
	return strings.Contains(strings.ToLower(modelID), "muse-spark")
}

// responsesTextPart 按 role 返回合法的文本 part 类型：
// user/developer 用 input_text，assistant 用 output_text。
// 上游原生 responses 对 assistant 消息中的 input_text 会 400
//（invalid_request_error: content type `input_text` is not valid on `assistant` messages），
// 因此必须区分。
func responsesTextPart(role, text string) map[string]any {
	if role == "assistant" {
		return map[string]any{"type": "output_text", "text": text}
	}
	return map[string]any{"type": "input_text", "text": text}
}

// claudeToResponsesTools 把 Claude tools 转为 Responses function tools。
// Server tools（无 input_schema）静默跳过，不报错。
func claudeToResponsesTools(claudeTools []ClaudeTool, modelID string) []ResponsesTool {
	if len(claudeTools) == 0 {
		return nil
	}
	tools, _ := claudeToOpenAITools(claudeTools)
	out := make([]ResponsesTool, 0, len(tools))
	for _, t := range tools {
		params := t.Function.Parameters
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if isMuseSparkModel(modelID) {
			params = normalizeResponsesToolParameters(params)
		}
		out = append(out, ResponsesTool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  params,
		})
	}
	return out
}

// normalizeResponsesToolParameters 保证 function parameters 满足上游原生
// responses 的严格校验：required 必须存在且包含 properties 的每一个 key，
// 缺失任一都会 400（Missing 'limit'）。这里做 best-effort 补齐，不报错。
func normalizeResponsesToolParameters(params map[string]any) map[string]any {
	if params == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}}
	}
	if _, ok := params["type"].(string); !ok {
		params["type"] = "object"
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		// properties 非对象时置空，避免上游 400。
		params["properties"] = map[string]any{}
		params["required"] = []any{}
		return params
	}
	required := make([]any, 0, len(props))
	for k := range props {
		required = append(required, k)
	}
	// 排序保证确定性输出，便于测试与缓存。
	for i := 0; i < len(required); i++ {
		for j := i + 1; j < len(required); j++ {
			if required[j].(string) < required[i].(string) {
				required[i], required[j] = required[j], required[i]
			}
		}
	}
	params["required"] = required
	// 透传 additionalProperties 等约束键，不做改动。
	return params
}

// claudeToolChoiceToResponses 把 Claude tool_choice 转为 Responses 形状。
// 未知形状原样透传，不报错。
func claudeToolChoiceToResponses(choice any) any {
	m, ok := choice.(map[string]any)
	if !ok {
		return choice
	}
	switch m["type"] {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if name, ok := m["name"].(string); ok && name != "" {
			return map[string]any{"type": "function", "name": name}
		}
		return "auto"
	}
	return choice
}

// claudeThinkingToResponsesEffort 从 thinking / output_config 推导 effort。
// 禁用或推导不出时返回 ""（调用方省略 reasoning 字段，不报错）。
// 上游原生 responses 对 effort 白名单校验（minimal/low/medium/high/xhigh），
// 不支持 max/none 等，非法值一律归一化或省略，绝不 400。
func claudeThinkingToResponsesEffort(claudeReq ClaudeRequest, modelID string) string {
	if getForceDisableThinking() || isThinkingDisabled(claudeReq.Thinking) {
		return ""
	}
	var effort string
	if e := effortFromOutputConfig(claudeReq.OutputConfig); e != "" {
		effort = e
	} else {
		effort = reasoningEffortFromThinking(claudeReq.Thinking)
	}
	if !isMuseSparkModel(modelID) {
		return effort
	}
	return normalizeResponsesEffort(effort)
}

// normalizeResponsesEffort 把 effort 归一化到上游白名单。
// max->xhigh（最接近的高档），none/"" /未知->""（省略 reasoning 字段）。
func normalizeResponsesEffort(effort string) string {
	effort = strings.TrimSpace(effort)
	switch effort {
	case "":
		return ""
	case "none":
		return ""
	case "minimal", "low", "medium", "high", "xhigh":
		return effort
	case "max":
		return "xhigh"
	default:
		return ""
	}
}

// claudeToResponsesBody 把 Claude 请求转为原生 Responses 请求体。永不报错，
// 失败时返回最小可用体（model + input），避免 400。
func claudeToResponsesBody(claudeReq ClaudeRequest, modelID string) []byte {
	instructions, input := claudeMessagesToResponsesInput(claudeReq.Messages, claudeReq.System)
	body := map[string]any{
		"model":  modelID,
		"input":  input,
		"stream": claudeReq.Stream,
	}
	if instructions != "" {
		body["instructions"] = instructions
	}
	if claudeReq.Stream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if claudeReq.Temperature != nil {
		body["temperature"] = *claudeReq.Temperature
	}
	if claudeReq.MaxTokens != nil {
		v := *claudeReq.MaxTokens
		if cap := getMaxTokensCapForModel(modelID); cap > 0 && v > cap {
			v = cap
		}
		body["max_output_tokens"] = v
	}
	if claudeReq.TopP != nil {
		body["top_p"] = *claudeReq.TopP
	}
	if tools := claudeToResponsesTools(claudeReq.Tools, modelID); len(tools) > 0 {
		body["tools"] = tools
	}
	if claudeReq.ToolChoice != nil {
		body["tool_choice"] = claudeToolChoiceToResponses(claudeReq.ToolChoice)
	}
	if effort := claudeThinkingToResponsesEffort(claudeReq, modelID); effort != "" && effort != "none" {
		body["reasoning"] = map[string]any{"effort": effort}
	}
	if user := narrowClaudeMetadataUser(claudeReq.Metadata); user != "" {
		body["user"] = user
	}
	// metadata best-effort 透传 map 形状，非 map 则丢弃，不报错。
	if claudeReq.Metadata != nil {
		if m, ok := claudeReq.Metadata.(map[string]any); ok {
			body["metadata"] = m
		}
	}
	if len(claudeReq.StopSequences) > 0 {
		body["stop"] = append([]string(nil), claudeReq.StopSequences...)
	}
	// top_k / cache_control / signature / context_management / betas 无对应物，丢弃。
	b, err := json.Marshal(body)
	if err != nil {
		fallback, _ := json.Marshal(map[string]any{"model": modelID, "input": input})
		return fallback
	}
	return b
}

// ======================== Responses -> Claude 转换（lenient） ========================

// responsesOutputToClaudeBlocks 把原生 Responses output 数组转为 Claude
// content blocks。未知 item 类型降级为文本，不报错。
func responsesOutputToClaudeBlocks(output []any, wantReasoning bool) ([]ClaudeContent, string, bool) {
	content := []ClaudeContent{}
	stopReason := "end_turn"
	hasToolUse := false
	var reasoningTexts []string
	var textParts []string
	var refusalText string

	// 先收集 reasoning / refusal / text / tool_use，保序输出：
	// thinking -> text -> tool_use（与 openAIToClaudeResponse 的 fallback 顺序一致）。
	type toolUseItem struct {
		id    string
		name  string
		input any
	}
	var tools []toolUseItem

	for _, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := item["type"].(string)
		switch typ {
		case "reasoning":
			var texts []string
			if summary, ok := item["summary"].([]any); ok {
				for _, s := range summary {
					if sm, ok := s.(map[string]any); ok {
						if t, ok := sm["text"].(string); ok && t != "" {
							texts = append(texts, t)
						}
					}
				}
			}
			// 兼容 summary 为字符串的非标准形态。
			if len(texts) == 0 {
				if s, ok := item["summary"].(string); ok && s != "" {
					texts = append(texts, s)
				}
			}
			if len(texts) > 0 {
				reasoningTexts = append(reasoningTexts, texts...)
			}
		case "message":
			c, _ := item["content"].([]any)
			for _, rc := range c {
				cm, ok := rc.(map[string]any)
				if !ok {
					continue
				}
				ct, _ := cm["type"].(string)
				switch ct {
				case "output_text", "input_text", "text":
					if t, ok := cm["text"].(string); ok && t != "" {
						textParts = append(textParts, t)
					} else if t, ok := cm["output_text"].(string); ok && t != "" {
						textParts = append(textParts, t)
					}
				case "refusal":
					if t, ok := cm["refusal"].(string); ok && t != "" {
						refusalText = t
						if refusalText != "" {
							textParts = append(textParts, refusalText)
						}
					}
				default:
					// annotations 等未知 content：尝试文本兜底。
					if t, ok := cm["text"].(string); ok && t != "" {
						textParts = append(textParts, t)
					}
				}
			}
		case "function_call":
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID, _ = item["id"].(string)
			}
			name, _ := item["name"].(string)
			if name == "" {
				continue
			}
			argsStr, _ := item["arguments"].(string)
			var input any
			if argsStr != "" {
				if err := json.Unmarshal([]byte(argsStr), &input); err != nil {
					input = map[string]any{"_raw": argsStr}
				}
			}
			if input == nil {
				input = map[string]any{}
			}
			tools = append(tools, toolUseItem{id: callID, name: name, input: input})
			hasToolUse = true
		case "apply_patch_call", "shell_call":
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID, _ = item["id"].(string)
			}
			name := "apply_patch"
			if typ == "shell_call" {
				name = "shell"
			}
			input := map[string]any{}
			for k, v := range item {
				switch k {
				case "id", "type", "status", "call_id":
					continue
				default:
					input[k] = v
				}
			}
			// 兼容 arguments 字符串形态。
			if argsStr, ok := item["arguments"].(string); ok && argsStr != "" {
				var parsed any
				if err := json.Unmarshal([]byte(argsStr), &parsed); err == nil {
					if m, ok := parsed.(map[string]any); ok {
						input = m
					} else {
						input = map[string]any{"input": parsed}
					}
				} else {
					input = map[string]any{"input": argsStr}
				}
			}
			tools = append(tools, toolUseItem{id: callID, name: name, input: input})
			hasToolUse = true
		case "function_call_output", "apply_patch_call_output", "shell_call_output", "tool_result":
			// 输入回显，不应出现在 assistant 输出，忽略，不报错。
			continue
		default:
			// 未知 output item（含 web_search_call / file_search_call /
			// computer_call / code_interpreter / image_generation / mcp 等）：
			// 尝试提取文本，提不出则序列化 JSON 为文本，不报错。
			if typ == "" {
				continue
			}
			if t := extractTextFromContentParts(item["content"]); t != "" {
				textParts = append(textParts, t)
				continue
			}
			if b, err := json.Marshal(item); err == nil {
				textParts = append(textParts, string(b))
			}
		}
	}

	if wantReasoning {
		for _, t := range reasoningTexts {
			content = append(content, ClaudeContent{Type: "thinking", Thinking: t})
		}
	}
	// wantReasoning==false 时 reasoning 直接丢弃（由调用方在空回复时 promote，
	// 与 openAIToClaudeResponse 的 keep 语义一致）；这里不提前 promote，
	// 统一在下方空回复保护中处理。

	joinedText := strings.Join(textParts, "\n")
	if joinedText == "" && len(reasoningTexts) > 0 && len(tools) == 0 {
		// 空回复保护：Go 网关常把正文放在 reasoning 里（#37635），提升为文本。
		joinedText = strings.Join(reasoningTexts, "\n")
	}
	if joinedText != "" {
		content = append(content, ClaudeContent{Type: "text", Text: joinedText})
	}
	for _, tl := range tools {
		content = append(content, ClaudeContent{Type: "tool_use", ID: tl.id, Name: tl.name, Input: tl.input})
	}
	if len(content) == 0 {
		content = append(content, ClaudeContent{Type: "text", Text: ""})
	}

	if refusalText != "" && !hasToolUse {
		stopReason = "refusal"
	} else if hasToolUse {
		stopReason = "tool_use"
	}
	return content, stopReason, hasToolUse
}

// convertResponsesToClaude 把原生 Responses 成功响应转为 Claude message。
// 解析失败时返回最小可用空文本消息，不报错。
func convertResponsesToClaude(respBody []byte, model string, wantReasoning bool) []byte {
	var resp struct {
		ID     string `json:"id"`
		Output []any  `json:"output"`
		Status string `json:"status"`
		Usage  map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		slog.Warn("convertResponsesToClaude unmarshal failed", "error", err)
		resp.Output = nil
	}
	content, stopReason, _ := responsesOutputToClaudeBlocks(resp.Output, wantReasoning)
	// incomplete/max_output_tokens 映射。
	if resp.Status == "incomplete" {
		stopReason = "max_tokens"
	}
	claudeResp := ClaudeResponse{
		ID:           normalizeClaudeMessageID(resp.ID),
		Type:         "message",
		Role:         "assistant",
		Content:      content,
		Model:        model,
		StopReason:   stopReason,
		StopSequence: nil,
	}
	if resp.Usage != nil {
		claudeResp.Usage = buildClaudeMessageUsage(responsesUsageToChat(resp.Usage))
	}
	result, _ := json.Marshal(claudeResp)
	return result
}

// responsesUsageToChat 把 Responses usage 口径转为 chat 口径，复用
// buildClaudeMessageUsage 的缓存解析逻辑。
func responsesUsageToChat(usage map[string]any) map[string]any {
	if usage == nil {
		return nil
	}
	out := map[string]any{}
	if v, ok := usage["input_tokens"]; ok {
		out["prompt_tokens"] = v
	} else if v, ok := usage["prompt_tokens"]; ok {
		out["prompt_tokens"] = v
	}
	if v, ok := usage["output_tokens"]; ok {
		out["completion_tokens"] = v
	} else if v, ok := usage["completion_tokens"]; ok {
		out["completion_tokens"] = v
	}
	if v, ok := usage["total_tokens"]; ok {
		if pt, ok := numberAsFloat(out["prompt_tokens"]); ok {
			if ct, ok := numberAsFloat(out["completion_tokens"]); ok {
				_ = pt
				_ = ct
			}
		}
		out["total_tokens"] = v
	}
	// 透传缓存与细节字段，buildClaudeUsageCore 会识别标准键。
	for _, k := range []string{"prompt_tokens_details", "completion_tokens_details", "input_tokens_details", "output_tokens_details", "cache_creation_input_tokens", "cache_read_input_tokens", "prompt_cache_hit_tokens", "server_tool_use", "service_tier"} {
		if v, ok := usage[k]; ok {
			out[k] = v
		}
	}
	return out
}

// convertResponsesErrorToClaude 把原生 Responses 错误体转为 Claude 错误形状。
// 上游 message 尽量保留，不暴露内部错误串。
func convertResponsesErrorToClaude(respBody []byte) []byte {
	message := "upstream error"
	errType := "api_error"
	var raw map[string]any
	if json.Unmarshal(respBody, &raw) == nil {
		if em, ok := raw["error"].(map[string]any); ok {
			if m, ok := em["message"].(string); ok && m != "" {
				message = m
			}
			if t, ok := em["type"].(string); ok && t != "" {
				errType = t
			}
		} else if m, ok := raw["message"].(string); ok && m != "" {
			message = m
		}
	}
	b, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": message},
	})
	return b
}

// recordClaudeResponsesUsage 按 Responses usage 口径记录 token 与缓存统计。
func recordClaudeResponsesUsage(model string, usage map[string]any) {
	if usage == nil {
		return
	}
	var pt, ct, tt int64
	if v, ok := numberAsFloat(usage["input_tokens"]); ok {
		pt = int64(v)
	} else if v, ok := numberAsFloat(usage["prompt_tokens"]); ok {
		pt = int64(v)
	}
	if v, ok := numberAsFloat(usage["output_tokens"]); ok {
		ct = int64(v)
	} else if v, ok := numberAsFloat(usage["completion_tokens"]); ok {
		ct = int64(v)
	}
	if v, ok := numberAsFloat(usage["total_tokens"]); ok {
		tt = int64(v)
	}
	if tt <= 0 && (pt > 0 || ct > 0) {
		tt = pt + ct
	}
	if tt > 0 {
		recordTokenUsage(model, pt, ct, tt)
		recordCacheUsage(model, responsesUsageToChat(usage))
	}
}

// ======================== Claude 经原生 responses 的转发/探测 ========================

// probeClaudeViaResponses 专用于 chat 翻译路径失败后的投机探测：仅上游原生
// responses 返回 2xx 时才转换写回并记住该模型；任何失败都返回 false 且不写
// 任何响应，调用方保留原翻译路径错误原样返回。
func probeClaudeViaResponses(ctx context.Context, w http.ResponseWriter, auth UpstreamAuth, modelID string, claudeReq ClaudeRequest, stream bool, wantReasoning bool) bool {
	upstreamBody := claudeToResponsesBody(claudeReq, modelID)
	rc, status, _, err := callOpenCodeEndpoint(ctx, "responses", upstreamBody, modelID, auth)
	if err != nil || status < 200 || status >= 300 {
		if rc != nil {
			rc.Close()
		}
		return false
	}
	defer rc.Close()

	rememberNativeResponsesModel(modelID)
	reqLogger(ctx).Info("claude_responses_probe_succeeded", "model", modelID, "stream", stream)

	if stream {
		claudeResponsesStreamHandler(ctx, w, rc, modelID, wantReasoning)
		return true
	}
	respBody, readErr := io.ReadAll(io.LimitReader(rc, 32*1024*1024))
	if readErr != nil {
		return false
	}
	claudeBody := convertResponsesToClaude(respBody, modelID, wantReasoning)
	result := summarizeClaudeResult(claudeBody)
	logRequestResult(ctx, result)
	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			recordClaudeResponsesUsage(modelID, u)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	maybeLogBodySummary(ctx, "claude responses response body", claudeBody)
	_, _ = w.Write(claudeBody)
	return true
}

// forwardClaudeViaResponses 处理已确认原生模型的转发：上游 2xx 转换成功后
// 写回；上游 4xx/5xx 转为 Claude 错误形状保真透传状态码；仅传输层错误
// （拿不到上游响应）返回 false，调用方兜底。
func forwardClaudeViaResponses(ctx context.Context, w http.ResponseWriter, auth UpstreamAuth, modelID string, claudeReq ClaudeRequest, stream bool, wantReasoning bool) bool {
	upstreamBody := claudeToResponsesBody(claudeReq, modelID)
	rc, status, _, err := callOpenCodeEndpoint(ctx, "responses", upstreamBody, modelID, auth)
	if err != nil {
		markNativeResponsesFailure(modelID)
		if rc != nil {
			rc.Close()
		}
		return false
	}
	defer rc.Close()

	if status >= 500 {
		markNativeResponsesFailure(modelID)
	}

	if stream && status >= 200 && status < 300 {
		claudeResponsesStreamHandler(ctx, w, rc, modelID, wantReasoning)
		return true
	}

	respBody, readErr := io.ReadAll(io.LimitReader(rc, 32*1024*1024))
	if readErr != nil {
		markNativeResponsesFailure(modelID)
		return false
	}

	if status >= 200 && status < 300 {
		claudeBody := convertResponsesToClaude(respBody, modelID, wantReasoning)
		result := summarizeClaudeResult(claudeBody)
		logRequestResult(ctx, result)
		var usageResp map[string]any
		if json.Unmarshal(respBody, &usageResp) == nil {
			if u, ok := usageResp["usage"].(map[string]any); ok {
				recordClaudeResponsesUsage(modelID, u)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		maybeLogBodySummary(ctx, "claude responses response body", claudeBody)
		_, _ = w.Write(claudeBody)
		return true
	}

	// 上游错误：转为 Claude 错误形状，状态码保真透传。
	claudeErr := convertResponsesErrorToClaude(respBody)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(claudeErr)
	return true
}

// ======================== Claude 经原生 responses 的流式转换 ========================

type claudeResponsesBlock struct {
	claudeIndex int
	kind        string // text | thinking | tool
	open        bool
	toolID      string
	toolName    string
}

func claudeResponsesStreamHandler(ctx context.Context, w http.ResponseWriter, rc io.Reader, model string, wantReasoning bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(rc)
	stats := &streamResultStats{start: time.Now()}

	msgID := fmt.Sprintf("msg_%s", randomString(24))
	blockIndex := 0
	messageStartSent := false
	finished := false
	stopReason := "end_turn"
	fullUsage := map[string]any{}
	reasoningFallback := strings.Builder{}
	producedText := false

	type unusedBlockInfo struct {
		claudeIndex int
		kind        string // text | thinking | tool
		open        bool
		toolID      string
		toolName    string
	}
	// output_index -> block（文本/推理按 output_index 聚合；工具同样）。
	blocks := map[int]*claudeResponsesBlock{}
	// item_id -> output_index，便于 delta 事件回查。
	itemToOutput := map[string]int{}
	toolOrder := []int{}
	fullTextLen := 0
	fullReasoningLen := 0

	emitEvent := func(event string, data any) {
		b, err := json.Marshal(data)
		if err != nil {
			return
		}
		_, _ = w.Write([]byte("event: " + event + "\n"))
		_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	emitError := func(msg string) {
		emitEvent("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": msg},
		})
	}
	ensureStart := func() {
		if messageStartSent {
			return
		}
		messageStartSent = true
		emitEvent("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": msgID, "type": "message", "role": "assistant",
				"content": []any{}, "model": model,
				"stop_reason": nil, "stop_sequence": nil,
				"usage": buildClaudeMessageUsage(fullUsage),
			},
		})
		emitEvent("ping", map[string]any{"type": "ping"})
	}
	getOrCreateBlock := func(outputIndex int, kind string) *claudeResponsesBlock {
		if b, ok := blocks[outputIndex]; ok {
			if b.kind == kind {
				if b.claudeIndex < 0 {
					b.claudeIndex = blockIndex
					blockIndex++
				}
				return b
			}
		}
		// 同一 output_index 上类型切换（如 message->tool）时沿用新 kind。
		b := &claudeResponsesBlock{claudeIndex: blockIndex, kind: kind}
		blocks[outputIndex] = b
		blockIndex++
		return b
	}
	startTextBlock := func(b *claudeResponsesBlock) {
		if b.open {
			return
		}
		b.open = true
		ensureStart()
		emitEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": b.claudeIndex,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
	}
	startThinkingBlock := func(b *claudeResponsesBlock) {
		if b.open {
			return
		}
		b.open = true
		ensureStart()
		emitEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": b.claudeIndex,
			"content_block": map[string]any{"type": "thinking", "thinking": ""},
		})
	}
	startToolBlock := func(b *claudeResponsesBlock) {
		if b.open {
			return
		}
		b.open = true
		ensureStart()
		if b.toolID == "" {
			b.toolID = "toolu_" + randomString(12)
		}
		emitEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": b.claudeIndex,
			"content_block": map[string]any{
				"type": "tool_use", "id": b.toolID, "name": b.toolName, "input": map[string]any{},
			},
		})
		if _, exists := indexOfToolOrder(toolOrder, b.claudeIndex); !exists {
			toolOrder = append(toolOrder, b.claudeIndex)
		}
	}
	_ = startToolBlock
	emitTextDelta := func(b *claudeResponsesBlock, text string) {
		if text == "" {
			return
		}
		stats.textChars += len(text)
		fullTextLen += len(text)
		producedText = true
		startTextBlock(b)
		emitEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": b.claudeIndex,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
	}
	emitThinkingDelta := func(b *claudeResponsesBlock, text string) {
		if text == "" {
			return
		}
		stats.reasoningChars += len(text)
		fullReasoningLen += len(text)
		if wantReasoning {
			reasoningFallback.WriteString(text)
			startThinkingBlock(b)
			emitEvent("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": b.claudeIndex,
				"delta": map[string]any{"type": "thinking_delta", "thinking": text},
			})
		} else {
			stats.promotedReasoning = true
			emitTextDelta(b, text)
		}
	}
	emitToolDelta := func(b *claudeResponsesBlock, partial string) {
		if partial == "" {
			return
		}
		startToolBlock(b)
		emitEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": b.claudeIndex,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": partial},
		})
	}

	defer func() {
		stats.toolCallCount = len(toolOrder)
		stats.log(ctx, "claude-responses")
		if len(fullUsage) > 0 {
			pt, ct, tt := usageFromResponsesMap(fullUsage)
			if tt > 0 {
				recordTokenUsage(model, pt, ct, tt)
				recordCacheUsage(model, responsesUsageToChat(fullUsage))
			}
		}
	}()

	// SSE 解析：按空行分帧，聚合 event: + data: 行。
	var frameEvent string
	var frameData []string
	finalized := false
	doFinalize := func() {
		if finalized {
			return
		}
		finalized = true
		finalizeClaudeResponsesStream(emitEvent, blocks, toolOrder, msgID, model, fullUsage, stopReason, reasoningFallback.String(), producedText)
	}
	flushFrame := func() {
		defer func() {
			frameEvent = ""
			frameData = nil
		}()
		if len(frameData) == 0 {
			return
		}
		for _, dataLine := range frameData {
			trimmed := strings.TrimSpace(dataLine)
			if trimmed == "[DONE]" {
				stats.doneSeen = true
				if !finalized && (producedText || len(toolOrder) > 0) {
					finished = true
					doFinalize()
				}
				return
			}
			if !strings.HasPrefix(trimmed, "{") {
				continue
			}
			var evt map[string]any
			if err := json.Unmarshal([]byte(trimmed), &evt); err != nil {
				continue
			}
			handleClaudeResponsesStreamEvent(evt, frameEvent, &messageStartSent, &finished, &stopReason, fullUsage, itemToOutput, blocks, getOrCreateBlock, ensureStart, emitTextDelta, emitThinkingDelta, emitToolDelta, emitEvent, emitError, stats, &producedText)
			if finished {
				doFinalize()
				return
			}
		}
	}

	// keepalive：首 token 前客户端仅能收到 ping。
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	type readResult struct {
		line string
		err  error
	}
	readCh := make(chan readResult)
	readerDone := make(chan struct{})
	readerExited := make(chan struct{})
	go func() {
		defer close(readerExited)
		for {
			line, err := reader.ReadString('\n')
			select {
			case readCh <- readResult{line: line, err: err}:
			case <-readerDone:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		close(readerDone)
		if c, ok := rc.(io.Closer); ok {
			c.Close()
		}
		<-readerExited
	}()

loop:
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			emitEvent("ping", map[string]any{"type": "ping"})
		case res := <-readCh:
			line := res.line
			trimmedRight := strings.TrimRight(line, "\r\n")
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				flushFrame()
				if finished {
					break loop
				}
			} else if strings.HasPrefix(trimmed, ":") {
				// SSE 注释，心跳，忽略。
				continue
			} else if strings.HasPrefix(trimmed, "event:") {
				frameEvent = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			} else if strings.HasPrefix(trimmed, "data:") {
				frameData = append(frameData, strings.TrimSpace(strings.TrimPrefix(trimmedRight, "data:")))
				// 非标准单行 data（无空行分隔）也尝试及时处理：仅当该行自身是完整 JSON
				// 且下一行未知时，flush 会在空行到达时执行；这里不提前 flush，
				// 避免半帧误解析。
			} else if strings.HasPrefix(trimmed, "{") {
				// 非标准裸 JSON 行。
				frameData = append(frameData, trimmed)
			}
			if res.err != nil {
				// EOF 前可能还有未以空行结尾的最后一帧，补 flush（completed 的
				// doFinalize 已在 flushFrame 内执行，此处不再重复）。
				if len(frameData) > 0 {
					flushFrame()
				}
				if !finalized {
					if producedText || len(toolOrder) > 0 {
						// 上游干净 EOF 但缺 completed（如 muse-spark 系只发事件不发 DONE）：
						// 合成正常结束，不报错。
						stats.sawFinish = true
						stats.finishReason = "stop"
						finished = true
						doFinalize()
					} else {
						emitError("stream ended without completion")
					}
				}
				break loop
			}
			if finished && len(frameData) == 0 {
				// completed 已处理，等待 EOF/DONE 后退出；继续消费避免 goroutine 泄漏。
				continue
			}
		}
	}
	_ = fullReasoningLen
	_ = fullTextLen
	_ = bytes.MinRead
}

func indexOfToolOrder(order []int, v int) (int, bool) {
	for i, x := range order {
		if x == v {
			return i, true
		}
	}
	return -1, false
}

func usageFromResponsesMap(usage map[string]any) (int64, int64, int64) {
	var pt, ct, tt int64
	if v, ok := numberAsFloat(usage["input_tokens"]); ok {
		pt = int64(v)
	} else if v, ok := numberAsFloat(usage["prompt_tokens"]); ok {
		pt = int64(v)
	}
	if v, ok := numberAsFloat(usage["output_tokens"]); ok {
		ct = int64(v)
	} else if v, ok := numberAsFloat(usage["completion_tokens"]); ok {
		ct = int64(v)
	}
	if v, ok := numberAsFloat(usage["total_tokens"]); ok {
		tt = int64(v)
	}
	if tt <= 0 && (pt > 0 || ct > 0) {
		tt = pt + ct
	}
	return pt, ct, tt
}

// handleClaudeResponsesStreamEvent 翻译单个 Responses SSE 事件为 Claude 事件。
func handleClaudeResponsesStreamEvent(evt map[string]any, frameEvent string, messageStartSent *bool, finished *bool, stopReason *string, fullUsage map[string]any, itemToOutput map[string]int, blocks map[int]*claudeResponsesBlock, getOrCreate func(int, string) *claudeResponsesBlock, ensureStart func(), emitText func(*claudeResponsesBlock, string), emitThinking func(*claudeResponsesBlock, string), emitTool func(*claudeResponsesBlock, string), emitEvent func(string, any), emitError func(string), stats *streamResultStats, producedText *bool) {
	typ, _ := evt["type"].(string)
	if typ == "" {
		typ = frameEvent
	}
	outputIndex := -1
	if v, ok := evt["output_index"].(float64); ok {
		outputIndex = int(v)
	}
	itemID, _ := evt["item_id"].(string)

	outputIndexForItem := func(id string, fallback int) int {
		if id == "" {
			return fallback
		}
		if oi, ok := itemToOutput[id]; ok {
			return oi
		}
		return fallback
	}

	switch typ {
	case "response.created", "response.in_progress", "response.queued":
		if resp, ok := evt["response"].(map[string]any); ok {
			if u, ok := resp["usage"].(map[string]any); ok {
				for k, v := range u {
					fullUsage[k] = v
				}
			}
		}
		ensureStart()
	case "response.output_item.added":
		item, _ := evt["item"].(map[string]any)
		if item == nil {
			return
		}
		itemType, _ := item["type"].(string)
		id, _ := item["id"].(string)
		if outputIndex >= 0 && id != "" {
			itemToOutput[id] = outputIndex
		}
		switch itemType {
		case "message":
			// content_part 后续会创建文本块，这里仅登记映射。
			if outputIndex >= 0 {
				if _, ok := blocks[outputIndex]; !ok {
					blocks[outputIndex] = &claudeResponsesBlock{claudeIndex: -1, kind: "text"}
				}
			}
		case "reasoning":
			if outputIndex >= 0 {
				b := getOrCreate(outputIndex, "thinking")
				_ = b
			}
		case "function_call", "tool_call":
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID, _ = item["id"].(string)
			}
			name, _ := item["name"].(string)
			if outputIndex >= 0 {
				b := getOrCreate(outputIndex, "tool")
				if b.claudeIndex < 0 {
					// getOrCreate 已分配，这里补齐（兼容占位）。
				}
				if callID != "" {
					b.toolID = callID
				}
				if name != "" {
					b.toolName = name
				}
				if id != "" {
					itemToOutput[id] = outputIndex
				}
				if callID != "" {
					itemToOutput[callID] = outputIndex
				}
			}
		case "apply_patch_call", "shell_call":
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID, _ = item["id"].(string)
			}
			name := "apply_patch"
			if itemType == "shell_call" {
				name = "shell"
			}
			if outputIndex >= 0 {
				b := getOrCreate(outputIndex, "tool")
				if callID != "" {
					b.toolID = callID
				}
				b.toolName = name
				if id != "" {
					itemToOutput[id] = outputIndex
				}
			}
		default:
			// 未知 item（web_search_call 等）：登记为文本占位，delta 到达时降级。
			if outputIndex >= 0 && itemType != "" {
				if _, ok := blocks[outputIndex]; !ok {
					blocks[outputIndex] = &claudeResponsesBlock{claudeIndex: -1, kind: "text"}
				}
			}
		}
	case "response.content_part.added":
		oi := outputIndex
		if oi < 0 {
			oi = outputIndexForItem(itemID, -1)
		}
		if oi < 0 {
			return
		}
		part, _ := evt["part"].(map[string]any)
		partType := ""
		if part != nil {
			partType, _ = part["type"].(string)
		}
		// refusal 也按文本处理。
		b := getOrCreate(oi, "text")
		if b.claudeIndex < 0 {
			// 占位块首次使用时分配真实序号（getOrCreate 已分配，这里无需处理）。
		}
		_ = partType
	case "response.output_text.delta":
		oi := outputIndex
		if oi < 0 {
			oi = outputIndexForItem(itemID, -1)
		}
		if oi < 0 {
			oi = 0
		}
		delta, _ := evt["delta"].(string)
		if delta == "" {
			return
		}
		stats.noteChunk()
		*producedText = true
		b := getOrCreate(oi, "text")
		emitText(b, delta)
	case "response.refusal.delta":
		oi := outputIndex
		if oi < 0 {
			oi = outputIndexForItem(itemID, 0)
		}
		delta, _ := evt["delta"].(string)
		if delta == "" {
			if r, ok := evt["refusal"].(string); ok {
				delta = r
			}
		}
		if delta == "" {
			return
		}
		stats.noteChunk()
		*producedText = true
		b := getOrCreate(oi, "text")
		emitText(b, delta)
	case "response.function_call_arguments.delta":
		oi := outputIndex
		if oi < 0 {
			oi = outputIndexForItem(itemID, -1)
		}
		if oi < 0 {
			return
		}
		delta, _ := evt["delta"].(string)
		if delta == "" {
			return
		}
		stats.noteChunk()
		b := getOrCreate(oi, "tool")
		emitTool(b, delta)
	case "response.reasoning_summary_text.delta":
		oi := outputIndex
		if oi < 0 {
			oi = outputIndexForItem(itemID, 0)
		}
		delta, _ := evt["delta"].(string)
		if delta == "" {
			return
		}
		stats.noteChunk()
		b := getOrCreate(oi, "thinking")
		emitThinking(b, delta)
	case "response.reasoning_text.delta":
		oi := outputIndex
		if oi < 0 {
			oi = outputIndexForItem(itemID, 0)
		}
		delta, _ := evt["delta"].(string)
		if delta == "" {
			if t, ok := evt["text"].(string); ok {
				delta = t
			}
		}
		if delta == "" {
			return
		}
		stats.noteChunk()
		b := getOrCreate(oi, "thinking")
		emitThinking(b, delta)
	case "response.output_text.done", "response.refusal.done", "response.function_call_arguments.done", "response.reasoning_summary_part.done", "response.reasoning_summary_text.done", "response.content_part.done", "response.output_item.done":
		// 结束标记：统一在 completed 处关块，避免半流提前关块后同 index 又来 delta。
		return
	case "response.completed":
		resp, _ := evt["response"].(map[string]any)
		if resp != nil {
			if u, ok := resp["usage"].(map[string]any); ok {
				for k, v := range u {
					fullUsage[k] = v
				}
			}
			if status, ok := resp["status"].(string); ok && status == "incomplete" {
				*stopReason = "max_tokens"
			}
			// 从完整 output 推导 tool_use 终止（流式 delta 可能漏 name）。
			if out, ok := resp["output"].([]any); ok {
				for _, raw := range out {
					if im, ok := raw.(map[string]any); ok {
						if t, _ := im["type"].(string); t == "function_call" || t == "apply_patch_call" || t == "shell_call" || t == "tool_call" {
							*stopReason = "tool_use"
							break
						}
					}
				}
			}
		} else if u, ok := evt["usage"].(map[string]any); ok {
			for k, v := range u {
				fullUsage[k] = v
			}
		}
		stats.sawFinish = true
		stats.finishReason = "stop"
		stats.doneSeen = true
		*finished = true
	case "response.incomplete":
		resp, _ := evt["response"].(map[string]any)
		if resp != nil {
			if u, ok := resp["usage"].(map[string]any); ok {
				for k, v := range u {
					fullUsage[k] = v
				}
			}
		}
		*stopReason = "max_tokens"
		stats.sawFinish = true
		stats.finishReason = "length"
		stats.doneSeen = true
		*finished = true
	case "response.failed", "error":
		msg := "upstream stream error"
		if em, ok := evt["error"].(map[string]any); ok {
			if m, ok := em["message"].(string); ok && m != "" {
				msg = m
			}
		} else if em, ok := evt["response"].(map[string]any); ok {
			if e2, ok := em["error"].(map[string]any); ok {
				if m, ok := e2["message"].(string); ok && m != "" {
					msg = m
				}
			}
		}
		emitError(msg)
		*finished = true
	default:
		// 顶层 usage 事件（部分上游直接发 usage）。
		if u, ok := evt["usage"].(map[string]any); ok {
			for k, v := range u {
				fullUsage[k] = v
			}
			return
		}
		// 未知事件：尝试通用 delta 字段降级为文本，不报错。
		if d, ok := evt["delta"].(string); ok && d != "" {
			oi := outputIndex
			if oi < 0 {
				oi = outputIndexForItem(itemID, 0)
			}
			stats.noteChunk()
			*producedText = true
			b := getOrCreate(oi, "text")
			emitText(b, d)
		}
	}
}

// finalizeClaudeResponsesStream 关闭所有已开块并发送 message_delta/stop。
func finalizeClaudeResponsesStream(emit func(string, any), blocks map[int]*claudeResponsesBlock, toolOrder []int, msgID, model string, fullUsage map[string]any, stopReason, reasoningFallback string, producedText bool) {
	// 空回复保护：有 reasoning 但无文本/tool 时提升为文本。
	if !producedText && len(toolOrder) == 0 && reasoningFallback != "" {
		b := &claudeResponsesBlock{claudeIndex: 9999, kind: "text", open: false}
		// 复用 emit 路径：直接发送一个文本块。
		emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": b.claudeIndex,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": b.claudeIndex,
			"delta": map[string]any{"type": "text_delta", "text": reasoningFallback},
		})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": b.claudeIndex})
	}
	// 按 claudeIndex 排序关块，保证序号单调。
	type kv struct {
		oi int
		b  *claudeResponsesBlock
	}
	var ordered []kv
	for oi, b := range blocks {
		if b.open && b.claudeIndex >= 0 {
			ordered = append(ordered, kv{oi, b})
		}
	}
	// 简单冒泡按 index 排序（块数极少）。
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if ordered[j].b.claudeIndex < ordered[i].b.claudeIndex {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}
	}
	for _, e := range ordered {
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": e.b.claudeIndex})
	}
	// 占位块（claudeIndex==-1，从未真正开块）无需关闭。
	emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": buildClaudeDeltaUsage(responsesUsageToChat(fullUsage)),
	})
	emit("message_stop", map[string]any{"type": "message_stop"})
	_ = msgID
	_ = model
}

var _ = slog.Info
var _ = fmt.Sprintf
