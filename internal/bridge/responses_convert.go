package bridge

import (
	"encoding/json"
	"strings"
)

// This file holds the pure Responses-protocol conversion builders moved out of
// internal/app: the shared response-outcome / output-index helpers, the
// Chat->Responses body converter, the usage mappers, and the Responses->Claude
// body converters. They perform no I/O, logging, metrics, or config reads; ID
// generation is injected via an IDGen. The app layer keeps same-name lowercase
// forwarding shells so existing *_test.go files compile unchanged.

// ======================== Responses 协议约定 ========================

// ResponseOutcome is shared by streaming and non-streaming builders so their
// top-level and item statuses cannot drift apart. (Moved from
// app/responses_protocol.go responseOutcome.)
type ResponseOutcome struct {
	Status            string
	Event             string
	IncompleteDetails any
}

// ResponsesOutcome maps a finish reason onto the Responses terminal outcome:
// length -> incomplete/max_output_tokens, anything else -> completed.
// (Moved from app/responses_protocol.go responsesOutcome.)
func ResponsesOutcome(finishReason string) ResponseOutcome {
	if finishReason == "length" {
		return ResponseOutcome{Status: "incomplete", Event: "response.incomplete", IncompleteDetails: map[string]any{"reason": "max_output_tokens"}}
	}
	return ResponseOutcome{Status: "completed", Event: "response.completed"}
}

// OutputIndexAllocator assigns indices by first appearance. It deliberately
// does not derive one item's index from whether another item happened to
// exist. (Moved from app/responses_protocol.go outputIndexAllocator.)
type OutputIndexAllocator struct{ next int }

// Allocate returns the next sequential output index.
func (a *OutputIndexAllocator) Allocate() int {
	index := a.next
	a.next++
	return index
}

// Len reports how many indices have been allocated.
func (a *OutputIndexAllocator) Len() int { return a.next }

// ResponsesInputTokensDetails normalizes a Responses input_tokens_details map:
// it guarantees a cached_tokens field, defaulting to 0 when absent.
// (Moved from app/responses.go responsesInputTokensDetails.)
func ResponsesInputTokensDetails(details any) map[string]any {
	if m, ok := details.(map[string]any); ok {
		if cached, ok := m["cached_tokens"]; ok && cached != nil {
			return m
		}
		m["cached_tokens"] = 0
		return m
	}
	return map[string]any{"cached_tokens": 0}
}

// ConvertChatToResponses 把一个上游 Chat Completions 响应体转换为 Responses
// 成功响应体。idGen 注入随机 ID 生成（生产默认 internal/random），用于把上游
// id 归一到 resp_ 命名空间。解析失败时按空响应继续（不报错）。
// (Moved from app/responses.go convertChatToResponses; unmarshal-failure warn
// dropped from the pure layer so bridge stays free of logging.)
func ConvertChatToResponses(idGen IDGen, chatBody []byte, model string, wantReasoning bool, tools []ResponsesTool, toolChoice any, include []string) []byte {
	var chat struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          any        `json:"content"`
				Refusal          string     `json:"refusal"`
				ReasoningContent string     `json:"reasoning_content"`
				ToolCalls        []ToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	// 解析失败时按空响应继续（不报错）；原实现记录 warn，迁移后由调用方负责
	// 可观测性，响应体形状保持一致。
	_ = json.Unmarshal(chatBody, &chat)

	reasoning := ""
	finishReason := ""
	var toolCalls []ToolCall
	messageContent := []any(nil)
	toolKinds := ResponsesToolKindMap(tools)
	if len(chat.Choices) > 0 {
		messageContent, _ = ChatContentToResponsesContent(chat.Choices[0].Message.Content)
		if refusal := chat.Choices[0].Message.Refusal; refusal != "" {
			messageContent = []any{map[string]any{"type": "refusal", "refusal": refusal}}
		}
		rc := chat.Choices[0].Message.ReasoningContent
		if wantReasoning {
			reasoning = rc
		}
		toolCalls = chat.Choices[0].Message.ToolCalls
		finishReason = chat.Choices[0].FinishReason
		if len(messageContent) == 0 && rc != "" && len(toolCalls) == 0 {
			messageContent, _ = ChatContentToResponsesContent(rc)
		}
	}

	outcome := ResponsesOutcome(finishReason)
	status := outcome.Status
	normalizedID := NormalizeResponsesID(idGen, chat.ID)
	responses := map[string]any{
		"id":                 normalizedID,
		"object":             "response",
		"status":             status,
		"background":         false,
		"error":              nil,
		"incomplete_details": outcome.IncompleteDetails,
		"model":              model,
		"created_at":         chat.Created,
	}
	if len(tools) > 0 {
		responses["tools"] = tools
	}
	if toolChoice != nil {
		responses["tool_choice"] = toolChoice
	}
	outputID := "msg_" + normalizedID + "_0"
	output := []any{}
	if reasoning != "" {
		reasoningItem := map[string]any{
			"id":      "rs_" + normalizedID,
			"type":    "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": reasoning}},
		}
		if IncludeHas(include, "reasoning.encrypted_content") {
			reasoningItem["encrypted_content"] = ""
		}
		output = append(output, reasoningItem)
	}
	if len(messageContent) > 0 {
		output = append(output, map[string]any{
			"id":      outputID,
			"type":    "message",
			"status":  status,
			"role":    "assistant",
			"content": messageContent,
		})
	}
	for _, tc := range toolCalls {
		item := BuildResponseToolCallItem(tc, ToolCallOutputType(tc.Function.Name, toolKinds))
		item["status"] = status
		output = append(output, item)
	}
	// 空输出补一条空 message：Responses 客户端（Codex/官方 SDK）期望非空
	// output；纯 reasoning（wantReasoning=false）+ 无内容的回合兜底空文本，
	// 保持数组形状与 status。
	if len(output) == 0 {
		output = append(output, EmptyAssistantMessageItem(outputID, status))
	}
	responses["output"] = output
	if chat.Usage != nil {
		responses["usage"] = ChatUsageMapToResponses(chat.Usage)
	}

	result, _ := json.Marshal(responses)
	return result
}

// EmptyAssistantMessageItem 构造条 status 一致的空 output_text message。
// (Moved from app/responses.go.)
func EmptyAssistantMessageItem(outputID, status string) map[string]any {
	return map[string]any{
		"id":     outputID,
		"type":   "message",
		"status": status,
		"role":   "assistant",
		"content": []any{map[string]any{
			"type":        "output_text",
			"text":        "",
			"annotations": []any{},
			"logprobs":    []any{},
		}},
	}
}

// ChatUsageMapToResponses 把 Chat Completions usage（上游 zen/go 口径）转
// Responses 口径：
//   - input_tokens = prompt_tokens + 顶层 cache_read_input_tokens +
//     cache_creation_input_tokens（AnthropicUsageToChat 已把这两个顶层字段透传
//     进 chat usage，而 prompt_tokens 是缓存感知的读数，按 Responses 口径加回）。
//     无缓存字段时退化为原 prompt_tokens（保持既有行为）。
//   - input_tokens_details.cached_tokens 优先取顶层 cache_read_input_tokens
//     （未见时退回 prompt_tokens_details.cached_tokens，再兜底 0）。
//   - output_tokens_details 从 completion_tokens_details 透传
//     reasoning_tokens（thinking token）等细节。
//
// (Moved from app/responses.go chatUsageMapToResponses.)
func ChatUsageMapToResponses(u map[string]any) map[string]any {
	usage := map[string]any{}
	readInt := func(v any) int64 {
		if n, ok := NumberAsFloat(v); ok {
			return int64(n)
		}
		return 0
	}
	// input：prompt 分量 + 顶层缓存字段
	prompt, hasPrompt := u["prompt_tokens"]
	cacheRead, hasCacheRead := u["cache_read_input_tokens"]
	cacheCreation, hasCacheCreation := u["cache_creation_input_tokens"]
	inputTotal := readInt(prompt)
	if hasPrompt {
		if hasCacheRead {
			inputTotal += readInt(cacheRead)
		}
		if hasCacheCreation {
			inputTotal += readInt(cacheCreation)
		}
		usage["input_tokens"] = inputTotal
	}
	// cached_tokens：优先顶层 cache_read_input_tokens，退回 prompt_tokens_details。
	// 其余 prompt_tokens_details 字段（text_tokens 等）原样透传。
	var detailsOut map[string]any
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		detailsOut = make(map[string]any, len(d)+1)
		for k, v := range d {
			detailsOut[k] = v
		}
	} else {
		detailsOut = map[string]any{}
	}
	cached := int64(0)
	if hasCacheRead {
		cached = readInt(cacheRead)
	} else if c, ok := detailsOut["cached_tokens"]; ok {
		cached = readInt(c)
	}
	detailsOut["cached_tokens"] = cached
	usage["input_tokens_details"] = detailsOut
	if v, ok := u["completion_tokens"]; ok {
		usage["output_tokens"] = v
	}
	if v, ok := u["completion_tokens_details"]; ok {
		usage["output_tokens_details"] = v
	} else if reasoningTokens := reasoningTokenEstimate(u); reasoningTokens > 0 {
		usage["output_tokens_details"] = map[string]any{"reasoning_tokens": reasoningTokens}
	}
	if v, ok := u["total_tokens"]; ok {
		usage["total_tokens"] = v
	}
	if v, ok := u["input_tokens"]; ok && usage["input_tokens"] == nil {
		usage["input_tokens"] = v
	}
	if v, ok := u["output_tokens"]; ok && usage["output_tokens"] == nil {
		usage["output_tokens"] = v
	}
	return usage
}

// reasoningTokenEstimate 从 usage 中提取 thinking/reasoning token 数量的兜底
// 估算：缺 completion_tokens_details 时按已知顶层/通用键查找。
// (Moved from app/responses.go.)
func reasoningTokenEstimate(u map[string]any) int64 {
	for _, k := range []string{"reasoning_tokens", "thinking_tokens"} {
		if v, ok := NumberAsFloat(u[k]); ok && v > 0 {
			return int64(v)
		}
	}
	return 0
}

// ReasoningTokenEstimate 是 reasoningTokenEstimate 的可导出形态。
// (Moved from app/responses.go.)
func ReasoningTokenEstimate(u map[string]any) int64 { return reasoningTokenEstimate(u) }

// ======================== Responses -> Claude 转换（lenient） ========================

// ResponsesOutputToClaudeBlocks 把原生 Responses output 数组转为 Claude
// content blocks。未知 item 类型降级为文本，不报错。
// (Moved from app/claude_responses.go responsesOutputToClaudeBlocks.)
func ResponsesOutputToClaudeBlocks(output []any, wantReasoning bool) ([]ClaudeContent, string, bool) {
	content := []ClaudeContent{}
	stopReason := "end_turn"
	hasToolUse := false
	// reasoningTexts 由 reasoning item 的 summary 文本组成，每段带上该
	// item 的 encrypted_content 作为 signature（仅首段）。
	type reasoningPart struct {
		text      string
		signature string
	}
	var reasoningTexts []reasoningPart
	var reasoningEncryptedOnly []string
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
			// encrypted_content 落到 thinking block 的 signature 槽位，供
			// Claude Code roundtrip（对齐 sub2api 的 reasoning item 方向）。
			sig, _ := item["encrypted_content"].(string)
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
			if len(texts) == 0 && sig != "" && wantReasoning {
				// 无 summary 的 encrypted-only reasoning：发出带 signature
				// 的空 thinking 槽位，保住 roundtrip（无文本可显示）。
				reasoningEncryptedOnly = append(reasoningEncryptedOnly, sig)
			}
			for _, t := range texts {
				reasoningTexts = append(reasoningTexts, reasoningPart{text: t, signature: sig})
				sig = "" // signature 只归属第一个 thinking block
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
			if t := ExtractTextFromContentParts(item["content"]); t != "" {
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
			cc := ClaudeContent{Type: "thinking", Thinking: t.text}
			if t.signature != "" {
				cc.Signature = t.signature
			}
			content = append(content, cc)
		}
		for _, sig := range reasoningEncryptedOnly {
			content = append(content, ClaudeContent{Type: "thinking", Thinking: "", Signature: sig})
		}
	}
	// wantReasoning==false 时 reasoning 直接丢弃（由调用方在空回复时 promote，
	// 与 openAIToClaudeResponse 的 keep 语义一致）；这里不提前 promote，
	// 统一在下方空回复保护中处理。

	var plainReasoning []string
	for _, t := range reasoningTexts {
		plainReasoning = append(plainReasoning, t.text)
	}
	joinedText := strings.Join(textParts, "\n")
	if joinedText == "" && len(plainReasoning) > 0 && len(tools) == 0 {
		// 空回复保护：Go 网关常把正文放在 reasoning 里（#37635），提升为文本。
		joinedText = strings.Join(plainReasoning, "\n")
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

// ConvertResponsesToClaude 把原生 Responses 成功响应转为 Claude message。
// idGen 注入随机 ID 生成（生产默认 internal/random），用于把上游 id 归一到
// msg_ 命名空间。解析失败时返回最小可用空文本消息，不报错。
// (Moved from app/claude_responses.go convertResponsesToClaude; unmarshal-failure
// warn dropped from the pure layer so bridge stays free of logging.)
func ConvertResponsesToClaude(idGen IDGen, respBody []byte, model string, wantReasoning bool) []byte {
	var resp struct {
		ID     string         `json:"id"`
		Output []any          `json:"output"`
		Status string         `json:"status"`
		Usage  map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		// 原实现记录 warn，迁移后由调用方负责可观测性，响应体形状保持一致。
		resp.Output = nil
	}
	content, stopReason, _ := ResponsesOutputToClaudeBlocks(resp.Output, wantReasoning)
	// incomplete/max_output_tokens 映射。
	if resp.Status == "incomplete" {
		stopReason = "max_tokens"
	}
	claudeResp := ClaudeResponse{
		ID:           NormalizeClaudeMessageID(idGen, resp.ID),
		Type:         "message",
		Role:         "assistant",
		Content:      content,
		Model:        model,
		StopReason:   stopReason,
		StopSequence: nil,
	}
	if resp.Usage != nil {
		claudeResp.Usage = BuildClaudeMessageUsage(ResponsesUsageToChat(resp.Usage))
	}
	result, _ := json.Marshal(claudeResp)
	return result
}

// ConvertResponsesErrorToClaude 把原生 Responses 错误体转为 Claude 错误形状。
// 上游 message 尽量保留，不暴露内部错误串。
// (Moved from app/claude_responses.go convertResponsesErrorToClaude.)
func ConvertResponsesErrorToClaude(respBody []byte) []byte {
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

// ClaudeResponsesBlock 是 Claude 经原生 responses 流式转换时，跟踪一个
// 输出 content block 的开放状态。纯数据容器，无任何方法副作用。
// (Moved from app/claude_responses.go claudeResponsesBlock. Field names are
// exported here so the app-side streaming shell, which retains the streaming
// logic, can read and mutate them; the app keeps a same-name alias.)
type ClaudeResponsesBlock struct {
	ClaudeIndex int
	Kind        string // text | thinking | tool
	Open        bool
	ToolID      string
	ToolName    string
	// Signature 是 reasoning item 的 encrypted_content，关 thinking block
	// 前以 signature_delta 发出（对齐 sub2api），供 Claude Code roundtrip。
	Signature string
}
