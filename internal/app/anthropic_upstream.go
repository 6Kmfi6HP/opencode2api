package app

import (
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

// ======================== Anthropic 上游直通（/v1/messages → /zen/v1/messages） ========================

// forwardClaudeViaAnthropic 把 Claude Messages 请求直通上游原生 Anthropic
// 端点：body 仅改写 model（并按 max_tokens 收敛到 [128, cap]），其余字段
// 无损。上游 2xx 时流式 SSE 原样管道转发、非流式 JSON 原样写回；上游
// 非 2xx 时统一 buffered 读回并以 application/json + 原状态码保真透传
// （无论请求是否 stream，客户端侧连 SSE 流都没建立，发给它的必须是 JSON
// 错误而不是包进 data frame 的错误 —— Claude Code 只认这个形状）。
// 仅传输层错误（拿不到上游响应）返回 false，调用方兜底回落 chat 翻译路径。
func forwardClaudeViaAnthropic(ctx context.Context, w http.ResponseWriter, auth UpstreamAuth, modelID string, rawBody []byte, stream bool) bool {
	log := logging.FromContext(ctx)
	var bodyMap map[string]any
	if err := json.Unmarshal(rawBody, &bodyMap); err != nil {
		// 无法解析的体不可能来自合法客户端（handler 已 unmarshal 过一次），
		// 这里兜底按原体直发，让上游给出明确错误。
		bodyMap = nil
	}
	upstreamBody := rawBody
	if bodyMap != nil {
		bodyMap["model"] = modelID
		// 直通仍遵守全局 max_tokens 预算：已有值收敛 [128, cap]；缺省时
		// Anthropic schema 要求必填,按 defaultClaudeMaxTokens 补(与
		// convertClaudeRequest 的 chat 翻译路径一致)。
		if clampAnthropicProtocolMaxTokens(bodyMap, modelID) == 0 {
			bodyMap["max_tokens"] = clampMaxTokens(defaultClaudeMaxTokens, config.MaxTokensCapFor(modelID))
		}
		// 工具名兼容：超长/非法字符 name 确定性缩短，消灭上游
		// "`name` must be at most 64 characters" 类 400；映射注入 ctx 供
		// 响应呈现面还原（重试重放同一已缩短 upstreamBody，跨 key 轮换稳定）。
		rw := newResponsesNameRewrites(shouldRestoreToolCase(ctx))
		if rw.shortenAnthropicBodyNames(bodyMap) {
			ctx = withAnthropicNameRewrites(ctx, rw)
		}
		if b, err := json.Marshal(bodyMap); err == nil {
			upstreamBody = b
		}
	}

	rc, status, header, err := callOpenCodeAnthropicEndpoint(ctx, upstreamBody, modelID, auth)
	if err != nil {
		if rc != nil {
			rc.Close()
		}
		log.Warn("anthropic upstream transport error", "model", modelID, "error", err)
		return false
	}
	defer rc.Close()

	// 非 2xx 即使请求方要求 stream 也统一走 buffered JSON 错误直转
	// （上游未建立 SSE 流，tee 会把错误 JSON 包进 data frame 破坏客户端解析）。
	if status >= 200 && status < 300 && stream {
		// 用 DriveStreamWithRetry 在 peek 失败时切换 key 重试,空流 / EOF /
		// 首字节超时被翻译为可重试的 errStreamIncompleteNoCommit。首轮复用
		// 调用方已打开的 rc,后续重试通过 callOpenCodeAnthropicEndpoint 让
		// key pool 切到下一把可用 key。
		pending := rc
		callOnce := func(c context.Context) (io.ReadCloser, int, error) {
			if pending != nil {
				r := pending
				pending = nil
				return r, status, nil
			}
			nrc, nstatus, _, nerr := callOpenCodeAnthropicEndpoint(c, upstreamBody, modelID, auth)
			// Drive 拿到非 2xx 会立即返回(不再 close)。retry 路径里我们已
			// 在上层只能是「走 chat 翻译兜底」,rc 在这里直接 close 掉防泄漏。
			if nerr == nil && (nstatus < 200 || nstatus >= 300) {
				if nrc != nil {
					nrc.Close()
				}
			}
			return nrc, nstatus, nerr
		}
		runOnce := func(c context.Context, w http.ResponseWriter, nrc io.Reader, _ []streamReadResult, _ *streamReader) (bool, error) {
			return pipeAnthropicStream(c, w, nrc, status, header, modelID)
		}
		committed, driveErr := DriveStreamWithRetry(ctx, w, AnthropicProtocolHooks, callOnce, runOnce)
		if !committed {
			// 一直未 commit,让上层走 chat 翻译路径;不写任何字节给客户端。
			if driveErr != nil {
				log.Warn("anthropic passthrough stream exhausted retries", "model", modelID, "err", driveErr)
			}
			return false
		}
		return true
	}
	relayAnthropicBuffered(ctx, w, rc, status, header, modelID)
	return true
}

// pipeAnthropicStream 把上游 Anthropic SSE 流字节级原样转发给客户端,同时
// 旁路 tee 解析 message_start / message_delta 中的 usage 记入 token 统计。
// 行边界、CRLF/LF、空行均不做改写,确保下游收到与上游完全一致的字节流。
// 仅在上游 2xx(真 SSE)时被调用；错误响应一律走 relayAnthropicBuffered。
//
// 返回 (true, nil)：已 commit（首帧已 peek + 写入）。EOF 时若未见过
// message_stop 但见过 message_start,合成一条 message_stop 保证客户端正常
// 关流；若两者皆无（不应发生：peek 至少要看到一帧）返回 false 供调用方
// 走未 commit 重试。返回 (false, err)：peek 未 commit(空流 / EOF / 错误帧 /
// 首字节超时),由 DriveStreamWithRetry 决定是否换 key 重发。
func pipeAnthropicStream(ctx context.Context, w http.ResponseWriter, rc io.Reader, status int, header http.Header, modelID string) (bool, error) {
	// peek 首帧:在 WriteHeader 之前约束 commit 边界,空流 / EOF / 错误帧 /
	// 首字节超时都返回 errStreamIncompleteNoCommit,由调用方驱动重试。
	peek := PeekFirstFrame(ctx, rc, time.Duration(config.StreamFirstByteTimeoutMs())*time.Millisecond, AnthropicProtocolHooks)
	if peek.Err != nil {
		return false, peek.Err
	}

	filtered := filterResponseHeaders(header)
	for k, v := range filtered {
		w.Header().Set(k, v[0])
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(status)

	stats := &logging.StreamStats{Start: time.Now()}
	fullUsage := map[string]any{}
	defer func() {
		if len(fullUsage) > 0 {
			statsx.RecordChatUsage(modelID, anthropicUsageToChat(fullUsage))
		}
		stats.Log(ctx, "claude")
	}()

	flusher, _ := w.(http.Flusher)
	// 写 peek 出的原始字节(完整保留 \r\n / 换行 / 空行),同时喂给
	// observeAnthropicStreamEvent 让 stats 与 message_start/stop 计数正确
	// 累计——peek 消费过的帧不再二次进 reader.Read() 通道,所以这里必须补
	// 一次观察。
	// peek 消费的首帧也要过 name 还原(帧尾常与首个 content_block_start 同行,
	// 而主循环的还原按行生效),否则首个 tool_use 的 name 会被原样透传。
	rw := nameRewritesFor(ctx)
	for _, res := range peek.Consumed {
		if res.line != "" {
			res.line = restoreAnthropicStreamLine(res.line, rw)
		}
		if _, err := io.WriteString(w, res.line); err != nil {
			return true, err
		}
	}
	sawMessageStop := false
	observeLine := func(line string) {
		stats.NoteChunk()
		observeAnthropicStreamEvent(stats, fullUsage, line)
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			return
		}
		var evt map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(payload)), &evt) != nil {
			return
		}
		if typ, _ := evt["type"].(string); typ == "message_stop" {
			sawMessageStop = true
		}
	}
	// 先用 peek 消费过的行回填 sawMessageStart / sawMessageStop——后续 EOF
	// 兜底合成 message_stop 需要知道是否已见过 message_start / message_stop。
	for _, res := range peek.Consumed {
		if res.line != "" {
			observeLine(res.line)
		}
	}
	if flusher != nil {
		flusher.Flush()
	}

	// 续用 peek 内部 streamReader(它的 bufio 已预读后续行),按 SSE 帧聚合
	// 再写客户端——与原 io.Copy 的「一次上游 chunk ≈ 一次 Write+Flush」
	// 节奏对齐,保留逐事件的打字机效果,而不是退回到 line-at-a-time。
	reader := peek.Reader
	if reader == nil {
		// EOF 收尾的 peek 没留下 reader——主循环立即结束。
		reader = newStreamReader(ctx, rc, 0)
	}
	defer reader.Close()

	var frameBuf strings.Builder
	flushFrame := func() error {
		if frameBuf.Len() == 0 {
			return nil
		}
		_, err := io.WriteString(w, frameBuf.String())
		frameBuf.Reset()
		if err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case result := <-reader.Read():
			line := result.line
			pendingErr := result.err
			if line != "" {
				observeLine(line)
				// name 还原(缩短名回原名 + 免费层占位名大小写,经 rw 统一
				// 门控),未命中的行字节原样。
				frameBuf.WriteString(restoreAnthropicStreamLine(line, rw))
				// 空行 = 帧边界:整帧一次写出再 Flush。
				if strings.TrimRight(line, "\r\n") == "" {
					if err := flushFrame(); err != nil {
						return true, err
					}
				}
			}
			if pendingErr != nil {
				// EOF / 上游读取失败。先把残帧(无空行收尾)吐出去,再看是否
				// 需要补 message_stop / 走重试。
				if err := flushFrame(); err != nil {
					return true, err
				}
				// 关键不变量:此时 WriteHeader + peeked 首帧字节已经发出去了,
				// 客户端连接已经处于 SSE 数据段。**绝不可再返回 (false, ...)**
				// 否则 DriveStreamWithRetry 会用同一个 ResponseWriter 二次
				// WriteHeader + 二次 replay peeked 字节,流被污染(I4/I7)。
				if !sawMessageStop {
					// 上游 EOF 但没关 message:展开成「合成 message_stop」让
					// Claude SDK 正常关流。sawMessageStart=false 也照发——客
					// 户端拿到「没 message_start 直接 message_stop」虽不规范,
					// 但比重复写 header 安全(对一个非法流,SDK 通常仅丢弃该
					// 事件,而不是报错)。
					if _, err := io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"); err != nil {
						return true, err
					}
					if flusher != nil {
						flusher.Flush()
					}
				}
				return true, nil
			}
		}
	}
}

// restoreAnthropicStreamLineCase 与 restoreAnthropicBodyToolCase 的实现已抽入
// name_compat.go（本文件保留调用点）；restoreAnthropicStreamLine /
// restoreAnthropicBodyNames 为其带映射参数的一般化版本。

// observeAnthropicStreamEvent 旁路解析一行 SSE，累计 usage 与流统计。
func observeAnthropicStreamEvent(stats *logging.StreamStats, fullUsage map[string]any, line string) {
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
				mergeUsage(fullUsage, u)
			}
		}
	case "message_delta":
		if u, ok := evt["usage"].(map[string]any); ok {
			mergeUsage(fullUsage, u)
		}
		if delta, ok := evt["delta"].(map[string]any); ok {
			if sr, ok := delta["stop_reason"].(string); ok && sr != "" {
				stats.FinishReason = sr
				stats.SawFinish = true
			}
		}
	case "message_stop":
		stats.DoneSeen = true
	}
}

// relayAnthropicBuffered 非流式直通与直通路径错误透传（含流式请求下的上游
// 非 2xx）：buffered 读回上游体，以 application/json + 原状态码保真写回，
// 并解析 usage 记入 token 统计；上游错误体同时记入去重日志。
func relayAnthropicBuffered(ctx context.Context, w http.ResponseWriter, rc io.Reader, status int, header http.Header, modelID string) {
	body, readErr := io.ReadAll(io.LimitReader(rc, 32*1024*1024))
	if readErr != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]any{
			"type":  "error",
			"error": map[string]string{"type": "api_error", "message": "upstream read error"},
		})
		return
	}
	filtered := filterResponseHeaders(header)
	for k, v := range filtered {
		w.Header().Set(k, v[0])
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// 非流式直通路径在写回前还原 tool_use name(缩短名回原名 + 免费层占位
	// 工具大小写,经 nameRewritesFor 统一门控),与流式 byte-relay 的
	// restoreAnthropicStreamLine 对应（幂等）。
	w.Write(restoreAnthropicBodyNames(body, nameRewritesFor(ctx)))

	if status >= 200 && status < 300 {
		var raw map[string]any
		if json.Unmarshal(body, &raw) == nil {
			if u, ok := raw["usage"].(map[string]any); ok {
				statsx.RecordChatUsage(modelID, anthropicUsageToChat(u))
			}
			result := logging.SummarizeClaudeResult(body)
			logging.LogResult(ctx, result)
		}
	} else {
		logging.UpstreamError(ctx, modelID, status, body, roundRobinBaseURL())
	}
}

// mergeUsage 把增量 usage 合并进累计表（新值覆盖旧值，保留未知键）。
func mergeUsage(full map[string]any, delta map[string]any) {
	for k, v := range delta {
		full[k] = v
	}
}

// parseAnthropicErrorBody 把上游 Anthropic 错误体转为 anthropicProtocolError，
// 供 writeUpstreamError 按入站协议（chat/responses）写回错误形状。
func parseAnthropicErrorBody(body []byte) (*anthropicProtocolError, bool) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false
	}
	if typ, _ := raw["type"].(string); typ != "error" {
		return nil, false
	}
	errType, message := "api_error", "upstream error"
	if em, ok := raw["error"].(map[string]any); ok {
		if t, ok := em["type"].(string); ok && t != "" {
			errType = t
		}
		if m, ok := em["message"].(string); ok && m != "" {
			message = m
		}
	}
	return &anthropicProtocolError{errType: errType, message: message}, true
}

// anthropicErrorMessage 从非 2xx 响应体提取人类可读错误信息（best-effort）。
func anthropicErrorMessage(body []byte) string {
	var raw map[string]any
	if json.Unmarshal(body, &raw) == nil {
		if em, ok := raw["error"].(map[string]any); ok {
			if m, ok := em["message"].(string); ok && m != "" {
				return m
			}
		}
		if m, ok := raw["message"].(string); ok && m != "" {
			return m
		}
	}
	if len(body) > 0 {
		return fmt.Sprintf("upstream error (%d bytes)", len(body))
	}
	return "upstream error"
}
