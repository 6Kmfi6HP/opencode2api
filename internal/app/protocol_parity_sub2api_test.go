package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestPreserveChatPassthroughKeys 顶层 parallel_tool_calls/service_tier
// 必须从原始 body 回填进 ExtraBody(类型化结构装不下),extra_body 显式键优先。
func TestPreserveChatPassthroughKeys(t *testing.T) {
	req := &OpenAIRequest{Model: "gpt-x"}
	preserveChatPassthroughKeys([]byte(`{"model":"gpt-x","parallel_tool_calls":false,"service_tier":"flex"}`), req)
	if req.ExtraBody["parallel_tool_calls"] != false {
		t.Fatalf("ExtraBody = %#v", req.ExtraBody)
	}
	if req.ExtraBody["service_tier"] != "flex" {
		t.Fatalf("ExtraBody = %#v", req.ExtraBody)
	}
	// extra_body 显式键优先,不被顶层覆盖。
	req2 := &OpenAIRequest{Model: "gpt-x", ExtraBody: map[string]any{"service_tier": "auto"}}
	preserveChatPassthroughKeys([]byte(`{"model":"gpt-x","service_tier":"flex"}`), req2)
	if req2.ExtraBody["service_tier"] != "auto" {
		t.Fatalf("ExtraBody = %#v", req2.ExtraBody)
	}
	// 无键时不建 ExtraBody。
	req3 := &OpenAIRequest{Model: "gpt-x"}
	preserveChatPassthroughKeys([]byte(`{"model":"gpt-x"}`), req3)
	if req3.ExtraBody != nil {
		t.Fatalf("ExtraBody = %#v, want nil", req3.ExtraBody)
	}
}

// TestChatToResponsesBody_ParityFields chat→responses 请求体必须透传
// parallel_tool_calls / service_tier,并把 response_format 映射为
// text.format(对齐 sub2api ChatCompletionsToResponses)。
func TestChatToResponsesBody_ParityFields(t *testing.T) {
	mt := 256
	req := &OpenAIRequest{
		Model:     "gpt-x",
		Messages:  []Message{{Role: "user", Content: "hi"}},
		MaxTokens: &mt,
		ResponseFormat: map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "ans",
				"schema": map[string]any{"type": "object"},
			},
		},
		ExtraBody: map[string]any{
			"parallel_tool_calls": false,
			"service_tier":        "flex",
		},
	}
	body := chatToResponsesBodyWithRaw(req, "gpt-x", map[string]any{})
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if v, _ := got["parallel_tool_calls"].(bool); v != false {
		t.Fatalf("parallel_tool_calls = %#v, want false passthrough", got["parallel_tool_calls"])
	}
	if got["service_tier"] != "flex" {
		t.Fatalf("service_tier = %#v, want flex", got["service_tier"])
	}
	text, _ := got["text"].(map[string]any)
	format, _ := text["format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Fatalf("text.format = %#v, want json_schema", text)
	}
	schema, _ := format["schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("text.format.schema = %#v", format)
	}
}

// TestChatToAnthropicBody_ThinkingStripsSampling thinking 生效时必须剥离
// temperature/top_p(对齐 sub2api 与本项目 convertClaudeRequest),否则上游
// Anthropic 400。thinking 关闭时采样参数保留。
func TestChatToAnthropicBody_ThinkingStripsSampling(t *testing.T) {
	temp, topP := 0.7, 0.9
	req := &OpenAIRequest{
		Model: "claude-x", Messages: []Message{{Role: "user", Content: "hi"}},
		Temperature: &temp, TopP: &topP,
		ReasoningEffort: "high",
	}
	var got map[string]any
	if err := json.Unmarshal(chatToAnthropicBody(req, "claude-x"), &got); err != nil {
		t.Fatal(err)
	}
	if _, exists := got["thinking"]; !exists {
		t.Fatal("want thinking injected")
	}
	if _, exists := got["temperature"]; exists {
		t.Fatalf("temperature must be stripped when thinking on: %#v", got["temperature"])
	}
	if _, exists := got["top_p"]; exists {
		t.Fatalf("top_p must be stripped when thinking on: %#v", got["top_p"])
	}

	plain := &OpenAIRequest{
		Model: "claude-x", Messages: []Message{{Role: "user", Content: "hi"}},
		Temperature: &temp, TopP: &topP,
	}
	var got2 map[string]any
	if err := json.Unmarshal(chatToAnthropicBody(plain, "claude-x"), &got2); err != nil {
		t.Fatal(err)
	}
	if got2["temperature"] != temp || got2["top_p"] != topP {
		t.Fatalf("sampling params must be kept when thinking off: %#v", got2)
	}
}

// TestAnthropicUsageToChat_CacheCreation 写缓存 token 必须归位到
// prompt_tokens_details.cache_creation_tokens(对齐 sub2api
// promptDetailsFromResponses),否则 OpenAI 形状下游丢失该分量。
func TestAnthropicUsageToChat_CacheCreation(t *testing.T) {
	out := anthropicUsageToChat(map[string]any{
		"input_tokens": float64(100), "output_tokens": float64(20),
		"cache_read_input_tokens": float64(30), "cache_creation_input_tokens": float64(10),
	})
	details, _ := out["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(30) {
		t.Fatalf("cached_tokens = %#v", details)
	}
	if details["cache_creation_tokens"] != float64(10) {
		t.Fatalf("cache_creation_tokens = %#v", details)
	}
}

// TestNormalizeFinishReason_UnknownClosed 未知 stop_reason 必须闭集合
// 回退为 stop(对齐 sub2api),不得透传污染 finish_reason。
func TestNormalizeFinishReason_UnknownClosed(t *testing.T) {
	if normalizeFinishReason("pause_turn") != "stop" {
		t.Fatalf("pause_turn → %q, want stop", normalizeFinishReason("pause_turn"))
	}
	if normalizeFinishReason("model_context_window_exceeded") != "stop" {
		t.Fatal("unknown reason must close to stop")
	}
	if normalizeFinishReason("tool_use") != "tool_calls" {
		t.Fatal("known mapping must be stable")
	}
}

// TestChatResponseFormatToResponsesTextFormat_JsonObject 非 schema 形态
// (json_object)透传 type,不伪造 schema。
func TestChatResponseFormatToResponsesTextFormat_JsonObject(t *testing.T) {
	got := chatResponseFormatToResponsesTextFormat(map[string]any{"type": "json_object"})
	if got["type"] != "json_object" {
		t.Fatalf("got = %#v", got)
	}
	if chatResponseFormatToResponsesTextFormat(nil) != nil {
		t.Fatal("nil input must yield nil")
	}
}

// TestResponsesSSEToChatStream_CustomToolCall custom/freeform 工具的
// input 增量与 done 必须落到同一 tool_calls index(对齐 sub2api
// resToChatHandleOutputItemAdded/custom_tool_call 分支)。
func TestResponsesSSEToChatStream_CustomToolCall(t *testing.T) {
	sse := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_c\",\"model\":\"gpt-x\"}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"custom_tool_call\",\"id\":\"ct_1\",\"call_id\":\"call_c1\",\"name\":\"apply_patch\"}}\n\n" +
		"event: response.custom_tool_call_input.delta\ndata: {\"type\":\"response.custom_tool_call_input.delta\",\"output_index\":0,\"item_id\":\"ct_1\",\"delta\":\"{\\\"op\\\":\"}}\n\n" +
		"event: response.custom_tool_call_input.done\ndata: {\"type\":\"response.custom_tool_call_input.done\",\"output_index\":0,\"item_id\":\"ct_1\",\"input\":\"{\\\"op\\\":\\\"create\\\"}\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_c\"}}\n\n"
	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		committed, err := responsesSSEToChatStream(context.Background(), w, strings.NewReader(sse), "gpt-x", false, false, nil, nil)
		if err != nil || !committed {
			t.Fatalf("responsesSSEToChatStream = (%v, %v), want (true, nil)", committed, err)
		}
	})
	names := map[int]string{}
	args := map[int]string{}
	for _, line := range strings.Split(body, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int `json:"index"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("chunk is not JSON: %q", payload)
		}
		for _, c := range chunk.Choices {
			for _, tc := range c.Delta.ToolCalls {
				names[tc.Index] += tc.Function.Name
				args[tc.Index] += tc.Function.Arguments
			}
		}
	}
	if len(names) != 1 || names[0] != "apply_patch" {
		t.Fatalf("names = %v, body: %s", names, body)
	}
	// delta 前缀 + done 差量补齐 == 完整 input,不重复。
	if args[0] != `{"op":"create"}` {
		t.Fatalf("args[0] = %q, want full input without duplication, body: %s", args[0], body)
	}
}

// TestResponsesSSEToChatStream_IncompleteContentFilter incomplete +
// content_filter 原因必须映射为 finish_reason content_filter(对齐 sub2api
// resToChatHandleCompleted),而非一律 length。
func TestResponsesSSEToChatStream_IncompleteContentFilter(t *testing.T) {
	sse := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_f\",\"model\":\"gpt-x\"}}\n\n" +
		"event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_f\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"content_filter\"}}}\n\n"
	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		committed, err := responsesSSEToChatStream(context.Background(), w, strings.NewReader(sse), "gpt-x", false, false, nil, nil)
		if err != nil || !committed {
			t.Fatalf("responsesSSEToChatStream = (%v, %v), want (true, nil)", committed, err)
		}
	})
	if !strings.Contains(body, `"finish_reason":"content_filter"`) {
		t.Fatalf("want content_filter finish reason, body: %s", body)
	}
}

// TestResponsesSSEToChatStream_ResponseDoneAlias response.done
// (Realtime/WS 别名)必须像 completed 一样终结流并补 [DONE](对齐 sub2api)。
func TestResponsesSSEToChatStream_ResponseDoneAlias(t *testing.T) {
	sse := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_d\",\"model\":\"gpt-x\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"event: response.done\ndata: {\"type\":\"response.done\",\"response\":{\"id\":\"resp_d\",\"status\":\"completed\"}}\n\n"
	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		committed, err := responsesSSEToChatStream(context.Background(), w, strings.NewReader(sse), "gpt-x", false, false, nil, nil)
		if err != nil || !committed {
			t.Fatalf("responsesSSEToChatStream = (%v, %v), want (true, nil)", committed, err)
		}
	})
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("want [DONE] sentinel, body: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("want stop finish chunk, body: %s", body)
	}
}

// TestResponsesSSEToChatStream_ServiceTierChunk 上游 service_tier 必须
// 回写到 chat chunk 顶层(对齐 sub2api ChatCompletionsChunk.ServiceTier)。
func TestResponsesSSEToChatStream_ServiceTierChunk(t *testing.T) {
	sse := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_s\",\"model\":\"gpt-x\",\"service_tier\":\"flex\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_s\"}}\n\n"
	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		committed, err := responsesSSEToChatStream(context.Background(), w, strings.NewReader(sse), "gpt-x", false, false, nil, nil)
		if err != nil || !committed {
			t.Fatalf("responsesSSEToChatStream = (%v, %v), want (true, nil)", committed, err)
		}
	})
	if !strings.Contains(body, `"service_tier":"flex"`) {
		t.Fatalf("want service_tier passthrough, body: %s", body)
	}
}

// TestConvertResponsesToChat_IncompleteContentFilter 非流式路径同样细分
// incomplete 原因。
func TestConvertResponsesToChat_IncompleteContentFilter(t *testing.T) {
	resp := `{"id":"resp_9","model":"gpt-x","status":"incomplete",
		"incomplete_details":{"reason":"content_filter"},
		"output":[{"type":"message","id":"m1","content":[{"type":"output_text","text":"x"}]}]}`
	out := convertResponsesToChat([]byte(resp), "gpt-x", false)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	choices := got["choices"].([]any)
	fr := choices[0].(map[string]any)["finish_reason"]
	if fr != "content_filter" {
		t.Fatalf("finish_reason = %#v, want content_filter", fr)
	}
}

// TestConvertResponsesToChat_CustomToolCall 非流式 function_call 外的
// custom_tool_call 必须转为 chat tool_calls(读 input 键)。
func TestConvertResponsesToChat_CustomToolCall(t *testing.T) {
	resp := `{"id":"resp_8","model":"gpt-x","status":"completed",
		"output":[{"type":"custom_tool_call","id":"ct_1","call_id":"call_c1","name":"apply_patch","input":"{\"op\":\"create\"}"}]}`
	out := convertResponsesToChat([]byte(resp), "gpt-x", false)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	choices := got["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls = %#v", msg["tool_calls"])
	}
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "apply_patch" || fn["arguments"] != `{"op":"create"}` {
		t.Fatalf("function = %#v", fn)
	}
	if choices[0].(map[string]any)["finish_reason"] != "tool_calls" {
		t.Fatalf("finish = %#v", choices[0].(map[string]any)["finish_reason"])
	}
}
