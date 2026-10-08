package app

import (
	"fmt"
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

func TestStripVolatileTokenCounters(t *testing.T) {
	sr := "<system-reminder>\nCodebase and user instructions are shown below.\n</system-reminder>"
	mkCounter := func(n string) string {
		return "<total_tokens>" + n + " tokens left</total_tokens>"
	}
	mkUser := func(parts ...string) map[string]any {
		ps := make([]any, len(parts))
		for i, s := range parts {
			ps[i] = map[string]any{"type": "text", "text": s}
		}
		return map[string]any{"type": "message", "role": "user", "content": ps}
	}
	// 计数块被剥离、其余文本保留、item 数不变。
	input := []any{
		mkUser(mkCounter("1000")+"\n"+sr, "use the tools"),
		map[string]any{"type": "message", "role": "assistant", "content": []any{
			map[string]any{"type": "output_text", "text": "ok"},
		}},
		mkUser("continue please"),
	}
	if !stripVolatileTokenCountersInPlace(input) {
		t.Fatalf("counter strip should report change")
	}
	if len(input) != 3 {
		t.Fatalf("item count should be unchanged, got %d", len(input))
	}
	firstParts, _ := input[0].(map[string]any)["content"].([]any)
	ft, _ := firstParts[0].(map[string]any)["text"].(string)
	if strings.Contains(ft, "<total_tokens>") || !strings.Contains(ft, "Codebase") {
		t.Fatalf("counter should be stripped, stable text kept, got %q", ft[:40])
	}
	if lt, _ := input[2].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string); lt != "continue please" {
		t.Fatalf("later user should stay as-is, got %q", lt)
	}
	// 无计数块：无改动。
	plain := []any{mkUser(sr), mkUser("hi")}
	if stripVolatileTokenCountersInPlace(plain) {
		t.Fatalf("no-counter input should report no change")
	}
	if p0, _ := plain[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string); p0 != sr {
		t.Fatalf("no-counter input should stay as-is, got %q", p0[:30])
	}
}

func TestStableUserTextFromParts(t *testing.T) {
	srA := "<system-reminder>\nCodebase and user instructions are shown below.\n</system-reminder>"
	srB := "<system-reminder>\nAttribution for git commits.\n</system-reminder>"
	// 注入块在前、真实请求在后：跳过注入块取请求文本。
	parts := []any{
		map[string]any{"type": "text", "text": srA},
		map[string]any{"type": "text", "text": srB},
		map[string]any{"type": "text", "text": "Use the Task tool to count files."},
	}
	if got := stableUserTextFromParts(parts); got != "Use the Task tool to count files." {
		t.Fatalf("want prompt text, got %q", got)
	}
	// 全为注入块：回退首个非空 part 并剥离计数块。
	fallback := []any{
		map[string]any{"type": "text", "text": "<total_tokens>99 tokens left</total_tokens>\n" + srA},
	}
	if got := stableUserTextFromParts(fallback); strings.Contains(got, "<total_tokens>") || !strings.Contains(got, "Codebase") {
		t.Fatalf("fallback should strip counter and keep SR text, got %q", got)
	}
	// 注入块里夹带逐请求计数：非整段 SR 的 part 剥离计数后应稳定。
	mixed := []any{
		map[string]any{"type": "text", "text": "<total_tokens>15000000 tokens left</total_tokens>\n\nnotes"},
	}
	if got := stableUserTextFromParts(mixed); got != "notes" {
		t.Fatalf("want stripped stable text, got %q", got)
	}
}

// chat 形状把计数以 role=system 消息 join 进 messages[0]:strip 必须覆盖
// system/developer,否则 system 消息逐轮漂移(实测 ling 上 system 尾部累积
// 3 处计数),上游前缀缓存从 system 就断。
func TestStripVolatileTokenCountersSystemRole(t *testing.T) {
	msgs := []any{
		map[string]any{
			"role":    "system",
			"content": "SYS_HEAD\n\n<total_tokens>15000000 tokens left</total_tokens>\n\nSYS_MID\n\n<total_tokens>14981101 tokens left</total_tokens>",
		},
		map[string]any{"role": "user", "content": "hello"},
	}
	body := map[string]any{"messages": msgs}
	if !stripVolatileCountersInMap(body) {
		t.Fatalf("strip should report change for system-role counters")
	}
	sys := msgs[0].(map[string]any)["content"].(string)
	if strings.Contains(sys, "<total_tokens>") {
		t.Fatalf("system message still contains counter: %q", sys)
	}
	// regex 连计数块周围空白一起吃(防逐轮累积空行),分隔符随计数消失。
	if sys != "SYS_HEADSYS_MID" {
		t.Fatalf("system message should be trimmed to stable head, got %q", sys)
	}
	// 幂等:再剥一次无改动
	if stripVolatileCountersInMap(body) {
		t.Fatalf("second strip should be a no-op")
	}
}

// chat 形状 system 钉首轮:首轮原文注册原样发;次轮 system 尾部增长(hand-back
// 通知等)时钉回首轮原文、增量挪末条 user 消息,system+tools 前缀跨轮稳定。
// fixture 用 >stableInstrHeadSegs 段的 system:稳定头哈希构成注册表键,段数
// 不足时头部全量取值、键随尾部增量漂移(与真实 CLI 122 段场景不同)。
func TestClipChatSystemStable(t *testing.T) {
	segs := make([]string, 0, 24)
	for i := 0; i < 24; i++ {
		segs = append(segs, fmt.Sprintf("SEG_%02d body text for segment %d", i, i))
	}
	base := strings.Join(segs, "\n\n")
	firstUser := map[string]any{"role": "user", "content": "seed task"}
	build := func(sys string) map[string]any {
		return map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": sys},
			firstUser,
			map[string]any{"role": "assistant", "content": "ok"},
			map[string]any{"role": "user", "content": "next"},
		}}
	}
	// 首轮:注册原样,无增量
	b1 := build(base)
	clipChatSystemStable(b1)
	if got := b1["messages"].([]any)[0].(map[string]any)["content"]; got != base {
		t.Fatalf("first turn should pass through unchanged, got %q", got)
	}
	// 次轮:system 尾部增长 → 钉回首轮原文,增量并入末条 user
	grown := base + "\n\nAnother Claude session sent a message while you were working:\n<agent-message from=\"x\">hi</agent-message>"
	b2 := build(grown)
	clipChatSystemStable(b2)
	msgs := b2["messages"].([]any)
	if got := msgs[0].(map[string]any)["content"]; got != base {
		t.Fatalf("second turn system should be pinned to first-turn original, got %q", got)
	}
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("delta should land on last user message, got role %v", last["role"])
	}
	if c, ok := last["content"].(string); !ok || !strings.HasPrefix(c, "Another Claude session") {
		t.Fatalf("delta should be prepended to last user string content, got %v", last["content"])
	}
	// 第三轮 system 不再增长:钉住 + 无增量,user 消息不被二次污染
	b3 := build(base)
	clipChatSystemStable(b3)
	if got := b3["messages"].([]any)[0].(map[string]any)["content"]; got != base {
		t.Fatalf("third turn system should stay pinned, got %q", got)
	}
	for _, im := range b3["messages"].([]any) {
		mm := im.(map[string]any)
		if mm["role"] == "user" {
			if c, ok := mm["content"].(string); ok && strings.HasPrefix(c, "Another Claude") {
				t.Fatalf("stable turn should not re-append delta to user message")
			}
		}
	}
}
