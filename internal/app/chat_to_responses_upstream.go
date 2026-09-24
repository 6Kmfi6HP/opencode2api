package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/6Kmfi6HP/opencode2api/internal/config"
	"github.com/6Kmfi6HP/opencode2api/internal/logging"
	statsx "github.com/6Kmfi6HP/opencode2api/internal/stats"
)

// ======================== /v1/chat/completions → Responses 上游 ========================

// chatMessagesToResponsesInput 把 Chat messages 转为 Responses instructions +
// input 数组。形状对齐 claudeMessagesToResponsesInput：system→instructions；
// user 文本→message/input_text；image_url→input_image；assistant.tool_calls→
// function_call；role=tool→function_call_output；assistant 文本→message/output_text。
func chatMessagesToResponsesInput(messages []Message) (string, []any) {
	var instructions string
	var input []any
	var systemParts []string
	for _, msg := range messages {
		switch msg.Role {
		case "system", "developer":
			if s, ok := msg.Content.(string); ok && s != "" {
				systemParts = append(systemParts, s)
			}
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				for _, tc := range msg.ToolCalls {
					item := map[string]any{
						"type":      "function_call",
						"call_id":   tc.ID,
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					}
					if tc.ID == "" {
						item["call_id"] = "call_" + randomHex(12)
					}
					if item["arguments"] == "" {
						item["arguments"] = "{}"
					}
					input = append(input, item)
				}
			}
			if s, ok := msg.Content.(string); ok && s != "" {
				input = append(input, map[string]any{
					"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": s}},
				})
			}
		case "tool":
			output := ""
			if s, ok := msg.Content.(string); ok {
				output = s
			} else if msg.Content != nil {
				if b, err := json.Marshal(msg.Content); err == nil {
					output = string(b)
				}
			}
			callID := msg.ToolCallID
			if callID == "" {
				callID = "call_" + randomHex(12)
			}
			input = append(input, map[string]any{
				"type": "function_call_output", "call_id": callID, "output": output,
			})
		default: // user / ""
			switch c := msg.Content.(type) {
			case string:
				if c != "" {
					input = append(input, map[string]any{
						"type": "message", "role": "user",
						"content": []any{map[string]any{"type": "input_text", "text": c}},
					})
				}
			case []any:
				var parts []any
				for _, p := range c {
					pm, ok := p.(map[string]any)
					if !ok {
						continue
					}
					switch pm["type"] {
					case "text":
						if t, _ := pm["text"].(string); t != "" {
							parts = append(parts, map[string]any{"type": "input_text", "text": t})
						}
					case "image_url":
						url, _ := pm["url"].(string)
						if url == "" {
							if iu, ok := pm["image_url"].(map[string]any); ok {
								url, _ = iu["url"].(string)
							}
						}
						if url != "" {
							parts = append(parts, map[string]any{
								"type": "input_image", "image_url": url,
							})
						}
					}
				}
				if len(parts) > 0 {
					input = append(input, map[string]any{
						"type": "message", "role": "user", "content": parts,
					})
				}
			}
		}
	}
	instructions = strings.Join(systemParts, "\n\n")
	return instructions, input
}

// chatToResponsesBody 把 Chat Completions 请求转为 Responses 请求体。
// rawBody 可选：同 chatToAnthropicBodyWithRaw,供 resolveMaxTokens 读
// max_completion_tokens 顶层字段。
func chatToResponsesBody(req *OpenAIRequest, modelID string) []byte {
	return chatToResponsesBodyWithRaw(req, modelID, nil)
}

func chatToResponsesBodyWithRaw(req *OpenAIRequest, modelID string, rawBody map[string]any) []byte {
	instructions, input := chatMessagesToResponsesInput(req.Messages)
	body := map[string]any{
		"model":  modelID,
		"input":  input,
		"stream": req.Stream,
		// 对零会话上游不写存储：对齐 sub2api 对无状态上游的默认行为。
		"store": false,
	}
	if instructions != "" {
		body["instructions"] = instructions
	}
	if req.Stream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	// max_output_tokens 统一经 resolveMaxTokens（与 chat→anthropic 同口径）：
	// 未显式设置时不再"无 cap 就缺省",而是兜底 8192;且钳制下限 128。
	body["max_output_tokens"] = resolveMaxTokens(rawBody, req, modelID)
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	// 注意：OpenAI Responses API 没有 stop 字段,Chat 侧入站的 stop 不向
	// 该请求体透传（透传既被上游忽略也是 spec 违例;Chat→Anthropic 方向的
	// stop_sequences 保留）。
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tool := map[string]any{
				"type": "function", "name": t.Function.Name,
			}
			if t.Function.Description != "" {
				tool["description"] = t.Function.Description
			}
			if t.Function.Parameters != nil {
				tool["parameters"] = t.Function.Parameters
			}
			tools = append(tools, tool)
		}
		body["tools"] = tools
	}
	if req.ToolChoice != nil {
		body["tool_choice"] = claudeToolChoiceCore(req.ToolChoice, false)
	}
	// reasoning：effort 映射；none/禁用省略。
	if !config.ForceDisableThinking() && !isThinkingDisabled(req.Thinking) {
		effort := req.ReasoningEffort
		if effort == "" {
			effort = reasoningEffortFromThinking(req.Thinking)
		}
		if effort != "" && effort != "none" {
			if r := responsesReasoningBody(mappedReasoningEffort(effort), modelID); r != nil {
				body["reasoning"] = r
			}
		}
	}
	// 客户端 include（从 ExtraBody / rawBody 顶层）先落入 body,再与
	// reasoning.encrypted_content 合并去重;非法形状忽略。
	if inc, ok := rawBody["include"].([]any); ok {
		body["include"] = inc
	} else if inc, ok := extraBodyValue(req, "include").([]any); ok {
		body["include"] = inc
	}
	// include 合并 reasoning.encrypted_content（客户端已给 include 数组时
	// 去重追加,否则新建数组）——为未来 signature roundtrip 做准备。当前
	// 本方向响应侧还不读 encrypted_content（槽位由 Worker C 在响应侧加）。
	mergeResponsesIncludeKey(body, "reasoning.encrypted_content")
	b, err := json.Marshal(body)
	if err != nil {
		return []byte(fmt.Sprintf(`{"model":%q,"input":[],"stream":%t}`, modelID, req.Stream))
	}
	return b
}

// responsesReasoningBody 构造上行 Responses 请求体的 reasoning 字段。
// 只带 effort 时上游（muse-spark 系实测如此）静默思考、summary 恒为空数组，
// 可见思考必须有 reasoning.summary；上行该字段只负责"请求 summary"，敏感
// 原文仍由 include reasoning.encrypted_content 的加密通道承载，可见 summary
// 由上游自行生成，因此对所有原生 responses 上游统一请求 summary:auto，
// 支持 / 忽略都不破坏请求。muse-spark 的原生 responses 对 effort 有白名单
// 校验，未归一化的取值在此收口（与透传路径同名归一化对齐，防 400）。
// effort 归一化后为空时返回 nil（调用方省略 reasoning 字段）。
func responsesReasoningBody(effort, modelID string) map[string]any {
	if isMuseSparkModel(modelID) {
		effort = normalizeResponsesEffort(effort)
	}
	if effort == "" {
		return nil
	}
	return map[string]any{"effort": effort, "summary": "auto"}
}

// mergeResponsesIncludeKey 把 key 合并进 Responses 请求体的顶层 include
// 数组：存在则去重追加,不存在则新建。非法形状（非数组）视为不存在。
func mergeResponsesIncludeKey(body map[string]any, key string) {
	if key == "" {
		return
	}
	existing, _ := body["include"].([]any)
	for _, e := range existing {
		if s, _ := e.(string); s == key {
			body["include"] = existing
			return
		}
	}
	body["include"] = append(existing, key)
}

// forwardChatViaResponses 处理规则命中 responses 的 Chat 入站请求：请求转
// Responses，响应转回 Chat 形状（流式 SSE→SSE / 非流式 JSON→JSON）。
func forwardChatViaResponses(w http.ResponseWriter, r *http.Request, auth UpstreamAuth, req *OpenAIRequest, keepReasoning bool) {
	ctx := r.Context()
	// 请求侧归一化前置：补全 assistant.tool_calls 的 tool 响应（fixToolCallGaps
	// 定义在 chat.go,Worker B 所有;这里只调用不修改）。
	req.Messages = fixToolCallGaps(req.Messages)
	upstreamBody := chatToResponsesBodyWithRaw(req, req.Model, rawRequestBodyMap(req))
	log := logging.FromContext(ctx)
	log.Info("chat via responses upstream", "model", req.Model, "stream", req.Stream, "keep_reasoning", keepReasoning)

	if req.Stream {
		rc, status, _, err := callOpenCodeEndpoint(ctx, "responses", upstreamBody, req.Model, auth)
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
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				w.Write(errBody)
				return
			}
			writeUpstreamError(w, status, fmt.Errorf("upstream error"), "chat")
			return
		}
		defer rc.Close()
		responsesSSEToChatStream(ctx, w, rc, req.Model, keepReasoning, true)
		return
	}

	rc, status, _, err := callOpenCodeEndpoint(ctx, "responses", upstreamBody, req.Model, auth)
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
	// 免费层强制 stream:true 后，Responses 上游回的是 SSE;非流式 chat 客户端
	// 需要单个 chat.completion JSON，先聚合（幂等：已是 JSON 时原样返回）。
	respBody = aggregateResponsesStreamToChat(respBody, req.Model, keepReasoning)
	outBody := convertResponsesToChat(respBody, req.Model, keepReasoning)
	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			statsx.RecordChatUsage(req.Model, responsesUsageToChatBridge(u))
		}
	}
	result := logging.SummarizeChatResult(outBody)
	logging.LogResult(ctx, result)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(outBody)
}

// aggregateResponsesStreamToChat 把上游强制 stream:true 返回的 Responses SSE
// 流聚合为一个完整的 chat.completion JSON。与 responsesSSEToChatStream 共用
// 同一套事件解析（output_text.delta / reasoning delta / output_item 与
// function_call_arguments、response.completed/incomplete 的 usage），只是终点
// 是合并后的 JSON 而不是逐块转发的 chat SSE。body 不是 Responses SSE（如已
// 是 JSON、空体或流中带 error 事件）时原样返回，交给上层既有处理（含
// convertResponsesToChat 的 JSON 解析），因此幂等。
func aggregateResponsesStreamToChat(body []byte, model string, wantReasoning bool) []byte {
	var id, outModel string
	var contentBuilder, reasoningBuilder strings.Builder
	var refusal string
	type toolAcc struct {
		callID, name, args string
	}
	tools := map[int]*toolAcc{}
	toolItemToIdx := map[string]int{}
	toolOrder := []int{}
	finishReason := "stop"
	var usage map[string]any
	sawChunk := false

	// toolIdxFor 解析 item id/call_id/output_index 对应的稳定 chat index。
	toolIdxFor := func(itemID, outputIndex string) int {
		if itemID != "" {
			if idx, ok := toolItemToIdx[itemID]; ok {
				return idx
			}
		}
		if outputIndex != "" {
			if idx, ok := toolItemToIdx["#"+outputIndex]; ok {
				return idx
			}
		}
		idx := len(toolOrder)
		toolOrder = append(toolOrder, idx)
		if itemID != "" {
			toolItemToIdx[itemID] = idx
		}
		if outputIndex != "" {
			toolItemToIdx["#"+outputIndex] = idx
		}
		return idx
	}
	ensureTool := func(itemID, outputIndex string) *toolAcc {
		idx := toolIdxFor(itemID, outputIndex)
		acc := tools[idx]
		if acc == nil {
			acc = &toolAcc{}
			tools[idx] = acc
		}
		return acc
	}

	for _, rawLine := range bytes.Split(body, []byte("\n")) {
		line := strings.TrimSpace(string(rawLine))
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		var evt map[string]any
		if json.Unmarshal([]byte(payload), &evt) != nil {
			continue
		}
		sawChunk = true
		if _, isErr := evt["error"]; isErr {
			return body
		}
		switch typ, _ := evt["type"].(string); typ {
		case "response.created", "response.in_progress", "response.queued":
			if resp, ok := evt["response"].(map[string]any); ok {
				if rid, _ := resp["id"].(string); rid != "" && id == "" {
					id = rid
				}
				if m, _ := resp["model"].(string); m != "" {
					outModel = m
				}
			}
		case "response.output_text.delta":
			if t, _ := evt["delta"].(string); t != "" {
				contentBuilder.WriteString(t)
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if wantReasoning {
				if t, _ := evt["delta"].(string); t != "" {
					reasoningBuilder.WriteString(t)
				}
			}
		case "response.refusal.delta":
			if t, _ := evt["delta"].(string); t != "" {
				refusal += t
			}
		case "response.output_item.added", "response.output_item.done":
			item, _ := evt["item"].(map[string]any)
			if item == nil {
				continue
			}
			it, _ := item["type"].(string)
			if it != "function_call" && it != "tool_call" {
				continue
			}
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID, _ = item["id"].(string)
			}
			oi, _ := item["output_index"].(float64)
			acc := ensureTool(callID, formatOutputIndex(oi))
			if callID != "" {
				acc.callID = callID
			}
			if n, _ := item["name"].(string); n != "" {
				acc.name = n
			}
			if args, _ := item["arguments"].(string); args != "" && acc.args == "" {
				acc.args = args
			}
		case "response.function_call_arguments.delta", "response.tool_call_arguments.delta":
			oi, _ := evt["output_index"].(float64)
			itemID, _ := evt["item_id"].(string)
			acc := ensureTool(itemID, formatOutputIndex(oi))
			if pj, _ := evt["delta"].(string); pj != "" {
				acc.args += pj
			}
		case "response.function_call_arguments.done", "response.tool_call_arguments.done":
			oi, _ := evt["output_index"].(float64)
			itemID, _ := evt["item_id"].(string)
			acc := ensureTool(itemID, formatOutputIndex(oi))
			if completed, _ := evt["arguments"].(string); completed != "" {
				acc.args = completed
			}
		case "response.completed", "response.incomplete":
			if resp, ok := evt["response"].(map[string]any); ok {
				if u, ok := resp["usage"].(map[string]any); ok {
					usage = u
				}
				if rid, _ := resp["id"].(string); rid != "" {
					id = rid
				}
				if m, _ := resp["model"].(string); m != "" {
					outModel = m
				}
				if status, _ := resp["status"].(string); status == "incomplete" {
					finishReason = "length"
				}
			}
		}
	}
	if !sawChunk {
		return body
	}
	if id == "" {
		id = "chatcmpl_" + randomString(24)
	}
	if outModel == "" {
		outModel = model
	}
	if len(toolOrder) > 0 && finishReason == "stop" {
		finishReason = "tool_calls"
	}

	msg := map[string]any{"role": "assistant"}
	content := contentBuilder.String()
	if content != "" || len(toolOrder) == 0 {
		msg["content"] = content
	}
	if reasoning := reasoningBuilder.String(); reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	if refusal != "" {
		msg["refusal"] = refusal
	}
	if len(toolOrder) > 0 {
		var toolCalls []map[string]any
		for _, idx := range toolOrder {
			acc := tools[idx]
			if acc == nil {
				continue
			}
			callID := acc.callID
			if callID == "" {
				callID = "call_" + randomHex(12)
			}
			args := acc.args
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      acc.name,
					"arguments": args,
				},
			})
		}
		msg["tool_calls"] = toolCalls
	}

	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   outModel,
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "finish_reason": finishReason,
		}},
	}
	if usage != nil {
		resp["usage"] = responsesUsageToChatBridge(usage)
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return body
	}
	return out
}

// formatOutputIndex 格式化 Responses output_index（number）为稳定 key。
// 与 item_id 一起作为 chat tool_calls index 的别名来源。
// 缺省/非法值返回空串，不参与别名。
func formatOutputIndex(v float64) string {
	if v == 0 {
		return ""
	}
	return fmt.Sprintf("%d", int(v))
}

// convertResponsesToChat 把上游 Responses JSON 响应转为 Chat Completions 响应。
func convertResponsesToChat(respBody []byte, model string, wantReasoning bool) []byte {
	var raw map[string]any
	if err := json.Unmarshal(respBody, &raw); err != nil {
		// 非 JSON（例如上游错误体）：原样返回，让客户端看到上游语义。
		return respBody
	}
	if em, ok := raw["error"]; ok && em != nil {
		return respBody
	}

	id, _ := raw["id"].(string)
	outModel, _ := raw["model"].(string)
	if outModel == "" {
		outModel = model
	}
	var textParts []string
	var reasoningParts []string
	var refusal string
	var toolCalls []map[string]any
	finishReason := "stop"
	if status, _ := raw["status"].(string); status == "incomplete" {
		finishReason = "length"
	}
	output, _ := raw["output"].([]any)
	for _, itemRaw := range output {
		item, ok := itemRaw.(map[string]any)
		if !ok {
			continue
		}
		switch item["type"] {
		case "message":
			content, _ := item["content"].([]any)
			for _, c := range content {
				cm, ok := c.(map[string]any)
				if !ok {
					continue
				}
				switch cm["type"] {
				case "output_text":
					if t, _ := cm["text"].(string); t != "" {
						textParts = append(textParts, t)
					}
				case "refusal":
					if t, _ := cm["refusal"].(string); t != "" {
						refusal += t
					}
				}
			}
		case "reasoning":
			if wantReasoning {
				if summary, ok := item["summary"].([]any); ok {
					for _, s := range summary {
						if sm, ok := s.(map[string]any); ok {
							if t, _ := sm["text"].(string); t != "" {
								reasoningParts = append(reasoningParts, t)
							}
						}
					}
				}
			}
		case "function_call", "tool_call":
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID, _ = item["id"].(string)
			}
			name, _ := item["name"].(string)
			args, _ := item["arguments"].(string)
			toolCalls = append(toolCalls, map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": args,
				},
			})
		}
	}
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	msg := map[string]any{"role": "assistant"}
	content := strings.Join(textParts, "")
	if content != "" || len(toolCalls) == 0 {
		msg["content"] = content
	}
	if reasoning := strings.Join(reasoningParts, "\n"); reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	if refusal != "" {
		msg["refusal"] = refusal
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}

	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   outModel,
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "finish_reason": finishReason,
		}},
	}
	if u, ok := raw["usage"]; ok && u != nil {
		resp["usage"] = responsesUsageToChatBridge(u.(map[string]any))
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return respBody
	}
	return out
}

// ======================== Responses SSE → Chat SSE 状态机 ========================

// responsesUsageToChatBridge 在 claude_responses.go 的 responsesUsageToChat
// 之上补齐 chat 桥接需要的 usage 口径:DeepSeek prompt_cache_hit/miss_tokens
// 透传、input_tokens_details.cached_tokens → prompt_tokens_details.cached_tokens
// 别名、output_tokens_details.reasoning_tokens/thinking_tokens 归位。
// responsesUsageToChat 归 Worker C 的文件所有,这里包一层而非改它。
func responsesUsageToChatBridge(usage map[string]any) map[string]any {
	out := responsesUsageToChat(usage)
	if out == nil {
		return nil
	}
	// total 兜底合成（responsesUsageToChat 也合成;这里防调用方绕过）。
	if _, has := out["total_tokens"]; !has {
		if p, pok := numberAsFloat(out["prompt_tokens"]); pok {
			if c, cok := numberAsFloat(out["completion_tokens"]); cok {
				out["total_tokens"] = p + c
			}
		}
	}
	// DeepSeek 缓存命中/未命中键透传。
	for _, k := range []string{"prompt_cache_hit_tokens", "prompt_cache_miss_tokens"} {
		if v, ok := usage[k]; ok {
			out[k] = v
		}
	}
	// cached_tokens 别名:input_tokens_details.cached_tokens ↔
	// prompt_tokens_details.cached_tokens（先有谁用谁,另一形态同步出来）。
	cached := 0.0
	hasCached := false
	if d, ok := usage["input_tokens_details"].(map[string]any); ok {
		if v, ok := numberAsFloat(d["cached_tokens"]); ok {
			cached = v
			hasCached = true
		}
	}
	if !hasCached {
		if d, ok := out["prompt_tokens_details"].(map[string]any); ok {
			if v, ok := numberAsFloat(d["cached_tokens"]); ok {
				cached = v
				hasCached = true
			}
		}
	}
	if hasCached {
		details, _ := out["prompt_tokens_details"].(map[string]any)
		if details == nil {
			details = map[string]any{}
		}
		details["cached_tokens"] = cached
		out["prompt_tokens_details"] = details
	}
	if od, ok := usage["output_tokens_details"].(map[string]any); ok {
		var reasoning float64
		var hasReasoning bool
		if v, ok := numberAsFloat(od["reasoning_tokens"]); ok {
			reasoning, hasReasoning = v, true
		} else if v, ok := numberAsFloat(od["thinking_tokens"]); ok {
			reasoning, hasReasoning = v, true
		}
		if hasReasoning {
			details, _ := out["completion_tokens_details"].(map[string]any)
			if details == nil {
				details = map[string]any{}
			}
			if existing, eok := numberAsFloat(details["reasoning_tokens"]); !eok || existing == 0 {
				details["reasoning_tokens"] = reasoning
			}
			out["completion_tokens_details"] = details
		}
	}
	return out
}

type responsesToChatState struct {
	w             http.ResponseWriter
	flusher       http.Flusher
	stats         *logging.StreamStats
	id            string
	model         string
	keepReasoning bool
	includeUsage  bool
	sentRole      bool
	toolIndices   map[string]int // item id/call_id/#output_index → chat tool_calls index
	toolAnnounced map[int]bool   // chat tool_calls index → 首 tool_calls chunk 已宣发
	arguments     map[int]string // chat tool_calls index → 已下发 arguments 累计文本
	toolCount     int
	sawTool       bool
	finishReason  string // 终态 finish 原因(空 = 未定,finalize 时合成)
	fullUsage     map[string]any
	finalized     bool // 已写 [DONE]/终态帧（幂等）
}

func responsesSSEToChatStream(ctx context.Context, w http.ResponseWriter, rc io.Reader, model string, keepReasoning bool, includeUsage bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	st := &responsesToChatState{
		w:             w,
		flusher:       flusher,
		stats:         &logging.StreamStats{Start: time.Now()},
		id:            "chatcmpl-" + randomHex(12),
		model:         model,
		keepReasoning: keepReasoning,
		includeUsage:  includeUsage,
		toolIndices:   map[string]int{},
		toolAnnounced: map[int]bool{},
		arguments:     map[int]string{},
		fullUsage:     map[string]any{},
	}
	defer func() {
		st.stats.ToolCallCount = st.toolCount
		if len(st.fullUsage) > 0 {
			statsx.RecordChatUsage(model, responsesUsageToChatBridge(st.fullUsage))
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
				// EOF / 读错误兜底:response.completed 未到达时补终态
				// finish chunk(+usage)+[DONE],保证 OpenAI SDK 不挂起
				// （幂等:已完成路径不受影响）。
				st.finalize()
				return
			}
		}
	}
}

// finalize 幂等地结束 chat 流:补发终态 finish chunk(incomplete → length,
// 有 tool call → tool_calls,否则 stop)、includeUsage 时的 usage 终块与
// [DONE]。response.completed/incomplete 正常路径与 response.failed/error/
// EOF 兜底路径共用。
func (st *responsesToChatState) finalize() {
	if st.finalized {
		return
	}
	st.finalized = true
	if st.finishReason == "" {
		st.finishReason = "stop"
		if st.sawTool {
			st.finishReason = "tool_calls"
		}
	}
	if st.sentRole {
		st.emitChunk(map[string]any{}, st.finishReason, nil)
	}
	if st.includeUsage && len(st.fullUsage) > 0 {
		st.emitChunk(map[string]any{}, "", responsesUsageToChatBridge(st.fullUsage))
	}
	st.w.Write([]byte("data: [DONE]\n\n"))
	if st.flusher != nil {
		st.flusher.Flush()
	}
	st.stats.DoneSeen = true
	st.stats.SawFinish = true
}

func (st *responsesToChatState) emitChunk(delta map[string]any, finishReason string, usage map[string]any) {
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

func (st *responsesToChatState) ensureRole() {
	if st.sentRole {
		return
	}
	st.sentRole = true
	st.emitChunk(map[string]any{"role": "assistant", "content": ""}, "", nil)
}

// toolIdxFor 解析 item id/call_id 对应的 chat tool_calls index:未知 item
// （上游没发 output_item.added 直接发 delta/done,Observed 场景）分配新
// index,保证后续补发首 chunk 时 index 稳定。
func (st *responsesToChatState) toolIdxFor(itemID string) int {
	if idx, ok := st.toolIndices[itemID]; ok {
		return idx
	}
	idx := st.toolCount
	st.toolCount++
	if itemID != "" {
		st.toolIndices[itemID] = idx
	}
	return idx
}

// aliasToolKey 把 done/delta 事件的 "另一形态" id（fc_* vs call_*）映到同一
// index。Responses 事件里 added 用 call_id、arguments.done 常用 item.id,
// 两方必须指向同一 chat tool_calls 槽位,否则 done 补发会落到新 index。
func (st *responsesToChatState) aliasToolKey(fromKey, toKey string) {
	if fromKey == "" || toKey == "" || fromKey == toKey {
		return
	}
	if idx, ok := st.toolIndices[toKey]; ok {
		st.toolIndices[fromKey] = idx
	}
}

// outputIndexKey namespaces a Responses output_index inside toolIndices,
// which is otherwise keyed by call_id / item id. It is the fallback key for
// argument events that carry no item_id, and it is the same key
// responses_to_anthropic.go pairs added/done events by.
func outputIndexKey(oi int) string {
	return fmt.Sprintf("#%d", oi)
}

// registerToolKeys allocates the chat tool_calls index for a tool call and
// points every id shape the later events may use at it: the call_id, the item
// id ("fc_..."), and the output_index. The index must be allocated BEFORE the
// aliases: aliasToolKey resolves through toolIndices[callID], so aliasing
// first is a silent no-op and the argument deltas open a new index.
func (st *responsesToChatState) registerToolKeys(evt, item map[string]any) (callID string, toolIdx int) {
	itemID, _ := item["id"].(string)
	callID, _ = item["call_id"].(string)
	if callID == "" {
		callID = itemID
	}
	toolIdx = st.toolIdxFor(callID)
	st.aliasToolKey(itemID, callID)
	if oi, ok := evt["output_index"].(float64); ok {
		st.aliasToolKey(outputIndexKey(int(oi)), callID)
	}
	return callID, toolIdx
}

// eventToolIdx resolves the chat tool_calls index an argument event refers
// to. Responses names the item by item_id ("fc_...") — never by the call_id
// that output_item.added announced — and may omit it entirely, leaving only
// output_index. Both are registered by registerToolKeys.
func (st *responsesToChatState) eventToolIdx(evt map[string]any) int {
	itemID, _ := evt["item_id"].(string)
	if itemID != "" {
		if idx, ok := st.toolIndices[itemID]; ok {
			return idx
		}
	}
	if oi, ok := evt["output_index"].(float64); ok {
		if idx, ok := st.toolIndices[outputIndexKey(int(oi))]; ok {
			return idx
		}
	}
	// Unknown item (no output_item.added seen): toolIdxFor allocates and
	// remembers, so a later done event keeps the same slot.
	return st.toolIdxFor(itemID)
}

func (st *responsesToChatState) handleLine(line string) {
	payload, ok := strings.CutPrefix(line, "data: ")
	if !ok {
		return
	}
	var evt map[string]any
	if json.Unmarshal([]byte(payload), &evt) != nil {
		return
	}
	switch typ, _ := evt["type"].(string); typ {
	case "response.created", "response.in_progress", "response.queued":
		if resp, ok := evt["response"].(map[string]any); ok {
			if id, _ := resp["id"].(string); id != "" {
				st.id = id
			}
			if m, _ := resp["model"].(string); m != "" {
				st.model = m
			}
			if u, ok := resp["usage"].(map[string]any); ok {
				mergeUsage(st.fullUsage, u)
			}
		}
		st.ensureRole()
	case "response.output_text.delta":
		st.ensureRole()
		if t, _ := evt["delta"].(string); t != "" {
			st.emitChunk(map[string]any{"content": t}, "", nil)
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if st.keepReasoning {
			st.ensureRole()
			if t, _ := evt["delta"].(string); t != "" {
				st.emitChunk(map[string]any{"reasoning_content": t}, "", nil)
			}
		}
	case "response.refusal.delta":
		// refusal 增量并入正文（与非流式 convertResponsesToChat 把 refusal
		// 放 message 字段不同——chat 流式 delta 没有 refusal 槽位且
		// content=-null 语义不完整;内联进 content 是对 OpenAI 客户端最
		// 保真的降级,与非流式 "content 里看不到 refusal" 略不一致,详见
		// convertResponsesToChat 的 refusal 注释）。
		st.ensureRole()
		if t, _ := evt["delta"].(string); t != "" {
			st.emitChunk(map[string]any{"content": t}, "", nil)
		}
	case "response.output_item.added":
		item, _ := evt["item"].(map[string]any)
		if item == nil {
			return
		}
		switch item["type"] {
		case "function_call", "tool_call":
			st.sawTool = true
			callID, toolIdx := st.registerToolKeys(evt, item)
			st.toolAnnounced[toolIdx] = true
			st.ensureRole()
			st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": toolIdx, "id": callID, "type": "function",
				"function": map[string]any{"name": toString(item["name"]), "arguments": ""},
			}}}, "", nil)
		}
	case "response.function_call_arguments.delta", "response.tool_call_arguments.delta":
		st.ensureRole()
		toolIdx := st.eventToolIdx(evt)
		if pj, _ := evt["delta"].(string); pj != "" {
			st.arguments[toolIdx] += pj
			st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": toolIdx, "id": nil, "type": "function",
				"function": map[string]any{"name": "", "arguments": pj},
			}}}, "", nil)
		}
	case "response.function_call_arguments.done", "response.tool_call_arguments.done":
		st.ensureRole()
		toolIdx := st.eventToolIdx(evt)
		// done 携带完整 arguments JSON:只补发已下发前缀之后的差量,
		// 避免客户端 concat 后重复（对齐 sub2api resToChatHandleFuncArgsDone）。
		if completed, _ := evt["arguments"].(string); completed != "" {
			emitted := st.arguments[toolIdx]
			if completed != emitted && strings.HasPrefix(completed, emitted) {
				remainder := completed[len(emitted):]
				st.arguments[toolIdx] = completed
				st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
					"index": toolIdx, "id": nil, "type": "function",
					"function": map[string]any{"name": "", "arguments": remainder},
				}}}, "", nil)
			}
		}
	case "response.output_item.done":
		item, _ := evt["item"].(map[string]any)
		if item == nil {
			return
		}
		switch item["type"] {
		case "function_call", "tool_call":
			st.sawTool = true
			callID, toolIdx := st.registerToolKeys(evt, item)
			// 没有 add/delta 出现过（罕见）:补一次首 chunk 宣告工具调用,
			// 并把 item 上的完整 arguments 作为单段增量发完。
			if !st.toolAnnounced[toolIdx] {
				st.toolAnnounced[toolIdx] = true
				name := toString(item["name"])
				st.ensureRole()
				st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
					"index": toolIdx, "id": callID, "type": "function",
					"function": map[string]any{"name": name, "arguments": ""},
				}}}, "", nil)
				if args, _ := item["arguments"].(string); args != "" && st.arguments[toolIdx] == "" {
					st.arguments[toolIdx] = args
					st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
						"index": toolIdx, "id": nil, "type": "function",
						"function": map[string]any{"name": "", "arguments": args},
					}}}, "", nil)
				}
			}
		}
	case "response.completed", "response.incomplete":
		if resp, ok := evt["response"].(map[string]any); ok {
			if u, ok := resp["usage"].(map[string]any); ok {
				mergeUsage(st.fullUsage, u)
			}
			if status, _ := resp["status"].(string); status == "incomplete" {
				st.finishReason = "length"
			} else if st.sawTool {
				st.finishReason = "tool_calls"
			} else {
				st.finishReason = "stop"
			}
		}
		st.finalize()
	case "response.failed", "error":
		em, _ := evt["response"].(map[string]any)
		message := "upstream error"
		if em != nil {
			if errObj, ok := em["error"].(map[string]any); ok {
				if m, _ := errObj["message"].(string); m != "" {
					message = m
				}
			}
		}
		if m, ok := evt["message"].(string); ok && m != "" {
			message = m
		}
		if !st.sentRole {
			st.ensureRole()
		}
		st.w.Write([]byte("data: " + `{"error":{"message":` + jsonString(message) + `}}` + "\n\n"))
		// 终态错误事件也补 [DONE](幂等):避免 OpenAI SDK 挂在缺哨兵的流上。
		st.finalize()
	}
}
