package bridge

import (
	"encoding/json"
	"fmt"
	"strings"
)

// This file holds the pure Chat Completions -> Anthropic Messages request-body
// construction moved out of internal/app/chat_to_anthropic.go. None of these
// functions perform I/O, logging, metrics, or config reads; the max_tokens cap
// and force-disable-thinking flag arrive via ConfigView. The app layer keeps
// same-name lowercase forwarding shells so existing *_test.go files compile
// unchanged, and re-emits the dropped-part debug logs the pure layer reports.

// DefaultAnthropicMaxTokens 是 Chat 入站缺省 max_tokens 时的兜底值。
// Anthropic Messages 上游把 max_tokens 视为必填。
const DefaultAnthropicMaxTokens = 8192

// DroppedPart describes one non-convertible multimodal / content part that the
// Chat -> Anthropic converter skipped. The pure layer records these instead of
// logging; the app shell turns each into the original slog.Debug line so the
// observable log stream is unchanged.
type DroppedPart struct {
	// Reason is the original debug message text (e.g. "image_url part without
	// url skipped"). It is the full human-readable message; the app prepends the
	// converter scope ("chat→anthropic"...) when logging.
	Reason string
	// PartType is the content part's "type" value when relevant ("" otherwise).
	PartType string
	// ToolUseID is the owning tool_use id for tool_result parts ("" otherwise).
	ToolUseID string
	// Prefix is the data-URI metadata prefix for the dropped data-URI case.
	Prefix string
}

// ChatToolChoiceToAnthropic 把 Chat tool_choice 转为 Anthropic 形状。
// (Moved from app/chat_to_anthropic.go chatToolChoiceToAnthropic.)
func ChatToolChoiceToAnthropic(choice any) any {
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

// ChatTextToAnthropicContent 把字符串或 Chat 多模态 content 数组转为
// Anthropic content block 数组。图片按 data URI / URL 归类为 base64/url source；
// 无法解析的 part 丢弃（计数经 dropped 返回，app 记 debug 日志），全部丢光时
// 回填 [image attached] 文本，保证多模态消息永远有内容（Anthropic 不接受空
// content 数组）。
// (Moved from app/chat_to_anthropic.go chatTextToAnthropicContent.)
func ChatTextToAnthropicContent(content any) (blocks []map[string]any, ok bool, dropped []DroppedPart) {
	switch v := content.(type) {
	case string:
		if v == "" {
			return nil, false, nil
		}
		return []map[string]any{{"type": "text", "text": v}}, true, nil
	case []any:
		var out []map[string]any
		droppedImage := false
		for _, part := range v {
			pm, isMap := part.(map[string]any)
			if !isMap {
				continue
			}
			switch pm["type"] {
			case "text":
				if t, _ := pm["text"].(string); t != "" {
					out = append(out, map[string]any{"type": "text", "text": t})
				}
			case "image_url":
				url, _ := pm["url"].(string)
				if url == "" {
					if iu, iuok := pm["image_url"].(map[string]any); iuok {
						url, _ = iu["url"].(string)
					}
				}
				if url == "" {
					dropped = append(dropped, DroppedPart{Reason: "image_url part without url skipped"})
					droppedImage = true
					continue
				}
				if block, d := ImageURLToAnthropicBlock(url); block != nil {
					out = append(out, block)
				} else {
					if d.Reason != "" {
						dropped = append(dropped, d)
					}
					droppedImage = true
				}
			default:
				dropped = append(dropped, DroppedPart{Reason: "unsupported content part skipped", PartType: ToString(pm["type"])})
			}
		}
		if len(out) == 0 && droppedImage {
			// 多模态消息的图片全部无法转换时保留一个占位文本,避免用户消息
			// 内容整体消失（对齐 text-only 降级的 MultimodalAttachedLabel）。
			out = append(out, map[string]any{"type": "text", "text": MultimodalAttachedLabel})
		}
		return out, len(out) > 0, dropped
	default:
		return nil, false, nil
	}
}

// ImageURLToAnthropicBlock 把 data URI 或 http(s) URL 转为 Anthropic image block。
// data URI 缺 ";base64," 或无法解析媒体类型时丢弃该 part（返回 nil + dropped
// 事件，app 记日志），不再把 data URI 塞进 url source 兜底。钝 media 类型
// （无 "/"）回退 image/png。
// (Moved from app/chat_to_anthropic.go imageURLToAnthropicBlock.)
func ImageURLToAnthropicBlock(url string) (map[string]any, DroppedPart) {
	if after, ok := strings.CutPrefix(url, "data:"); ok {
		meta, b64, ok := strings.Cut(after, ";base64,")
		if !ok {
			return nil, DroppedPart{Reason: "data URI without ;base64, marker dropped", Prefix: meta}
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
		}, DroppedPart{}
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type": "url", "url": url,
		},
	}, DroppedPart{}
}

// ChatToAnthropicBody 把 Chat Completions 请求转为 Anthropic Messages 请求体
// 字节。rawBody 为 nil。dropped 汇总被丢弃 part 的事件供 app 记日志；失败时
// 返回最小可用体字节（与原 fallback 一致）。
// (Moved from app/chat_to_anthropic.go chatToAnthropicBody.)
func ChatToAnthropicBody(cv ConfigView, req *OpenAIRequest, modelID string, maxTokensCap int) ([]byte, []DroppedPart) {
	return ChatToAnthropicBodyWithRaw(cv, req, modelID, maxTokensCap, nil)
}

// ChatToAnthropicBodyWithRaw 把 Chat Completions 请求转为 Anthropic Messages
// 请求体。modelID 作为 body["model"]（原实现用形参而非 req.Model，二者在调用
// 点相同）。rawBody 可选：传入调用方的原始请求体 map,ResolveMaxTokens 用它读
// max_completion_tokens（类型化 OpenAIRequest 装不下的顶层字段）。dropped 汇总
// 被丢弃 part 的事件供 app 记日志；序列化失败返回最小可用体（保持原 fallback
// 字节完全一致）。
// (Moved from app/chat_to_anthropic.go chatToAnthropicBodyWithRaw.)
func ChatToAnthropicBodyWithRaw(cv ConfigView, req *OpenAIRequest, modelID string, maxTokensCap int, rawBody map[string]any) ([]byte, []DroppedPart) {
	system, messages, dropped := ChatMessagesToAnthropic(req.Messages)
	body := map[string]any{
		"model":    modelID,
		"messages": messages,
		"stream":   req.Stream,
	}
	if system != "" {
		body["system"] = system
	}
	maxTokens := ResolveMaxTokens(rawBody, req, maxTokensCap)
	body["max_tokens"] = maxTokens
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if stop := ExtraBodyValue(req, "stop"); stop != nil {
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
		if choice := ChatToolChoiceToAnthropic(req.ToolChoice); choice != nil {
			body["tool_choice"] = choice
		}
	}
	// thinking：effort 映射为预算；ForceDisableThinking 或客户端显式禁用则省略。
	if !cv.ForceDisableThinking && !IsThinkingDisabled(req.Thinking) {
		effort := req.ReasoningEffort
		if effort == "" {
			effort = ReasoningEffortFromThinking(req.Thinking)
		}
		if effort != "" && effort != "none" {
			effort = MappedReasoningEffort(cv.ReasoningEffortMap, effort)
			if budget := EffortToThinkingBudget(effort); budget > 0 {
				body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
			}
		}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return []byte(fmt.Sprintf(`{"model":%q,"messages":[],"max_tokens":%d,"stream":%t}`, modelID, maxTokens, req.Stream)), dropped
	}
	return b, dropped
}

// ChatMessagesToAnthropic 把 Chat messages 转为 Anthropic system + messages。
// system/developer 角色提取为 system（多条 "\n\n" 连接）；role=tool 转为
// tool_result user 消息；assistant tool_calls 内联为 tool_use block。
// 返回的 messages 已做连续同角色合并（Anthropic 要求交替）。dropped 汇总所有
// 被丢弃 part 的事件供 app 记日志。
// (Moved from app/chat_to_anthropic.go chatMessagesToAnthropic.)
func ChatMessagesToAnthropic(messages []Message) (system string, out []map[string]any, dropped []DroppedPart) {
	var systemParts []string
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
			if blocks, ok, d := ChatTextToAnthropicContent(msg.Content); ok {
				dropped = append(dropped, d...)
				for _, b := range blocks {
					if t, _ := b["text"].(string); t != "" {
						systemParts = append(systemParts, t)
					}
				}
			} else {
				dropped = append(dropped, d...)
			}
		case "assistant":
			var blocks []map[string]any
			if bs, ok, d := ChatTextToAnthropicContent(msg.Content); ok {
				dropped = append(dropped, d...)
				blocks = append(blocks, bs...)
			} else {
				dropped = append(dropped, d...)
			}
			for _, tc := range msg.ToolCalls {
				input := ParseToolCallArguments(tc.Function.Arguments)
				blocks = append(blocks, map[string]any{
					"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
				})
			}
			appendBlocks("assistant", blocks)
		case "tool":
			block, d := ChatToolResultBlock(msg)
			dropped = append(dropped, d...)
			appendBlocks("user", []map[string]any{block})
		case "user", "":
			if blocks, ok, d := ChatTextToAnthropicContent(msg.Content); ok {
				dropped = append(dropped, d...)
				appendBlocks("user", blocks)
			} else {
				dropped = append(dropped, d...)
			}
		}
	}
	return strings.Join(systemParts, "\n\n"), out, dropped
}

// ChatToolResultBlock 构造 role=tool 消息的 tool_result block。字符串 content
// 保持现状写为单个 text block；[]any parts 逐 part 映射进 content blocks
// （text→text、image_url→image，未知 part 跳过并经 dropped 返回由 app 记
// 日志）；空内容写 "(empty)"（Anthropic 不接受空 tool_result content）。
// (Moved from app/chat_to_anthropic.go chatToolResultBlock.)
func ChatToolResultBlock(msg Message) (map[string]any, []DroppedPart) {
	var dropped []DroppedPart
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
				content = append(content, map[string]any{"type": "text", "text": ToString(pm["text"])})
			case "image_url":
				url, _ := pm["url"].(string)
				if url == "" {
					if iu, ok := pm["image_url"].(map[string]any); ok {
						url, _ = iu["url"].(string)
					}
				}
				if url == "" {
					dropped = append(dropped, DroppedPart{Reason: "tool_result: image part without url skipped", ToolUseID: msg.ToolCallID})
					continue
				}
				if block, d := ImageURLToAnthropicBlock(url); block != nil {
					content = append(content, block)
				} else if d.Reason != "" {
					d.ToolUseID = msg.ToolCallID
					dropped = append(dropped, d)
				}
			default:
				dropped = append(dropped, DroppedPart{Reason: "tool_result: unsupported part skipped", PartType: ToString(pm["type"]), ToolUseID: msg.ToolCallID})
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
	}, dropped
}

// ResolveMaxTokens 统一 Chat→Anthropic / Chat→Responses 两个方向的 max tokens
// 口径：max_completion_tokens 优先于 max_tokens，两个键都可以从 typed
// OpenAIRequest（MaxTokens 字段）顶层或 ExtraBody / 原始请求体 map 顶层拿到
// （OpenAIRequest 没有 max_completion_tokens 字段，调用方须先 json.Unmarshal
// 入站 body 传入 rawBody）。均未设置（或 <=0）时回退 maxTokensCap（config
// MaxTokensCapFor(modelID)），仍无 cap 则兜底 8192。最终结果钳制到 [128, cap]
// （cap>0 且 <128 时以 cap 为准，不再强制下限 —— 配置者显式限满时须尊重）。
// (Moved from app/chat_to_anthropic.go resolveMaxTokens.)
func ResolveMaxTokens(rawBody map[string]any, req *OpenAIRequest, maxTokensCap int) int {
	value := 0
	if rawBody != nil {
		if v, ok := IntFromAny(rawBody["max_completion_tokens"]); ok && v > 0 {
			value = v
		}
	}
	if value <= 0 && req != nil {
		if v, ok := IntFromAny(ExtraBodyValue(req, "max_completion_tokens")); ok && v > 0 {
			value = v
		}
	}
	if value <= 0 && rawBody != nil {
		if v, ok := IntFromAny(rawBody["max_tokens"]); ok && v > 0 {
			value = v
		}
	}
	if value <= 0 && req != nil && req.MaxTokens != nil && *req.MaxTokens > 0 {
		value = *req.MaxTokens
	}
	// tokenCap 已由调用方按 modelID 解析（modelID=="" 时调用方传 0，与
	// 原 config.MaxTokensCapFor 的 modelID!="" 守卫一致）。
	tokenCap := maxTokensCap
	if value <= 0 {
		if tokenCap > 0 {
			value = tokenCap
		} else {
			value = DefaultAnthropicMaxTokens
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

// IntFromAny 宽松地把 JSON 数字 / int 值转为 int。
// (Moved from app/chat_to_anthropic.go intFromAny.)
func IntFromAny(v any) (int, bool) {
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

// ExtraBodyValue 取 ExtraBody 中的顶层字段。
// (Moved from app/chat_to_anthropic.go extraBodyValue.)
func ExtraBodyValue(req *OpenAIRequest, key string) any {
	if req.ExtraBody == nil {
		return nil
	}
	return req.ExtraBody[key]
}

// RawRequestBodyMap 从已解析的 OpenAIRequest 重建入站 body 的顶层键视图，
// 供 ResolveMaxTokens 读取 max_completion_tokens 等类型化结构装不下的字段。
// 注：理想形态是解析调用方持有的原始 body 字节（chat.go 的
// readJSONRequestBody 读后未把字节留存在请求上下文里,改它超出本文件边界）,
// 这里用 ExtraBody+标准字段重建等价视图 —— ExtraBody 内的顶层键天然就位,
// req.MaxTokens 回填为 max_tokens。
// (Moved from app/chat_to_anthropic.go rawRequestBodyMap.)
func RawRequestBodyMap(req *OpenAIRequest) map[string]any {
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
