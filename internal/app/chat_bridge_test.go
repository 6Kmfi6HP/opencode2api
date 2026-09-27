package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/config"
)

// ======================== G1: resolveMaxTokens 统一口径 ========================

func TestResolveMaxTokens_PrecedenceAndClamp(t *testing.T) {
	old := config.Get()
	config.Update(func(s *config.Snapshot) {
		s.MaxTokensCap = 131072
		s.MaxTokensCapPerModel = map[string]int{"capped": 1000, "tight": 50}
	})
	t.Cleanup(func() { config.Update(func(s *config.Snapshot) { *s = old }) })

	tests := []struct {
		name  string
		raw   map[string]any
		req   *OpenAIRequest
		model string
		want  int
	}{
		{
			name:  "max_completion_tokens 优先于 max_tokens",
			raw:   map[string]any{"max_completion_tokens": 4096, "max_tokens": 10},
			req:   &OpenAIRequest{MaxTokens: ptr(10)},
			model: "capped", // cap 1000 → 4096 clamp 到 1000
			want:  1000,
		},
		{
			name:  "max_completion_tokens 优先（无 cap）",
			raw:   map[string]any{"max_completion_tokens": 300},
			req:   &OpenAIRequest{},
			model: "",
			want:  300,
		},
		{
			name:  "ExtraBody 的 max_completion_tokens",
			raw:   nil,
			req:   &OpenAIRequest{ExtraBody: map[string]any{"max_completion_tokens": 2048}},
			model: "",
			want:  2048,
		},
		{
			name:  "仅 max_tokens;128 下限",
			raw:   map[string]any{"max_tokens": 5},
			req:   &OpenAIRequest{},
			model: "",
			want:  128,
		},
		{
			name:  "未设置 → cap",
			raw:   nil,
			req:   &OpenAIRequest{},
			model: "capped",
			want:  1000,
		},
		{
			name:  "未设置且无 cap → 8192",
			raw:   nil,
			req:   &OpenAIRequest{},
			model: "",
			want:  defaultAnthropicMaxTokens,
		},
		{
			name:  "cap < 128 时尊重 cap（不强制紧跟 128 下限）",
			raw:   nil,
			req:   &OpenAIRequest{MaxTokens: ptr(30)},
			model: "tight",
			want:  30,
		},
		{
			name:  "负值 max_completion_tokens 视为未设置",
			raw:   map[string]any{"max_completion_tokens": -3, "max_tokens": 700},
			req:   &OpenAIRequest{},
			model: "",
			want:  700,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveMaxTokens(tc.raw, tc.req, tc.model)
			if got != tc.want {
				t.Fatalf("resolveMaxTokens = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestChatToAnthropicBody_UsesResolveMaxTokens(t *testing.T) {
	old := config.Get()
	config.Update(func(s *config.Snapshot) { s.MaxTokensCap = 0; s.MaxTokensCapPerModel = nil })
	t.Cleanup(func() { config.Update(func(s *config.Snapshot) { *s = old }) })

	// 无显式 max_tokens → 8192 兜底（不再 "无 cap 就缺省"）。
	req := &OpenAIRequest{Model: "claude-x", Messages: []Message{{Role: "user", Content: "hi"}}}
	body := chatToAnthropicBodyWithRaw(req, "claude-x", nil)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"] != float64(defaultAnthropicMaxTokens) {
		t.Fatalf("default max_tokens = %#v, want %d", got["max_tokens"], defaultAnthropicMaxTokens)
	}

	// 客户端给 max_completion_tokens=42 → clamp 到 128 下限,且优先于 max_tokens。
	raw := map[string]any{"max_completion_tokens": float64(42), "max_tokens": float64(500)}
	req2 := &OpenAIRequest{Model: "claude-x", Messages: []Message{{Role: "user", Content: "hi"}}}
	body2 := chatToAnthropicBodyWithRaw(req2, "claude-x", raw)
	var got2 map[string]any
	if err := json.Unmarshal(body2, &got2); err != nil {
		t.Fatal(err)
	}
	if got2["max_tokens"] != float64(128) {
		t.Fatalf("max_completion_tokens 应 clamp 到 128, got %#v", got2["max_tokens"])
	}
}

func TestChatToResponsesBody_UsesResolveMaxTokens(t *testing.T) {
	old := config.Get()
	config.Update(func(s *config.Snapshot) { s.MaxTokensCap = 0; s.MaxTokensCapPerModel = nil })
	t.Cleanup(func() { config.Update(func(s *config.Snapshot) { *s = old }) })

	req := &OpenAIRequest{Model: "gpt-x", Messages: []Message{{Role: "user", Content: "hi"}}}
	body := chatToResponsesBodyWithRaw(req, "gpt-x", nil)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["max_output_tokens"] != float64(defaultAnthropicMaxTokens) {
		t.Fatalf("默认 max_output_tokens = %#v, want %d", got["max_output_tokens"], defaultAnthropicMaxTokens)
	}
}

// ======================== G2/G3: Responses 请求体 ========================

func TestChatToResponsesBody_NoStopField(t *testing.T) {
	req := &OpenAIRequest{
		Model:     "gpt-x",
		Messages:  []Message{{Role: "user", Content: "hi"}},
		ExtraBody: map[string]any{"stop": []any{"END"}},
	}
	body := chatToResponsesBody(req, "gpt-x")
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["stop"]; ok {
		t.Fatalf("Responses 请求体不应含 stop 键: %#v", got["stop"])
	}
}

func TestChatToResponsesBody_StoreFalseAndIncludeEncryptedContent(t *testing.T) {
	// 客户端未给 include → 新建含 reasoning.encrypted_content 的数组,store=false。
	req := &OpenAIRequest{Model: "gpt-x", Messages: []Message{{Role: "user", Content: "hi"}}}
	body := chatToResponsesBody(req, "gpt-x")
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["store"] != false {
		t.Fatalf("store = %#v, want false", got["store"])
	}
	inc, ok := got["include"].([]any)
	if !ok || len(inc) != 1 || inc[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %#v, want [\"reasoning.encrypted_content\"]", got["include"])
	}

	// 客户端已给 include → append 去重。
	req2 := &OpenAIRequest{
		Model:     "gpt-x",
		Messages:  []Message{{Role: "user", Content: "hi"}},
		ExtraBody: map[string]any{"include": []any{"reasoning.encrypted_content", "output_text"}},
	}
	body2 := chatToResponsesBody(req2, "gpt-x")
	var got2 map[string]any
	if err := json.Unmarshal(body2, &got2); err != nil {
		t.Fatal(err)
	}
	inc2, _ := got2["include"].([]any)
	seen := map[string]bool{}
	for _, v := range inc2 {
		if s, _ := v.(string); s != "" {
			seen[s] = true
		}
	}
	if !seen["reasoning.encrypted_content"] || !seen["output_text"] {
		t.Fatalf("include 合并后缺少键: %#v", inc2)
	}
	// 去重:同一个 reasoning.encrypted_content 只出现一次。
	enc := 0
	for _, v := range inc2 {
		if v == "reasoning.encrypted_content" {
			enc++
		}
	}
	if enc != 1 {
		t.Fatalf("reasoning.encrypted_content 应去重为一次, got %d (%#v)", enc, inc2)
	}
}

// ======================== G4: role=tool parts → tool_result blocks ========================

func TestChatMessagesToAnthropic_ToolPartsToBlocks(t *testing.T) {
	img := "data:image/png;base64,iVBORw0KGgo="
	msgs := []Message{
		{Role: "user", Content: "weather?"},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "call_1", Type: "function",
			Function: FunctionCall{Name: "vision", Arguments: `{"where":"lab"}`},
		}}},
		{Role: "tool", ToolCallID: "call_1", Content: []any{
			map[string]any{"type": "text", "text": "analysis:"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": img}},
			map[string]any{"type": "unknown_part", "value": 1},
		}},
	}
	_, out := chatMessagesToAnthropic(msgs)
	if len(out) != 3 {
		t.Fatalf("messages = %#v, want 3 (user/assistant/user+tool_result)", out)
	}
	usr := out[2]
	blocks, _ := usr["content"].([]map[string]any)
	if len(blocks) != 1 || blocks[0]["type"] != "tool_result" {
		t.Fatalf("tool_result block missing: %#v", usr["content"])
	}
	inner, _ := blocks[0]["content"].([]map[string]any)
	if len(inner) != 2 {
		t.Fatalf("tool_result content = %#v, want [text,image] (unknown part skipped)", inner)
	}
	if inner[0]["type"] != "text" || inner[0]["text"] != "analysis:" {
		t.Fatalf("text part = %#v", inner[0])
	}
	src, _ := inner[1]["source"].(map[string]any)
	if inner[1]["type"] != "image" || src["type"] != "base64" || src["media_type"] != "image/png" {
		t.Fatalf("image part = %#v", inner[1])
	}
}

func TestChatMessagesToAnthropic_ToolEmptyContentFallback(t *testing.T) {
	msgs := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "c1", Type: "function",
			Function: FunctionCall{Name: "f", Arguments: `{}`},
		}}},
		{Role: "tool", ToolCallID: "c1", Content: ""},
	}
	_, out := chatMessagesToAnthropic(msgs)
	blocks := out[1]["content"].([]map[string]any)
	inner := blocks[0]["content"].([]map[string]any)
	if len(inner) != 1 || inner[0]["text"] != "(empty)" {
		t.Fatalf("empty tool content → %#v, want [{text:\"(empty)\"}]", inner)
	}
}

func TestChatMessagesToAnthropic_AssistantBadArgumentsKeepRaw(t *testing.T) {
	msgs := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "c1", Type: "function",
			Function: FunctionCall{Name: "f", Arguments: `not json`},
		}}},
	}
	_, out := chatMessagesToAnthropic(msgs)
	blocks := out[0]["content"].([]map[string]any)
	tu := blocks[0]
	input := tu["input"].(map[string]any)
	if input["_raw"] != "not json" {
		t.Fatalf("_raw fallback = %#v", input)
	}
}

// ======================== G5: 图片 content edge cases ========================

func TestChatTextToAnthropicContent_ImageEdgeCases(t *testing.T) {
	// 空 url 与非法 data URI 都丢弃；全部丢光时回填 [image attached] 占位文本。
	blocks, ok := chatTextToAnthropicContent([]any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": ""}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:;not-base64,xxxx"}},
	})
	if !ok {
		t.Fatal("content 为空却未回填占位文本")
	}
	if len(blocks) != 1 || blocks[0]["type"] != "text" || blocks[0]["text"] != multimodalAttachedLabel {
		t.Fatalf("fallback blocks = %#v", blocks)
	}

	// 部分丢弃:合法图片 + 无 url 图片 → 保留合法部分。
	blocks2, ok2 := chatTextToAnthropicContent([]any{
		map[string]any{"type": "text", "text": "see"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": ""}},
	})
	if !ok2 || len(blocks2) != 1 || blocks2[0]["text"] != "see" {
		t.Fatalf("partial-drop blocks = %#v", blocks2)
	}
}

// ======================== G8: Anthropic SSE → Chat 状态机 ========================

func TestAnthropicToChat_EOFEmitsFinishAndDone(t *testing.T) {
	// 刻意无 message_stop —— EOF 必须补 finish+[DONE]（幂等,不会重复发两次）。
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"model\":\"claude-x\",\"usage\":{\"input_tokens\":3}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n"
	// 无 message_stop。
	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		anthropicSSEToChatStream(context.Background(), w, strings.NewReader(sse), "claude-x", false, true)
	})
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("EOF 后缺 finish chunk: %s", body)
	}
	if !strings.Contains(body, `"prompt_tokens":3`) || !strings.Contains(body, `"completion_tokens":5`) {
		t.Fatalf("EOF 后缺 usage 终块: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("EOF 后缺 [DONE]: %s", body)
	}
	// 幂等:不应出现两条 [DONE]。
	if got := strings.Count(body, "data: [DONE]"); got != 1 {
		t.Fatalf("[DONE] 出现 %d 次, want 1", got)
	}
}

func TestAnthropicToChat_TextStartPreEmitsInitialText(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"claude-x\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"partial-prefix\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		anthropicSSEToChatStream(context.Background(), w, strings.NewReader(sse), "claude-x", true, false)
	})
	if !strings.Contains(body, `"content":"partial-prefix"`) {
		t.Fatalf("start 初始 text 未预 emit: %s", body)
	}
}

func TestAnthropicToChat_SignatureAndRedactedAreSkipped(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"claude-x\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig-abc\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"redacted_thinking\",\"data\":\"xyz\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		anthropicSSEToChatStream(context.Background(), w, strings.NewReader(sse), "claude-x", true, false)
	})
	if strings.Contains(body, "sig-abc") || strings.Contains(body, "reasoning_content") {
		t.Fatalf("signature/redacted 不应进入 reasoning_content: %s", body)
	}
}

// ======================== G9: Responses SSE → Chat 状态机 ========================

func TestResponsesToChat_FunctionArgumentsDoneEmitsOnlyRemainder(t *testing.T) {
	sse := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-x\"}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"fc1\",\"call_id\":\"call_1\",\"name\":\"lookup\"}}\n\n" +
		"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc1\",\"delta\":\"{\\\"q\\\":\"}\n\n" +
		"event: response.function_call_arguments.done\ndata: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc1\",\"arguments\":\"{\\\"q\\\":\\\"beijing\\\"}\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"usage\":{\"input_tokens\":4,\"output_tokens\":2,\"total_tokens\":6}}}\n\n"
	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		responsesSSEToChatStream(context.Background(), w, strings.NewReader(sse), "gpt-x", false, true)
	})
	// delta 已发 `{"q":`;done 只补发后缀 `"beijing"}`,不能重复 `{"q":`。
	if !strings.Contains(body, `"arguments":"{\"q\":"`) {
		t.Fatalf("缺少 delta 增量: %s", body)
	}
	if !strings.Contains(body, `"arguments":"\"beijing\"}"`) {
		t.Fatalf("done 未补发后缀: %s", body)
	}
	// 不能把完整 arguments 再整体重发一次。
	if strings.Contains(body, `"arguments":"{\"q\":\"beijing\"}"`) {
		t.Fatalf("done 事件把完整 arguments 重发（会被客户端重复拼接）: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("缺 [DONE]: %s", body)
	}
}

func TestResponsesToChat_OutputItemDoneAnnouncesUndeclaredCall(t *testing.T) {
	// 只有 output_item.done 没有 added/delta 的场景（罕见但存在）。
	sse := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"gpt-x\"}}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"id\":\"fc9\",\"call_id\":\"call_9\",\"name\":\"ping\",\"arguments\":\"{}\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"
	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		responsesSSEToChatStream(context.Background(), w, strings.NewReader(sse), "gpt-x", false, false)
	})
	if !strings.Contains(body, `"id":"call_9"`) || !strings.Contains(body, `"name":"ping"`) {
		t.Fatalf("未补发首 chunk 宣告工具调用: %s", body)
	}
	if !strings.Contains(body, `"arguments":"{}"`) {
		t.Fatalf("未补发完整 arguments: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("缺 finish_reason=tool_calls: %s", body)
	}
}

func TestResponsesToChat_FailedThenDoneSentinel(t *testing.T) {
	sse := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"gpt-x\"}}\n\n" +
		"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"r\",\"error\":{\"message\":\"upstream blew up\"}}}\n\n"
	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		responsesSSEToChatStream(context.Background(), w, strings.NewReader(sse), "gpt-x", false, false)
	})
	if !strings.Contains(body, `"error":{"message":"upstream blew up"}`) {
		t.Fatalf("缺错误事件: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("response.failed 后缺 [DONE]: %s", body)
	}
	// 幂等:不应再出现第二个 [DONE]。
	if got := strings.Count(body, "data: [DONE]"); got != 1 {
		t.Fatalf("[DONE] 出现 %d 次, want 1", got)
	}
}

// ======================== G10: usage 口径 ========================

func TestAnthropicUsageToChat_CachedAndTotal(t *testing.T) {
	usage := map[string]any{
		"input_tokens":                float64(100),
		"output_tokens":               float64(20),
		"cache_read_input_tokens":     float64(30),
		"cache_creation_input_tokens": float64(10),
	}
	out := anthropicUsageToChat(usage)
	if out["prompt_tokens"] != float64(100) || out["completion_tokens"] != float64(20) {
		t.Fatalf("基础字段 = %#v", out)
	}
	if out["total_tokens"] != float64(120) {
		t.Fatalf("total_tokens 合成 = %#v", out["total_tokens"])
	}
	// 原 Anthropic 键透传保留。
	if out["cache_read_input_tokens"] != float64(30) || out["cache_creation_input_tokens"] != float64(10) {
		t.Fatalf("原 Anthropic 键应透传: %#v", out)
	}
	// prompt_tokens_details.cached_tokens 由 cache_read 填入。
	details, _ := out["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(30) {
		t.Fatalf("cached_tokens = %#v", details["cached_tokens"])
	}
	// output_tokens_details.thinking_tokens → reasoning_tokens。
	usage2 := map[string]any{
		"input_tokens":          float64(5),
		"output_tokens":         float64(7),
		"output_tokens_details": map[string]any{"thinking_tokens": float64(4)},
	}
	out2 := anthropicUsageToChat(usage2)
	d2, _ := out2["completion_tokens_details"].(map[string]any)
	if d2["reasoning_tokens"] != float64(4) {
		t.Fatalf("reasoning_tokens = %#v", d2)
	}
}

func TestResponsesUsageToChat_CachedAliasAndDeepSeekPassthrough(t *testing.T) {
	usage := map[string]any{
		"input_tokens":             float64(10),
		"output_tokens":            float64(4),
		"input_tokens_details":     map[string]any{"cached_tokens": float64(6)},
		"prompt_cache_hit_tokens":  float64(2),
		"prompt_cache_miss_tokens": float64(8),
	}
	out := responsesUsageToChatBridge(usage)
	if out["total_tokens"] != float64(14) {
		t.Fatalf("total 合成 = %#v", out["total_tokens"])
	}
	// DeepSeek 专有键透传。
	if out["prompt_cache_hit_tokens"] != float64(2) || out["prompt_cache_miss_tokens"] != float64(8) {
		t.Fatalf("prompt_cache_* 透传 = %#v", out)
	}
	// input_tokens_details.cached_tokens → prompt_tokens_details.cached_tokens。
	details, _ := out["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(6) {
		t.Fatalf("cached_tokens = %#v", details["cached_tokens"])
	}
}

// ======================== 回归协议用例（追加,不改既有语义） ========================

// G8 EOF 兜底不影响既有 message_stop 正常路径:finish+usage+[DONE] 各恰好一次。
func TestAnthropicToChat_NormalStopNotDoubleFinalized(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"claude-x\",\"usage\":{\"input_tokens\":2}}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		anthropicSSEToChatStream(context.Background(), w, strings.NewReader(sse), "claude-x", true, true)
	})
	if cnt := strings.Count(body, "data: [DONE]"); cnt != 1 {
		t.Fatalf("[DONE] 出现 %d 次, want 1", cnt)
	}
	if cnt := strings.Count(body, `"finish_reason":"stop"`); cnt != 1 {
		t.Fatalf("finish chunk(finish_reason=stop) 出现 %d 次, want 1: %s", cnt, body)
	}
	if cnt := strings.Count(body, `"prompt_tokens"`); cnt != 1 {
		t.Fatalf("usage 终块出现 %d 次, want 1: %s", cnt, body)
	}
}

// 仅 include_usage=true 时 usage 终块照发（既有行为）;false 时不发。
func TestAnthropicToChat_UsageChunkOnlyWhenIncludeUsage(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"claude-x\",\"usage\":{\"input_tokens\":2}}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	noUsage := drainSSEFromHandler(func(w http.ResponseWriter) {
		anthropicSSEToChatStream(context.Background(), w, strings.NewReader(sse), "claude-x", false, false)
	})
	if strings.Contains(noUsage, `"prompt_tokens"`) {
		t.Fatalf("includeUsage=false 不应发 usage 终块: %s", noUsage)
	}
	withUsage := drainSSEFromHandler(func(w http.ResponseWriter) {
		anthropicSSEToChatStream(context.Background(), w, strings.NewReader(sse), "claude-x", false, true)
	})
	if !strings.Contains(withUsage, `"prompt_tokens":2`) {
		t.Fatalf("includeUsage=true 应发 usage 终块: %s", withUsage)
	}
}

// convertResponsesToChat 非流式侧 refusal 已放 message;流式侧 delta 内联进
// content —— 这里钉死两边的 refusal 输出形状避免回归。
func TestConvertResponsesToChat_RefusalFieldPreserved(t *testing.T) {
	resp := `{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"cannot do that"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	out := convertResponsesToChat([]byte(resp), "gpt-x", false)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if r, _ := msg["refusal"].(string); r != "cannot do that" {
		t.Fatalf("非流式 refusal 字段 = %#v, want \"cannot do that\"", msg["refusal"])
	}
}

func TestChatToResponsesBody_ReasoningSummaryAuto(t *testing.T) {
	reasoningOf := func(t *testing.T, effort, model string) map[string]any {
		t.Helper()
		req := &OpenAIRequest{
			Model:           model,
			Messages:        []Message{{Role: "user", Content: "hi"}},
			ReasoningEffort: effort,
		}
		var got map[string]any
		if err := json.Unmarshal(chatToResponsesBody(req, model), &got); err != nil {
			t.Fatal(err)
		}
		r, _ := got["reasoning"].(map[string]any)
		return r
	}

	// 通用 reasoning 模型：请求 summary:auto，否则上游静默思考、客户端只见
	// reasoning_tokens 增长而无可见思考文本。
	if r := reasoningOf(t, "low", "mimo-v2.6-flash-free"); r["effort"] != "low" || r["summary"] != "auto" {
		t.Fatalf("reasoning = %#v, want {effort:low summary:auto}", r)
	}

	// muse-spark：白名单内 effort 不动并补 summary:auto（默认让思考可见）。
	if r := reasoningOf(t, "high", "muse-spark-1.3-contributor"); r["effort"] != "high" || r["summary"] != "auto" {
		t.Fatalf("reasoning = %#v, want {effort:high summary:auto}", r)
	}

	// muse-spark：effort 收口白名单（max->xhigh），归一化后同样请求 summary:auto。
	if r := reasoningOf(t, "max", "muse-spark-1.3-contributor"); r["effort"] != "xhigh" || r["summary"] != "auto" {
		t.Fatalf("reasoning = %#v, want {effort:xhigh summary:auto}", r)
	}

	// muse-spark + 无法归一化的 effort（如 minimal 之外的自定义串）：
	// 归一化为空后整体省略 reasoning，避免 400。
	if r := reasoningOf(t, "ultra", "muse-spark-1.3-contributor"); r != nil {
		t.Fatalf("reasoning 应省略（effort 归一化为空）, got %#v", r)
	}

	// 非 muse-spark 的未知 effort 不归一化，保持原值透传（附带 summary:auto）。
	if r := reasoningOf(t, "ultra", "some-other-model"); r["effort"] != "ultra" || r["summary"] != "auto" {
		t.Fatalf("reasoning = %#v, want {effort:ultra summary:auto}", r)
	}
}
