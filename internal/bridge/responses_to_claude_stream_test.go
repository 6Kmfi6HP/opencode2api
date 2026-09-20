package bridge

import (
	"strings"
	"testing"
)

// fixedIDGen returns a deterministic body so frame bytes are stable under test.
// It mimics random.String's shape (the prefix is unused, exactly like the
// production DefaultIDGen).
func fixedIDGen(prefix string, n int) string {
	_ = prefix
	// Repeat "ab" to reach n chars (always even n here: 24 and 12).
	var b strings.Builder
	for b.Len() < n {
		b.WriteString("ab")
	}
	return b.String()[:n]
}

// collectRaw concatenates the Raw bytes of every OutEvent for byte assertions.
func collectRaw(evs []OutEvent) string {
	var b strings.Builder
	for _, e := range evs {
		b.Write(e.Raw)
	}
	return b.String()
}

// TestResponsesToClaude_BasicTextThenCompleted verifies the byte shape of a
// minimal text stream: message_start+ping once, content_block start/delta, then
// the message_delta/message_stop trailer with stop_reason=end_turn.
func TestResponsesToClaude_BasicTextThenCompleted(t *testing.T) {
	st := NewResponsesToClaudeStream(fixedIDGen, "m", true)

	var sb strings.Builder
	push := func(evt map[string]any, fe string) {
		sb.WriteString(collectRaw(st.Handle(ResponsesEvent{Event: evt, FrameEvent: fe})))
		if st.Finished() && !st.Finalized() {
			sb.WriteString(collectRaw(st.Finalize()))
		}
	}

	push(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_s1"}}, "response.created")
	push(map[string]any{"type": "response.output_item.added", "output_index": float64(0), "item": map[string]any{"id": "msg_1", "type": "message", "role": "assistant"}}, "")
	push(map[string]any{"type": "response.output_text.delta", "output_index": float64(0), "item_id": "msg_1", "delta": "hello"}, "")
	push(map[string]any{"type": "response.output_text.delta", "output_index": float64(0), "item_id": "msg_1", "delta": " world"}, "")
	push(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_s1", "status": "completed", "usage": map[string]any{"input_tokens": 5, "output_tokens": 6, "total_tokens": 11}}}, "")

	body := sb.String()
	// adoptResponseID must have normalized resp_s1 -> msg_-prefixed deterministic id.
	if strings.Contains(body, `"id":"resp_s1"`) {
		t.Fatalf("downstream id must not leak resp_ shape:\n%s", body)
	}
	for _, want := range []string{
		"event: message_start\n",
		"event: ping\n",
		"event: content_block_start\n",
		`"type":"text"`,
		"event: content_block_delta\n",
		`"text":"hello"`,
		`"text":" world"`,
		"event: message_delta\n",
		`"stop_reason":"end_turn"`,
		"event: message_stop\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
	// text_chars=11, saw/done counters set.
	c := st.Counters()
	if c.TextChars != 11 || !c.SawFinish || !c.DoneSeen || c.FinishReason != "stop" {
		t.Fatalf("counters = %+v", c)
	}
	if st.ChunkCount() != 2 {
		t.Fatalf("chunkCount = %d, want 2 (one per text delta)", st.ChunkCount())
	}
	if _, _, tt := UsageFromResponsesMap(st.Usage()); tt != 11 {
		t.Fatalf("usage total = %d, want 11", tt)
	}
}

// TestResponsesToClaude_AdoptResponseIDFallbackToMsg verifies that when no
// response.id is supplied the initial msg_-prefixed random id is used.
func TestResponsesToClaude_AdoptResponseIDFallbackToMsg(t *testing.T) {
	st := NewResponsesToClaudeStream(fixedIDGen, "m", true)
	out := st.Handle(ResponsesEvent{Event: map[string]any{"type": "response.created", "response": map[string]any{}}, FrameEvent: "response.created"})
	body := collectRaw(out)
	if !strings.Contains(body, `"id":"msg_abababababababababababab"`) {
		t.Fatalf("expected fallback msg id msg_+24 from fixedIDGen:\n%s", body)
	}
}

// TestResponsesToClaude_SignatureRoundtrip verifies encrypted_content latching
// onto a thinking block and emission as signature_delta before its
// content_block_stop during finalize.
func TestResponsesToClaude_SignatureRoundtrip(t *testing.T) {
	st := NewResponsesToClaudeStream(fixedIDGen, "m", true)
	var sb strings.Builder
	push := func(evt map[string]any) {
		sb.WriteString(collectRaw(st.Handle(ResponsesEvent{Event: evt})))
		if st.Finished() && !st.Finalized() {
			sb.WriteString(collectRaw(st.Finalize()))
		}
	}
	push(map[string]any{"type": "response.output_item.added", "output_index": float64(0), "item": map[string]any{"id": "rs_1", "type": "reasoning"}})
	push(map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": float64(0), "item_id": "rs_1", "delta": "thinking"})
	push(map[string]any{"type": "response.output_text.delta", "output_index": float64(1), "item_id": "msg_1", "delta": "answer"})
	push(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{
		map[string]any{"id": "rs_1", "type": "reasoning", "encrypted_content": "sig-abc"},
	}}})
	body := sb.String()
	if !strings.Contains(body, `"type":"signature_delta"`) || !strings.Contains(body, `"signature":"sig-abc"`) {
		t.Fatalf("signature_delta with latched signature missing:\n%s", body)
	}
	// signature_delta must precede the thinking block's stop.
	sigIdx := strings.Index(body, `"type":"signature_delta"`)
	if sigIdx < 0 {
		t.Fatalf("no signature_delta:\n%s", body)
	}
}

// TestResponsesToClaude_EmptyReplyPromotion verifies the reasoning fallback is
// promoted to a single text block when no text/tool_use was produced.
func TestResponsesToClaude_EmptyReplyPromotion(t *testing.T) {
	st := NewResponsesToClaudeStream(fixedIDGen, "m", true)
	var sb strings.Builder
	push := func(evt map[string]any) {
		sb.WriteString(collectRaw(st.Handle(ResponsesEvent{Event: evt})))
		if st.Finished() && !st.Finalized() {
			sb.WriteString(collectRaw(st.Finalize()))
		}
	}
	push(map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": float64(0), "item_id": "rs_1", "delta": "only reasoning"})
	push(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}})
	body := sb.String()
	// reasoning emitted as thinking block, then promoted again as a text block.
	if !strings.Contains(body, `"text":"only reasoning"`) {
		t.Fatalf("empty-reply promotion to a text block missing:\n%s", body)
	}
	if !strings.Contains(body, `"type":"text"`) {
		t.Fatalf("promoted text block_start missing:\n%s", body)
	}
}

// TestResponsesToClaude_IncompleteStopReason verifies response.incomplete maps
// to stop_reason=max_tokens (and the length finish counter).
func TestResponsesToClaude_IncompleteStopReason(t *testing.T) {
	st := NewResponsesToClaudeStream(fixedIDGen, "m", true)
	var sb strings.Builder
	push := func(evt map[string]any) {
		sb.WriteString(collectRaw(st.Handle(ResponsesEvent{Event: evt})))
		if st.Finished() && !st.Finalized() {
			sb.WriteString(collectRaw(st.Finalize()))
		}
	}
	push(map[string]any{"type": "response.output_text.delta", "output_index": float64(0), "item_id": "msg_1", "delta": "cut off"})
	push(map[string]any{"type": "response.incomplete", "response": map[string]any{"usage": map[string]any{"input_tokens": 1, "output_tokens": 2}}})
	body := sb.String()
	if !strings.Contains(body, `"stop_reason":"max_tokens"`) {
		t.Fatalf("incomplete should map to max_tokens:\n%s", body)
	}
	if c := st.Counters(); c.FinishReason != "length" {
		t.Fatalf("finishReason = %q, want length", c.FinishReason)
	}
}

// TestResponsesToClaude_ToolUseStopReason verifies a function_call in the
// completed output drives stop_reason=tool_use and the tool_use block bytes.
func TestResponsesToClaude_ToolUseStopReason(t *testing.T) {
	st := NewResponsesToClaudeStream(fixedIDGen, "m", true)
	var sb strings.Builder
	push := func(evt map[string]any) {
		sb.WriteString(collectRaw(st.Handle(ResponsesEvent{Event: evt})))
		if st.Finished() && !st.Finalized() {
			sb.WriteString(collectRaw(st.Finalize()))
		}
	}
	push(map[string]any{"type": "response.output_item.added", "output_index": float64(0), "item": map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "get_weather"}})
	push(map[string]any{"type": "response.function_call_arguments.delta", "output_index": float64(0), "item_id": "fc_1", "delta": `{"city":"SF"}`})
	push(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{
		map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": `{"city":"SF"}`},
	}}})
	body := sb.String()
	if !strings.Contains(body, `"stop_reason":"tool_use"`) {
		t.Fatalf("tool_use stop reason missing:\n%s", body)
	}
	if !strings.Contains(body, `"type":"tool_use"`) || !strings.Contains(body, `"name":"get_weather"`) || !strings.Contains(body, `"id":"call_1"`) {
		t.Fatalf("tool_use block bytes missing:\n%s", body)
	}
	if !strings.Contains(body, `"type":"input_json_delta"`) {
		t.Fatalf("input_json_delta missing:\n%s", body)
	}
	if c := st.Counters(); c.ToolCallCount != 1 {
		t.Fatalf("ToolCallCount = %d, want 1", c.ToolCallCount)
	}
}

// TestResponsesToClaude_PromotedReasoningWhenNotKept verifies reasoning is
// promoted to visible text (and counted) when wantReasoning is false.
func TestResponsesToClaude_PromotedReasoningWhenNotKept(t *testing.T) {
	st := NewResponsesToClaudeStream(fixedIDGen, "m", false)
	var sb strings.Builder
	push := func(evt map[string]any) {
		sb.WriteString(collectRaw(st.Handle(ResponsesEvent{Event: evt})))
		if st.Finished() && !st.Finalized() {
			sb.WriteString(collectRaw(st.Finalize()))
		}
	}
	push(map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": float64(0), "item_id": "rs_1", "delta": "internal monologue"})
	push(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}})
	body := sb.String()
	if strings.Contains(body, `"type":"thinking"`) {
		t.Fatalf("thinking block must not appear when !wantReasoning:\n%s", body)
	}
	if !strings.Contains(body, `"text":"internal monologue"`) {
		t.Fatalf("reasoning should be promoted to text:\n%s", body)
	}
	if c := st.Counters(); !c.PromotedReasoning || c.ReasoningChars != len("internal monologue") {
		t.Fatalf("counters = %+v", c)
	}
}

// TestResponsesToClaude_FinalizeIdempotent verifies Finalize emits the trailer
// exactly once.
func TestResponsesToClaude_FinalizeIdempotent(t *testing.T) {
	st := NewResponsesToClaudeStream(fixedIDGen, "m", true)
	st.Handle(ResponsesEvent{Event: map[string]any{"type": "response.output_text.delta", "output_index": float64(0), "delta": "hi"}})
	st.Handle(ResponsesEvent{Event: map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}}})
	first := collectRaw(st.Finalize())
	if !strings.Contains(first, "message_stop") {
		t.Fatalf("first Finalize should emit message_stop:\n%s", first)
	}
	if second := st.Finalize(); len(second) != 0 {
		t.Fatalf("second Finalize must be a no-op, got %d events", len(second))
	}
	if !st.Finalized() {
		t.Fatal("Finalized should report true after Finalize")
	}
}

// TestResponsesToClaude_EOFErrorEvent verifies the no-output EOF error frame
// byte shape matches the original emitError("stream ended without completion").
func TestResponsesToClaude_EOFErrorEvent(t *testing.T) {
	st := NewResponsesToClaudeStream(fixedIDGen, "m", true)
	ev := st.EOFErrorEvent()
	if ev.Event != "error" {
		t.Fatalf("EOFErrorEvent event = %q, want error", ev.Event)
	}
	if !strings.Contains(string(ev.Raw), `"message":"stream ended without completion"`) {
		t.Fatalf("EOF error bytes = %s", string(ev.Raw))
	}
	if !strings.HasPrefix(string(ev.Raw), "event: error\n") || !strings.HasSuffix(string(ev.Raw), "\n\n") {
		t.Fatalf("EOF error frame shape wrong: %q", string(ev.Raw))
	}
}

// TestResponsesToClaude_PingEvent verifies the keepalive ping frame byte shape.
func TestResponsesToClaude_PingEvent(t *testing.T) {
	st := NewResponsesToClaudeStream(fixedIDGen, "m", true)
	ev := st.PingEvent()
	if string(ev.Raw) != "event: ping\ndata: {\"type\":\"ping\"}\n\n" {
		t.Fatalf("ping frame = %q", string(ev.Raw))
	}
}
