package bridge

import (
	"encoding/json"
	"strings"
)

// This file holds the pure Claude Messages -> Responses request-body
// construction moved out of internal/app/claude_responses.go. None of these
// functions perform I/O, logging, metrics, or config reads; the
// force-disable-thinking flag and max_tokens cap arrive via ConfigView, and the
// tool-use-ID generator is injected (production: internal/random string). The
// app layer keeps same-name lowercase forwarding shells so existing *_test.go
// files compile unchanged.

// IsAnthropicBillingHeader 报告 Claude Code 注入的计费头块（对齐 sub2api
// isAnthropicBillingHeaderText）：该块只对 Anthropic 计费链路有意义，转发到
// OpenAI Responses 上游只会浪费上下文，system/instructions 组装时滤掉。
// (Moved from app/claude_responses.go isAnthropicBillingHeader.)
func IsAnthropicBillingHeader(text string) bool {
	return strings.HasPrefix(text, "x-anthropic-billing-header: ")
}

// ExtractClaudeSystemTextFiltered 与 ExtractClaudeSystemText 相同，但滤掉
// Claude Code 注入的 x-anthropic-billing-header 文本块。本转换保持
// system->instructions 的现状（不像 sub2api 那样转 developer item）。
// (Moved from app/claude_responses.go extractClaudeSystemTextFiltered.)
func ExtractClaudeSystemTextFiltered(system any) string {
	if system == nil {
		return ""
	}
	switch v := system.(type) {
	case string:
		if IsAnthropicBillingHeader(v) {
			return ""
		}
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if block, ok := item.(map[string]any); ok {
				if block["type"] == "text" {
					if text, ok := block["text"].(string); ok && text != "" && !IsAnthropicBillingHeader(text) {
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

// ClaudeMessagesToResponsesInput 把 Claude messages + system 转为 Responses 的
// instructions + input 数组。永不返回错误。缺失 tool_use id 由 idGen 注入随机
// string 兜底（生产：internal/random，"toolu_"+12 位）。
// (Moved from app/claude_responses.go claudeMessagesToResponsesInput.)
func ClaudeMessagesToResponsesInput(idGen IDGen, msgs []ClaudeMessage, system any) (string, []any) {
	var instructionParts []string
	if sysText := ExtractClaudeSystemTextFiltered(system); sysText != "" {
		instructionParts = append(instructionParts, sysText)
	}
	input := []any{} // 非 nil 空数组，避免上游对 null 的严格校验

	// tool_result 中的 image/document 提取为独立的 user message item（紧跟在
	// function_call_output 之后），而不是把 "[image attached]" 字符串塞进
	// output：sub2api 选独立 user message 而非 output parts 数组，理由是
	// function_call_output.output 只接字符串或 input parts 数组，codex 早期
	// 版本对 parts output 场景支持少。这里沿用同一取舍。
	var pendingToolImages []any
	emitToolImages := func() {
		if len(pendingToolImages) == 0 {
			return
		}
		parts := make([]any, 0, len(pendingToolImages))
		parts = append(parts, pendingToolImages...)
		input = append(input, map[string]any{
			"type":    "message",
			"role":    "user",
			"content": parts,
		})
		pendingToolImages = nil
	}

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
		// system role 消息并入 instructions（滤掉 Claude Code 注入的
		// x-anthropic-billing-header 块），不产生 input item。
		if msg.Role == "system" {
			if text := ExtractClaudeContentText(msg.Content); text != "" && !IsAnthropicBillingHeader(text) {
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
			emitToolImages()
			if content == "" {
				continue
			}
			input = append(input, map[string]any{
				"type":    "message",
				"role":    role,
				"content": []any{ResponsesTextPart(role, content)},
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
						pending = append(pending, ResponsesTextPart(role, text))
					}
				case "image":
					if role == "assistant" {
						// assistant 消息不允许 input_image，降级为文本标注。
						pending = append(pending, ResponsesTextPart(role, "[image attached]"))
					} else if part, ok := ClaudeImageBlockToOpenAI(block); ok {
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
							pending = append(pending, ResponsesTextPart(role, "[image attached]"))
						}
					} else {
						pending = append(pending, ResponsesTextPart(role, "[image attached]"))
					}
				case "document":
					if role == "assistant" {
						pending = append(pending, ResponsesTextPart(role, "[document attached]"))
					} else if part, ok := ClaudeDocumentBlockToOpenAI(block); ok {
						if fm, ok := part["file"].(map[string]any); ok {
							item := map[string]any{"type": "input_file"}
							for k, v := range fm {
								item[k] = v
							}
							pending = append(pending, item)
						} else {
							pending = append(pending, ResponsesTextPart(role, "[document attached]"))
						}
					} else {
						pending = append(pending, ResponsesTextPart(role, "[document attached]"))
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
					emitToolImages()
					id, _ := block["id"].(string)
					name, _ := block["name"].(string)
					if name == "" {
						// 缺 name 无法映射为 function_call，跳过该 block。
						continue
					}
					if id == "" {
						id = "toolu_" + idGen("toolu_", 12)
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
					text := ClaudeToolResultToText(block)
					if text == "" {
						// 空 output 一律写 "(empty)"（对齐 sub2api
						// convertToolResultOutput）：上游对空串 output 有
						// 严格校验时不再 400，且模型能看到工具确实无输出。
						text = "(empty)"
					}
					if isErr, _ := block["is_error"].(bool); isErr {
						text = ApplyErrorPrefix(text)
					}
					if toolUseID == "" {
						// 缺 ID 无法配对，降级为普通 user 文本，保留上下文。
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
					// tool_result 内的 image/document part 提取为独立 user
					// message（紧跟 function_call_output 之后），而不是塞
					// "[image attached]" 字符串。选独立 user message 而非
					// output parts 数组的理由与 sub2api 一致：上游对
					// function_call_output.output 的 parts 数组场景支持少。
					for _, part := range ClaudeToolResultMediaParts(block) {
						pendingToolImages = append(pendingToolImages, part)
					}
				case "":
					// 空 type：尝试按 role+content 兜底为文本。
					if text := ExtractClaudeContentText([]any{block}); text != "" {
						pending = append(pending, ResponsesTextPart(role, text))
					}
				default:
					// 未知 block（含 server_tool_use / web_search_* / mcp_* /
					// code_execution_* / search_result / container_upload 等）：
					// 尝试提取文本，提不出则序列化 JSON 保留上下文，不报错。
					if text := ExtractClaudeContentText([]any{block}); text != "" {
						pending = append(pending, ResponsesTextPart(role, text))
						continue
					}
					// 嵌套 content 数组里可能还有文本（如 web_search_tool_result）。
					if c, ok := block["content"]; ok && c != nil {
						if text := JoinToolResultContent(c); text != "" {
							pending = append(pending, ResponsesTextPart(role, text))
							continue
						}
					}
					if b, err := json.Marshal(block); err == nil {
						pending = append(pending, ResponsesTextPart(role, string(b)))
					}
				}
			}
			flushPending()
			emitToolImages()
		default:
			// 非常规 content 形状：序列化为文本，不报错。
			emitToolImages()
			if content == nil {
				continue
			}
			if b, err := json.Marshal(content); err == nil {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    role,
					"content": []any{ResponsesTextPart(role, string(b))},
				})
			}
		}
		emitToolImages()
	}

	instructions := strings.Join(instructionParts, "\n\n")
	return instructions, input
}

// ClaudeToolResultMediaParts 提取 tool_result content 数组里的
// image/document part，转为 Responses input_image/input_file part（非法或
// 无 source 的降级跳过——文本侧已有 "[image attached]" 标注兜底）。这些 part
// 由调用方组装成独立的 user message item 跟在 function_call_output 之后。
// (Moved from app/claude_responses.go claudeToolResultMediaParts.)
func ClaudeToolResultMediaParts(block map[string]any) []any {
	c, ok := block["content"].([]any)
	if !ok {
		return nil
	}
	var parts []any
	for _, p := range c {
		pb, ok := p.(map[string]any)
		if !ok {
			continue
		}
		switch pb["type"] {
		case "image":
			// {type:image_url, image_url:{url}} -> Responses input_image
			if part, ok := ClaudeImageBlockToOpenAI(pb); ok {
				url := ""
				if m, ok := part["image_url"].(map[string]string); ok {
					url = m["url"]
				} else if m, ok := part["image_url"].(map[string]any); ok {
					url, _ = m["url"].(string)
				}
				if url != "" {
					parts = append(parts, map[string]any{"type": "input_image", "image_url": url})
				}
			}
		case "document":
			if part, ok := ClaudeDocumentBlockToOpenAI(pb); ok {
				if fm, ok := part["file"].(map[string]any); ok {
					item := map[string]any{"type": "input_file"}
					for k, v := range fm {
						item[k] = v
					}
					parts = append(parts, item)
				}
			}
		}
	}
	return parts
}

// ClaudeToolResultToText 提取 tool_result 的文本，图片/文档附件转为标注。
// (Moved from app/claude_responses.go claudeToolResultToText.)
func ClaudeToolResultToText(block map[string]any) string {
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

// IsMuseSparkModel 报告是否为 muse-spark 系模型（大小写不敏感子串匹配）。
// 目前仅该系列上游对原生 responses 做严格校验（required 全覆盖、effort 白名单、
// 整数参数拒绝浮点），归一化只对它生效，避免影响其它模型的现有行为。
// (Moved from app/claude_responses.go isMuseSparkModel.)
func IsMuseSparkModel(modelID string) bool {
	return strings.Contains(strings.ToLower(modelID), "muse-spark")
}

// ResponsesTextPart 按 role 返回合法的文本 part 类型：
// user/developer 用 input_text，assistant 用 output_text。
// 上游原生 responses 对 assistant 消息中的 input_text 会 400
// （invalid_request_error: content type `input_text` is not valid on `assistant` messages），
// 因此必须区分。
// (Moved from app/claude_responses.go responsesTextPart.)
func ResponsesTextPart(role, text string) map[string]any {
	if role == "assistant" {
		return map[string]any{"type": "output_text", "text": text}
	}
	return map[string]any{"type": "input_text", "text": text}
}

// ClaudeToResponsesTools 把 Claude tools 转为 Responses function tools。
// Server tools（无 input_schema）静默跳过，不报错。
// (Moved from app/claude_responses.go claudeToResponsesTools.)
func ClaudeToResponsesTools(claudeTools []ClaudeTool, modelID string) []ResponsesTool {
	if len(claudeTools) == 0 {
		return nil
	}
	tools, _ := ClaudeToOpenAITools(claudeTools)
	out := make([]ResponsesTool, 0, len(tools))
	for _, t := range tools {
		params := t.Function.Parameters
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if IsMuseSparkModel(modelID) {
			params = NormalizeResponsesToolParameters(params)
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

// NormalizeResponsesToolParameters 保证 function parameters 满足上游原生
// responses 的严格校验：required 必须存在且包含 properties 的每一个 key，
// 缺失任一都会 400（Missing 'limit'）。这里做 best-effort 补齐，不报错。
// (Moved from app/claude_responses.go normalizeResponsesToolParameters.)
func NormalizeResponsesToolParameters(params map[string]any) map[string]any {
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

// ClaudeToolChoiceToResponses 把 Claude tool_choice 转为 Responses 形状的薄包装，
// 核心逻辑在 ClaudeToolChoiceCore（anthropic_protocol.go）。
// (Moved from app/claude_responses.go claudeToolChoiceToResponses.)
func ClaudeToolChoiceToResponses(choice any) any {
	return ClaudeToolChoiceCore(choice, false)
}

// ClaudeThinkingToResponsesEffort 从 thinking / output_config 推导 effort。
// 禁用或推导不出时返回 ""（调用方省略 reasoning 字段，不报错）。
// 上游原生 responses 对 effort 白名单校验（minimal/low/medium/high/xhigh），
// 不支持 max/none 等，非法值一律归一化或省略，绝不 400。
// forceDisableThinking 由调用方按 config 解析后注入。
// (Moved from app/claude_responses.go claudeThinkingToResponsesEffort.)
func ClaudeThinkingToResponsesEffort(forceDisableThinking bool, claudeReq ClaudeRequest, modelID string) string {
	if forceDisableThinking || IsThinkingDisabled(claudeReq.Thinking) {
		return ""
	}
	var effort string
	if e := EffortFromOutputConfig(claudeReq.OutputConfig); e != "" {
		effort = e
	} else {
		effort = ReasoningEffortFromThinking(claudeReq.Thinking)
	}
	if !IsMuseSparkModel(modelID) {
		return effort
	}
	return NormalizeResponsesEffort(effort)
}

// ClaudeToResponsesBody 把 Claude 请求转为原生 Responses 请求体。永不报错，
// 失败时返回最小可用体（model + input），避免 400。store 恒 false、include 恒
// 含 reasoning.encrypted_content、stream 时 stream_options.include_usage 恒
// true；max_tokens -> max_output_tokens 钳到 [128, cap]（cap 由调用方按
// config.MaxTokensCapFor(modelID) 注入）；thinking 与 temperature/top_p 互斥。
// (Moved from app/claude_responses.go claudeToResponsesBody.)
func ClaudeToResponsesBody(cv ConfigView, idGen IDGen, claudeReq ClaudeRequest, modelID string) []byte {
	instructions, input := ClaudeMessagesToResponsesInput(idGen, claudeReq.Messages, claudeReq.System)
	body := map[string]any{
		"model":  modelID,
		"input":  input,
		"stream": claudeReq.Stream,
		// store:false 与 sub2api 一致：网关不依赖服务端会话存储，避免上游
		// 为匿名会话堆积状态（Worker A 的 ChatToResponsesBody 同样显式 false）。
		"store": false,
		// 索取 reasoning.encrypted_content（对齐 Worker A 的 G3 与 sub2api），
		// 响应侧把 encrypted_content 落到 thinking block 的 signature 槽位，
		// 供 Claude Code 下一轮原样带回。
		"include": []string{"reasoning.encrypted_content"},
	}
	if instructions != "" {
		body["instructions"] = instructions
	}
	if claudeReq.Stream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	// reasoning effort 先于采样参数求值：thinking 与 temperature/top_p 互斥。
	effort := ClaudeThinkingToResponsesEffort(cv.ForceDisableThinking, claudeReq, modelID)
	reasoningOn := effort != "" && effort != "none"
	if claudeReq.Temperature != nil && !reasoningOn {
		body["temperature"] = *claudeReq.Temperature
	}
	// max_tokens -> max_output_tokens，clamp 到 [128, cap]（cap 见
	// config.MaxTokensCapFor；min 128 由 ClampClaudeMaxTokens 统一保证）。
	// 未显式设置时注入 cap；无 cap 则抬到 128（与 responses 直通同口径）。
	tokenCap := cv.MaxTokensCap
	v := 0
	if claudeReq.MaxTokens != nil {
		v = *claudeReq.MaxTokens
	}
	if v <= 0 {
		v = tokenCap
	}
	body["max_output_tokens"] = ClampClaudeMaxTokens(v, tokenCap)
	if claudeReq.TopP != nil && !reasoningOn {
		body["top_p"] = *claudeReq.TopP
	}
	if tools := ClaudeToResponsesTools(claudeReq.Tools, modelID); len(tools) > 0 {
		body["tools"] = tools
	}
	if claudeReq.ToolChoice != nil {
		body["tool_choice"] = ClaudeToolChoiceToResponses(claudeReq.ToolChoice)
	}
	// disable_parallel_tool_use 反向映射：Anthropic 的 disable 为 true 时
	// Responses 侧写 parallel_tool_calls=false。
	if ClaudeToolChoiceDisablesParallel(claudeReq.ToolChoice) {
		body["parallel_tool_calls"] = false
	}
	if reasoningOn {
		body["reasoning"] = map[string]any{"effort": effort}
	}
	if user := NarrowClaudeMetadataUser(claudeReq.Metadata); user != "" {
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
	// 互斥说明：Anthropic thinking 模式与 OpenAI Responses 的 gpt-5 族都
	// 不接受 temperature/top_p 采样参数（"Unsupported parameter" 400），
	// 因此 reasoningOn 时上面已跳过二者（对齐 sub2api AnthropicToResponses）。
	// top_k / cache_control / signature / context_management / betas 无对应物，丢弃。
	b, err := json.Marshal(body)
	if err != nil {
		fallback, _ := json.Marshal(map[string]any{"model": modelID, "input": input})
		return fallback
	}
	return b
}
