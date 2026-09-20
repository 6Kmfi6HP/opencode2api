package bridge

import (
	"encoding/json"
	"strings"
)

// chat_to_claude_stream.go holds the pure Chat Completions SSE → Claude
// Messages SSE state machine (app state machine migrated out of
// internal/app/claude.go's claudeStreamHandler). Every frame the old writer
// pushed onto the http.ResponseWriter is now returned as an OutEvent whose Raw
// bytes are the exact `event: <name>\ndata: <json>\n\n` payload the old
// writeSSEEvent produced. ID generation (msg_/toolu_ bodies) is injected so the
// conversion is deterministic and I/O-free.
//
// The state transitions, usage merging, and fallback rules are preserved
// verbatim. See the red-line invariants in the unit brief:
//
//   - EOF / [DONE] semantics: when usageTerminalSeen (a trailing usage-only
//     chunk carried completion tokens) and the stream produced output, the
//     stream is sealed as a normal stop (message_delta stop_reason=end_turn +
//     message_stop), NOT an error. A usage-only/[DONE]-only stream with no
//     produced output emits an `error` frame and no message_delta/message_stop.
//   - finish mapping: stop→end_turn, length→max_tokens, tool_calls/
//     function_call→tool_use, content_filter→refusal.
//   - promoteMisplacedReasoning: when !KeepReasoning, upstream reasoning_content
//     is promoted to a visible text delta (counted via PromotedReasoning) instead
//     of a thinking delta.
//   - emitEmptyTextFallback: when keepReasoning produced no text/tool_use, the
//     accumulated reasoning fallback is emitted as one text delta so the client
//     never gets an empty message.
//   - finalize is idempotent for the content-block half (finalizeContentBlocks
//     runs once, at the first finish/[DONE]/EOF); the message_delta/message_stop
//     trailer is emitted exactly once by the app's post-loop.
//
// The error/EOF/[DONE] "give up" paths emit ONLY an `error` event and mark the
// stream sealed; the app shell then stops and never reaches the post-loop
// trailer, so no message_delta/message_stop follows an error.

// chatToClaudeToolState tracks one open tool_use block reconstructed from the
// upstream chat tool_calls deltas.
type chatToClaudeToolState struct {
	id   string
	name string
	args string
}

// ChatToClaudeStream converts an upstream Chat Completions SSE stream into a
// Claude Messages SSE stream. It holds the full stream state and emits named
// `event:`/`data:` frames as OutEvent values. It is driven by the app shell's
// read/keepalive loop; all side effects (writing, flushing, stats) live there.
type ChatToClaudeStream struct {
	// idGen mints the message id (msg_ + idGen(24)) and any missing tool_use id
	// (toolu_ + idGen(12)); production wires it to internal/random string.
	idGen IDGen

	msgID         string
	model         string
	blockIndex    int
	keepReasoning bool

	thinkingBlockOpen bool
	textBlockOpen     bool
	// toolCallAccumulator / toolBlockIndices / toolCallOrder reconstruct the
	// per-index tool_use blocks. toolCallOrder preserves first-seen order so
	// content_block_stop frames go out in declaration order.
	toolCallAccumulator map[int]*chatToClaudeToolState
	toolBlockIndices    map[int]int
	toolCallOrder       []int
	messageStartSent    bool
	finished            bool
	stopReason          string
	// usageTerminalSeen records that a trailing usage-only chunk carried
	// completion tokens (the OpenAI stream_options.include_usage terminal cue).
	usageTerminalSeen bool
	fullUsage         map[string]any
	// reasoningFallback accumulates reasoning when keepReasoning is on so the
	// stream can fall back to a text block if it never produces content or
	// tool_use (#37635).
	reasoningFallback strings.Builder

	// hooks into the app's StreamStats assembly, kept as pure counters here.
	textChars         int
	reasoningChars    int
	promotedReasoning bool
	sawFinish         bool
	finishReason      string
	doneSeen          bool
	// chunkCount counts upstream chunks that carried at least one choice — the
	// exact points at which the original handler called stats.NoteChunk. The app
	// shell diffs this counter to call stats.NoteChunk with identical timing.
	chunkCount int
	// trailerSent gates Finalize so the message_delta/message_stop trailer is
	// emitted at most once.
	trailerSent bool
	// errored records that an `error` frame was emitted. The original handler
	// returns immediately on any error path (in-band error, malformed JSON,
	// [DONE]/EOF without finish, non-EOF read error) WITHOUT the
	// message_delta/message_stop trailer — even if a finish_reason was seen
	// earlier. The shell therefore gates the trailer on st.Errored() == false.
	errored bool
}

// NewChatToClaudeStream builds the state machine for one upstream Chat
// Completions stream. model is the echoed Claude message model. idGen injects
// randomness (production: internal/random string). When idGen is nil the
// production default is used.
func NewChatToClaudeStream(idGen IDGen, model string, keepReasoning bool) *ChatToClaudeStream {
	if idGen == nil {
		idGen = DefaultIDGen
	}
	return &ChatToClaudeStream{
		idGen:               idGen,
		msgID:               "msg_" + idGen("msg_", 24),
		model:               model,
		blockIndex:          0,
		keepReasoning:       keepReasoning,
		toolCallAccumulator: map[int]*chatToClaudeToolState{},
		toolBlockIndices:    map[int]int{},
		toolCallOrder:       []int{},
		stopReason:          "end_turn",
		fullUsage:           map[string]any{},
	}
}

// Usage returns the accumulated, merged upstream chat usage map. The app shell
// records it via statsx at end of stream.
func (st *ChatToClaudeStream) Usage() map[string]any { return st.fullUsage }

// Counters returns a read-only snapshot of the stream accounting. ToolCallCount
// is the number of tool_use blocks started.
func (st *ChatToClaudeStream) Counters() StreamCounters {
	return StreamCounters{
		TextChars:         st.textChars,
		ReasoningChars:    st.reasoningChars,
		PromotedReasoning: st.promotedReasoning,
		ToolCallCount:     len(st.toolCallOrder),
		FinishReason:      st.finishReason,
		SawFinish:         st.sawFinish,
		DoneSeen:          st.doneSeen,
	}
}

// Finished reports whether a valid finish_reason (or a synthesized equivalent)
// was observed, i.e. the post-loop message_delta/message_stop trailer path was
// reached rather than an error return.
func (st *ChatToClaudeStream) Finished() bool { return st.finished }

// Errored reports whether an `error` frame was emitted. The shell suppresses
// the message_delta/message_stop trailer when an error was emitted, even if the
// stream had already seen a finish_reason (the original returned immediately on
// every error path).
func (st *ChatToClaudeStream) Errored() bool { return st.errored }

// ChunkCount reports how many upstream chunks carried at least one choice — the
// exact points at which the original handler called stats.NoteChunk. The app
// shell calls stats.NoteChunk once per increment so its Chunks/FirstChunkAt
// timing matches the old handler (which noted a chunk only on choice-bearing
// lines, not on usage-only chunks or [DONE]).
func (st *ChatToClaudeStream) ChunkCount() int { return st.chunkCount }

// emit builds one named `event: <name>\ndata: <json>\n\n` frame, exactly matching
// the old writeSSEEvent byte format. Returns the zero OutEvent when the payload
// fails to marshal (kept from the original, which silently returned without
// writing); the caller's emit helper skips zero events.
func (st *ChatToClaudeStream) emit(event string, data map[string]any) OutEvent {
	b, err := json.Marshal(data)
	if err != nil {
		return OutEvent{}
	}
	return OutEvent{Event: event, Data: data, Raw: []byte("event: " + event + "\ndata: " + string(b) + "\n\n")}
}

// emitError builds the Claude `error` frame the old emitClaudeError produced,
// and marks the stream errored so the app shell suppresses the terminal
// message_delta/message_stop trailer on the error path.
func (st *ChatToClaudeStream) emitError(msg string) OutEvent {
	st.errored = true
	return st.emit("error", map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "api_error",
			"message": msg,
		},
	})
}

func (st *ChatToClaudeStream) closeThinkingBlock(out *[]OutEvent) {
	if !st.thinkingBlockOpen {
		return
	}
	*out = append(*out, st.emit("content_block_stop", map[string]any{
		"type":          "content_block_stop",
		"index":         st.blockIndex - 1,
		"content_block": map[string]any{"type": "thinking"},
	}))
	st.thinkingBlockOpen = false
}

func (st *ChatToClaudeStream) closeTextBlock(out *[]OutEvent) {
	if !st.textBlockOpen {
		return
	}
	*out = append(*out, st.emit("content_block_stop", map[string]any{
		"type":          "content_block_stop",
		"index":         st.blockIndex - 1,
		"content_block": map[string]any{"type": "text"},
	}))
	st.textBlockOpen = false
}

// ensureMessageStart emits message_start (+ping) exactly once. usage is the
// message-level usage built from the usage accumulated so far (mirrors the
// original, which built it from fullUsage at first content).
func (st *ChatToClaudeStream) ensureMessageStart(out *[]OutEvent) {
	if st.messageStartSent {
		return
	}
	st.messageStartSent = true
	*out = append(*out, st.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            st.msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         st.model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         BuildClaudeMessageUsage(st.fullUsage),
		},
	}))
	*out = append(*out, st.emit("ping", map[string]any{"type": "ping"}))
}

func (st *ChatToClaudeStream) emitTextDelta(out *[]OutEvent, contentStr string) {
	if contentStr == "" {
		return
	}
	st.textChars += len(contentStr)
	st.closeThinkingBlock(out)
	if !st.textBlockOpen {
		*out = append(*out, st.emit("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": st.blockIndex,
			"content_block": map[string]any{
				"type": "text",
				"text": "",
			},
		}))
		st.textBlockOpen = true
		st.blockIndex++
	}
	*out = append(*out, st.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": st.blockIndex - 1,
		"delta": map[string]any{
			"type": "text_delta",
			"text": contentStr,
		},
	}))
}

// emitEmptyTextFallback emits the accumulated reasoning as a single text delta
// when the stream produced no visible text and no tool_use (#37635).
func (st *ChatToClaudeStream) emitEmptyTextFallback(out *[]OutEvent) {
	if st.textBlockOpen || len(st.toolCallOrder) > 0 {
		return
	}
	fallback := st.reasoningFallback.String()
	if fallback == "" {
		return
	}
	st.promotedReasoning = true
	st.emitTextDelta(out, fallback)
}

// finalizeContentBlocks closes any open thinking/text block, then emits
// content_block_stop for each tool_use block in declaration order (backfilling
// its accumulated input). Kept verbatim from the original closure.
func (st *ChatToClaudeStream) finalizeContentBlocks(out *[]OutEvent) {
	st.emitEmptyTextFallback(out)
	st.closeThinkingBlock(out)
	st.closeTextBlock(out)
	for _, idx := range st.toolCallOrder {
		acc := st.toolCallAccumulator[idx]
		*out = append(*out, st.emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": st.toolBlockIndices[idx],
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    acc.id,
				"name":  acc.name,
				"input": map[string]any{},
			},
		}))
	}
}

// StreamProducedOutput reports whether a stream emitted any assistant content
// or tool calls before the terminal usage chunk. Pure form of the old
// app-layer streamProducedOutput(stats, toolCalls): StreamStats.TextChars /
// ReasoningChars / toolCalls are supplied as plain ints so it never depends on
// internal/logging. Used by ChatToClaudeStream internally and re-exported for
// the app shell's thin wrapper.
func StreamProducedOutput(textChars, reasoningChars, toolCalls int) bool {
	return textChars > 0 || reasoningChars > 0 || toolCalls > 0
}

// streamProducedOutput reports whether the stream emitted any assistant content
// or tool calls before the terminal usage chunk.
func (st *ChatToClaudeStream) streamProducedOutput() bool {
	return StreamProducedOutput(st.textChars, st.reasoningChars, len(st.toolCallOrder))
}

// handleDone processes a terminal `[DONE]` line: either synthesize a normal
// stop (usage-terminal + produced output) or emit an error, never both. It
// returns the frames to write; the app loop always breaks after a [DONE] line,
// then consults st.Finished() to decide whether to emit the trailer.
func (st *ChatToClaudeStream) handleDone() []OutEvent {
	st.doneSeen = true
	if !st.finished {
		if st.usageTerminalSeen && st.streamProducedOutput() {
			st.sawFinish = true
			st.finishReason = "stop"
			st.finished = true
			var out []OutEvent
			st.finalizeContentBlocks(&out)
			return out
		}
		return []OutEvent{st.emitError("stream ended with [DONE] but no finish_reason")}
	}
	return nil
}

// HandleEOF mirrors handleDone for the io.EOF path. The error message differs
// from the [DONE] message verbatim ("stream ended without finish_reason"). Called
// by the app shell when the upstream read returns io.EOF (and the accompanying
// line, if any, did not already seal the stream). Returns nil when the stream was
// already finished (the shell then breaks and emits the trailer via Finalize);
// returns one error frame otherwise (the shell returns without a trailer).
func (st *ChatToClaudeStream) HandleEOF() []OutEvent {
	if !st.finished {
		if st.usageTerminalSeen && st.streamProducedOutput() {
			st.sawFinish = true
			st.finishReason = "stop"
			st.finished = true
			var out []OutEvent
			st.finalizeContentBlocks(&out)
			return out
		}
		return []OutEvent{st.emitError("stream ended without finish_reason")}
	}
	return nil
}

// ReadErrorEvent returns the `error` frame for a non-EOF upstream read error.
// The shell logs the underlying error itself (it owns the logger) before
// writing this frame; the error path never emits the trailer.
func (st *ChatToClaudeStream) ReadErrorEvent() OutEvent {
	return st.emitError("stream read error")
}

// Handle consumes one upstream Chat Completions SSE line (the full line
// including the `data: ` prefix, exactly as the app read loop reads it) and
// returns the Claude frames to forward downstream, in order.
//
// The returned breakLoop tells the app shell to stop reading after writing the
// frames:
//   - breakLoop=false: write frames (if any), continue reading.
//   - breakLoop=true: a terminal condition was reached — a `[DONE]`, an in-band
//     error, or malformed JSON. After writing the frames the shell inspects
//     st.Finished(): when true it breaks the loop and emits the
//     message_delta/message_stop trailer; when false an error was emitted and
//     the shell returns WITHOUT the trailer.
//
// Keepalive (PingEvent) and ctx.Done are handled entirely by the app shell and
// never flow through Handle.
func (st *ChatToClaudeStream) Handle(ev StreamEvent) (out []OutEvent, breakLoop bool) {
	line := ev.Line()
	trimmed := strings.TrimSpace(line)
	if trimmed == "data: [DONE]" || trimmed == "[DONE]" {
		return st.handleDone(), true
	}
	if !strings.HasPrefix(line, "data: ") {
		return nil, false
	}
	payload := line[6:]
	if strings.TrimSpace(payload) == "" {
		return nil, false
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return []OutEvent{st.emitError("stream received malformed JSON data")}, true
	}

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
		return []OutEvent{st.emitError(errMsg)}, true
	}

	if usage, ok := chunk["usage"].(map[string]any); ok {
		st.fullUsage = MergeUsageMaps(st.fullUsage, usage)
	}

	usageChunk, _ := chunk["usage"].(map[string]any)
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		// Usage-only trailing chunk (OpenAI stream_options.include_usage).
		if UsageHasCompletion(usageChunk) {
			st.usageTerminalSeen = true
		}
		return nil, false
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	finishReason, _ := choice["finish_reason"].(string)
	st.chunkCount++

	st.ensureMessageStart(&out)

	// After finish_reason, ignore further content deltas but keep reading so a
	// later usage-only chunk can populate fullUsage.
	if st.finished {
		return out, false
	}

	if rc, ok := delta["reasoning_content"]; ok {
		if rcStr, _ := rc.(string); rcStr != "" {
			st.reasoningChars += len(rcStr)
			if st.keepReasoning {
				st.reasoningFallback.WriteString(rcStr)
				st.closeTextBlock(&out)
				if !st.thinkingBlockOpen {
					out = append(out, st.emit("content_block_start", map[string]any{
						"type":  "content_block_start",
						"index": st.blockIndex,
						"content_block": map[string]any{
							"type":     "thinking",
							"thinking": "",
						},
					}))
					st.thinkingBlockOpen = true
					st.blockIndex++
				}
				out = append(out, st.emit("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": st.blockIndex - 1,
					"delta": map[string]any{
						"type":     "thinking_delta",
						"thinking": rcStr,
					},
				}))
			} else {
				// Thinking not requested: promote misplaced CoT to visible text (#37635).
				st.promotedReasoning = true
				st.emitTextDelta(&out, rcStr)
			}
		}
	}

	if c, ok := delta["content"]; ok && c != nil {
		if contentStr, _ := c.(string); contentStr != "" {
			st.emitTextDelta(&out, contentStr)
		}
	}

	if rawToolCalls, ok := delta["tool_calls"].([]any); ok {
		for _, rawTC := range rawToolCalls {
			tc, ok := rawTC.(map[string]any)
			if !ok {
				continue
			}
			idxFloat, _ := tc["index"].(float64)
			upstreamIndex := int(idxFloat)

			st.closeThinkingBlock(&out)
			st.closeTextBlock(&out)

			if _, exists := st.toolCallAccumulator[upstreamIndex]; !exists {
				callID, _ := tc["id"].(string)
				if callID == "" {
					callID = "toolu_" + st.idGen("toolu_", 12)
				}
				fn, _ := tc["function"].(map[string]any)
				name, _ := fn["name"].(string)
				st.toolCallAccumulator[upstreamIndex] = &chatToClaudeToolState{
					id:   callID,
					name: name,
					args: "",
				}
				st.toolCallOrder = append(st.toolCallOrder, upstreamIndex)
				st.toolBlockIndices[upstreamIndex] = st.blockIndex
				out = append(out, st.emit("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": st.blockIndex,
					"content_block": map[string]any{
						"type":  "tool_use",
						"id":    callID,
						"name":  name,
						"input": map[string]any{},
					},
				}))
				st.blockIndex++
			}

			fn, _ := tc["function"].(map[string]any)
			if argDelta, ok := fn["arguments"].(string); ok && argDelta != "" {
				st.toolCallAccumulator[upstreamIndex].args += argDelta
				out = append(out, st.emit("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": st.toolBlockIndices[upstreamIndex],
					"delta": map[string]any{
						"type":         "input_json_delta",
						"partial_json": argDelta,
					},
				}))
			}
		}
	}

	if finishReason == "stop" || finishReason == "length" || finishReason == "tool_calls" || finishReason == "function_call" || finishReason == "content_filter" {
		st.finishReason = finishReason
		st.sawFinish = true
		st.finished = true
		st.finalizeContentBlocks(&out)

		st.stopReason = "end_turn"
		switch finishReason {
		case "length":
			st.stopReason = "max_tokens"
		case "tool_calls", "function_call":
			st.stopReason = "tool_use"
		case "content_filter":
			st.stopReason = "refusal"
		}
		// Do not emit message_delta/stop yet: OpenAI-compatible upstreams often
		// send the usage-only chunk after finish_reason when include_usage=true.
	}
	return out, false
}

// PingEvent returns the keepalive ping frame the app shell emits on the
// keepalive tick (before the first upstream token this is the only thing the
// client receives; it must NOT fake message_start). This mirrors the original
// keepalive branch which emitted a bare `ping` event.
func (st *ChatToClaudeStream) PingEvent() OutEvent {
	return st.emit("ping", map[string]any{"type": "ping"})
}

// Finalize emits the post-loop message_delta + message_stop trailer. It is ONLY
// meaningful on a finished (non-error) stream: the app shell calls it after
// breaking the loop when st.Finished() is true. Finalize is idempotent for the
// content-block half (finalizeContentBlocks), but the trailer itself is gated
// by the caller on Finished — calling it twice would emit a duplicate trailer,
// so the app calls it exactly once after the loop. Note: this differs from the
// Anthropic/Responses machines where Finalize is the EOF funnel; here EOF is
// handled inside Handle via handleEOF, and Finalize is the finalized-stream
// trailer only.
func (st *ChatToClaudeStream) Finalize() []OutEvent {
	if !st.finished {
		return nil
	}
	if st.trailerSent {
		return nil
	}
	st.trailerSent = true
	var out []OutEvent
	st.ensureMessageStart(&out)
	out = append(out, st.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": st.stopReason, "stop_sequence": nil},
		"usage": BuildClaudeDeltaUsage(st.fullUsage),
	}))
	out = append(out, st.emit("message_stop", map[string]any{"type": "message_stop"}))
	return out
}
