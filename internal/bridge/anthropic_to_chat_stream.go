package bridge

import (
	"encoding/json"
	"strings"
)

// anthropic_to_chat_stream.go holds the pure Anthropic Messages SSE → Chat
// Completions SSE state machine (app state machine #1), migrated out of
// internal/app/chat_to_anthropic.go. Every frame the old writer pushed onto
// the http.ResponseWriter is now returned as an OutEvent whose Raw bytes are
// the exact `data: ...\n\n` payload; stamping (`created`) and ID generation are
// injected so the conversion is deterministic and I/O-free.
//
// The state transitions, usage merging, and fallback rules are preserved
// verbatim. See the red-line invariants in the unit brief:
//
//   - Finalize is idempotent (finalized switch shared by message_stop and EOF).
//   - EOF supplies the terminal finish+usage+[DONE] block, never twice.
//   - A tool_use block that never saw input_json_delta gets content_block_stop
//     backfilled with initialInput or `{}`.
//   - reasoning_tokens ≈ thinking chars/4 is only a fallback for a missing
//     upstream thinking_tokens.
//   - finish_reason maps Anthropic stop reasons onto the Chat closed set.

// AnthropicToolState tracks one open tool_use block. InitialInput caches the
// content_block_start-attached initial input; SawInputDelta records whether any
// input_json_delta arrived. The two are mutually exclusive consumers: on stop,
// if SawInputDelta is false and InitialInput is non-empty, one fallback frame is
// emitted, otherwise that tool call's input would be silently lost (an
// occasional upstream behavior).
type AnthropicToolState struct {
	SawInputDelta bool
	InitialInput  any // string or map[string]any; empty/nil means none
}

// InitialArguments returns the initial arguments string to hand the Chat side,
// consumed on content_block_stop.
func (t *AnthropicToolState) InitialArguments() string {
	switch v := t.InitialInput.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
		return ""
	}
}

// AnthropicToChatState converts an upstream Anthropic SSE stream into Chat
// Completions frames. It implements Streamer.
type AnthropicToChatState struct {
	// idGen stamps the chat completion id once at construction ("chatcmpl-" +
	// idGen(prefix, 12)); production wires it to internal/random hex.
	idGen HexIDGen
	// nowFn stamps each chunk's `created` field; production is time.Now().Unix.
	nowFn NowFn

	id            string
	model         string
	keepReasoning bool
	includeUsage  bool
	sentRole      bool
	blocks        map[int]string // anthropic block index → "text"|"thinking"|"tool_use"
	toolIndices   map[int]int    // anthropic block index → chat tool_calls index
	toolStates    map[int]*AnthropicToolState
	toolCount     int
	stopReason    string
	fullUsage     map[string]any
	// reasoningTokens is a rough count of thinking_delta characters (tokens ≈
	// chars/4), used only as a completion_tokens_details.reasoning_tokens
	// fallback when the upstream Anthropic usage carries no
	// output_tokens_details.thinking_tokens (the exact value wins).
	reasoningTokens int
	skippedSig      int
	skippedRedacted int
	sawFinish       bool
	doneSeen        bool
	finalized       bool // idempotent Finalize switch: message_stop and EOF share it
}

// NewAnthropicToChatState builds the state machine for one upstream stream.
// model is the fallback chat model id until message_start supplies the real one.
// idGen/nowFn inject randomness and time so the conversion stays pure and
// deterministic under test.
func NewAnthropicToChatState(idGen HexIDGen, nowFn NowFn, model string, keepReasoning, includeUsage bool) *AnthropicToChatState {
	if idGen == nil {
		idGen = DefaultHexIDGen
	}
	if nowFn == nil {
		nowFn = DefaultNowFn
	}
	return &AnthropicToChatState{
		idGen:         idGen,
		nowFn:         nowFn,
		id:            "chatcmpl-" + idGen("chatcmpl-", 12),
		model:         model,
		keepReasoning: keepReasoning,
		includeUsage:  includeUsage,
		blocks:        map[int]string{},
		toolIndices:   map[int]int{},
		toolStates:    map[int]*AnthropicToolState{},
		fullUsage:     map[string]any{},
	}
}

// Usage returns the accumulated, merged upstream Anthropic usage map. The app
// shell converts it (AnthropicUsageToChat) and records it.
func (st *AnthropicToChatState) Usage() map[string]any { return st.fullUsage }

// Counters returns a read-only snapshot of the stream accounting.
func (st *AnthropicToChatState) Counters() StreamCounters {
	return StreamCounters{
		ToolCallCount:     st.toolCount,
		SkippedSignatures: st.skippedSig,
		SkippedRedacted:   st.skippedRedacted,
		FinishReason:      st.stopReason,
		SawFinish:         st.sawFinish,
		DoneSeen:          st.doneSeen,
	}
}

// Finalize idempotently ends the Chat stream: emit the finish chunk (defaulting
// stopReason to "stop"), the usage terminal block when includeUsage and usage
// is present, and [DONE]. Both the message_stop path and the EOF fallback share
// this; the second and later calls are no-ops.
func (st *AnthropicToChatState) Finalize() []OutEvent {
	if st.finalized {
		return nil
	}
	st.finalized = true
	if st.stopReason == "" {
		st.stopReason = "stop"
	}
	out := []OutEvent{st.emitChunk(map[string]any{}, st.stopReason, nil)}
	if st.includeUsage && len(st.fullUsage) > 0 {
		out = append(out, st.emitChunk(map[string]any{}, "", st.chatUsage()))
	}
	out = append(out, OutEvent{Raw: []byte("data: [DONE]\n\n"), Terminal: true})
	st.doneSeen = true
	st.sawFinish = true
	return out
}

// chatUsage returns the usage to send the chat client. When the Anthropic side
// did not carry output_tokens_details.thinking_tokens, fall back to the
// thinking_delta character count to estimate reasoning_tokens (roughly
// tokens ≈ chars/4; only enabled when no exact value exists).
func (st *AnthropicToChatState) chatUsage() map[string]any {
	usage := AnthropicUsageToChat(st.fullUsage)
	if usage == nil {
		return nil
	}
	if st.reasoningTokens > 0 {
		details, _ := usage["completion_tokens_details"].(map[string]any)
		if details == nil {
			details = map[string]any{}
		}
		if v, ok := NumberAsFloat(details["reasoning_tokens"]); !ok || v <= 0 {
			est := st.reasoningTokens / 4
			if est < 1 {
				est = 1
			}
			details["reasoning_tokens"] = est
		}
		if len(details) > 0 {
			usage["completion_tokens_details"] = details
		}
	}
	return usage
}

// emitChunk builds one Chat SSE chunk frame. delta nil becomes {}; finishReason
// non-empty attaches finish_reason; usage non-nil attaches usage (terminal block
// only). Returns nil if the chunk fails to marshal (kept from the original
// behavior, which silently dropped the frame).
func (st *AnthropicToChatState) emitChunk(delta map[string]any, finishReason string, usage map[string]any) OutEvent {
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

// Handle consumes one upstream Anthropic SSE line and returns the frames to
// forward downstream.
func (st *AnthropicToChatState) Handle(ev StreamEvent) []OutEvent {
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
	case "message_start":
		if msg, ok := evt["message"].(map[string]any); ok {
			if u, ok := msg["usage"].(map[string]any); ok {
				MergeUsage(st.fullUsage, u)
			}
			if m, _ := msg["model"].(string); m != "" {
				st.model = m
			}
		}
		if !st.sentRole {
			st.sentRole = true
			emit(st.emitChunk(map[string]any{"role": "assistant", "content": ""}, "", nil))
		}
	case "content_block_start":
		idx := NumberToInt(evt["index"])
		cb, _ := evt["content_block"].(map[string]any)
		bt, _ := cb["type"].(string)
		st.blocks[idx] = bt
		if bt == "text" {
			// A start event that directly carries an initial non-empty text gets a
			// pre-emitted text_delta; otherwise that starting content is lost (the
			// upstream normally follows with a text_delta, but a content_block_start
			// carrying full text is valid).
			if t, _ := cb["text"].(string); t != "" {
				emit(st.emitChunk(map[string]any{"content": t}, "", nil))
			}
		}
		if bt == "tool_use" {
			toolIdx := st.toolCount
			st.toolCount++
			st.toolIndices[idx] = toolIdx
			tool := &AnthropicToolState{}
			st.toolStates[idx] = tool
			name, _ := cb["name"].(string)
			id, _ := cb["id"].(string)
			// Cache the start block's initial input (commonly {}); do NOT emit it to
			// the chat side yet — an OpenAI client concatenates all arguments
			// fragments, so if start delivered an initial value the later partial_json
			// would assemble invalid JSON. Backfill on stop: when !sawInputDelta
			// prefer the initial value, else "{}".
			if raw, ok := cb["input"]; ok && raw != nil {
				if s, ok := raw.(string); ok && s != "" && s != "{}" {
					tool.InitialInput = s
				} else if m, ok := raw.(map[string]any); ok && len(m) > 0 {
					tool.InitialInput = m
				}
			}
			emit(st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": toolIdx, "id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": ""},
			}}}, "", nil))
		}
	case "content_block_delta":
		idx := NumberToInt(evt["index"])
		d, _ := evt["delta"].(map[string]any)
		dt, _ := d["type"].(string)
		switch dt {
		case "text_delta":
			if t, _ := d["text"].(string); t != "" {
				emit(st.emitChunk(map[string]any{"content": t}, "", nil))
			}
		case "thinking_delta":
			if t, _ := d["thinking"].(string); t != "" {
				// Roughly count reasoning tokens (chars/4), only used as the
				// completion_tokens_details fallback when upstream usage lacks
				// thinking_tokens.
				st.reasoningTokens += len(t)
				if st.keepReasoning {
					emit(st.emitChunk(map[string]any{"reasoning_content": t}, "", nil))
				}
			}
		case "input_json_delta":
			if toolIdx, ok := st.toolIndices[idx]; ok {
				if tool, ok := st.toolStates[idx]; ok {
					tool.SawInputDelta = true
				}
				if pj, _ := d["partial_json"].(string); pj != "" {
					emit(st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
						"index": toolIdx, "id": nil, "type": "function",
						"function": map[string]any{"name": "", "arguments": pj},
					}}}, "", nil))
				}
			}
		case "signature_delta":
			// OpenAI chat has no corresponding field (signatures are only meaningful
			// for same-protocol roundtrips); explicitly skip and count, never folded
			// into reasoning_content.
			st.skippedSig++
		case "redacted_thinking":
			st.skippedRedacted++
		}
	case "content_block_stop":
		idx := NumberToInt(evt["index"])
		// If a tool_use never received an input_json_delta, backfill once on stop:
		// the initial input when present, else "{}". The OpenAI client concatenates
		// the per-chunk arguments and must end with valid JSON — an empty string
		// would fail json.Unmarshal; "{}" matches buildOpenAIResponse's non-stream
		// semantics for nil/{} input.
		if tool, ok := st.toolStates[idx]; ok && !tool.SawInputDelta {
			if toolIdx, ok := st.toolIndices[idx]; ok {
				s := tool.InitialArguments()
				if s == "" {
					s = "{}"
				}
				emit(st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
					"index": toolIdx, "id": nil, "type": "function",
					"function": map[string]any{"name": "", "arguments": s},
				}}}, "", nil))
			}
		}
		delete(st.blocks, idx)
		delete(st.toolIndices, idx)
		delete(st.toolStates, idx)
	case "message_delta":
		if delta, ok := evt["delta"].(map[string]any); ok {
			if sr, _ := delta["stop_reason"].(string); sr != "" {
				st.stopReason = NormalizeFinishReason(sr)
			}
		}
		if u, ok := evt["usage"].(map[string]any); ok {
			MergeUsage(st.fullUsage, u)
		}
	case "message_stop":
		// The normal terminal path and the EOF fallback share Finalize (idempotent).
		out = append(out, st.Finalize()...)
	case "error":
		em, _ := evt["error"].(map[string]any)
		msg := "upstream error"
		if m, _ := em["message"].(string); m != "" {
			msg = m
		}
		emit(OutEvent{Raw: []byte("data: " + `{"error":{"message":` + JSONString(msg) + `}}` + "\n\n")})
	}
	return out
}
