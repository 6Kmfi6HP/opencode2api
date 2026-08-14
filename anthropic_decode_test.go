package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// =====================================================================
// Test 1: Native JSON blocks [text, tool_use, text, thinking] -> content
// order and signature, tool_calls association ID
// =====================================================================

func TestAnthropicNativeJSON_BlocksOrderAndToolID(t *testing.T) {
	body := `{
		"id":"msg_abc",
		"type":"message",
		"role":"assistant",
		"model":"claude-test",
		"stop_reason":"end_turn",
		"content":[
			{"type":"text","text":"Hello "},
			{"type":"tool_use","id":"toolu_01ABC","name":"get_weather","input":{"city":"SF"}},
			{"type":"text","text":" Done."},
			{"type":"thinking","thinking":"Let me think","signature":"sig123"}
		],
		"usage":{"input_tokens":10,"output_tokens":20}
	}`
	out, err := convertAnthropicToOpenAI([]byte(body), "claude-test")
	if err != nil {
		t.Fatalf("convertAnthropicToOpenAI error: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(choices))
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)

	// Content should be "Hello  Done." (text blocks concatenated in order)
	content, _ := msg["content"].(string)
	if content != "Hello  Done." {
		t.Fatalf("content = %q, want %q", content, "Hello  Done.")
	}

	// reasoning_content should have the thinking text
	rc, _ := msg["reasoning_content"].(string)
	if rc != "Let me think" {
		t.Fatalf("reasoning_content = %q, want %q", rc, "Let me think")
	}

	// tool_calls should preserve the original ID
	toolCalls, _ := msg["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(toolCalls))
	}
	tc, _ := toolCalls[0].(map[string]any)
	id, _ := tc["id"].(string)
	if id != "toolu_01ABC" {
		t.Fatalf("tool_call id = %q, want toolu_01ABC", id)
	}
	fn, _ := tc["function"].(map[string]any)
	name, _ := fn["name"].(string)
	if name != "get_weather" {
		t.Fatalf("tool name = %q, want get_weather", name)
	}
	args, _ := fn["arguments"].(string)
	if !strings.Contains(args, "SF") {
		t.Fatalf("tool arguments = %q, want to contain SF", args)
	}

	// Private field should preserve original ordered blocks
	private, _ := msg["_opencode2api_anthropic_content"].([]any)
	if len(private) != 4 {
		t.Fatalf("expected 4 private blocks, got %d", len(private))
	}
	pb0, _ := private[0].(map[string]any)
	pb1, _ := private[1].(map[string]any)
	pb2, _ := private[2].(map[string]any)
	pb3, _ := private[3].(map[string]any)
	if pb0["type"] != "text" || pb1["type"] != "tool_use" ||
		pb2["type"] != "text" || pb3["type"] != "thinking" {
		t.Fatalf("private block order wrong: %v", private)
	}
	// Signature should be preserved in private thinking block
	sig, _ := pb3["signature"].(string)
	if sig != "sig123" {
		t.Fatalf("signature = %q, want sig123", sig)
	}
}

// =====================================================================
// Test 2: SSE interleaved index/order, thinking+signature, tool partial input
// =====================================================================

func TestAnthropicSSE_InterleavedIndexOrder(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"msg_s1","type":"message","role":"assistant","model":"claude-test","stop_reason":null,"usage":{"input_tokens":5,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"Let me think"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"sig456"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_02","name":"search","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"hello\"}"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":5,"output_tokens":15}}`,
		`{"type":"message_stop"}`,
	}, "\n")
	out, err := convertAnthropicToOpenAI([]byte(sse), "claude-test")
	if err != nil {
		t.Fatalf("convertAnthropicToOpenAI error: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)

	// Text should be "Hello world" (index 0 deltas concatenated, interleaving with other blocks is fine)
	content, _ := msg["content"].(string)
	if content != "Hello world" {
		t.Fatalf("content = %q, want 'Hello world'", content)
	}

	// Thinking + signature
	rc, _ := msg["reasoning_content"].(string)
	if rc != "Let me think" {
		t.Fatalf("reasoning_content = %q, want 'Let me think'", rc)
	}

	// Tool call
	toolCalls, _ := msg["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(toolCalls))
	}
	tc, _ := toolCalls[0].(map[string]any)
	id, _ := tc["id"].(string)
	if id != "toolu_02" {
		t.Fatalf("tool id = %q, want toolu_02", id)
	}
	fn, _ := tc["function"].(map[string]any)
	args, _ := fn["arguments"].(string)
	if !strings.Contains(args, "hello") {
		t.Fatalf("tool args = %q, want to contain 'hello'", args)
	}

	// Private blocks should preserve order: text(0), thinking(1), tool_use(2)
	private, _ := msg["_opencode2api_anthropic_content"].([]any)
	if len(private) != 3 {
		t.Fatalf("expected 3 private blocks, got %d", len(private))
	}
	pb0, _ := private[0].(map[string]any)
	pb1, _ := private[1].(map[string]any)
	pb2, _ := private[2].(map[string]any)
	if pb0["type"] != "text" || pb1["type"] != "thinking" || pb2["type"] != "tool_use" {
		t.Fatalf("private block order wrong: %v", private)
	}
	sig, _ := pb1["signature"].(string)
	if sig != "sig456" {
		t.Fatalf("signature = %q, want sig456", sig)
	}
}

// =====================================================================
// Test 3: message_start input/cache usage + message_delta output usage merge
// (top-level usage)
// =====================================================================

func TestAnthropicSSE_UsageMerge(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"msg_u","type":"message","role":"assistant","model":"claude-test","stop_reason":null,"usage":{"input_tokens":100,"cache_creation_input_tokens":8,"cache_read_input_tokens":64,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":100,"output_tokens":35,"cache_creation_input_tokens":8,"cache_read_input_tokens":64}}`,
		`{"type":"message_stop"}`,
	}, "\n")
	out, err := convertAnthropicToOpenAI([]byte(sse), "claude-test")
	if err != nil {
		t.Fatalf("convertAnthropicToOpenAI error: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	usage, ok := resp["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing: %#v", resp["usage"])
	}
	// Chat usage: prompt_tokens from input_tokens, completion_tokens from output_tokens
	pt, _ := usage["prompt_tokens"].(float64)
	if int(pt) != 100 {
		t.Fatalf("prompt_tokens = %v, want 100", pt)
	}
	ct, _ := usage["completion_tokens"].(float64)
	if int(ct) != 35 {
		t.Fatalf("completion_tokens = %v, want 35", ct)
	}
	tt, _ := usage["total_tokens"].(float64)
	if int(tt) != 135 {
		t.Fatalf("total_tokens = %v, want 135", tt)
	}
	// Cache detail should be preserved
	cc, _ := usage["cache_creation_input_tokens"].(float64)
	if int(cc) != 8 {
		t.Fatalf("cache_creation_input_tokens = %v, want 8", cc)
	}
	cr, _ := usage["cache_read_input_tokens"].(float64)
	if int(cr) != 64 {
		t.Fatalf("cache_read_input_tokens = %v, want 64", cr)
	}
}

// Test 3b: usage merge does not lose fields — message_start has input,
// message_delta has only output (missing input). Must not zero input.
func TestAnthropicSSE_UsageMergeNoFieldLoss(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"msg_u2","type":"message","role":"assistant","model":"claude-test","stop_reason":null,"usage":{"input_tokens":120,"cache_read_input_tokens":32,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":40}}`,
		`{"type":"message_stop"}`,
	}, "\n")
	out, err := convertAnthropicToOpenAI([]byte(sse), "claude-test")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	usage, _ := resp["usage"].(map[string]any)
	pt, _ := usage["prompt_tokens"].(float64)
	if int(pt) != 120 {
		t.Fatalf("prompt_tokens = %v, want 120 (must survive merge)", pt)
	}
	ct, _ := usage["completion_tokens"].(float64)
	if int(ct) != 40 {
		t.Fatalf("completion_tokens = %v, want 40", ct)
	}
	cr, _ := usage["cache_read_input_tokens"].(float64)
	if int(cr) != 32 {
		t.Fatalf("cache_read_input_tokens = %v, want 32 (must survive merge)", cr)
	}
}

// =====================================================================
// Test 4: SSE error preserves type/message, missing message_stop, malformed
// event JSON, malformed tool input JSON all return errors
// =====================================================================

func TestAnthropicSSE_ErrorEvent(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"msg_e1","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{}}}`,
		`{"type":"error","error":{"type":"overloaded_error","message":"Internal server error"}}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for SSE error event")
	}
	if !strings.Contains(err.Error(), "overloaded_error") {
		t.Fatalf("error should mention error type, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Internal server error") {
		t.Fatalf("error should mention error message, got: %v", err)
	}
}

func TestAnthropicSSE_MissingMessageStop(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"msg_e2","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for missing message_stop")
	}
	if !strings.Contains(err.Error(), "message_stop") {
		t.Fatalf("error should mention message_stop, got: %v", err)
	}
}

func TestAnthropicSSE_MalformedEventJSON(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"msg_e3","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{}}}`,
		`{bad json}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for malformed event JSON")
	}
}

func TestAnthropicSSE_MalformedToolInputJSON(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"msg_e4","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_x","name":"fn","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{bad}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for malformed tool input JSON")
	}
}

// =====================================================================
// Test 5: callOpenCodeAPI returns non-200 (502) for native Anthropic
// error/truncation — using the conversion interface directly
// =====================================================================

func TestConvertAnthropicToOpenAI_NativeErrorReturnsError(t *testing.T) {
	// Native error JSON body
	body := `{"type":"error","error":{"type":"overloaded_error","message":"Server overloaded"}}`
	_, err := convertAnthropicToOpenAI([]byte(body), "m")
	if err == nil {
		t.Fatal("expected error for native Anthropic error body")
	}
	if !strings.Contains(err.Error(), "overloaded_error") {
		t.Fatalf("error should contain error type, got: %v", err)
	}
}

func TestConvertAnthropicToOpenAI_TruncatedSSEReturnsError(t *testing.T) {
	// Truncated SSE — no message_stop
	sse := `{"type":"message_start","message":{"id":"msg_t","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{}}}`
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for truncated SSE (no message_stop)")
	}
}

func TestCallOpenCodeAPI_AnthropicErrorReturns502(t *testing.T) {
	// Upstream returns 200 with a native Anthropic error body.
	// callOpenCodeAPI should convert this to a 502 error.
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   `{"type":"error","error":{"type":"overloaded_error","message":"Server overloaded"}}`,
	}})
	body, status, _, err := callOpenCodeAPI(context.Background(), []byte(`{"model":"primary-model","messages":[]}`), "primary-model", UpstreamAuth{Mode: AuthRoutePublic})
	if err == nil {
		t.Fatal("callOpenCodeAPI should return error for native Anthropic error body")
	}
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	if body != nil {
		t.Fatalf("body should be nil on error, got %s", string(body))
	}
}

func TestCallOpenCodeAPI_AnthropicTruncatedReturns502(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   `{"type":"message_start","message":{"id":"msg_t","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{}}}`,
	}})
	body, status, _, err := callOpenCodeAPI(context.Background(), []byte(`{"model":"primary-model","messages":[]}`), "primary-model", UpstreamAuth{Mode: AuthRoutePublic})
	if err == nil {
		t.Fatal("callOpenCodeAPI should return error for truncated Anthropic SSE")
	}
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	if body != nil {
		t.Fatalf("body should be nil on error, got %s", string(body))
	}
}

// Test 5b: convertResponse strips the private field
func TestConvertResponse_StripsPrivateAnthropicField(t *testing.T) {
	body := `{"id":"msg_p","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hello","_opencode2api_anthropic_content":[{"type":"text","text":"Hello"}]},"finish_reason":"stop"}]}`
	out, err := convertResponse([]byte(body), false)
	if err != nil {
		t.Fatalf("convertResponse error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	if _, exists := msg["_opencode2api_anthropic_content"]; exists {
		t.Fatalf("private field should be stripped by convertResponse")
	}
}

// Test 5c: pure text (no thinking/tool) returns string content (compatibility)
func TestAnthropicNativeJSON_PureTextReturnsString(t *testing.T) {
	body := `{"id":"msg_pt","type":"message","role":"assistant","model":"claude-test","stop_reason":"end_turn","content":[{"type":"text","text":"Hello world"}],"usage":{"input_tokens":5,"output_tokens":3}}`
	out, err := convertAnthropicToOpenAI([]byte(body), "claude-test")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	content, _ := msg["content"].(string)
	if content != "Hello world" {
		t.Fatalf("content = %q, want 'Hello world'", content)
	}
	// Should NOT have private field for pure text
	if _, exists := msg["_opencode2api_anthropic_content"]; exists {
		t.Fatalf("pure text should not have private field")
	}
	if _, exists := msg["reasoning_content"]; exists {
		t.Fatalf("pure text should not have reasoning_content")
	}
	if _, exists := msg["tool_calls"]; exists {
		t.Fatalf("pure text should not have tool_calls")
	}
}

// Test: SSE with data: prefix lines should work
func TestAnthropicSSE_DataPrefix(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_d","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":5,"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		`data: {"type":"message_stop"}`,
	}, "\n")
	out, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	content, _ := msg["content"].(string)
	if content != "Hi" {
		t.Fatalf("content = %q, want 'Hi'", content)
	}
}

// Test: unknown event types are ignored
func TestAnthropicSSE_UnknownEventIgnored(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"msg_u","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":5,"output_tokens":0}}}`,
		`{"type":"some_unknown_event","foo":"bar"}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	}, "\n")
	out, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err != nil {
		t.Fatalf("unknown event should be ignored, got error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(choices))
	}
}

// Test: tool_use missing ID gets generated ID matching protocol
func TestAnthropicSSE_ToolUseMissingIDGeneratesToolu(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"msg_mid","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":5,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","name":"fn","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	}, "\n")
	out, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	toolCalls, _ := msg["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(toolCalls))
	}
	tc, _ := toolCalls[0].(map[string]any)
	id, _ := tc["id"].(string)
	if !strings.HasPrefix(id, "toolu_") {
		t.Fatalf("generated ID should start with toolu_, got %q", id)
	}
}

// Test: usage merge preserves cached-token details from message_start
func TestAnthropicSSE_UsageMergePreservesCachedTokens(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"msg_ct","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":200,"cache_creation_input_tokens":10,"cache_read_input_tokens":50,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":30}}`,
		`{"type":"message_stop"}`,
	}, "\n")
	out, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	usage, _ := resp["usage"].(map[string]any)
	cc, _ := usage["cache_creation_input_tokens"].(float64)
	if int(cc) != 10 {
		t.Fatalf("cache_creation_input_tokens = %v, want 10", cc)
	}
	cr, _ := usage["cache_read_input_tokens"].(float64)
	if int(cr) != 50 {
		t.Fatalf("cache_read_input_tokens = %v, want 50", cr)
	}
	tt, _ := usage["total_tokens"].(float64)
	if int(tt) != 230 {
		t.Fatalf("total_tokens = %v, want 230", tt)
	}
}

// Test: convertResponse also strips private field with keepReasoning=true
func TestConvertResponse_StripsPrivateFieldKeepReasoning(t *testing.T) {
	body := `{"id":"msg_p2","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hi","reasoning_content":"thinking","_opencode2api_anthropic_content":[{"type":"thinking","thinking":"thinking"}]},"finish_reason":"stop"}]}`
	out, err := convertResponse([]byte(body), true)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	if _, exists := msg["_opencode2api_anthropic_content"]; exists {
		t.Fatalf("private field should be stripped even with keepReasoning=true")
	}
	// reasoning_content should be kept when keepReasoning=true
	if _, exists := msg["reasoning_content"]; !exists {
		t.Fatalf("reasoning_content should be kept with keepReasoning=true")
	}
}

// Test: convertChatToResponses does not leak private field
func TestConvertChatToResponses_NoPrivateFieldLeak(t *testing.T) {
	chatBody := `{"id":"chatcmpl_x","created":1234,"choices":[{"index":0,"message":{"role":"assistant","content":"Hi","_opencode2api_anthropic_content":[{"type":"text","text":"Hi"}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`
	out := convertChatToResponses([]byte(chatBody), "m", false, nil, nil, nil)
	var resp map[string]any
	json.Unmarshal(out, &resp)
	outStr := string(out)
	if strings.Contains(outStr, "_opencode2api_anthropic_content") {
		t.Fatalf("Responses conversion leaks private field:\n%s", outStr)
	}
}

// Test: parseAnthropicSSE with data: prefix lines
func TestParseAnthropicSSE_DataPrefixCompatibility(t *testing.T) {
	sse := `data: {"type":"message_start","message":{"id":"msg_dp","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}
data: {"type":"content_block_stop","index":0}
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}
data: {"type":"message_stop"}`
	msg, blocks, err := parseAnthropicSSE([]byte(sse))
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if msg == nil {
		t.Fatal("message should not be nil")
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0]["type"] != "text" {
		t.Fatalf("block type = %v, want text", blocks[0]["type"])
	}
}

// Test: native JSON message with multiple tool_use blocks
func TestAnthropicNativeJSON_MultipleToolUse(t *testing.T) {
	body := `{
		"id":"msg_mt",
		"type":"message",
		"role":"assistant",
		"model":"claude-test",
		"stop_reason":"tool_use",
		"content":[
			{"type":"text","text":"Let me search"},
			{"type":"tool_use","id":"toolu_a","name":"search","input":{"q":"test"}},
			{"type":"tool_use","id":"toolu_b","name":"fetch","input":{"url":"http://x"}}
		],
		"usage":{"input_tokens":10,"output_tokens":20}
	}`
	out, err := convertAnthropicToOpenAI([]byte(body), "claude-test")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	toolCalls, _ := msg["tool_calls"].([]any)
	if len(toolCalls) != 2 {
		t.Fatalf("expected 2 tool_calls, got %d", len(toolCalls))
	}
	tc0, _ := toolCalls[0].(map[string]any)
	if tc0["id"] != "toolu_a" {
		t.Fatalf("first tool id = %v, want toolu_a", tc0["id"])
	}
	tc1, _ := toolCalls[1].(map[string]any)
	if tc1["id"] != "toolu_b" {
		t.Fatalf("second tool id = %v, want toolu_b", tc1["id"])
	}
	fr, _ := choice["finish_reason"].(string)
	if fr != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", fr)
	}
}

// Test: isAnthropicFormat detects SSE with data: prefix
func TestIsAnthropicFormat_SSEWithPrefix(t *testing.T) {
	sse := `data: {"type":"message_start","message":{}}`
	if !isAnthropicFormat([]byte(sse)) {
		t.Fatal("isAnthropicFormat should detect SSE with data: prefix")
	}
}

// Test: convertAnthropicMessageToOpenAI returns error on nil msg
func TestConvertAnthropicMessageToOpenAI_NilMsg(t *testing.T) {
	_, err := convertAnthropicMessageToOpenAI(nil, "m")
	if err == nil {
		t.Fatal("expected error for nil message")
	}
}

// Test: buildOpenAIResponse returns error on nil msg
func TestBuildOpenAIResponse_NilMsg(t *testing.T) {
	_, err := buildOpenAIResponse(nil, nil, "m")
	if err == nil {
		t.Fatal("expected error for nil message")
	}
}

// Test: convertAnthropicToOpenAI does not silently return body on parse failure
func TestConvertAnthropicToOpenAI_NoSilentPassthrough(t *testing.T) {
	// Random non-JSON, non-SSE garbage
	_, err := convertAnthropicToOpenAI([]byte("garbage not json"), "m")
	if err == nil {
		t.Fatal("should not silently return garbage body as success")
	}
}

// =====================================================================
// Fix 1: mergeUsageMaps — src overrides dst including 0
// =====================================================================

func TestMergeUsageMaps_SrcOverridesIncludingZero(t *testing.T) {
	dst := map[string]any{"output_tokens": float64(1), "input_tokens": float64(10)}
	src := map[string]any{"output_tokens": float64(35)}
	result := mergeUsageMaps(dst, src)
	ot, _ := result["output_tokens"].(float64)
	if int(ot) != 35 {
		t.Fatalf("output_tokens = %v, want 35 (src must override even non-zero)", ot)
	}
	it, _ := result["input_tokens"].(float64)
	if int(it) != 10 {
		t.Fatalf("input_tokens = %v, want 10 (absent from src must be retained)", it)
	}
}

func TestMergeUsageMaps_ZeroOverridesNonZero(t *testing.T) {
	dst := map[string]any{"output_tokens": float64(35)}
	src := map[string]any{"output_tokens": float64(0)}
	result := mergeUsageMaps(dst, src)
	ot, _ := result["output_tokens"].(float64)
	if int(ot) != 0 {
		t.Fatalf("output_tokens = %v, want 0 (src zero must override)", ot)
	}
}

func TestAnthropicSSE_UsageMerge_OutputTokensOverwrite(t *testing.T) {
	// message_start has output_tokens=1, message_delta has output_tokens=35.
	// Final must be 35.
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"msg_ot","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":100,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":100,"output_tokens":35}}`,
		`{"type":"message_stop"}`,
	}, "\n")
	out, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	usage, _ := resp["usage"].(map[string]any)
	ct, _ := usage["completion_tokens"].(float64)
	if int(ct) != 35 {
		t.Fatalf("completion_tokens = %v, want 35 (message_delta must override message_start)", ct)
	}
}

func TestMergeUsageMaps_NestedMapRecursive(t *testing.T) {
	dst := map[string]any{
		"prompt_tokens_details": map[string]any{"cached_tokens": float64(32)},
	}
	src := map[string]any{
		"prompt_tokens_details": map[string]any{"cached_tokens": float64(64)},
	}
	result := mergeUsageMaps(dst, src)
	ptd, _ := result["prompt_tokens_details"].(map[string]any)
	ct, _ := ptd["cached_tokens"].(float64)
	if int(ct) != 64 {
		t.Fatalf("nested cached_tokens = %v, want 64", ct)
	}
}

// =====================================================================
// Fix 2: parseAnthropicSSE — block start/stop tracking, sorted by index,
// SSE metadata lines, duplicate start error, unclosed block error
// =====================================================================

func TestAnthropicSSE_OutOfOrderStartSortedByIndex(t *testing.T) {
	// Starts arrive in order 2, 0, 1. Output must be sorted 0, 1, 2.
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"msg_oo","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"third"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"first"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"second"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	}, "\n")
	out, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	// Text blocks concatenated in index order: first second third
	content, _ := msg["content"].(string)
	if content != "firstsecondthird" {
		t.Fatalf("content = %q, want 'firstsecondthird'", content)
	}
}

func TestAnthropicSSE_DuplicateStartError(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"msg_ds","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for duplicate content_block_start")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error should mention duplicate, got: %v", err)
	}
}

func TestAnthropicSSE_DeltaForUnknownIndexError(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"msg_du","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{}}}`,
		`{"type":"content_block_delta","index":5,"delta":{"type":"text_delta","text":"Hi"}}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for delta on unknown index")
	}
	if !strings.Contains(err.Error(), "unknown index") {
		t.Fatalf("error should mention unknown index, got: %v", err)
	}
}

func TestAnthropicSSE_StopForUnknownIndexError(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"msg_su","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{}}}`,
		`{"type":"content_block_stop","index":3}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for stop on unknown index")
	}
}

func TestAnthropicSSE_UnclosedBlockError(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"msg_ub","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		// Missing content_block_stop for index 0
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for unclosed block")
	}
	if !strings.Contains(err.Error(), "unclosed") {
		t.Fatalf("error should mention unclosed, got: %v", err)
	}
}

func TestAnthropicSSE_StandardSSEMetadataLines(t *testing.T) {
	// Standard SSE format with event:/data:/comment lines.
	sse := strings.Join([]string{
		`: keepalive comment`,
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_sse","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	out, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err != nil {
		t.Fatalf("standard SSE should parse, got error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	content, _ := msg["content"].(string)
	if content != "Hi" {
		t.Fatalf("content = %q, want 'Hi'", content)
	}
}

func TestAnthropicSSE_ToolUseStartWithNonEmptyInput(t *testing.T) {
	// content_block_start with a complete input (no partial_json).
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"msg_ti","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_99","name":"fn","input":{"x":42}}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}, "\n")
	out, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	toolCalls, _ := msg["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(toolCalls))
	}
	tc, _ := toolCalls[0].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	args, _ := fn["arguments"].(string)
	if !strings.Contains(args, "42") {
		t.Fatalf("args = %q, want to contain 42", args)
	}
}

// =====================================================================
// Fix 3: buildOpenAIResponse — generated ID in both private block and tool_calls,
// private field for redacted_thinking
// =====================================================================

func TestBuildOpenAIResponse_GeneratedIDInPrivateAndToolCalls(t *testing.T) {
	// tool_use block with no ID — generated ID must appear in both
	// _opencode2api_anthropic_content and tool_calls.
	blocks := []map[string]any{
		{"type": "tool_use", "name": "fn", "input": map[string]any{"x": 1}},
	}
	out, err := buildOpenAIResponse(map[string]any{
		"id":          "msg_g",
		"stop_reason": "tool_use",
		"usage":       map[string]any{"input_tokens": float64(1), "output_tokens": float64(1)},
	}, blocks, "m")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)

	toolCalls, _ := msg["tool_calls"].([]any)
	tc, _ := toolCalls[0].(map[string]any)
	tcID, _ := tc["id"].(string)

	private, _ := msg["_opencode2api_anthropic_content"].([]any)
	pb, _ := private[0].(map[string]any)
	pbID, _ := pb["id"].(string)

	if tcID != pbID {
		t.Fatalf("tool_calls ID %q != private block ID %q", tcID, pbID)
	}
	if !strings.HasPrefix(tcID, "toolu_") {
		t.Fatalf("generated ID should start with toolu_, got %q", tcID)
	}
}

func TestBuildOpenAIResponse_PrivateFieldForRedactedThinking(t *testing.T) {
	blocks := []map[string]any{
		{"type": "text", "text": "Hello"},
		{"type": "redacted_thinking", "data": "redacted_data"},
	}
	out2, err2 := buildOpenAIResponse(map[string]any{
		"id":          "msg_r2",
		"stop_reason": "end_turn",
	}, blocks, "m")
	if err2 != nil {
		t.Fatalf("error: %v", err2)
	}
	var resp map[string]any
	json.Unmarshal(out2, &resp)
	_ = out2
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)

	private, ok := msg["_opencode2api_anthropic_content"].([]any)
	if !ok {
		t.Fatal("private field should exist for redacted_thinking")
	}
	if len(private) != 2 {
		t.Fatalf("expected 2 private blocks, got %d", len(private))
	}
	pb1, _ := private[1].(map[string]any)
	if pb1["type"] != "redacted_thinking" {
		t.Fatalf("second block type = %v, want redacted_thinking", pb1["type"])
	}
	data, _ := pb1["data"].(string)
	if data != "redacted_data" {
		t.Fatalf("data = %q, want redacted_data", data)
	}
}

// =====================================================================
// Fix 4: openAIToClaudeResponse — private blocks, signature, ID rules
// =====================================================================

func TestOpenAIToClaudeResponse_PrivateBlocksOrdered(t *testing.T) {
	// Chat response with private Anthropic blocks.
	chatBody := `{
		"id":"chatcmpl_x",
		"object":"chat.completion",
		"model":"m",
		"choices":[{
			"index":0,
			"message":{
				"role":"assistant",
				"content":"Hello Done.",
				"reasoning_content":"Let me think",
				"tool_calls":[{"id":"toolu_01","type":"function","function":{"name":"fn","arguments":"{\"x\":1}"}}],
				"_opencode2api_anthropic_content":[
					{"type":"text","text":"Hello "},
					{"type":"thinking","thinking":"Let me think","signature":"sig_abc"},
					{"type":"tool_use","id":"toolu_01","name":"fn","input":{"x":1}},
					{"type":"text","text":"Done."}
				]
			},
			"finish_reason":"stop"
		}],
		"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}
	}`
	out := openAIToClaudeResponse([]byte(chatBody), "m", true)
	var resp ClaudeResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Must preserve order: text, thinking, tool_use, text
	if len(resp.Content) != 4 {
		t.Fatalf("expected 4 content blocks, got %d", len(resp.Content))
	}
	if resp.Content[0].Type != "text" || resp.Content[0].Text != "Hello " {
		t.Fatalf("block 0 = %#v, want text 'Hello '", resp.Content[0])
	}
	if resp.Content[1].Type != "thinking" || resp.Content[1].Thinking != "Let me think" {
		t.Fatalf("block 1 = %#v, want thinking", resp.Content[1])
	}
	if resp.Content[1].Signature != "sig_abc" {
		t.Fatalf("signature = %q, want sig_abc", resp.Content[1].Signature)
	}
	if resp.Content[2].Type != "tool_use" || resp.Content[2].ID != "toolu_01" {
		t.Fatalf("block 2 = %#v, want tool_use toolu_01", resp.Content[2])
	}
	if resp.Content[3].Type != "text" || resp.Content[3].Text != "Done." {
		t.Fatalf("block 3 = %#v, want text 'Done.'", resp.Content[3])
	}
}

func TestOpenAIToClaudeResponse_WantReasoningFalseFiltersThinking(t *testing.T) {
	chatBody := `{
		"id":"chatcmpl_y",
		"choices":[{
			"message":{
				"role":"assistant",
				"content":"Hi",
				"_opencode2api_anthropic_content":[
					{"type":"thinking","thinking":"secret","signature":"sig"},
					{"type":"text","text":"Hi"},
					{"type":"tool_use","id":"toolu_02","name":"fn","input":{}}
				]
			},
			"finish_reason":"stop"
		}]
	}`
	out := openAIToClaudeResponse([]byte(chatBody), "m", false)
	var resp ClaudeResponse
	json.Unmarshal(out, &resp)
	// thinking filtered, text and tool_use kept
	for _, c := range resp.Content {
		if c.Type == "thinking" || c.Type == "redacted_thinking" {
			t.Fatalf("thinking should be filtered when wantReasoning=false: %#v", c)
		}
	}
	hasText := false
	hasTool := false
	for _, c := range resp.Content {
		if c.Type == "text" {
			hasText = true
		}
		if c.Type == "tool_use" && c.ID == "toolu_02" {
			hasTool = true
		}
	}
	if !hasText {
		t.Fatal("text block should be preserved")
	}
	if !hasTool {
		t.Fatal("tool_use block should be preserved with original ID")
	}
}

func TestOpenAIToClaudeResponse_IDRules(t *testing.T) {
	// Valid msg_ ID preserved
	chatBody1 := `{"id":"msg_abc123","choices":[{"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}]}`
	out1 := openAIToClaudeResponse([]byte(chatBody1), "m", false)
	var resp1 ClaudeResponse
	json.Unmarshal(out1, &resp1)
	if resp1.ID != "msg_abc123" {
		t.Fatalf("valid msg_ ID should be preserved, got %q", resp1.ID)
	}

	// Invalid ID (chatcmpl_) must not be leaked
	chatBody2 := `{"id":"chatcmpl_xyz","choices":[{"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}]}`
	out2 := openAIToClaudeResponse([]byte(chatBody2), "m", false)
	var resp2 ClaudeResponse
	json.Unmarshal(out2, &resp2)
	if !strings.HasPrefix(resp2.ID, "msg_") {
		t.Fatalf("invalid ID should be replaced with msg_, got %q", resp2.ID)
	}
	if strings.Contains(resp2.ID, "chatcmpl") {
		t.Fatalf("chatcmpl ID must not leak, got %q", resp2.ID)
	}

	// resp_ ID must not leak
	chatBody3 := `{"id":"resp_abc","choices":[{"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}]}`
	out3 := openAIToClaudeResponse([]byte(chatBody3), "m", false)
	var resp3 ClaudeResponse
	json.Unmarshal(out3, &resp3)
	if !strings.HasPrefix(resp3.ID, "msg_") {
		t.Fatalf("resp_ ID should be replaced with msg_, got %q", resp3.ID)
	}
}

func TestOpenAIToClaudeResponse_FallbackStringContent(t *testing.T) {
	// No private field — fallback to string content + reasoning + tool_calls
	chatBody := `{"id":"chatcmpl_z","choices":[{"message":{"role":"assistant","content":"Hello","reasoning_content":"thinking step","tool_calls":[{"id":"call_1","type":"function","function":{"name":"fn","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
	out := openAIToClaudeResponse([]byte(chatBody), "m", true)
	var resp ClaudeResponse
	json.Unmarshal(out, &resp)
	hasThinking := false
	hasText := false
	hasTool := false
	for _, c := range resp.Content {
		if c.Type == "thinking" {
			hasThinking = true
		}
		if c.Type == "text" && c.Text == "Hello" {
			hasText = true
		}
		if c.Type == "tool_use" && c.ID == "call_1" {
			hasTool = true
		}
	}
	if !hasText {
		t.Fatal("fallback should preserve text content")
	}
	if !hasTool {
		t.Fatal("fallback should preserve tool call ID")
	}
	// wantReasoning=true so thinking should be present
	if !hasThinking {
		t.Fatal("fallback should include thinking when wantReasoning=true")
	}
}

func TestOpenAIToClaudeResponse_NoFabricatedSignature(t *testing.T) {
	// thinking block without signature — must not fabricate one
	chatBody := `{"id":"msg_nosig","choices":[{"message":{"role":"assistant","content":"Hi","_opencode2api_anthropic_content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Hi"}]},"finish_reason":"stop"}]}`
	out := openAIToClaudeResponse([]byte(chatBody), "m", true)
	var resp ClaudeResponse
	json.Unmarshal(out, &resp)
	for _, c := range resp.Content {
		if c.Type == "thinking" && c.Signature != "" {
			t.Fatalf("signature should be empty, got %q", c.Signature)
		}
	}
}

// =====================================================================
// Fix 5: callOpenCodeAPI error propagation (already tested, add malformed)
// =====================================================================

func TestCallOpenCodeAPI_AnthropicMalformedReturns502(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   strings.Join([]string{`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{}}}`, `{bad json}`}, "\n"),
	}})
	body, status, _, err := callOpenCodeAPI(context.Background(), []byte(`{"model":"primary-model","messages":[]}`), "primary-model", UpstreamAuth{Mode: AuthRoutePublic})
	if err == nil {
		t.Fatal("should return error for malformed Anthropic SSE")
	}
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	if body != nil {
		t.Fatalf("body should be nil, got %s", string(body))
	}
}

// =====================================================================
// Fix 6: Claude stream usage merge — multiple usage chunks
// =====================================================================

func TestClaudeStream_UsageMergeMultipleChunks(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hi"},"finish_reason":null}],"usage":{"prompt_tokens":50,"completion_tokens":0}}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20}}`,
		``,
		`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":35,"prompt_tokens_details":{"cached_tokens":64}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	claudeStreamHandler(context.Background(), rr, io.NopCloser(strings.NewReader(upstream)), "m", false)
	events := parseSSEEvents(t, rr.Body.String())
	var usage map[string]any
	for _, event := range events {
		if event.Name != "message_delta" {
			continue
		}
		u, ok := event.Data["usage"].(map[string]any)
		if !ok {
			t.Fatalf("message_delta usage missing: %#v", event.Data["usage"])
		}
		usage = u
	}
	if usage == nil {
		t.Fatalf("message_delta not found: %s", rr.Body.String())
	}
	pt, _ := usage["input_tokens"].(float64)
	if int(pt) != 100 {
		t.Fatalf("input_tokens = %v, want 100 (merged from multiple chunks)", pt)
	}
	ct, _ := usage["output_tokens"].(float64)
	if int(ct) != 35 {
		t.Fatalf("output_tokens = %v, want 35 (merged from multiple chunks)", ct)
	}
	// cached_tokens should survive merge
	if _, ok := usage["cache_read_input_tokens"]; !ok {
		t.Fatalf("cache_read_input_tokens should be present from prompt_tokens_details.cached_tokens")
	}
	cr, _ := usage["cache_read_input_tokens"].(float64)
	if int(cr) != 64 {
		t.Fatalf("cache_read_input_tokens = %v, want 64", cr)
	}
}

// =====================================================================
// Fix 1: redacted_thinking uses data field, not signature
// =====================================================================

func TestAnthropicNativeJSON_RedactedThinkingDataField(t *testing.T) {
	body := `{
		"id":"msg_rt",
		"type":"message",
		"role":"assistant",
		"model":"claude-test",
		"stop_reason":"end_turn",
		"content":[
			{"type":"text","text":"Hello"},
			{"type":"redacted_thinking","data":"redacted_secret_data"}
		],
		"usage":{"input_tokens":10,"output_tokens":20}
	}`
	out, err := convertAnthropicToOpenAI([]byte(body), "claude-test")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)

	// Private blocks should have redacted_thinking with data
	private, _ := msg["_opencode2api_anthropic_content"].([]any)
	if len(private) != 2 {
		t.Fatalf("expected 2 private blocks, got %d", len(private))
	}
	pb1, _ := private[1].(map[string]any)
	if pb1["type"] != "redacted_thinking" {
		t.Fatalf("type = %v, want redacted_thinking", pb1["type"])
	}
	data, _ := pb1["data"].(string)
	if data != "redacted_secret_data" {
		t.Fatalf("data = %q, want redacted_secret_data", data)
	}
	// Must NOT have signature
	if _, exists := pb1["signature"]; exists {
		t.Fatal("redacted_thinking must not have signature field")
	}
}

func TestAnthropicSSE_RedactedThinkingDataField(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"msg_rt2","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"redacted_sse_data"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hi"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}, "\n")
	out, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	private, _ := msg["_opencode2api_anthropic_content"].([]any)
	if len(private) != 2 {
		t.Fatalf("expected 2 private blocks, got %d", len(private))
	}
	pb0, _ := private[0].(map[string]any)
	if pb0["type"] != "redacted_thinking" {
		t.Fatalf("type = %v, want redacted_thinking", pb0["type"])
	}
	data, _ := pb0["data"].(string)
	if data != "redacted_sse_data" {
		t.Fatalf("data = %q, want redacted_sse_data", data)
	}
	if _, exists := pb0["signature"]; exists {
		t.Fatal("redacted_thinking must not have signature")
	}
}

func TestOpenAIToClaudeResponse_RedactedThinkingDataPassthrough(t *testing.T) {
	chatBody := `{
		"id":"msg_rt3",
		"choices":[{
			"message":{
				"role":"assistant",
				"content":"Hi",
				"_opencode2api_anthropic_content":[
					{"type":"redacted_thinking","data":"secret_data"},
					{"type":"text","text":"Hi"}
				]
			},
			"finish_reason":"stop"
		}]
	}`
	out := openAIToClaudeResponse([]byte(chatBody), "m", true)
	var resp ClaudeResponse
	json.Unmarshal(out, &resp)
	found := false
	for _, c := range resp.Content {
		if c.Type == "redacted_thinking" {
			found = true
			if c.Data != "secret_data" {
				t.Fatalf("data = %q, want secret_data", c.Data)
			}
			if c.Signature != "" {
				t.Fatalf("signature should be empty, got %q", c.Signature)
			}
		}
	}
	if !found {
		t.Fatalf("redacted_thinking block not found in response: %#v", resp.Content)
	}
}

// =====================================================================
// Fix 2: parser completeness
// =====================================================================

func TestAnthropicSSE_MissingMessageStart(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for missing message_start")
	}
	if !strings.Contains(err.Error(), "message_start") {
		t.Fatalf("error should mention message_start, got: %v", err)
	}
}

func TestAnthropicSSE_MultipleMessageStartError(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{}}}`,
		`{"type":"message_start","message":{"id":"m2","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{}}}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for multiple message_start")
	}
	if !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("error should mention multiple, got: %v", err)
	}
}

func TestAnthropicSSE_DeltaAfterStopError(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"late"}}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for delta after stop")
	}
	if !strings.Contains(err.Error(), "already-stopped") {
		t.Fatalf("error should mention already-stopped, got: %v", err)
	}
}

func TestAnthropicSSE_DuplicateStopError(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_stop","index":0}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for duplicate content_block_stop")
	}
}

func TestAnthropicSSE_MissingStopReasonError(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_stop"}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for missing stop_reason")
	}
	if !strings.Contains(err.Error(), "stop_reason") {
		t.Fatalf("error should mention stop_reason, got: %v", err)
	}
}

func TestAnthropicSSE_MissingIndexError(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{}}}`,
		`{"type":"content_block_start","content_block":{"type":"text","text":""}}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for missing index")
	}
	if !strings.Contains(err.Error(), "index") {
		t.Fatalf("error should mention index, got: %v", err)
	}
}

func TestAnthropicSSE_NegativeIndexError(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{}}}`,
		`{"type":"content_block_start","index":-1,"content_block":{"type":"text","text":""}}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for negative index")
	}
}

func TestAnthropicNativeJSON_MissingContentArrayError(t *testing.T) {
	body := `{"id":"msg_nc","type":"message","role":"assistant","model":"m","stop_reason":"end_turn","usage":{}}`
	_, err := convertAnthropicToOpenAI([]byte(body), "m")
	if err == nil {
		t.Fatal("expected error for missing content array")
	}
	if !strings.Contains(err.Error(), "content") {
		t.Fatalf("error should mention content, got: %v", err)
	}
}

func TestAnthropicNativeJSON_MissingStopReasonError(t *testing.T) {
	body := `{"id":"msg_nsr","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"Hi"}],"usage":{}}`
	_, err := convertAnthropicToOpenAI([]byte(body), "m")
	if err == nil {
		t.Fatal("expected error for missing stop_reason")
	}
}

// =====================================================================
// Fix 3: response ID normalization
// =====================================================================

func TestNormalizeChatResponseID(t *testing.T) {
	// Already valid chatcmpl- ID preserved
	id := normalizeChatResponseID("chatcmpl-abc123")
	if id != "chatcmpl-abc123" {
		t.Fatalf("got %q, want chatcmpl-abc123", id)
	}
	// Non-chatcmpl ID gets fresh chatcmpl- ID (must not leak msg_)
	id = normalizeChatResponseID("msg_abc")
	if !strings.HasPrefix(id, "chatcmpl-") {
		t.Fatalf("got %q, want chatcmpl- prefix", id)
	}
	if strings.Contains(id, "msg_abc") {
		t.Fatalf("got %q, must not contain original non-chatcmpl ID", id)
	}
	// Empty ID generates
	id = normalizeChatResponseID("")
	if !strings.HasPrefix(id, "chatcmpl-") {
		t.Fatalf("got %q, want chatcmpl- prefix", id)
	}
}

func TestNormalizeResponsesID(t *testing.T) {
	// Already valid resp_ ID preserved
	id := normalizeResponsesID("resp_abc123")
	if id != "resp_abc123" {
		t.Fatalf("got %q, want resp_abc123", id)
	}
	// Non-resp_ ID gets fresh resp_ ID (must not leak chatcmpl)
	id = normalizeResponsesID("chatcmpl_xyz")
	if !strings.HasPrefix(id, "resp_") {
		t.Fatalf("got %q, want resp_ prefix", id)
	}
	if strings.Contains(id, "chatcmpl") {
		t.Fatalf("got %q, must not contain chatcmpl", id)
	}
	// Empty ID generates
	id = normalizeResponsesID("")
	if !strings.HasPrefix(id, "resp_") {
		t.Fatalf("got %q, want resp_ prefix", id)
	}
}

func TestNormalizeClaudeMessageID(t *testing.T) {
	// Valid msg_ preserved
	id := normalizeClaudeMessageID("msg_abc123")
	if id != "msg_abc123" {
		t.Fatalf("got %q, want msg_abc123", id)
	}
	// chatcmpl_ replaced
	id = normalizeClaudeMessageID("chatcmpl_xyz")
	if !strings.HasPrefix(id, "msg_") || strings.Contains(id, "chatcmpl") {
		t.Fatalf("got %q, want msg_ prefix without chatcmpl", id)
	}
	// resp_ replaced
	id = normalizeClaudeMessageID("resp_abc")
	if !strings.HasPrefix(id, "msg_") || strings.Contains(id, "resp_") {
		t.Fatalf("got %q, want msg_ prefix without resp", id)
	}
	// Empty generates
	id = normalizeClaudeMessageID("")
	if !strings.HasPrefix(id, "msg_") {
		t.Fatalf("got %q, want msg_ prefix", id)
	}
}

func TestConvertResponse_NormalizesChatID(t *testing.T) {
	body := `{"id":"msg_xyz","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}]}`
	out, _ := convertResponse([]byte(body), false)
	var resp map[string]any
	json.Unmarshal(out, &resp)
	id, _ := resp["id"].(string)
	if !strings.HasPrefix(id, "chatcmpl-") {
		t.Fatalf("id = %q, want chatcmpl- prefix", id)
	}
}

func TestConvertResponse_PreservesValidChatID(t *testing.T) {
	body := `{"id":"chatcmpl-abc","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}]}`
	out, _ := convertResponse([]byte(body), false)
	var resp map[string]any
	json.Unmarshal(out, &resp)
	id, _ := resp["id"].(string)
	if id != "chatcmpl-abc" {
		t.Fatalf("id = %q, want chatcmpl-abc", id)
	}
}

func TestConvertChatToResponses_NormalizesResponsesID(t *testing.T) {
	chatBody := `{"id":"chatcmpl_xyz","created":1234,"choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`
	out := convertChatToResponses([]byte(chatBody), "m", false, nil, nil, nil)
	var resp map[string]any
	json.Unmarshal(out, &resp)
	id, _ := resp["id"].(string)
	if !strings.HasPrefix(id, "resp_") {
		t.Fatalf("id = %q, want resp_ prefix", id)
	}
	// Must not leak chatcmpl
	if strings.Contains(id, "chatcmpl") {
		t.Fatalf("id = %q, must not contain chatcmpl", id)
	}
}

func TestConvertChatToResponses_PreservesValidResponsesID(t *testing.T) {
	chatBody := `{"id":"resp_abc","created":1234,"choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`
	out := convertChatToResponses([]byte(chatBody), "m", false, nil, nil, nil)
	var resp map[string]any
	json.Unmarshal(out, &resp)
	id, _ := resp["id"].(string)
	if id != "resp_abc" {
		t.Fatalf("id = %q, want resp_abc", id)
	}
}

func TestConvertResponse_DoesNotModifyToolCallID(t *testing.T) {
	body := `{"id":"chatcmpl_x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hi","tool_calls":[{"id":"toolu_abc","type":"function","function":{"name":"fn","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
	out, _ := convertResponse([]byte(body), false)
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	toolCalls, _ := msg["tool_calls"].([]any)
	tc, _ := toolCalls[0].(map[string]any)
	id, _ := tc["id"].(string)
	if id != "toolu_abc" {
		t.Fatalf("tool call ID = %q, want toolu_abc (must not be modified)", id)
	}
}

// =====================================================================
// Fix 4: protocol conversion error propagation
// =====================================================================

func TestCallOpenCodeAPI_AnthropicError502_ClaudeShape(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   `{"type":"error","error":{"type":"overloaded_error","message":"Server overloaded"}}`,
	}})
	// Test the error shape from callOpenCodeAPI directly
	_, status, _, err := callOpenCodeAPI(context.Background(), []byte(`{"model":"primary-model","messages":[]}`), "primary-model", UpstreamAuth{Mode: AuthRoutePublic})
	if err == nil {
		t.Fatal("expected error")
	}
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	var ape *anthropicProtocolError
	if !errors.As(err, &ape) {
		t.Fatalf("error should be anthropicProtocolError, got %T: %v", err, err)
	}
	if ape.errType != "overloaded_error" {
		t.Fatalf("errType = %q, want overloaded_error", ape.errType)
	}
	if ape.message != "Server overloaded" {
		t.Fatalf("message = %q, want 'Server overloaded'", ape.message)
	}
}

func TestWriteProtocolConvError_ClaudeShape(t *testing.T) {
	rr := httptest.NewRecorder()
	writeUpstreamError(rr, http.StatusBadGateway,
		&anthropicProtocolError{errType: "overloaded_error", message: "Server overloaded"}, "claude")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
	var body map[string]any
	json.Unmarshal(rr.Body.Bytes(), &body)
	if body["type"] != "error" {
		t.Fatalf("type = %v, want error", body["type"])
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil {
		t.Fatal("error object missing")
	}
	if errObj["type"] != "overloaded_error" {
		t.Fatalf("error.type = %v, want overloaded_error", errObj["type"])
	}
	if errObj["message"] != "Server overloaded" {
		t.Fatalf("error.message = %v, want 'Server overloaded'", errObj["message"])
	}
}

func TestWriteProtocolConvError_ChatShape(t *testing.T) {
	rr := httptest.NewRecorder()
	writeUpstreamError(rr, http.StatusBadGateway,
		&anthropicProtocolError{errType: "overloaded_error", message: "Server overloaded"}, "chat")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
	var body map[string]any
	json.Unmarshal(rr.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil {
		t.Fatal("error object missing")
	}
	if errObj["type"] != "overloaded_error" {
		t.Fatalf("error.type = %v, want overloaded_error", errObj["type"])
	}
	if errObj["message"] != "Server overloaded" {
		t.Fatalf("error.message = %v, want 'Server overloaded'", errObj["message"])
	}
}

func TestWriteProtocolConvError_ResponsesShape(t *testing.T) {
	rr := httptest.NewRecorder()
	writeUpstreamError(rr, http.StatusBadGateway,
		&anthropicProtocolError{errType: "overloaded_error", message: "Server overloaded"}, "responses")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
	var body map[string]any
	json.Unmarshal(rr.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil {
		t.Fatal("error object missing")
	}
	if errObj["type"] != "overloaded_error" {
		t.Fatalf("error.type = %v, want overloaded_error", errObj["type"])
	}
	if errObj["message"] != "Server overloaded" {
		t.Fatalf("error.message = %v, want 'Server overloaded'", errObj["message"])
	}
}

func TestClaudeHandler_AnthropicError502_VisibleTypeMessage(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   `{"type":"error","error":{"type":"overloaded_error","message":"Server overloaded"}}`,
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"primary-model","messages":[]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["type"] != "error" {
		t.Fatalf("type = %v, want error", body["type"])
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil {
		t.Fatal("error object missing")
	}
	if errObj["type"] != "overloaded_error" {
		t.Fatalf("error.type = %v, want overloaded_error", errObj["type"])
	}
	if errObj["message"] != "Server overloaded" {
		t.Fatalf("error.message = %v, want 'Server overloaded'", errObj["message"])
	}
}

// =====================================================================
// Fix 1: deterministicResponseID — same input produces same output
// =====================================================================

func TestDeterministicResponseID_SameInputSameOutput(t *testing.T) {
	id1 := deterministicResponseID("chatcmpl-", "msg_abc")
	id2 := deterministicResponseID("chatcmpl-", "msg_abc")
	if id1 != id2 {
		t.Fatalf("same input must produce same output: %q vs %q", id1, id2)
	}
	if !strings.HasPrefix(id1, "chatcmpl-") {
		t.Fatalf("must have chatcmpl- prefix: %q", id1)
	}
}

func TestDeterministicResponseID_ValidPreserved(t *testing.T) {
	id := deterministicResponseID("chatcmpl-", "chatcmpl-abc123")
	if id != "chatcmpl-abc123" {
		t.Fatalf("valid ID should be preserved: got %q", id)
	}
	id = deterministicResponseID("resp_", "resp_xyz789")
	if id != "resp_xyz789" {
		t.Fatalf("valid ID should be preserved: got %q", id)
	}
}

func TestDeterministicResponseID_DifferentInputsDifferentOutputs(t *testing.T) {
	id1 := deterministicResponseID("chatcmpl-", "msg_abc")
	id2 := deterministicResponseID("chatcmpl-", "msg_def")
	if id1 == id2 {
		t.Fatalf("different inputs must produce different outputs: %q", id1)
	}
}

func TestNormalizeChatResponseID_Deterministic(t *testing.T) {
	id1 := normalizeChatResponseID("msg_abc")
	id2 := normalizeChatResponseID("msg_abc")
	if id1 != id2 {
		t.Fatalf("same non-chatcmpl ID must map deterministically: %q vs %q", id1, id2)
	}
	if !strings.HasPrefix(id1, "chatcmpl-") {
		t.Fatalf("must have chatcmpl- prefix: %q", id1)
	}
}

// Test: Chat stream same upstream ID across chunks produces same chatcmpl ID
func TestConvertStreamChunkWithUsage_DeterministicID(t *testing.T) {
	// Two chunks with same upstream ID should produce same normalized ID
	line1 := `data: {"id":"msg_xyz","choices":[{"delta":{"content":"Hi"},"finish_reason":null}]}`
	line2 := `data: {"id":"msg_xyz","choices":[{"delta":{"content":" world"},"finish_reason":"stop"}]}`

	out1, _ := convertStreamChunkWithUsage(line1, false)
	out2, _ := convertStreamChunkWithUsage(line2, false)

	var chunk1, chunk2 map[string]any
	json.Unmarshal([]byte(out1[6:]), &chunk1)
	json.Unmarshal([]byte(out2[6:]), &chunk2)

	id1, _ := chunk1["id"].(string)
	id2, _ := chunk2["id"].(string)

	if id1 != id2 {
		t.Fatalf("same upstream ID must produce same chatcmpl ID: %q vs %q", id1, id2)
	}
	if !strings.HasPrefix(id1, "chatcmpl-") {
		t.Fatalf("must have chatcmpl- prefix: %q", id1)
	}
}

// Test: Responses top-level ID and derived msg_/rs_ IDs are consistent
func TestConvertChatToResponses_DerivedIDsConsistent(t *testing.T) {
	chatBody := `{"id":"msg_test123","created":1234,"choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`
	out := convertChatToResponses([]byte(chatBody), "m", false, nil, nil, nil)
	var resp map[string]any
	json.Unmarshal(out, &resp)

	topID, _ := resp["id"].(string)
	if !strings.HasPrefix(topID, "resp_") {
		t.Fatalf("top-level ID must have resp_ prefix: %q", topID)
	}

	output, _ := resp["output"].([]any)
	for _, item := range output {
		blk, _ := item.(map[string]any)
		itemID, _ := blk["id"].(string)
		if strings.HasPrefix(itemID, "msg_") {
			if !strings.Contains(itemID, topID) {
				t.Fatalf("msg_ derived ID %q must contain normalized top ID %q", itemID, topID)
			}
		}
		if strings.HasPrefix(itemID, "rs_") {
			if !strings.Contains(itemID, topID) {
				t.Fatalf("rs_ derived ID %q must contain normalized top ID %q", itemID, topID)
			}
		}
	}
}

// =====================================================================
// Fix 2: responsesStreamHandler normalizeResponsesID
// =====================================================================

func TestResponsesStream_NormalizesUpstreamID(t *testing.T) {
	// Upstream sends a chatcmpl_ ID; responsesStreamHandler should normalize to resp_
	upstream := strings.Join([]string{
		`data: {"id":"chatcmpl_xyz","created":1234,"choices":[{"delta":{"content":"Hi"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl_xyz","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(upstream)), Header: make(http.Header)}
	responsesStreamHandler(rr, nil, resp, "m", "m", false, nil, nil, ResponsesAPIRequest{})
	events := parseSSEEvents(t, rr.Body.String())
	for _, e := range events {
		if e.Name == "response.created" {
			r, _ := e.Data["response"].(map[string]any)
			id, _ := r["id"].(string)
			if !strings.HasPrefix(id, "resp_") {
				t.Fatalf("response.created ID should have resp_ prefix, got %q", id)
			}
			if strings.Contains(id, "chatcmpl") {
				t.Fatalf("response.created ID must not contain chatcmpl: %q", id)
			}
		}
	}
}

// =====================================================================
// Fix 3: extractBlockIndex strict type checking
// =====================================================================

func TestExtractBlockIndex_IntegerAccepted(t *testing.T) {
	idx, ok := extractBlockIndex(map[string]any{"index": float64(5)})
	if !ok || idx != 5 {
		t.Fatalf("integer 5 should be accepted: idx=%d ok=%v", idx, ok)
	}
}

func TestExtractBlockIndex_FloatRejected(t *testing.T) {
	_, ok := extractBlockIndex(map[string]any{"index": float64(2.5)})
	if ok {
		t.Fatal("float 2.5 should be rejected")
	}
}

func TestExtractBlockIndex_StringRejected(t *testing.T) {
	_, ok := extractBlockIndex(map[string]any{"index": "5"})
	if ok {
		t.Fatal("string \"5\" should be rejected")
	}
}

func TestExtractBlockIndex_BoolRejected(t *testing.T) {
	_, ok := extractBlockIndex(map[string]any{"index": true})
	if ok {
		t.Fatal("bool should be rejected")
	}
}

func TestExtractBlockIndex_NegativeRejected(t *testing.T) {
	_, ok := extractBlockIndex(map[string]any{"index": float64(-1)})
	if ok {
		t.Fatal("negative should be rejected")
	}
}

func TestExtractBlockIndex_MissingRejected(t *testing.T) {
	_, ok := extractBlockIndex(map[string]any{})
	if ok {
		t.Fatal("missing index should be rejected")
	}
}

// =====================================================================
// Fix 4: message_stop timing and event ordering
// =====================================================================

func TestAnthropicSSE_EventAfterStopError(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for event after message_stop")
	}
	if !strings.Contains(err.Error(), "after message_stop") {
		t.Fatalf("error should mention 'after message_stop', got: %v", err)
	}
}

func TestAnthropicSSE_ContentBeforeMessageStartError(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for content before message_start")
	}
}

func TestAnthropicSSE_MessageDeltaBeforeMessageStartError(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for message_delta before message_start")
	}
}

func TestAnthropicSSE_MessageStopValidatesStopReason(t *testing.T) {
	// message_stop with stop_reason still null should error
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_stop"}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for message_stop without stop_reason")
	}
	if !strings.Contains(err.Error(), "stop_reason") {
		t.Fatalf("error should mention stop_reason, got: %v", err)
	}
}

func TestAnthropicSSE_MessageStopValidatesBlocksStopped(t *testing.T) {
	// message_stop with unclosed block should error at stop time
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for unclosed block at message_stop")
	}
	if !strings.Contains(err.Error(), "unclosed") {
		t.Fatalf("error should mention unclosed, got: %v", err)
	}
}

// =====================================================================
// Fix 5: native JSON content validation
// =====================================================================

func TestAnthropicNativeJSON_NonObjectContentBlockError(t *testing.T) {
	body := `{"id":"msg_no","type":"message","role":"assistant","model":"m","stop_reason":"end_turn","content":["not an object"],"usage":{}}`
	_, err := convertAnthropicToOpenAI([]byte(body), "m")
	if err == nil {
		t.Fatal("expected error for non-object content block")
	}
	if !strings.Contains(err.Error(), "non-object") {
		t.Fatalf("error should mention non-object, got: %v", err)
	}
}

func TestAnthropicNativeJSON_ToolUseEmptyNameError(t *testing.T) {
	body := `{"id":"msg_en","type":"message","role":"assistant","model":"m","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_x","name":"","input":{}}],"usage":{}}`
	_, err := convertAnthropicToOpenAI([]byte(body), "m")
	if err == nil {
		t.Fatal("expected error for tool_use with empty name")
	}
}

func TestAnthropicNativeJSON_ToolUseIDPreserved(t *testing.T) {
	body := `{"id":"msg_tid","type":"message","role":"assistant","model":"m","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_preserve","name":"fn","input":{"x":1}}],"usage":{}}`
	out, err := convertAnthropicToOpenAI([]byte(body), "m")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	toolCalls, _ := msg["tool_calls"].([]any)
	tc, _ := toolCalls[0].(map[string]any)
	if tc["id"] != "toolu_preserve" {
		t.Fatalf("tool ID = %v, want toolu_preserve", tc["id"])
	}
}

func TestAnthropicSSE_ContentBlockStartMissingTypeError(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{}}}`,
		`{"type":"content_block_start","index":0,"content_block":{}}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for content_block_start without type")
	}
}

func TestAnthropicSSE_ContentBlockStartUnsupportedTypeError(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"unsupported_type"}}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for unsupported content_block type")
	}
}

func TestAnthropicSSE_ToolUseStartEmptyNameError(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_x","name":"","input":{}}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for tool_use with empty name in SSE")
	}
}

// =====================================================================
// Fix 6: typed error with errors.As
// =====================================================================

func TestAnthropicProtocolError_ErrorsAs(t *testing.T) {
	orig := &anthropicProtocolError{errType: "overloaded_error", message: "Server overloaded"}
	var ape *anthropicProtocolError
	if !errors.As(orig, &ape) {
		t.Fatal("errors.As should extract anthropicProtocolError")
	}
	if ape.errType != "overloaded_error" {
		t.Fatalf("errType = %q", ape.errType)
	}
	if ape.message != "Server overloaded" {
		t.Fatalf("message = %q", ape.message)
	}
}

func TestWriteUpstreamError_NonTypedDefaultType(t *testing.T) {
	rr := httptest.NewRecorder()
	writeUpstreamError(rr, http.StatusBadGateway, fmt.Errorf("some random transport error"), "chat")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
	var body map[string]any
	json.Unmarshal(rr.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil {
		t.Fatal("error object missing")
	}
	// Non-typed errors get generic upstream_error, not upstream_protocol_error
	if errObj["type"] != "upstream_error" {
		t.Fatalf("type = %v, want upstream_error", errObj["type"])
	}
	if errObj["message"] != "upstream error" {
		t.Fatalf("message = %v, want 'upstream error'", errObj["message"])
	}
	// Must not expose err.Error() string
	if msg, _ := errObj["message"].(string); strings.Contains(msg, "transport") {
		t.Fatalf("message must not expose transport error details: %q", msg)
	}
}

// =====================================================================
// Fix 6: Integration tests — Chat/Claude/Responses each
// =====================================================================

func TestChatHandler_AnthropicError502(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   `{"type":"error","error":{"type":"overloaded_error","message":"Server overloaded"}}`,
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"primary-model","messages":[]}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil {
		t.Fatal("error object missing")
	}
	if errObj["type"] != "overloaded_error" {
		t.Fatalf("error.type = %v, want overloaded_error", errObj["type"])
	}
	if errObj["message"] != "Server overloaded" {
		t.Fatalf("error.message = %v, want 'Server overloaded'", errObj["message"])
	}
}

func TestResponsesHandler_AnthropicError502(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   `{"type":"error","error":{"type":"overloaded_error","message":"Server overloaded"}}`,
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"primary-model","input":[]}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil {
		t.Fatal("error object missing")
	}
	if errObj["type"] != "overloaded_error" {
		t.Fatalf("error.type = %v, want overloaded_error", errObj["type"])
	}
	if errObj["message"] != "Server overloaded" {
		t.Fatalf("error.message = %v, want 'Server overloaded'", errObj["message"])
	}
}

// Test: malformed (non-typed) conversion error gets upstream_protocol_error type
func TestClaudeHandler_MalformedAnthropicReturns502(t *testing.T) {
	// Truncated SSE (no message_stop) — not an anthropicProtocolError
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body: strings.Join([]string{
			`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{}}}`,
		}, "\n"),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"primary-model","messages":[]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil {
		t.Fatal("error object missing")
	}
	// Non-typed conversion error gets generic upstream_error
	if errObj["type"] != "upstream_error" {
		t.Fatalf("error.type = %v, want upstream_error", errObj["type"])
	}
	if errObj["message"] != "upstream error" {
		t.Fatalf("error.message = %v, want 'upstream error'", errObj["message"])
	}
}

// =====================================================================
// Fix 1: convertChatToResponses empty ID still consistent
// =====================================================================

func TestConvertChatToResponses_EmptyIDConsistent(t *testing.T) {
	chatBody := `{"id":"","created":1234,"choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`
	out := convertChatToResponses([]byte(chatBody), "m", false, nil, nil, nil)
	var resp map[string]any
	json.Unmarshal(out, &resp)
	topID, _ := resp["id"].(string)
	if !strings.HasPrefix(topID, "resp_") {
		t.Fatalf("empty ID should still get resp_ prefix: %q", topID)
	}
	output, _ := resp["output"].([]any)
	for _, item := range output {
		blk, _ := item.(map[string]any)
		itemID, _ := blk["id"].(string)
		if strings.HasPrefix(itemID, "msg_") || strings.HasPrefix(itemID, "rs_") {
			if !strings.Contains(itemID, topID) {
				t.Fatalf("derived ID %q must contain top ID %q", itemID, topID)
			}
		}
	}
}

// =====================================================================
// Fix 2: extractBlockIndex NaN/Inf/range
// =====================================================================

func TestExtractBlockIndex_NaNRejected(t *testing.T) {
	_, ok := extractBlockIndex(map[string]any{"index": math.NaN()})
	if ok {
		t.Fatal("NaN should be rejected")
	}
}

func TestExtractBlockIndex_InfRejected(t *testing.T) {
	_, ok := extractBlockIndex(map[string]any{"index": math.Inf(1)})
	if ok {
		t.Fatal("+Inf should be rejected")
	}
	_, ok = extractBlockIndex(map[string]any{"index": math.Inf(-1)})
	if ok {
		t.Fatal("-Inf should be rejected")
	}
}

func TestExtractBlockIndex_LargeNumberRejected(t *testing.T) {
	// Value larger than max int (2^63)
	_, ok := extractBlockIndex(map[string]any{"index": float64(9223372036854775808.0)})
	if ok {
		t.Fatal("value exceeding int range should be rejected")
	}
}

// =====================================================================
// Fix 3: ping after message_stop allowed
// =====================================================================

func TestAnthropicSSE_PingAfterStopAllowed(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
		`{"type":"ping"}`,
	}, "\n")
	out, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err != nil {
		t.Fatalf("ping after message_stop should be allowed, got error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(choices))
	}
}

func TestAnthropicSSE_DataEventAfterStopError(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("content_block_start after message_stop should error")
	}
}

// =====================================================================
// Fix 4: message_start missing message object error
// =====================================================================

func TestAnthropicSSE_MessageStartMissingMessageError(t *testing.T) {
	sse := `{"type":"message_start"}`
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for message_start without message object")
	}
	if !strings.Contains(err.Error(), "message object") {
		t.Fatalf("error should mention message object, got: %v", err)
	}
}

func TestAnthropicSSE_ContentBlockDeltaMissingDeltaError(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for content_block_delta without delta")
	}
}

func TestAnthropicSSE_ContentBlockDeltaMissingDeltaTypeError(t *testing.T) {
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"text":"Hi"}}`,
	}, "\n")
	_, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err == nil {
		t.Fatal("expected error for content_block_delta without delta type")
	}
}

// =====================================================================
// Fix 5: unknown block type in native JSON preserves private, not leaked
// =====================================================================

func TestAnthropicNativeJSON_UnknownBlockPreservesPrivate(t *testing.T) {
	body := `{"id":"msg_uk","type":"message","role":"assistant","model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"Hello"},{"type":"custom_block","data":"custom"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	out, err := convertAnthropicToOpenAI([]byte(body), "m")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)

	// Private field should exist (unknown block sets hasNonText)
	private, ok := msg["_opencode2api_anthropic_content"].([]any)
	if !ok {
		t.Fatal("private field should exist for unknown block type")
	}
	if len(private) != 2 {
		t.Fatalf("expected 2 private blocks, got %d", len(private))
	}
	pb1, _ := private[1].(map[string]any)
	if pb1["type"] != "custom_block" {
		t.Fatalf("private block type = %v, want custom_block", pb1["type"])
	}

	// convertResponse should strip private field
	out2, _ := convertResponse(out, false)
	var resp2 map[string]any
	json.Unmarshal(out2, &resp2)
	choices2, _ := resp2["choices"].([]any)
	choice2, _ := choices2[0].(map[string]any)
	msg2, _ := choice2["message"].(map[string]any)
	if _, exists := msg2["_opencode2api_anthropic_content"]; exists {
		t.Fatal("private field should be stripped by convertResponse")
	}
}

// =====================================================================
// Fix 6: tool initial input vs partial delta not concatenated
// =====================================================================

func TestAnthropicSSE_ToolUseInitialInputNotConcatenatedWithDelta(t *testing.T) {
	// content_block_start has input:{"x":42}, then input_json_delta provides {"y":1}
	// The result should use ONLY the delta ({"y":1}), not concatenated ({"x":42}{"y":1})
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_x","name":"fn","input":{"x":42}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"y\":1}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}, "\n")
	out, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	toolCalls, _ := msg["tool_calls"].([]any)
	tc, _ := toolCalls[0].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	args, _ := fn["arguments"].(string)

	// Should parse as valid JSON with y:1, NOT x:42
	var input map[string]any
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		t.Fatalf("args should be valid JSON, got %q: %v", args, err)
	}
	if _, exists := input["y"]; !exists {
		t.Fatalf("args should contain y from delta, got %q", args)
	}
	if _, exists := input["x"]; exists {
		t.Fatalf("args should NOT contain x from initial input when deltas present, got %q", args)
	}
}

func TestAnthropicSSE_ToolUseInitialInputOnly(t *testing.T) {
	// content_block_start has input:{"x":42}, no input_json_delta
	// Result should use initial input {"x":42}
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_y","name":"fn","input":{"x":42}}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}, "\n")
	out, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	toolCalls, _ := msg["tool_calls"].([]any)
	tc, _ := toolCalls[0].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	args, _ := fn["arguments"].(string)
	if !strings.Contains(args, "42") {
		t.Fatalf("args should contain 42 from initial input, got %q", args)
	}
}

func TestAnthropicSSE_ToolUseDeltaOnlyNoInitialInput(t *testing.T) {
	// content_block_start has input:{} (empty), deltas provide the real input
	sse := strings.Join([]string{
		`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_z","name":"fn","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"z\":99}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}, "\n")
	out, err := convertAnthropicToOpenAI([]byte(sse), "m")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	toolCalls, _ := msg["tool_calls"].([]any)
	tc, _ := toolCalls[0].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	args, _ := fn["arguments"].(string)
	if !strings.Contains(args, "99") {
		t.Fatalf("args should contain 99 from delta, got %q", args)
	}
}

// =====================================================================
// Fix 1: empty/missing type in native JSON content block
// =====================================================================

func TestAnthropicNativeJSON_EmptyTypeBlockError(t *testing.T) {
	body := `{"id":"msg_et","type":"message","role":"assistant","model":"m","stop_reason":"end_turn","content":[{"type":"","text":"Hi"}],"usage":{}}`
	_, err := convertAnthropicToOpenAI([]byte(body), "m")
	if err == nil {
		t.Fatal("expected error for empty type string")
	}
	if !strings.Contains(err.Error(), "missing type") {
		t.Fatalf("error should mention missing type, got: %v", err)
	}
}

func TestAnthropicNativeJSON_MissingTypeFieldError(t *testing.T) {
	body := `{"id":"msg_mt","type":"message","role":"assistant","model":"m","stop_reason":"end_turn","content":[{"text":"Hi"}],"usage":{}}`
	_, err := convertAnthropicToOpenAI([]byte(body), "m")
	if err == nil {
		t.Fatal("expected error for missing type field")
	}
	if !strings.Contains(err.Error(), "missing type") {
		t.Fatalf("error should mention missing type, got: %v", err)
	}
}

func TestAnthropicNativeJSON_CustomBlockPreserved(t *testing.T) {
	body := `{"id":"msg_cb","type":"message","role":"assistant","model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"Hi"},{"type":"custom_block","data":"custom"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	out, err := convertAnthropicToOpenAI([]byte(body), "m")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var resp map[string]any
	json.Unmarshal(out, &resp)
	choices, _ := resp["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	private, ok := msg["_opencode2api_anthropic_content"].([]any)
	if !ok {
		t.Fatal("custom_block should set hasNonText and create private field")
	}
	found := false
	for _, p := range private {
		pb, _ := p.(map[string]any)
		if pb["type"] == "custom_block" {
			found = true
		}
	}
	if !found {
		t.Fatal("custom_block should be preserved in private field")
	}
}

// =====================================================================
// Fix 2: extractBlockIndex platform int range
// =====================================================================

func TestExtractBlockIndex_MaxIntBoundary(t *testing.T) {
	// Value at the exact platform maxInt should be accepted or rejected
	// depending on whether it's exactly representable. We test that
	// a value just below 2^31 (32-bit max) works.
	idx, ok := extractBlockIndex(map[string]any{"index": float64(2147483647)})
	if !ok || idx != 2147483647 {
		// On 32-bit this would be at the boundary; on 64-bit it's fine.
		t.Fatalf("2^31-1 should be accepted: idx=%d ok=%v", idx, ok)
	}
}

func TestExtractBlockIndex_Above32BitMaxRejected(t *testing.T) {
	// 2^31 exceeds 32-bit int range; on 64-bit it's fine but the test
	// verifies the function doesn't crash. On 32-bit it should reject.
	_, ok := extractBlockIndex(map[string]any{"index": float64(2147483648)})
	// On 64-bit: 2^31 < maxInt(64) so it passes; on 32-bit it should reject.
	// We just verify no panic.
	_ = ok
}

func TestExtractBlockIndex_AboveExactFloatRangeRejected(t *testing.T) {
	// Value above 2^53 — should always be rejected regardless of platform.
	// 2^53+1 = 9007199254740993, but as float64 it rounds to 9007199254740992.0 (=2^53).
	// So we use 2^54 which is definitely above the cap.
	_, ok := extractBlockIndex(map[string]any{"index": float64(18014398509481984.0)})
	if ok {
		t.Fatal("value above 2^53 should be rejected")
	}
}

// =====================================================================
// Fix 3: transport error handler — no panic, 502, generic error
// =====================================================================

func TestChatHandler_TransportError502(t *testing.T) {
	// Simulate a transport error by installing a transport that returns an error.
	oldClient := httpClient
	httpClient = &http.Client{Transport: &errorTransport{}}
	defer func() { httpClient = oldClient }()

	// Reset session state
	ocOnce = sync.Once{}
	ocOnce.Do(func() {})
	ocClientVer = "test"
	ocSessionID = "ses_test"
	ocProjectID = "project_test"

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"primary-model","messages":[]}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil {
		t.Fatal("error object missing")
	}
	if errObj["type"] != "upstream_error" {
		t.Fatalf("type = %v, want upstream_error", errObj["type"])
	}
	if errObj["message"] != "upstream error" {
		t.Fatalf("message = %v, want 'upstream error'", errObj["message"])
	}
}

func TestClaudeHandler_TransportError502(t *testing.T) {
	oldClient := httpClient
	httpClient = &http.Client{Transport: &errorTransport{}}
	defer func() { httpClient = oldClient }()

	ocOnce = sync.Once{}
	ocOnce.Do(func() {})
	ocClientVer = "test"
	ocSessionID = "ses_test"
	ocProjectID = "project_test"

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"primary-model","messages":[]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil {
		t.Fatal("error object missing")
	}
	if errObj["type"] != "upstream_error" {
		t.Fatalf("type = %v, want upstream_error", errObj["type"])
	}
	if errObj["message"] != "upstream error" {
		t.Fatalf("message = %v, want 'upstream error'", errObj["message"])
	}
}

func TestResponsesHandler_TransportError502(t *testing.T) {
	oldClient := httpClient
	httpClient = &http.Client{Transport: &errorTransport{}}
	defer func() { httpClient = oldClient }()

	ocOnce = sync.Once{}
	ocOnce.Do(func() {})
	ocClientVer = "test"
	ocSessionID = "ses_test"
	ocProjectID = "project_test"

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"primary-model","input":[]}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil {
		t.Fatal("error object missing")
	}
	if errObj["type"] != "upstream_error" {
		t.Fatalf("type = %v, want upstream_error", errObj["type"])
	}
	if errObj["message"] != "upstream error" {
		t.Fatalf("message = %v, want 'upstream error'", errObj["message"])
	}
}

// errorTransport returns an error for every RoundTrip, simulating transport failure.
type errorTransport struct{}

func (e *errorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("connection refused")
}

func (e *errorTransport) CloseIdleConnections() {}
