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

// SessionStart hook 注入前缀剥离:注入只存在于当轮请求(客户端 session 文件存
// 干净版本,重放历史不带),不剥会让上游前缀缓存从该消息逐字节分叉。
func TestStripHookPrefixFromUserText(t *testing.T) {
	const marker = "SessionStart hook additional context: "
	body := strings.Repeat("Do not read any new content. From the conversation history, answer in ONE line: ", 4)
	block1 := "<context_window_protection>\n  <priority_instructions>\n    Every byte a tool returns enters your conversation memory.\n  </priority_instructions>\n</context_window_protection>"
	block2 := "<tool_selection_hierarchy>\n  <tip>Batch independent calls in one block.</tip>\n</tool_selection_hierarchy>"
	bare := "Concise output style is active. Be concise: lead with the result, skip preamble and narration, keep only what the user needs."
	dataRef := "## Data References\n- The pasted text contains many 'SECTION 1 MARK-1: ff3a' lines. (215932 bytes — query mcp__plugin_context-mode_context-mode__ctx_search)"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no injection passthrough", "plain question " + strings.Repeat("with enough body text. ", 12), "plain question " + strings.Repeat("with enough body text. ", 12)},
		{"hook prefix + single xml block", marker + "\n" + block1 + "\n\n" + body, body},
		// 模拟真实抓包样本:多 XML 块 + 裸文本直接跟在闭合标签后 + \n\n + 正文。
		{"hook + multi xml + bare text", marker + "\n" + block1 + "\n" + block2 + bare + "\n\n" + body, body},
		// 正文以 XML 开头但不带 SessionStart 前缀:不误伤。
		{"xml-leading body without marker", block1 + "\n\n" + body, block1 + "\n\n" + body},
		// 新 CLI 形态(实测 dump):引用块插在 XML 块序列中间,外层 session_*
		// 闭合标签落单,引用块紧跟上一块的 \n\n 之后——贪心 XML 匹配须跳过
		// 引用块继续匹配,剥离结果与正文逐字节相等。
		{"new cli: ref block mid-sequence with orphan closes", marker + "\n" + block1 + "\n\n" + dataRef + "\n\n" + "</session_guide>\n" + block2 + "\n</session_knowledge>" + bare + "\n\n" + body, body},
		{"new cli: ref block between xml blocks", marker + "\n" + block1 + "\n" + dataRef + "\n\n" + block2 + bare + "\n\n" + body, body},
		// 引用块缺失 " — query" 检索特征:不误吞(防把用户正文里的
		// ## Data References 段落误当引用剥掉)。
		{"ref block without query feature kept", marker + "\n" + block1 + "\n\n" + "## Data References\n- Local notes without the query feature.\n\n" + body, "## Data References\n- Local notes without the query feature.\n\n" + body},
	}
	for _, tc := range cases {
		if got := stripHookPrefixFromUserText(tc.in); got != tc.want {
			t.Fatalf("%s: got %d bytes want %d bytes, head=%q", tc.name, len(got), len(tc.want), got[:min(len(got), 60)])
		}
	}
}

// 护栏:剩余正文少于 200 字节、或剥离总量超过原文 80%,视为误判返回原文。
func TestStripHookPrefixFromUserTextGuards(t *testing.T) {
	const marker = "SessionStart hook additional context: "
	block := "<ctx>" + strings.Repeat("padding text ", 150) + "</ctx>"
	// 剩余正文过短。
	tiny := marker + "\n" + block + "\n\nhi"
	if got := stripHookPrefixFromUserText(tiny); got != tiny {
		t.Fatalf("tiny-body guard should keep original, got %d bytes", len(got))
	}
	// 剥离总量超过原文 80%(6000 字节注入 + ~360 字节正文)。
	big := marker + "\n<ctx>" + strings.Repeat("p", 6000) + "</ctx>\n\n" + strings.Repeat("body ", 72)
	if got := stripHookPrefixFromUserText(big); got != big {
		t.Fatalf("over-80%% strip guard should keep original, got %d bytes", len(got))
	}
}

// 真实长样本(注入段 2000+ 字节):构造 hookVersion = 注入段 + 干净正文,
// 剥离结果必须与客户端 session 存的干净版本逐字节一致——上游按前缀逐字节
// 做缓存,差一字节即全部 miss。
func TestStripHookPrefixFromUserTextLongSample(t *testing.T) {
	const marker = "SessionStart hook additional context: "
	var b strings.Builder
	b.WriteString(marker + "\n")
	for _, tag := range []string{"context_window_protection", "tool_selection_hierarchy", "deferred_tool_bootstrap", "output_constraints"} {
		b.WriteString("<" + tag + ">\n  <seg>\n    " + strings.Repeat("instruction line for "+tag+". ", 20) + "\n  </seg>\n</" + tag + ">\n")
	}
	bare := "Concise output style is active. Be concise: lead with the result, skip preamble and narration, keep only what the user needs."
	cleanBody := strings.Repeat("修复说明正文,必须完整保留。", 40)
	// 裸文本直接跟在末块闭合标签后(真实抓包形态)。
	hookVersion := b.String() + bare + "\n\n" + cleanBody
	if got := stripHookPrefixFromUserText(hookVersion); got != cleanBody {
		t.Fatalf("long sample: stripped != cleanBody (got %d bytes, want %d)", len(got), len(cleanBody))
	}
	// 纯 XML 无裸文本变体:块序列以 \n\n 结尾紧跟正文。
	pureXML := strings.TrimSuffix(b.String(), "\n") + "\n\n" + cleanBody
	if got := stripHookPrefixFromUserText(pureXML); got != cleanBody {
		t.Fatalf("pure-xml variant: stripped != cleanBody (got %d bytes, want %d)", len(got), len(cleanBody))
	}
	// 新 CLI 变体:块序列中间插入 Data References 引用块与孤儿闭合标签,
	// 引用内容(字节数)逐轮变化。
	newCLI := b.String() + "</session_guide>\n\n" +
		"## Data References\n- The pasted text contains many 'SECTION 1 MARK-1: ff3a' lines. (215932 bytes — query mcp__ctx_search)\n\n" +
		"<session_search>\nDetailed session data is indexed in context-mode FTS5.\n</session_search>\n</session_knowledge>" + bare + "\n\n" + cleanBody
	if got := stripHookPrefixFromUserText(newCLI); got != cleanBody {
		t.Fatalf("new-cli variant: stripped != cleanBody (got %d bytes, want %d)", len(got), len(cleanBody))
	}
}

// 形态 2(实测 F3 dump):resume/continue hook 的引用块直接开头,SessionStart
// 的 XML 块夹在注入序列中段,双层叠加,整个序列以最后一次 </session_knowledge>
// + 非空裸文本 + \n\n 结尾——结尾锚点剥离,结果与正文逐字节相等。
func TestStripHookPrefixFromUserTextForm2(t *testing.T) {
	const marker = "SessionStart hook additional context: "
	bare := "Concise output style is active. Be concise: lead with the result, skip preamble and narration, keep only what the user needs."
	body := strings.Repeat("Answer from the conversation history in one line. ", 12)
	// 双层叠加:第一层 resume hook(引用块直接开头、XML 块夹中段),第二层
	// SessionStart hook(完整 XML 块 + 嵌套 session_knowledge source="continue")。
	form2 := "## Data References\n- The pasted text contains many 'SECTION 1 MARK-1: ff3a' lines. (215932 bytes — query mcp__plugin_context-mode_context-mode__ctx_search(source: \"session-events\"))\n\n" +
		"</session_guide>\n<session_search>\nDetailed session data is indexed in context-mode FTS5.\n</session_search>\n</session_knowledge>" + bare + "\n\n" +
		marker + "\n<context_window_protection>\n  <priority_instructions>\n    " + strings.Repeat("rule line. ", 40) + "\n  </priority_instructions>\n</context_window_protection>\n" +
		"<session_knowledge source=\"continue\">\n<session_guide>\n## Project Rules\n.claude/CLAUDE.md\n\n" +
		"## Data References\n- Ref A. (215932 bytes — query mcp__a)\n- Ref B. (215929 bytes — query mcp__b)\n\n" +
		"</session_guide>\n<session_search>\nIndexed session data.\n</session_search>\n</session_knowledge>" + bare + "\n\n" + body
	if got := stripHookPrefixFromUserText(form2); got != body {
		t.Fatalf("form-2 double-stack: stripped != body (got %d bytes, want %d)", len(got), len(body))
	}
	// 正文包含 </session_knowledge> 字符串但开头无注入特征:入口不进,不误伤。
	plainWithClose := "Question about </session_knowledge> usage. " + strings.Repeat("with enough body text to pass guards. ", 8)
	if got := stripHookPrefixFromUserText(plainWithClose); got != plainWithClose {
		t.Fatalf("body containing session_knowledge close without injection head must stay as-is")
	}
	// 形态 2 + 护栏触发(正文过短):返回原文。
	form2Short := strings.TrimSuffix(form2, body) + "hi"
	if got := stripHookPrefixFromUserText(form2Short); got != form2Short {
		t.Fatalf("form-2 with tiny body should keep original, got %d bytes", len(got))
	}
}

// 包装函数只动最后一条 user 消息:历史里重放的旧 user 与 assistant 均不动;
// content 兼容 string 与 parts;多条 text parts 只剥第一条。
func TestStripHookPrefixFromLastUser(t *testing.T) {
	const marker = "SessionStart hook additional context: "
	body := strings.Repeat("Please continue the task from where it stopped. ", 6)
	hooked := marker + "\n<ctx>\n  <seg>stable instruction content for the session hook.</seg>\n</ctx>\n\n" + body
	mkUser := func(text string) map[string]any {
		return map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "text", "text": text}}}
	}
	// 最后一条被剥离,前面的 user 与 assistant 均不动。
	input := []any{
		mkUser(hooked),
		map[string]any{"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": hooked}}},
		mkUser(hooked),
	}
	if !stripHookPrefixFromLastUser(input) {
		t.Fatalf("last user strip should report change")
	}
	if got := input[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; got != hooked {
		t.Fatalf("earlier user must stay untouched")
	}
	if got := input[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; got != hooked {
		t.Fatalf("assistant must stay untouched")
	}
	if got := input[2].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string); got != body {
		t.Fatalf("last user should be stripped to clean body, got %q", got[:min(len(got), 60)])
	}
	// 无注入:返回 false 且内容不动。
	plain := []any{mkUser(body)}
	if stripHookPrefixFromLastUser(plain) {
		t.Fatalf("no-injection input should report no change")
	}
	// content 为 string 的形状。
	strInput := []any{map[string]any{"role": "user", "content": hooked}}
	if !stripHookPrefixFromLastUser(strInput) {
		t.Fatalf("string content strip should report change")
	}
	if got := strInput[0].(map[string]any)["content"]; got != body {
		t.Fatalf("string content should be stripped, got %q", got)
	}
	// 多条 text parts:只剥第一条 part,其余不动。
	multi := []any{map[string]any{"type": "message", "role": "user", "content": []any{
		map[string]any{"type": "text", "text": hooked},
		map[string]any{"type": "text", "text": body},
	}}}
	if !stripHookPrefixFromLastUser(multi) {
		t.Fatalf("multi-part strip should report change")
	}
	parts := multi[0].(map[string]any)["content"].([]any)
	if got := parts[0].(map[string]any)["text"]; got != body {
		t.Fatalf("first text part should be stripped")
	}
	if got := parts[1].(map[string]any)["text"]; got != body {
		t.Fatalf("second text part must stay untouched")
	}
	// 空 input。
	if stripHookPrefixFromLastUser([]any{}) {
		t.Fatalf("empty input should report no change")
	}
}

// chat 形状接入:stripVolatileCountersInMap 同时覆盖 input 与 messages 数组的
// 末条 user hook 前缀(buildUpstreamBodyFromClaude 与发送侧共用的接入点)。
func TestStripVolatileCountersInMapHookPrefix(t *testing.T) {
	const marker = "SessionStart hook additional context: "
	// TrimRight:计数剥离对 string content 会 TrimSpace,正文不得以空白结尾,
	// 否则断言差一个尾随空格(剥离逻辑本身与该行为无关)。
	body := strings.TrimRight(strings.Repeat("Continue with the migration plan. ", 8), " ")
	hooked := marker + "\n<ctx>\n  <seg>hook context block.</seg>\n</ctx>\n\n" + body
	m := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "SYS"},
		map[string]any{"role": "user", "content": hooked},
	}}
	if !stripVolatileCountersInMap(m) {
		t.Fatalf("map strip should report change for hook prefix")
	}
	if got := m["messages"].([]any)[1].(map[string]any)["content"]; got != body {
		t.Fatalf("messages last user should be stripped, got %q", got)
	}
	// 幂等:再跑一次无改动。
	if stripVolatileCountersInMap(m) {
		t.Fatalf("second map strip should be a no-op")
	}
}
