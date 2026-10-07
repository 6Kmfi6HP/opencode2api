package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/6Kmfi6HP/opencode2api/internal/config"
	"github.com/6Kmfi6HP/opencode2api/internal/logging"
	statsx "github.com/6Kmfi6HP/opencode2api/internal/stats"
)

// ======================== /v1/responses → Anthropic 上游 ========================

// forwardResponsesViaAnthropic 处理规则命中 anthropic 的 Responses 入站请求：
// 请求侧复用 handler 已构建的 chatReq（保留 responsesInputToMessages、
// fixToolCallGaps、多模态与 text-only 降级等既有行为）→ chatToAnthropicBody；
// 响应按客户端流式偏好转回 Responses 形状。
func forwardResponsesViaAnthropic(w http.ResponseWriter, r *http.Request, auth UpstreamAuth, chatReq *OpenAIRequest, wantReasoning bool) {
	ctx := r.Context()
	// 工具名兼容：超长/非法字符 name 确定性缩短，映射注入 ctx 供响应呈现面
	// 还原（该路径现状无占位名大小写还原，保持 restoreStubCase=false）。
	upstreamBody, rewrites := sanitizeAnthropicUpstreamBody(chatToAnthropicBody(chatReq, chatReq.Model, isClaudeCodeClient(r.Header.Get("User-Agent"))), false)
	ctx = withAnthropicNameRewrites(ctx, rewrites)
	log := logging.FromContext(ctx)
	log.Info("responses via anthropic upstream",
		"model", chatReq.Model, "stream", chatReq.Stream, "keep_reasoning", wantReasoning)

	if chatReq.Stream {
		// DriveStreamWithRetry 链路(与 chat→anthropic 同形):peek 失败
		// (空流 EOF/读错/上游错误帧/首字节看门狗)按 StreamEmptyRetryMax 换
		// key 重试;UpstreamErrorCapture 暂存非 2xx 响应,重试全失败后透传
		// 上游真实 status+body。
		upstreamCap := &UpstreamErrorCapture{}
		callOnce := upstreamCap.WrapCallOnce(func(callCtx context.Context) (io.ReadCloser, int, error) {
			nrc, nstatus, _, nerr := callOpenCodeAnthropicEndpoint(callCtx, upstreamBody, chatReq.Model, auth)
			return nrc, nstatus, nerr
		})
		runOnce := func(runCtx context.Context, rw http.ResponseWriter, nrc io.Reader, peeked []streamReadResult, rd *streamReader) (bool, error) {
			return anthropicSSEToResponsesStream(runCtx, rw, nrc, chatReq.Model, wantReasoning, peeked, rd)
		}
		committed, streamErr := DriveStreamWithRetry(ctx, w, AnthropicProtocolHooks, callOnce, runOnce)
		if committed {
			return
		}
		// 全部 attempt 都未 commit:尚未向客户端写过任何字节,落明确错误,
		// 不伪装半截流。ctx 取消由调用方按客户端断开处理,原样透传。
		if errors.Is(streamErr, context.Canceled) || errors.Is(streamErr, context.DeadlineExceeded) {
			return
		}
		log.Warn("responses via anthropic stream empty after retries",
			"model", chatReq.Model, "err", streamErr)
		if upstreamCap.RC != nil && upstreamCap.Status != 0 {
			writeCrossProtocolError(w, upstreamCap.Status, nil, upstreamCap.RC, "responses")
			return
		}
		writeUpstreamError(w, http.StatusBadGateway, fmt.Errorf("upstream stream empty after retries"), "responses")
		return
	}

	rc, status, _, err := callOpenCodeAnthropicEndpoint(ctx, upstreamBody, chatReq.Model, auth)
	if err != nil || status < 200 || status >= 300 {
		writeCrossProtocolError(w, status, err, rc, "responses")
		return
	}
	defer rc.Close()

	respBody, readErr := io.ReadAll(io.LimitReader(rc, 32*1024*1024))
	if readErr != nil {
		writeUpstreamError(w, http.StatusBadGateway, fmt.Errorf("upstream read error"), "responses")
		return
	}
	// 先还原缩短名（含 SSE 兜底），转换器天然携带客户端原始名。
	respBody = restoreAnthropicResponseNames(respBody, rewrites)
	responsesBody := convertAnthropicToResponses(respBody, chatReq.Model, wantReasoning)
	result := logging.SummarizeChatResult(responsesBody)
	logging.LogResult(ctx, result)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(responsesBody)
}

// writeCrossProtocolError 把上游错误统一写回：优先解析 Anthropic 错误体转
// 类型化错误，其次原样透传错误体，最后合成默认错误。
func writeCrossProtocolError(w http.ResponseWriter, status int, err error, rc io.ReadCloser, protocol string) {
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
			writeUpstreamError(w, status, ape, protocol)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(errBody)
		return
	}
	if err == nil {
		err = fmt.Errorf("upstream error")
	}
	writeUpstreamError(w, status, err, protocol)
}

// convertAnthropicToResponses 把 Anthropic message JSON 转为 Responses 对象：
// 链式复用 convertAnthropicToOpenAI（Anthropic→Chat）与 convertChatToResponses
// （Chat→Responses），随后应用请求回显与会话状态保存。
func convertAnthropicToResponses(anthropicBody []byte, model string, wantReasoning bool) []byte {
	chatBody, err := convertAnthropicToOpenAI(anthropicBody, model)
	if err != nil {
		return []byte(`{"error":{"message":"failed to convert anthropic response","type":"upstream_error"}}`)
	}
	return convertChatToResponses(chatBody, model, wantReasoning, nil, nil, nil)
}

// ======================== Anthropic SSE → Responses SSE 状态机 ========================

// anthropicToResponsesState 把上游 Anthropic Messages SSE 事件流转换为
// Responses SSE 事件流。事件序列对齐 responsesStreamHandler 的输出形状。
type anthropicToResponsesState struct {
	w             http.ResponseWriter
	flusher       http.Flusher
	stats         *logging.StreamStats
	id            string
	model         string
	wantReasoning bool
	// rewrites 携带本请求的缩短名还原映射（anthropic 上游边界注入 ctx）；
	// nil 安全。
	rewrites    *responsesNameRewrites
	seq         int
	outputIndex int
	fullUsage   map[string]any
	// 按 anthropic block index 跟踪打开中的 content_block。
	// 每个 block 一个 struct,对应 claude.go anthropicBlockState 的形状;
	// 不再用多个平行 map(避免 cleanup 时漏删某个 map 导致串扰)。
	blocks    map[int]*responsesBlockState
	toolCount int
	// 计数器：仅统计（不对外产出）
	signatureDeltas int
	// 已完成的 output 汇总（message_stop 时写入 response.completed）
	completedOutput []any
	terminalSent    bool
}

// responsesBlockState 记录一个打开中的 content_block 的所有元数据。
// text/thinking 共用 builder,kind 区分类型;tool_call 不需要 builder
// (其 arguments 已通过 initialArguments + input_json_delta 流出)。
type responsesBlockState struct {
	kind        string // "text" | "thinking" | "tool_use"
	itemID      string
	outputIndex int
	toolIdx     int              // 仅 tool_use 使用
	callID      string           // 仅 tool_use：Anthropic tool_use id（fc_ 前缀前的原始 id）
	name        string           // 仅 tool_use
	text        *strings.Builder // kind=text → 累积 output_text;kind=thinking → 累积 reasoning summary
	// tool_use:content_block_start 的初始 input 缓存；若到 content_block_stop
	// 仍没有任何 input_json_delta,closeBlock 先补发一条 argument.delta 带完整
	// input JSON 再发 done（否则仅有 "in_progress" 的 item 与缺参数的 done）。
	initialArguments string
	argDeltaSeen     bool
}

// anthropicSSEToResponsesStream 把上游 Anthropic Messages SSE 翻译为 Responses
// SSE。peek 首帧在 WriteHeader 之前约束 commit 边界:空流 EOF/读错/上游错误
// 帧/首字节超时都返回 (false, errStreamIncompleteNoCommit),由
// DriveStreamWithRetry 换 key 重试。已 commit 后的读错误按错误类型分流:
// 干净 EOF 合成 response.completed;RST/unexpected EOF/墙钟超时等传输中断
// 按 response.failed 收尾(对照 responses.go 的截断标记),不伪造 completed。
// 返回 (true, nil):已 commit;(false, err):未 commit,可重试。
func anthropicSSEToResponsesStream(ctx context.Context, w http.ResponseWriter, rc io.Reader, model string, wantReasoning bool, peeked []streamReadResult, rd *streamReader) (bool, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	// WriteHeader 延迟到首行真正写出（避免 peek 阶段占用 200,让空流的
	// attempt 可安全重试而不污染客户端）。
	wroteHeader := false
	writeHeaderOnce := func() {
		if wroteHeader {
			return
		}
		w.WriteHeader(http.StatusOK)
		wroteHeader = true
	}

	st := &anthropicToResponsesState{
		w:             w,
		flusher:       flusher,
		stats:         &logging.StreamStats{Start: time.Now()},
		id:            "resp_" + randomHex(16),
		model:         model,
		wantReasoning: wantReasoning,
		rewrites:      nameRewritesFor(ctx),
		blocks:        map[int]*responsesBlockState{},
		fullUsage:     map[string]any{},
	}
	defer func() {
		st.stats.ToolCallCount = st.toolCount
		// TextChars/ReasoningChars 在 text_delta/thinking_delta 时即时累计,
		// 等价于旧实现 fullText/fullReasoning 的全流长度(且覆盖 EOF 残留
		// block 的尾部数据)。
		if len(st.fullUsage) > 0 {
			statsx.RecordChatUsage(model, responsesUsageToChat(st.fullUsage))
		}
		st.stats.Log(ctx, "responses")
	}()

	var reader *streamReader
	if len(peeked) == 0 {
		peek := PeekFirstFrame(ctx, rc, time.Duration(config.StreamFirstByteTimeoutMs())*time.Millisecond, AnthropicProtocolHooks)
		if peek.Err != nil {
			return false, peek.Err
		}
		peeked = peek.Consumed
		reader = peek.Reader
		if reader == nil {
			// 上游 EOF 但已见完整帧（极少见：单帧流）。续读的 reader 直接
			// 落在已 EOF 的 rc 上,主循环立即收 EOF 并走 ensureTerminal。
			reader = newStreamReader(ctx, rc, 0)
		}
	} else if rd != nil {
		reader = rd
	} else {
		reader = newStreamReader(ctx, rc, 0)
	}
	defer reader.Close()

	// processResult 处理一行上游 SSE（err 非空表示当前行已是最后一行）。
	// 关键不变量:一旦 writeHeaderOnce 触发,HTTP 头已发出,绝不能返回
	// (false, ...) 否则 DriveStreamWithRetry 会用同一个 ResponseWriter 二次
	// WriteHeader,流被污染(I4/I7)。
	processResult := func(result streamReadResult) (done bool, retErr error) {
		if result.line != "" {
			st.stats.NoteChunk()
			writeHeaderOnce()
			st.handleLine(result.line)
			// handleLine 发出终结事件(message_stop/error/重复 start 兜底)
			// 后直接退出,不再消费上游后续行。
			if st.terminalSent {
				return true, nil
			}
		}
		if result.err != nil {
			if !wroteHeader {
				// 真未 commit(一字未写)= 让 Drive retry。
				return true, errStreamIncompleteNoCommit
			}
			if !errors.Is(result.err, io.EOF) {
				// 传输中断:SawFinish 保持 false（stream_result 记
				// truncated=true）,按 response.failed 收尾——客户端得以
				// 感知失败并重试该回合,而不是把 status=completed 的半截
				// 回合记入历史。
				logging.FromContext(ctx).Warn("responses via anthropic stream interrupted mid-stream",
					"model", model, "err", result.err)
				st.ensureTerminalError("failed", "response.failed", "upstream stream interrupted before completion")
				return true, nil
			}
			// 干净 EOF 未发 message_stop：补 response.completed 保证客户端终止。
			st.ensureTerminal("completed", "response.completed")
			return true, nil
		}
		return false, nil
	}

	// 先回放 peeked,再进入主循环。
	pending := append([]streamReadResult(nil), peeked...)
	for _, res := range pending {
		done, err := processResult(res)
		if done {
			return wroteHeader, err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return wroteHeader, ctx.Err()
		case result := <-reader.Read():
			done, err := processResult(result)
			if done {
				return wroteHeader, err
			}
		}
	}
}

// emitEvent 写出一个 Responses SSE 事件，自动递增 sequence_number。
func (st *anthropicToResponsesState) emitEvent(event string, data map[string]any) {
	st.seq++
	data["type"] = event
	data["sequence_number"] = st.seq
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	st.w.Write([]byte("event: " + event + "\ndata: " + string(b) + "\n\n"))
	if st.flusher != nil {
		st.flusher.Flush()
	}
}

// ensureTerminal 幂等写出终结事件（completed/incomplete）。
// 上游断流时仍可能有未关闭的 content_block:全部按 output_index 升序收尾
// (补各类 done 事件并回填 completedOutput),否则以终结事件为准重组 output
// 的客户端会丢失尾部的 text/thinking/tool_use 内容,或留下永久 in_progress
// 的 item(added 已发而 done 未到)。
func (st *anthropicToResponsesState) ensureTerminal(status, event string) {
	st.ensureTerminalError(status, event, "")
}

// ensureTerminalError 同 ensureTerminal,但允许在 response 对象上携带 error
// 字段——传输中断按 failed 终止时使用(对照 responses.go emitResponseFailed)。
// failed 终止不置 SawFinish/DoneSeen:stream_result 记 truncated=true,真实
// 中断在日志侧保持可见。
func (st *anthropicToResponsesState) ensureTerminalError(status, event, errMsg string) {
	if st.terminalSent {
		return
	}
	st.terminalSent = true
	blocks := make([]*responsesBlockState, 0, len(st.blocks))
	for _, b := range st.blocks {
		blocks = append(blocks, b)
	}
	// 按 output_index(即 start 到达序)升序,避免 map 遍历乱序导致
	// completedOutput 顺序漂移。
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].outputIndex < blocks[j].outputIndex })
	for _, b := range blocks {
		st.closeBlock(b)
	}
	clear(st.blocks)
	response := map[string]any{
		"id":      st.id,
		"object":  "response",
		"created": time.Now().Unix(),
		"status":  status,
		"model":   st.model,
		"output":  st.completedOutput,
		"usage":   st.fullUsage,
	}
	if status == "incomplete" {
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if errMsg != "" {
		response["error"] = map[string]any{"code": "server_error", "message": errMsg}
	}
	st.emitEvent(event, map[string]any{"response": response})
	st.w.Write([]byte("data: [DONE]\n\n"))
	if st.flusher != nil {
		st.flusher.Flush()
	}
	if errMsg == "" {
		st.stats.DoneSeen = true
		st.stats.SawFinish = true
	}
}

// closeBlock 收尾单个 content_block:补发 done 事件并把 item 回填进
// completedOutput。正常 content_block_stop 与 ensureTerminal(上游断流
// 兜底)共用,保证两条路径的事件形状完全一致,不会互相漂移。
func (st *anthropicToResponsesState) closeBlock(b *responsesBlockState) {
	switch b.kind {
	case "thinking":
		st.completedOutput = append(st.completedOutput, map[string]any{
			"type": "reasoning", "id": b.itemID, "summary": []any{},
		})
		st.emitEvent("response.output_item.done", map[string]any{
			"output_index": b.outputIndex,
			"item": map[string]any{
				"type": "reasoning", "id": b.itemID, "summary": []any{},
			},
		})
	case "text":
		text := ""
		if b.text != nil {
			text = b.text.String()
		}
		st.completedOutput = append(st.completedOutput, map[string]any{
			"type": "message", "id": b.itemID, "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text}},
		})
		st.emitEvent("response.output_text.done", map[string]any{
			"item_id": b.itemID, "text": text,
		})
		st.emitEvent("response.output_item.done", map[string]any{
			"output_index": b.outputIndex,
			"item": map[string]any{
				"type": "message", "id": b.itemID, "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": text}},
			},
		})
	case "tool_use":
		// closeBlock 里 tool_use 的 arguments.done / output_item.done 事件
		// output_index 统一用 b.outputIndex（不是 b.toolIdx——后者是 per-turn
		// 工具序号，会令 output_index 与实际 added 事件的 index 错位）。
		// item 回填完整 {id, call_id, name, arguments, status:completed, type=function_call}。
		if b.initialArguments != "" && !b.argDeltaSeen {
			st.emitEvent("response.function_call_arguments.delta", map[string]any{
				"item_id": b.itemID, "output_index": b.outputIndex, "delta": b.initialArguments,
			})
		}
		arguments := b.initialArguments
		if arguments == "" {
			arguments = "{}"
		}
		item := map[string]any{
			"type": "function_call", "id": b.itemID, "call_id": b.callID,
			"name": b.name, "arguments": arguments, "status": "completed",
		}
		st.completedOutput = append(st.completedOutput, item)
		st.emitEvent("response.function_call_arguments.done", map[string]any{
			"item_id": b.itemID, "output_index": b.outputIndex, "arguments": arguments,
		})
		st.emitEvent("response.output_item.done", map[string]any{
			"output_index": b.outputIndex,
			"item":         item,
		})
	}
}

func (st *anthropicToResponsesState) handleLine(line string) {
	// 终结事件(response.failed/completed + [DONE])已发出后,后续行一律忽略:
	// 既不能向客户端再写事件,也不再消费 st.blocks 状态。
	if st.terminalSent {
		return
	}
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
			if m, _ := msg["model"].(string); m != "" {
				st.model = m
			}
			if u, ok := msg["usage"].(map[string]any); ok {
				mergeUsage(st.fullUsage, u)
			}
		}
		st.emitEvent("response.created", map[string]any{
			"response": map[string]any{
				"id": st.id, "object": "response", "created": time.Now().Unix(),
				"status": "in_progress", "model": st.model, "output": []any{},
			},
		})
	case "content_block_start":
		idx := numberToInt(evt["index"])
		cb, _ := evt["content_block"].(map[string]any)
		bt, _ := cb["type"].(string)
		// 上游若在同一 idx 上重复 start(中间无 stop),旧映射会被覆盖且产生
		// 一个永不关闭的 output_index —— 显式拒绝,让上游错误更明显。
		if _, dup := st.blocks[idx]; dup {
			st.emitEvent("response.failed", map[string]any{
				"response": map[string]any{
					"id": st.id, "status": "failed",
					"error": map[string]any{"message": fmt.Sprintf("duplicate content_block_start for index %d", idx)},
				},
			})
			st.w.Write([]byte("data: [DONE]\n\n"))
			if st.flusher != nil {
				st.flusher.Flush()
			}
			st.terminalSent = true
			return
		}
		// 在 start 时分配并记录 output_index,确保 stop 时回填的是同一个 index。
		b := &responsesBlockState{kind: bt, outputIndex: st.outputIndex}
		st.blocks[idx] = b
		st.outputIndex++
		switch bt {
		case "thinking":
			b.itemID = "rs_" + randomHex(12)
			b.text = &strings.Builder{}
			st.emitEvent("response.output_item.added", map[string]any{
				"output_index": b.outputIndex,
				"item": map[string]any{
					"type": "reasoning", "id": b.itemID, "summary": []any{},
				},
			})
		case "text":
			b.itemID = "msg_" + randomHex(12)
			b.text = &strings.Builder{}
			st.emitEvent("response.output_item.added", map[string]any{
				"output_index": b.outputIndex,
				"item": map[string]any{
					"type": "message", "id": b.itemID, "role": "assistant",
					"status": "in_progress", "content": []any{},
				},
			})
			st.emitEvent("response.content_part.added", map[string]any{
				"item_id": b.itemID, "output_index": b.outputIndex,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": ""},
			})
		case "tool_use":
			b.itemID = "fc_" + randomHex(12)
			b.toolIdx = st.toolCount
			st.toolCount++
			callID, _ := cb["id"].(string)
			// 上游 tool_use name 还原为客户端原始名（缩短名经 rewrites
			// 映射），覆盖 output_item.added / output_item.done /
			// response.completed 全部呈现面。
			name, _ := cb["name"].(string)
			name = st.rewrites.restore(name)
			b.callID = callID
			b.name = name
			if inp, ok := cb["input"]; ok && inp != nil {
				if raw, err := json.Marshal(inp); err == nil {
					b.initialArguments = string(raw)
				}
			}
			st.emitEvent("response.output_item.added", map[string]any{
				"output_index": b.outputIndex,
				"item": map[string]any{
					"type": "function_call", "id": b.itemID, "call_id": callID,
					"name": name, "arguments": "", "status": "in_progress",
				},
			})
		}
	case "content_block_delta":
		idx := numberToInt(evt["index"])
		b := st.blocks[idx]
		if b == nil {
			return
		}
		d, _ := evt["delta"].(map[string]any)
		dt, _ := d["type"].(string)
		switch dt {
		case "text_delta":
			if t, _ := d["text"].(string); t != "" {
				if b.text != nil {
					b.text.WriteString(t)
				}
				st.stats.TextChars += len(t)
				st.emitEvent("response.output_text.delta", map[string]any{
					"item_id": b.itemID, "delta": t, "logprobs": []any{},
				})
			}
		case "thinking_delta":
			if st.wantReasoning {
				if t, _ := d["thinking"].(string); t != "" {
					if b.text != nil {
						b.text.WriteString(t)
					}
					st.stats.ReasoningChars += len(t)
					st.emitEvent("response.reasoning_summary_text.delta", map[string]any{
						"item_id": b.itemID, "delta": t,
					})
				}
			}
		case "input_json_delta":
			if pj, _ := d["partial_json"].(string); pj != "" {
				b.argDeltaSeen = true
				st.emitEvent("response.function_call_arguments.delta", map[string]any{
					"item_id": b.itemID, "output_index": b.outputIndex, "delta": pj,
				})
			}
		case "signature_delta":
			// Anthropic thinking/redacted_thinking 的 signature_delta：仅计数，
			// 不对下游产生任何事件（Responses 无对应概念；下游 strict 反序列化
			// 会拒绝未知增量字段）。
			st.signatureDeltas++
		}
	case "content_block_stop":
		idx := numberToInt(evt["index"])
		b := st.blocks[idx]
		if b == nil {
			return
		}
		st.closeBlock(b)
		delete(st.blocks, idx)
	case "message_delta":
		if u, ok := evt["usage"].(map[string]any); ok {
			mergeUsage(st.fullUsage, u)
		}
		if delta, ok := evt["delta"].(map[string]any); ok {
			if sr, _ := delta["stop_reason"].(string); sr != "" {
				st.stats.FinishReason = sr
				st.stats.SawFinish = true
			}
		}
	case "message_stop":
		if st.stats.FinishReason == "max_tokens" || st.stats.FinishReason == "max_tokens_cap" {
			st.ensureTerminal("incomplete", "response.incomplete")
		} else {
			st.ensureTerminal("completed", "response.completed")
		}
	case "error":
		em, _ := evt["error"].(map[string]any)
		message := "upstream error"
		if m, _ := em["message"].(string); m != "" {
			message = m
		}
		st.emitEvent("response.failed", map[string]any{
			"response": map[string]any{
				"id": st.id, "status": "failed",
				"error": map[string]any{"message": message},
			},
		})
		st.w.Write([]byte("data: [DONE]\n\n"))
		if st.flusher != nil {
			st.flusher.Flush()
		}
		st.terminalSent = true
	}
}
