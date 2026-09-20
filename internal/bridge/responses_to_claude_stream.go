package bridge

import (
	"encoding/json"
	"strings"
)

// responses_to_claude_stream.go holds the pure Responses SSE → Claude Messages
// SSE state machine (app state machine #4, migrated out of
// internal/app/claude_responses.go's claudeResponsesStreamHandler). Every frame
// the old writer pushed onto the http.ResponseWriter via writeSSEEvent is now
// returned as an OutEvent whose Raw bytes are the exact
// `event: <name>\ndata: <json>\n\n` payload. ID generation (msg_/toolu_ bodies)
// is injected so the conversion is deterministic and I/O-free.
//
// Unlike the line-oriented state machines, this machine's input *frame* is the
// unit the app shell aggregates (an `event:` line plus one or more `data:`
// lines delimited by a blank line). The shell owns that SSE reassembly and only
// hands the bridge a decoded event map plus the `event:` line value; the bridge
// decides type by evt["type"] with the frame event as fallback, exactly as the
// original handleEvent did.
//
// The state transitions, usage merging, and fallback rules are preserved
// verbatim. See the red-line invariants in the unit brief:
//
//   - adoptResponseID: response.created/in_progress carrying a non-empty
//     response.id re-stamps the message id via NormalizeClaudeMessageID (the
//     msg_-prefixed deterministic form), aligning with the non-streaming
//     convertResponsesToClaude shape (a chatcmpl_/resp_ id never leaks).
//   - encrypted_content → signature_delta roundtrip: on response.completed a
//     reasoning item's encrypted_content is latched onto its thinking block,
//     then emitted as a signature_delta right before that block's
//     content_block_stop during finalize (Claude Code can echo the signature
//     back on the next turn).
//   - Empty-reply guard: when no text/tool_use was produced but reasoning was
//     accumulated (keepReasoning on), the reasoning fallback is promoted to a
//     single text block so the client never receives an empty message.
//   - Blocks are closed in ascending claudeIndex order (finalize sorts them), so
//     content_block_stop frames go out in index order.
//   - finalize is idempotent: the shell's doFinalize gates on its own
//     `finalized` flag, and the bridge's Finalize is a no-op after the first
//     call, so EOF / [DONE] / response.completed never double-emit the
//     message_delta/message_stop trailer.

// ResponsesEvent is the StreamEvent fed to ResponsesToClaudeStream: one decoded
// upstream Responses SSE event plus the SSE `event:` line value (used as the
// type fallback when the JSON has no "type" field). The zero value carries no
// data.
type ResponsesEvent struct {
	// Event is the decoded JSON event payload (a single `data:` line, or a bare
	// JSON line). Nil when the frame carried no usable JSON.
	Event map[string]any
	// FrameEvent is the `event:` field of the SSE frame, or "" when absent.
	FrameEvent string
}

// Line implements StreamEvent. This machine does not operate on raw lines, so
// Line always returns ""; the payload lives in Event/FrameEvent.
func (e ResponsesEvent) Line() string { return "" }

// responsesToClaudeDoneEvent builds the terminal frames are *not* needed here:
// the Claude Messages protocol has no downstream `[DONE]` sentinel (unlike the
// Responses / Chat shapes). Termination is conveyed purely by message_stop.
// The OutEvent.Terminal flag is set on the closing message_stop so the shell
// can stop selecting.

// ResponsesToClaudeStream converts an upstream native Responses SSE stream into
// a Claude Messages SSE stream. It holds the full stream state and emits named
// `event:`/`data:` frames as OutEvent values. It is driven by the app shell's
// frame-reassembly loop; all side effects (writing, flushing, stats, usage
// recording) live in the shell.
type ResponsesToClaudeStream struct {
	// idGen mints the fallback message id body (msg_ + 24) and any missing
	// tool_use id body (toolu_ + 12); production wires it to internal/random
	// string. It also backs NormalizeClaudeMessageID's deterministic mapping.
	idGen IDGen

	msgID         string
	model         string
	wantReasoning bool

	blockIndex       int
	messageStartSent bool
	finished         bool
	stopReason       string
	fullUsage        map[string]any
	producedText     bool

	// output_index -> block (text/thinking/tool all aggregate by output_index).
	blocks map[int]*ClaudeResponsesBlock
	// item_id -> output_index, so a delta event can resolve its output_index.
	itemToOutput map[string]int
	toolOrder    []int
	// reasoningFallback accumulates reasoning while wantReasoning is on, for the
	// empty-reply text promotion guard.
	reasoningFallback strings.Builder

	// counters mirrored into logging.StreamStats by the app shell.
	textChars         int
	reasoningChars    int
	promotedReasoning bool
	finishReason      string
	sawFinish         bool
	doneSeen          bool
	// chunkCount counts each delta event the original handleEvent passed to
	// stats.NoteChunk (one per non-empty text/refusal/arguments/reasoning delta
	// and per default-fallback delta). The app shell diffs this counter to call
	// stats.NoteChunk with identical Chunks/FirstChunkAt timing.
	chunkCount int
	// finalized gates Finalize to a single emission (the original used a local
	// `finalized` bool in the shell; here the bridge owns the idempotence).
	finalized bool
}

// NewResponsesToClaudeStream builds the state machine for one upstream native
// Responses stream. model is the echoed Claude message model; wantReasoning
// controls whether reasoning surfaces as thinking blocks (true) or is promoted
// to visible text (false). idGen injects randomness (production: internal/
// random string); when nil the production default is used.
func NewResponsesToClaudeStream(idGen IDGen, model string, wantReasoning bool) *ResponsesToClaudeStream {
	if idGen == nil {
		idGen = DefaultIDGen
	}
	return &ResponsesToClaudeStream{
		idGen:         idGen,
		msgID:         "msg_" + idGen("msg_", 24),
		model:         model,
		wantReasoning: wantReasoning,
		stopReason:    "end_turn",
		fullUsage:     map[string]any{},
		blocks:        map[int]*ClaudeResponsesBlock{},
		itemToOutput:  map[string]int{},
		toolOrder:     []int{},
	}
}

// Usage returns the accumulated, merged upstream Responses usage map (the raw
// input_tokens/output_tokens/total_tokens shape). The app shell converts it
// (ResponsesUsageToChat) and records it via statsx at end of stream.
func (st *ResponsesToClaudeStream) Usage() map[string]any { return st.fullUsage }

// Counters returns a read-only snapshot of the stream accounting. ToolCallCount
// is the number of tool_use blocks started.
func (st *ResponsesToClaudeStream) Counters() StreamCounters {
	return StreamCounters{
		TextChars:         st.textChars,
		ReasoningChars:    st.reasoningChars,
		PromotedReasoning: st.promotedReasoning,
		ToolCallCount:     len(st.toolOrder),
		FinishReason:      st.finishReason,
		SawFinish:         st.sawFinish,
		DoneSeen:          st.doneSeen,
	}
}

// ChunkCount reports how many delta events the original handleEvent passed to
// stats.NoteChunk. The app shell calls stats.NoteChunk once per increment so
// its Chunks/FirstChunkAt timing matches the old handler.
func (st *ResponsesToClaudeStream) ChunkCount() int { return st.chunkCount }

// Finished reports whether response.completed/incomplete (or an EOF/[DONE]
// synthesis) marked the stream finished, i.e. the finalize path was reached.
// The app shell uses this to know the message_delta/message_stop trailer is
// pending.
func (st *ResponsesToClaudeStream) Finished() bool { return st.finished }

// Finalized reports whether the message_delta/message_stop trailer has already
// been emitted (Finalize ran). The app shell's EOF path uses it to avoid a
// double trailer after a completed/[DONE] already finalized the stream.
func (st *ResponsesToClaudeStream) Finalized() bool { return st.finalized }

// PingEvent returns the keepalive ping frame the app shell emits on the
// keepalive tick (before the first upstream token this is the only thing the
// client receives; it must NOT fake message_start). This mirrors the original
// keepalive branch which emitted a bare `ping` event.
func (st *ResponsesToClaudeStream) PingEvent() OutEvent {
	return st.emit("ping", map[string]any{"type": "ping"})
}

// ProducedOutput reports whether any text delta or tool_use block was produced
// — the guard the original shell used to decide between synthesizing a normal
// finalize vs an error on a clean EOF.
func (st *ResponsesToClaudeStream) ProducedOutput() bool {
	return st.producedText || len(st.toolOrder) > 0
}

// IndexOfToolOrder reports the position of v in order (the first-seen tool
// declaration order). Pure helper kept verbatim from the original
// indexOfToolOrder.
func IndexOfToolOrder(order []int, v int) (int, bool) {
	for i, x := range order {
		if x == v {
			return i, true
		}
	}
	return -1, false
}

// emit builds one named `event: <name>\ndata: <json>\n\n` frame, exactly
// matching the old writeSSEEvent byte format. Returns the zero OutEvent when
// the payload fails to marshal (kept from the original, which silently returned
// without writing); the caller's emit helper skips zero events.
func (st *ResponsesToClaudeStream) emit(event string, data map[string]any) OutEvent {
	b, err := json.Marshal(data)
	if err != nil {
		return OutEvent{}
	}
	return OutEvent{Event: event, Data: data, Raw: []byte("event: " + event + "\ndata: " + string(b) + "\n\n")}
}

// emitError builds the Claude `error` frame the old emitError closure produced.
func (st *ResponsesToClaudeStream) emitError(msg string) OutEvent {
	return st.emit("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": msg},
	})
}

// adoptResponseID re-stamps the message id from a response.created/in_progress
// event's response.id (normalized to the msg_ namespace), aligning with the
// non-streaming convertResponsesToClaude shape. No-op once message_start went
// out.
func (st *ResponsesToClaudeStream) adoptResponseID(evt map[string]any) {
	if st.messageStartSent {
		return
	}
	if resp, ok := evt["response"].(map[string]any); ok {
		if id, _ := resp["id"].(string); id != "" {
			st.msgID = NormalizeClaudeMessageID(st.idGen, id)
		}
	}
}

// ensureStart emits message_start (+ping) exactly once, building the message
// usage from the usage accumulated so far.
func (st *ResponsesToClaudeStream) ensureStart(out *[]OutEvent) {
	if st.messageStartSent {
		return
	}
	st.messageStartSent = true
	*out = append(*out, st.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": st.msgID, "type": "message", "role": "assistant",
			"content": []any{}, "model": st.model,
			"stop_reason": nil, "stop_sequence": nil,
			"usage": BuildClaudeMessageUsage(st.fullUsage),
		},
	}))
	*out = append(*out, st.emit("ping", map[string]any{"type": "ping"}))
}

// getOrCreate returns the block for outputIndex, allocating a fresh one (and a
// claude index) on first use or on a kind switch. Kept verbatim from the
// original getOrCreateBlock closure.
func (st *ResponsesToClaudeStream) getOrCreate(outputIndex int, kind string) *ClaudeResponsesBlock {
	if b, ok := st.blocks[outputIndex]; ok {
		if b.Kind == kind {
			if b.ClaudeIndex < 0 {
				b.ClaudeIndex = st.blockIndex
				st.blockIndex++
			}
			return b
		}
	}
	// 同一 output_index 上类型切换（如 message->tool）时沿用新 kind。
	b := &ClaudeResponsesBlock{ClaudeIndex: st.blockIndex, Kind: kind}
	st.blocks[outputIndex] = b
	st.blockIndex++
	return b
}

func (st *ResponsesToClaudeStream) startTextBlock(out *[]OutEvent, b *ClaudeResponsesBlock) {
	if b.Open {
		return
	}
	b.Open = true
	st.ensureStart(out)
	*out = append(*out, st.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": b.ClaudeIndex,
		"content_block": map[string]any{"type": "text", "text": ""},
	}))
}

func (st *ResponsesToClaudeStream) startThinkingBlock(out *[]OutEvent, b *ClaudeResponsesBlock) {
	if b.Open {
		return
	}
	b.Open = true
	st.ensureStart(out)
	*out = append(*out, st.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": b.ClaudeIndex,
		"content_block": map[string]any{"type": "thinking", "thinking": ""},
	}))
}

func (st *ResponsesToClaudeStream) startToolBlock(out *[]OutEvent, b *ClaudeResponsesBlock) {
	if b.Open {
		return
	}
	b.Open = true
	st.ensureStart(out)
	if b.ToolID == "" {
		b.ToolID = "toolu_" + st.idGen("toolu_", 12)
	}
	*out = append(*out, st.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": b.ClaudeIndex,
		"content_block": map[string]any{
			"type": "tool_use", "id": b.ToolID, "name": b.ToolName, "input": map[string]any{},
		},
	}))
	if _, exists := IndexOfToolOrder(st.toolOrder, b.ClaudeIndex); !exists {
		st.toolOrder = append(st.toolOrder, b.ClaudeIndex)
	}
}

// emitText emits a text_delta for block b (starting the text block first) and
// accounts for the produced text. Empty text is a no-op.
func (st *ResponsesToClaudeStream) emitText(out *[]OutEvent, b *ClaudeResponsesBlock, text string) {
	if text == "" {
		return
	}
	st.textChars += len(text)
	st.producedText = true
	st.startTextBlock(out, b)
	*out = append(*out, st.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": b.ClaudeIndex,
		"delta": map[string]any{"type": "text_delta", "text": text},
	}))
}

// emitThinking emits a thinking_delta for block b when wantReasoning, else
// promotes the reasoning to a visible text delta (PromotedReasoning). Empty
// text is a no-op.
func (st *ResponsesToClaudeStream) emitThinking(out *[]OutEvent, b *ClaudeResponsesBlock, text string) {
	if text == "" {
		return
	}
	st.reasoningChars += len(text)
	if st.wantReasoning {
		st.reasoningFallback.WriteString(text)
		st.startThinkingBlock(out, b)
		*out = append(*out, st.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": b.ClaudeIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": text},
		}))
	} else {
		st.promotedReasoning = true
		st.emitText(out, b, text)
	}
}

// emitTool emits an input_json_delta for block b (starting the tool block
// first). Empty partial is a no-op.
func (st *ResponsesToClaudeStream) emitTool(out *[]OutEvent, b *ClaudeResponsesBlock, partial string) {
	if partial == "" {
		return
	}
	st.startToolBlock(out, b)
	*out = append(*out, st.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": b.ClaudeIndex,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": partial},
	}))
}

// Handle consumes one decoded upstream Responses SSE event (plus the frame's
// `event:` value) and returns the Claude frames to forward downstream, in
// order. It is the pure form of the original handleEvent method: all state
// transitions and callback closures (getOrCreate/ensureStart/adoptResponseID/
// emitText/emitThinking/emitTool/emitError) are folded into methods that append
// to the output slice.
//
// Terminal bookkeeping (sawFinish/finishReason/doneSeen/finished) is updated
// here but the actual message_delta/message_stop trailer is emitted by
// Finalize, which the app shell calls once after Handle reports a finished
// stream (or on the EOF/[DONE] synthesis).
func (st *ResponsesToClaudeStream) Handle(ev StreamEvent) []OutEvent {
	rev, _ := ev.(ResponsesEvent)
	evt := rev.Event
	if evt == nil {
		return nil
	}
	frameEvent := rev.FrameEvent

	var out []OutEvent

	typ, _ := evt["type"].(string)
	if typ == "" {
		typ = frameEvent
	}
	outputIndex := -1
	if v, ok := evt["output_index"].(float64); ok {
		outputIndex = int(v)
	}
	itemID, _ := evt["item_id"].(string)

	outputIndexForItem := func(id string, fallback int) int {
		if id == "" {
			return fallback
		}
		if oi, ok := st.itemToOutput[id]; ok {
			return oi
		}
		return fallback
	}

	switch typ {
	case "response.created", "response.in_progress", "response.queued":
		st.adoptResponseID(evt)
		if resp, ok := evt["response"].(map[string]any); ok {
			if u, ok := resp["usage"].(map[string]any); ok {
				for k, v := range u {
					st.fullUsage[k] = v
				}
			}
		}
		st.ensureStart(&out)
	case "response.output_item.added":
		item, _ := evt["item"].(map[string]any)
		if item == nil {
			return out
		}
		itemType, _ := item["type"].(string)
		id, _ := item["id"].(string)
		if outputIndex >= 0 && id != "" {
			st.itemToOutput[id] = outputIndex
		}
		switch itemType {
		case "message":
			// content_part 后续会创建文本块，这里仅登记映射。
			if outputIndex >= 0 {
				if _, ok := st.blocks[outputIndex]; !ok {
					st.blocks[outputIndex] = &ClaudeResponsesBlock{ClaudeIndex: -1, Kind: "text"}
				}
			}
		case "reasoning":
			if outputIndex >= 0 {
				b := st.getOrCreate(outputIndex, "thinking")
				_ = b
			}
		case "function_call", "tool_call":
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID, _ = item["id"].(string)
			}
			name, _ := item["name"].(string)
			if outputIndex >= 0 {
				b := st.getOrCreate(outputIndex, "tool")
				if callID != "" {
					b.ToolID = callID
				}
				if name != "" {
					b.ToolName = name
				}
				if id != "" {
					st.itemToOutput[id] = outputIndex
				}
				if callID != "" {
					st.itemToOutput[callID] = outputIndex
				}
			}
		case "apply_patch_call", "shell_call":
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID, _ = item["id"].(string)
			}
			name := "apply_patch"
			if itemType == "shell_call" {
				name = "shell"
			}
			if outputIndex >= 0 {
				b := st.getOrCreate(outputIndex, "tool")
				if callID != "" {
					b.ToolID = callID
				}
				b.ToolName = name
				if id != "" {
					st.itemToOutput[id] = outputIndex
				}
			}
		default:
			// 未知 item（web_search_call 等）：登记为文本占位，delta 到达时降级。
			if outputIndex >= 0 && itemType != "" {
				if _, ok := st.blocks[outputIndex]; !ok {
					st.blocks[outputIndex] = &ClaudeResponsesBlock{ClaudeIndex: -1, Kind: "text"}
				}
			}
		}
	case "response.content_part.added":
		oi := outputIndex
		if oi < 0 {
			oi = outputIndexForItem(itemID, -1)
		}
		if oi < 0 {
			return out
		}
		part, _ := evt["part"].(map[string]any)
		partType := ""
		if part != nil {
			partType, _ = part["type"].(string)
		}
		// refusal 也按文本处理。
		b := st.getOrCreate(oi, "text")
		_ = b
		_ = partType
	case "response.output_text.delta":
		oi := outputIndex
		if oi < 0 {
			oi = outputIndexForItem(itemID, -1)
		}
		if oi < 0 {
			oi = 0
		}
		delta, _ := evt["delta"].(string)
		if delta == "" {
			return out
		}
		st.chunkCount++
		b := st.getOrCreate(oi, "text")
		st.emitText(&out, b, delta)
	case "response.refusal.delta":
		oi := outputIndex
		if oi < 0 {
			oi = outputIndexForItem(itemID, 0)
		}
		delta, _ := evt["delta"].(string)
		if delta == "" {
			if r, ok := evt["refusal"].(string); ok {
				delta = r
			}
		}
		if delta == "" {
			return out
		}
		st.chunkCount++
		b := st.getOrCreate(oi, "text")
		st.emitText(&out, b, delta)
	case "response.function_call_arguments.delta":
		oi := outputIndex
		if oi < 0 {
			oi = outputIndexForItem(itemID, -1)
		}
		if oi < 0 {
			return out
		}
		delta, _ := evt["delta"].(string)
		if delta == "" {
			return out
		}
		st.chunkCount++
		b := st.getOrCreate(oi, "tool")
		st.emitTool(&out, b, delta)
	case "response.reasoning_summary_text.delta":
		oi := outputIndex
		if oi < 0 {
			oi = outputIndexForItem(itemID, 0)
		}
		delta, _ := evt["delta"].(string)
		if delta == "" {
			return out
		}
		st.chunkCount++
		b := st.getOrCreate(oi, "thinking")
		st.emitThinking(&out, b, delta)
	case "response.reasoning_text.delta":
		oi := outputIndex
		if oi < 0 {
			oi = outputIndexForItem(itemID, 0)
		}
		delta, _ := evt["delta"].(string)
		if delta == "" {
			if t, ok := evt["text"].(string); ok {
				delta = t
			}
		}
		if delta == "" {
			return out
		}
		st.chunkCount++
		b := st.getOrCreate(oi, "thinking")
		st.emitThinking(&out, b, delta)
	case "response.output_text.done", "response.refusal.done", "response.function_call_arguments.done", "response.reasoning_summary_part.done", "response.reasoning_summary_text.done", "response.content_part.done", "response.output_item.done":
		// 结束标记：统一在 completed 处关块，避免半流提前关块后同 index 又来 delta。
		return out
	case "response.completed":
		resp, _ := evt["response"].(map[string]any)
		if resp != nil {
			if u, ok := resp["usage"].(map[string]any); ok {
				for k, v := range u {
					st.fullUsage[k] = v
				}
			}
			if status, ok := resp["status"].(string); ok && status == "incomplete" {
				st.stopReason = "max_tokens"
			}
			// 从完整 output 推导 tool_use 终止（流式 delta 可能漏 name）；同时
			// 把 reasoning item 的 encrypted_content 落到对应 thinking block 的
			// signature（请求侧 include 了 reasoning.encrypted_content）。
			if output, ok := resp["output"].([]any); ok {
				for oi, raw := range output {
					im, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					t, _ := im["type"].(string)
					switch t {
					case "function_call", "apply_patch_call", "shell_call", "tool_call":
						st.stopReason = "tool_use"
					case "reasoning":
						sig, _ := im["encrypted_content"].(string)
						if sig == "" {
							continue
						}
						if b, ok2 := st.blocks[oi]; ok2 && b.Kind == "thinking" {
							b.Signature = sig
						}
						if id, _ := im["id"].(string); id != "" {
							if oi2, ok2 := st.itemToOutput[id]; ok2 {
								if b, ok3 := st.blocks[oi2]; ok3 && b.Kind == "thinking" {
									b.Signature = sig
								}
							}
						}
					}
					if st.stopReason == "tool_use" && t != "reasoning" {
						break
					}
				}
			}
		} else if u, ok := evt["usage"].(map[string]any); ok {
			for k, v := range u {
				st.fullUsage[k] = v
			}
		}
		st.sawFinish = true
		st.finishReason = "stop"
		st.doneSeen = true
		st.finished = true
	case "response.incomplete":
		resp, _ := evt["response"].(map[string]any)
		if resp != nil {
			if u, ok := resp["usage"].(map[string]any); ok {
				for k, v := range u {
					st.fullUsage[k] = v
				}
			}
		}
		st.stopReason = "max_tokens"
		st.sawFinish = true
		st.finishReason = "length"
		st.doneSeen = true
		st.finished = true
	case "response.failed", "error":
		msg := "upstream stream error"
		if em, ok := evt["error"].(map[string]any); ok {
			if m, ok := em["message"].(string); ok && m != "" {
				msg = m
			}
		} else if em, ok := evt["response"].(map[string]any); ok {
			if e2, ok := em["error"].(map[string]any); ok {
				if m, ok := e2["message"].(string); ok && m != "" {
					msg = m
				}
			}
		}
		out = append(out, st.emitError(msg))
		st.finished = true
	default:
		// 顶层 usage 事件（部分上游直接发 usage）。
		if u, ok := evt["usage"].(map[string]any); ok {
			for k, v := range u {
				st.fullUsage[k] = v
			}
			return out
		}
		// 未知事件：尝试通用 delta 字段降级为文本，不报错。
		if d, ok := evt["delta"].(string); ok && d != "" {
			oi := outputIndex
			if oi < 0 {
				oi = outputIndexForItem(itemID, 0)
			}
			st.chunkCount++
			b := st.getOrCreate(oi, "text")
			st.emitText(&out, b, d)
		}
	}
	return out
}

// MarkDoneSeen records that an upstream `[DONE]` sentinel was consumed (the
// shell sees the literal `[DONE]` line and reports it). When the stream already
// produced output it is sealed as finished (matching the original `[DONE]`
// branch which set finished=true without going through doFinalize's guard).
func (st *ResponsesToClaudeStream) MarkDoneSeen() {
	st.doneSeen = true
	if st.ProducedOutput() {
		st.finished = true
	}
}

// MarkEOFStop records a clean EOF that lacked response.completed (e.g. some
// upstreams send events but never DONE): the stream is sealed as a normal stop.
// The shell only calls this when the stream produced output; otherwise it emits
// the EOF error itself.
func (st *ResponsesToClaudeStream) MarkEOFStop() {
	st.sawFinish = true
	st.finishReason = "stop"
	st.finished = true
}

// EOFErrorEvent returns the `error` frame for an upstream stream that ended
// without producing any output or completion (the original's
// emitError("stream ended without completion") path).
func (st *ResponsesToClaudeStream) EOFErrorEvent() OutEvent {
	return st.emitError("stream ended without completion")
}

// Finalize idempotently closes the stream: it emits the empty-reply text
// promotion (if any), closes every open block in ascending claudeIndex order
// (emitting each thinking block's signature_delta first for the roundtrip),
// then the message_delta/message_stop trailer. The second and later calls are
// no-ops, so EOF / [DONE] / response.completed never double-emit the trailer.
//
// The returned message_stop frame carries Terminal=true so the shell knows the
// stream is fully sealed.
func (st *ResponsesToClaudeStream) Finalize() []OutEvent {
	if st.finalized {
		return nil
	}
	st.finalized = true
	var out []OutEvent

	// 下一个可用 claude 序号：不再用固定 9999 兜底（同流多段/大序号时可能
	// 与既有块冲突），取当前最大序号 +1。
	nextIndex := 0
	for _, b := range st.blocks {
		if b.ClaudeIndex >= nextIndex {
			nextIndex = b.ClaudeIndex + 1
		}
	}
	// 空回复保护：有 reasoning 但无文本/tool 时提升为文本。
	reasoningFallback := st.reasoningFallback.String()
	if !st.producedText && len(st.toolOrder) == 0 && reasoningFallback != "" {
		b := &ClaudeResponsesBlock{ClaudeIndex: nextIndex, Kind: "text", Open: false}
		// 复用 emit 路径：直接发送一个文本块。
		out = append(out, st.emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": b.ClaudeIndex,
			"content_block": map[string]any{"type": "text", "text": ""},
		}))
		out = append(out, st.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": b.ClaudeIndex,
			"delta": map[string]any{"type": "text_delta", "text": reasoningFallback},
		}))
		out = append(out, st.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": b.ClaudeIndex}))
	}
	// 按 claudeIndex 排序关块，保证序号单调。
	type kv struct {
		oi int
		b  *ClaudeResponsesBlock
	}
	var ordered []kv
	for oi, b := range st.blocks {
		if b.Open && b.ClaudeIndex >= 0 {
			ordered = append(ordered, kv{oi, b})
		}
	}
	// 简单冒泡按 index 排序（块数极少）。
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if ordered[j].b.ClaudeIndex < ordered[i].b.ClaudeIndex {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}
	}
	for _, e := range ordered {
		// thinking block 带 encrypted_content 时先补 signature_delta 再关
		// 块（对齐 sub2api）：Claude Code 下一轮可把 signature 原样带回。
		if e.b.Kind == "thinking" && e.b.Signature != "" {
			out = append(out, st.emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": e.b.ClaudeIndex,
				"delta": map[string]any{"type": "signature_delta", "signature": e.b.Signature},
			}))
		}
		out = append(out, st.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": e.b.ClaudeIndex}))
	}
	// 占位块（claudeIndex==-1，从未真正开块）无需关闭。
	out = append(out, st.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": st.stopReason, "stop_sequence": nil},
		"usage": BuildClaudeDeltaUsage(ResponsesUsageToChat(st.fullUsage)),
	}))
	stop := st.emit("message_stop", map[string]any{"type": "message_stop"})
	stop.Terminal = true
	out = append(out, stop)
	return out
}

// UsageFromResponsesMap extracts (prompt, completion, total) from a Responses
// usage map, synthesizing total from the components when absent — the same
// fallback rule ResponsesUsageToChat uses. It parses without statsx.FromMap so
// the bridge stays free of the internal/stats dependency: input_tokens and
// prompt_tokens are both accepted for the prompt side, output_tokens and
// completion_tokens for the completion side, and total_tokens for the total
// (matching statsx.TokenUsage.FromMap's preferred-key order and number
// coercion).
func UsageFromResponsesMap(usage map[string]any) (int64, int64, int64) {
	pt := firstResponsesUsageToken(usage, "input_tokens", "prompt_tokens")
	ct := firstResponsesUsageToken(usage, "output_tokens", "completion_tokens")
	tt := firstResponsesUsageToken(usage, "total_tokens")
	if tt <= 0 && (pt > 0 || ct > 0) {
		tt = pt + ct
	}
	return pt, ct, tt
}

// firstResponsesUsageToken returns the first present key's value as int64, or 0
// when none of the keys hold a number (coerced via NumberAsFloat so
// integer-valued JSON numbers are not dropped).
func firstResponsesUsageToken(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := NumberAsFloat(m[k]); ok {
			return int64(v)
		}
	}
	return 0
}
