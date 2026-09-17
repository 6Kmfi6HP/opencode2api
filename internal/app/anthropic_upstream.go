package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/6Kmfi6HP/opencode2api/internal/logging"
	statsx "github.com/6Kmfi6HP/opencode2api/internal/stats"
)

// ======================== Anthropic 上游直通（/v1/messages → /zen/v1/messages） ========================

// forwardClaudeViaAnthropic 把 Claude Messages 请求直通上游原生 Anthropic
// 端点：body 仅改写 model，其余字段无损；流式 SSE 原样管道转发，非流式
// JSON 原样写回。上游 4xx/5xx 状态码与错误体保真透传（Anthropic 错误形状
// 与入站协议天然一致）。仅传输层错误（拿不到上游响应）返回 false，调用方
// 兜底回落 chat 翻译路径。
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

	if stream {
		pipeAnthropicStream(ctx, w, rc, status, header, modelID)
		return true
	}
	relayAnthropicBuffered(ctx, w, rc, status, header, modelID)
	return true
}

// flushWriter 在每次 Write 后立即 Flush,保证 SSE 以事件粒度实时下发;
// http.ResponseWriter 内部带 bufio 缓冲,不显式 Flush 会把事件攒批到 EOF
// (与 responses_passthrough.go relayResponsesStream 的逐行 Flush 同一约定)。
type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if n > 0 {
		fw.f.Flush()
	}
	return n, err
}

// pipeAnthropicStream 把上游 Anthropic SSE 流字节级原样转发给客户端,同时
// 旁路 tee 解析 message_start / message_delta 中的 usage 记入 token 统计。
// 行边界、CRLF/LF、空行均不做改写,确保下游收到与上游完全一致的字节流。
func pipeAnthropicStream(ctx context.Context, w http.ResponseWriter, rc io.Reader, status int, header http.Header, modelID string) {
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

	// tee 管道:旁路解析走 pipeWriter,主流走 io.Copy 直透;两组无背压,
	// io.Copy 返回(EOF、rc 读取失败、pw.Write 失败)时主动 pw.Close()
	// 告知解析端收尾;ctx 取消则先 close 上游 rc 解锁 io.Copy,再
	// pw.CloseWithError(ctx.Err()) 让 pr.Read 立刻返回。
	pr, pw := io.Pipe()
	copyDone := make(chan struct{})
	go func() {
		defer close(copyDone)
		// MultiWriter 把每个 read 同步写给客户端与旁路解析端;flushWriter
		// 让每片上游数据即时下发(不攒批)。任一侧写失败 io.Copy 立即返回,
		// 随后 pw.Close 告知解析端收尾,最终 close(copyDone) 供主循环 join。
		flusher, _ := w.(http.Flusher)
		cw := io.Writer(w)
		if flusher != nil {
			cw = flushWriter{w: w, f: flusher}
		}
		_, _ = io.Copy(io.MultiWriter(cw, pw), rc)
		_ = pw.Close()
	}()

	// tee 解析流:复用 newStreamReader 的协程,读到行就 observe,不写出。
	reader := newStreamReader(ctx, pr, 0)
	defer func() {
		if len(fullUsage) > 0 {
			statsx.RecordChatUsage(modelID, anthropicUsageToChat(fullUsage))
		}
		stats.Log(ctx, "claude")
	}()
	defer reader.Close()

	for {
		select {
		case <-ctx.Done():
			// 先 close 上游,让 io.Copy 立刻读到错误退出(不再卡在 w.Write),
			// 再 close pipe 让旁路解析收尾,这样 copyDone 不会等慢客户端。
			if c, ok := rc.(io.Closer); ok {
				_ = c.Close()
			}
			_ = pw.CloseWithError(ctx.Err())
			<-copyDone
			return
		case result := <-reader.Read():
			pendingErr := result.err
			line := result.line
			if line != "" {
				stats.NoteChunk()
				observeAnthropicStreamEvent(stats, fullUsage, line)
			}
			if pendingErr != nil {
				// pr 的错误只可能来自 pw.Close(),即 copy 协程已越过 io.Copy,
				// 此处 join 必然立即返回;保证协程不再于 handler 返回后触碰
				// 已交还的 http.ResponseWriter(net/http 禁止这种并发使用)。
				<-copyDone
				return
			}
		}
	}
}

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

// relayAnthropicBuffered 非流式直通：上游 JSON 原样写回（含非 2xx 错误体与
// 状态码保真），并解析 usage 记入 token 统计。
func relayAnthropicBuffered(ctx context.Context, w http.ResponseWriter, rc io.Reader, status int, header http.Header, modelID string) {
	body, readErr := io.ReadAll(io.LimitReader(rc, 32*1024*1024))
	if readErr != nil {
		w.Header().Set("Content-Type", "application/json")
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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)

	if status >= 200 && status < 300 {
		var raw map[string]any
		if json.Unmarshal(body, &raw) == nil {
			if u, ok := raw["usage"].(map[string]any); ok {
				statsx.RecordChatUsage(modelID, anthropicUsageToChat(u))
			}
			result := logging.SummarizeClaudeResult(body)
			logging.LogResult(ctx, result)
		}
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
