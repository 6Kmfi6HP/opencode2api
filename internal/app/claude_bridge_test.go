package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/config"
)

// ---------- H1/H2: convertClaudeRequest 思考互斥 + max_tokens 兜底 ----------

func TestClaudeBridge_ConvertClaudeRequest_ThinkingStripsSampling(t *testing.T) {
	cases := []struct {
		name     string
		thinking any
		effort   any // output_config.effort
	}{
		{"enabled", map[string]any{"type": "enabled", "budget_tokens": 2048.0}, nil},
		{"adaptive", map[string]any{"type": "adaptive", "budget_tokens": 2048.0}, nil},
		{"effort via output_config", nil, map[string]any{"effort": "high"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			temp := 0.7
			topP := 0.9
			topK := 40
			maxTok := 512
			req := ClaudeRequest{
				Model:        "m",
				Messages:     []ClaudeMessage{{Role: "user", Content: "hi"}},
				MaxTokens:    &maxTok,
				Temperature:  &temp,
				TopP:         &topP,
				TopK:         &topK,
				Thinking:     tc.thinking,
				OutputConfig: tc.effort,
			}
			out, _ := convertClaudeRequest(req)
			if out.Temperature != nil {
				t.Fatalf("temperature should be stripped when thinking on, got %v", *out.Temperature)
			}
			if out.TopP != nil {
				t.Fatalf("top_p should be stripped when thinking on, got %v", *out.TopP)
			}
			if out.ExtraBody != nil {
				if _, ok := out.ExtraBody["top_k"]; ok {
					t.Fatal("top_k should be stripped when thinking on")
				}
			}
		})
	}

	t.Run("thinking disabled keeps sampling", func(t *testing.T) {
		temp := 0.7
		topP := 0.9
		topK := 40
		maxTok := 512
		req := ClaudeRequest{
			Model:       "m",
			Messages:    []ClaudeMessage{{Role: "user", Content: "hi"}},
			MaxTokens:   &maxTok,
			Temperature: &temp,
			TopP:        &topP,
			TopK:        &topK,
			Thinking:    map[string]any{"type": "disabled"},
		}
		out, _ := convertClaudeRequest(req)
		if out.Temperature == nil || *out.Temperature != temp {
			t.Fatalf("temperature should be kept when thinking disabled, got %#v", out.Temperature)
		}
		if out.TopP == nil || *out.TopP != topP {
			t.Fatalf("top_p should be kept, got %#v", out.TopP)
		}
		if out.ExtraBody == nil || out.ExtraBody["top_k"] != 40 {
			t.Fatalf("top_k should pass through, got %#v", out.ExtraBody)
		}
	})
}

func TestClaudeBridge_ConvertClaudeRequest_MaxTokensFallback(t *testing.T) {
	old := config.Update(func(s *config.Snapshot) {
		s.MaxTokensCap = 4096
		s.MaxTokensCapPerModel = map[string]int{}
	})
	t.Cleanup(func() { config.Update(func(s *config.Snapshot) { *s = old }) })

	// nil -> 8192，再按 cap 4096 收敛。
	out, _ := convertClaudeRequest(ClaudeRequest{Model: "m", Messages: []ClaudeMessage{{Role: "user", Content: "hi"}}})
	if out.MaxTokens == nil {
		t.Fatal("nil max_tokens must default to 8192 (clamped here)")
	}
	if *out.MaxTokens != 4096 {
		t.Fatalf("max_tokens = %d, want cap 4096", *out.MaxTokens)
	}

	// 低于 128 收敛到 128。
	small := 8
	out, _ = convertClaudeRequest(ClaudeRequest{Model: "m", Messages: []ClaudeMessage{{Role: "user", Content: "hi"}}, MaxTokens: &small})
	if out.MaxTokens == nil || *out.MaxTokens != 128 {
		t.Fatalf("max_tokens = %#v, want 128", out.MaxTokens)
	}

	// 上限 cap。
	big := 99999
	out, _ = convertClaudeRequest(ClaudeRequest{Model: "m", Messages: []ClaudeMessage{{Role: "user", Content: "hi"}}, MaxTokens: &big})
	if out.MaxTokens == nil || *out.MaxTokens != 4096 {
		t.Fatalf("max_tokens = %#v, want 4096", out.MaxTokens)
	}

	// 无 cap 时 nil -> 8192。
	config.Update(func(s *config.Snapshot) { s.MaxTokensCap = 0 })
	out, _ = convertClaudeRequest(ClaudeRequest{Model: "m", Messages: []ClaudeMessage{{Role: "user", Content: "hi"}}})
	if out.MaxTokens == nil || *out.MaxTokens != 8192 {
		t.Fatalf("max_tokens = %#v, want 8192", out.MaxTokens)
	}
}

func TestClaudeBridge_ConvertRequest_StopAndMaxCompletionTokens(t *testing.T) {
	old := config.Update(func(s *config.Snapshot) { s.MaxTokensCap = 4096 })
	t.Cleanup(func() { config.Update(func(s *config.Snapshot) { *s = old }) })

	maxTok := 100
	maxComp := 99999 // 会被 cap 到 4096
	freq := 0.5
	pres := 0.25
	n := 2
	seed := 7
	req := OpenAIRequest{
		Model:               "m",
		Messages:            []Message{{Role: "user", Content: "hi"}},
		MaxTokens:           &maxTok,
		MaxCompletionTokens: &maxComp,
		Stop:                []string{"\n"},
		FrequencyPenalty:    &freq,
		PresencePenalty:     &pres,
		LogitBias:           map[string]int{"1": -1},
		N:                   &n,
		User:                "u1",
		ResponseFormat:      map[string]any{"type": "json_object"},
		Seed:                &seed,
	}
	out := convertRequest(&req)
	if out["max_tokens"] != 100 {
		t.Fatalf("max_tokens = %#v", out["max_tokens"])
	}
	if out["max_completion_tokens"] != 4096 {
		t.Fatalf("max_completion_tokens = %#v, want 4096", out["max_completion_tokens"])
	}
	if _, ok := out["stop"].([]string); !ok {
		t.Fatalf("stop = %#v, want []string", out["stop"])
	}
	if out["frequency_penalty"] != freq || out["presence_penalty"] != pres {
		t.Fatalf("penalties = %#v/%#v", out["frequency_penalty"], out["presence_penalty"])
	}
	if out["logit_bias"] == nil || out["n"] != n || out["user"] != "u1" || out["seed"] != seed {
		t.Fatalf("logit_bias/n/user/seed = %#v/%#v/%#v/%#v", out["logit_bias"], out["n"], out["user"], out["seed"])
	}
	if out["response_format"] == nil {
		t.Fatal("response_format missing")
	}
}

// ---------- H3: claudeToOpenAIMessages ----------

func TestClaudeBridge_ToOpenAIMessages_UnknownBlockSerialized(t *testing.T) {
	msgs := claudeToOpenAIMessages([]ClaudeMessage{
		{Role: "assistant", Content: []any{
			map[string]any{"type": "web_search_tool_result", "content": "r1", "tool_use_id": "srv_1"},
		}},
	}, nil)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	parts, ok := msgs[0].Content.([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("content = %#v, want one text part", msgs[0].Content)
	}
	part, _ := parts[0].(map[string]any)
	if part["type"] != "text" {
		t.Fatalf("part type = %#v, want text", part["type"])
	}
	text, _ := part["text"].(string)
	if !strings.Contains(text, `"type":"web_search_tool_result"`) || !strings.Contains(text, "srv_1") {
		t.Fatalf("unknown block not serialized into context: %q", text)
	}
	// 计数仍保留到 unsupported_blocks（不静默）。
	counts := scanClaudeUnsupportedBlocks([]ClaudeMessage{
		{Role: "assistant", Content: []any{
			map[string]any{"type": "web_search_tool_result", "content": "r1"},
		}},
	})
	if counts["web_search_tool_result"] != 1 {
		t.Fatalf("unsupported_blocks count = %#v", counts)
	}
}

func TestClaudeBridge_ToOpenAIMessages_ReasoningOnlyWithToolCalls(t *testing.T) {
	// 带 tool_calls 的 assistant：reasoning_content 保留（DeepSeek 兼容）。
	withTool := claudeToOpenAIMessages([]ClaudeMessage{
		{Role: "assistant", Content: []any{
			map[string]any{"type": "thinking", "thinking": "plan"},
			map[string]any{"type": "tool_use", "id": "toolu_1", "name": "f", "input": map[string]any{}},
		}},
	}, nil)
	var asst *Message
	for i := range withTool {
		if withTool[i].Role == "assistant" {
			asst = &withTool[i]
		}
	}
	if asst == nil {
		t.Fatal("assistant message missing")
	}
	if asst.ReasoningContent == nil || *asst.ReasoningContent != "plan" {
		t.Fatalf("reasoning_content = %#v, want plan", asst.ReasoningContent)
	}

	// 纯文本 assistant：思考丢弃。
	noTool := claudeToOpenAIMessages([]ClaudeMessage{
		{Role: "assistant", Content: []any{
			map[string]any{"type": "thinking", "thinking": "plan"},
			map[string]any{"type": "text", "text": "hello"},
		}},
	}, nil)
	var asst2 *Message
	for i := range noTool {
		if noTool[i].Role == "assistant" {
			asst2 = &noTool[i]
		}
	}
	if asst2 == nil {
		t.Fatal("assistant message missing")
	}
	if asst2.ReasoningContent != nil {
		t.Fatalf("reasoning_content should be dropped without tool_calls, got %#v", *asst2.ReasoningContent)
	}
}

func TestClaudeBridge_ToOpenAIMessages_BadSourceDegradesToPlaceholder(t *testing.T) {
	msgs := claudeToOpenAIMessages([]ClaudeMessage{
		{Role: "user", Content: []any{
			map[string]any{"type": "image"}, // 无 source
			map[string]any{"type": "document", "source": map[string]any{"type": "base64"}}, // 无 data
			map[string]any{"type": "text", "text": "x"},
		}},
	}, nil)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	parts, ok := msgs[0].Content.([]any)
	if !ok || len(parts) != 3 {
		t.Fatalf("content = %#v, want 3 parts", msgs[0].Content)
	}
	p0, _ := parts[0].(map[string]any)
	p1, _ := parts[1].(map[string]any)
	if p0["type"] != "text" || p0["text"] != "[image attached]" {
		t.Fatalf("image placeholder = %#v", p0)
	}
	if p1["type"] != "text" || p1["text"] != "[document attached]" {
		t.Fatalf("document placeholder = %#v", p1)
	}
}

// ---------- H4: claudeToResponsesBody 请求侧 ----------

func TestClaudeBridge_ToResponsesBody_FiltersBillingHeader(t *testing.T) {
	var claudeReq ClaudeRequest
	raw := `{
		"model":"m",
		"system":[
			{"type":"text","text":"x-anthropic-billing-header: abc"},
			{"type":"text","text":"real system"}
		],
		"max_tokens":128,
		"messages":[{"role":"user","content":"hi"}]
	}`
	if err := json.Unmarshal([]byte(raw), &claudeReq); err != nil {
		t.Fatal(err)
	}
	body := claudeToResponsesBody(claudeReq, "m")
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	instr, _ := req["instructions"].(string)
	if strings.Contains(instr, "x-anthropic-billing-header") {
		t.Fatalf("billing header should be filtered: %q", instr)
	}
	if !strings.Contains(instr, "real system") {
		t.Fatalf("real system text lost: %q", instr)
	}

	// system 为纯字符串且整串是 billing header 时，instructions 省略。
	raw2 := `{"model":"m","system":"x-anthropic-billing-header: abc","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`
	var cr2 ClaudeRequest
	if err := json.Unmarshal([]byte(raw2), &cr2); err != nil {
		t.Fatal(err)
	}
	body2 := claudeToResponsesBody(cr2, "m")
	var req2 map[string]any
	if err := json.Unmarshal(body2, &req2); err != nil {
		t.Fatal(err)
	}
	if _, exists := req2["instructions"]; exists {
		t.Fatalf("instructions should be omitted for billing-only system: %#v", req2["instructions"])
	}
}

func TestClaudeBridge_ToResponsesBody_ToolResultImagesAndEmpty(t *testing.T) {
	var claudeReq ClaudeRequest
	raw := `{
		"model":"m",
		"max_tokens":128,
		"messages":[
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[
					{"type":"text","text":"here is the image"},
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}
				]},
				{"type":"tool_result","tool_use_id":"toolu_2","content":[]},
				{"type":"tool_result","tool_use_id":"toolu_3","content":"备","is_error":true}
			]}
		]
	}`
	if err := json.Unmarshal([]byte(raw), &claudeReq); err != nil {
		t.Fatal(err)
	}
	body := claudeToResponsesBody(claudeReq, "m")
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	input, _ := req["input"].([]any)
	var outputs []map[string]any
	var imageMsg map[string]any
	for _, it := range input {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] == "function_call_output" {
			outputs = append(outputs, m)
		}
		if m["type"] == "message" && m["role"] == "user" {
			if c, ok := m["content"].([]any); ok {
				for _, p := range c {
					if pm, ok := p.(map[string]any); ok && pm["type"] == "input_image" {
						imageMsg = m
					}
				}
			}
		}
	}
	if len(outputs) != 3 {
		t.Fatalf("function_call_output count = %d, want 3", len(outputs))
	}
	if imageMsg == nil {
		t.Fatalf("image tool_result should emit separate user message with input_image: %#v", input)
	} else {
		c, _ := imageMsg["content"].([]any)
		pm, _ := c[0].(map[string]any)
		if url, _ := pm["image_url"].(string); !strings.HasPrefix(url, "data:image/png;base64,") {
			t.Fatalf("input_image url = %#v", pm["image_url"])
		}
	}
	// 空 output -> "(empty)"
	byCallID := map[string]map[string]any{}
	for _, o := range outputs {
		byCallID[o["call_id"].(string)] = o
	}
	if got, _ := byCallID["toolu_2"]["output"].(string); got != "(empty)" {
		t.Fatalf("empty output = %q, want (empty)", got)
	}
	if got, _ := byCallID["toolu_1"]["output"].(string); !strings.Contains(got, "here is the image") {
		t.Fatalf("toolu_1 output = %q", got)
	}
}

func TestClaudeBridge_ToResponsesBody_ParallelToolCallsAndClamp(t *testing.T) {
	old := config.Update(func(s *config.Snapshot) {
		s.MaxTokensCap = 0
		s.MaxTokensCapPerModel = map[string]int{}
	})
	t.Cleanup(func() { config.Update(func(s *config.Snapshot) { *s = old }) })

	// disable_parallel_tool_use:true -> parallel_tool_calls:false
	var cr ClaudeRequest
	raw := `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		t.Fatal(err)
	}
	body := claudeToResponsesBody(cr, "m")
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if req["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %#v, want false", req["parallel_tool_calls"])
	}
	// max_output_tokens clamp min 128（无 cap）
	if req["max_output_tokens"] != float64(128) {
		t.Fatalf("max_output_tokens = %#v, want 128", req["max_output_tokens"])
	}

	// 未设置 disable_parallel_tool_use -> 不写 parallel_tool_calls
	raw2 := `{"model":"m","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"auto"}}`
	var cr2 ClaudeRequest
	if err := json.Unmarshal([]byte(raw2), &cr2); err != nil {
		t.Fatal(err)
	}
	body2 := claudeToResponsesBody(cr2, "m")
	var req2 map[string]any
	if err := json.Unmarshal(body2, &req2); err != nil {
		t.Fatal(err)
	}
	if _, exists := req2["parallel_tool_calls"]; exists {
		t.Fatalf("parallel_tool_calls should be omitted, got %#v", req2["parallel_tool_calls"])
	}
}

// ---------- H5: claudeToResponsesBody store/include + reasoning signature ----------

func TestClaudeBridge_ToResponsesBody_StoreFalseAndInclude(t *testing.T) {
	var cr ClaudeRequest
	raw := `{"model":"m","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		t.Fatal(err)
	}
	body := claudeToResponsesBody(cr, "m")
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if req["store"] != false {
		t.Fatalf("store = %#v, want false", req["store"])
	}
	inc, _ := req["include"].([]any)
	found := false
	for _, v := range inc {
		if s, _ := v.(string); s == "reasoning.encrypted_content" {
			found = true
		}
	}
	if !found {
		t.Fatalf("include = %#v, want reasoning.encrypted_content", req["include"])
	}
}

func TestClaudeBridge_ConvertResponsesToClaude_ReasoningSignature(t *testing.T) {
	resp := `{"id":"msg_x2","model":"m","status":"completed","output":[
		{"type":"reasoning","summary":[{"type":"summary_text","text":"think so"}],"encrypted_content":"sig_abc"},
		{"type":"message","content":[{"type":"output_text","text":"answer"}]}
	]}`
	out := convertResponsesToClaude([]byte(resp), "m", true)
	var cr ClaudeResponse
	if err := json.Unmarshal(out, &cr); err != nil {
		t.Fatal(err)
	}
	if len(cr.Content) < 2 {
		t.Fatalf("content = %#v", cr.Content)
	}
	var thinking *ClaudeContent
	for i := range cr.Content {
		if cr.Content[i].Type == "thinking" {
			thinking = &cr.Content[i]
		}
	}
	if thinking == nil {
		t.Fatalf("thinking block missing: %#v", cr.Content)
	}
	if thinking.Thinking != "think so" {
		t.Fatalf("thinking = %q", thinking.Thinking)
	}
	if thinking.Signature != "sig_abc" {
		t.Fatalf("signature = %q, want sig_abc", thinking.Signature)
	}
}

// ---------- H6: convertStreamChunkWithUsage 客户端 include_usage 控制 ----------

func TestClaudeBridge_StreamUsageChunk_ClientGate(t *testing.T) {
	line := `data: {"id":"x","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`

	// 客户端未请求 include_usage：usage chunk 抑制，但 usage 仍解析出来供统计。
	out, usage := convertStreamChunkWithUsage(line, true, false)
	if out != "" {
		t.Fatalf("usage-only chunk should be dropped when client did not request it, got %q", out)
	}
	if usage == nil {
		t.Fatal("usage should still be extracted for stats")
	}

	// 客户端请求了 include_usage：透传。
	out2, usage2 := convertStreamChunkWithUsage(line, true, true)
	if out2 == "" {
		t.Fatal("usage-only chunk should be forwarded when client requested it")
	}
	if usage2 == nil {
		t.Fatal("usage missing")
	}

	// 带 choices 的 chunk 永远透传。
	line2 := `data: {"id":"x","choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`
	out3, _ := convertStreamChunkWithUsage(line2, true, false)
	if !strings.Contains(out3, `"content":"hi"`) {
		t.Fatalf("content chunk should pass through, got %q", out3)
	}
}

func TestClaudeBridge_ClientStreamUsageWanted(t *testing.T) {
	if !clientStreamUsageWanted([]byte(`{"stream_options":{"include_usage":true}}`)) {
		t.Fatal("top-level include_usage=true should be detected")
	}
	if !clientStreamUsageWanted([]byte(`{"extra_body":{"stream_options":{"include_usage":true}}}`)) {
		t.Fatal("extra_body include_usage=true should be detected")
	}
	if clientStreamUsageWanted([]byte(`{"stream_options":{"include_usage":false}}`)) {
		t.Fatal("include_usage=false should not be treated as wanted")
	}
	if clientStreamUsageWanted([]byte(`{}`)) {
		t.Fatal("missing stream_options should default to false")
	}
}
