package bridge

// stream.go defines the transport-independent streaming contract shared by the
// protocol state machines that live in this package. A state machine consumes
// upstream SSE lines (as StreamEvent values) and produces OutEvent values — the
// serialized SSE frames to hand to the client — without ever touching an
// http.ResponseWriter, http.Flusher, logger, or stats recorder. The app shell
// owns those side effects: it serializes/writes each OutEvent and, at end of
// stream, reads the read-only snapshots to assemble logging.StreamStats and
// record usage.
//
// Keeping the wire bytes vs. the side effects separate is what lets the bridge
// stay pure: OutEvent.Raw already carries the exact bytes the old app-layer
// frame writer produced (including the trailing blank line), so the shell is a
// mechanical write+flush loop with byte-for-byte identical output.

// StreamEvent is a single decoded upstream SSE input. Implementations wrap one
// upstream line (or a parsed field of it) so the state machine never has to
// care how the line was read off the wire. The zero value carries no data.
type StreamEvent interface {
	// Line returns the raw SSE line (e.g. `data: {...}`), without the trailing
	// newline. State machines that operate on whole lines use this directly.
	Line() string
}

// LineEvent is the canonical StreamEvent: one raw upstream SSE line.
type LineEvent struct {
	// RawLine is the unparsed SSE line as read from the upstream body.
	RawLine string
}

// Line returns the raw SSE line.
func (e LineEvent) Line() string { return e.RawLine }

// OutEvent is one fully-serialized SSE frame the state machine wants the client
// to receive, plus the structured payload so the app shell may inspect fields
// without re-parsing Raw.
//
// Exactly one of these emission shapes is used per event to preserve byte
// equivalence with the previous app-layer writer:
//
//   - Raw set: the complete serialized SSE frame (e.g. `data: {...}\n\n` or
//     `data: [DONE]\n\n`). The shell writes Raw verbatim.
//   - Data set: a structured payload the shell serializes itself. No bridge
//     state machine uses this today; the field exists so callers that prefer
//     typed introspection have a stable place to read the chunk body.
type OutEvent struct {
	// Event is an optional SSE `event:` field name. Empty for the chat stream,
	// which only uses `data:` frames. Kept for parity with the Responses /
	// Anthropic state machines that emit named events.
	Event string
	// Data is the structured frame payload (the chunk map), when the state
	// machine produces one. Nil when Raw carries the whole frame.
	Data map[string]any
	// Raw is the complete serialized frame to write verbatim, including the
	// `data: ` prefix, the JSON or `[DONE]` token, and the trailing blank line.
	Raw []byte
	// Terminal reports that this is the final frame for the stream. The shell
	// may treat it as a hint to flush once more / stop selecting, but stream
	// termination is decided by the state machine's idempotent Finalize, never
	// by the transport.
	Terminal bool
}

// StreamCounters is the read-only snapshot of the per-stream accounting a state
// machine tracked, mirrored onto the field names of logging.StreamStats so the
// app shell can assemble that struct mechanically. The bridge keeps these as
// plain ints/bools/strings; only the shell imports internal/logging.
type StreamCounters struct {
	// TextChars accumulates the characters of emitted `content` deltas.
	TextChars int
	// ReasoningChars accumulates the characters of emitted `reasoning_content`
	// deltas.
	ReasoningChars int
	// PromotedReasoning reports reasoning that was surfaced as text because the
	// client did not ask to keep it separate.
	PromotedReasoning bool
	// ToolCallCount is the number of tool calls started during the stream.
	ToolCallCount int
	// SkippedSignatures counts signature deltas dropped because the target
	// protocol has no field for them.
	SkippedSignatures int
	// SkippedRedacted counts redacted_thinking deltas dropped for the same
	// reason.
	SkippedRedacted int
	// FinishReason is the terminal finish/stop reason the stream settled on.
	FinishReason string
	// SawFinish reports that a finish chunk was emitted.
	SawFinish bool
	// DoneSeen reports that the terminal `[DONE]`-equivalent frame was emitted.
	DoneSeen bool
}

// Streamer is a protocol state machine: feed it upstream events (Handle), then
// close it (Finalize). Both return the OutEvent frames to forward to the client.
//
// Implementations are NOT safe for concurrent use; the app shell drives them
// from its single read loop.
type Streamer interface {
	// Handle consumes one upstream stream event and returns the frames to send
	// downstream in order. It must be a pure transformation of internal state —
	// no I/O, logging, metrics, or config reads.
	Handle(ev StreamEvent) []OutEvent
	// Finalize idempotently closes the stream, emitting any terminal frames
	// (finish chunk, usage block, `[DONE]`-equivalent). Calling it more than
	// once must be a no-op after the first call. EOF and the protocol's normal
	// terminal event both funnel here.
	Finalize() []OutEvent
	// Usage returns the accumulated, merged upstream usage map (the full, raw
	// protocol-side usage — not the client-shaped one). The app shell converts
	// it (e.g. AnthropicUsageToChat) and records it via statsx.
	Usage() map[string]any
	// Counters returns a read-only snapshot of the stream accounting, for the
	// app shell to fold into logging.StreamStats.
	Counters() StreamCounters
}
