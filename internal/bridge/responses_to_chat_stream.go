package bridge

import (
	"encoding/json"
	"strings"
)

// responses_to_chat_stream.go holds the pure Responses SSE → Chat Completions
// SSE state machine (app state machine #2), migrated out of
// internal/app/chat_to_responses_upstream.go. Every frame the old writer pushed
// onto the http.ResponseWriter is now returned as an OutEvent whose Raw bytes
// are the exact `data: ...\n\n` payload; stamping (`created`) and the chat
// completion ID are injected so the conversion is deterministic and I/O-free.
//
// The state transitions, usage merging, and fallback rules are preserved
// verbatim. See the red-line invariants in the unit brief:
//
//   - Finalize is idempotent (finalized switch shared by response.completed /
//     incomplete / failed / error and the EOF fallback); the terminal
//     finish+usage+[DONE] block is emitted exactly once, never twice.
//   - The tool-call three keys (call_id / item id "fc_*" / output_index) all
//     resolve to the same chat tool_calls index.
//   - output_item.done emits only the still-unsent argument remainder
//     (concat-safe); an undeclared call gets its announce chunk + full args.
//   - incomplete → length; sawTool → tool_calls; otherwise stop.
//   - A refusal delta is folded inline into content (the chat streaming delta
//     has no refusal slot).
//   - responsesUsageToChatBridge cached_tokens alias + reasoning_tokens
//     relocation run only on the terminal usage block (Finalize), keyed off the
//     merged fullUsage snapshot.

// ResponsesToChatState converts an upstream Responses SSE stream into Chat
// Completions frames. It implements Streamer.
type ResponsesToChatState struct {
	// idGen stamps the chat completion id once at construction ("chatcmpl-" +
	// idGen(prefix, 12)); production wires it to internal/random hex so reopened
	// ids keep the chatcmpl- shape (never chatcmpl_/resp_ — see DeterministicResponseID).
	idGen HexIDGen
	// nowFn stamps each chunk's `created` field; production is time.Now().Unix.
	nowFn NowFn

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
	finishReason  string // 终态 finish 原因(空 = 未定,Finalize 时合成)
	fullUsage     map[string]any
	finalized     bool // idempotent Finalize switch (already wrote [DONE]/terminal frame)
}

// NewResponsesToChatState builds the state machine for one upstream Responses
// stream. model is the fallback chat model id until response.created supplies
// the real one. idGen/nowFn inject randomness and time so the conversion stays
// pure and deterministic under test.
func NewResponsesToChatState(idGen HexIDGen, nowFn NowFn, model string, keepReasoning, includeUsage bool) *ResponsesToChatState {
	if idGen == nil {
		idGen = DefaultHexIDGen
	}
	if nowFn == nil {
		nowFn = DefaultNowFn
	}
	return &ResponsesToChatState{
		idGen:         idGen,
		nowFn:         nowFn,
		id:            "chatcmpl-" + idGen("chatcmpl-", 12),
		model:         model,
		keepReasoning: keepReasoning,
		includeUsage:  includeUsage,
		toolIndices:   map[string]int{},
		toolAnnounced: map[int]bool{},
		arguments:     map[int]string{},
		fullUsage:     map[string]any{},
	}
}

// Usage returns the accumulated, merged upstream Responses usage map (the full,
// raw protocol-side shape). The app shell converts it
// (ResponsesUsageToChatBridge) and records it via statsx.
func (st *ResponsesToChatState) Usage() map[string]any { return st.fullUsage }

// Counters returns a read-only snapshot of the stream accounting.
func (st *ResponsesToChatState) Counters() StreamCounters {
	return StreamCounters{
		ToolCallCount: st.toolCount,
		FinishReason:  st.finishReason,
		SawFinish:     st.finalized,
		DoneSeen:      st.finalized,
	}
}

// Finalize 幂等地结束 chat 流:补发终态 finish chunk(incomplete → length,
// 有 tool call → tool_calls,否则 stop)、includeUsage 时的 usage 终块与
// [DONE]。response.completed/incomplete 正常路径与 response.failed/error/
// EOF 兜底路径共用;第二次及以后调用是 no-op。
func (st *ResponsesToChatState) Finalize() []OutEvent {
	if st.finalized {
		return nil
	}
	st.finalized = true
	if st.finishReason == "" {
		st.finishReason = "stop"
		if st.sawTool {
			st.finishReason = "tool_calls"
		}
	}
	var out []OutEvent
	if st.sentRole {
		out = append(out, st.emitChunk(map[string]any{}, st.finishReason, nil))
	}
	if st.includeUsage && len(st.fullUsage) > 0 {
		out = append(out, st.emitChunk(map[string]any{}, "", ResponsesUsageToChatBridge(st.fullUsage)))
	}
	out = append(out, OutEvent{Raw: []byte("data: [DONE]\n\n"), Terminal: true})
	return out
}

// emitChunk builds one Chat SSE chunk frame. finishReason non-empty attaches
// finish_reason; usage non-nil attaches usage (terminal block only). Returns the
// zero OutEvent when the chunk fails to marshal (kept from the original, which
// silently dropped the frame); the Handle dispatcher skips zero events.
func (st *ResponsesToChatState) emitChunk(delta map[string]any, finishReason string, usage map[string]any) OutEvent {
	chunk := map[string]any{
		"id":      st.id,
		"object":  "chat.completion.chunk",
		"created": st.nowFn(),
		"model":   st.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": FinishReasonOr(finishReason),
		}},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return OutEvent{}
	}
	return OutEvent{Data: chunk, Raw: []byte("data: " + string(b) + "\n\n")}
}

// ensureRole emits the first assistant-role chunk exactly once.
func (st *ResponsesToChatState) ensureRole() OutEvent {
	if st.sentRole {
		return OutEvent{}
	}
	st.sentRole = true
	return st.emitChunk(map[string]any{"role": "assistant", "content": ""}, "", nil)
}

// toolIdxFor 解析 item id/call_id 对应的 chat tool_calls index:未知 item
// （上游没发 output_item.added 直接发 delta/done,Observed 场景）分配新
// index,保证后续补发首 chunk 时 index 稳定。
func (st *ResponsesToChatState) toolIdxFor(itemID string) int {
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
func (st *ResponsesToChatState) aliasToolKey(fromKey, toKey string) {
	if fromKey == "" || toKey == "" || fromKey == toKey {
		return
	}
	if idx, ok := st.toolIndices[toKey]; ok {
		st.toolIndices[fromKey] = idx
	}
}

// registerToolKeys allocates the chat tool_calls index for a tool call and
// points every id shape the later events may use at it: the call_id, the item
// id ("fc_..."), and the output_index. The index must be allocated BEFORE the
// aliases: aliasToolKey resolves through toolIndices[callID], so aliasing
// first is a silent no-op and the argument deltas open a new index.
func (st *ResponsesToChatState) registerToolKeys(evt, item map[string]any) (callID string, toolIdx int) {
	itemID, _ := item["id"].(string)
	callID, _ = item["call_id"].(string)
	if callID == "" {
		callID = itemID
	}
	toolIdx = st.toolIdxFor(callID)
	st.aliasToolKey(itemID, callID)
	if oi, ok := evt["output_index"].(float64); ok {
		st.aliasToolKey(OutputIndexKey(int(oi)), callID)
	}
	return callID, toolIdx
}

// eventToolIdx resolves the chat tool_calls index an argument event refers
// to. Responses names the item by item_id ("fc_...") — never by the call_id
// that output_item.added announced — and may omit it entirely, leaving only
// output_index. Both are registered by registerToolKeys.
func (st *ResponsesToChatState) eventToolIdx(evt map[string]any) int {
	itemID, _ := evt["item_id"].(string)
	if itemID != "" {
		if idx, ok := st.toolIndices[itemID]; ok {
			return idx
		}
	}
	if oi, ok := evt["output_index"].(float64); ok {
		if idx, ok := st.toolIndices[OutputIndexKey(int(oi))]; ok {
			return idx
		}
	}
	// Unknown item (no output_item.added seen): toolIdxFor allocates and
	// remembers, so a later done event keeps the same slot.
	return st.toolIdxFor(itemID)
}

// Handle consumes one upstream Responses SSE line and returns the frames to
// forward downstream, in order.
func (st *ResponsesToChatState) Handle(ev StreamEvent) []OutEvent {
	payload, ok := strings.CutPrefix(ev.Line(), "data: ")
	if !ok {
		return nil
	}
	var evt map[string]any
	if json.Unmarshal([]byte(payload), &evt) != nil {
		return nil
	}
	var out []OutEvent
	emit := func(e OutEvent) {
		if len(e.Raw) == 0 && e.Data == nil && e.Event == "" {
			return
		}
		out = append(out, e)
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
				MergeUsage(st.fullUsage, u)
			}
		}
		emit(st.ensureRole())
	case "response.output_text.delta":
		emit(st.ensureRole())
		if t, _ := evt["delta"].(string); t != "" {
			emit(st.emitChunk(map[string]any{"content": t}, "", nil))
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if st.keepReasoning {
			emit(st.ensureRole())
			if t, _ := evt["delta"].(string); t != "" {
				emit(st.emitChunk(map[string]any{"reasoning_content": t}, "", nil))
			}
		}
	case "response.refusal.delta":
		// refusal 增量并入正文（与非流式 convertResponsesToChat 把 refusal
		// 放 message 字段不同——chat 流式 delta 没有 refusal 槽位且
		// content=-null 语义不完整;内联进 content 是对 OpenAI 客户端最
		// 保真的降级,与非流式 "content 里看不到 refusal" 略不一致,详见
		// convertResponsesToChat 的 refusal 注释）。
		emit(st.ensureRole())
		if t, _ := evt["delta"].(string); t != "" {
			emit(st.emitChunk(map[string]any{"content": t}, "", nil))
		}
	case "response.output_item.added":
		item, _ := evt["item"].(map[string]any)
		if item == nil {
			return out
		}
		switch item["type"] {
		case "function_call", "tool_call":
			st.sawTool = true
			callID, toolIdx := st.registerToolKeys(evt, item)
			st.toolAnnounced[toolIdx] = true
			emit(st.ensureRole())
			emit(st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": toolIdx, "id": callID, "type": "function",
				"function": map[string]any{"name": ToString(item["name"]), "arguments": ""},
			}}}, "", nil))
		}
	case "response.function_call_arguments.delta", "response.tool_call_arguments.delta":
		emit(st.ensureRole())
		toolIdx := st.eventToolIdx(evt)
		if pj, _ := evt["delta"].(string); pj != "" {
			st.arguments[toolIdx] += pj
			emit(st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": toolIdx, "id": nil, "type": "function",
				"function": map[string]any{"name": "", "arguments": pj},
			}}}, "", nil))
		}
	case "response.function_call_arguments.done", "response.tool_call_arguments.done":
		emit(st.ensureRole())
		toolIdx := st.eventToolIdx(evt)
		// done 携带完整 arguments JSON:只补发已下发前缀之后的差量,
		// 避免客户端 concat 后重复（对齐 sub2api resToChatHandleFuncArgsDone）。
		if completed, _ := evt["arguments"].(string); completed != "" {
			emitted := st.arguments[toolIdx]
			if completed != emitted && strings.HasPrefix(completed, emitted) {
				remainder := completed[len(emitted):]
				st.arguments[toolIdx] = completed
				emit(st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
					"index": toolIdx, "id": nil, "type": "function",
					"function": map[string]any{"name": "", "arguments": remainder},
				}}}, "", nil))
			}
		}
	case "response.output_item.done":
		item, _ := evt["item"].(map[string]any)
		if item == nil {
			return out
		}
		switch item["type"] {
		case "function_call", "tool_call":
			st.sawTool = true
			callID, toolIdx := st.registerToolKeys(evt, item)
			// 没有 add/delta 出现过（罕见）:补一次首 chunk 宣告工具调用,
			// 并把 item 上的完整 arguments 作为单段增量发完。
			if !st.toolAnnounced[toolIdx] {
				st.toolAnnounced[toolIdx] = true
				name := ToString(item["name"])
				emit(st.ensureRole())
				emit(st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
					"index": toolIdx, "id": callID, "type": "function",
					"function": map[string]any{"name": name, "arguments": ""},
				}}}, "", nil))
				if args, _ := item["arguments"].(string); args != "" && st.arguments[toolIdx] == "" {
					st.arguments[toolIdx] = args
					emit(st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
						"index": toolIdx, "id": nil, "type": "function",
						"function": map[string]any{"name": "", "arguments": args},
					}}}, "", nil))
				}
			}
		}
	case "response.completed", "response.incomplete":
		if resp, ok := evt["response"].(map[string]any); ok {
			if u, ok := resp["usage"].(map[string]any); ok {
				MergeUsage(st.fullUsage, u)
			}
			if status, _ := resp["status"].(string); status == "incomplete" {
				st.finishReason = "length"
			} else if st.sawTool {
				st.finishReason = "tool_calls"
			} else {
				st.finishReason = "stop"
			}
		}
		out = append(out, st.Finalize()...)
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
		// 终态错误事件前先确保 role 已宣告（与原实现的 ensureRole 一致）,
		// 让 OpenAI 客户端看到合法的首 chunk 再看到错误帧。
		if !st.sentRole {
			emit(st.ensureRole())
		}
		emit(OutEvent{Raw: []byte("data: " + `{"error":{"message":` + JSONString(message) + `}}` + "\n\n")})
		// 终态错误事件也补 [DONE](幂等):避免 OpenAI SDK 挂在缺哨兵的流上。
		out = append(out, st.Finalize()...)
	}
	return out
}
