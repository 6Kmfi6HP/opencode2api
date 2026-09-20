package app

import (
	"context"
	"encoding/json"
	"github.com/6Kmfi6HP/opencode2api/internal/bridge"
	"github.com/6Kmfi6HP/opencode2api/internal/config"
	"github.com/6Kmfi6HP/opencode2api/internal/logging"
	statsx "github.com/6Kmfi6HP/opencode2api/internal/stats"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// ======================== Responses API ========================

func responsesInputToMessages(input any, instructions string) []Message {
	return bridge.ResponsesInputToMessages(input, instructions)
}

// convertResponsesTools 把 Responses tools 转 Chat Completions tools。
// 服务端工具（web_search/file_search/computer_use/mcp/local_shell/custom 等）
// 无对应 function 形状，由 responsesToolFunction 返回 ok=false，此处丢弃并
// 日志计数；若 tool_choice 指向被丢弃的工具，由调用方 normalizeToolChoiceWithTools
// 兜底为不传。
// ======== tool_use/tool_result 配对归一化（Responses→Anthropic 组装前） ========

// messageTextContent 从 Chat content（纯字符串或多模态 parts 数组）提取可见文本。
// 非文本分片（image_url/file 等）在无文本时兜底为其 JSON 字符串，避免空内容消息。
func messageTextContent(content any) string { return bridge.MessageTextContent(content) }

// mergeConsecutiveSameRole 把相邻同 role 的 domain.Message 合并。tool（合并到
// 前一条，保留 ToolCallID 供配对索引，后续已展开为 tool_result）与
// system/developer/user/assistant（仅无 tool_calls 时按对话回合合并）分别处理。
// Anthropic 要求 tool_use 与其 tool_result 之间无任意 user/assistant 文本，
// 本归一化与 normalizeAnthropicToolPairing 配合恢复工具配对与回合交替。
func mergeConsecutiveSameRole(msgs []Message) []Message {
	return bridge.MergeConsecutiveSameRole(msgs)
}

// mergeAdjacentToolCallAssistants 把相邻的「纯 tool_call assistant」消息合并
// 为单条，使 Responses 并行调用（多个 function_call）共享同一 assistant,
// 对应随后的 tool 结果按 call 序紧邻排列（normalizeAnthropicToolPairing
// 视为同一 assistant turn 的处理单元）。
func mergeAdjacentToolCallAssistants(msgs []Message) []Message {
	return bridge.MergeAdjacentToolCallAssistants(msgs)
}

// normalizeAnthropicToolPairing 把 Chat messages 归一化成满足 Anthropic
// tool_use/tool_result 不变量的序列（详见 normalizeAnthropicToolPairing
// 上层注释）：剔除未答复的 assistant.tool_calls（连同空 assistant 消息）、
// 剔除孤儿 tool 消息，并把每个 tool 结果紧贴它的 assistant 消息后排序。
//
// 同时剔除 parseToolCallArguments 解析失败（`_raw` 兜底）的非法 arguments 调用
// 及其 output —— 防上游 400 死循环（本归一化覆盖 Worker A chat_to_anthropic 的
// `_raw` 兜底，以组装后的序列为准）。最后跑一次相邻同 role 合并恢复交替。
func normalizeAnthropicToolPairing(messages []Message) []Message {
	out, droppedCalls, droppedOrphans := bridge.NormalizeAnthropicToolPairing(messages)
	if droppedCalls > 0 || droppedOrphans > 0 {
		slog.Info("normalizeAnthropicToolPairing",
			"unanswered_or_invalid_calls_dropped", droppedCalls,
			"standalone_tool_msgs_dropped", droppedOrphans)
	}
	return out
}

func convertResponsesTools(tools []ResponsesTool) []Tool {
	converted, dropped := bridge.ConvertResponsesTools(tools)
	if dropped > 0 {
		slog.Info("responses tools dropped (server-side tool types unsupported on chat path)",
			"dropped", dropped, "kept", len(converted))
	}
	return converted
}

// convertResponsesTextToResponseFormat translates the Responses API `text`
// parameter ({format:{type:...}, verbosity:...}) into the Chat Completions
// `response_format` shape ({type:...}) that upstream providers require.
//
// Returns nil when no representable format can be built (unknown type,
// missing required json_schema fields, or a non-object text value) so the
// caller can omit response_format instead of sending a malformed object that
// upstream would reject with a 400.
func convertResponsesTextToResponseFormat(text any) any {
	return bridge.ConvertResponsesTextToResponseFormat(text)
}

func responsesToolFunction(tool ResponsesTool) (ToolFunction, bool) {
	return bridge.ResponsesToolFunction(tool)
}

func responsesToolName(tool ResponsesTool) string { return bridge.ResponsesToolName(tool) }

func responsesToolKindMap(tools []ResponsesTool) map[string]string {
	return bridge.ResponsesToolKindMap(tools)
}

// includeHas reports whether the include array contains the given key.
func includeHas(include []string, key string) bool { return bridge.IncludeHas(include, key) }

func toolCallOutputType(name string, kinds map[string]string) string {
	return bridge.ToolCallOutputType(name, kinds)
}

// normalizeToolChoiceWithTools 在 convertResponsesToolChoice 结果上兜底：
// 当 tool_choice.name 指向的函数不在已保留的 Chat tools 里（例如对应的是被
// 丢弃的服务端工具 web_search/file_search/computer_use/mcp/local_shell/custom），
// 把 tool_choice 改为不传（返回 nil），避免上游因引用了不存在的工具而 400。
func normalizeToolChoiceWithTools(choice any, tools []Tool) any {
	return bridge.NormalizeToolChoiceWithTools(choice, tools)
}

func convertResponsesToolChoice(choice any) any {
	return bridge.ConvertResponsesToolChoice(choice)
}

func collectFunctionOutputs(items []any) map[string]string {
	return bridge.CollectFunctionOutputs(items)
}

// normalizeToolResultOutput is the single helper that extracts a textual
// output from a tool/function output item. It prefers the standard `output`
// field; for Anthropic-style tool_result it reads `content` when `output` is
// absent. content supports a string, a string array, or an array of
// {type:"text"|"input_text"|"output_text", text:"..."} blocks joined by
// newlines in original order. The boolean reports whether a payload was
// present (an empty string is a legitimate provided output).
func normalizeToolResultOutput(elem map[string]any) (string, bool) {
	return bridge.NormalizeToolResultOutput(elem)
}

// joinToolResultContent renders an Anthropic tool_result content value to text.
func joinToolResultContent(content any) string { return bridge.JoinToolResultContent(content) }

func parseJSONString(input string) any { return bridge.ParseJSONString(input) }

func buildBuiltInToolCallArguments(itemType string, elem map[string]any) string {
	return bridge.BuildBuiltInToolCallArguments(itemType, elem)
}

func buildResponseToolCallItem(tc ToolCall, outputType string) map[string]any {
	return bridge.BuildResponseToolCallItem(tc, outputType)
}

func cloneJSONValue[T any](value T) T { return bridge.CloneJSONValue(value) }

func storeResponseState(response map[string]any, req ResponsesAPIRequest) {
	if req.Store != nil && !*req.Store {
		return
	}
	responseID, _ := response["id"].(string)
	if responseID == "" {
		return
	}
	output, _ := response["output"].([]any)
	storedResponsesMu.Lock()
	storedResponses[responseID] = StoredResponseState{
		Model:        req.Model,
		Instructions: req.Instructions,
		Tools:        cloneJSONValue(req.Tools),
		ToolChoice:   cloneJSONValue(req.ToolChoice),
		Output:       cloneJSONValue(output),
	}
	storedResponsesMu.Unlock()
}

func loadResponseState(responseID string) (StoredResponseState, bool) {
	storedResponsesMu.RLock()
	defer storedResponsesMu.RUnlock()
	state, ok := storedResponses[responseID]
	if !ok {
		return StoredResponseState{}, false
	}
	return cloneJSONValue(state), true
}

func extractTextFromContentParts(content any) string {
	return bridge.ExtractTextFromContentParts(content)
}

func convertResponsesContentPart(part map[string]any) (map[string]any, bool) {
	return bridge.ConvertResponsesContentPart(part)
}

// into tool_use input, document source, schemas, or arbitrary domain data.
func validateClaudeDocumentBlocks(msgs []ClaudeMessage) string {
	return bridge.ValidateClaudeDocumentBlocks(msgs)
}

// when a malformed file item is found.
func validateResponsesFileItems(input any) string {
	return bridge.ValidateResponsesFileItems(input)
}

// Returns (file, true) when a usable payload exists; (nil, false) otherwise.
func responsesInputFileToFile(part map[string]any) (map[string]any, bool) {
	return bridge.ResponsesInputFileToFile(part)
}

func responsesContentToMessageContent(content any) any {
	return bridge.ResponsesContentToMessageContent(content)
}

func chatContentToResponsesContent(content any) ([]any, string) {
	return bridge.ChatContentToResponsesContent(content)
}

func responsesHandler(w http.ResponseWriter, r *http.Request) {
	auth, body, ok := readJSONRequestBody(w, r)
	if !ok {
		return
	}

	logging.MaybeBodySummary(r.Context(), "responses request body", body)

	var respReq ResponsesAPIRequest
	if err := json.Unmarshal(body, &respReq); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	modelIn := respReq.Model
	respReq.Model = resolveModelForAuth(auth, respReq.Model)
	if !validateRequestTemperature(w, respReq.Temperature, "responses", 0, 2) {
		return
	}
	if msg := validateResponsesFileItems(respReq.Input); msg != "" {
		writeProtocolValidation400(w, "responses", "input_file", msg)
		return
	}
	// Note: respReq.Messages (nonstandard compatibility field) is forwarded
	// as-is using Chat content shapes; Responses-style input_file parts are
	// not validated or converted there. Use the official `input` field for
	// input_file support.
	previousState, hasPreviousState := StoredResponseState{}, false
	if respReq.PreviousResponseID != "" {
		previousState, hasPreviousState = loadResponseState(respReq.PreviousResponseID)
		if respReq.Model == "" && previousState.Model != "" {
			respReq.Model = previousState.Model
		}
		if len(respReq.Tools) == 0 && len(previousState.Tools) > 0 {
			respReq.Tools = previousState.Tools
		}
		if respReq.ToolChoice == nil && previousState.ToolChoice != nil {
			respReq.ToolChoice = previousState.ToolChoice
		}
		// 续链时若未带 instructions，回填上一轮的系统指令（与 Tools/ToolChoice
		// 逻辑一致），保证跨轮系统提示不丢。
		if respReq.Instructions == "" && previousState.Instructions != "" {
			respReq.Instructions = previousState.Instructions
		}
	}
	if respReq.Model == "" {
		modelIDs := getModelIDs()
		if len(modelIDs) > 0 {
			respReq.Model = modelIDs[0]
		} else {
			respReq.Model = "deepseek-v4-flash-free"
		}
	}
	respReq.Model = mapPublicToFreeModel(auth, respReq.Model)

	// 协议路由：显式规则 > native-responses 运行时记忆 > 默认 chat。
	// responses 分支为既有透传；anthropic 分支延迟到 chatReq 构建完成后
	// 调用（复用 messages 转换结果）；chat 分支保持既有翻译路径。
	upstreamProto := resolveUpstreamProtocol(respReq.Model)
	if upstreamProto == upstreamProtocolResponses {
		slog.Info("responses passthrough (remembered)",
			"model_in", modelIn, "model", respReq.Model, "stream", respReq.Stream)
		if forwardNativeResponses(r.Context(), w, auth, respReq.Model, body, respReq.Stream, respReq) {
			return
		}
		// 仅传输层错误（拿不到上游响应）才会到这里，上游 4xx/5xx 已由
		// forward 原样透传状态码与错误信息。
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream connection error"}})
		return
	}

	// 多模态路由

	messages := respReq.Messages
	if len(messages) == 0 {
		if hasPreviousState && len(previousState.Output) > 0 {
			messages = append(messages, responsesInputToMessages(previousState.Output, "")...)
		}
		messages = append(messages, responsesInputToMessages(respReq.Input, respReq.Instructions)...)
	} else if respReq.Instructions != "" {
		messages = append([]Message{{Role: "system", Content: respReq.Instructions}}, messages...)
	}

	chatReq := OpenAIRequest{
		Model:    respReq.Model,
		Messages: messages,
		Stream:   respReq.Stream,
	}
	if respReq.Stream {
		chatReq.ExtraBody = map[string]any{
			"stream_options": map[string]any{"include_usage": true},
		}
	}
	if respReq.Temperature != nil {
		chatReq.Temperature = respReq.Temperature
	}
	if respReq.MaxTokens != nil {
		chatReq.MaxTokens = respReq.MaxTokens
	}
	if respReq.TopP != nil {
		chatReq.TopP = respReq.TopP
	}
	if len(respReq.Tools) > 0 {
		chatReq.Tools = convertResponsesTools(respReq.Tools)
	}
	if respReq.ToolChoice != nil {
		// normalizeToolChoiceWithTools 兜底：tool_choice 指向被丢弃的服务端
		// 工具时改为不传，避免上游因引用不存在的 function 而 400。
		chatReq.ToolChoice = normalizeToolChoiceWithTools(convertResponsesToolChoice(respReq.ToolChoice), chatReq.Tools)
	}
	if respReq.ParallelToolCalls != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["parallel_tool_calls"] = *respReq.ParallelToolCalls
	}
	if respReq.Stop != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["stop"] = respReq.Stop
	}
	if respReq.FrequencyPenalty != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["frequency_penalty"] = *respReq.FrequencyPenalty
	}
	if respReq.PresencePenalty != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["presence_penalty"] = *respReq.PresencePenalty
	}
	if respReq.User != "" {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["user"] = respReq.User
	}
	if respReq.Text != nil {
		// OpenAI Responses API `text` is {format:{type:...}, verbosity:...};
		// upstream expects Chat Completions `response_format` with a top-level
		// `type`. Translate, and drop the field entirely when it cannot be
		// represented (never send a malformed response_format upstream, which
		// would surface as a 400).
		if rf := convertResponsesTextToResponseFormat(respReq.Text); rf != nil {
			if chatReq.ExtraBody == nil {
				chatReq.ExtraBody = map[string]any{}
			}
			chatReq.ExtraBody["response_format"] = rf
		}
	}
	if respReq.Truncation != "" {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["truncation"] = respReq.Truncation
	}
	if respReq.ServiceTier != "" {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["service_tier"] = respReq.ServiceTier
	}
	if respReq.PromptCacheKey != "" {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["prompt_cache_key"] = respReq.PromptCacheKey
	}
	if respReq.SafetyIdentifier != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["safety_identifier"] = respReq.SafetyIdentifier
	}
	if respReq.TopLogprobs != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["top_logprobs"] = *respReq.TopLogprobs
	}
	if respReq.StreamOptions != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		streamOptions, ok := respReq.StreamOptions.(map[string]any)
		if !ok {
			streamOptions = map[string]any{}
		}
		if _, exists := streamOptions["include_usage"]; !exists && respReq.Stream {
			streamOptions["include_usage"] = true
		}
		chatReq.ExtraBody["stream_options"] = streamOptions
	}
	// 将 Responses API reasoning.effort 映射到 Chat Completions
	if !config.ForceDisableThinking() && respReq.Reasoning.Effort != "" {
		if respReq.Reasoning.Effort != "none" {
			chatReq.ReasoningEffort = respReq.Reasoning.Effort
		}
	}

	// 协议路由 anthropic 分支：chatReq（含 messages 转换/多模态/text-only 降级）
	// 已就绪，转交 chatToAnthropicBody 走上游原生 /v1/messages。
	if upstreamProto == upstreamProtocolAnthropic {
		// 与下方 keepReasoning 语义一致：fixToolCallGaps/ensureReasoningContent
		// 属于 chat 翻译路径的修补，交叉路径由 chatToAnthropicBody 自行处理。
		// 组装前先做 tool_use/tool_result 配对归一化（见
		// normalizeAnthropicToolPairing；发生在 chatMessagesToAnthropic 之前）。
		chatReq.Messages = normalizeAnthropicToolPairing(chatReq.Messages)
		// 转 Anthropic 时 service_tier 仅放行上游白名单（auto/standard_only）；
		// 其余值（priority/flex 等 OpenAI 口径）丢弃，避免上游 400。
		if respReq.ServiceTier != "" {
			switch respReq.ServiceTier {
			case "auto", "standard_only":
				if chatReq.ExtraBody == nil {
					chatReq.ExtraBody = map[string]any{}
				}
				chatReq.ExtraBody["service_tier"] = respReq.ServiceTier
			default:
				slog.Info("responses service_tier dropped for anthropic upstream",
					"model", chatReq.Model, "service_tier", respReq.ServiceTier)
			}
		}
		wantReasoningX := !config.ForceDisableThinking()
		forwardResponsesViaAnthropic(w, r, auth, &chatReq, wantReasoningX)
		return
	}

	wantReasoning := !config.ForceDisableThinking()
	chatReq.Messages = fixToolCallGaps(chatReq.Messages)
	keepReasoning := wantsReasoning(&chatReq)
	chatReq.Messages = ensureReasoningContent(chatReq.Messages, keepReasoning)

	effortIn := chatReq.ReasoningEffort
	if effortIn == "" {
		effortIn = respReq.Reasoning.Effort
	}
	upstreamSurface := "zen"
	if auth.shouldUseGoEndpoint(chatReq.Model) {
		upstreamSurface = "go"
	}
	logging.PlanRequest(r.Context(), map[string]any{
		"protocol":             "responses",
		"model_in":             modelIn,
		"model_resolved":       chatReq.Model,
		"auth_mode":            authModeString(auth.Mode),
		"auth_source":          auth.Source,
		"has_key":              auth.Token != "",
		"upstream_surface":     upstreamSurface,
		"stream":               respReq.Stream,
		"keep_reasoning":       keepReasoning,
		"thinking":             thinkingState(nil),
		"reasoning_effort_in":  effortIn,
		"reasoning_effort_out": mappedReasoningEffort(effortIn),
		"tools_count":          len(respReq.Tools),
		"messages_count":       len(chatReq.Messages),
		"multimodal_parts":     countMultimodalParts(chatReq.Messages),
		"text_only_model":      modelIsTextOnly(chatReq.Model),
		"max_tokens":           chatReq.MaxTokens,
		"max_tokens_cap":       config.MaxTokensCapFor(chatReq.Model),
	})

	upstreamBody := buildUpstreamBody(&chatReq)

	if respReq.Stream {
		upResp, status, _, err := callOpenCodeAPIStream(r.Context(), upstreamBody, chatReq.Model, auth)
		if err != nil || status < 200 || status >= 300 {
			// 先保留翻译路径的上游错误体，再探测原生透传。
			var transErrBody []byte
			if upResp != nil {
				transErrBody, _ = io.ReadAll(upResp)
				upResp.Close()
			}
			// 翻译路径失败：探测上游原生 responses，成功则透传并记住该模型。
			// 类型化转换错误（上游有明确错误信息）不探测，原样返回。
			if shouldProbeNativeResponses(status, err) && probeNativeResponses(r.Context(), w, auth, chatReq.Model, body, true, respReq) {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if len(transErrBody) > 0 {
				w.Write(transErrBody)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error"}})
			return
		}
		defer upResp.Close()

		resp := &http.Response{
			StatusCode: status,
			Body:       upResp,
			Header:     make(http.Header),
		}
		responsesStreamHandler(w, r, resp, chatReq.Model, chatReq.Model, wantReasoning, respReq.Tools, respReq.ToolChoice, respReq)
		return
	}

	respBody, status, _, err := callOpenCodeAPI(r.Context(), upstreamBody, chatReq.Model, auth)
	if err != nil || status < 200 || status >= 300 {
		// 翻译路径失败：探测上游原生 responses，成功则透传并记住该模型。
		// 类型化转换错误（上游有明确错误信息）不探测，原样返回。
		if shouldProbeNativeResponses(status, err) && probeNativeResponses(r.Context(), w, auth, chatReq.Model, body, false, respReq) {
			return
		}
		if err != nil {
			writeUpstreamError(w, status, err, "responses")
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if len(respBody) > 0 {
				w.Write(respBody)
			} else {
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error"}})
			}
		}
		return
	}

	responsesBody := convertChatToResponses(respBody, chatReq.Model, wantReasoning, respReq.Tools, respReq.ToolChoice, respReq.Include)
	var responseMap map[string]any
	if json.Unmarshal(responsesBody, &responseMap) == nil {
		applyResponsesRequestEcho(responseMap, respReq)
		if enriched, marshalErr := json.Marshal(responseMap); marshalErr == nil {
			responsesBody = enriched
		}
		storeResponseState(responseMap, respReq)
	}

	result := logging.SummarizeChatResult(respBody)
	logging.LogResult(r.Context(), result)

	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			statsx.RecordChatUsage(chatReq.Model, u)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	logging.MaybeBodySummary(r.Context(), "responses response body", responsesBody)
	w.Write(responsesBody)
}

// ======================== Responses Stream Handler ========================

func responsesInputTokensDetails(details any) map[string]any {
	return bridge.ResponsesInputTokensDetails(details)
}

func responsesStreamHandler(w http.ResponseWriter, r *http.Request, resp *http.Response, model string, _ string, wantReasoning bool, tools []ResponsesTool, toolChoice any, originalReq ResponsesAPIRequest) {
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	stats := &logging.StreamStats{Start: time.Now()}

	responseID := "resp_" + time.Now().Format("20060102150405") + "_" + randomString(8)
	reasoningID := "rs_" + responseID
	msgID := "msg_" + responseID + "_0"
	createdAt := time.Now().Unix()
	seq := 0

	reasoningStarted := false
	reasoningDone := false
	messageStarted := false
	messageDone := false
	fullReasoning := ""
	fullText := ""
	fullRefusal := ""
	refusalStarted := false
	totalUsage := map[string]any{}
	createdSent := false
	terminalStatus := "completed"
	terminalEvent := "response.completed"
	itemStatus := "completed"
	finished := false
	// Some upstreams (e.g. muse-spark-1.2-contributor-free) terminate a stream
	// with a usage-only chunk but no finish_reason and no [DONE]. When the turn
	// produced output and we saw a terminal usage chunk, synthesize completion.
	usageTerminalSeen := false
	toolCalls := map[int]map[string]any{}
	toolOrder := []int{}
	toolKinds := responsesToolKindMap(tools)
	indexAllocator := outputIndexAllocator{}
	reasoningOutputIndex := -1
	messageIndex := -1

	reader := newStreamReader(ctx, resp.Body, 0)

	defer func() {
		stats.TextChars = len(fullText)
		stats.ReasoningChars = len(fullReasoning)
		stats.ToolCallCount = len(toolOrder)
		stats.Log(ctx, "responses")
	}()
	// Reader cleanup: signal goroutine, unblock any pending read, wait for exit.
	defer reader.Close()

	messageOutputIndex := func() int {
		if messageIndex < 0 {
			messageIndex = indexAllocator.Allocate()
		}
		return messageIndex
	}

	reasoningItem := func(status string) map[string]any {
		item := map[string]any{
			"id":      reasoningID,
			"type":    "reasoning",
			"summary": []any{},
		}
		if status != "" {
			item["status"] = status
		}
		if status == "completed" && includeHas(originalReq.Include, "reasoning.encrypted_content") {
			item["encrypted_content"] = ""
		}
		if fullReasoning != "" {
			item["summary"] = []any{map[string]any{"type": "summary_text", "text": fullReasoning}}
		}
		return item
	}

	messageItem := func(status string) map[string]any {
		content := []any{}
		if fullRefusal != "" {
			content = append(content, map[string]any{
				"type":    "refusal",
				"refusal": fullRefusal,
			})
		}
		content = append(content, map[string]any{
			"type":        "output_text",
			"annotations": []any{},
			"logprobs":    []any{},
			"text":        fullText,
		})
		return map[string]any{
			"id":      msgID,
			"type":    "message",
			"status":  status,
			"content": content,
			"role":    "assistant",
		}
	}

	emitReasoningDone := func() {
		if !reasoningStarted || reasoningDone {
			return
		}
		seq++
		writeSSEEvent(w, flusher, "response.reasoning_summary_text.done", map[string]any{
			"type":            "response.reasoning_summary_text.done",
			"sequence_number": seq,
			"item_id":         reasoningID,
			"output_index":    reasoningOutputIndex,
			"summary_index":   0,
			"text":            fullReasoning,
		})
		seq++
		writeSSEEvent(w, flusher, "response.reasoning_summary_part.done", map[string]any{
			"type":            "response.reasoning_summary_part.done",
			"sequence_number": seq,
			"item_id":         reasoningID,
			"output_index":    reasoningOutputIndex,
			"summary_index":   0,
			"part":            map[string]any{"type": "summary_text", "text": fullReasoning},
		})
		seq++
		writeSSEEvent(w, flusher, "response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
			"sequence_number": seq,
			"output_index":    reasoningOutputIndex,
			"item":            reasoningItem(itemStatus),
		})
		reasoningDone = true
	}

	emitMessageDone := func() {
		if !messageStarted || messageDone {
			return
		}
		idx := messageOutputIndex()
		seq++
		writeSSEEvent(w, flusher, "response.output_text.done", map[string]any{
			"type":            "response.output_text.done",
			"sequence_number": seq,
			"item_id":         msgID,
			"output_index":    idx,
			"content_index":   0,
			"text":            fullText,
			"logprobs":        []any{},
		})
		seq++
		writeSSEEvent(w, flusher, "response.content_part.done", map[string]any{
			"type":            "response.content_part.done",
			"sequence_number": seq,
			"item_id":         msgID,
			"output_index":    idx,
			"content_index":   0,
			"part":            map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": fullText},
		})
		seq++
		writeSSEEvent(w, flusher, "response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
			"sequence_number": seq,
			"output_index":    idx,
			"item":            messageItem(itemStatus),
		})
		messageDone = true
	}

	emitRefusalDone := func() {
		if !refusalStarted {
			return
		}
		idx := messageOutputIndex()
		seq++
		writeSSEEvent(w, flusher, "response.refusal.done", map[string]any{
			"type":            "response.refusal.done",
			"sequence_number": seq,
			"item_id":         msgID,
			"output_index":    idx,
			"content_index":   0,
			"refusal":         fullRefusal,
		})
	}

	emitToolCallDone := func(idx int, call map[string]any) {
		if done, _ := call["done"].(bool); done {
			return
		}
		call["done"] = true
		itemID, _ := call["item_id"].(string)
		callID, _ := call["call_id"].(string)
		name, _ := call["name"].(string)
		args, _ := call["arguments"].(string)
		seq++
		writeSSEEvent(w, flusher, "response.function_call_arguments.done", map[string]any{
			"type":            "response.function_call_arguments.done",
			"sequence_number": seq,
			"item_id":         itemID,
			"output_index":    idx,
			"name":            name,
			"arguments":       args,
		})
		seq++
		itemType, _ := call["item_type"].(string)
		if itemType == "" {
			itemType = "function_call"
		}
		item := buildResponseToolCallItem(ToolCall{ID: callID, Function: FunctionCall{Name: name, Arguments: args}}, itemType)
		item["status"] = itemStatus
		writeSSEEvent(w, flusher, "response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
			"sequence_number": seq,
			"output_index":    idx,
			"item":            item,
		})
	}

	ensureCreated := func(chunk map[string]any) {
		if createdSent {
			return
		}
		if chunk != nil {
			if id, ok := chunk["id"].(string); ok && id != "" {
				responseID = normalizeResponsesID(id)
				reasoningID = "rs_" + responseID + "_0"
				msgID = "msg_" + responseID + "_0"
			}
			if created, ok := chunk["created"].(float64); ok {
				createdAt = int64(created)
			}
		}
		seq++
		writeSSEEvent(w, flusher, "response.created", map[string]any{
			"type":            "response.created",
			"sequence_number": seq,
			"response":        map[string]any{"id": responseID, "object": "response", "created_at": createdAt, "status": "in_progress", "background": false, "error": nil, "output": []any{}},
		})
		seq++
		writeSSEEvent(w, flusher, "response.in_progress", map[string]any{
			"type":            "response.in_progress",
			"sequence_number": seq,
			"response":        map[string]any{"id": responseID, "object": "response", "created_at": createdAt, "status": "in_progress"},
		})
		createdSent = true
	}

	emitResponseFailed := func(msg string) {
		ensureCreated(nil)
		failedResponse := map[string]any{
			"id":         responseID,
			"object":     "response",
			"created_at": createdAt,
			"status":     "failed",
			"background": false,
			"error": map[string]any{
				"code":    "server_error",
				"message": msg,
			},
			"incomplete_details": nil,
			"model":              model,
			"output":             []any{},
		}
		applyResponsesRequestEcho(failedResponse, originalReq)
		seq++
		writeSSEEvent(w, flusher, "response.failed", map[string]any{
			"type":            "response.failed",
			"sequence_number": seq,
			"response":        failedResponse,
		})
		if flusher != nil {
			flusher.Flush()
		}
	}

loop:
	for {
		select {
		case <-ctx.Done():
			// Client cancelled: quiet exit, no error writes.
			return
		case result := <-reader.Read():
			// bufio.ReadString may return both a non-empty line and an error
			// (e.g. the last line without a trailing newline + io.EOF). Process
			// the line first, then handle the accompanying error via pendingErr.
			pendingErr := result.err

			line := result.line
			trimmed := strings.TrimSpace(line)
			if trimmed == "data: [DONE]" || trimmed == "[DONE]" {
				stats.DoneSeen = true
				if !finished {
					if usageTerminalSeen && (messageStarted || reasoningStarted || len(toolCalls) > 0) {
						stats.SawFinish = true
						stats.FinishReason = "stop"
						finished = true
						break loop
					}
					emitResponseFailed("stream ended with [DONE] but no finish_reason")
					return
				}
				break loop
			}
			if strings.HasPrefix(line, "data: ") {
				payload := line[6:]
				if strings.TrimSpace(payload) != "" {
					var chunk map[string]any
					if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
						emitResponseFailed("stream received malformed JSON data")
						return
					} else {
						// In-band error from upstream.
						if errVal, ok := chunk["error"]; ok && errVal != nil {
							errMsg := "upstream stream error"
							if errMap, ok := errVal.(map[string]any); ok {
								if m, ok := errMap["message"].(string); ok && m != "" {
									errMsg = m
								}
							} else if errStr, ok := errVal.(string); ok && errStr != "" {
								errMsg = errStr
							}
							emitResponseFailed(errMsg)
							return
						} else {
							stats.NoteChunk()
							ensureCreated(chunk)
							choices, ok := chunk["choices"].([]any)
							if !ok || len(choices) == 0 {
								if usage, ok := chunk["usage"].(map[string]any); ok {
									totalUsage = usage
									if usageHasCompletion(usage) {
										usageTerminalSeen = true
									}
								}
							} else {
								choice, _ := choices[0].(map[string]any)
								delta, _ := choice["delta"].(map[string]any)
								finishReason, _ := choice["finish_reason"].(string)
								if finishReason != "" {
									stats.FinishReason = finishReason
									stats.SawFinish = true
								}

								if !finished {
									if rc, ok := delta["reasoning_content"]; ok && wantReasoning {
										rcStr, _ := rc.(string)
										if rcStr != "" {
											if !reasoningStarted {
												reasoningOutputIndex = indexAllocator.Allocate()
												seq++
												writeSSEEvent(w, flusher, "response.output_item.added", map[string]any{
													"type":            "response.output_item.added",
													"sequence_number": seq,
													"output_index":    reasoningOutputIndex,
													"item":            reasoningItem("in_progress"),
												})
												seq++
												writeSSEEvent(w, flusher, "response.reasoning_summary_part.added", map[string]any{
													"type":            "response.reasoning_summary_part.added",
													"sequence_number": seq,
													"item_id":         reasoningID,
													"output_index":    reasoningOutputIndex,
													"summary_index":   0,
													"part":            map[string]any{"type": "summary_text", "text": ""},
												})
												reasoningStarted = true
											}
											fullReasoning += rcStr
											seq++
											writeSSEEvent(w, flusher, "response.reasoning_summary_text.delta", map[string]any{
												"type":            "response.reasoning_summary_text.delta",
												"sequence_number": seq,
												"item_id":         reasoningID,
												"output_index":    reasoningOutputIndex,
												"summary_index":   0,
												"delta":           rcStr,
											})
										}
									}

									contentStr := ""
									if c, ok := delta["content"]; ok && c != nil {
										contentStr, _ = c.(string)
									}
									// #37635: when thinking is not kept, promote misplaced reasoning to visible text.
									if contentStr == "" && !wantReasoning {
										if rc, ok := delta["reasoning_content"].(string); ok {
											if rc != "" {
												stats.PromotedReasoning = true
											}
											contentStr = rc
										}
									}
									if contentStr != "" {
										// The terminal finish reason determines the item's final status. Keep the
										// reasoning item open until that reason is known so a truncation cannot
										// first announce it as completed.
										if !messageStarted {
											idx := messageOutputIndex()
											seq++
											writeSSEEvent(w, flusher, "response.output_item.added", map[string]any{
												"type":            "response.output_item.added",
												"sequence_number": seq,
												"output_index":    idx,
												"item":            map[string]any{"id": msgID, "type": "message", "status": "in_progress", "content": []any{}, "role": "assistant"},
											})
											seq++
											writeSSEEvent(w, flusher, "response.content_part.added", map[string]any{
												"type":            "response.content_part.added",
												"sequence_number": seq,
												"item_id":         msgID,
												"output_index":    idx,
												"content_index":   0,
												"part":            map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": ""},
											})
											messageStarted = true
										}
										fullText += contentStr
										seq++
										writeSSEEvent(w, flusher, "response.output_text.delta", map[string]any{
											"type":            "response.output_text.delta",
											"sequence_number": seq,
											"item_id":         msgID,
											"output_index":    messageOutputIndex(),
											"content_index":   0,
											"delta":           contentStr,
											"logprobs":        []any{},
										})
									}

									if refusalStr, ok := delta["refusal"].(string); ok && refusalStr != "" {
										if !refusalStarted {
											refusalStarted = true
										}
										fullRefusal += refusalStr
										seq++
										writeSSEEvent(w, flusher, "response.refusal.delta", map[string]any{
											"type":            "response.refusal.delta",
											"sequence_number": seq,
											"item_id":         msgID,
											"output_index":    messageOutputIndex(),
											"content_index":   0,
											"delta":           refusalStr,
										})
									}

									rawToolCalls, _ := delta["tool_calls"].([]any)
									for _, rawToolCall := range rawToolCalls {
										tc, ok := rawToolCall.(map[string]any)
										if !ok {
											continue
										}
										idxFloat, _ := tc["index"].(float64)
										upstreamIndex := int(idxFloat)
										call, exists := toolCalls[upstreamIndex]
										if !exists {
											outputIndex := indexAllocator.Allocate()
											callID, _ := tc["id"].(string)
											if callID == "" {
												callID = "call_" + randomString(12)
											}
											fn, _ := tc["function"].(map[string]any)
											name, _ := fn["name"].(string)
											itemType := toolCallOutputType(name, toolKinds)
											call = map[string]any{
												"output_index": outputIndex,
												"item_id":      "fc_" + callID,
												"call_id":      callID,
												"name":         name,
												"arguments":    "",
												"done":         false,
												"item_type":    itemType,
											}
											toolCalls[upstreamIndex] = call
											toolOrder = append(toolOrder, upstreamIndex)
											seq++
											writeSSEEvent(w, flusher, "response.output_item.added", map[string]any{
												"type":            "response.output_item.added",
												"sequence_number": seq,
												"output_index":    outputIndex,
												"item": map[string]any{
													"id":        call["item_id"],
													"type":      itemType,
													"status":    "in_progress",
													"arguments": "",
													"call_id":   callID,
													"name":      name,
												},
											})
										}
										fn, _ := tc["function"].(map[string]any)
										if name, _ := fn["name"].(string); name != "" {
											call["name"] = name
											if call["item_type"] == "function_call" {
												call["item_type"] = toolCallOutputType(name, toolKinds)
											}
										}
										if argDelta, _ := fn["arguments"].(string); argDelta != "" {
											call["arguments"] = call["arguments"].(string) + argDelta
											seq++
											writeSSEEvent(w, flusher, "response.function_call_arguments.delta", map[string]any{
												"type":            "response.function_call_arguments.delta",
												"sequence_number": seq,
												"item_id":         call["item_id"],
												"output_index":    call["output_index"],
												"delta":           argDelta,
											})
										}
									}

									if usage, ok := chunk["usage"].(map[string]any); ok {
										totalUsage = usage
									}
									if finishReason == "stop" || finishReason == "length" || finishReason == "tool_calls" || finishReason == "function_call" || finishReason == "content_filter" {
										finished = true
										if finishReason == "length" {
											terminalStatus = "incomplete"
											terminalEvent = "response.incomplete"
											itemStatus = "incomplete"
										}
										// Do not emit done events yet: a trailing error
										// must still produce response.failed without any
										// status=completed item.done. Done events are
										// emitted only after the loop exits cleanly.
									}
								} else {
									// After finish_reason, only look for usage-only trailing chunks.
									if usage, ok := chunk["usage"].(map[string]any); ok {
										totalUsage = usage
									}
								}
							}
						}
					}
				}
			}

			// Now handle a pending error from the read.
			if pendingErr != nil {
				if pendingErr == io.EOF {
					if !finished {
						if usageTerminalSeen && (messageStarted || reasoningStarted || len(toolCalls) > 0) {
							stats.SawFinish = true
							stats.FinishReason = "stop"
							finished = true
							break loop
						}
						emitResponseFailed("stream ended without finish_reason")
						return
					}
					break loop
				}
				logging.FromContext(ctx).Error("stream read error", "error", pendingErr)
				emitResponseFailed("stream read error")
				return
			}
		}
	}

	// Reached only when finished is true.
	emitReasoningDone()
	emitRefusalDone()
	if !messageStarted && len(toolCalls) == 0 {
		idx := messageOutputIndex()
		seq++
		writeSSEEvent(w, flusher, "response.output_item.added", map[string]any{
			"type":            "response.output_item.added",
			"sequence_number": seq,
			"output_index":    idx,
			"item":            map[string]any{"id": msgID, "type": "message", "status": "in_progress", "content": []any{}, "role": "assistant"},
		})
		seq++
		writeSSEEvent(w, flusher, "response.content_part.added", map[string]any{
			"type":            "response.content_part.added",
			"sequence_number": seq,
			"item_id":         msgID,
			"output_index":    idx,
			"content_index":   0,
			"part":            map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": ""},
		})
		messageStarted = true
	}
	emitMessageDone()
	for _, idx := range toolOrder {
		emitToolCallDone(toolCalls[idx]["output_index"].(int), toolCalls[idx])
	}

	output := make([]any, indexAllocator.Len())
	if reasoningStarted {
		output[reasoningOutputIndex] = reasoningItem(itemStatus)
	}
	if messageStarted {
		output[messageIndex] = messageItem(itemStatus)
	}
	for _, idx := range toolOrder {
		call := toolCalls[idx]
		itemType, _ := call["item_type"].(string)
		if itemType == "" {
			itemType = "function_call"
		}
		item := buildResponseToolCallItem(ToolCall{
			ID: call["call_id"].(string),
			Function: FunctionCall{
				Name:      call["name"].(string),
				Arguments: call["arguments"].(string),
			},
		}, itemType)
		item["status"] = itemStatus
		output[call["output_index"].(int)] = item
	}

	completedResponse := map[string]any{
		"id":                 responseID,
		"object":             "response",
		"created_at":         createdAt,
		"status":             terminalStatus,
		"background":         false,
		"error":              nil,
		"incomplete_details": nil,
		"model":              model,
		"output":             output,
	}
	if terminalStatus == "incomplete" {
		completedResponse["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	applyResponsesRequestEcho(completedResponse, originalReq)
	if len(tools) > 0 {
		completedResponse["tools"] = tools
	}
	if toolChoice != nil {
		completedResponse["tool_choice"] = toolChoice
	}

	if len(totalUsage) > 0 {
		usage := map[string]any{}
		if v, ok := totalUsage["prompt_tokens"]; ok {
			usage["input_tokens"] = v
		}
		usage["input_tokens_details"] = responsesInputTokensDetails(totalUsage["prompt_tokens_details"])
		if v, ok := totalUsage["completion_tokens"]; ok {
			usage["output_tokens"] = v
		}
		if v, ok := totalUsage["completion_tokens_details"]; ok {
			usage["output_tokens_details"] = v
		}
		if v, ok := totalUsage["total_tokens"]; ok {
			usage["total_tokens"] = v
		}
		if v, ok := totalUsage["input_tokens"]; ok && usage["input_tokens"] == nil {
			usage["input_tokens"] = v
		}
		if v, ok := totalUsage["output_tokens"]; ok && usage["output_tokens"] == nil {
			usage["output_tokens"] = v
		}
		completedResponse["usage"] = usage
	}

	if totalUsage != nil {
		statsx.RecordChatUsage(model, totalUsage)
	}

	seq++
	writeSSEEvent(w, flusher, terminalEvent, map[string]any{
		"type":            terminalEvent,
		"sequence_number": seq,
		"response":        completedResponse,
	})

	if flusher != nil {
		flusher.Flush()
	}
	storeResponseState(completedResponse, originalReq)
}

func convertChatToResponses(chatBody []byte, model string, wantReasoning bool, tools []ResponsesTool, toolChoice any, include []string) []byte {
	return bridge.ConvertChatToResponses(randomIDGen, chatBody, model, wantReasoning, tools, toolChoice, include)
}

// emptyAssistantMessageItem 构造条 status 一致的空 output_text message。
func emptyAssistantMessageItem(outputID, status string) map[string]any {
	return bridge.EmptyAssistantMessageItem(outputID, status)
}

// chatUsageMapToResponses 把 Chat Completions usage（上游 zen/go 口径）转
// Responses 口径：
//   - input_tokens = prompt_tokens + 顶层 cache_read_input_tokens +
//     cache_creation_input_tokens（anthropicUsageToChat 已把这两个顶层字段透传
//     进 chat usage，而 prompt_tokens 是缓存感知的读数，按 Responses 口径加回）。
//     无缓存字段时退化为原 prompt_tokens（保持既有行为）。
//   - input_tokens_details.cached_tokens 优先取顶层 cache_read_input_tokens
//     （未见时退回 prompt_tokens_details.cached_tokens，再兜底 0）。
//   - output_tokens_details 从 completion_tokens_details 透传
//     reasoning_tokens（thinking token）等细节。
func chatUsageMapToResponses(u map[string]any) map[string]any {
	return bridge.ChatUsageMapToResponses(u)
}

// reasoningTokenEstimate 从 usage 中提取 thinking/reasoning token 数量的兜底
// 估算：缺 completion_tokens_details 时按已知顶层/通用键查找。
func reasoningTokenEstimate(u map[string]any) int64 { return bridge.ReasoningTokenEstimate(u) }
