package app

import (
	"strings"
	"testing"
)

func TestClipInstructionsToStablePrefix(t *testing.T) {
	base := strings.Repeat("seg\n\n", 20) + "tail-seg-20"
	firstUser := "test-first-user-clip"
	// 首轮：未命中注册表，原样返回、无增量。
	stable, delta := clipInstructionsToStablePrefix(base, firstUser)
	if stable != base || delta != "" {
		t.Fatalf("first call: stable=%q delta=%q, want as-is/no-delta", stable[:20], delta)
	}
	// 次轮：追加尾部段 → 返回首轮原文 + 增量。
	grown := base + "\n\nappended-reminder-a"
	stable, delta = clipInstructionsToStablePrefix(grown, firstUser)
	if stable != base {
		t.Fatalf("second call: stable changed, want pinned to first-seen original")
	}
	if delta != "\n\nappended-reminder-a" {
		t.Fatalf("second call: delta=%q, want appended tail", delta)
	}
	// 第三轮继续追加：仍钉住首轮原文。
	grown2 := grown + "\n\nappended-reminder-b"
	stable, delta = clipInstructionsToStablePrefix(grown2, firstUser)
	if stable != base || delta != "\n\nappended-reminder-a\n\nappended-reminder-b" {
		t.Fatalf("third call: stable/delta drifted")
	}
	// 不同 firstUser：不同注册表条目，原样返回。
	other := clipStableTestInstr()
	stable2, delta2 := clipInstructionsToStablePrefix(other, "another-user")
	if delta2 != "" || stable2 != other {
		t.Fatalf("other user: want as-is, got delta=%q", delta2[:20])
	}
}

func clipStableTestInstr() string {
	return strings.Repeat("other\n\n", 20) + "other-tail"
}

func TestAppendInstructionsDeltaToLastUser(t *testing.T) {
	input := []any{
		map[string]any{"type": "message", "role": "user",
			"content": []any{responsesTextPart("user", "first question")}},
		map[string]any{"type": "message", "role": "assistant",
			"content": []any{responsesTextPart("assistant", "answer")}},
		map[string]any{"type": "message", "role": "user",
			"content": []any{responsesTextPart("user", "second question")}},
	}
	out := appendInstructionsDeltaToLastUser(input, "\n\nreminder-delta")
	last := out[2].(map[string]any)
	parts := last["content"].([]any)
	if got := parts[0].(map[string]any)["text"]; got != "\n\nreminder-delta\n\nsecond question" {
		t.Fatalf("delta not prepended to last user text: %q", got)
	}
	// 首 part 非文本（图片开头）：插入文本 part 到最前。
	input2 := []any{
		map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_image", "image_url": "x"}}},
	}
	out2 := appendInstructionsDeltaToLastUser(input2, "delta-2")
	parts2 := out2[0].(map[string]any)["content"].([]any)
	if parts2[0].(map[string]any)["text"] != "delta-2" {
		t.Fatalf("delta not inserted before image part")
	}
	// 无 user item：追加独立 user item。
	input3 := []any{
		map[string]any{"type": "message", "role": "assistant",
			"content": []any{responsesTextPart("assistant", "hi")}},
	}
	out3 := appendInstructionsDeltaToLastUser(input3, "delta-3")
	last3 := out3[len(out3)-1].(map[string]any)
	if last3["role"] != "user" {
		t.Fatalf("want appended user item, got role=%v", last3["role"])
	}
}

func TestContentPromptCacheKey_ToolsNamesIgnoreSchemaDrift(t *testing.T) {
	mk := func(schema string) map[string]any {
		return map[string]any{
			"model":        "m",
			"instructions": "stable instructions",
			"messages":     []any{map[string]any{"role": "user", "content": "hello"}},
			"tools": []any{
				map[string]any{"type": "function", "name": "Bash", "description": "run", "input_schema": schema},
				map[string]any{"type": "function", "name": "Read", "description": "read", "input_schema": schema},
			},
		}
	}
	k1 := contentPromptCacheKey(mk("v1"))
	k2 := contentPromptCacheKey(mk("v2-drifted"))
	if k1 == "" || k1 != k2 {
		t.Fatalf("tool schema drift changed pck: %q vs %q", k1, k2)
	}
	// 工具名集合变化：pck 必须变。
	m3 := mk("v1")
	m3["tools"] = []any{
		map[string]any{"type": "function", "name": "Bash", "input_schema": "v1"},
		map[string]any{"type": "function", "name": "Write", "input_schema": "v1"},
	}
	if k3 := contentPromptCacheKey(m3); k3 == k1 {
		t.Fatalf("tool name set change did not change pck")
	}
}
