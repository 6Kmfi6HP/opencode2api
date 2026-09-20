package app

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/6Kmfi6HP/opencode2api/internal/bridge"
	"github.com/6Kmfi6HP/opencode2api/internal/config"
	"github.com/6Kmfi6HP/opencode2api/internal/logging"
	"github.com/6Kmfi6HP/opencode2api/internal/modelsdev"
	"github.com/6Kmfi6HP/opencode2api/internal/random"
	statsx "github.com/6Kmfi6HP/opencode2api/internal/stats"
)

// Claude roundtrip; convertResponse strips it before responding to clients.
// 纯核已迁入 bridge.BuildOpenAIResponse；本壳注入 now/idGen 并包装类型化
// 错误为 anthropicProtocolError。
func buildOpenAIResponse(anthropicMsg map[string]any, contentBlocks []map[string]any, modelID string) ([]byte, error) {
	out, err := bridge.BuildOpenAIResponse(time.Now().Unix, randomIDGen, anthropicMsg, contentBlocks, modelID)
	return out, wrapBridgeProtocolError(err)
}

// (non-streaming) to Chat Completions format. Returns an error on malformed input.
// 纯核已迁入 bridge.ConvertAnthropicMessageToOpenAI；薄壳注入 now/idGen。
func convertAnthropicMessageToOpenAI(msg map[string]any, modelID string) ([]byte, error) {
	out, err := bridge.ConvertAnthropicMessageToOpenAI(time.Now().Unix, randomIDGen, msg, modelID)
	return out, wrapBridgeProtocolError(err)
}

// Returns an error if the body is malformed, truncated, or contains an error event.
// 纯核已迁入 bridge.ConvertAnthropicToOpenAI；薄壳注入 now/idGen 并包装
// 类型化错误为 anthropicProtocolError。
func convertAnthropicToOpenAI(body []byte, modelID string) ([]byte, error) {
	out, err := bridge.ConvertAnthropicToOpenAI(time.Now().Unix, randomIDGen, body, modelID)
	return out, wrapBridgeProtocolError(err)
}

// ======================== 响应清理 ========================

// 纯核已迁入 bridge.CleanNulls；薄壳转发。
func cleanNulls(m map[string]any) {
	bridge.CleanNulls(m)
}

// precedes tool calls is left alone when keepReasoning is true.
// 纯核已迁入 bridge.PromoteMisplacedReasoning；薄壳转发。
func promoteMisplacedReasoning(fields map[string]any, keepReasoning bool) bool {
	return bridge.PromoteMisplacedReasoning(fields, keepReasoning)
}

// 纯核已迁入 bridge.CleanStreamDelta；薄壳转发。
func cleanStreamDelta(delta map[string]any, keepReasoning bool) {
	bridge.CleanStreamDelta(delta, keepReasoning)
}

// clientStreamUsageWanted 解析客户端原始请求体中的
// stream_options.include_usage（顶层或 extra_body 扩展域），决定网关是否把
// 上游 include_usage=true 产出的 usage chunk 透传给客户端。
// 纯核已迁入 bridge.ClientStreamUsageWanted；薄壳转发。
func clientStreamUsageWanted(body []byte) bool {
	return bridge.ClientStreamUsageWanted(body)
}

// convertStreamChunkWithUsage 转换流式 chunk，并在同一次解析中顺带返回 usage。
// 注意：流循环（chat.go 的 stream 处理）仍会为流统计单独解析一次 chunk；
// 这里的 "顺带提取" 只是免去了 usage 的第三次解析。
// clientWantsUsage=false 时丢弃只含 usage 且 choices 为空的 chunk：那是网
// 关为流统计向上游强制 include_usage=true 产出的，客户端未请求就不该收到。
// 纯核已迁入 bridge.ConvertStreamChunkWithUsage；薄壳注入 idGen。
func convertStreamChunkWithUsage(line string, keepReasoning, clientWantsUsage bool) (string, map[string]any) {
	return bridge.ConvertStreamChunkWithUsage(randomIDGen, line, keepReasoning, clientWantsUsage)
}

// 纯核已迁入 bridge.ConvertResponse；薄壳注入 idGen。原实现对反序列化失败
// 仅 slog.Warn 后原样返回，纯层静默透传（保持一致的字节级行为）。
func convertResponse(data []byte, keepReasoning bool) ([]byte, error) {
	return bridge.ConvertResponse(randomIDGen, data, keepReasoning)
}

// ======================== Chat Completions Handler ========================

func chatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	auth, body, ok := readJSONRequestBody(w, r)
	if !ok {
		return
	}

	logging.MaybeBodySummary(r.Context(), "chat completion request body", body)

	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	modelIn := req.Model
	req.Model = resolveModelForAuth(auth, req.Model)
	if req.Model == "" {
		modelIDs := getModelIDs()
		if len(modelIDs) > 0 {
			req.Model = modelIDs[0]
		} else {
			req.Model = "deepseek-v4-flash-free"
		}
	}
	req.Model = mapPublicToFreeModel(auth, req.Model)
	if !validateRequestTemperature(w, req.Temperature, "chat", 0, 2) {
		return
	}

	// 协议路由：显式规则与 native-responses 运行时记忆命中时按上游原生协议
	// 转发。memory 层必须查：muse-spark-*-contributor[-free] 的 chat 通道被
	// 上游整档 500，只能靠原生 responses 透传；漏查会让 Chat 客户端拿不到
	// probe/记忆带来的回退。
	proto, protoSource := resolveUpstreamProtocolWithSource(req.Model)
	switch proto {
	case upstreamProtocolAnthropic:
		forwardChatViaAnthropic(w, r, auth, &req, wantsReasoning(&req))
		return
	case upstreamProtocolResponses:
		slog.Info("chat dispatch via native responses", "model", req.Model, "proto_source", protoSource)
		forwardChatViaResponses(w, r, auth, &req, wantsReasoning(&req))
		return
	}

	// 多模态路由：检测到图片时转发到配置的上游

	req.Messages = fixToolCallGaps(req.Messages)
	keepReasoning := wantsReasoning(&req)
	req.Messages = ensureReasoningContent(req.Messages, keepReasoning)
	// 客户端 stream_options.include_usage 意图：OpenAIRequest 不持有该字
	// 段，从原始 body 读 "stream_options":{"include_usage":bool} 或扩展域
	// "extra_body"."stream_options".include_usage。向上游始终强制
	// include_usage=true（下方），但向下游客户端只在它曾显式请求时才回
	// usage chunk。
	clientWantsUsage := clientStreamUsageWanted(body)
	if req.Stream {
		if req.ExtraBody == nil {
			req.ExtraBody = map[string]any{}
		}
		req.ExtraBody["stream_options"] = map[string]any{"include_usage": true}
	}
	effortIn := req.ReasoningEffort
	if effortIn == "" && !isThinkingDisabled(req.Thinking) {
		effortIn = reasoningEffortFromThinking(req.Thinking)
	}
	upstreamSurface := "zen"
	if auth.shouldUseGoEndpoint(req.Model) {
		upstreamSurface = "go"
	}
	logging.PlanRequest(r.Context(), map[string]any{
		"protocol":             "chat",
		"model_in":             modelIn,
		"model_resolved":       req.Model,
		"auth_mode":            authModeString(auth.Mode),
		"auth_source":          auth.Source,
		"has_key":              auth.Token != "",
		"upstream_surface":     upstreamSurface,
		"stream":               req.Stream,
		"keep_reasoning":       keepReasoning,
		"thinking":             thinkingState(req.Thinking),
		"reasoning_effort_in":  effortIn,
		"reasoning_effort_out": mappedReasoningEffort(effortIn),
		"tools_count":          len(req.Tools),
		"messages_count":       len(req.Messages),
		"multimodal_parts":     countMultimodalParts(req.Messages),
		"text_only_model":      modelIsTextOnly(req.Model),
		"max_tokens":           req.MaxTokens,
		"max_tokens_cap":       config.MaxTokensCapFor(req.Model),
	})
	upstreamBody := buildUpstreamBody(&req)

	if req.Stream {
		upResp, status, _, err := callOpenCodeAPIStream(r.Context(), upstreamBody, req.Model, auth)
		if err != nil || status < 200 || status >= 300 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if upResp != nil {
				errBody, _ := io.ReadAll(upResp)
				if len(errBody) > 0 {
					w.Write(errBody)
					return
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error", "type": "upstream_error"}})
			return
		}
		defer upResp.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		reader := bufio.NewReader(upResp)
		stats := &logging.StreamStats{Start: time.Now()}
		doneSeen := false
		// sendDone 幂等补发 [DONE]：正常路径上游会自带；上游提前断流（EOF
		// 而未发 DONE）时由这里兜底，保证客户端总能收到终止标记。
		sendDone := func() {
			if doneSeen {
				return
			}
			doneSeen = true
			stats.DoneSeen = true
			w.Write([]byte("data: [DONE]\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					sendDone()
					break
				}
				logging.FromContext(r.Context()).Error("stream read error", "error", err)
				// 发送错误事件通知客户端
				w.Write([]byte("data: {\"error\":\"stream read error\"}\n\n"))
				sendDone()
				stats.Log(r.Context(), "chat")
				return
			}
			if doneSeen {
				// [DONE] 已发（上游自带或兜底），后续仅腾空缓冲区。
				continue
			}
			trimmed := strings.TrimSpace(line)
			if trimmed == "data: [DONE]" {
				sendDone()
				continue
			}

			if strings.HasPrefix(line, "data: ") {
				var raw map[string]any
				if json.Unmarshal([]byte(line[6:]), &raw) == nil {
					if choices, ok := raw["choices"].([]any); ok && len(choices) > 0 {
						if choice, ok := choices[0].(map[string]any); ok {
							if delta, ok := choice["delta"].(map[string]any); ok {
								stats.ObserveDelta(delta, keepReasoning)
							}
							if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
								stats.FinishReason = fr
								stats.SawFinish = true
							}
						}
					}
				}
			}

			out, usage := convertStreamChunkWithUsage(line, keepReasoning, clientWantsUsage)
			if out == "" {
				// 空choices chunk，但可能有 usage
				if usage != nil {
					statsx.RecordChatUsage(req.Model, usage)
				}
				continue
			}

			// 提取 usage（已在 convertStreamChunkWithUsage 中解析）
			if usage != nil && !doneSeen {
				statsx.RecordChatUsage(req.Model, usage)
			}

			w.Write([]byte(out))
			w.Write([]byte("\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		stats.Log(r.Context(), "chat")
		return
	}

	respBody, status, _, err := callOpenCodeAPI(r.Context(), upstreamBody, req.Model, auth)
	if err != nil || status < 200 || status >= 300 {
		if err != nil {
			writeUpstreamError(w, status, err, "chat")
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if len(respBody) > 0 {
				w.Write(respBody)
			} else {
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error", "type": "upstream_error"}})
			}
		}
		return
	}
	outBody := respBody
	convertedResp, err := convertResponse(respBody, keepReasoning)
	if err == nil {
		outBody = convertedResp
	}
	result := logging.SummarizeChatResult(outBody)
	if !keepReasoning {
		var before map[string]any
		if json.Unmarshal(respBody, &before) == nil {
			if choices, ok := before["choices"].([]any); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]any); ok {
					if msg, ok := choice["message"].(map[string]any); ok {
						content, _ := msg["content"].(string)
						rc, _ := msg["reasoning_content"].(string)
						if content == "" && rc != "" {
							result["promoted_reasoning"] = true
						}
					}
				}
			}
		}
	}
	logging.LogResult(r.Context(), result)
	// Record token usage
	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			statsx.RecordChatUsage(req.Model, u)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(outBody)
}

// ======================== Models Handler ========================

func listModelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	modelMu.RLock()
	loaded, models := modelsLoaded, modelsCache
	modelMu.RUnlock()
	if !loaded || len(models) == 0 {
		fetched, err := fetchModels()
		if err == nil && len(fetched) > 0 {
			modelMu.Lock()
			modelsCache = fetched
			modelsLoaded = true
			models = modelsCache
			modelMu.Unlock()
		}
	}
	if len(models) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "无法获取模型列表，请检查上游服务是否可用",
		})
		return
	}
	// 保存别名快照；目录权限仍按真实上游模型判断，最后再替换为客户端可见名称。
	aliases := getModelAliasMap()

	auth := extractUpstreamAuth(r)
	var combinedModels []ModelInfo
	switch {
	case auth.shouldUseGoCatalog():
		modelMu.RLock()
		combinedModels = make([]ModelInfo, 0, len(models)+len(goModelsCache))
		for _, model := range models {
			if isFreeModel(model.ID) {
				combinedModels = append(combinedModels, model)
			}
		}
		for _, goModel := range goModelsCache {
			if !containsModelWithID(combinedModels, goModel.ID) {
				combinedModels = append(combinedModels, goModel)
			}
		}
		modelMu.RUnlock()
	case auth.Mode == AuthRoutePublic:
		combinedModels = models
		filtered := make([]ModelInfo, 0, len(combinedModels))
		for _, m := range combinedModels {
			if isFreeModel(m.ID) {
				filtered = append(filtered, m)
			}
		}
		if len(filtered) > 0 {
			combinedModels = filtered
		}
	default:
		combinedModels = models
	}
	allModels := replaceModelIDsWithAliases(combinedModels, aliases)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   allModels,
	})
}

func replaceModelIDsWithAliases(models []ModelInfo, aliases map[string]string) []ModelInfo {
	aliasesByUpstream := make(map[string][]string, len(aliases))
	for alias, upstream := range aliases {
		alias = strings.TrimSpace(alias)
		upstream = strings.TrimSpace(upstream)
		if alias == "" || upstream == "" {
			continue
		}
		aliasesByUpstream[upstream] = append(aliasesByUpstream[upstream], alias)
	}
	for upstream := range aliasesByUpstream {
		sort.Strings(aliasesByUpstream[upstream])
	}

	result := make([]ModelInfo, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		visibleIDs := aliasesByUpstream[model.ID]
		if len(visibleIDs) == 0 {
			visibleIDs = []string{publicFacingModelID(model.ID)}
		}
		for _, visibleID := range visibleIDs {
			if _, exists := seen[visibleID]; exists {
				continue
			}
			visibleModel := model
			visibleModel.ID = visibleID
			if visibleID != model.ID {
				visibleModel.OwnedBy = "alias"
			}
			result = append(result, visibleModel)
			seen[visibleID] = struct{}{}
		}
	}
	return result
}

// ======================== Thinking/Reasoning 判断 ========================
// 纯判定逻辑已迁入 internal/bridge；以下为同名薄壳，config 相关取值在 app 侧
// 读取后注入，保证 *_test.go 无需修改。

func isThinkingEnabled(value any) bool { return bridge.IsThinkingEnabled(value) }

// effortFromOutputConfig reads Claude Code's output_config.effort
// (set by --effort / CLAUDE_CODE_EFFORT_LEVEL).
func effortFromOutputConfig(value any) string { return bridge.EffortFromOutputConfig(value) }

func isThinkingDisabled(value any) bool { return bridge.IsThinkingDisabled(value) }

// buildUpstreamThinking preserves budget_tokens / effort fields when present.
func buildUpstreamThinking(value any) map[string]any { return bridge.BuildUpstreamThinking(value) }

// reasoningEffortFromThinking maps an Anthropic-style thinking object onto an
// OpenAI-compatible reasoning_effort when the client did not set one
// explicitly. An explicit "effort" string wins; otherwise the shared
// thinkingBudgetToEffort tiers budget_tokens.
func reasoningEffortFromThinking(value any) string { return bridge.ReasoningEffortFromThinking(value) }

func wantsReasoning(req *OpenAIRequest) bool {
	return bridge.WantsReasoning(config.ForceDisableThinking(), req)
}

// bridgeConfigViewFor 按 model 解析一份 bridge.ConfigView 快照并注入纯核。
// 每个配置取值与原实现一致（各自 config.Get() 快照）——保持逐项读取而非单次
// 合并快照，行为逐一等价。per-model 的 MaxTokensCap/TextOnly/RejectsCacheControl
// 按 modelID 解析。
func bridgeConfigViewFor(modelID string) bridge.ConfigView {
	return bridge.ConfigView{
		MaxTokensCap:         config.MaxTokensCapFor(modelID),
		ForceDisableThinking: config.ForceDisableThinking(),
		ReasoningEffortMap:   config.ReasoningEffortMap(),
		PromptCacheRetention: config.PromptCacheRetention(),
		CacheBreakpoints:     config.CacheBreakpoints(),
		RejectsCacheControl:  rejectsCacheControl(modelID),
		TextOnly:             modelIsTextOnly(modelID),
	}
}

// 能力协商由 opencode 客户端 + 上游负责；这里既不"硬降级"也不"补全"。
// 薄壳转发到 bridge。
func normalizeContent(content any) any {
	return bridge.NormalizeContent(content)
}

// fixToolCallGaps 补全 assistant.tool_calls 缺失的 tool 响应。薄壳转发到 bridge。
func fixToolCallGaps(messages []Message) []Message {
	return bridge.FixToolCallGaps(messages)
}

// ensureReasoningContent 在 thinking 开启时为 reasoning_content==nil 的
// assistant 消息补空串槽位。它不覆盖已有值：WeChat/DeepSeek 兼容要求带
// tool_calls 的 assistant 消息携带产生它的推理（claudeToOpenAIMessages 只在
// 该情况下写入 reasoning_content），空串槽位只补到没有推理文本的普通
// assistant 轮，序列化后被 convertStreamChunkWithUsage/cleanNulls 的空串清
// 理兜住，因此不与收窄后的写入语义冲突。薄壳转发到 bridge。
func ensureReasoningContent(messages []Message, thinking bool) []Message {
	return bridge.EnsureReasoningContent(messages, thinking)
}

// multimodalAttachedLabel is the text annotation that replaces multimodal
// image/document parts when the resolved upstream model only accepts text.
// It matches the label the Claude converter already uses for tool_result
// attachments, so tool images and message images degrade identically.
// 常量定义已迁入 bridge；此处为同包别名，保持 app 内引用不变。
const (
	multimodalAttachedLabel = bridge.MultimodalAttachedLabel
	multimodalDocumentLabel = bridge.MultimodalDocumentLabel
)

// modelIsTextOnly reports whether the resolved upstream model should receive
// text-only content. The models.dev catalog data decides for known models
// (input modalities containing only "text"); the configured text_only_models
// prefixes act as an explicit manual override on top. Unknown models are not
// downgraded so the upstream error stays truthful.
//
// 该判定读取 config.IsTextOnlyModel 与 modelsdev 目录（带缓存的 process 级
// 读取），按纯度契约留在 app 侧；产出的布尔经 ConfigView.TextOnly /
// textOnly 参数注入 bridge 纯核。
func modelIsTextOnly(model string) bool {
	return config.IsTextOnlyModel(model) ||
		modelsdev.IsTextOnly(model, modelsdev.GetCachedModalities())
}

// countMultimodalParts returns the number of image/document content parts in
// a request, for observability (request_plan). 薄壳转发到 bridge。
func countMultimodalParts(messages []Message) int {
	return bridge.CountMultimodalParts(messages)
}

// downgradeMultimodalContent replaces image_url ("[image attached]") and file
// ("[document attached]") content parts with text annotations so requests to
// text-only upstream models (e.g. DeepSeek) keep working instead of failing
// with "image not supported". Text and all other parts are preserved, as is
// the relative order. Returns the original slice unchanged when model is not
// text-only or there is nothing to downgrade. 薄壳转发到 bridge。
func downgradeMultimodalContent(content []any, textOnly bool) any {
	return bridge.DowngradeMultimodalContent(content, textOnly)
}

// convertMessagesForUpstream 序列化 Chat messages 为上游 messages 数组。
// 薄壳转发到 bridge。
func convertMessagesForUpstream(messages []Message, textOnly bool) []map[string]any {
	return bridge.ConvertMessagesForUpstream(messages, textOnly)
}

// ======================== 完整请求转换（含 thinking/reasoning_effort/ExtraBody） ========================

// convertRequest 把 Chat Completions 请求转为 OpenCode 上游请求体 map。纯核已
// 迁入 bridge.ConvertRequest；本壳按 req.Model 解析 ConfigView 快照注入。
func convertRequest(req *OpenAIRequest) map[string]any {
	return bridge.ConvertRequest(bridgeConfigViewFor(req.Model), req)
}

func buildUpstreamBody(req *OpenAIRequest) []byte {
	b, err := bridge.MarshalUpstreamBody(bridgeConfigViewFor(req.Model), req)
	if err != nil {
		slog.Error("marshal upstream body failed", "error", err)
	}
	return b
}

// randomIDGen 是 bridge ID 生成注入的生产实现，转发到 internal/random。
// bridge 的 idGen 约定为 func(prefix string, n int) string；random.String 已
// 满足 n 位随机小写字母/数字的语义，prefix 形参仅作占位（与签名对齐）。
func randomIDGen(_ string, n int) string { return random.String(n) }

// randomHexIDGen 是 bridge hex ID 生成注入的生产实现，转发到 internal/random。
// random.Hex 满足 n 位随机 hex（0-9a-f）的语义，与原 randomHex 一致。
func randomHexIDGen(_ string, n int) string { return random.Hex(n) }

// deterministicResponseID 把任意上游 id 归一到 prefix 命名空间：已带 prefix
// 的 id 原样返回；空 id 取随机后缀（调用方应缓存）；其余经 sha256[0:16] 做
// 确定性映射并以前缀隔离（claude client 不得泄漏 chatcmpl_/resp_ 形态）。
// 薄壳转发到 bridge。
func deterministicResponseID(prefix, id string) string {
	return bridge.DeterministicResponseID(randomIDGen, prefix, id)
}

// normalizeChatResponseID ensures a Chat response ID has the chatcmpl- prefix.
func normalizeChatResponseID(id string) string {
	return deterministicResponseID("chatcmpl-", id)
}

// normalizeResponsesID ensures a Responses response ID has the resp_ prefix.
func normalizeResponsesID(id string) string {
	return deterministicResponseID("resp_", id)
}

// normalizeClaudeMessageID ensures a Claude message ID has the msg_ prefix.
func normalizeClaudeMessageID(id string) string {
	return deterministicResponseID("msg_", id)
}
