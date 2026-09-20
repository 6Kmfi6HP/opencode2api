package bridge

import (
	"encoding/json"
)

// This file holds the pure non-streaming Chat -> OpenAI upstream request-body
// construction moved out of internal/app/chat.go. None of these functions
// perform I/O, logging, metrics, or config reads; every operator-tunable input
// arrives via ConfigView. The app layer keeps same-name lowercase forwarding
// shells so existing *_test.go files compile unchanged.

// MultimodalAttachedLabel is the text annotation that replaces multimodal
// image parts when the resolved upstream model only accepts text. It matches
// the label the Claude converter already uses for tool_result attachments, so
// tool images and message images degrade identically.
const (
	MultimodalAttachedLabel = "[image attached]"
	MultimodalDocumentLabel = "[document attached]"
)

// 能力协商由 opencode 客户端 + 上游负责；这里既不"硬降级"也不"补全"。
// (Moved from app/chat.go normalizeContent.)
func NormalizeContent(content any) any {
	if content == nil {
		return nil
	}
	if s, ok := content.(string); ok {
		return s
	}
	if arr, ok := content.([]any); ok {
		return arr
	}
	b, err := json.Marshal(content)
	if err != nil {
		return nil
	}
	return string(b)
}

// FixToolCallGaps 补全 assistant.tool_calls 缺失的 tool 响应：每条 tool_call
// 之后插入对应 role=tool 消息，缺失响应时回填占位文本。重复 tool_call_id 的
// 多余 tool 消息被去重。(Moved from app/chat.go fixToolCallGaps.)
func FixToolCallGaps(messages []Message) []Message {
	toolResponses := map[string]*Message{}
	for i := range messages {
		if messages[i].Role == "tool" && messages[i].ToolCallID != "" {
			toolResponses[messages[i].ToolCallID] = &messages[i]
		}
	}
	fixed := make([]Message, 0, len(messages)+len(messages)/4)
	emitted := map[string]bool{}
	for _, msg := range messages {
		if msg.Role == "tool" && msg.ToolCallID != "" {
			if emitted[msg.ToolCallID] {
				continue
			}
		}
		fixed = append(fixed, msg)
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				if resp, found := toolResponses[tc.ID]; found {
					fixed = append(fixed, *resp)
				} else {
					fixed = append(fixed, Message{Role: "tool", ToolCallID: tc.ID, Content: "Tool call result not available"})
				}
				emitted[tc.ID] = true
			}
		}
	}
	return fixed
}

// EnsureReasoningContent 在 thinking 开启时为 reasoning_content==nil 的
// assistant 消息补空串槽位。它不覆盖已有值：WeChat/DeepSeek 兼容要求带
// tool_calls 的 assistant 消息携带产生它的推理（claudeToOpenAIMessages 只在
// 该情况下写入 reasoning_content），空串槽位只补到没有推理文本的普通
// assistant 轮，序列化后被 convertStreamChunkWithUsage/cleanNulls 的空串清
// 理兜住，因此不与收窄后的写入语义冲突。
// (Moved from app/chat.go ensureReasoningContent.)
func EnsureReasoningContent(messages []Message, thinking bool) []Message {
	if !thinking {
		return messages
	}
	for i := range messages {
		if messages[i].Role == "assistant" && messages[i].ReasoningContent == nil {
			empty := ""
			messages[i].ReasoningContent = &empty
		}
	}
	return messages
}

// CountMultimodalParts returns the number of image/document content parts in
// a request, for observability (request_plan).
// (Moved from app/chat.go countMultimodalParts.)
func CountMultimodalParts(messages []Message) int {
	n := 0
	for _, msg := range messages {
		arr, ok := msg.Content.([]any)
		if !ok {
			continue
		}
		for _, part := range arr {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch pm["type"] {
			case "image_url", "file":
				n++
			}
		}
	}
	return n
}

// DowngradeMultimodalContent replaces image_url ("[image attached]") and file
// ("[document attached]") content parts with text annotations so requests to
// text-only upstream models (e.g. DeepSeek) keep working instead of failing
// with "image not supported". Text and all other parts are preserved, as is
// the relative order. Returns the original slice unchanged when model is not
// text-only or there is nothing to downgrade.
// (Moved from app/chat.go downgradeMultimodalContent.)
func DowngradeMultimodalContent(content []any, textOnly bool) any {
	if !textOnly {
		return content
	}
	out := make([]any, 0, len(content))
	downgraded := false
	for _, part := range content {
		pm, ok := part.(map[string]any)
		if !ok {
			out = append(out, part)
			continue
		}
		switch pm["type"] {
		case "image_url":
			downgraded = true
			out = append(out, map[string]any{"type": "text", "text": MultimodalAttachedLabel})
		case "file":
			downgraded = true
			out = append(out, map[string]any{"type": "text", "text": MultimodalDocumentLabel})
		default:
			out = append(out, part)
		}
	}
	if !downgraded {
		return content
	}
	return out
}

// ConvertMessagesForUpstream 把 Chat messages 序列化为上游 OpenAI 请求体的
// messages 数组：role/content/reasoning_content/tool_calls/tool_call_id/name
// 按非空键落位；content 多模态数组在 textOnly 时降级为文本标注。
// (Moved from app/chat.go convertMessagesForUpstream.)
func ConvertMessagesForUpstream(messages []Message, textOnly bool) []map[string]any {
	converted := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		clean := map[string]any{}
		if msg.Role != "" {
			clean["role"] = msg.Role
		}
		content := NormalizeContent(msg.Content)
		if arr, ok := content.([]any); ok {
			content = DowngradeMultimodalContent(arr, textOnly)
		}
		reasoningContent := msg.ReasoningContent
		if content != nil {
			clean["content"] = content
		}
		if reasoningContent != nil {
			clean["reasoning_content"] = *reasoningContent
		}
		if len(msg.ToolCalls) > 0 {
			clean["tool_calls"] = msg.ToolCalls
		}
		if msg.ToolCallID != "" {
			clean["tool_call_id"] = msg.ToolCallID
		}
		if msg.Name != "" {
			clean["name"] = msg.Name
		}
		converted = append(converted, clean)
	}
	return converted
}

// ConvertRequest 把 Chat Completions 请求转为 OpenCode(OpenAI 形状)上游请求体。
// 所有 operator 配置经 cv 注入（max_tokens cap、force-disable-thinking、
// reasoning_effort map、prompt_cache_retention、cache_control 断点、text_only、
// rejects_cache_control）；max_tokens 收敛到 [1, cap]。客户端显式 stop/penalty/
// logit_bias/n/user/response_format/seed/tools/tool_choice 显式透传，ExtraBody
// 合并只补缺不覆盖。客户端显式 stream_options.include_usage 由调用方在 ExtraBody
// 注入（本函数只透传）。thinking 与 temperature/top_p 互斥不在此层做（chat 入站
// 不强制剥离；该互斥只在 claude->* 与 *->responses 方向处理）。
// (Moved from app/chat.go convertRequest.)
func ConvertRequest(cv ConfigView, req *OpenAIRequest) map[string]any {
	converted := map[string]any{
		"model":    req.Model,
		"messages": ConvertMessagesForUpstream(req.Messages, cv.TextOnly),
		"stream":   req.Stream,
	}
	if req.Temperature != nil {
		converted["temperature"] = *req.Temperature
	}
	if req.MaxTokens != nil {
		// ClampMaxTokens 复用 cap 收敛；chat 入站的 max_tokens 是客户端可选
		// 字段，下限收敛无害。
		converted["max_tokens"] = ClampMaxTokens(*req.MaxTokens, cv.MaxTokensCap)
	} else if cv.MaxTokensCap > 0 {
		// 未显式设置时注入 cap：与 responses 直通口径一致（cap 即上游默认
		// 预算，避免上游按自身小默认截断）。
		converted["max_tokens"] = cv.MaxTokensCap
	}
	if req.MaxCompletionTokens != nil {
		converted["max_completion_tokens"] = ClampMaxTokens(*req.MaxCompletionTokens, cv.MaxTokensCap)
	}
	if req.TopP != nil {
		converted["top_p"] = *req.TopP
	}
	// stop/penalties/logit_bias/n：domain/types.go 已由 Worker C 加字段，
	// 显式透传（ExtraBody 合并只补缺，不会覆盖）。
	if req.Stop != nil {
		converted["stop"] = req.Stop
	}
	if req.FrequencyPenalty != nil {
		converted["frequency_penalty"] = *req.FrequencyPenalty
	}
	if req.PresencePenalty != nil {
		converted["presence_penalty"] = *req.PresencePenalty
	}
	if req.LogitBias != nil {
		converted["logit_bias"] = req.LogitBias
	}
	if req.N != nil {
		converted["n"] = *req.N
	}
	if req.User != "" {
		converted["user"] = req.User
	}
	if req.ResponseFormat != nil {
		converted["response_format"] = req.ResponseFormat
	}
	if req.Seed != nil {
		converted["seed"] = *req.Seed
	}
	if len(req.Tools) > 0 {
		converted["tools"] = req.Tools
	}
	if req.ToolChoice != nil {
		converted["tool_choice"] = req.ToolChoice
	}
	// 处理思维模式 — 仅当用户显式指定时才发送，避免 MiniMax 等模型报错
	if cv.ForceDisableThinking || IsThinkingDisabled(req.Thinking) {
		converted["thinking"] = map[string]string{"type": "disabled"}
	} else if req.Thinking != nil && IsThinkingEnabled(req.Thinking) {
		converted["thinking"] = BuildUpstreamThinking(req.Thinking)
	} else if req.ExtraBody != nil {
		if IsThinkingDisabled(req.ExtraBody["thinking"]) {
			converted["thinking"] = map[string]string{"type": "disabled"}
		} else if IsThinkingEnabled(req.ExtraBody["thinking"]) {
			converted["thinking"] = BuildUpstreamThinking(req.ExtraBody["thinking"])
		}
	}
	// 处理 reasoning_effort（含从 thinking.budget_tokens 推导）
	effort := req.ReasoningEffort
	if effort == "" && !IsThinkingDisabled(req.Thinking) {
		effort = ReasoningEffortFromThinking(req.Thinking)
	}
	if !cv.ForceDisableThinking && effort != "" {
		converted["reasoning_effort"] = MappedReasoningEffort(cv.ReasoningEffortMap, effort)
	}
	// 合并 ExtraBody
	if req.ExtraBody != nil {
		for k, v := range req.ExtraBody {
			if _, exists := converted[k]; !exists {
				converted[k] = v
			}
		}
	}
	// 缓存增强:向 zen 上游显式声明 prompt 前缀缓存的保留时长。
	// 上游默认约 5 分钟(in_memory),agent 任务间歇易过期,导致缓存难命中;
	// 注入 retention 后拉长到 24h。客户端显式传入的值(extra_body)优先。
	if retention := cv.PromptCacheRetention; retention != "" && retention != "off" {
		if _, exists := converted["prompt_cache_retention"]; !exists {
			converted["prompt_cache_retention"] = retention
		}
	}
	// Anthropic 风格 cache_control 断点:对接受该字段的模型(排除 GLM/Zhipu)
	// 显式标记缓存断点并拉长 TTL。对不支持的上游,zen 网关负责剥离;
	// DeepSeek 等自动前缀缓存不受影响(实测追加字段后命中率一致)。
	if cv.CacheBreakpoints && !cv.RejectsCacheControl {
		if _, exists := converted["cache_control"]; !exists {
			converted["cache_control"] = map[string]any{"type": "ephemeral", "ttl": "1h"}
		}
	}
	return converted
}

// MarshalUpstreamBody 把 ConvertRequest 的 map 序列化为上游请求体字节。
// 序列化失败只允许发生在不可 marshal 的 ExtraBody 值上；纯层不打日志，
// 错误返回给 app 壳记录。返回字节与错误（原实现 slog.Error 后返回 nil 字节）。
// (Moved from app/chat.go buildUpstreamBody.)
func MarshalUpstreamBody(cv ConfigView, req *OpenAIRequest) ([]byte, error) {
	return json.Marshal(ConvertRequest(cv, req))
}
