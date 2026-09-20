package bridge

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// anthropic_to_responses_stream.go holds the pure Anthropic Messages SSE →
// Responses SSE state machine (app state machine #3), migrated out of
// internal/app/responses_to_anthropic.go. Every frame the old writer pushed
// onto the http.ResponseWriter is now returned as an OutEvent whose Raw bytes
// are the exact wire bytes; stamping (`created`), ID generation, and the
// sequence counter are injected/held by the bridge so the conversion stays pure
// and deterministic under test.
//
// Unlike the Chat state machines (which only emit `data: ...\n\n` frames), this
// machine emits *named* Responses SSE frames: `event: <name>\ndata: <json>\n\n`.
// The OutEvent.Raw bytes carry the full named frame verbatim; the terminal
// `[DONE]` sentinel is a bare `data: [DONE]\n\n` frame with no `event:` line,
// exactly as the old writer produced. The shell is a mechanical write+flush
// loop.
//
// The state transitions, usage merging, and fallback rules are preserved
// verbatim. See the red-line invariants in the unit brief:
//
//   - ensureTerminal is idempotent (terminalSent); EOF and the protocol's
//     message_stop / error / duplicate-start paths all funnel through it.
//   - EOF closes still-open blocks in ascending output_index order, backfilling
//     each into completedOutput before emitting the terminal completed/
//     incomplete response (so a client rebuilding output from the terminal
//     event never loses trailing text/thinking/tool_use).
//   - A tool_use block whose initialArguments is non-empty but which never saw
//     input_json_delta gets ONE function_call_arguments.delta (with the full
//     initial JSON) before done; empty initialArguments collapses to "{}".
//   - A duplicate content_block_start for an index already open, or an upstream
//     error event, emits response.failed + [DONE] and seals the stream
//     (terminal) WITHOUT touching the finish/done counters — those are only set
//     by the completed/incomplete path through ensureTerminal.
//   - message_stop maps a max_tokens / max_tokens_cap finish to
//     response.incomplete (with incomplete_details.reason = max_output_tokens),
//     otherwise response.completed.
//   - sequence_number is held by the bridge and strictly monotonic: emitEvent
//     pre-increments so every event frame carries a fresh, increasing value.

// ResponsesBlockState records all metadata for one open content_block.
// text/thinking share the text builder, kind distinguishes the type; tool_use
// needs no builder (its arguments already streamed out via initialArguments +
// input_json_delta). Field names are exported so the bridge package owns the
// full state; the app shell never reads them.
type ResponsesBlockState struct {
	Kind        string // "text" | "thinking" | "tool_use"
	ItemID      string
	OutputIndex int
	ToolIdx     int              // tool_use only: per-turn tool ordinal
	CallID      string           // tool_use only: Anthropic tool_use id
	Name        string           // tool_use only
	Text        *strings.Builder // Kind=text → output_text; Kind=thinking → reasoning summary
	// InitialArguments caches the content_block_start-attached input. If no
	// input_json_delta arrived by content_block_stop, closeBlock first emits one
	// argument.delta carrying the full input JSON, then done (otherwise the item
	// would be left with only an in_progress add and a parameter-less done).
	InitialArguments string
	ArgDeltaSeen     bool
}

// AnthropicToResponsesState converts an upstream Anthropic Messages SSE stream
// into a Responses SSE stream. The event sequence mirrors what
// responsesStreamHandler emits downstream. It implements Streamer.
type AnthropicToResponsesState struct {
	// hexGen mints the block item ids (rs_/msg_/fc_ prefixes); production wires
	// it to internal/random hex. The resp_ response id also derives from it.
	hexGen HexIDGen
	// nowFn stamps each response object's `created` field; production is
	// time.Now().Unix.
	nowFn NowFn

	id            string
	model         string
	wantReasoning bool
	seq           int
	outputIndex   int
	fullUsage     map[string]any
	// blocks tracks the open content_blocks by anthropic block index — one
	// struct per block (no parallel maps, so cleanup cannot drop one).
	blocks    map[int]*ResponsesBlockState
	toolCount int
	// signatureDeltas only counts (produces no downstream event): Responses has
	// no concept for Anthropic signature deltas and a strict downstream would
	// reject the unknown delta field, so they are dropped but tallied.
	signatureDeltas int
	// completedOutput aggregates finished output items; written into the
	// terminal response.completed / response.incomplete payload.
	completedOutput []any
	// terminalSent is the idempotent terminal switch, shared by every terminal
	// path (ensureTerminal's completed/incomplete and the duplicate-start /
	// error response.failed fallbacks). ensureTerminal checks it, so any of
	// those paths seals the stream against a later EOF Finalize.
	terminalSent bool
	// Counters mirrored into logging.StreamStats by the app shell. noteFinish
	// (finishReason/sawFinish/doneSeen) is only set by the completed/incomplete
	// path via ensureTerminal — the duplicate-start / error response.failed
	// paths seal the stream without touching them, matching the original.
	textChars      int
	reasoningChars int
	finishReason   string
	sawFinish      bool
	doneSeen       bool
}

// NewAnthropicToResponsesState builds the state machine for one upstream
// stream. model is the fallback response model until message_start supplies the
// real one. hexGen/nowFn inject randomness and time so the conversion stays
// pure and deterministic under test.
func NewAnthropicToResponsesState(hexGen HexIDGen, nowFn NowFn, model string, wantReasoning bool) *AnthropicToResponsesState {
	if hexGen == nil {
		hexGen = DefaultHexIDGen
	}
	if nowFn == nil {
		nowFn = DefaultNowFn
	}
	return &AnthropicToResponsesState{
		hexGen:        hexGen,
		nowFn:         nowFn,
		id:            "resp_" + hexGen("resp_", 16),
		model:         model,
		wantReasoning: wantReasoning,
		blocks:        map[int]*ResponsesBlockState{},
		fullUsage:     map[string]any{},
	}
}

// Usage returns the accumulated, merged upstream Anthropic usage map (the full,
// raw protocol-side shape). The app shell converts it (responsesUsageToChat)
// and records it via statsx.
func (st *AnthropicToResponsesState) Usage() map[string]any { return st.fullUsage }

// Counters returns a read-only snapshot of the stream accounting.
func (st *AnthropicToResponsesState) Counters() StreamCounters {
	return StreamCounters{
		TextChars:         st.textChars,
		ReasoningChars:    st.reasoningChars,
		ToolCallCount:     st.toolCount,
		SkippedSignatures: st.signatureDeltas,
		FinishReason:      st.finishReason,
		SawFinish:         st.sawFinish,
		DoneSeen:          st.doneSeen,
	}
}

// Terminal reports whether the terminal frame (response.completed / incomplete
// / failed + [DONE]) has been emitted. The app shell stops consuming upstream
// lines once Handle reports the stream sealed.
func (st *AnthropicToResponsesState) Terminal() bool { return st.terminalSent }

// Finalize idempotently ends the Responses stream with the completed terminal
// event. The app shell calls it on the EOF/read-error fallback; the normal
// message_stop and error paths reach the same code through ensureTerminal
// inside Handle. The second and later calls are no-ops.
func (st *AnthropicToResponsesState) Finalize() []OutEvent {
	return st.ensureTerminal("completed", "response.completed")
}

// emitEvent builds one named Responses SSE frame, auto-incrementing
// sequence_number (strictly monotonic: pre-incremented per emitted event).
// Returns the zero OutEvent when the payload fails to marshal (kept from the
// original, which silently dropped the frame); the caller's emit helper skips
// zero events.
func (st *AnthropicToResponsesState) emitEvent(event string, data map[string]any) OutEvent {
	st.seq++
	data["type"] = event
	data["sequence_number"] = st.seq
	b, err := json.Marshal(data)
	if err != nil {
		return OutEvent{}
	}
	return OutEvent{Event: event, Data: data, Raw: []byte("event: " + event + "\ndata: " + string(b) + "\n\n")}
}

// doneEvent builds the terminal `[DONE]` sentinel frame: a bare `data:
// [DONE]\n\n` frame (no `event:` line), marked Terminal.
func doneEvent() OutEvent {
	return OutEvent{Raw: []byte("data: [DONE]\n\n"), Terminal: true}
}

// ensureTerminal idempotently emits the terminal event (completed / incomplete
// / failed). An upstream EOF may leave content_blocks still open: every one is
// closed in ascending output_index order (each emitting its done events and
// backfilling completedOutput), otherwise a client that rebuilds output from
// the terminal event would lose trailing text/thinking/tool_use content, or be
// left with a permanently in_progress item (added sent, done never arrived).
func (st *AnthropicToResponsesState) ensureTerminal(status, event string) []OutEvent {
	if st.terminalSent {
		return nil
	}
	st.terminalSent = true
	var out []OutEvent
	blocks := make([]*ResponsesBlockState, 0, len(st.blocks))
	for _, b := range st.blocks {
		blocks = append(blocks, b)
	}
	// Sort by output_index (i.e. start arrival order) ascending, so map
	// iteration order cannot scramble completedOutput ordering.
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].OutputIndex < blocks[j].OutputIndex })
	for _, b := range blocks {
		out = append(out, st.closeBlock(b)...)
	}
	clear(st.blocks)
	response := map[string]any{
		"id":      st.id,
		"object":  "response",
		"created": st.nowFn(),
		"status":  status,
		"model":   st.model,
		"output":  st.completedOutput,
		"usage":   st.fullUsage,
	}
	if status == "incomplete" {
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	out = append(out, st.emitEvent(event, map[string]any{"response": response}))
	out = append(out, doneEvent())
	st.doneSeen = true
	st.sawFinish = true
	return out
}

// closeBlock closes a single content_block: it emits the done events and
// backfills the item into completedOutput. The normal content_block_stop path
// and ensureTerminal (the EOF fallback) share it, so the two paths produce
// identical event shapes and never drift.
func (st *AnthropicToResponsesState) closeBlock(b *ResponsesBlockState) []OutEvent {
	var out []OutEvent
	switch b.Kind {
	case "thinking":
		st.completedOutput = append(st.completedOutput, map[string]any{
			"type": "reasoning", "id": b.ItemID, "summary": []any{},
		})
		out = append(out, st.emitEvent("response.output_item.done", map[string]any{
			"output_index": b.OutputIndex,
			"item": map[string]any{
				"type": "reasoning", "id": b.ItemID, "summary": []any{},
			},
		}))
	case "text":
		text := ""
		if b.Text != nil {
			text = b.Text.String()
		}
		st.completedOutput = append(st.completedOutput, map[string]any{
			"type": "message", "id": b.ItemID, "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text}},
		})
		out = append(out, st.emitEvent("response.output_text.done", map[string]any{
			"item_id": b.ItemID, "text": text,
		}))
		out = append(out, st.emitEvent("response.output_item.done", map[string]any{
			"output_index": b.OutputIndex,
			"item": map[string]any{
				"type": "message", "id": b.ItemID, "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": text}},
			},
		}))
	case "tool_use":
		// The arguments.done / output_item.done events here use b.OutputIndex
		// (not b.ToolIdx — the latter is the per-turn tool ordinal and would
		// misalign output_index against the actual added event's index). The
		// item is backfilled with the full {id, call_id, name, arguments,
		// status:completed, type:function_call}.
		if b.InitialArguments != "" && !b.ArgDeltaSeen {
			out = append(out, st.emitEvent("response.function_call_arguments.delta", map[string]any{
				"item_id": b.ItemID, "output_index": b.OutputIndex, "delta": b.InitialArguments,
			}))
		}
		arguments := b.InitialArguments
		if arguments == "" {
			arguments = "{}"
		}
		item := map[string]any{
			"type": "function_call", "id": b.ItemID, "call_id": b.CallID,
			"name": b.Name, "arguments": arguments, "status": "completed",
		}
		st.completedOutput = append(st.completedOutput, item)
		out = append(out, st.emitEvent("response.function_call_arguments.done", map[string]any{
			"item_id": b.ItemID, "output_index": b.OutputIndex, "arguments": arguments,
		}))
		out = append(out, st.emitEvent("response.output_item.done", map[string]any{
			"output_index": b.OutputIndex,
			"item":         item,
		}))
	}
	return out
}

// Handle consumes one upstream Anthropic SSE line and returns the frames to
// forward downstream, in order. Once a terminal event (response.failed /
// completed + [DONE], or the duplicate-start fallback) has been emitted, every
// later line is ignored: nothing more may be written to the client and the
// block state is no longer consulted.
func (st *AnthropicToResponsesState) Handle(ev StreamEvent) []OutEvent {
	if st.terminalSent {
		return nil
	}
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
			if m, _ := msg["model"].(string); m != "" {
				st.model = m
			}
			if u, ok := msg["usage"].(map[string]any); ok {
				MergeUsage(st.fullUsage, u)
			}
		}
		emit(st.emitEvent("response.created", map[string]any{
			"response": map[string]any{
				"id": st.id, "object": "response", "created": st.nowFn(),
				"status": "in_progress", "model": st.model, "output": []any{},
			},
		}))
	case "content_block_start":
		idx := NumberToInt(evt["index"])
		cb, _ := evt["content_block"].(map[string]any)
		bt, _ := cb["type"].(string)
		// If the upstream repeats a start on the same idx with no intervening
		// stop, the old mapping would be overwritten and produce a never-closed
		// output_index — reject explicitly so the upstream error is more visible.
		if _, dup := st.blocks[idx]; dup {
			emit(st.emitEvent("response.failed", map[string]any{
				"response": map[string]any{
					"id": st.id, "status": "failed",
					"error": map[string]any{"message": fmt.Sprintf("duplicate content_block_start for index %d", idx)},
				},
			}))
			emit(doneEvent())
			st.terminalSent = true
			return out
		}
		// Allocate and record output_index at start, so the index backfilled at
		// stop is the same one.
		b := &ResponsesBlockState{Kind: bt, OutputIndex: st.outputIndex}
		st.blocks[idx] = b
		st.outputIndex++
		switch bt {
		case "thinking":
			b.ItemID = "rs_" + st.hexGen("rs_", 12)
			b.Text = &strings.Builder{}
			emit(st.emitEvent("response.output_item.added", map[string]any{
				"output_index": b.OutputIndex,
				"item": map[string]any{
					"type": "reasoning", "id": b.ItemID, "summary": []any{},
				},
			}))
		case "text":
			b.ItemID = "msg_" + st.hexGen("msg_", 12)
			b.Text = &strings.Builder{}
			emit(st.emitEvent("response.output_item.added", map[string]any{
				"output_index": b.OutputIndex,
				"item": map[string]any{
					"type": "message", "id": b.ItemID, "role": "assistant",
					"status": "in_progress", "content": []any{},
				},
			}))
			emit(st.emitEvent("response.content_part.added", map[string]any{
				"item_id": b.ItemID, "output_index": b.OutputIndex,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": ""},
			}))
		case "tool_use":
			b.ItemID = "fc_" + st.hexGen("fc_", 12)
			b.ToolIdx = st.toolCount
			st.toolCount++
			callID, _ := cb["id"].(string)
			name, _ := cb["name"].(string)
			b.CallID = callID
			b.Name = name
			if inp, ok := cb["input"]; ok && inp != nil {
				if raw, err := json.Marshal(inp); err == nil {
					b.InitialArguments = string(raw)
				}
			}
			emit(st.emitEvent("response.output_item.added", map[string]any{
				"output_index": b.OutputIndex,
				"item": map[string]any{
					"type": "function_call", "id": b.ItemID, "call_id": callID,
					"name": name, "arguments": "", "status": "in_progress",
				},
			}))
		}
	case "content_block_delta":
		idx := NumberToInt(evt["index"])
		b := st.blocks[idx]
		if b == nil {
			return out
		}
		d, _ := evt["delta"].(map[string]any)
		dt, _ := d["type"].(string)
		switch dt {
		case "text_delta":
			if t, _ := d["text"].(string); t != "" {
				if b.Text != nil {
					b.Text.WriteString(t)
				}
				st.textChars += len(t)
				emit(st.emitEvent("response.output_text.delta", map[string]any{
					"item_id": b.ItemID, "delta": t, "logprobs": []any{},
				}))
			}
		case "thinking_delta":
			if st.wantReasoning {
				if t, _ := d["thinking"].(string); t != "" {
					if b.Text != nil {
						b.Text.WriteString(t)
					}
					st.reasoningChars += len(t)
					emit(st.emitEvent("response.reasoning_summary_text.delta", map[string]any{
						"item_id": b.ItemID, "delta": t,
					}))
				}
			}
		case "input_json_delta":
			if pj, _ := d["partial_json"].(string); pj != "" {
				b.ArgDeltaSeen = true
				emit(st.emitEvent("response.function_call_arguments.delta", map[string]any{
					"item_id": b.ItemID, "output_index": b.OutputIndex, "delta": pj,
				}))
			}
		case "signature_delta":
			// Anthropic thinking/redacted_thinking signature_delta: count only,
			// emit nothing downstream (Responses has no concept for it; a strict
			// downstream would reject the unknown delta field).
			st.signatureDeltas++
		}
	case "content_block_stop":
		idx := NumberToInt(evt["index"])
		b := st.blocks[idx]
		if b == nil {
			return out
		}
		out = append(out, st.closeBlock(b)...)
		delete(st.blocks, idx)
	case "message_delta":
		if u, ok := evt["usage"].(map[string]any); ok {
			MergeUsage(st.fullUsage, u)
		}
		if delta, ok := evt["delta"].(map[string]any); ok {
			if sr, _ := delta["stop_reason"].(string); sr != "" {
				st.finishReason = sr
				st.sawFinish = true
			}
		}
	case "message_stop":
		if st.finishReason == "max_tokens" || st.finishReason == "max_tokens_cap" {
			out = append(out, st.ensureTerminal("incomplete", "response.incomplete")...)
		} else {
			out = append(out, st.ensureTerminal("completed", "response.completed")...)
		}
	case "error":
		em, _ := evt["error"].(map[string]any)
		message := "upstream error"
		if m, _ := em["message"].(string); m != "" {
			message = m
		}
		emit(st.emitEvent("response.failed", map[string]any{
			"response": map[string]any{
				"id": st.id, "status": "failed",
				"error": map[string]any{"message": message},
			},
		}))
		emit(doneEvent())
		st.terminalSent = true
	}
	return out
}
