package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/config"
)

// ---------- 批次3 thinking/reasoning 全链（对齐 Bifrost） ----------

// chatToAnthropicThinkingOf 解析 chatToAnthropicBody 产出的 thinking 对象。
func chatToAnthropicThinkingOf(t *testing.T, body []byte) (map[string]any, map[string]any) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	thinking, _ := got["thinking"].(map[string]any)
	outputConfig, _ := got["output_config"].(map[string]any)
	return thinking, outputConfig
}

func TestThinkingModel_AdaptiveOnlyChatToAnthropic(t *testing.T) {
	// adaptive-only 机型（Opus 4.7+/Sonnet 5+/Fable 系）拒绝 budget_tokens
	// thinking（对齐 Bifrost chat.go:779-782 + 869-873）：必须发 adaptive +
	// output_config.effort，display 默认 summarized。
	cases := []struct {
		name  string
		model string
	}{
		{"opus-4-7", "claude-opus-4-7"},
		{"opus-4.7", "anthropic--claude-4.7-opus"},
		{"opus-5", "claude-opus-5"},
		{"sonnet-5", "claude-sonnet-5"},
		{"fable", "claude-fable-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &OpenAIRequest{
				Model:           tc.model,
				Messages:        []Message{{Role: "user", Content: "hi"}},
				ReasoningEffort: "high",
			}
			thinking, outputConfig := chatToAnthropicThinkingOf(t, chatToAnthropicBody(req, tc.model, true))
			if thinking == nil || thinking["type"] != "adaptive" {
				t.Fatalf("model %s thinking = %#v, want adaptive", tc.model, thinking)
			}
			if _, hasBudget := thinking["budget_tokens"]; hasBudget {
				t.Fatalf("model %s adaptive thinking must not carry budget_tokens: %#v", tc.model, thinking)
			}
			if thinking["display"] != "summarized" {
				t.Fatalf("model %s display = %#v, want summarized (default)", tc.model, thinking["display"])
			}
			if outputConfig == nil || outputConfig["effort"] != "high" {
				t.Fatalf("model %s output_config = %#v, want effort:high", tc.model, outputConfig)
			}
		})
	}

	t.Run("non adaptive-only keeps enabled budget", func(t *testing.T) {
		// 其余机型保持既有 enabled+budget（对齐 Bifrost chat.go:783-803）。
		req := &OpenAIRequest{
			Model:           "claude-x",
			Messages:        []Message{{Role: "user", Content: "hi"}},
			ReasoningEffort: "high",
		}
		thinking, outputConfig := chatToAnthropicThinkingOf(t, chatToAnthropicBody(req, "claude-x", true))
		if thinking == nil || thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(10240) {
			t.Fatalf("thinking = %#v, want enabled/10240", thinking)
		}
		if outputConfig != nil {
			t.Fatalf("output_config = %#v, want omitted for enabled+budget models", outputConfig)
		}
	})

	t.Run("adaptive-only without effort still sends adaptive", func(t *testing.T) {
		req := &OpenAIRequest{
			Model:    "claude-opus-4-7",
			Messages: []Message{{Role: "user", Content: "hi"}},
		}
		body := chatToAnthropicBody(req, "claude-opus-4-7", true)
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if _, exists := got["thinking"]; exists {
			t.Fatalf("no effort in, no thinking out: %#v", got["thinking"])
		}
	})

	t.Run("sampling params stripped when thinking on", func(t *testing.T) {
		temp := 0.7
		topP := 0.9
		req := &OpenAIRequest{
			Model:           "claude-opus-4-7",
			Messages:        []Message{{Role: "user", Content: "hi"}},
			ReasoningEffort: "high",
			Temperature:     &temp,
			TopP:            &topP,
		}
		body := chatToAnthropicBody(req, "claude-opus-4-7", true)
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if _, exists := got["temperature"]; exists {
			t.Fatalf("temperature should be stripped, got %#v", got["temperature"])
		}
		if _, exists := got["top_p"]; exists {
			t.Fatalf("top_p should be stripped, got %#v", got["top_p"])
		}
	})

	t.Run("adaptive-only without effort strips sampling params", func(t *testing.T) {
		// adaptive-only 机型（Opus 4.7+/Sonnet 5+/Fable 系）拒绝 temperature/
		// top_p/top_k（400,对齐 Bifrost chat.go:424-435）——与 thinking 决策
		// 无关:无 reasoning effort 请求下 thinkingObj 落 nil,剥离门仍须挂
		// 机型,否则温度原样透传整条请求被拒。
		cases := []struct {
			name  string
			model string
		}{
			{"opus-4-7", "claude-opus-4-7"},
			{"sonnet-5", "claude-sonnet-5"},
			{"fable", "claude-fable-5"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				temp, topP := 0.7, 0.9
				req := &OpenAIRequest{
					Model:       tc.model,
					Messages:    []Message{{Role: "user", Content: "hi"}},
					Temperature: &temp,
					TopP:        &topP,
				}
				var got map[string]any
				if err := json.Unmarshal(chatToAnthropicBody(req, tc.model, true), &got); err != nil {
					t.Fatal(err)
				}
				if _, exists := got["temperature"]; exists {
					t.Fatalf("model %s temperature must be stripped unconditionally, got %#v", tc.model, got["temperature"])
				}
				if _, exists := got["top_p"]; exists {
					t.Fatalf("model %s top_p must be stripped unconditionally, got %#v", tc.model, got["top_p"])
				}
			})
		}
	})

	t.Run("force disable thinking omits", func(t *testing.T) {
		oldSnap := config.Get()
		t.Cleanup(func() { config.Update(func(s *config.Snapshot) { *s = oldSnap }) })
		config.Update(func(s *config.Snapshot) { s.ForceDisableThinking = true })
		req := &OpenAIRequest{
			Model:           "claude-opus-4-7",
			Messages:        []Message{{Role: "user", Content: "hi"}},
			ReasoningEffort: "high",
		}
		thinking, outputConfig := chatToAnthropicThinkingOf(t, chatToAnthropicBody(req, "claude-opus-4-7", true))
		if thinking != nil || outputConfig != nil {
			t.Fatalf("force-disable should omit thinking/output_config, got %#v / %#v", thinking, outputConfig)
		}
	})
}

func TestThinkingModel_BetweenToolsFallback(t *testing.T) {
	cases := []struct {
		name  string
		model string
		want  any
	}{
		// Sonnet 5.5+ 原样透传 between_tools（对齐 Bifrost BetweenToolsThinking）。
		{"sonnet-5.5 passes through", "claude-sonnet-5-5", "between_tools"},
		// 其余机型就近降级：disabled 被接受时降 disabled。
		{"opus-4-5 falls back to disabled", "claude-opus-4-5", "disabled"},
		{"glm falls back to disabled", "glm-5.3", "disabled"},
		// 始终在线机型（Fable 系）省略 thinking。
		{"fable omits", "claude-fable-5", nil},
		{"opus-5.5 omits", "claude-opus-5.5", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &OpenAIRequest{
				Model:    tc.model,
				Messages: []Message{{Role: "user", Content: "hi"}},
				Thinking: map[string]any{"type": "between_tools"},
			}
			thinking, _ := chatToAnthropicThinkingOf(t, chatToAnthropicBody(req, tc.model, true))
			if tc.want == nil {
				if thinking != nil {
					t.Fatalf("model %s thinking = %#v, want omitted", tc.model, thinking)
				}
				return
			}
			if thinking == nil || thinking["type"] != tc.want {
				t.Fatalf("model %s thinking = %#v, want %v", tc.model, thinking, tc.want)
			}
			if _, hasDisplay := thinking["display"]; hasDisplay {
				t.Fatalf("between_tools takes no display: %#v", thinking)
			}
		})
	}
}

func TestThinkingModel_ClientDisplayPassthrough(t *testing.T) {
	// 客户端显式 display 优先于默认（对齐 Bifrost chat.go:869-873）。
	req := &OpenAIRequest{
		Model:           "claude-opus-4-7",
		Messages:        []Message{{Role: "user", Content: "hi"}},
		ReasoningEffort: "high",
		Thinking:        map[string]any{"type": "adaptive", "display": "omitted"},
	}
	thinking, _ := chatToAnthropicThinkingOf(t, chatToAnthropicBody(req, "claude-opus-4-7", true))
	if thinking == nil || thinking["display"] != "omitted" {
		t.Fatalf("display = %#v, want omitted (client explicit)", thinking["display"])
	}
}

func TestThinkingModel_BuildUpstreamThinkingPreservesAdaptive(t *testing.T) {
	// adaptive 语义在 chat 面透传时不被改写回 enabled（adaptive-only 机型
	// 拒绝 enabled+budget）；adaptive 模式不携带 budget_tokens。
	got := buildUpstreamThinking(map[string]any{"type": "adaptive", "budget_tokens": 2048, "effort": "high"})
	if got["type"] != "adaptive" {
		t.Fatalf("type = %#v, want adaptive", got["type"])
	}
	if _, hasBudget := got["budget_tokens"]; hasBudget {
		t.Fatalf("adaptive must not carry budget_tokens: %#v", got)
	}
	if got["effort"] != "high" {
		t.Fatalf("effort = %#v, want high", got["effort"])
	}
	enabled := buildUpstreamThinking(map[string]any{"type": "enabled", "budget_tokens": 2048})
	if enabled["type"] != "enabled" || enabled["budget_tokens"] != 2048 {
		t.Fatalf("enabled passthrough = %#v", enabled)
	}
}

func TestClaudeBridge_ConvertClaudeRequest_AdaptiveOnlyRewrite(t *testing.T) {
	cases := []struct {
		name     string
		model    string
		thinking any
		effort   any
		wantType string
	}{
		// adaptive-only 机型收到 enabled+budget：重写为 adaptive + effort
		// （对齐 Bifrost chat.go:779-782 + setEffortOnOutputConfig）。
		{"enabled+budget rewritten", "claude-opus-4-7", map[string]any{"type": "enabled", "budget_tokens": 8000.0}, nil, "adaptive"},
		{"adaptive kept", "claude-opus-4-7", map[string]any{"type": "adaptive"}, nil, "adaptive"},
		// 其余机型保持既有 enabled 归一。
		{"non adaptive stays enabled", "claude-sonnet-4-5", map[string]any{"type": "adaptive", "budget_tokens": 2048.0}, nil, "enabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := ClaudeRequest{
				Model:     tc.model,
				Messages:  []ClaudeMessage{{Role: "user", Content: "hi"}},
				MaxTokens: nil,
				Thinking:  tc.thinking,
			}
			if tc.effort != nil {
				req.OutputConfig = tc.effort
			}
			out, _ := convertClaudeRequest(req)
			thinking, _ := out.Thinking.(map[string]any)
			if thinking == nil || thinking["type"] != tc.wantType {
				t.Fatalf("model %s thinking = %#v, want type %s", tc.model, out.Thinking, tc.wantType)
			}
		})
	}

	t.Run("adaptive-only enabled+budget derives effort from budget", func(t *testing.T) {
		out, _ := convertClaudeRequest(ClaudeRequest{
			Model:    "claude-opus-4-7",
			Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
			Thinking: map[string]any{"type": "enabled", "budget_tokens": 12000.0},
		})
		if out.ReasoningEffort != "high" {
			t.Fatalf("ReasoningEffort = %q, want high (budget 12000 tier)", out.ReasoningEffort)
		}
		thinking, _ := out.Thinking.(map[string]any)
		if thinking["effort"] != "high" {
			t.Fatalf("thinking.effort = %#v, want high", thinking["effort"])
		}
		if _, hasBudget := thinking["budget_tokens"]; hasBudget {
			t.Fatalf("adaptive rewrite must not carry budget_tokens: %#v", thinking)
		}
	})

	t.Run("adaptive-only effort from output_config wins", func(t *testing.T) {
		out, _ := convertClaudeRequest(ClaudeRequest{
			Model:        "claude-opus-4-7",
			Messages:     []ClaudeMessage{{Role: "user", Content: "hi"}},
			Thinking:     map[string]any{"type": "enabled", "budget_tokens": 12000.0},
			OutputConfig: map[string]any{"effort": "max"},
		})
		thinking, _ := out.Thinking.(map[string]any)
		if thinking["type"] != "adaptive" {
			t.Fatalf("thinking = %#v, want adaptive", thinking)
		}
		if thinking["effort"] != "max" {
			t.Fatalf("thinking.effort = %#v, want max (output_config wins)", thinking["effort"])
		}
	})
}

// ---------- redacted_thinking typed 槽位（不可回放/密文泄漏修复） ----------

func TestChatBridge_AnthropicDecode_RedactedThinkingTypedSlot(t *testing.T) {
	// 对齐 Bifrost chat.go:1319-1331 ReasoningDetails{encrypted,data,index}：
	// redacted 密文与 thinking 签名落 typed 槽位，不再只置 hasNonText。
	anthropicMsg := map[string]any{
		"id":          "msg_1",
		"role":        "assistant",
		"content":     []any{},
		"stop_reason": "tool_use",
	}
	anthropicMsg["content"] = []any{
		map[string]any{"type": "thinking", "thinking": "step by step", "signature": "sig-1"},
		map[string]any{"type": "redacted_thinking", "data": "enc-payload"},
		map[string]any{"type": "text", "text": "answer"},
	}
	out, err := convertAnthropicMessageToOpenAI(anthropicMsg, "claude-x")
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Choices []struct {
			Message map[string]any `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %#v", resp.Choices)
	}
	msg := resp.Choices[0].Message
	details, ok := msg["reasoning_details"].([]any)
	if !ok || len(details) != 2 {
		t.Fatalf("reasoning_details = %#v, want 2 entries (text+encrypted)", msg["reasoning_details"])
	}
	first, _ := details[0].(map[string]any)
	if first["type"] != "reasoning.text" || first["text"] != "step by step" || first["signature"] != "sig-1" {
		t.Fatalf("thinking detail = %#v, want text+signature", first)
	}
	if first["index"] != float64(0) {
		t.Fatalf("thinking detail index = %#v, want 0", first["index"])
	}
	second, _ := details[1].(map[string]any)
	if second["type"] != "reasoning.encrypted" || second["data"] != "enc-payload" {
		t.Fatalf("redacted detail = %#v, want encrypted data", second)
	}
	if second["index"] != float64(1) {
		t.Fatalf("redacted detail index = %#v, want 1", second["index"])
	}
	// reasoning_content 仍只接思考文本,不含密文。
	if rc, _ := msg["reasoning_content"].(string); rc != "step by step" {
		t.Fatalf("reasoning_content = %#v, want thinking text only", msg["reasoning_content"])
	}
}

func TestClaudeBridge_History_RedactedThinkingNotStringified(t *testing.T) {
	// 密文不再 JSON-stringify 进文本（密文泄漏为可读文本）：redacted_thinking
	// 落 typed reasoning.encrypted 槽位。
	msgs := claudeToOpenAIMessages([]ClaudeMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: []any{
			map[string]any{"type": "redacted_thinking", "data": "enc-payload"},
			map[string]any{"type": "text", "text": "answer"},
		}},
	}, nil)
	if len(msgs) != 2 {
		t.Fatalf("messages = %#v", msgs)
	}
	asst := msgs[1]
	details := asst.ReasoningDetails
	if len(details) != 1 {
		t.Fatalf("ReasoningDetails = %#v, want 1 encrypted entry", details)
	}
	if details[0].Type != "reasoning.encrypted" || details[0].Data != "enc-payload" {
		t.Fatalf("detail = %#v, want encrypted enc-payload", details[0])
	}
	// 文本通道不得出现密文。
	switch c := asst.Content.(type) {
	case string:
		if strings.Contains(c, "enc-payload") {
			t.Fatalf("ciphertext leaked into text: %q", c)
		}
	case []any:
		for _, part := range c {
			pm, _ := part.(map[string]any)
			if t2, _ := pm["text"].(string); strings.Contains(t2, "enc-payload") {
				t.Fatalf("ciphertext leaked into text part: %#v", pm)
			}
		}
	}
}

func TestClaudeBridge_History_ThinkingSignatureTypedSlot(t *testing.T) {
	// thinking+signature 落 typed 槽位（对齐 Bifrost ReasoningDetails{type:text}）,
	// 签名不再整体丢失。
	msgs := claudeToOpenAIMessages([]ClaudeMessage{
		{Role: "assistant", Content: []any{
			map[string]any{"type": "thinking", "thinking": "ponder", "signature": "sig-9"},
			map[string]any{"type": "text", "text": "answer"},
		}},
	}, nil)
	if len(msgs) != 1 {
		t.Fatalf("messages = %#v", msgs)
	}
	if len(msgs[0].ReasoningDetails) != 1 {
		t.Fatalf("ReasoningDetails = %#v, want 1 text entry", msgs[0].ReasoningDetails)
	}
	d := msgs[0].ReasoningDetails[0]
	if d.Type != "reasoning.text" || d.Text != "ponder" || d.Signature != "sig-9" {
		t.Fatalf("detail = %#v, want text+signature", d)
	}
}

// ---------- 流式 thinking typed 槽位（文本随 reasoning_details 流出） ----------

// TestChatBridge_AnthropicSSEToChatStream_ThinkingDetailCarriesText 钉死
// thinking_delta 同时经 reasoning_details typed 通道流出思考文本（对齐
// Bifrost chat.go:1850-1866）：客户端按 index 聚合后,下一轮 assistant 历史
// 才能重放 thinking+signature 配对块（thinking-head 要求）,签名-only detail
// 被出站丢弃会让 thinking-enabled 的 tool-use 回合被上游 400 拒绝。
func TestChatBridge_AnthropicSSEToChatStream_ThinkingDetailCarriesText(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"model\":\"claude-x\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"ponder\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig-1\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	rec := httptest.NewRecorder()
	committed, err := anthropicSSEToChatStream(context.Background(), rec, strings.NewReader(sse), "claude-x", true, false, nil, nil)
	if !committed || err != nil {
		t.Fatalf("committed=%v err=%v, want committed=true err=nil", committed, err)
	}
	// 解析 data: 行（chat 桥 chunk 无 event: 行,delta 在 choices[0] 里）。
	var details []map[string]any
	sawReasoningContent := false
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || strings.TrimSpace(payload) == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta map[string]any `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if rc, ok := delta["reasoning_content"].(string); ok && rc == "ponder" {
			sawReasoningContent = true
		}
		if ds, ok := delta["reasoning_details"].([]any); ok {
			for _, d := range ds {
				if m, ok := d.(map[string]any); ok {
					details = append(details, m)
				}
			}
		}
	}
	if !sawReasoningContent {
		t.Fatalf("missing reasoning_content delta, body:\n%s", rec.Body.String())
	}
	// typed 通道:文本 detail + 签名 detail,同一 index 聚合后可重放配对块。
	var sawText, sawSig bool
	for _, d := range details {
		if d["type"] != "reasoning.text" {
			continue
		}
		if t2, _ := d["text"].(string); t2 == "ponder" {
			sawText = true
			if d["index"] != float64(0) {
				t.Fatalf("thinking detail index = %#v, want 0", d["index"])
			}
		}
		if s, _ := d["signature"].(string); s == "sig-1" {
			sawSig = true
			if d["index"] != float64(0) {
				t.Fatalf("signature detail index = %#v, want 0（与文本同序号聚合）", d["index"])
			}
		}
	}
	if !sawText || !sawSig {
		t.Fatalf("reasoning_details = %#v, want text detail + signature detail", details)
	}

	// keepReasoning=false 时 typed 通道与 reasoning_content 同契约抑制。
	rec2 := httptest.NewRecorder()
	anthropicSSEToChatStream(context.Background(), rec2, strings.NewReader(sse), "claude-x", false, false, nil, nil)
	if strings.Contains(rec2.Body.String(), "reasoning_details") || strings.Contains(rec2.Body.String(), "ponder") {
		t.Fatalf("keepReasoning=false must suppress typed channel too, body:\n%s", rec2.Body.String())
	}
}

// ---------- assistant 历史推理回放（chat→anthropic） ----------

func TestChatToAnthropic_ReplaysAssistantReasoningBlocks(t *testing.T) {
	// 对齐 Bifrost chat.go:1073-1092：assistant 消息头部先重放
	// thinking+signature / redacted_thinking 块（thinking-head 要求）。
	msgs := []Message{
		{Role: "user", Content: "hi"},
		{
			Role: "assistant",
			Content: []any{
				map[string]any{"type": "text", "text": "doing it"},
			},
			ReasoningDetails: []ReasoningDetail{
				{Index: 0, Type: "reasoning.text", Text: "ponder", Signature: "sig-1"},
				{Index: 1, Type: "reasoning.encrypted", Data: "enc-payload"},
			},
			ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: FunctionCall{Name: "f", Arguments: "{}"}}},
		},
		{Role: "tool", ToolCallID: "call_1", Content: "ok"},
	}
	_, out := chatMessagesToAnthropic(msgs, true)
	if len(out) < 2 {
		t.Fatalf("messages = %#v", out)
	}
	// assistant 消息（含 tool_result user 消息之前的 assistant 块）。
	var asst map[string]any
	for _, m := range out {
		if m["role"] == "assistant" {
			asst = m
			break
		}
	}
	if asst == nil {
		t.Fatalf("assistant message missing: %#v", out)
	}
	blocks, _ := asst["content"].([]map[string]any)
	if len(blocks) < 3 {
		t.Fatalf("assistant blocks = %#v, want reasoning head + text + tool_use", blocks)
	}
	// 推理块在头部：thinking -> redacted_thinking -> text -> tool_use。
	if blocks[0]["type"] != "thinking" || blocks[0]["thinking"] != "ponder" || blocks[0]["signature"] != "sig-1" {
		t.Fatalf("first block = %#v, want thinking+signature", blocks[0])
	}
	if blocks[1]["type"] != "redacted_thinking" || blocks[1]["data"] != "enc-payload" {
		t.Fatalf("second block = %#v, want redacted_thinking", blocks[1])
	}
	if blocks[2]["type"] != "text" {
		t.Fatalf("third block = %#v, want text", blocks[2])
	}
}

func TestChatToAnthropic_DropsUnsignedReasoningReplay(t *testing.T) {
	// 签名缺失的 text 详情不发——unsigned thinking 块上游必拒。
	msgs := []Message{
		{Role: "assistant", Content: "plain", ReasoningDetails: []ReasoningDetail{
			{Index: 0, Type: "reasoning.text", Text: "unsigned thinking"},
		}},
	}
	_, out := chatMessagesToAnthropic(msgs, true)
	if len(out) != 1 {
		t.Fatalf("messages = %#v", out)
	}
	blocks, _ := out[0]["content"].([]map[string]any)
	for _, b := range blocks {
		if b["type"] == "thinking" || b["type"] == "redacted_thinking" {
			t.Fatalf("unsigned reasoning should not be replayed: %#v", b)
		}
	}
}

// ---------- 历史推理保留（claude→responses） ----------

func TestClaudeResponses_HistoryReasoningDualStorage(t *testing.T) {
	// 对齐 Bifrost responses.go:760-850 的 summary 与 encrypted_content 双存：
	// thinking 文本落 summary、signature 落 encrypted_content；redacted 密文
	// 落 encrypted_content（至少保留 encrypted 半）。
	msgs := []ClaudeMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: []any{
			map[string]any{"type": "thinking", "thinking": "ponder", "signature": "sig-1"},
			map[string]any{"type": "redacted_thinking", "data": "enc-payload"},
			map[string]any{"type": "text", "text": "answer"},
		}},
	}
	_, input := claudeMessagesToResponsesInput(msgs, nil)
	var reasoningItems []map[string]any
	for _, it := range input {
		if m, ok := it.(map[string]any); ok && m["type"] == "reasoning" {
			reasoningItems = append(reasoningItems, m)
		}
	}
	if len(reasoningItems) != 2 {
		t.Fatalf("reasoning items = %#v, want 2 (summary+encrypted / encrypted-only)", reasoningItems)
	}
	first := reasoningItems[0]
	summary, _ := first["summary"].([]any)
	if len(summary) != 1 {
		t.Fatalf("summary = %#v, want 1 entry", summary)
	}
	if sm, _ := summary[0].(map[string]any); sm["type"] != "summary_text" || sm["text"] != "ponder" {
		t.Fatalf("summary entry = %#v, want summary_text:ponder", sm)
	}
	if first["encrypted_content"] != "sig-1" {
		t.Fatalf("encrypted_content = %#v, want sig-1 (signature slot)", first["encrypted_content"])
	}
	second := reasoningItems[1]
	if second["encrypted_content"] != "enc-payload" {
		t.Fatalf("redacted encrypted_content = %#v, want enc-payload", second["encrypted_content"])
	}
	if _, hasSummary := second["summary"]; hasSummary {
		if sm, _ := second["summary"].([]any); len(sm) > 0 {
			t.Fatalf("encrypted-only item should carry empty summary: %#v", second["summary"])
		}
	}
}

// ---------- 双块 reasoning 双存映射（responses→claude 非流式） ----------

func TestResponsesOutput_DualReasoningMapping(t *testing.T) {
	// 对齐 Bifrost responses.go:760-850：summary 与 encrypted_content 各落其位
	// ——一个 reasoning item → 一个 thinking 块（summary 多段连接），密文落
	// signature,不再只让首段带签名。
	output := []any{
		map[string]any{
			"type": "reasoning",
			"summary": []any{
				map[string]any{"type": "summary_text", "text": "step one"},
				map[string]any{"type": "summary_text", "text": "step two"},
			},
			"encrypted_content": "enc-1",
		},
		map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "done"}}},
	}
	content, _, _ := responsesOutputToClaudeBlocks(output, true, true)
	var thinking *ClaudeContent
	count := 0
	for i := range content {
		if content[i].Type == "thinking" {
			thinking = &content[i]
			count++
		}
	}
	if count != 1 || thinking == nil {
		t.Fatalf("thinking blocks = %d, want 1 per reasoning item: %#v", count, content)
	}
	if thinking.Thinking != "step one\nstep two" {
		t.Fatalf("thinking = %q, want joined summary", thinking.Thinking)
	}
	if thinking.Signature != "enc-1" {
		t.Fatalf("signature = %q, want enc-1 (encrypted_content slot)", thinking.Signature)
	}
}

func TestResponsesOutput_EncryptedOnlyReasoningKeepsSignature(t *testing.T) {
	output := []any{
		map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": "enc-2"},
		map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "done"}}},
	}
	content, _, _ := responsesOutputToClaudeBlocks(output, true, true)
	found := false
	for _, cc := range content {
		if cc.Type == "thinking" && cc.Signature == "enc-2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("encrypted-only reasoning should keep signature slot: %#v", content)
	}
}
