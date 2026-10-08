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
	body := chatToAnthropicBodyWithRaw(req, "claude-x", nil, true)
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
	body2 := chatToAnthropicBodyWithRaw(req2, "claude-x", raw, true)
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
	_, out := chatMessagesToAnthropic(msgs, true)
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
	_, out := chatMessagesToAnthropic(msgs, true)
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
	_, out := chatMessagesToAnthropic(msgs, true)
	blocks := out[0]["content"].([]map[string]any)
	tu := blocks[0]
	input := tu["input"].(map[string]any)
	if input["_raw"] != "not json" {
		t.Fatalf("_raw fallback = %#v", input)
	}
}

// ======================== 工具链：ID 清洗 / 孤儿降级 / is_error ========================

// TestChatMessagesToAnthropic_ToolIDsSanitizeAndPair 钉死 tool_use.id 与
// tool_result.tool_use_id 的确定性清洗：同一原始 ID 在两侧必须清洗到相同值
// 才能配对（Anthropic 要求 ID 仅 [A-Za-z0-9_-]，"functions.x:0" 类照发必 400）。
func TestChatMessagesToAnthropic_ToolIDsSanitizeAndPair(t *testing.T) {
	badID := "functions.x:0"
	msgs := []Message{
		{Role: "user", Content: "run it"},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: badID, Type: "function",
			Function: FunctionCall{Name: "shell", Arguments: `{"cmd":"ls"}`},
		}}},
		{Role: "tool", ToolCallID: badID, Content: "ok"},
	}
	_, out := chatMessagesToAnthropic(msgs, true)
	if len(out) != 3 {
		t.Fatalf("messages = %#v, want 3", out)
	}
	asst := out[1]
	blocks, _ := asst["content"].([]map[string]any)
	var toolUseID string
	for _, b := range blocks {
		if b["type"] == "tool_use" {
			toolUseID, _ = b["id"].(string)
		}
	}
	if toolUseID == "" || toolUseID == badID {
		t.Fatalf("tool_use id not sanitized: %q", toolUseID)
	}
	usr := out[2]
	blocks, _ = usr["content"].([]map[string]any)
	if len(blocks) != 1 || blocks[0]["type"] != "tool_result" {
		t.Fatalf("tool_result block missing: %#v", usr["content"])
	}
	resultID, _ := blocks[0]["tool_use_id"].(string)
	if resultID != toolUseID {
		t.Fatalf("pairing broken: tool_use %q vs tool_result %q", toolUseID, resultID)
	}
	// Anthropic 字符集：仅 [A-Za-z0-9_-]。
	for _, r := range resultID {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			t.Fatalf("sanitized id %q has illegal rune %q", resultID, r)
		}
	}
}

// TestChatMessagesToAnthropic_OrphanToolResultDegradesToUserText 钉死孤儿
// tool_result（空 tool_use_id）降级为普通 user 文本——空 ID 照发必 400。
func TestChatMessagesToAnthropic_OrphanToolResultDegradesToUserText(t *testing.T) {
	msgs := []Message{
		{Role: "tool", ToolCallID: "", Content: "orphan output"},
		{Role: "user", Content: "next"},
	}
	_, out := chatMessagesToAnthropic(msgs, true)
	// 合并后只有一条 user 消息,内容为降级文本 + 原始 user 文本;绝不含 tool_result。
	if len(out) != 1 {
		t.Fatalf("messages = %#v, want 1 merged user message", out)
	}
	blocks, _ := out[0]["content"].([]map[string]any)
	texts := ""
	for _, b := range blocks {
		if b["type"] == "tool_result" {
			t.Fatalf("orphan tool_result must not be forwarded: %#v", blocks)
		}
		if t2, _ := b["text"].(string); t2 != "" {
			texts += t2 + "\n"
		}
	}
	if !strings.Contains(texts, "orphan output") {
		t.Fatalf("orphan output lost: %#v", blocks)
	}
}

// TestChatMessagesToAnthropic_EmptyIDToolCallSkipped 钉死空 id 的 tool call
// 被跳过（无法配对,照发 tool_use 空 id 必 400）。
func TestChatMessagesToAnthropic_EmptyIDToolCallSkipped(t *testing.T) {
	msgs := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "", Type: "function",
			Function: FunctionCall{Name: "f", Arguments: `{}`},
		}}},
	}
	_, out := chatMessagesToAnthropic(msgs, true)
	// assistant 消息无剩余内容 → 整条消失,不发 tool_use。
	if len(out) != 0 {
		t.Fatalf("messages = %#v, want 0 (empty-id tool call dropped)", out)
	}
}

// TestChatMessagesToAnthropic_IsErrorFlagToToolResult 钉死 Message.IsError →
// tool_result.is_error:true（错误语义不再用 "Error: " 文本前缀污染内容）。
func TestChatMessagesToAnthropic_IsErrorFlagToToolResult(t *testing.T) {
	isErr := true
	clean := false
	msgs := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "call_e", Type: "function",
			Function: FunctionCall{Name: "f", Arguments: `{}`},
		}}},
		{Role: "tool", ToolCallID: "call_e", Content: "boom", IsError: &isErr},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "call_o", Type: "function",
			Function: FunctionCall{Name: "f", Arguments: `{}`},
		}}},
		{Role: "tool", ToolCallID: "call_o", Content: "fine", IsError: &clean},
	}
	_, out := chatMessagesToAnthropic(msgs, true)
	// out[1] 含 call_e 的 tool_result（is_error:true），out[3] 含 call_o 的。
	toolResult := func(m map[string]any) map[string]any {
		blocks, _ := m["content"].([]map[string]any)
		for _, b := range blocks {
			if b["type"] == "tool_result" {
				return b
			}
		}
		return nil
	}
	errResult := toolResult(out[1])
	if errResult == nil {
		t.Fatalf("tool_result missing: %#v", out[1])
	}
	if errResult["is_error"] != true {
		t.Fatalf("is_error = %#v, want true", errResult["is_error"])
	}
	if inner, _ := errResult["content"].([]map[string]any); len(inner) != 1 || inner[0]["text"] != "boom" {
		t.Fatalf("content polluted: %#v", errResult["content"])
	}
	okResult := toolResult(out[3])
	if okResult == nil {
		t.Fatalf("tool_result missing: %#v", out[3])
	}
	if _, has := okResult["is_error"]; has {
		t.Fatalf("is_error should be absent without the flag: %#v", okResult)
	}
}

// TestParseToolCallArguments_CompactPassthroughKeepsKeyOrder 钉死合法 JSON
// 对象 arguments 的紧凑透传——原始键序保留（map 往返会重排键序,破坏上游
// 前缀缓存），坏 JSON 仍兜底 {"_raw": ...} 不丢数据。
func TestParseToolCallArguments_CompactPassthroughKeepsKeyOrder(t *testing.T) {
	tests := []struct {
		name string
		args string
		want string // 期望 marshal 后的 input 字节
	}{
		{"空串补 {}", "", `{}`},
		{"合法对象透传", `{"cmd":"ls","cwd":"/tmp"}`, `{"cmd":"ls","cwd":"/tmp"}`},
		{"空白被压缩", "{ \"cmd\" : \"ls\" }", `{"cmd":"ls"}`},
		{"坏 JSON 兜底 _raw", `not json`, `{"_raw":"not json"}`},
		{"数组兜底 _raw", `[1,2]`, `{"_raw":"[1,2]"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := parseToolCallArguments(tc.args)
			b, err := json.Marshal(input)
			if err != nil {
				t.Fatalf("marshal input: %v", err)
			}
			if string(b) != tc.want {
				t.Fatalf("input = %s, want %s", b, tc.want)
			}
		})
	}
	// 键序保留：非字母序的原始键序在 marshal 后不被重排。
	raw := `{"z_last":1,"a_first":2}`
	b, _ := json.Marshal(parseToolCallArguments(raw))
	if string(b) != raw {
		t.Fatalf("key order destroyed: %s, want %s", b, raw)
	}
}

// ======================== 工具链：tool_choice 转换 / allowed_tools / parallel ========================

// toolChoiceBody 是 allowed_tools/parallel 用例的公共请求形状。
func toolChoiceBody(toolChoice any, extra parallelExtra) *OpenAIRequest {
	req := &OpenAIRequest{
		Model:      "claude-x",
		Messages:   []Message{{Role: "user", Content: "hi"}},
		ToolChoice: toolChoice,
	}
	if extra != nil {
		req.ExtraBody = map[string]any(extra)
	}
	return req
}

type parallelExtra map[string]any

func decodeToolChoice(t *testing.T, req *OpenAIRequest) (map[string]any, []string) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(chatToAnthropicBodyWithRaw(req, "claude-x", nil, false), &body); err != nil {
		t.Fatal(err)
	}
	var names []string
	if tools, ok := body["tools"].([]any); ok {
		for _, tl := range tools {
			if m, ok := tl.(map[string]any); ok {
				if n, _ := m["name"].(string); n != "" {
					names = append(names, n)
				}
			}
		}
	}
	tc, _ := body["tool_choice"].(map[string]any)
	return tc, names
}

// TestChatToAnthropicBody_AllowedToolsRestrictsDeclarations 钉死 allowed_tools
// 按声明收窄工具列表（绝不静默放行全量工具）：mode=required → any。
func TestChatToAnthropicBody_AllowedToolsRestrictsDeclarations(t *testing.T) {
	req := toolChoiceBody(map[string]any{
		"type": "allowed_tools", "mode": "required",
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "tool_a"}}},
	}, nil)
	req.Tools = []Tool{
		{Type: "function", Function: ToolFunction{Name: "tool_a"}},
		{Type: "function", Function: ToolFunction{Name: "tool_b"}},
	}
	tc, names := decodeToolChoice(t, req)
	if strings.Join(names, ",") != "tool_a" {
		t.Fatalf("tool names = %#v, want [tool_a] (tool_b must not be forwarded)", names)
	}
	if tc["type"] != "any" {
		t.Fatalf(`mode "required" means a tool must be called: %#v`, tc)
	}
}

// TestChatToAnthropicBody_AllowedToolsAutoModeDoesNotForceACall 钉死 mode=auto
// → tool_choice auto（不得强制 any）。
func TestChatToAnthropicBody_AllowedToolsAutoModeDoesNotForceACall(t *testing.T) {
	req := toolChoiceBody(map[string]any{
		"type": "allowed_tools", "mode": "auto",
		"tools": []any{map[string]any{"function": map[string]any{"name": "tool_a"}}},
	}, nil)
	req.Tools = []Tool{
		{Type: "function", Function: ToolFunction{Name: "tool_a"}},
		{Type: "function", Function: ToolFunction{Name: "tool_b"}},
	}
	tc, names := decodeToolChoice(t, req)
	if strings.Join(names, ",") != "tool_a" {
		t.Fatalf("tool names = %#v, want [tool_a]", names)
	}
	if tc["type"] != "auto" {
		t.Fatalf(`mode "auto" must not force a tool call: %#v`, tc)
	}
}

// TestChatToAnthropicBody_AllowedToolsUnknownNamesPermitNothing 钉死空交集
// → tool_choice none（绝不静默放行全量工具）。
func TestChatToAnthropicBody_AllowedToolsUnknownNamesPermitNothing(t *testing.T) {
	req := toolChoiceBody(map[string]any{
		"type": "allowed_tools", "mode": "required",
		"tools": []any{map[string]any{"function": map[string]any{"name": "not_declared"}}},
	}, nil)
	req.Tools = []Tool{
		{Type: "function", Function: ToolFunction{Name: "tool_a"}},
		{Type: "function", Function: ToolFunction{Name: "tool_b"}},
	}
	tc, names := decodeToolChoice(t, req)
	if len(names) != 0 {
		t.Fatalf("no declared tool was allowed, so none may be forwarded: %#v", names)
	}
	if tc["type"] != "none" {
		t.Fatalf(`empty intersection must land on tool_choice none: %#v`, tc)
	}
}

// TestChatToAnthropicBody_ParallelToolCallsFalseDisablesParallelUse 钉死
// parallel_tool_calls:false → disable_parallel_tool_use:true（含无 tool_choice
// 时的 auto 载体）。
func TestChatToAnthropicBody_ParallelToolCallsFalseDisablesParallelUse(t *testing.T) {
	reqTools := []Tool{{Type: "function", Function: ToolFunction{Name: "tool_a"}}}

	t.Run("with an explicit tool choice", func(t *testing.T) {
		req := toolChoiceBody(map[string]any{"type": "auto"}, parallelExtra{"parallel_tool_calls": false})
		req.Tools = reqTools
		tc, _ := decodeToolChoice(t, req)
		if tc == nil {
			t.Fatal("tool_choice missing")
		}
		if tc["disable_parallel_tool_use"] != true {
			t.Fatalf("disable_parallel_tool_use = %#v, want true", tc["disable_parallel_tool_use"])
		}
	})
	t.Run("with no tool choice of its own", func(t *testing.T) {
		req := toolChoiceBody(nil, parallelExtra{"parallel_tool_calls": false})
		req.Tools = reqTools
		tc, _ := decodeToolChoice(t, req)
		if tc == nil {
			t.Fatal("the flag has nowhere to live without a tool_choice")
		}
		if tc["type"] != "auto" {
			t.Fatalf("auto is Anthropic's default, so this adds no restriction: %#v", tc)
		}
		if tc["disable_parallel_tool_use"] != true {
			t.Fatalf("disable_parallel_tool_use = %#v, want true", tc["disable_parallel_tool_use"])
		}
	})
	t.Run("tool_choice none skips the flag", func(t *testing.T) {
		req := toolChoiceBody(map[string]any{"type": "none"}, parallelExtra{"parallel_tool_calls": false})
		req.Tools = reqTools
		tc, _ := decodeToolChoice(t, req)
		if tc["type"] != "none" {
			t.Fatalf("tool_choice type = %#v, want none", tc["type"])
		}
		if _, has := tc["disable_parallel_tool_use"]; has {
			t.Fatalf("none means no tool may run; flag is meaningless: %#v", tc)
		}
	})
}

// TestChatToAnthropicBody_ParallelToolCallsTrueSendsNothing 钉死
// parallel_tool_calls:true / 缺省不发 disable_parallel_tool_use。
func TestChatToAnthropicBody_ParallelToolCallsTrueSendsNothing(t *testing.T) {
	reqTools := []Tool{{Type: "function", Function: ToolFunction{Name: "tool_a"}}}
	for _, extra := range []parallelExtra{
		{"parallel_tool_calls": true},
		{},
	} {
		req := toolChoiceBody(nil, extra)
		req.Tools = reqTools
		tc, _ := decodeToolChoice(t, req)
		if _, has := tc["disable_parallel_tool_use"]; has {
			t.Fatalf("parallel default must not set disable_parallel_tool_use: %#v", tc)
		}
	}
}

// TestChatToAnthropicBody_AnyAndCustomToolChoice 钉死 tool_choice any →
// {"type":"any"}、custom → auto（此前二者被静默丢弃）。
func TestChatToAnthropicBody_AnyAndCustomToolChoice(t *testing.T) {
	reqTools := []Tool{{Type: "function", Function: ToolFunction{Name: "tool_a"}}}
	tests := []struct {
		name     string
		choice   any
		wantType string
	}{
		{"any 透传", map[string]any{"type": "any"}, "any"},
		{"custom 映射 auto", map[string]any{"type": "custom"}, "auto"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := toolChoiceBody(tt.choice, nil)
			req.Tools = reqTools
			tc, _ := decodeToolChoice(t, req)
			if tc == nil {
				t.Fatalf("tool_choice dropped for %v", tt.choice)
			}
			if tc["type"] != tt.wantType {
				t.Fatalf("tool_choice type = %#v, want %q", tc["type"], tt.wantType)
			}
		})
	}
}

// ======================== G5: 图片 content edge cases ========================

func TestChatTextToAnthropicContent_ImageEdgeCases(t *testing.T) {
	// 空 url、畸形 data URI（无 media type）与白名单外 scheme（file://）都
	// 丢弃；全部丢光时回填 [image attached] 占位文本。合法的非 base64 data
	// URI（如 data:image/svg+xml,...）不再直接丢弃——走 url source 透传
	//（对齐 Bifrost ExtractURLTypeInfo,见 TestImageURLToAnthropicBlock_SchemeWhitelist）。
	blocks, ok := chatTextToAnthropicContent([]any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": ""}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:;not-base64,xxxx"}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "file:///etc/passwd"}},
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
		anthropicSSEToChatStream(context.Background(), w, strings.NewReader(sse), "claude-x", false, true, nil, nil)
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
		anthropicSSEToChatStream(context.Background(), w, strings.NewReader(sse), "claude-x", true, false, nil, nil)
	})
	if !strings.Contains(body, `"content":"partial-prefix"`) {
		t.Fatalf("start 初始 text 未预 emit: %s", body)
	}
}

func TestAnthropicToChat_SignatureAndRedactedLandInReasoningDetails(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"claude-x\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig-abc\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"redacted_thinking\",\"data\":\"xyz\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		anthropicSSEToChatStream(context.Background(), w, strings.NewReader(sse), "claude-x", true, false, nil, nil)
	})
	// reasoning_content 只接思考文本,签名/密文不拼进文本通道。
	if strings.Contains(body, "reasoning_content") {
		t.Fatalf("signature/redacted 不应进入 reasoning_content: %s", body)
	}
	// typed 槽位（reasoning_details）承载签名与密文供回放（对齐 Bifrost
	// reasoning.encrypted/reasoning.text details）。
	if !strings.Contains(body, `"signature":"sig-abc"`) {
		t.Fatalf("signature 未落 reasoning_details: %s", body)
	}
	if !strings.Contains(body, `"type":"reasoning.text"`) {
		t.Fatalf("signature detail 缺 reasoning.text 类型: %s", body)
	}
	if !strings.Contains(body, `"data":"xyz"`) {
		t.Fatalf("redacted 密文未落 reasoning_details: %s", body)
	}
	if !strings.Contains(body, `"type":"reasoning.encrypted"`) {
		t.Fatalf("redacted detail 缺 reasoning.encrypted 类型: %s", body)
	}
}

func TestAnthropicToChat_ReasoningDetailsSuppressedWithoutKeepReasoning(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"claude-x\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"redacted_thinking\",\"data\":\"xyz\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		anthropicSSEToChatStream(context.Background(), w, strings.NewReader(sse), "claude-x", false, false, nil, nil)
	})
	// keepReasoning=false：typed 推理槽位与 reasoning_content 同一抑制契约。
	if strings.Contains(body, "reasoning_details") || strings.Contains(body, "reasoning_content") {
		t.Fatalf("keepReasoning=false 时 reasoning_details 应被抑制: %s", body)
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
		committed, err := responsesSSEToChatStream(context.Background(), w, strings.NewReader(sse), "gpt-x", false, true, nil, nil)
		if err != nil || !committed {
			t.Fatalf("responsesSSEToChatStream = (%v, %v), want (true, nil)", committed, err)
		}
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
		committed, err := responsesSSEToChatStream(context.Background(), w, strings.NewReader(sse), "gpt-x", false, false, nil, nil)
		if err != nil || !committed {
			t.Fatalf("responsesSSEToChatStream = (%v, %v), want (true, nil)", committed, err)
		}
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
	// failed 前先给一段真实 delta：宽限 peek 下纯壳帧不再 commit，直接
	// 调 handler 必须自带产出帧才能进入 committed 分支。
	sse := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"gpt-x\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n" +
		"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"r\",\"error\":{\"message\":\"upstream blew up\"}}}\n\n"
	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		committed, err := responsesSSEToChatStream(context.Background(), w, strings.NewReader(sse), "gpt-x", false, false, nil, nil)
		if err != nil || !committed {
			t.Fatalf("responsesSSEToChatStream = (%v, %v), want (true, nil)", committed, err)
		}
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

// TestResponsesUsageToChat_InputDetailsCachedAlias 锁定 muse-spark 回归：上游
// 原生 responses 口径用 input_tokens_details.cached_tokens，下游
// buildClaudeUsageCore / parseCacheUsage 只认 prompt_tokens_details 形态，
// responsesUsageToChat 必须做别名归位，否则客户端 usage 与缓存统计双双丢 read。
func TestResponsesUsageToChat_InputDetailsCachedAlias(t *testing.T) {
	// 实测上游形状（cache_debug_stream_usage）：input_tokens 含 cached 部分。
	usage := map[string]any{
		"input_tokens":          float64(749),
		"input_tokens_details":  map[string]any{"cached_tokens": float64(625)},
		"output_tokens":         float64(243),
		"output_tokens_details": map[string]any{"reasoning_tokens": float64(230)},
		"total_tokens":          float64(992),
	}
	out := responsesUsageToChat(usage)
	details, _ := out["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(625) {
		t.Fatalf("cached_tokens = %#v, want 625", details["cached_tokens"])
	}
	// 归位后 buildClaudeUsageCore 应产出顶层 cache_read_input_tokens 并做
	// readFromSplit 减法（input 749-625=124），避免 double-count。
	claudeUsage := buildClaudeUsageCore(out)
	if claudeUsage["cache_read_input_tokens"] != 625 {
		t.Fatalf("cache_read_input_tokens = %#v, want 625", claudeUsage["cache_read_input_tokens"])
	}
	if claudeUsage["input_tokens"] != 124 {
		t.Fatalf("input_tokens = %#v, want 124 (749-625)", claudeUsage["input_tokens"])
	}

	// prompt_tokens_details 先有值时 input 形态不再覆盖（先有谁用谁）。
	usage2 := map[string]any{
		"input_tokens":          float64(100),
		"input_tokens_details":  map[string]any{"cached_tokens": float64(60)},
		"prompt_tokens_details": map[string]any{"cached_tokens": float64(30)},
		"output_tokens":         float64(10),
		"total_tokens":          float64(110),
	}
	out2 := responsesUsageToChat(usage2)
	d2, _ := out2["prompt_tokens_details"].(map[string]any)
	if d2["cached_tokens"] != float64(60) {
		t.Fatalf("cached_tokens = %#v, want 60 (input 形态优先)", d2["cached_tokens"])
	}
}

// ======================== 回归协议用例（追加,不改既有语义） ========================

// G8 EOF 兜底不影响既有 message_stop 正常路径:finish+usage+[DONE] 各恰好一次。
func TestAnthropicToChat_NormalStopNotDoubleFinalized(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"claude-x\",\"usage\":{\"input_tokens\":2}}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		anthropicSSEToChatStream(context.Background(), w, strings.NewReader(sse), "claude-x", true, true, nil, nil)
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
		anthropicSSEToChatStream(context.Background(), w, strings.NewReader(sse), "claude-x", false, false, nil, nil)
	})
	if strings.Contains(noUsage, `"prompt_tokens"`) {
		t.Fatalf("includeUsage=false 不应发 usage 终块: %s", noUsage)
	}
	withUsage := drainSSEFromHandler(func(w http.ResponseWriter) {
		anthropicSSEToChatStream(context.Background(), w, strings.NewReader(sse), "claude-x", false, true, nil, nil)
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

// Issue #35：客户端只带 max_completion_tokens（不带 max_tokens）时，cap 注入
// 不得补 max_tokens —— OpenAI 已废弃 max_tokens 并要求二者互斥，zen 部分
// 后端对并存严格 400 (cannot both be set)，注入造成偶发硬失败。
func TestConvertRequest_NoMaxTokensInjectionWhenMaxCompletionTokensSet(t *testing.T) {
	old := config.Get()
	config.Update(func(s *config.Snapshot) {
		s.MaxTokensCap = 200000
		s.MaxTokensCapPerModel = map[string]int{"big-pickle": 200000}
	})
	t.Cleanup(func() { config.Update(func(s *config.Snapshot) { *s = old }) })

	// 仅 max_completion_tokens（按 issue #35 原始线上 JSON 解析）：max_tokens
	// 不出现，max_completion_tokens 原样转发。
	req1 := &OpenAIRequest{}
	if err := json.Unmarshal([]byte(`{
		"model": "big-pickle",
		"stream": true,
		"max_completion_tokens": 32000,
		"messages": [{"role": "user", "content": "hi"}]
	}`), req1); err != nil {
		t.Fatal(err)
	}
	out := convertRequest(req1)
	if v, ok := out["max_tokens"]; ok {
		t.Fatalf("max_tokens 不应被注入, got %#v", v)
	}
	if out["max_completion_tokens"] != 32000 {
		t.Fatalf("max_completion_tokens = %#v, want 32000", out["max_completion_tokens"])
	}

	// 双字段均未设置：cap 注入照旧（无 conflict 风险）。
	out2 := convertRequest(&OpenAIRequest{
		Model:    "big-pickle",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if out2["max_tokens"] != 200000 {
		t.Fatalf("max_tokens = %#v, want 200000 (cap 注入)", out2["max_tokens"])
	}

	// 仅 max_tokens：注入路径不介入，显式值照常收敛。
	out3 := convertRequest(&OpenAIRequest{
		Model:     "big-pickle",
		Messages:  []Message{{Role: "user", Content: "hi"}},
		MaxTokens: ptr(5000),
	})
	if out3["max_tokens"] != 5000 {
		t.Fatalf("max_tokens = %#v, want 5000", out3["max_tokens"])
	}
	if _, ok := out3["max_completion_tokens"]; ok {
		t.Fatalf("max_completion_tokens 不应出现")
	}

	// extra_body 携带 max_completion_tokens（SDK extra_body 顶层合并）：
	// 同样不得注入 max_tokens，且客户端值原样上行。
	req4 := &OpenAIRequest{}
	if err := json.Unmarshal([]byte(`{
		"model": "big-pickle",
		"messages": [{"role": "user", "content": "hi"}],
		"extra_body": {"max_completion_tokens": 4096}
	}`), req4); err != nil {
		t.Fatal(err)
	}
	out4 := convertRequest(req4)
	if v, ok := out4["max_tokens"]; ok {
		t.Fatalf("extra_body 场景 max_tokens 不应被注入, got %#v", v)
	}
	if out4["max_completion_tokens"] != float64(4096) {
		t.Fatalf("max_completion_tokens = %#v (%T), want 4096 (extra_body 透传)", out4["max_completion_tokens"], out4["max_completion_tokens"])
	}
}

// TestChatToResponsesBody_LongToolCallIDCappedAndPaired 回归
//（input[N].call_id must be <= 64）：chat→responses 上游路径的 tool 消息
// call_id 之前未清洗——客户端回放合法但 >64 的 tool_call_id 原样进
// input[].call_id 被上游 400。现在 tool 侧与 assistant 侧同用
// sanitizeAnthropicToolUseID：两侧 <=64 且保持配对。
func TestChatToResponsesBody_LongToolCallIDCappedAndPaired(t *testing.T) {
	longID := strings.Repeat("a", 80)
	req := &OpenAIRequest{
		Model: "gpt-x",
		Messages: []Message{
			{Role: "assistant", ToolCalls: []ToolCall{{ID: longID, Type: "function", Function: FunctionCall{Name: "shell", Arguments: `{"cmd":"ls"}`}}}},
			{Role: "tool", ToolCallID: longID, Content: "ok"},
		},
	}
	body := chatToResponsesBody(req, "gpt-x")
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	input, _ := got["input"].([]any)
	var callID, outputID string
	for _, it := range input {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		switch m["type"] {
		case "function_call":
			callID, _ = m["call_id"].(string)
		case "function_call_output":
			outputID, _ = m["call_id"].(string)
		}
	}
	if callID == "" || len(callID) > 64 {
		t.Fatalf("function_call call_id = %q (len %d), want 非空且 <= 64", callID, len(callID))
	}
	if outputID != callID {
		t.Fatalf("pairing broken: function_call %q vs function_call_output %q", callID, outputID)
	}
}
