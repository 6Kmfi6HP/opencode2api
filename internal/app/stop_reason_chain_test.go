package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件锁死 stop_reason / finish_reason / status 全链的映射(对齐 Bifrost):
//   - refusal 折叠:content_filter/refusal → stop_reason="refusal",流式与非
//     流式同终态;
//   - model_context_window_exceeded → chat "length" / Responses
//     incomplete+max_output_tokens;
//   - pause_turn/compaction 表示未完结,出口不报完成态;
//   - stop_sequence 往返:输出以停止串结尾时恢复 stop_reason 与该串;
//   - 空串 stop_reason 编 null(严格客户端拒收 "")。

// TestResponsesOutcome_TerminalStatuses 非流式 Responses 出口的 status 链。
func TestResponsesOutcome_TerminalStatuses(t *testing.T) {
	cases := []struct {
		name         string
		finishReason string
		status       string // "" = status 置空不发
		details      string // incomplete_details.reason, "" = nil
	}{
		{"length", "length", "incomplete", "max_output_tokens"},
		{"model_context_window_exceeded", "model_context_window_exceeded", "incomplete", "max_output_tokens"},
		{"content_filter", "content_filter", "incomplete", "content_filter"},
		{"stop", "stop", "completed", ""},
		{"tool_calls", "tool_calls", "completed", ""},
		{"pause_turn_not_completed", "pause_turn", "", ""},
		{"compaction_not_completed", "compaction", "", ""},
	}
	for _, tc := range cases {
		out := responsesOutcome(tc.finishReason)
		if out.Status != tc.status {
			t.Fatalf("%s: status = %q, want %q", tc.name, out.Status, tc.status)
		}
		if tc.details == "" {
			if out.IncompleteDetails != nil {
				t.Fatalf("%s: incomplete_details = %#v, want nil", tc.name, out.IncompleteDetails)
			}
			continue
		}
		details, _ := out.IncompleteDetails.(map[string]any)
		if details["reason"] != tc.details {
			t.Fatalf("%s: incomplete_details = %#v, want reason %q", tc.name, out.IncompleteDetails, tc.details)
		}
	}
}

// TestConvertChatToResponses_TerminalStatuses 非流式 chat→responses 出口的
// status 链须与非流式 responsesOutcome 同源(不得漂移)。
func TestConvertChatToResponses_TerminalStatuses(t *testing.T) {
	cases := []struct {
		name         string
		finishReason string
		status       string // "" = status 键不发
		details      string // "" = incomplete_details 为 null
	}{
		{"stop", "stop", "completed", ""},
		{"length", "length", "incomplete", "max_output_tokens"},
		{"content_filter", "content_filter", "incomplete", "content_filter"},
		{"model_context_window_exceeded", "model_context_window_exceeded", "incomplete", "max_output_tokens"},
		{"pause_turn_not_completed", "pause_turn", "", ""},
		{"compaction_not_completed", "compaction", "", ""},
	}
	for _, tc := range cases {
		chat := `{"id":"chatcmpl_1","choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"` + tc.finishReason + `"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
		out := convertChatToResponses([]byte(chat), "gpt-x", false, nil, nil, nil)
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if status, _ := got["status"].(string); status != tc.status {
			t.Fatalf("%s: status = %q, want %q, body: %s", tc.name, status, tc.status, out)
		}
		details, _ := got["incomplete_details"].(map[string]any)
		if tc.details == "" {
			if got["incomplete_details"] != nil {
				t.Fatalf("%s: incomplete_details = %#v, want null", tc.name, got["incomplete_details"])
			}
			continue
		}
		if details["reason"] != tc.details {
			t.Fatalf("%s: incomplete_details = %#v, want reason %q, body: %s", tc.name, got["incomplete_details"], tc.details, out)
		}
	}
}

// TestOpenAIToClaudeResponse_ContentFilterRefusal 非流式 claude 出口与流式
// 路径必须同一终态:content_filter → stop_reason="refusal",不折叠 end_turn。
func TestOpenAIToClaudeResponse_ContentFilterRefusal(t *testing.T) {
	body := `{"id":"chatcmpl_x","choices":[{"message":{"role":"assistant","content":"cannot help with that"},"finish_reason":"content_filter"}],"usage":{"prompt_tokens":1,"completion_tokens":4}}`
	out := openAIToClaudeResponse([]byte(body), "m", false, true, nil)
	var got ClaudeResponse
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.StopReason != "refusal" {
		t.Fatalf("stop_reason = %q, want refusal", got.StopReason)
	}
}

// TestOpenAIToClaudeResponse_StopSequenceRestore stop_sequence 往返:输出以
// 停止串结尾时恢复 stop_reason="stop_sequence" 与该串(仅 end_turn 升级,
// 对齐 Bifrost anthropicStopReasonWithSequence);未命中时不发 stop_sequence。
func TestOpenAIToClaudeResponse_StopSequenceRestore(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		sequences    []string
		wantReason   string
		wantSequence string // "" = stop_sequence 键不得出现
	}{
		{
			name:         "hit restores stop_sequence",
			body:         `{"choices":[{"message":{"content":"answer STOP"},"finish_reason":"stop"}]}`,
			sequences:    []string{"STOP"},
			wantReason:   "stop_sequence",
			wantSequence: "STOP",
		},
		{
			name:         "no hit keeps end_turn",
			body:         `{"choices":[{"message":{"content":"answer"},"finish_reason":"stop"}]}`,
			sequences:    []string{"STOP"},
			wantReason:   "end_turn",
			wantSequence: "",
		},
		{
			name:         "tool_use not upgraded",
			body:         `{"choices":[{"message":{"content":"answer STOP","tool_calls":[{"id":"t1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
			sequences:    []string{"STOP"},
			wantReason:   "tool_use",
			wantSequence: "",
		},
		{
			name:         "longest suffix wins",
			body:         `{"choices":[{"message":{"content":"answer END TURN"},"finish_reason":"stop"}]}`,
			sequences:    []string{"TURN", "END TURN"},
			wantReason:   "stop_sequence",
			wantSequence: "END TURN",
		},
	}
	for _, tc := range cases {
		out := openAIToClaudeResponse([]byte(tc.body), "m", false, true, tc.sequences)
		var got ClaudeResponse
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if string(got.StopReason) != tc.wantReason {
			t.Fatalf("%s: stop_reason = %q, want %q", tc.name, got.StopReason, tc.wantReason)
		}
		if tc.wantSequence == "" {
			if got.StopSequence != nil {
				t.Fatalf("%s: stop_sequence = %q, want absent", tc.name, *got.StopSequence)
			}
			continue
		}
		if got.StopSequence == nil || *got.StopSequence != tc.wantSequence {
			t.Fatalf("%s: stop_sequence = %#v, want %q", tc.name, got.StopSequence, tc.wantSequence)
		}
	}
}

// TestClaudeStreamHandler_StopSequenceRestore 流式 claude 出口同非流式:
// 输出以停止串结尾时 message_delta 报 stop_sequence 与该串,未命中保持
// end_turn。
func TestClaudeStreamHandler_StopSequenceRestore(t *testing.T) {
	cases := []struct {
		name         string
		content      string
		wantReason   string
		wantSequence string
	}{
		{"hit restores stop_sequence", "answer STOP", "stop_sequence", "STOP"},
		{"no hit keeps end_turn", "answer", "end_turn", ""},
	}
	for _, tc := range cases {
		upstream := strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"` + tc.content + `"},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
			``,
		}, "\n")
		rr := httptest.NewRecorder()
		claudeStreamHandler(context.Background(), rr, io.NopCloser(strings.NewReader(upstream)), "m", false, []string{"STOP"})
		var delta map[string]any
		for _, e := range parseSSEEvents(t, rr.Body.String()) {
			if e.Name == "message_delta" {
				delta, _ = e.Data["delta"].(map[string]any)
			}
		}
		if delta == nil {
			t.Fatalf("%s: no message_delta, body:\n%s", tc.name, rr.Body.String())
		}
		if delta["stop_reason"] != tc.wantReason {
			t.Fatalf("%s: stop_reason = %#v, want %q, body:\n%s", tc.name, delta["stop_reason"], tc.wantReason, rr.Body.String())
		}
		if tc.wantSequence == "" {
			if v, ok := delta["stop_sequence"]; ok && v != nil {
				t.Fatalf("%s: stop_sequence = %#v, want null", tc.name, v)
			}
			continue
		}
		if delta["stop_sequence"] != tc.wantSequence {
			t.Fatalf("%s: stop_sequence = %#v, want %q, body:\n%s", tc.name, delta["stop_sequence"], tc.wantSequence, rr.Body.String())
		}
	}
}

// TestOpenAIToClaudeResponse_ContextWindowExceededMaxTokens 非流式 claude 出口
// 识别 model_context_window_exceeded:截断与 length 同折 max_tokens（对齐
// normalizeFinishReason 的 length 折叠）,不落初值 end_turn 把截断当正常完成。
func TestOpenAIToClaudeResponse_ContextWindowExceededMaxTokens(t *testing.T) {
	cases := []struct {
		name         string
		finishReason string
		wantReason   string
	}{
		{"model_context_window_exceeded collapses to max_tokens", "model_context_window_exceeded", "max_tokens"},
		{"length unchanged", "length", "max_tokens"},
		{"stop unchanged", "stop", "end_turn"},
	}
	for _, tc := range cases {
		body := `{"id":"chatcmpl_x","choices":[{"message":{"role":"assistant","content":"partial"},"finish_reason":"` + tc.finishReason + `"}],"usage":{"prompt_tokens":1,"completion_tokens":4}}`
		out := openAIToClaudeResponse([]byte(body), "m", false, true, nil)
		var got ClaudeResponse
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if string(got.StopReason) != tc.wantReason {
			t.Fatalf("%s: stop_reason = %q, want %q", tc.name, got.StopReason, tc.wantReason)
		}
	}
}

// TestClaudeStreamHandler_ContextWindowExceededMaxTokens 流式 claude 出口同非
// 流式:上游 finish_reason=model_context_window_exceeded 记 finished,message_delta
// 报 max_tokens,不再走 [DONE]/usage-only 兜底报 end_turn（客户端把截断当正常
// 完成继续对话）,上游不回 usage 块也不再报 stream error。
func TestClaudeStreamHandler_ContextWindowExceededMaxTokens(t *testing.T) {
	cases := []struct {
		name        string
		withUsage   bool // 上游是否回 usage-only 尾块
		wantPresent bool
	}{
		{"usage tail present", true, true},
		{"no usage tail still finishes", false, true},
	}
	for _, tc := range cases {
		lines := []string{
			`data: {"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"model_context_window_exceeded"}]}`,
		}
		if tc.withUsage {
			lines = append(lines, `data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":4}}`)
		}
		lines = append(lines, `data: [DONE]`, ``)
		upstream := strings.Join(lines, "\n")
		rr := httptest.NewRecorder()
		claudeStreamHandler(context.Background(), rr, io.NopCloser(strings.NewReader(upstream)), "m", false, nil)
		body := rr.Body.String()
		var delta map[string]any
		for _, e := range parseSSEEvents(t, body) {
			if e.Name == "message_delta" {
				delta, _ = e.Data["delta"].(map[string]any)
			}
		}
		if delta == nil {
			t.Fatalf("%s: no message_delta, body:\n%s", tc.name, body)
		}
		if delta["stop_reason"] != "max_tokens" {
			t.Fatalf("%s: stop_reason = %#v, want max_tokens, body:\n%s", tc.name, delta["stop_reason"], body)
		}
		if strings.Contains(body, `"type":"error"`) {
			t.Fatalf("%s: context-window truncation must not surface as stream error, body:\n%s", tc.name, body)
		}
	}
}

// TestConvertResponsesToClaude_IncompleteReasonMapping incomplete 按
// incomplete_details.reason 细分:content_filter → refusal,
// max_output_tokens(及未知)→ max_tokens。
func TestConvertResponsesToClaude_IncompleteReasonMapping(t *testing.T) {
	cases := []struct {
		name       string
		details    string
		wantReason string
	}{
		{"max_output_tokens", `{"reason":"max_output_tokens"}`, "max_tokens"},
		{"content_filter", `{"reason":"content_filter"}`, "refusal"},
		{"missing_details", "", "max_tokens"},
	}
	for _, tc := range cases {
		resp := `{"id":"msg_x","status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]`
		if tc.details != "" {
			resp += `,"incomplete_details":` + tc.details
		}
		resp += `}`
		out := convertResponsesToClaude([]byte(resp), "m", false, true, nil)
		var got ClaudeResponse
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if string(got.StopReason) != tc.wantReason {
			t.Fatalf("%s: stop_reason = %q, want %q", tc.name, got.StopReason, tc.wantReason)
		}
	}
}

// TestConvertResponsesToClaude_StopSequenceRestore 同 chat 路径:输出以停止
// 串结尾时恢复 stop_sequence 终止形态(Responses 上游不透传 stop,最小往返)。
func TestConvertResponsesToClaude_StopSequenceRestore(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		sequences    []string
		wantReason   string
		wantSequence string
	}{
		{
			name:         "hit restores stop_sequence",
			body:         `{"id":"msg_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"answer STOP"}]}]}`,
			sequences:    []string{"STOP"},
			wantReason:   "stop_sequence",
			wantSequence: "STOP",
		},
		{
			name:         "no hit keeps end_turn",
			body:         `{"id":"msg_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}`,
			sequences:    []string{"STOP"},
			wantReason:   "end_turn",
			wantSequence: "",
		},
	}
	for _, tc := range cases {
		out := convertResponsesToClaude([]byte(tc.body), "m", false, true, tc.sequences)
		var got ClaudeResponse
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if string(got.StopReason) != tc.wantReason {
			t.Fatalf("%s: stop_reason = %q, want %q", tc.name, got.StopReason, tc.wantReason)
		}
		if tc.wantSequence == "" {
			if got.StopSequence != nil {
				t.Fatalf("%s: stop_sequence = %q, want absent", tc.name, *got.StopSequence)
			}
			continue
		}
		if got.StopSequence == nil || *got.StopSequence != tc.wantSequence {
			t.Fatalf("%s: stop_sequence = %#v, want %q", tc.name, got.StopSequence, tc.wantSequence)
		}
	}
}

// TestClaudeResponse_StopReasonNullEncoding 空串编出 null、stop_sequence
// 有值才出现(对齐 Bifrost AnthropicStopReason.MarshalJSON 的 required-null
// 契约),严格客户端拒收 ""。
func TestClaudeResponse_StopReasonNullEncoding(t *testing.T) {
	raw, err := json.Marshal(ClaudeResponse{ID: "msg_1", StopReason: ""})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if v, ok := got["stop_reason"]; !ok || v != nil {
		t.Fatalf("empty stop_reason must marshal as null, got %#v (%s)", got["stop_reason"], raw)
	}
	if _, ok := got["stop_sequence"]; ok {
		t.Fatalf("nil stop_sequence must be omitted, got %s", raw)
	}

	seq := "STOP"
	raw, err = json.Marshal(ClaudeResponse{ID: "msg_1", StopReason: ClaudeStopReason("stop_sequence"), StopSequence: &seq})
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]any{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["stop_reason"] != "stop_sequence" {
		t.Fatalf("stop_reason = %#v, want stop_sequence (%s)", got["stop_reason"], raw)
	}
	if got["stop_sequence"] != "STOP" {
		t.Fatalf("stop_sequence = %#v, want STOP (%s)", got["stop_sequence"], raw)
	}
}

// TestBuildOpenAIResponse_PauseTurnNotCompleted pause_turn/compaction 在
// chat 出口不报完成态:finish_reason 键不发(对齐 Bifrost unmapped → unset)。
func TestBuildOpenAIResponse_PauseTurnNotCompleted(t *testing.T) {
	cases := []struct {
		name        string
		stopReason  string
		wantPresent bool
	}{
		{"pause_turn omitted", "pause_turn", false},
		{"compaction omitted", "compaction", false},
		{"end_turn kept", "end_turn", true},
	}
	for _, tc := range cases {
		anthropicMsg := map[string]any{"role": "assistant", "stop_reason": tc.stopReason}
		out, err := buildOpenAIResponse(anthropicMsg, []map[string]any{{"type": "text", "text": "hi"}}, "m")
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		choice := got["choices"].([]any)[0].(map[string]any)
		fr, ok := choice["finish_reason"]
		if ok != tc.wantPresent {
			t.Fatalf("%s: finish_reason present = %v (value %#v)", tc.name, ok, fr)
		}
		if tc.wantPresent && fr != "stop" {
			t.Fatalf("%s: finish_reason = %#v, want stop", tc.name, fr)
		}
	}
}

// TestAnthropicSSEToChatStream_PauseTurnNotCompleted 流式 chat 桥同非流式:
// pause_turn 终块 finish_reason 编 null,不再缺省 "stop" 报完成态。
func TestAnthropicSSEToChatStream_PauseTurnNotCompleted(t *testing.T) {
	sse := anthropicTruncHealthySSE
	sse = strings.Replace(sse, `"delta":{"stop_reason":"end_turn"}`, `"delta":{"stop_reason":"pause_turn"}`, 1)
	rec := httptest.NewRecorder()
	committed, err := anthropicSSEToChatStream(context.Background(), rec, strings.NewReader(sse), "claude-chat-model", false, true, nil, nil)
	if !committed || err != nil {
		t.Fatalf("committed=%v err=%v, want committed=true err=nil", committed, err)
	}
	body := rec.Body.String()
	if strings.Contains(body, `"finish_reason":"`) {
		t.Fatalf("pause_turn must not map to any chat finish_reason, body:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing [DONE] sentinel, body:\n%s", body)
	}
}
