package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- helpers ---

func hasEvent(events []sseEvent, name string) bool {
	for _, e := range events {
		if e.Name == name {
			return true
		}
	}
	return false
}

func countEvent(events []sseEvent, name string) int {
	n := 0
	for _, e := range events {
		if e.Name == name {
			n++
		}
	}
	return n
}

// errorReader simulates a non-EOF read error after writing some data.
type errorReader struct {
	data   string
	pos    int
	errMsg string
}

func (e *errorReader) Read(p []byte) (int, error) {
	if e.pos >= len(e.data) {
		return 0, io.ErrClosedPipe
	}
	n := copy(p, e.data[e.pos:])
	e.pos += n
	if e.pos >= len(e.data) {
		return n, io.ErrClosedPipe
	}
	return n, nil
}

func (e *errorReader) Close() error { return nil }

// slowReader simulates a reader that blocks until closed, to test keepalive.
type slowReader struct {
	closed chan struct{}
}

func newSlowReader() *slowReader {
	return &slowReader{closed: make(chan struct{})}
}

func (s *slowReader) Read(p []byte) (int, error) {
	<-s.closed
	return 0, io.EOF
}

func (s *slowReader) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return nil
}

// safeRecorder wraps httptest.ResponseRecorder so tests can read the body
// concurrently while the handler goroutine may still be writing.
type safeRecorder struct {
	mu sync.Mutex
	rr *httptest.ResponseRecorder
}

func newSafeRecorder() *safeRecorder {
	return &safeRecorder{rr: httptest.NewRecorder()}
}

func (s *safeRecorder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rr.Write(p)
}
func (s *safeRecorder) WriteHeader(code int) { s.rr.WriteHeader(code) }
func (s *safeRecorder) Header() http.Header  { return s.rr.Header() }

func (s *safeRecorder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rr.Body.String()
}

// =====================================================================
// Claude stream handler tests
// =====================================================================

func TestClaudeStream_PartialEOF_NoFinish_ErrorOnly(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	claudeStreamHandler(context.Background(), rr, io.NopCloser(strings.NewReader(upstream)), "m", false)
	events := parseSSEEvents(t, rr.Body.String())
	if !hasEvent(events, "error") {
		t.Fatalf("expected error event, got:\n%s", rr.Body.String())
	}
	if hasEvent(events, "message_stop") {
		t.Fatalf("must not emit message_stop on partial EOF:\n%s", rr.Body.String())
	}
	if hasEvent(events, "message_delta") {
		t.Fatalf("must not emit message_delta on partial EOF:\n%s", rr.Body.String())
	}
	// message_start is expected — the stream did deliver content before EOF.
	// What must NOT happen: message_delta, message_stop, end_turn.
	// Verify error structure
	for _, e := range events {
		if e.Name == "error" {
			errObj, ok := e.Data["error"].(map[string]any)
			if !ok {
				t.Fatalf("error event missing error object: %#v", e.Data)
			}
			if errObj["type"] != "api_error" {
				t.Fatalf("error type = %#v, want api_error", errObj["type"])
			}
			if errObj["message"] == "" {
				t.Fatalf("error message empty: %#v", errObj)
			}
		}
	}
}

func TestClaudeStream_DoneNoFinish_ErrorOnly(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hello"},"finish_reason":null}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	claudeStreamHandler(context.Background(), rr, io.NopCloser(strings.NewReader(upstream)), "m", false)
	events := parseSSEEvents(t, rr.Body.String())
	if !hasEvent(events, "error") {
		t.Fatalf("expected error event for [DONE] without finish:\n%s", rr.Body.String())
	}
	if hasEvent(events, "message_stop") || hasEvent(events, "message_delta") {
		t.Fatalf("must not emit message_stop/delta on [DONE] without finish:\n%s", rr.Body.String())
	}
}

func TestClaudeStream_InBandError_ErrorOnly(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
		``,
		`data: {"error":{"type":"rate_limit_error","message":"quota exceeded"}}`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	claudeStreamHandler(context.Background(), rr, io.NopCloser(strings.NewReader(upstream)), "m", false)
	events := parseSSEEvents(t, rr.Body.String())
	if !hasEvent(events, "error") {
		t.Fatalf("expected error event for in-band error:\n%s", rr.Body.String())
	}
	if hasEvent(events, "message_stop") || hasEvent(events, "message_delta") {
		t.Fatalf("must not emit message_stop/delta on in-band error:\n%s", rr.Body.String())
	}
	// Verify the error message is propagated
	for _, e := range events {
		if e.Name == "error" {
			errObj, _ := e.Data["error"].(map[string]any)
			msg, _ := errObj["message"].(string)
			if !strings.Contains(msg, "quota exceeded") {
				t.Fatalf("error message should contain 'quota exceeded', got %q", msg)
			}
		}
	}
}

func TestClaudeStream_ReaderError_ErrorOnly(t *testing.T) {
	er := &errorReader{
		data: "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n",
	}
	rr := httptest.NewRecorder()
	claudeStreamHandler(context.Background(), rr, er, "m", false)
	events := parseSSEEvents(t, rr.Body.String())
	if !hasEvent(events, "error") {
		t.Fatalf("expected error event for reader error:\n%s", rr.Body.String())
	}
	if hasEvent(events, "message_stop") || hasEvent(events, "message_delta") {
		t.Fatalf("must not emit message_stop/delta on reader error:\n%s", rr.Body.String())
	}
}

func TestClaudeStream_BadJSON_ErrorOnly(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hello"},"finish_reason":null}]}`,
		``,
		`data: {not valid json}`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	claudeStreamHandler(context.Background(), rr, io.NopCloser(strings.NewReader(upstream)), "m", false)
	events := parseSSEEvents(t, rr.Body.String())
	if !hasEvent(events, "error") {
		t.Fatalf("expected error event for bad JSON:\n%s", rr.Body.String())
	}
	if hasEvent(events, "message_stop") || hasEvent(events, "message_delta") {
		t.Fatalf("must not emit message_stop/delta on bad JSON:\n%s", rr.Body.String())
	}
}

func TestClaudeStream_NormalFinishWithUsage(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hello world"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	claudeStreamHandler(context.Background(), rr, io.NopCloser(strings.NewReader(upstream)), "m", false)
	events := parseSSEEvents(t, rr.Body.String())
	if hasEvent(events, "error") {
		t.Fatalf("must not emit error on normal finish:\n%s", rr.Body.String())
	}
	if !hasEvent(events, "message_start") {
		t.Fatalf("expected message_start:\n%s", rr.Body.String())
	}
	if !hasEvent(events, "message_delta") {
		t.Fatalf("expected message_delta:\n%s", rr.Body.String())
	}
	if !hasEvent(events, "message_stop") {
		t.Fatalf("expected message_stop:\n%s", rr.Body.String())
	}
	// Verify usage in message_delta
	for _, e := range events {
		if e.Name == "message_delta" {
			usage, ok := e.Data["usage"].(map[string]any)
			if !ok {
				t.Fatalf("message_delta missing usage: %#v", e.Data["usage"])
			}
			in, _ := usage["input_tokens"].(float64)
			if int(in) != 10 {
				t.Fatalf("input_tokens = %v, want 10", in)
			}
		}
	}
}

func TestClaudeStream_KeepalivePingBeforeFirstChunk(t *testing.T) {
	// Use a very short keepalive interval so the ticker fires before any data arrives.
	old := claudeKeepaliveInterval
	claudeKeepaliveInterval = 10 * time.Millisecond
	t.Cleanup(func() { claudeKeepaliveInterval = old })

	sr := newSlowReader()
	tsr := newSafeRecorder()

	done := make(chan struct{})
	go func() {
		claudeStreamHandler(context.Background(), tsr, sr, "m", false)
		close(done)
	}()

	// Wait long enough for at least one ticker ping to arrive.
	time.Sleep(80 * time.Millisecond)

	body := tsr.String()
	// We should see at least one ping event in the output.
	if !strings.Contains(body, "event: ping") {
		t.Fatalf("expected at least one ping before first chunk:\n%s", body)
	}
	// Must NOT have message_start yet (no upstream token seen).
	if strings.Contains(body, "event: message_start") {
		t.Fatalf("must not emit message_start before first upstream token:\n%s", body)
	}

	// Now close the reader so the handler can exit.
	sr.Close()
	<-done
}

func TestClaudeStream_ContextCancel_QuietExit(t *testing.T) {
	sr := newSlowReader()
	tsr := newSafeRecorder()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		claudeStreamHandler(ctx, tsr, sr, "m", false)
		close(done)
	}()

	// Let a keepalive or two fire, then cancel context.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := tsr.String()
	// Should not contain error events from cancellation.
	// (ping events are fine, but error events are not.)
	if strings.Contains(body, "event: error") {
		t.Fatalf("must not emit error on context cancel:\n%s", body)
	}
}

// =====================================================================
// Claude I/O boundary tests
// =====================================================================

func TestClaudeStream_FinishNoTrailingNewline_EOF(t *testing.T) {
	// Valid finish JSON without trailing newline + EOF should complete normally.
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hello"},"finish_reason":null}]}`,
		``,
		// Last line has NO trailing newline — bufio returns it + io.EOF
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
	}, "\n")
	rr := httptest.NewRecorder()
	claudeStreamHandler(context.Background(), rr, io.NopCloser(strings.NewReader(upstream)), "m", false)
	events := parseSSEEvents(t, rr.Body.String())
	if hasEvent(events, "error") {
		t.Fatalf("must not emit error when finish chunk has no trailing newline:\n%s", rr.Body.String())
	}
	if !hasEvent(events, "message_stop") {
		t.Fatalf("expected message_stop:\n%s", rr.Body.String())
	}
	if !hasEvent(events, "message_delta") {
		t.Fatalf("expected message_delta:\n%s", rr.Body.String())
	}
}

func TestClaudeStream_PartialDeltaThenReaderError(t *testing.T) {
	// A valid content delta line followed immediately by a non-EOF reader error.
	// The delta should be emitted, then the error event should follow.
	er := &errorReader{
		data: `data: {"choices":[{"delta":{"content":"partial delta"},"finish_reason":null}]}` + "\n\n",
	}
	rr := httptest.NewRecorder()
	claudeStreamHandler(context.Background(), rr, er, "m", false)
	body := rr.Body.String()
	// The partial delta content should be present
	if !strings.Contains(body, "partial delta") {
		t.Fatalf("partial delta must be emitted before error:\n%s", body)
	}
	// And then an error event
	events := parseSSEEvents(t, body)
	if !hasEvent(events, "error") {
		t.Fatalf("expected error event after partial delta:\n%s", body)
	}
	if hasEvent(events, "message_stop") || hasEvent(events, "message_delta") {
		t.Fatalf("must not emit message_stop/delta on reader error:\n%s", body)
	}
}

// =====================================================================
// Responses stream handler tests
// =====================================================================

func TestResponsesStream_PartialEOF_ResponseFailed(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"r","created":1,"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(upstream)), Header: make(http.Header)}
	responsesStreamHandler(rr, nil, resp, "m", "m", false, nil, nil, ResponsesAPIRequest{})
	events := parseSSEEvents(t, rr.Body.String())
	if !hasEvent(events, "response.failed") {
		t.Fatalf("expected response.failed on partial EOF:\n%s", rr.Body.String())
	}
	if hasEvent(events, "response.completed") {
		t.Fatalf("must not emit response.completed on partial EOF:\n%s", rr.Body.String())
	}
	// Verify status=failed in the response.failed payload
	for _, e := range events {
		if e.Name == "response.failed" {
			r, _ := e.Data["response"].(map[string]any)
			if r["status"] != "failed" {
				t.Fatalf("response.failed status = %#v, want failed", r["status"])
			}
			errObj, _ := r["error"].(map[string]any)
			if errObj == nil {
				t.Fatalf("response.failed missing error object")
			}
			if errObj["message"] == nil || errObj["message"] == "" {
				t.Fatalf("response.failed error message empty")
			}
		}
	}
}

func TestResponsesStream_InBandError_ResponseFailed(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"r","created":1,"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
		``,
		`data: {"error":{"message":"model overloaded"}}`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(upstream)), Header: make(http.Header)}
	responsesStreamHandler(rr, nil, resp, "m", "m", false, nil, nil, ResponsesAPIRequest{})
	events := parseSSEEvents(t, rr.Body.String())
	if !hasEvent(events, "response.failed") {
		t.Fatalf("expected response.failed on in-band error:\n%s", rr.Body.String())
	}
	if hasEvent(events, "response.completed") {
		t.Fatalf("must not emit response.completed on in-band error:\n%s", rr.Body.String())
	}
}

func TestResponsesStream_ReaderError_ResponseFailed(t *testing.T) {
	er := &errorReader{
		data: "data: {\"id\":\"r\",\"created\":1,\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n",
	}
	rr := httptest.NewRecorder()
	resp := &http.Response{StatusCode: 200, Body: er, Header: make(http.Header)}
	responsesStreamHandler(rr, nil, resp, "m", "m", false, nil, nil, ResponsesAPIRequest{})
	events := parseSSEEvents(t, rr.Body.String())
	if !hasEvent(events, "response.failed") {
		t.Fatalf("expected response.failed on reader error:\n%s", rr.Body.String())
	}
	if hasEvent(events, "response.completed") {
		t.Fatalf("must not emit response.completed on reader error:\n%s", rr.Body.String())
	}
}

func TestResponsesStream_DoneNoFinish_ResponseFailed(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"r","created":1,"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(upstream)), Header: make(http.Header)}
	responsesStreamHandler(rr, nil, resp, "m", "m", false, nil, nil, ResponsesAPIRequest{})
	events := parseSSEEvents(t, rr.Body.String())
	if !hasEvent(events, "response.failed") {
		t.Fatalf("expected response.failed on [DONE] without finish:\n%s", rr.Body.String())
	}
	if hasEvent(events, "response.completed") {
		t.Fatalf("must not emit response.completed on [DONE] without finish:\n%s", rr.Body.String())
	}
}

func TestResponsesStream_BadJSON_ResponseFailed(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"r","created":1,"choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
		``,
		`data: {not valid json}`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(upstream)), Header: make(http.Header)}
	responsesStreamHandler(rr, nil, resp, "m", "m", false, nil, nil, ResponsesAPIRequest{})
	events := parseSSEEvents(t, rr.Body.String())
	if !hasEvent(events, "response.failed") {
		t.Fatalf("expected response.failed on bad JSON:\n%s", rr.Body.String())
	}
	if hasEvent(events, "response.completed") {
		t.Fatalf("must not emit response.completed on bad JSON:\n%s", rr.Body.String())
	}
}

func TestResponsesStream_NormalFinish(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"r","created":1,"choices":[{"delta":{"content":"hello world"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(upstream)), Header: make(http.Header)}
	responsesStreamHandler(rr, nil, resp, "m", "m", false, nil, nil, ResponsesAPIRequest{})
	events := parseSSEEvents(t, rr.Body.String())
	if hasEvent(events, "response.failed") {
		t.Fatalf("must not emit response.failed on normal finish:\n%s", rr.Body.String())
	}
	if !hasEvent(events, "response.completed") {
		t.Fatalf("expected response.completed on normal finish:\n%s", rr.Body.String())
	}
}

func TestResponsesStream_LengthFinish_Incomplete(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"r","created":1,"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(upstream)), Header: make(http.Header)}
	responsesStreamHandler(rr, nil, resp, "m", "m", false, nil, nil, ResponsesAPIRequest{})
	events := parseSSEEvents(t, rr.Body.String())
	if hasEvent(events, "response.failed") {
		t.Fatalf("must not emit response.failed on length finish:\n%s", rr.Body.String())
	}
	if !hasEvent(events, "response.incomplete") {
		t.Fatalf("expected response.incomplete on length finish:\n%s", rr.Body.String())
	}
	if hasEvent(events, "response.completed") {
		t.Fatalf("must not emit response.completed on length finish:\n%s", rr.Body.String())
	}
}

// =====================================================================
// Responses I/O boundary tests
// =====================================================================

func TestResponsesStream_FinishNoTrailingNewline_EOF(t *testing.T) {
	// Valid finish JSON without trailing newline + EOF should complete normally.
	upstream := strings.Join([]string{
		`data: {"id":"r","created":1,"choices":[{"delta":{"content":"hello"},"finish_reason":null}]}`,
		``,
		// Last line has NO trailing newline — bufio returns it + io.EOF
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
	}, "\n")
	rr := httptest.NewRecorder()
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(upstream)), Header: make(http.Header)}
	responsesStreamHandler(rr, nil, resp, "m", "m", false, nil, nil, ResponsesAPIRequest{})
	events := parseSSEEvents(t, rr.Body.String())
	if hasEvent(events, "response.failed") {
		t.Fatalf("must not emit response.failed when finish chunk has no trailing newline:\n%s", rr.Body.String())
	}
	if !hasEvent(events, "response.completed") {
		t.Fatalf("expected response.completed:\n%s", rr.Body.String())
	}
}

func TestResponsesStream_PartialDeltaThenReaderError(t *testing.T) {
	// A valid content delta line followed immediately by a non-EOF reader error.
	er := &errorReader{
		data: `data: {"id":"r","created":1,"choices":[{"delta":{"content":"partial delta"},"finish_reason":null}]}` + "\n\n",
	}
	rr := httptest.NewRecorder()
	resp := &http.Response{StatusCode: 200, Body: er, Header: make(http.Header)}
	responsesStreamHandler(rr, nil, resp, "m", "m", false, nil, nil, ResponsesAPIRequest{})
	body := rr.Body.String()
	// The partial delta should be present (as output_text.delta)
	if !strings.Contains(body, "partial delta") {
		t.Fatalf("partial delta must be emitted before response.failed:\n%s", body)
	}
	events := parseSSEEvents(t, body)
	if !hasEvent(events, "response.failed") {
		t.Fatalf("expected response.failed after partial delta:\n%s", body)
	}
	if hasEvent(events, "response.completed") {
		t.Fatalf("must not emit response.completed on reader error:\n%s", body)
	}
}

// =====================================================================
// Trailing error tests (finish_reason seen, then error follows)
// =====================================================================

func TestClaudeStream_FinishThenInBandError(t *testing.T) {
	// finish_reason=stop seen, then in-band error. Must emit error, not message_stop/message_delta.
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hello"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: {"error":{"message":"overloaded after finish"}}`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	claudeStreamHandler(context.Background(), rr, io.NopCloser(strings.NewReader(upstream)), "m", false)
	events := parseSSEEvents(t, rr.Body.String())
	if !hasEvent(events, "error") {
		t.Fatalf("expected error event after finish+in-band error: %s", rr.Body.String())
	}
	if hasEvent(events, "message_stop") || hasEvent(events, "message_delta") {
		t.Fatalf("must not emit message_stop/delta after trailing in-band error: %s", rr.Body.String())
	}
}

func TestClaudeStream_FinishThenReaderError(t *testing.T) {
	// finish_reason=stop seen, then non-EOF reader error. Must emit error, not message_stop/message_delta.
	er := &errorReader{
		data: strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"hello"},"finish_reason":null}]}`,
			``,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			``,
		}, "\n"),
	}
	rr := httptest.NewRecorder()
	claudeStreamHandler(context.Background(), rr, er, "m", false)
	events := parseSSEEvents(t, rr.Body.String())
	if !hasEvent(events, "error") {
		t.Fatalf("expected error event after finish+reader error: %s", rr.Body.String())
	}
	if hasEvent(events, "message_stop") || hasEvent(events, "message_delta") {
		t.Fatalf("must not emit message_stop/delta after trailing reader error: %s", rr.Body.String())
	}
	// Must not leak the raw Go transport error string.
	for _, e := range events {
		if e.Name == "error" {
			errObj, _ := e.Data["error"].(map[string]any)
			msg, _ := errObj["message"].(string)
			if msg == "" || strings.Contains(msg, "io: read/write on closed pipe") {
				t.Fatalf("error message must be fixed string, got %q: %s", msg, rr.Body.String())
			}
		}
	}
}

func TestResponsesStream_FinishThenInBandError(t *testing.T) {
	// finish_reason=stop seen, then in-band error. Must emit response.failed, not response.completed.
	// Must NOT have any output_item.done (which would carry status=completed).
	upstream := strings.Join([]string{
		`data: {"id":"r","created":1,"choices":[{"delta":{"content":"hello"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		``,
		`data: {"error":{"message":"overloaded after finish"}}`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(upstream)), Header: make(http.Header)}
	responsesStreamHandler(rr, nil, resp, "m", "m", false, nil, nil, ResponsesAPIRequest{})
	events := parseSSEEvents(t, rr.Body.String())
	if !hasEvent(events, "response.failed") {
		t.Fatalf("expected response.failed after finish+in-band error: %s", rr.Body.String())
	}
	if hasEvent(events, "response.completed") {
		t.Fatalf("must not emit response.completed after trailing in-band error: %s", rr.Body.String())
	}
	if hasEvent(events, "response.output_item.done") {
		t.Fatalf("must not emit output_item.done before response.failed: %s", rr.Body.String())
	}
}

func TestResponsesStream_FinishThenReaderError(t *testing.T) {
	// finish_reason=stop seen, then non-EOF reader error.
	er := &errorReader{
		data: strings.Join([]string{
			`data: {"id":"r","created":1,"choices":[{"delta":{"content":"hello"},"finish_reason":null}]}`,
			``,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
			``,
		}, "\n"),
	}
	rr := httptest.NewRecorder()
	resp := &http.Response{StatusCode: 200, Body: er, Header: make(http.Header)}
	responsesStreamHandler(rr, nil, resp, "m", "m", false, nil, nil, ResponsesAPIRequest{})
	events := parseSSEEvents(t, rr.Body.String())
	if !hasEvent(events, "response.failed") {
		t.Fatalf("expected response.failed after finish+reader error: %s", rr.Body.String())
	}
	if hasEvent(events, "response.completed") {
		t.Fatalf("must not emit response.completed after trailing reader error: %s", rr.Body.String())
	}
	if hasEvent(events, "response.output_item.done") {
		t.Fatalf("must not emit output_item.done before response.failed: %s", rr.Body.String())
	}
	// Must not leak the raw Go transport error string.
	for _, e := range events {
		if e.Name == "response.failed" {
			resp, _ := e.Data["response"].(map[string]any)
			errObj, _ := resp["error"].(map[string]any)
			msg, _ := errObj["message"].(string)
			if msg == "" || strings.Contains(msg, "io: read/write on closed pipe") {
				t.Fatalf("error message must be fixed string, got %q: %s", msg, rr.Body.String())
			}
		}
	}
}

func TestResponsesStream_RefusalDelta(t *testing.T) {
	// Given: upstream sends a refusal delta then a normal stop
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body: strings.Join([]string{
			`data: {"choices":[{"delta":{"refusal":"I cannot help."},"finish_reason":null}]}`,
			``,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n"),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"primary-model","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()

	// When
	responsesHandler(rec, req)

	// Then
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	events := parseSSEEvents(t, rec.Body.String())
	if !hasEvent(events, "response.refusal.delta") {
		t.Fatalf("expected response.refusal.delta event:\n%s", rec.Body.String())
	}
	if !hasEvent(events, "response.refusal.done") {
		t.Fatalf("expected response.refusal.done event:\n%s", rec.Body.String())
	}

	// Verify refusal delta content
	for _, e := range events {
		if e.Name == "response.refusal.delta" {
			delta, _ := e.Data["delta"].(string)
			if delta != "I cannot help." {
				t.Fatalf("refusal delta = %q, want %q", delta, "I cannot help.")
			}
		}
		if e.Name == "response.refusal.done" {
			refusal, _ := e.Data["refusal"].(string)
			if refusal != "I cannot help." {
				t.Fatalf("refusal done = %q, want %q", refusal, "I cannot help.")
			}
		}
	}

	// Verify response.completed includes refusal content in the message item
	var completedEvent *sseEvent
	for i := range events {
		if events[i].Name == "response.completed" {
			completedEvent = &events[i]
			break
		}
	}
	if completedEvent == nil {
		t.Fatalf("expected response.completed event:\n%s", rec.Body.String())
	}
	resp, ok := completedEvent.Data["response"].(map[string]any)
	if !ok {
		t.Fatalf("response.completed missing response object: %#v", completedEvent.Data)
	}
	output, ok := resp["output"].([]any)
	if !ok {
		t.Fatalf("response.completed missing output array: %#v", resp["output"])
	}
	var foundRefusal bool
	for _, item := range output {
		itemMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if itemMap["type"] != "message" {
			continue
		}
		content, ok := itemMap["content"].([]any)
		if !ok {
			t.Fatalf("message item missing content array: %#v", itemMap)
		}
		for _, c := range content {
			cMap, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if cMap["type"] == "refusal" {
				foundRefusal = true
				if r, _ := cMap["refusal"].(string); r != "I cannot help." {
					t.Fatalf("refusal content = %q, want %q", r, "I cannot help.")
				}
			}
		}
	}
	if !foundRefusal {
		t.Fatalf("response.completed message item does not include refusal content:\n%s", rec.Body.String())
	}
}
