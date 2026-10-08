package app

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ======================== 对话中 system 轮（P0） ========================

// TestChatMessagesToAnthropic_MidConversationSystemInlinesReminder 钉死对话
// 位置的 system 轮放置规则（对齐 Bifrost chat.go:891-924）：首条 user 轮之前
// 的 system 提取为顶层 system；对话中间出现的 system 原地转 <system-reminder>
// 包裹的 user 轮（不再全量 join 到顶层 system——顶层 system 在每条消息之前
// 渲染,中途增长会使其后的缓存前缀整体失效）。相邻同 role user 消息按既有
// merge 行为合并,断言只看消息角色与块内容位置。
func TestChatMessagesToAnthropic_MidConversationSystemInlinesReminder(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "lead sys"},
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "system", Content: "mid sys"},
		{Role: "user", Content: "q2"},
	}
	system, out := chatMessagesToAnthropic(msgs, true)
	if system != "lead sys" {
		t.Fatalf("顶层 system = %q, want 首条前的 lead sys", system)
	}
	// user(q1) / assistant(a1) / user(reminder+q2 合并)。
	if len(out) != 3 {
		t.Fatalf("messages = %#v, want 3", out)
	}
	for _, m := range out {
		if role, _ := m["role"].(string); role == "system" {
			t.Fatalf("对话中段 system 不得出现在 messages: %#v", out)
		}
	}
	// 中段 system 变成 user 轮,文本包进 <system-reminder> 信封,且在 a1 之后。
	reminderMsg := out[2]
	if reminderMsg["role"] != "user" {
		t.Fatalf("中段 system 角色 = %#v, want user", reminderMsg["role"])
	}
	blocks, _ := reminderMsg["content"].([]map[string]any)
	reminderText := ""
	q2Text := ""
	q2AfterReminder := false
	for i, b := range blocks {
		if s, _ := b["text"].(string); strings.Contains(s, "system-reminder") {
			reminderText = s
			q2AfterReminder = i < len(blocks)-1
		}
		if s, _ := b["text"].(string); s == "q2" {
			q2Text = s
		}
	}
	if !strings.HasPrefix(reminderText, "<system-reminder>\nmid sys\n</system-reminder>") {
		t.Fatalf("reminder text = %q", reminderText)
	}
	if q2Text == "" || !q2AfterReminder {
		t.Fatalf("reminder 与后续 user 轮错位: %#v", blocks)
	}
	if strings.Contains(reminderText, "q1") || strings.Contains(system, "mid sys") {
		t.Fatalf("中段 system 被提升到顶层: system=%q", system)
	}
	// 首条 user 文本原位保留。
	if b, _ := out[0]["content"].([]map[string]any); len(b) != 1 || b[0]["text"] != "q1" {
		t.Fatalf("first user = %#v", out[0])
	}
}

// TestChatMessagesToAnthropic_AllSystemDegradesToSingleUser 钉死全-system
// （无任何 user/assistant 轮）的降级：单个 user 消息承载 system 文本,
// 禁止编出 messages:[]（对齐 Bifrost chat.go:985-992,Anthropic 不接受空
// messages）。
func TestChatMessagesToAnthropic_AllSystemDegradesToSingleUser(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "sys one"},
		{Role: "developer", Content: "sys two"},
	}
	system, out := chatMessagesToAnthropic(msgs, true)
	if system != "" {
		t.Fatalf("system = %q, want 空（已降级）", system)
	}
	if len(out) != 1 {
		t.Fatalf("messages = %#v, want 1", out)
	}
	if out[0]["role"] != "user" {
		t.Fatalf("role = %#v, want user", out[0]["role"])
	}
	blocks, _ := out[0]["content"].([]map[string]any)
	if len(blocks) != 1 || blocks[0]["text"] != "sys one\n\nsys two" {
		t.Fatalf("degraded content = %#v", blocks)
	}
}

// TestChatToAnthropicBody_MidConversationSystemPlacement 端到端钉死请求体：
// 中段 system 不进顶层 system 键,messages 非空;全-system 请求体 messages 为
// 单条 user。
func TestChatToAnthropicBody_MidConversationSystemPlacement(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-x",
		Messages: []Message{
			{Role: "system", Content: "lead"},
			{Role: "user", Content: "hi"},
			{Role: "system", Content: "mid"},
		},
	}
	var got map[string]any
	if err := json.Unmarshal(chatToAnthropicBody(req, "claude-x", true), &got); err != nil {
		t.Fatal(err)
	}
	sys, _ := got["system"].(string)
	if sys != "lead" || strings.Contains(sys, "mid") {
		t.Fatalf("system = %q, want lead only", sys)
	}
	msgRaw, _ := json.Marshal(got["messages"])
	// 中段 system 内联为 user 轮后与相邻 user 消息合并（既有 merge 行为）:
	// 一条 user 消息同时含 hi 与 <system-reminder> 信封的 mid。
	if !strings.Contains(string(msgRaw), "system-reminder") || !strings.Contains(string(msgRaw), "mid") {
		t.Fatalf("中段 system 未内联: %s", msgRaw)
	}
	msgList, _ := got["messages"].([]any)
	if len(msgList) != 1 {
		t.Fatalf("messages = %#v, want 1 (相邻 user 合并)", msgList)
	}

	// 全-system 请求体：messages 单条 user,顶层 system 键不出现。
	req2 := &OpenAIRequest{
		Model:    "claude-x",
		Messages: []Message{{Role: "system", Content: "only sys"}},
	}
	var got2 map[string]any
	if err := json.Unmarshal(chatToAnthropicBody(req2, "claude-x", true), &got2); err != nil {
		t.Fatal(err)
	}
	if _, has := got2["system"]; has {
		t.Fatalf("全-system 请求不应有顶层 system 键: %#v", got2["system"])
	}
	msgList2, _ := got2["messages"].([]any)
	if len(msgList2) != 1 {
		t.Fatalf("全-system messages = %#v, want 1 条 user", msgList2)
	}
	only, _ := msgList2[0].(map[string]any)
	if only["role"] != "user" {
		t.Fatalf("全-system messages role = %#v, want user", only["role"])
	}
}

// TestChatMessagesToAnthropic_MidConversationSystemKeepsToolPairing 钉死中段
// system 内联为 user 轮后 tool_use/tool_result 配对不被破坏（tool_result 仍
// 与 tool_use 相邻）。
func TestChatMessagesToAnthropic_MidConversationSystemKeepsToolPairing(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "run"},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "call_1", Type: "function",
			Function: FunctionCall{Name: "shell", Arguments: `{"cmd":"ls"}`},
		}}},
		{Role: "system", Content: "mid sys"},
		{Role: "tool", ToolCallID: "call_1", Content: "ok"},
	}
	_, out := chatMessagesToAnthropic(msgs, true)
	// assistant 之后的中段 system 内联为 user 轮,与后续 tool_result 合并。
	if len(out) != 3 {
		t.Fatalf("messages = %#v, want 3 (user/assistant/user[reminder+tool_result] 合并)", out)
	}
	usr := out[2]
	blocks, _ := usr["content"].([]map[string]any)
	var sawReminder, sawToolResult bool
	for _, b := range blocks {
		switch b["type"] {
		case "tool_result":
			sawToolResult = true
		case "text":
			if strings.Contains(b["text"].(string), "system-reminder") {
				sawReminder = true
			}
		}
	}
	if !sawReminder || !sawToolResult {
		t.Fatalf("merged user blocks = %#v, want reminder + tool_result", blocks)
	}
	// tool_result 必须在消息首位:紧跟 assistant tool_use 的 user 消息要求以
	// tool_result 开头（Anthropic 校验 "Did not find N `tool_result` block(s)
	// at the beginning of this message"）,<system-reminder> 文本随其后。
	if blocks[0]["type"] != "tool_result" {
		t.Fatalf("tool_result 不在消息首位: %#v", blocks)
	}
}

// ======================== document / file 映射（P1） ========================

// TestChatTextToAnthropicContent_FileToDocumentBlock 钉死 Chat file part →
// Anthropic document 块的映射（对齐 Bifrost ConvertToAnthropicDocumentBlock）：
// 此前 file/document 走 default 丢弃,doc-only 消息整条消失。
func TestChatTextToAnthropicContent_FileToDocumentBlock(t *testing.T) {
	pdfB64 := "JVBERi0xLjQK" // %PDF-1.4
	blocks, ok := chatTextToAnthropicContent([]any{
		map[string]any{"type": "file", "file": map[string]any{
			"filename": "report.pdf", "file_type": "application/pdf",
			"file_data": "data:application/pdf;base64," + pdfB64,
		}},
	})
	if !ok {
		t.Fatal("doc-only 消息不应整体消失")
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks = %#v, want [\".\"占位, document]", blocks)
	}
	if blocks[0]["type"] != "text" || blocks[0]["text"] != documentPlaceholderText {
		t.Fatalf("placeholder = %#v", blocks[0])
	}
	doc := blocks[1]
	if doc["type"] != "document" || doc["title"] != "report.pdf" {
		t.Fatalf("document block = %#v", doc)
	}
	src, _ := doc["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "application/pdf" || src["data"] != pdfB64 {
		t.Fatalf("document source = %#v", src)
	}
}

// TestChatTextToAnthropicContent_DocumentSourcesVariants 表驱动钉死
// file_id / file_url / Anthropic 风格 source / text data URL 的 document 映射。
func TestChatTextToAnthropicContent_DocumentSourcesVariants(t *testing.T) {
	tests := []struct {
		name    string
		part    map[string]any
		srcType string
		check   func(t *testing.T, src map[string]any, doc map[string]any)
	}{
		{
			name: "file_id 引用",
			part: map[string]any{"type": "file", "file": map[string]any{
				"filename": "a.pdf", "file_id": "file-123",
			}},
			srcType: "file",
			check: func(t *testing.T, src map[string]any, doc map[string]any) {
				if src["file_id"] != "file-123" {
					t.Fatalf("file_id source = %#v", src)
				}
			},
		},
		{
			name: "file_url 引用",
			part: map[string]any{"type": "file", "file": map[string]any{
				"file_url": "https://example.test/doc.pdf",
			}},
			srcType: "url",
			check: func(t *testing.T, src map[string]any, doc map[string]any) {
				if src["url"] != "https://example.test/doc.pdf" {
					t.Fatalf("url source = %#v", src)
				}
			},
		},
		{
			name: "Anthropic 风格 source(base64)",
			part: map[string]any{"type": "document",
				"title":  "notes.txt",
				"source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": "JVBERi0="},
			},
			srcType: "base64",
			check: func(t *testing.T, src map[string]any, doc map[string]any) {
				if src["data"] != "JVBERi0=" || src["media_type"] != "application/pdf" {
					t.Fatalf("base64 source = %#v", src)
				}
				if doc["title"] != "notes.txt" {
					t.Fatalf("title = %#v", doc)
				}
			},
		},
		{
			name: "text data URL 解码为 text source",
			part: map[string]any{"type": "file", "file": map[string]any{
				"filename":  "notes.md",
				"file_data": "data:text/plain;base64," + base64Of("hello doc"),
			}},
			srcType: "text",
			check: func(t *testing.T, src map[string]any, doc map[string]any) {
				if src["media_type"] != "text/plain" || src["data"] != "hello doc" {
					t.Fatalf("text source = %#v", src)
				}
			},
		},
		{
			name: "无 file_type 的纯 base64 缺省 application/pdf",
			part: map[string]any{"type": "file", "file": map[string]any{
				"file_data": "JVBERi0xLjQK",
			}},
			srcType: "base64",
			check: func(t *testing.T, src map[string]any, doc map[string]any) {
				if src["data"] != "JVBERi0xLjQK" || src["media_type"] != "application/pdf" {
					t.Fatalf("default source = %#v", src)
				}
			},
		},
		{
			name: "纯文本 file_data 按 text/plain 落 text source",
			part: map[string]any{"type": "file", "file": map[string]any{
				"file_type": "text/plain", "file_data": "plain body",
			}},
			srcType: "text",
			check: func(t *testing.T, src map[string]any, doc map[string]any) {
				if src["data"] != "plain body" || src["media_type"] != "text/plain" {
					t.Fatalf("plain text source = %#v", src)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocks, ok := chatTextToAnthropicContent([]any{tt.part})
			if !ok {
				t.Fatalf("blocks 为空: %#v", blocks)
			}
			// 占位 + document（占位在 index 0）。
			var doc map[string]any
			for _, b := range blocks {
				if b["type"] == "document" {
					doc = b
				}
			}
			if doc == nil {
				t.Fatalf("document block missing: %#v", blocks)
			}
			src, _ := doc["source"].(map[string]any)
			if src["type"] != tt.srcType {
				t.Fatalf("source.type = %#v, want %q", src["type"], tt.srcType)
			}
			tt.check(t, src, doc)
		})
	}
}

// TestChatTextToAnthropicContent_DocumentOnlyPlaceholder 钉死 doc-only 消息
// 的 "." 占位（document 块必须伴随 text 块,对齐 Bifrost）,以及 doc + text
// 共存时不再重复占位。
func TestChatTextToAnthropicContent_DocumentOnlyPlaceholder(t *testing.T) {
	docPart := map[string]any{"type": "file", "file": map[string]any{
		"file_id": "file-1",
	}}
	// doc-only：占位文本在 index 0。
	blocks, ok := chatTextToAnthropicContent([]any{docPart})
	if !ok || blocks[0]["type"] != "text" || blocks[0]["text"] != "." {
		t.Fatalf("doc-only placeholder = %#v (ok=%v)", blocks, ok)
	}
	// doc + text：不重复占位,text 原位。
	blocks2, ok2 := chatTextToAnthropicContent([]any{
		map[string]any{"type": "text", "text": "read this"},
		docPart,
	})
	if !ok2 || len(blocks2) != 2 || blocks2[0]["text"] != "read this" {
		t.Fatalf("doc+text blocks = %#v (ok=%v)", blocks2, ok2)
	}
	for _, b := range blocks2 {
		if b["type"] == "text" && b["text"] == "." {
			t.Fatalf("已有 text 仍补占位: %#v", blocks2)
		}
	}
	// 无法解析的 file part：计数丢弃并兜底占位,消息不整条消失。
	blocks3, ok3 := chatTextToAnthropicContent([]any{
		map[string]any{"type": "file"},
	})
	if !ok3 || len(blocks3) != 1 || blocks3[0]["text"] != documentPlaceholderText {
		t.Fatalf("unrepresentable file fallback = %#v (ok=%v)", blocks3, ok3)
	}
}

// ======================== 图片 URL 白名单（P2） ========================

// TestImageURLToAnthropicBlock_SchemeWhitelist 表驱动钉死图片 URL 清洗
// （对齐 Bifrost SanitizeImageURL/ExtractURLTypeInfo）：http(s) 白名单放行
// 透传;白名单外 scheme（file:// 等）丢弃;非 base64 data URI 不再直接丢弃,
// 归 url source 透传;裸 base64 包成 data URI。
func TestImageURLToAnthropicBlock_SchemeWhitelist(t *testing.T) {
	rawB64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8AAAwAB/AF+G0/eAAAAAElFTkSuQmCC"
	tests := []struct {
		name    string
		url     string
		wantNil bool
		srcType string
	}{
		{"https 白名单放行", "https://example.test/a.png", false, "url"},
		{"http 白名单放行", "http://example.test/a.png", false, "url"},
		{"file:// 白名单外丢弃", "file:///etc/passwd", true, ""},
		{"ftp:// 白名单外丢弃", "ftp://example.test/a.png", true, ""},
		{"无 scheme 丢弃", "example.test/a.png", true, ""},
		{"base64 data URI 归 base64 source", "data:image/png;base64," + rawB64, false, "base64"},
		{"非 base64 data URI 透传 url source", "data:image/svg+xml,%3Csvg%3E%3C/svg%3E", false, "url"},
		{"无 media type 畸形 data URI 丢弃", "data:;not-base64,xxxx", true, ""},
		{"裸 base64 包成 data URI", rawB64, false, "base64"},
		{"空 url 丢弃", "", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			block := imageURLToAnthropicBlock(tt.url)
			if tt.wantNil {
				if block != nil {
					t.Fatalf("block = %#v, want nil", block)
				}
				return
			}
			if block == nil {
				t.Fatal("block 丢失")
			}
			src, _ := block["source"].(map[string]any)
			if src["type"] != tt.srcType {
				t.Fatalf("source.type = %#v, want %q", src["type"], tt.srcType)
			}
			if tt.srcType == "url" {
				if _, ok := src["url"].(string); !ok || src["url"] == "" {
					t.Fatalf("url source = %#v", src)
				}
			}
		})
	}
	// 裸 base64 包成 data URI 后 media type 按签名嗅探为 png。
	block := imageURLToAnthropicBlock(rawB64)
	src, _ := block["source"].(map[string]any)
	if src["media_type"] != "image/png" || src["data"] != rawB64 {
		t.Fatalf("raw base64 source = %#v", src)
	}
	// https URL 原样透传（不重写路径）。
	if got := imageURLToAnthropicBlock("https://example.test/a.png"); got != nil {
		if src, _ := got["source"].(map[string]any); src["url"] != "https://example.test/a.png" {
			t.Fatalf("https passthrough = %#v", src)
		}
	}
}

// TestChatTextToAnthropicContent_ImageEdgeCases 的既有断言位于
// chat_bridge_test.go（本批次更新其断言以反映白名单行为）。

// ======================== 逐块 cache_control 透传（P2） ========================

// TestChatTextToAnthropicContent_CacheControlPassthrough 钉死客户端逐块携带
// 的 cache_control breakpoint 透传（此前仅顶层注入一次,块级断点丢失）。
func TestChatTextToAnthropicContent_CacheControlPassthrough(t *testing.T) {
	cc := map[string]any{"type": "ephemeral"}
	blocks, ok := chatTextToAnthropicContent([]any{
		map[string]any{"type": "text", "text": "anchor", "cache_control": cc},
		map[string]any{"type": "text", "text": "no marker"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/a.png"}, "cache_control": cc},
	})
	if !ok || len(blocks) != 3 {
		t.Fatalf("blocks = %#v (ok=%v)", blocks, ok)
	}
	if got, _ := blocks[0]["cache_control"].(map[string]any); got == nil || got["type"] != "ephemeral" {
		t.Fatalf("text cache_control lost: %#v", blocks[0])
	}
	if _, has := blocks[1]["cache_control"]; has {
		t.Fatalf("无断点的 part 不应发明 cache_control: %#v", blocks[1])
	}
	if got, _ := blocks[2]["cache_control"].(map[string]any); got == nil || got["type"] != "ephemeral" {
		t.Fatalf("image cache_control lost: %#v", blocks[2])
	}
}

// TestChatToAnthropicBody_PerBlockCacheControlEndToEnd 端到端钉死请求体里
// 内容块携带 cache_control（经 chat→anthropic 转换与 body marshal 后保留）。
func TestChatToAnthropicBody_PerBlockCacheControlEndToEnd(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-x",
		Messages: []Message{
			{Role: "user", Content: []any{
				map[string]any{"type": "text", "text": "long context", "cache_control": map[string]any{"type": "ephemeral"}},
			}},
		},
	}
	body := chatToAnthropicBody(req, "claude-x", true)
	var got struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 1 || len(got.Messages[0].Content) != 1 {
		t.Fatalf("messages = %#v", got.Messages)
	}
	cc, ok := got.Messages[0].Content[0]["cache_control"].(map[string]any)
	if !ok || cc["type"] != "ephemeral" {
		t.Fatalf("cache_control lost in body: %#v", got.Messages[0].Content[0])
	}
}

// TestChatMessagesToAnthropic_MidConvSystemCarriesCacheControl 钉死中段
// system 内联 user 轮时块内 cache_control 断点折叠到最后一块（对齐 Bifrost
// inlineMidConversationSystem——同一消息内中间断点不买任何东西）。相邻同
// role user 消息按既有 merge 行为合并,断言在合并后的 content 里做。
func TestChatMessagesToAnthropic_MidConvSystemCarriesCacheControl(t *testing.T) {
	cc := map[string]any{"type": "ephemeral"}
	msgs := []Message{
		{Role: "user", Content: "hi"},
		{Role: "system", Content: []any{
			map[string]any{"type": "text", "text": "first", "cache_control": cc},
			map[string]any{"type": "text", "text": "second"},
		}},
		{Role: "user", Content: "next"},
	}
	_, out := chatMessagesToAnthropic(msgs, true)
	if len(out) != 1 {
		t.Fatalf("messages = %#v, want 1 合并 user", out)
	}
	blocks, _ := out[0]["content"].([]map[string]any)
	var reminderIdx []int
	for i, b := range blocks {
		if s, _ := b["text"].(string); strings.Contains(s, "system-reminder") {
			reminderIdx = append(reminderIdx, i)
		}
	}
	if len(reminderIdx) != 2 {
		t.Fatalf("reminder blocks = %#v, want 2 段 reminder", blocks)
	}
	first, last := blocks[reminderIdx[0]], blocks[reminderIdx[len(reminderIdx)-1]]
	if _, has := first["cache_control"]; has {
		t.Fatalf("中间块不应保留断点: %#v", first)
	}
	if got, _ := last["cache_control"].(map[string]any); got == nil || got["type"] != "ephemeral" {
		t.Fatalf("最后一块应折叠断点: %#v", last)
	}
}

// TestChatToolResultBlock_CacheControlHoisted 钉死 tool_result content part
// 上的逐块 cache_control breakpoint 提升到 tool_result 块本身（Anthropic 拒绝
// 嵌在 tool_result.content 里的块级断点——"cache_control may not be specified
// within `tool_result.content`. Instead, place it directly on `tool_result`",
// 对齐 Bifrost chat.go:1021-1029）,嵌套块不落副本,客户端仍拿到一个断点。
func TestChatToolResultBlock_CacheControlHoisted(t *testing.T) {
	cc := map[string]any{"type": "ephemeral"}
	msgs := []Message{
		{Role: "user", Content: "run"},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "call_1", Type: "function",
			Function: FunctionCall{Name: "shell", Arguments: `{}`},
		}}},
		{Role: "tool", ToolCallID: "call_1", Content: []any{
			map[string]any{"type": "text", "text": "output", "cache_control": cc},
			map[string]any{"type": "text", "text": "plain"},
		}},
	}
	_, out := chatMessagesToAnthropic(msgs, true)
	if len(out) != 3 {
		t.Fatalf("messages = %#v, want 3", out)
	}
	blocks, _ := out[2]["content"].([]map[string]any)
	tr, _ := blocks[0]["content"].([]map[string]any)
	if len(tr) != 2 {
		t.Fatalf("tool_result content = %#v", tr)
	}
	// 嵌套块不落断点:Anthropic 拒绝 tool_result.content 内嵌 cache_control。
	for i, b := range tr {
		if _, has := b["cache_control"]; has {
			t.Fatalf("嵌套块不应保留 cache_control: tr[%d] = %#v", i, b)
		}
	}
	// 提升位:断点落在 tool_result 块本身。
	if got, _ := blocks[0]["cache_control"].(map[string]any); got == nil || got["type"] != "ephemeral" {
		t.Fatalf("tool_result cache_control hoist lost: %#v", blocks[0])
	}
}

// ======================== usage 口径（P2） ========================

// TestAnthropicUsageToChat_ServerToolUseAndCacheWrite 表驱动钉死两处计费
// 修复：server_tool_use.web_search_requests → tool_usage/num_search_queries;
// cache_creation 5m/1h 明细 → cached_write_token_details + 扁平键合成。
func TestAnthropicUsageToChat_ServerToolUseAndCacheWrite(t *testing.T) {
	t.Run("server_tool_use 归位", func(t *testing.T) {
		out := anthropicUsageToChat(map[string]any{
			"input_tokens": float64(10), "output_tokens": float64(5),
			"server_tool_use": map[string]any{"web_search_requests": float64(3)},
		})
		if got, ok := out["tool_usage"].(map[string]any); !ok {
			t.Fatalf("tool_usage missing: %#v", out)
		} else if ws, ok := got["web_search"].(map[string]any); !ok || ws["num_requests"] != float64(3) {
			t.Fatalf("tool_usage.web_search = %#v", got)
		}
		details, _ := out["completion_tokens_details"].(map[string]any)
		if details["num_search_queries"] != float64(3) {
			t.Fatalf("num_search_queries = %#v", details)
		}
		// 原顶层键透传保留。
		if _, ok := out["server_tool_use"]; !ok {
			t.Fatalf("顶层 server_tool_use 应透传: %#v", out)
		}
	})

	t.Run("cache_creation 5m/1h 明细", func(t *testing.T) {
		out := anthropicUsageToChat(map[string]any{
			"input_tokens": float64(100), "output_tokens": float64(20),
			"cache_creation": map[string]any{
				"ephemeral_5m_input_tokens": float64(10),
				"ephemeral_1h_input_tokens": float64(4),
			},
		})
		details, _ := out["prompt_tokens_details"].(map[string]any)
		writeDetails, _ := details["cached_write_token_details"].(map[string]any)
		if writeDetails == nil {
			t.Fatalf("cached_write_token_details missing: %#v", details)
		}
		if writeDetails["cached_write_tokens_5m"] != float64(10) || writeDetails["cached_write_tokens_1h"] != float64(4) {
			t.Fatalf("write details = %#v", writeDetails)
		}
		// 扁平键缺失时以 5m+1h 合成（计费/统计只认扁平键）。
		if out["cache_creation_input_tokens"] != float64(14) {
			t.Fatalf("synthesized cache_creation_input_tokens = %#v", out["cache_creation_input_tokens"])
		}
		if details["cache_creation_tokens"] != float64(14) {
			t.Fatalf("cache_creation_tokens = %#v", details)
		}
	})

	t.Run("扁平键优先不被 5m/1h 覆盖", func(t *testing.T) {
		out := anthropicUsageToChat(map[string]any{
			"input_tokens":                float64(100),
			"cache_creation_input_tokens": float64(15),
			"cache_creation": map[string]any{
				"ephemeral_5m_input_tokens": float64(10),
				"ephemeral_1h_input_tokens": float64(4),
			},
		})
		if out["cache_creation_input_tokens"] != float64(15) {
			t.Fatalf("flat key overwritten: %#v", out["cache_creation_input_tokens"])
		}
		details, _ := out["prompt_tokens_details"].(map[string]any)
		if details["cache_creation_tokens"] != float64(15) {
			t.Fatalf("cache_creation_tokens = %#v", details)
		}
		writeDetails, _ := details["cached_write_token_details"].(map[string]any)
		if writeDetails["cached_write_tokens_5m"] != float64(10) || writeDetails["cached_write_tokens_1h"] != float64(4) {
			t.Fatalf("write details = %#v", writeDetails)
		}
	})

	t.Run("search 与 reasoning 共存", func(t *testing.T) {
		out := anthropicUsageToChat(map[string]any{
			"input_tokens": float64(10), "output_tokens": float64(5),
			"output_tokens_details": map[string]any{"thinking_tokens": float64(2)},
			"server_tool_use":       map[string]any{"web_search_requests": float64(1)},
		})
		details, _ := out["completion_tokens_details"].(map[string]any)
		if details["reasoning_tokens"] != float64(2) || details["num_search_queries"] != float64(1) {
			t.Fatalf("completion_tokens_details = %#v", details)
		}
	})
}

// TestChatUsageMapToResponses_CacheWriteDetailsRoundtrip 钉死 Chat → Responses
// 的缓存写明细往返：此前 cache_creation 5m/1h 整段丢弃,现归位到
// input_tokens_details.cached_write_token_details。
func TestChatUsageMapToResponses_CacheWriteDetailsRoundtrip(t *testing.T) {
	u := map[string]any{
		"prompt_tokens":               float64(100),
		"completion_tokens":           float64(20),
		"total_tokens":                float64(120),
		"cache_read_input_tokens":     float64(30),
		"cache_creation_input_tokens": float64(14),
		"prompt_tokens_details": map[string]any{
			"cached_tokens": float64(30), "cache_creation_tokens": float64(14),
			"cached_write_token_details": map[string]any{
				"cached_write_tokens_5m": float64(10), "cached_write_tokens_1h": float64(4),
			},
		},
	}
	usage := chatUsageMapToResponses(u)
	details, _ := usage["input_tokens_details"].(map[string]any)
	if details == nil {
		t.Fatalf("input_tokens_details missing: %#v", usage)
	}
	writeDetails, _ := details["cached_write_token_details"].(map[string]any)
	if writeDetails == nil {
		t.Fatalf("cached_write_token_details dropped: %#v", details)
	}
	if writeDetails["cached_write_tokens_5m"] != float64(10) || writeDetails["cached_write_tokens_1h"] != float64(4) {
		t.Fatalf("write details = %#v", writeDetails)
	}
	// chatUsageMapToResponses 把 cached_tokens 重写为 int64;其余字段原样透传。
	if v, ok := numberAsFloat(details["cached_tokens"]); !ok || v != 30 {
		t.Fatalf("cached_tokens = %#v", details["cached_tokens"])
	}
	if v, ok := numberAsFloat(details["cache_creation_tokens"]); !ok || v != 14 {
		t.Fatalf("cache_creation_tokens = %#v", details["cache_creation_tokens"])
	}

	// chat usage 顶层 cache_creation object 也归位（不整段丢弃）。
	u2 := map[string]any{
		"prompt_tokens": float64(50), "completion_tokens": float64(5),
		"cache_creation": map[string]any{
			"ephemeral_5m_input_tokens": float64(7), "ephemeral_1h_input_tokens": float64(2),
		},
	}
	usage2 := chatUsageMapToResponses(u2)
	details2, _ := usage2["input_tokens_details"].(map[string]any)
	writeDetails2, _ := details2["cached_write_token_details"].(map[string]any)
	if writeDetails2 == nil || writeDetails2["cached_write_tokens_5m"] != float64(7) || writeDetails2["cached_write_tokens_1h"] != float64(2) {
		t.Fatalf("top-level cache_creation roundtrip = %#v", details2)
	}
}

// TestMergeUsage_NestedCountersMaxMerged 钉死 server_tool_use / cache_creation
// 嵌套计数 map 的 max 合并（对齐 Bifrost passthrough_usage——Anthropic 把
// usage 拆到 message_start 与 message_delta,计数单调增长,按 max 合并与事件
// 顺序无关）。
func TestMergeUsage_NestedCountersMaxMerged(t *testing.T) {
	full := map[string]any{
		"input_tokens":    float64(10),
		"server_tool_use": map[string]any{"web_search_requests": float64(2)},
		"cache_creation": map[string]any{
			"ephemeral_5m_input_tokens": float64(5), "ephemeral_1h_input_tokens": float64(0),
		},
	}
	mergeUsage(full, map[string]any{
		"output_tokens":   float64(7),
		"server_tool_use": map[string]any{"web_search_requests": float64(5)},
		"cache_creation": map[string]any{
			"ephemeral_5m_input_tokens": float64(3), "ephemeral_1h_input_tokens": float64(4),
		},
	})
	stu, _ := full["server_tool_use"].(map[string]any)
	if stu["web_search_requests"] != float64(5) {
		t.Fatalf("server_tool_use 应取 max: %#v", stu)
	}
	cc, _ := full["cache_creation"].(map[string]any)
	if cc["ephemeral_5m_input_tokens"] != float64(5) || cc["ephemeral_1h_input_tokens"] != float64(4) {
		t.Fatalf("cache_creation 应逐键取 max: %#v", cc)
	}
	// 嵌套值不变时旧值保留（max 语义,非覆盖为 0）。
	full2 := map[string]any{
		"server_tool_use": map[string]any{"web_search_requests": float64(9)},
	}
	mergeUsage(full2, map[string]any{
		"server_tool_use": map[string]any{"web_search_requests": float64(1)},
	})
	if stu2, _ := full2["server_tool_use"].(map[string]any); stu2["web_search_requests"] != float64(9) {
		t.Fatalf("max 合并被覆盖: %#v", stu2)
	}
}

// ======================== 辅助 ========================

func base64Of(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// TestForwardChatViaAnthropic_DocumentAndSystemPlacement 端到端钉死 chat 入站
// 经 anthropic 上游的请求体（中段 system 内联 + document 块存活 + 块级
// cache_control 透传）。fake 上游捕获请求载荷验证。
func TestForwardChatViaAnthropic_DocumentAndSystemPlacement(t *testing.T) {
	stubRetryConfig(t, 0, 5000)
	stubAnthropicRoute(t, "claude-*")
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"claude-x","stream":false,"messages":[
			{"role":"system","content":"lead sys"},
			{"role":"user","content":[
				{"type":"text","text":"see this","cache_control":{"type":"ephemeral"}},
				{"type":"file","file":{"filename":"doc.pdf","file_type":"application/pdf","file_data":"JVBERi0xLjQK"}}
			]},
			{"role":"system","content":"mid sys"}
		]}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(transport.requestPayloads) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(transport.requestPayloads))
	}
	sent, _ := json.Marshal(transport.requestPayloads[0])
	if sys, _ := transport.requestPayloads[0]["system"].(string); sys != "lead sys" || strings.Contains(sys, "mid sys") {
		t.Fatalf("upstream system = %q", sys)
	}
	// json.Marshal 转义 < >,按信封词断言。
	if !strings.Contains(string(sent), "system-reminder") || !strings.Contains(string(sent), "mid sys") {
		t.Fatalf("中段 system 未内联: %s", sent)
	}
	if !strings.Contains(string(sent), `"document"`) || !strings.Contains(string(sent), "JVBERi0xLjQK") {
		t.Fatalf("document 块丢失: %s", sent)
	}
	if !strings.Contains(string(sent), `"cache_control"`) {
		t.Fatalf("块级 cache_control 丢失: %s", sent)
	}
}
