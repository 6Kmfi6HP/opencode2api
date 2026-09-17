package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/6Kmfi6HP/opencode2api/internal/config"
	"github.com/6Kmfi6HP/opencode2api/internal/logging"
	statsx "github.com/6Kmfi6HP/opencode2api/internal/stats"
)

// ======================== /v1/chat/completions → Anthropic 上游 ========================

// defaultAnthropicMaxTokens 是 Chat 入站缺省 max_tokens 时的兜底值。
// Anthropic Messages 上游把 max_tokens 视为必填。
const defaultAnthropicMaxTokens = 8192

// effortToThinkingBudget 把 reasoning_effort 映射为 Anthropic thinking 预算。
func effortToThinkingBudget(effort string) int {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal", "low":
		return 1024
	case "medium":
		return 4096
	case "high":
		return 10240
	case "xhigh", "max":
		return 32768
	default:
		return 0
	}
}

// chatToolChoiceToAnthropic 把 Chat tool_choice 转为 Anthropic 形状。
func chatToolChoiceToAnthropic(choice any) any {
	switch v := choice.(type) {
	case string:
		switch v {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required":
			return map[string]any{"type": "any"}
		case "none":
			return map[string]any{"type": "none"}
		}
	case map[string]any:
		if fn, ok := v["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		}
	}
	return nil
}

// chatTextToAnthropicContent 把字符串或 Chat 多模态 content 数组转为
// Anthropic content block 数组。图片按 data URI / URL 归类为 base64/url source；
// 无法解析的 part 丢弃（记 debug 日志），全部丢光时回填 [image attached] 文本，
// 保证多模态消息永远有内容（Anthropic 不接受空 content 数组）。
func chatTextToAnthropicContent(content any) ([]map[string]any, bool) {
	switch v := content.(type) {
	case string:
		if v == "" {
			return nil, false
		}
		return []map[string]any{{"type": "text", "text": v}}, true
	case []any:
		var blocks []map[string]any
		droppedImage := false
		for _, part := range v {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch pm["type"] {
			case "text":
				if t, _ := pm["text"].(string); t != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": t})
				}
			case "image_url":
				url, _ := pm["url"].(string)
				if url == "" {
					if iu, ok := pm["image_url"].(map[string]any); ok {
						url, _ = iu["url"].(string)
					}
				}
				if url == "" {
					slog.Debug("chat→anthropic: image_url part without url skipped")
					droppedImage = true
					continue
				}
				if block := imageURLToAnthropicBlock(url); block != nil {
					blocks = append(blocks, block)
				} else {
					droppedImage = true
				}
			default:
				slog.Debug("chat→anthropic: unsupported content part skipped", "part_type", pm["type"])
			}
		}
		if len(blocks) == 0 && droppedImage {
			// 多模态消息的图片全部无法转换时保留一个占位文本,避免用户消息
			// 内容整体消失（对齐 text-only 降级的 multimodalAttachedLabel）。
			blocks = append(blocks, map[string]any{"type": "text", "text": multimodalAttachedLabel})
		}
		return blocks, len(blocks) > 0
	default:
		return nil, false
	}
}

// imageURLToAnthropicBlock 把 data URI 或 http(s) URL 转为 Anthropic image block。
// data URI 缺 ";base64," 或无法解析媒体类型时丢弃该 part（返回 nil）并日志,
// 不再把 data URI 塞进 url source 兜底。钝 media 类型（无 "/"）回退 image/png。
func imageURLToAnthropicBlock(url string) map[string]any {
	if after, ok := strings.CutPrefix(url, "data:"); ok {
		meta, b64, ok := strings.Cut(after, ";base64,")
		if !ok {
			slog.Debug("chat→anthropic: data URI without ;base64, marker dropped", "prefix", meta)
			return nil
		}
		mediaType := meta
		media := strings.TrimPrefix(meta, "image/")
		if mediaType == "" || media == "" || media == mediaType {
			// 钝 media 类型（如 data:png;base64 或空）视为 png。
			media = "png"
			mediaType = "image/png"
		}
		return map[string]any{
			"type": "image",
			"source": map[string]any{
				"type": "base64", "media_type": mediaType, "data": b64,
			},
		}
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type": "url", "url": url,
		},
	}
}

// chatMessagesToAnthropic 把 Chat messages 转为 Anthropic system + messages。
// system/developer 角色提取为 system（多条 "\n\n" 连接）；role=tool 转为
// tool_result user 消息；assistant tool_calls 内联为 tool_use block。
// 返回的 messages 已做连续同角色合并（Anthropic 要求交替）。
func chatMessagesToAnthropic(messages []Message) (string, []map[string]any) {
	var systemParts []string
	var out []map[string]any
	appendBlocks := func(role string, blocks []map[string]any) {
		if len(blocks) == 0 {
			return
		}
		// 连续同角色合并:Anthropic 不允许相邻同 role,且合并 tool_use 是
		// 合法的(同 turn 可含 text+tool_use 序列)。
		// 注:真正的非法情况是 assistant[tool_use] 之后无 user[tool_result]
		// 直接又出现 assistant —— 那种序列本身在 chat 输入里就不规范,
		// 上游会以 tool_use_without_result 拒绝,这里不做单独修复(超出
		// merge 的职责)。
		if n := len(out); n > 0 {
			if prevRole, _ := out[n-1]["role"].(string); prevRole == role {
				prev, _ := out[n-1]["content"].([]map[string]any)
				out[n-1]["content"] = append(prev, blocks...)
				return
			}
		}
		out = append(out, map[string]any{"role": role, "content": blocks})
	}
	for _, msg := range messages {
		switch msg.Role {
		case "system", "developer":
			// parts 数组只提取 text 分片进 system（保持现状;system 文本无需
			// 携带多模态内容,图片分片有意丢弃）。
			if blocks, ok := chatTextToAnthropicContent(msg.Content); ok {
				for _, b := range blocks {
					if t, _ := b["text"].(string); t != "" {
						systemParts = append(systemParts, t)
					}
				}
			}
		case "assistant":
			var blocks []map[string]any
			if bs, ok := chatTextToAnthropicContent(msg.Content); ok {
				blocks = append(blocks, bs...)
			}
			for _, tc := range msg.ToolCalls {
				input := parseToolCallArguments(tc.Function.Arguments)
				blocks = append(blocks, map[string]any{
					"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
				})
			}
			appendBlocks("assistant", blocks)
		case "tool":
			appendBlocks("user", []map[string]any{chatToolResultBlock(msg)})
		case "user", "":
			if blocks, ok := chatTextToAnthropicContent(msg.Content); ok {
				appendBlocks("user", blocks)
			}
		}
	}
	return strings.Join(systemParts, "\n\n"), out
}

// chatToolResultBlock 构造 role=tool 消息的 tool_result block。字符串 content
// 保持现状写为单个 text block；[]any parts 逐 part 映射进 content blocks
// （text→text、image_url→image，未知 part 跳过并记日志）；空内容写 "(empty)"
// （Anthropic 不接受空 tool_result content）。
func chatToolResultBlock(msg Message) map[string]any {
	content := []map[string]any{}
	switch c := msg.Content.(type) {
	case string:
		if c == "" {
			c = "(empty)"
		}
		content = append(content, map[string]any{"type": "text", "text": c})
	case []any:
		for _, part := range c {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch pm["type"] {
			case "text":
				content = append(content, map[string]any{"type": "text", "text": toString(pm["text"])})
			case "image_url":
				url, _ := pm["url"].(string)
				if url == "" {
					if iu, ok := pm["image_url"].(map[string]any); ok {
						url, _ = iu["url"].(string)
					}
				}
				if url == "" {
					slog.Debug("chat→anthropic tool_result: image part without url skipped", "tool_use_id", msg.ToolCallID)
					continue
				}
				if block := imageURLToAnthropicBlock(url); block != nil {
					content = append(content, block)
				}
			default:
				slog.Debug("chat→anthropic tool_result: unsupported part skipped", "part_type", pm["type"], "tool_use_id", msg.ToolCallID)
			}
		}
		if len(content) == 0 {
			// parts 全部无法映射（或本来就是空数组）：回填占位文本。
			content = append(content, map[string]any{"type": "text", "text": "(empty)"})
		}
	default:
		// 数值/对象等非标准 content：序列化为文本，保持现状（不丢数据）。
		if msg.Content != nil {
			text := ""
			if b, err := json.Marshal(msg.Content); err == nil {
				text = string(b)
			}
			content = append(content, map[string]any{"type": "text", "text": text})
		} else {
			content = append(content, map[string]any{"type": "text", "text": "(empty)"})
		}
	}
	return map[string]any{
		"type": "tool_result", "tool_use_id": msg.ToolCallID,
		"content": content,
	}
}

// parseToolCallArguments 把 Chat 工具调用 arguments JSON 解析为 Anthropic
// input 对象；空补 {}，坏 JSON 兜底 {"_raw": ...} 不丢数据。
func parseToolCallArguments(args string) map[string]any {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return map[string]any{}
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(trimmed), &input); err == nil {
		return input
	}
	return map[string]any{"_raw": args}
}

// resolveMaxTokens 统一 Chat→Anthropic / Chat→Responses 两个方向的 max tokens
// 口径：max_completion_tokens 优先于 max_tokens，两个键都可以从 typed
// OpenAIRequest（MaxTokens 字段）顶层或 ExtraBody / 原始请求体 map 顶层拿到
// （OpenAIRequest 没有 max_completion_tokens 字段，调用方须先 json.Unmarshal
// 入站 body 传入 rawBody）。均未设置（或 <=0）时回退 MaxTokensCapFor(modelID)，
// 仍无 cap 则兜底 8192。最终结果钳制到 [128, cap]（cap>0 且 <128 时以 cap 为准，
// 不再强制下限 —— 配置者显式限满时须尊重）。
func resolveMaxTokens(rawBody map[string]any, req *OpenAIRequest, modelID string) int {
	value := 0
	if rawBody != nil {
		if v, ok := intFromAny(rawBody["max_completion_tokens"]); ok && v > 0 {
			value = v
		}
	}
	if value <= 0 && req != nil {
		if v, ok := intFromAny(extraBodyValue(req, "max_completion_tokens")); ok && v > 0 {
			value = v
		}
	}
	if value <= 0 && rawBody != nil {
		if v, ok := intFromAny(rawBody["max_tokens"]); ok && v > 0 {
			value = v
		}
	}
	if value <= 0 && req != nil && req.MaxTokens != nil && *req.MaxTokens > 0 {
		value = *req.MaxTokens
	}
	tokenCap := 0
	if modelID != "" {
		tokenCap = config.MaxTokensCapFor(modelID)
	}
	if value <= 0 {
		if tokenCap > 0 {
			value = tokenCap
		} else {
			value = defaultAnthropicMaxTokens
		}
	}
	if tokenCap > 0 && value > tokenCap {
		value = tokenCap
	}
	if value < 128 && !(tokenCap > 0 && tokenCap < 128) {
		value = 128
	}
	return value
}

// intFromAny 宽松地把 JSON 数字 / int 值转为 int。
func intFromAny(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case int32:
		return int(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
		return 0, false
	default:
		return 0, false
	}
}

// chatToAnthropicBody 把 Chat Completions 请求转为 Anthropic Messages 请求体。
// rawBody 可选：传入调用方的原始请求体 map,resolveMaxTokens 用它读
// max_completion_tokens（类型化 OpenAIRequest 装不下的顶层字段）。
func chatToAnthropicBody(req *OpenAIRequest, modelID string) []byte {
	return chatToAnthropicBodyWithRaw(req, modelID, nil)
}

func chatToAnthropicBodyWithRaw(req *OpenAIRequest, modelID string, rawBody map[string]any) []byte {
	system, messages := chatMessagesToAnthropic(req.Messages)
	body := map[string]any{
		"model":    modelID,
		"messages": messages,
		"stream":   req.Stream,
	}
	if system != "" {
		body["system"] = system
	}
	maxTokens := resolveMaxTokens(rawBody, req, modelID)
	body["max_tokens"] = maxTokens
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if stop := extraBodyValue(req, "stop"); stop != nil {
		if arr, ok := stop.([]any); ok {
			var strs []string
			for _, v := range arr {
				if s, ok := v.(string); ok {
					strs = append(strs, s)
				}
			}
			if len(strs) > 0 {
				body["stop_sequences"] = strs
			}
		} else if s, ok := stop.(string); ok && s != "" {
			body["stop_sequences"] = []string{s}
		}
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			schema := t.Function.Parameters
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tool := map[string]any{
				"name":         t.Function.Name,
				"input_schema": schema,
			}
			if t.Function.Description != "" {
				tool["description"] = t.Function.Description
			}
			tools = append(tools, tool)
		}
		body["tools"] = tools
	}
	if req.ToolChoice != nil {
		if choice := chatToolChoiceToAnthropic(req.ToolChoice); choice != nil {
			body["tool_choice"] = choice
		}
	}
	// thinking：effort 映射为预算；ForceDisableThinking 或客户端显式禁用则省略。
	if !config.ForceDisableThinking() && !isThinkingDisabled(req.Thinking) {
		effort := req.ReasoningEffort
		if effort == "" {
			effort = reasoningEffortFromThinking(req.Thinking)
		}
		if effort != "" && effort != "none" {
			effort = mappedReasoningEffort(effort)
			if budget := effortToThinkingBudget(effort); budget > 0 {
				body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
			}
		}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return []byte(fmt.Sprintf(`{"model":%q,"messages":[],"max_tokens":%d,"stream":%t}`, modelID, maxTokens, req.Stream))
	}
	return b
}

// extraBodyValue 取 ExtraBody 中的顶层字段。
func extraBodyValue(req *OpenAIRequest, key string) any {
	if req.ExtraBody == nil {
		return nil
	}
	return req.ExtraBody[key]
}

// rawRequestBodyMap 从已解析的 OpenAIRequest 重建入站 body 的顶层键视图，
// 供 resolveMaxTokens 读取 max_completion_tokens 等类型化结构装不下的字段。
// 注：理想形态是解析调用方持有的原始 body 字节（chat.go 的
// readJSONRequestBody 读后未把字节留存在请求上下文里,改它超出本文件边界）,
// 这里用 ExtraBody+标准字段重建等价视图 —— ExtraBody 内的顶层键天然就位,
// req.MaxTokens 回填为 max_tokens。
func rawRequestBodyMap(req *OpenAIRequest) map[string]any {
	raw := map[string]any{}
	if req == nil {
		return raw
	}
	if req.MaxTokens != nil {
		raw["max_tokens"] = *req.MaxTokens
	}
	for k, v := range req.ExtraBody {
		raw[k] = v
	}
	return raw
}

// forwardChatViaAnthropic 处理规则命中 anthropic 的 Chat 入站请求：请求转为
// Anthropic Messages，响应按客户端流式偏好转换回 Chat 形状。上游 4xx/5xx
// 经 parseAnthropicErrorBody 转为 chat 错误形状写回。
func forwardChatViaAnthropic(w http.ResponseWriter, r *http.Request, auth UpstreamAuth, req *OpenAIRequest, keepReasoning bool) {
	ctx := r.Context()
	// 请求侧归一化前置：补全 assistant.tool_calls 的 tool 响应、text-only 模型
	// 降级多模态 content 为 "[image attached]" 文本。fixToolCallGaps /
	// modelIsTextOnly / downgradeMultimodalContent 定义在 chat.go（Worker B
	// 所有），这里只调用不修改。
	req.Messages = fixToolCallGaps(req.Messages)
	textOnly := modelIsTextOnly(req.Model)
	for i := range req.Messages {
		if parts, ok := req.Messages[i].Content.([]any); ok {
			req.Messages[i].Content = downgradeMultimodalContent(parts, textOnly)
		}
	}
	rawBody := rawRequestBodyMap(req)
	upstreamBody := chatToAnthropicBodyWithRaw(req, req.Model, rawBody)
	log := logging.FromContext(ctx)
	log.Info("chat via anthropic upstream",
		"model", req.Model, "stream", req.Stream, "keep_reasoning", keepReasoning)

	if req.Stream {
		rc, status, _, err := callOpenCodeAnthropicEndpoint(ctx, upstreamBody, req.Model, auth)
		if err != nil || status < 200 || status >= 300 {
			var errBody []byte
			if rc != nil {
				errBody, _ = io.ReadAll(io.LimitReader(rc, 64*1024))
				rc.Close()
			}
			if status < 100 || status >= 600 {
				status = http.StatusBadGateway
			}
			if len(errBody) > 0 {
				if ape, ok := parseAnthropicErrorBody(errBody); ok {
					writeUpstreamError(w, status, ape, "chat")
					return
				}
				// 上游错误体非 Anthropic 形状：原样透传，保真状态码。
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				w.Write(errBody)
				return
			}
			writeUpstreamError(w, status, fmt.Errorf("upstream error"), "chat")
			return
		}
		defer rc.Close()
		anthropicSSEToChatStream(ctx, w, rc, req.Model, keepReasoning, true)
		return
	}

	rc, status, _, err := callOpenCodeAnthropicEndpoint(ctx, upstreamBody, req.Model, auth)
	if err != nil || status < 200 || status >= 300 {
		var errBody []byte
		if rc != nil {
			errBody, _ = io.ReadAll(io.LimitReader(rc, 64*1024))
			rc.Close()
		}
		if status < 100 || status >= 600 {
			status = http.StatusBadGateway
		}
		if len(errBody) > 0 {
			if ape, ok := parseAnthropicErrorBody(errBody); ok {
				writeUpstreamError(w, status, ape, "chat")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			w.Write(errBody)
			return
		}
		writeUpstreamError(w, status, fmt.Errorf("upstream error"), "chat")
		return
	}
	defer rc.Close()

	respBody, readErr := io.ReadAll(io.LimitReader(rc, 32*1024*1024))
	if readErr != nil {
		writeUpstreamError(w, http.StatusBadGateway, fmt.Errorf("upstream read error"), "chat")
		return
	}
	// Anthropic message（或 SSE 缓冲）→ Chat，复用既有回归转换器。
	outBody, convErr := convertAnthropicToOpenAI(respBody, req.Model)
	if convErr != nil {
		writeUpstreamError(w, http.StatusBadGateway, convErr, "chat")
		return
	}
	if cleaned, err := convertResponse(outBody, keepReasoning); err == nil {
		outBody = cleaned
	}
	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			statsx.RecordChatUsage(req.Model, anthropicUsageToChat(u))
		}
	}
	result := logging.SummarizeChatResult(outBody)
	logging.LogResult(ctx, result)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(outBody)
}

// ======================== Anthropic SSE → Chat SSE 状态机 ========================

type anthropicToChatState struct {
	w             http.ResponseWriter
	flusher       http.Flusher
	stats         *logging.StreamStats
	id            string
	model         string
	keepReasoning bool
	includeUsage  bool
	sentRole      bool
	blocks        map[int]string // anthropic block index → "text"|"thinking"|"tool_use"
	toolIndices   map[int]int    // anthropic block index → chat tool_calls index
	// 每个打开中的 tool_use block 的元数据。initialInput 缓存 start 块附带的
	// 初始 input;sawInputDelta 记录是否已有 input_json_delta —— 两者互斥,
	// stop 时若 sawInputDelta=false 且 initialInput 非空,需要兜底 emit 一次,
	// 否则该 tool call 的 input 会整体丢失(上游偶发场景)。
	toolStates map[int]*anthropicToolState
	toolCount  int
	stopReason string
	fullUsage  map[string]any
	// reasoningTokens 由 thinking_delta 的字符数粗计(tokens≈字符/4)，仅当上游
	// Anthropic usage 未提供 output_tokens_details.thinking_tokens 时兜底填
	// completion_tokens_details.reasoning_tokens（上游精确值覆盖本近似）。
	reasoningTokens int
	skippedSig      int
	skippedRedacted int
	finalized       bool // finalize 幂等开关:message_stop 与 EOF 路径共用
}

type anthropicToolState struct {
	sawInputDelta bool
	initialInput  any // string 或 map[string]any;空/nil 表示没有
}

// 保留提供给 chat 端 arguments 的初值;在 content_block_stop 消费。
func (t *anthropicToolState) initialArguments() string {
	switch v := t.initialInput.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
		return ""
	}
}

func anthropicSSEToChatStream(ctx context.Context, w http.ResponseWriter, rc io.Reader, model string, keepReasoning bool, includeUsage bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	st := &anthropicToChatState{
		w:             w,
		flusher:       flusher,
		stats:         &logging.StreamStats{Start: time.Now()},
		id:            "chatcmpl-" + randomHex(12),
		model:         model,
		keepReasoning: keepReasoning,
		includeUsage:  includeUsage,
		blocks:        map[int]string{},
		toolIndices:   map[int]int{},
		toolStates:    map[int]*anthropicToolState{},
		fullUsage:     map[string]any{},
	}
	defer func() {
		st.stats.ToolCallCount = st.toolCount
		if len(st.fullUsage) > 0 {
			statsx.RecordChatUsage(model, anthropicUsageToChat(st.fullUsage))
		}
		st.stats.Log(ctx, "chat")
	}()

	reader := newStreamReader(ctx, rc, 0)
	defer reader.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case result := <-reader.Read():
			pendingErr := result.err
			if result.line != "" {
				st.stats.NoteChunk()
				st.handleLine(result.line)
			}
			if pendingErr != nil {
				// EOF / 读错误兜底:message_stop 未到达时补 finish+usage 终块
				// +[DONE],保证 OpenAI SDK 不挂起（幂等）。
				st.finalize()
				if st.skippedSig > 0 || st.skippedRedacted > 0 {
					slog.Debug("chat stream: dropped unrepresentable anthropic deltas",
						"model", model, "signature_delta", st.skippedSig, "redacted_thinking", st.skippedRedacted)
				}
				return
			}
		}
	}
}

// finalize 幂等地结束 chat 流：补发 finish chunk（stopReason 缺省 "stop"）、
// includeUsage 时的 usage 终块与 [DONE]。message_stop 正常路径与 EOF 兜底
// 路径共用。
func (st *anthropicToChatState) finalize() {
	if st.finalized {
		return
	}
	st.finalized = true
	if st.stopReason == "" {
		st.stopReason = "stop"
	}
	st.emitChunk(map[string]any{}, st.stopReason, nil)
	if st.includeUsage && len(st.fullUsage) > 0 {
		st.emitChunk(map[string]any{}, "", st.chatUsage())
	}
	st.w.Write([]byte("data: [DONE]\n\n"))
	if st.flusher != nil {
		st.flusher.Flush()
	}
	st.stats.DoneSeen = true
	st.stats.SawFinish = true
	st.stats.FinishReason = st.stopReason
}

// chatUsage 返回发给 chat 客户端的 usage：Anthropic 侧未携带
// output_tokens_details.thinking_tokens 时,用 thinking_delta 字符计数估算
// reasoning_tokens 兜底（粗略 tokens≈chars/4,仅无精确值时启用）。
func (st *anthropicToChatState) chatUsage() map[string]any {
	usage := anthropicUsageToChat(st.fullUsage)
	if usage == nil {
		return nil
	}
	if st.reasoningTokens > 0 {
		details, _ := usage["completion_tokens_details"].(map[string]any)
		if details == nil {
			details = map[string]any{}
		}
		if v, ok := numberAsFloat(details["reasoning_tokens"]); !ok || v <= 0 {
			est := st.reasoningTokens / 4
			if est < 1 {
				est = 1
			}
			details["reasoning_tokens"] = est
		}
		if len(details) > 0 {
			usage["completion_tokens_details"] = details
		}
	}
	return usage
}

// emitChunk 写出一个 Chat SSE chunk。delta 为 nil 时用 {}；finishReason 非空时
// 附带 finish_reason；usage 非空时附带 usage（仅终块）。
func (st *anthropicToChatState) emitChunk(delta map[string]any, finishReason string, usage map[string]any) {
	chunk := map[string]any{
		"id":      st.id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   st.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReasonOr(finishReason),
		}},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	st.w.Write([]byte("data: " + string(b) + "\n\n"))
	if st.flusher != nil {
		st.flusher.Flush()
	}
}

func finishReasonOr(fr string) any {
	if fr == "" {
		return nil
	}
	return fr
}

// handleLine 处理一行上游 Anthropic SSE。
func (st *anthropicToChatState) handleLine(line string) {
	payload, ok := strings.CutPrefix(line, "data: ")
	if !ok {
		return
	}
	var evt map[string]any
	if json.Unmarshal([]byte(payload), &evt) != nil {
		return
	}
	switch typ, _ := evt["type"].(string); typ {
	case "message_start":
		if msg, ok := evt["message"].(map[string]any); ok {
			if u, ok := msg["usage"].(map[string]any); ok {
				mergeUsage(st.fullUsage, u)
			}
			if m, _ := msg["model"].(string); m != "" {
				st.model = m
			}
		}
		if !st.sentRole {
			st.sentRole = true
			st.emitChunk(map[string]any{"role": "assistant", "content": ""}, "", nil)
		}
	case "content_block_start":
		idx := numberToInt(evt["index"])
		cb, _ := evt["content_block"].(map[string]any)
		bt, _ := cb["type"].(string)
		st.blocks[idx] = bt
		if bt == "text" {
			// start 事件直接携带初始非空 text 时预 emit 一次 text_delta,
			// 否则这段起始内容会丢失（上游通常随后再发 text_delta,但
			// 携带完整文本的 content_block_start 是合法的）。
			if t, _ := cb["text"].(string); t != "" {
				st.emitChunk(map[string]any{"content": t}, "", nil)
			}
		}
		if bt == "tool_use" {
			toolIdx := st.toolCount
			st.toolCount++
			st.toolIndices[idx] = toolIdx
			tool := &anthropicToolState{}
			st.toolStates[idx] = tool
			name, _ := cb["name"].(string)
			id, _ := cb["id"].(string)
			// 缓存 start 块的 initial input(常见 {});不要立刻 emit 给 chat 端
			// —— OpenAI 客户端会 concat 所有 arguments 片段,若 start 下发了
			// initial,后续 partial_json 会拼出非法 JSON。stop 时兜底 emit:
			// !sawInputDelta 时优先 initial,initial 为空则补 "{}"。
			if raw, ok := cb["input"]; ok && raw != nil {
				if s, ok := raw.(string); ok && s != "" && s != "{}" {
					tool.initialInput = s
				} else if m, ok := raw.(map[string]any); ok && len(m) > 0 {
					tool.initialInput = m
				}
			}
			st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": toolIdx, "id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": ""},
			}}}, "", nil)
		}
	case "content_block_delta":
		idx := numberToInt(evt["index"])
		d, _ := evt["delta"].(map[string]any)
		dt, _ := d["type"].(string)
		switch dt {
		case "text_delta":
			if t, _ := d["text"].(string); t != "" {
				st.emitChunk(map[string]any{"content": t}, "", nil)
			}
		case "thinking_delta":
			if t, _ := d["thinking"].(string); t != "" {
				// 粗略计数 reasoning tokens(字符/4),仅在上游 usage 缺
				// thinking_tokens 时作 completion_tokens_details 兜底。
				st.reasoningTokens += len(t)
				if st.keepReasoning {
					st.emitChunk(map[string]any{"reasoning_content": t}, "", nil)
				}
			}
		case "input_json_delta":
			if toolIdx, ok := st.toolIndices[idx]; ok {
				if tool, ok := st.toolStates[idx]; ok {
					tool.sawInputDelta = true
				}
				if pj, _ := d["partial_json"].(string); pj != "" {
					st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
						"index": toolIdx, "id": nil, "type": "function",
						"function": map[string]any{"name": "", "arguments": pj},
					}}}, "", nil)
				}
			}
		case "signature_delta":
			// OpenAI chat 无对应字段（签名只在同协议 roundtrip 有意义）,
			// 显式跳过并计数,不拼进 reasoning_content。
			st.skippedSig++
		case "redacted_thinking":
			st.skippedRedacted++
		}
	case "content_block_stop":
		idx := numberToInt(evt["index"])
		// 若 tool_use 全程没收到 input_json_delta,在 stop 时兜底 emit 一次:
		// 有 initial input 用 initial,否则补 "{}"。OpenAI 客户端 concat 各
		// chunk 的 arguments 后须得到合法 JSON —— 空串会让 json.Unmarshal
		// 失败;"{}" 对齐 buildOpenAIResponse 对 input nil/{} 的非流式语义。
		if tool, ok := st.toolStates[idx]; ok && !tool.sawInputDelta {
			if toolIdx, ok := st.toolIndices[idx]; ok {
				s := tool.initialArguments()
				if s == "" {
					s = "{}"
				}
				st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
					"index": toolIdx, "id": nil, "type": "function",
					"function": map[string]any{"name": "", "arguments": s},
				}}}, "", nil)
			}
		}
		delete(st.blocks, idx)
		delete(st.toolIndices, idx)
		delete(st.toolStates, idx)
	case "message_delta":
		if delta, ok := evt["delta"].(map[string]any); ok {
			if sr, _ := delta["stop_reason"].(string); sr != "" {
				st.stopReason = normalizeFinishReason(sr)
			}
		}
		if u, ok := evt["usage"].(map[string]any); ok {
			mergeUsage(st.fullUsage, u)
		}
	case "message_stop":
		// 正常终止路径与 EOF 兜底共用 finalize（幂等）。
		st.finalize()
	case "error":
		em, _ := evt["error"].(map[string]any)
		msg := "upstream error"
		if m, _ := em["message"].(string); m != "" {
			msg = m
		}
		st.w.Write([]byte("data: " + `{"error":{"message":` + jsonString(msg) + `}}` + "\n\n"))
		if st.flusher != nil {
			st.flusher.Flush()
		}
	}
}

// numberToInt 宽松地把 any 数字转为 int（SSE JSON 解码后多为 float64）。
func numberToInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}

// jsonString 由 count_tokens.go 提供（Worker D 所有），这里复用。
