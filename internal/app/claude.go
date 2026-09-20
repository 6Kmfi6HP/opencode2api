package app

import (
	"bufio"
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
	"sync"
	"time"
)

func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, event string, data any) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return
	}
	_, _ = w.Write([]byte("event: " + event + "\n"))
	_, _ = w.Write([]byte("data: " + string(jsonData) + "\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

// streamReadResult carries one line read from an upstream SSE body. A
// non-empty line may arrive together with a non-nil Err (e.g. a final line
// without trailing newline + io.EOF); callers must process Line first.
type streamReadResult struct {
	line string
	err  error
}

// streamReader owns the reader goroutine shared by the SSE stream handlers:
// it reads lines off the upstream body and forwards them on readCh so the
// handler loop can keep selecting on read/keepalive/context without
// blocking. Lines stop flowing when ctx is done or Close has been called.
type streamReader struct {
	ctx           context.Context
	body          io.Reader
	readCh        chan streamReadResult
	done          chan struct{}
	exited        chan struct{}
	keepCh        <-chan time.Time
	stopKeepalive func()
	closeOnce     sync.Once
}

// newStreamReader starts the reader goroutine. A keepaliveInterval <= 0
// disables the keepalive channel (Keepalive then returns nil, which blocks
// forever in a select case).
func newStreamReader(ctx context.Context, body io.Reader, keepaliveInterval time.Duration) *streamReader {
	r := &streamReader{
		ctx:    ctx,
		body:   body,
		readCh: make(chan streamReadResult),
		done:   make(chan struct{}),
		exited: make(chan struct{}),
	}
	if keepaliveInterval > 0 {
		ticker := time.NewTicker(keepaliveInterval)
		r.keepCh = ticker.C
		r.stopKeepalive = ticker.Stop
	}
	go func() {
		defer close(r.exited)
		reader := bufio.NewReader(body)
		for {
			line, err := reader.ReadString('\n')
			select {
			case r.readCh <- streamReadResult{line: line, err: err}:
			case <-r.done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return r
}

// Read delivers the next line read from the upstream body.
func (r *streamReader) Read() <-chan streamReadResult { return r.readCh }

// Keepalive ticks at the configured interval, or never when keepalive is
// disabled.
func (r *streamReader) Keepalive() <-chan time.Time { return r.keepCh }

// Close stops the reader: it signals the goroutine, unblocks any pending
// read by closing the upstream body, and waits for the goroutine to exit.
func (r *streamReader) Close() {
	r.closeOnce.Do(func() {
		close(r.done)
		if c, ok := r.body.(io.Closer); ok {
			c.Close()
		}
		if r.stopKeepalive != nil {
			r.stopKeepalive()
		}
		<-r.exited
	})
}

// ======================== Claude Messages API ========================

// extractClaudeSystemText 提取 Claude system 字段的纯文本。薄壳转发到 bridge。
func extractClaudeSystemText(system any) string {
	return bridge.ExtractClaudeSystemText(system)
}

// cleanJsonSchema 递归清理 JSON Schema（剥注解键）。薄壳转发到 bridge。
func cleanJsonSchema(schema any) any {
	return bridge.CleanJSONSchema(schema)
}

// claudeImageBlockToOpenAI 把 Anthropic image block 转为 Chat image_url part。
// 薄壳转发到 bridge。
func claudeImageBlockToOpenAI(block map[string]any) (map[string]any, bool) {
	return bridge.ClaudeImageBlockToOpenAI(block)
}

// claudeDocumentBlockToOpenAI maps an Anthropic document content block to a
// Chat Completions file content part. Thin shell forwarding to bridge.
func claudeDocumentBlockToOpenAI(block map[string]any) (map[string]any, bool) {
	return bridge.ClaudeDocumentBlockToOpenAI(block)
}

// extractClaudeContentText 提取任意 Claude content 的纯文本。薄壳转发到 bridge。
func extractClaudeContentText(content any) string {
	return bridge.ExtractClaudeContentText(content)
}

// claudeToOpenAIMessages 把 Claude messages + system 转为 Chat messages。
// 薄壳转发到 bridge。
func claudeToOpenAIMessages(claudeMsgs []ClaudeMessage, system any) []Message {
	return bridge.ClaudeToOpenAIMessages(claudeMsgs, system)
}

// claudeToOpenAITools 把 Claude tools 转为 Chat function tools（server tools
// 跳过并记入 skipped）。薄壳转发到 bridge。
func claudeToOpenAITools(claudeTools []ClaudeTool) ([]Tool, []string) {
	return bridge.ClaudeToOpenAITools(claudeTools)
}

func countClaudeSystemParts(msgs []ClaudeMessage, system any) int {
	n := 0
	if extractClaudeSystemText(system) != "" {
		n++
	}
	for _, msg := range msgs {
		if msg.Role == "system" && extractClaudeContentText(msg.Content) != "" {
			n++
		}
	}
	return n
}

func countAnthropicBetas(header string) int {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	n := 0
	for _, part := range strings.Split(header, ",") {
		if strings.TrimSpace(part) != "" {
			n++
		}
	}
	return n
}

// countCacheControlInValue counts cache_control breakpoints on content
// blocks within system, message content, and tool_result content arrays. It
// recurses into all values except input_schema and tool_use input, so a
// property named cache_control inside a schema or input object is not
// falsely counted as a breakpoint.
func countCacheControlInValue(v any) int {
	switch x := v.(type) {
	case map[string]any:
		n := 0
		if _, ok := x["cache_control"]; ok {
			n++
		}
		for key, child := range x {
			// Skip input_schema and input — cache_control inside these is a
			// schema/input property, not a content-block breakpoint.
			if key == "input_schema" || key == "input" {
				continue
			}
			n += countCacheControlInValue(child)
		}
		return n
	case []any:
		n := 0
		for _, child := range x {
			n += countCacheControlInValue(child)
		}
		return n
	default:
		return 0
	}
}

func countClaudeCacheControlBlocks(req ClaudeRequest) int {
	n := countCacheControlInValue(req.System)
	for _, msg := range req.Messages {
		n += countCacheControlInValue(msg.Content)
	}
	// Count actual tool-level cache_control breakpoints on tool definitions,
	// not properties named cache_control inside input_schema.
	for _, tool := range req.Tools {
		if tool.CacheControl != nil {
			n++
		}
	}
	return n
}

// countClaudeThinkingSignatures counts non-empty signature fields on
// thinking content blocks at the top level of each message's content array.
// Only actual message content blocks are counted — not nested values inside
// tool_use input or other objects that happen to have type:"thinking" and a
// signature key. The signature content itself is never recorded; only the
// count is exposed in request_plan for observability. These signatures have
// no Chat Completions equivalent and are dropped upstream.
func countClaudeThinkingSignatures(msgs []ClaudeMessage) int {
	var n int
	for _, msg := range msgs {
		blocks, ok := msg.Content.([]any)
		if !ok {
			continue
		}
		for _, item := range blocks {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := block["type"].(string); t == "thinking" {
				if sig, ok := block["signature"].(string); ok && sig != "" {
					n++
				}
			}
		}
	}
	return n
}

// listed here so it is not counted as unsupported.
var claudeUnsupportedBlockTypes = map[string]struct{}{
	"redacted_thinking":               {},
	"search_result":                   {},
	"server_tool_use":                 {},
	"web_search_tool_result":          {},
	"container_upload":                {},
	"code_execution_tool_use":         {},
	"code_execution_tool_result":      {},
	"mcp_tool_use":                    {},
	"mcp_tool_result":                 {},
	"bash_code_execution_tool_result": {},
	"web_fetch_tool_result":           {},
	"tool_reference":                  {},
}

func scanClaudeUnsupportedBlocks(msgs []ClaudeMessage) map[string]int {
	counts := map[string]int{}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if t, _ := x["type"].(string); t != "" {
				if _, ok := claudeUnsupportedBlockTypes[t]; ok {
					counts[t]++
				}
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	for _, msg := range msgs {
		walk(msg.Content)
	}
	if len(counts) == 0 {
		return nil
	}
	return counts
}

// 纯核已迁入 bridge.OpenAIToClaudeResponse；薄壳注入 randomIDGen。原实现对
// 顶层反序列化失败仅 slog.Warn 后继续，纯层静默透传（保持一致的字节行为）。
func openAIToClaudeResponse(chatBody []byte, model string, wantReasoning bool) []byte {
	return bridge.OpenAIToClaudeResponse(randomIDGen, chatBody, model, wantReasoning)
}

func toFloat64(v any) float64 {
	return bridge.ToFloat64(v)
}

func usageIntField(fields map[string]any, key string) (int, bool) {
	if fields == nil {
		return 0, false
	}
	value, ok := fields[key]
	if !ok || value == nil {
		return 0, false
	}
	return int(toFloat64(value)), true
}

func usageMapField(fields map[string]any, key string) (map[string]any, bool) {
	if fields == nil {
		return nil, false
	}
	value, ok := fields[key]
	if !ok || value == nil {
		return nil, false
	}
	mapped, ok := value.(map[string]any)
	return mapped, ok
}

func buildClaudeUsageCore(upstreamUsage map[string]any) ClaudeUsage {
	return bridge.BuildClaudeUsageCore(upstreamUsage)
}

func buildClaudeMessageUsage(upstreamUsage map[string]any) ClaudeUsage {
	return bridge.BuildClaudeMessageUsage(upstreamUsage)
}

func buildClaudeDeltaUsage(upstreamUsage map[string]any) ClaudeUsage {
	return bridge.BuildClaudeDeltaUsage(upstreamUsage)
}

func claudeMessagesHandler(w http.ResponseWriter, r *http.Request) {
	auth, body, ok := readJSONRequestBody(w, r)
	if !ok {
		return
	}

	logging.MaybeBodySummary(r.Context(), "claude messages request body", body)

	var claudeReq ClaudeRequest
	if err := json.Unmarshal(body, &claudeReq); err != nil {
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"Invalid JSON"}}`, http.StatusBadRequest)
		return
	}
	modelIn := claudeReq.Model
	claudeReq.Model = resolveModelForAuth(auth, claudeReq.Model)
	claudeReq.Model = mapPublicToFreeModel(auth, claudeReq.Model)
	if !validateRequestTemperature(w, claudeReq.Temperature, "claude", 0, 1) {
		return
	}

	// 协议路由：显式规则 > native-responses 运行时记忆 > 默认 chat。
	// 规则命中 anthropic 时 body 直通上游原生 /v1/messages；命中 responses
	// 时走 Claude->Responses 转换快路径（lenient，不返回 400，跳过
	// validateClaudeDocumentBlocks 严格校验）。仅传输层错误回落 chat 翻译。
	wantReasoningEarly := !config.ForceDisableThinking()
	if claudeReq.Thinking != nil && isThinkingDisabled(claudeReq.Thinking) {
		wantReasoningEarly = false
	}
	proto, protoSource := resolveUpstreamProtocolWithSource(claudeReq.Model)
	switch proto {
	case upstreamProtocolAnthropic:
		slog.Info("claude anthropic passthrough",
			"model_in", modelIn, "model", claudeReq.Model, "stream", claudeReq.Stream, "via", protoSource)
		if forwardClaudeViaAnthropic(r.Context(), w, auth, claudeReq.Model, body, claudeReq.Stream) {
			return
		}
		// 仅传输层错误才会到这里（上游 4xx/5xx 已写回）。继续回落到常规
		// chat 翻译路径，做 best-effort 二次尝试。
		slog.Warn("claude anthropic passthrough failed, falling back to chat", "model", claudeReq.Model, "via", protoSource)
	case upstreamProtocolResponses:
		slog.Info("claude responses passthrough",
			"model_in", modelIn, "model", claudeReq.Model, "stream", claudeReq.Stream, "via", protoSource)
		if forwardClaudeViaResponses(r.Context(), w, auth, claudeReq.Model, claudeReq, claudeReq.Stream, wantReasoningEarly) {
			return
		}
		// 仅传输层错误才会到这里（上游 4xx/5xx 已转换写回）。继续回落到
		// 常规 chat 翻译路径，做 best-effort 二次尝试。
		slog.Warn("claude responses forward failed, falling back to chat", "model", claudeReq.Model, "via", protoSource)
	}
	if msg := validateClaudeDocumentBlocks(claudeReq.Messages); msg != "" {
		writeProtocolValidation400(w, "claude", "", msg)
		return
	}

	// 多模态路由

	chatReq, skippedServerTools := convertClaudeRequest(claudeReq)
	chatReq.Messages = fixToolCallGaps(chatReq.Messages)
	if claudeReq.Stream {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["stream_options"] = map[string]any{"include_usage": true}
	}

	// Keep CoT by default so Claude Code still sees thinking blocks. Only drop
	// reasoning when force-disabled or the client explicitly disables thinking.
	// Empty-reply protection is handled by promoteMisplacedReasoning (!keep)
	// and emitEmptyTextFallback (keep + no text/tool_use).
	wantReasoning := !config.ForceDisableThinking()
	if claudeReq.Thinking != nil && isThinkingDisabled(claudeReq.Thinking) {
		wantReasoning = false
	}
	keepReasoning := wantReasoning
	chatReq.Messages = ensureReasoningContent(chatReq.Messages, keepReasoning)

	effortIn := chatReq.ReasoningEffort
	if effortIn == "" && !isThinkingDisabled(claudeReq.Thinking) {
		effortIn = reasoningEffortFromThinking(claudeReq.Thinking)
	}
	upstreamSurface := "zen"
	if auth.shouldUseGoEndpoint(chatReq.Model) {
		upstreamSurface = "go"
	}
	systemMerged := countClaudeSystemParts(claudeReq.Messages, claudeReq.System) > 1
	plan := map[string]any{
		"protocol":                "claude",
		"model_in":                modelIn,
		"model_resolved":          chatReq.Model,
		"auth_mode":               authModeString(auth.Mode),
		"auth_source":             auth.Source,
		"has_key":                 auth.Token != "",
		"upstream_surface":        upstreamSurface,
		"stream":                  claudeReq.Stream,
		"keep_reasoning":          keepReasoning,
		"thinking":                thinkingState(claudeReq.Thinking),
		"reasoning_effort_in":     effortIn,
		"reasoning_effort_out":    mappedReasoningEffort(effortIn),
		"tools_count":             len(chatReq.Tools),
		"messages_count":          len(chatReq.Messages),
		"multimodal_parts":        countMultimodalParts(chatReq.Messages),
		"text_only_model":         modelIsTextOnly(chatReq.Model),
		"system_merged":           systemMerged,
		"context_management":      claudeReq.ContextManagement != nil,
		"cache_control_blocks":    countClaudeCacheControlBlocks(claudeReq),
		"history_signature_count": countClaudeThinkingSignatures(claudeReq.Messages),
		"client_beta_count":       countAnthropicBetas(r.Header.Get("anthropic-beta")),
		"unsupported_blocks":      scanClaudeUnsupportedBlocks(claudeReq.Messages),
		"max_tokens":              chatReq.MaxTokens,
		"max_tokens_cap":          config.MaxTokensCapFor(chatReq.Model),
	}
	if len(skippedServerTools) > 0 {
		plan["skipped_server_tools"] = skippedServerTools
	}
	logging.PlanRequest(r.Context(), plan)

	upstreamBody := buildUpstreamBody(&chatReq)

	if claudeReq.Stream {
		upResp, status, _, err := callOpenCodeAPIStream(r.Context(), upstreamBody, chatReq.Model, auth)
		if err != nil || status < 200 || status >= 300 {
			var transErrBody []byte
			if upResp != nil {
				transErrBody, _ = io.ReadAll(upResp)
				upResp.Close()
			}
			_ = transErrBody
			// 翻译路径失败：探测上游原生 responses，成功则转换并记住该模型。
			if shouldProbeNativeResponses(status, err) && probeClaudeViaResponses(r.Context(), w, auth, chatReq.Model, claudeReq, true, wantReasoning) {
				return
			}
			errResp := map[string]any{
				"type":  "error",
				"error": map[string]string{"type": "api_error", "message": "upstream error"},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(errResp)
			return
		}
		defer upResp.Close()
		claudeStreamHandler(r.Context(), w, upResp, claudeReq.Model, keepReasoning)
		return
	}

	respBody, status, _, err := callOpenCodeAPI(r.Context(), upstreamBody, chatReq.Model, auth)
	if err != nil || status < 200 || status >= 300 {
		// 翻译路径失败：探测上游原生 responses，成功则转换并记住该模型。
		// 本地转换失败（上游 200 但包体无法解析，err 非 "upstream error"）
		// 与类型化转换错误不探测，原样返回，避免把合成 502 误判为上游 502。
		isUpstreamHTTPError := err != nil && err.Error() == "upstream error"
		if isUpstreamHTTPError && shouldProbeNativeResponses(status, err) && probeClaudeViaResponses(r.Context(), w, auth, chatReq.Model, claudeReq, false, wantReasoning) {
			return
		}
		if err != nil {
			writeUpstreamError(w, status, err, "claude")
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if len(respBody) > 0 {
				w.Write(respBody)
			} else {
				json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": "upstream error"}})
			}
		}
		return
	}

	claudeRespBody := openAIToClaudeResponse(respBody, claudeReq.Model, wantReasoning)
	result := logging.SummarizeClaudeResult(claudeRespBody)
	if !wantReasoning {
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
			statsx.RecordChatUsage(claudeReq.Model, u)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	logging.MaybeBodySummary(r.Context(), "claude response body", claudeRespBody)
	w.Write(claudeRespBody)
}

var claudeKeepaliveInterval = 15 * time.Second

func claudeStreamHandler(ctx context.Context, w http.ResponseWriter, respBody io.ReadCloser, model string, keepReasoning bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	// 纯核 Chat SSE -> Claude SSE 状态机已迁入 bridge.ChatToClaudeStream
	// (internal/bridge/chat_to_claude_stream.go)。原闭包 ensureMessageStart /
	// emitTextDelta / closeThinkingBlock / closeTextBlock / emitEmptyTextFallback /
	// finalizeContentBlocks / emitClaudeError 与全部状态 (msgID/blockIndex/
	// toolCallAccumulator/toolBlockIndices/toolCallOrder/messageStartSent/finished/
	// stopReason/usageTerminalSeen/fullUsage/reasoningFallback) 入 struct;产物为
	// bridge.OutEvent(Raw 即原 writeSSEEvent 写出的 `event: ...\ndata: ...\n\n`
	// 字节,逐字节等价)。streamProducedOutput 由依赖 *logging.StreamStats 改为纯
	// (textChars,reasoningChars,toolCalls)->bool。本壳保留全部副作用:HTTP 头与
	// WriteHeader、writeSSEEvent 序列化+w.Write+flusher.Flush、streamReader/
	// keepalive 循环、logging.FromContext(ctx).Error(读错误)、deferred stats 上报。
	st := bridge.NewChatToClaudeStream(randomIDGen, model, keepReasoning)

	stats := &logging.StreamStats{Start: time.Now()}
	defer func() {
		c := st.Counters()
		stats.TextChars = c.TextChars
		stats.ReasoningChars = c.ReasoningChars
		stats.PromotedReasoning = c.PromotedReasoning
		stats.ToolCallCount = c.ToolCallCount
		stats.FinishReason = c.FinishReason
		stats.SawFinish = c.SawFinish
		stats.DoneSeen = c.DoneSeen
		if fu := st.Usage(); len(fu) > 0 {
			statsx.RecordChatUsage(model, fu)
		}
		stats.Log(ctx, "claude")
	}()

	keepaliveInterval := claudeKeepaliveInterval
	if keepaliveInterval <= 0 {
		keepaliveInterval = 15 * time.Second
	}
	reader := newStreamReader(ctx, respBody, keepaliveInterval)
	defer reader.Close()

	writeAll := func(evs []bridge.OutEvent) {
		for _, ev := range evs {
			if len(ev.Raw) > 0 {
				w.Write(ev.Raw)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}

	// statsChunks tracks how many choice-bearing chunks have been noted so the
	// shell can call stats.NoteChunk exactly when the state machine consumes one
	// (matching the original, which noted a chunk only on choice-bearing lines,
	// not on usage-only trailing chunks or [DONE]).
	statsChunks := 0

loop:
	for {
		select {
		case <-ctx.Done():
			// Client cancelled: quiet exit, no error writes.
			return
		case <-reader.Keepalive():
			// Keepalive ping — before the first upstream token this is the only
			// thing the client receives; it does NOT fake message_start.
			writeAll([]bridge.OutEvent{st.PingEvent()})
		case result := <-reader.Read():
			// bufio.ReadString may return both a non-empty line and an error
			// (e.g. the last line without a trailing newline + io.EOF). Process
			// the line first, then handle the accompanying error via pendingErr.
			pendingErr := result.err

			if line := result.line; line != "" {
				out, breakLoop := st.Handle(bridge.LineEvent{RawLine: line})
				// Note a chunk exactly when the state machine did (choice-bearing
				// lines only), preserving the original FirstChunkAt/Chunks timing.
				for cc := st.ChunkCount(); statsChunks < cc; statsChunks++ {
					stats.NoteChunk()
				}
				writeAll(out)
				if breakLoop {
					// [DONE] / in-band error / malformed JSON reached. Emit the
					// message_delta/message_stop trailer only when a valid finish
					// (or the synthesized usage-terminal stop) was seen AND no error
					// frame went out; an error returns without the trailer.
					if st.Finished() && !st.Errored() {
						writeAll(st.Finalize())
					}
					break loop
				}
			}

			// Now handle a pending error from the read.
			if pendingErr != nil {
				if pendingErr == io.EOF {
					out := st.HandleEOF()
					// Emit the trailer only on a clean/synthesized stop (finished and
					// no error). EOF without finish_reason emits only the error frame
					// from HandleEOF, no trailer.
					if st.Finished() && !st.Errored() {
						writeAll(out)
						writeAll(st.Finalize())
					} else {
						writeAll(out)
					}
					return
				}
				logging.FromContext(ctx).Error("stream read error", "error", pendingErr)
				writeAll([]bridge.OutEvent{st.ReadErrorEvent()})
				return
			}
		}
	}
}

// ======================== Anthropic 格式兼容 ========================

// 纯核已迁入 bridge.IsAnthropicFormat；薄壳转发。
func isAnthropicFormat(body []byte) bool {
	return bridge.IsAnthropicFormat(body)
}

// anthropicBlockState tracks per-index content block reconstruction.
// 纯核已迁入 bridge.AnthropicBlockState；薄壳别名转发。
type anthropicBlockState = bridge.AnthropicBlockState

// usageHasCompletion reports whether an upstream usage object includes output
// token accounting, which upstreams send as the terminal chunk when
// stream_options.include_usage is set.
// 纯核已迁入 bridge.UsageHasCompletion；薄壳转发。
func usageHasCompletion(usage map[string]any) bool {
	return bridge.UsageHasCompletion(usage)
}

// streamProducedOutput reports whether a stream has emitted any assistant
// content or tool calls before the terminal usage chunk. 薄壳转发到 bridge;
// 纯形式 (textChars,reasoningChars,toolCalls)->bool,不再依赖
// *logging.StreamStats。保留同名同包薄壳以兼容既有调用方/测试。
func streamProducedOutput(textChars, reasoningChars, toolCalls int) bool {
	return bridge.StreamProducedOutput(textChars, reasoningChars, toolCalls)
}

// mergeUsageMaps merges src into dst. Anthropic usage values are snapshots /
// cumulative: a field present in src always replaces the value in dst
// (including 0). Nested maps are recursively merged. Fields absent from src
// are retained. 薄壳转发到 bridge。
func mergeUsageMaps(dst any, src map[string]any) map[string]any {
	return bridge.MergeUsageMaps(dst, src)
}

// parseAnthropicSSE consumes a complete Anthropic Messages SSE body and
// returns the terminal message object plus the reconstructed content blocks.
//
// Accepted framing:
//   - standard SSE: "data: <json>", "event: <name>", comment lines starting
//     with ":" (ignored as metadata)
//
// Malformed/truncated conditions that return an error:
//   - missing message_stop
//   - error event from upstream
//   - malformed event JSON
//   - delta for an unknown/un-started index
//   - content_block_stop for an unknown/un-started index
//   - duplicate content_block_start for the same index
//   - message_stop with unclosed (not-yet-stopped) blocks
//   - malformed tool_use input JSON
//
// 纯核已迁入 bridge.ParseAnthropicSSE；薄壳转发并包装类型化错误为
// anthropicProtocolError。
func parseAnthropicSSE(body []byte) (map[string]any, []map[string]any, error) {
	msg, blocks, err := bridge.ParseAnthropicSSE(body)
	return msg, blocks, wrapBridgeProtocolError(err)
}

// undefined/saturating behavior on overflow.
// 纯核已迁入 bridge.ExtractBlockIndex；薄壳转发。
func extractBlockIndex(event map[string]any) (int, bool) {
	return bridge.ExtractBlockIndex(event)
}
