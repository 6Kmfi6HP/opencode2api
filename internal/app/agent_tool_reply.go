package app

import (
	"context"
	"encoding/json"
	"strings"
)

// ======================== 回复侧：manifest 工具调用映射 ========================
//
// 请求侧把 CLI manifest 前置进 tools（见 fingerprint_manifest.go），模型因此
// 可能用 manifest 的名字与参数回答。回复呈现面在写给客户端前做两件事（对齐
// zen-proxy 的 remapAgentTool + keepableFor）：
//
//  1. 映射：manifest 工具名改回调用方工具名（read → Read / 调用方声明的同名
//     小写工具），参数按 agentToolMap 改名，值为 nil 的参数（bash 的 workdir）
//     丢弃，其余参数原样保留。
//  2. 丢弃：调用方没有对应工具的 manifest 调用、以及模型凭空发明的工具名，
//     一律不出现在回复里——调用方无法执行。判定按"调用方实际声明的工具名"
//     而不是 manifest 名单，所以发明名（不在 manifest、也没人声明）同样被拦。
//
// 调用方未声明任何工具时（bridged）全部工具调用丢弃，finish 语义降为文本。
// 无调用方上下文（测试、内部聚合）时只做映射、不丢弃，保持既有透传行为。

// agentToolReplyContext 是一次请求的回复侧工具策略，经 ctx 从请求边界传到
// 各协议的回复呈现面。
type agentToolReplyContext struct {
	// viaMap 为 true 时按 Anthropic 官方名比对（manifest read ≡ 调用方 Read）；
	// chat/responses 路线调用方工具名已是 manifest 词汇，直接比对。
	viaMap bool
	// callerHasTools 为 false 且 manifest 已注入（bridged）时回复不带任何
	// 工具调用。
	callerHasTools bool
	// keptTool 记录本回复是否保留过至少一次工具调用，供 stop_reason 降级。
	keptTool bool
	// offered 是调用方声明的工具名（chat 形 function.name / 裸 name 都收）。
	offered map[string]bool
	// offeredLower 是小写索引：manifest 名与调用方声明同名（大小写不敏感）
	// 时，映射回调用方声明的原始大小写而不是官方 PascalCase。
	offeredLower map[string]string
	// dropped / rawByIndex 是流式回复的 per-index 状态：名字块决定丢弃，
	// 后续参数块跟随。单请求单流，就地改写。
	dropped    map[int]bool
	rawByIndex map[int]string
}

func (c *agentToolReplyContext) active() bool {
	return c != nil && (len(c.offered) > 0 || !c.callerHasTools)
}

// callerToolName 返回 manifest 工具名对应的调用方工具名：调用方按官方名
// （viaMap）或按 manifest 名（大小写不敏感）声明过时用调用方的原始拼写，
// 否则用映射表的官方名。不在映射表里返回 ("", false)。
func (c *agentToolReplyContext) callerToolName(manifestName string) (string, bool) {
	spec, ok := agentToolMap[manifestName]
	if !ok {
		return "", false
	}
	if c != nil {
		if declared, ok := c.offeredLower[manifestName]; ok {
			return declared, true
		}
		if c.viaMap {
			if declared, ok := c.offeredLower[spec.Name]; ok {
				return declared, true
			}
		}
	}
	return spec.Name, true
}

// keepRawName 报告上游原始工具名是否允许出现在回复里。无上下文时全部保留
// （只映射不丢弃）；bridged（调用方无工具）全部丢弃；其余只保留调用方声明
// 过的名字——manifest 名在其映射目标被声明时也算声明过。
func (c *agentToolReplyContext) keepRawName(rawName string) bool {
	if c == nil {
		return true
	}
	if !c.callerHasTools {
		return false
	}
	if c.offered[rawName] {
		return true
	}
	if mapped, ok := c.callerToolName(rawName); ok && c.offered[mapped] {
		return true
	}
	return false
}

// remapAgentToolCall 把一条上游工具调用改写成调用方形态：名字映射、参数改名
// （nil 映射的参数丢弃，未知参数原样保留）。rawArgs 为非法 JSON 时参数置空
// 对象（对齐 zen-proxy remapAgentTool 的容错）。非 manifest 名原样返回。
func (c *agentToolReplyContext) remapAgentToolCall(rawName, rawArgs string) (string, string) {
	spec, ok := agentToolMap[rawName]
	if !ok {
		return rawName, rawArgs
	}
	name, _ := c.callerToolName(rawName)
	if rawArgs == "" {
		return name, rawArgs
	}
	var args map[string]any
	if json.Unmarshal([]byte(rawArgs), &args) != nil || args == nil {
		return name, "{}"
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		if mapped, drop := spec.Args[k]; drop {
			if mapped == nil {
				continue
			}
			out[*mapped] = v
			continue
		}
		out[k] = v
	}
	b, err := json.Marshal(out)
	if err != nil {
		return name, rawArgs
	}
	return name, string(b)
}

// remapAgentToolInput 同 remapAgentToolCall，但参数是已解析的对象（Anthropic
// tool_use input）。返回改写后的名字与 input；非 manifest 名原样返回。
func (c *agentToolReplyContext) remapAgentToolInput(rawName string, input any) (string, any) {
	spec, ok := agentToolMap[rawName]
	if !ok {
		return rawName, input
	}
	name, _ := c.callerToolName(rawName)
	args, ok := input.(map[string]any)
	if !ok || args == nil {
		return name, input
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		if mapped, drop := spec.Args[k]; drop {
			if mapped == nil {
				continue
			}
			out[*mapped] = v
			continue
		}
		out[k] = v
	}
	return name, out
}

// ======================== 请求工具名收集 ========================

// offeredToolNames 从客户端原始工具列表收集声明名（chat 的 function.name 与
// 裸 name 都收），供回复侧判定"调用方能执行什么"。
func offeredToolNames(tools []Tool) map[string]bool {
	out := make(map[string]bool, len(tools))
	for _, t := range tools {
		if t.Function.Name != "" {
			out[t.Function.Name] = true
		}
	}
	return out
}

// offeredClaudeToolNames 收集 Claude 请求声明的工具名。
func offeredClaudeToolNames(tools []ClaudeTool) map[string]bool {
	out := make(map[string]bool, len(tools))
	for _, t := range tools {
		if t.Name != "" {
			out[t.Name] = true
		}
	}
	return out
}

// offeredResponsesToolNames 收集 Responses 请求声明的工具名（顶层 name 与
// function.name 都收）。
func offeredResponsesToolNames(tools []ResponsesTool) map[string]bool {
	out := make(map[string]bool, len(tools))
	for _, t := range tools {
		if t.Name != "" {
			out[t.Name] = true
		}
		if t.Function != nil && t.Function.Name != "" {
			out[t.Function.Name] = true
		}
	}
	return out
}

// newAgentToolReplyContext 构造回复侧策略。offered 为空且 callerHasTools 为
// false 时表示 bridged（全部丢弃）；两者都空时返回 nil（无策略，只映射）。
func newAgentToolReplyContext(offered map[string]bool, viaMap, callerHasTools bool) *agentToolReplyContext {
	if len(offered) == 0 && callerHasTools {
		return nil
	}
	if len(offered) == 0 && !callerHasTools {
		return &agentToolReplyContext{viaMap: viaMap}
	}
	lower := make(map[string]string, len(offered))
	for name := range offered {
		lower[lowerASCII(name)] = name
	}
	return &agentToolReplyContext{
		viaMap:         viaMap,
		callerHasTools: callerHasTools,
		offered:        offered,
		offeredLower:   lower,
	}
}

// lowerASCII 只折叠 ASCII 大小写：工具名是标识符，无需 unicode 折叠。
func lowerASCII(s string) string {
	b := make([]byte, len(s))
	for i := range len(s) {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

// ======================== chat 形回复改写 ========================

// rewriteChatToolCalls 就地改写一条 chat message/delta 的 tool_calls：映射
// manifest 名与参数，丢弃 keepRawName 拒绝的调用。全部丢弃时删除 tool_calls
// 键。返回是否发生改动。
func (c *agentToolReplyContext) rewriteChatToolCalls(fields map[string]any) bool {
	if c == nil || !c.active() {
		return false
	}
	tcs, ok := fields["tool_calls"].([]any)
	if !ok || len(tcs) == 0 {
		return false
	}
	kept := make([]any, 0, len(tcs))
	changed := false
	for _, raw := range tcs {
		tc, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		fn, _ := tc["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			kept = append(kept, tc)
			continue
		}
		if !c.keepRawName(name) {
			changed = true
			continue
		}
		args, _ := fn["arguments"].(string)
		mappedName, mappedArgs := c.remapAgentToolCall(name, args)
		if mappedName != name || mappedArgs != args {
			fn["name"] = mappedName
			if mappedArgs != args {
				fn["arguments"] = mappedArgs
			}
			tc["function"] = fn
			changed = true
		}
		kept = append(kept, tc)
	}
	if !changed {
		return false
	}
	if len(kept) == 0 {
		delete(fields, "tool_calls")
	} else {
		fields["tool_calls"] = kept
	}
	return true
}

// rewriteChatChoiceFinish 在工具调用被删空后把 finish_reason=tool_calls 降为
// stop（空的 tool_calls 声明对所有客户端都是 bug 而不是答案）。
func rewriteChatChoiceFinish(choice map[string]any, hadToolCalls bool) {
	if hadToolCalls {
		return
	}
	if fr, _ := choice["finish_reason"].(string); fr == "tool_calls" || fr == "function_call" {
		choice["finish_reason"] = "stop"
	}
}

type agentToolReplyContextKey struct{}

func withAgentToolReply(ctx context.Context, c *agentToolReplyContext) context.Context {
	if ctx == nil || c == nil {
		return ctx
	}
	return context.WithValue(ctx, agentToolReplyContextKey{}, c)
}

func agentToolReplyFromContext(ctx context.Context) *agentToolReplyContext {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(agentToolReplyContextKey{}).(*agentToolReplyContext)
	return c
}

// attachFreeTierToolReply 把免费模型请求的调用方工具名单放进 ctx，供回复
// 呈现面映射/丢弃 manifest 调用。付费模型不注入 manifest，不挂策略。
func attachFreeTierToolReply(ctx context.Context, modelID string, offered map[string]bool, viaMap bool) context.Context {
	if !isFreeModel(modelID) {
		return ctx
	}
	return withAgentToolReply(ctx, newAgentToolReplyContext(offered, viaMap, len(offered) > 0))
}

func (c *agentToolReplyContext) noteDrop(idx int) {
	if c.dropped == nil {
		c.dropped = map[int]bool{}
	}
	c.dropped[idx] = true
}

func (c *agentToolReplyContext) noteRaw(idx int, name string) {
	if c.rawByIndex == nil {
		c.rawByIndex = map[int]string{}
	}
	if name != "" {
		c.rawByIndex[idx] = name
	}
}

// remapArgFragment 把流式参数片段里已完整出现的 manifest 参数名改成调用方
// 参数名。nil 映射（workdir）留在片段里：半截 JSON 上删键不安全，调用方忽略
// 未知键。
func remapArgFragment(manifestName, fragment string) string {
	spec, ok := agentToolMap[manifestName]
	if !ok || fragment == "" {
		return fragment
	}
	for from, to := range spec.Args {
		if to == nil || *to == from {
			continue
		}
		fragment = strings.ReplaceAll(fragment, `"`+from+`"`, `"`+*to+`"`)
	}
	return fragment
}

// filterChatToolCallDelta 就地过滤一条 chat delta/message 的 tool_calls。
// 名字块决定保留与映射；同 index 的后续参数块跟随该决定，并改写参数键名。
func (c *agentToolReplyContext) filterChatToolCallDelta(fields map[string]any) {
	if c == nil || !c.active() {
		return
	}
	tcs, ok := fields["tool_calls"].([]any)
	if !ok || len(tcs) == 0 {
		return
	}
	kept := make([]any, 0, len(tcs))
	for _, raw := range tcs {
		tc, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		idx := 0
		if n, ok := tc["index"].(float64); ok {
			idx = int(n)
		}
		fn, _ := tc["function"].(map[string]any)
		if fn == nil {
			if c.dropped[idx] {
				continue
			}
			kept = append(kept, tc)
			continue
		}
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		if name != "" {
			if !c.keepRawName(name) {
				c.noteDrop(idx)
				continue
			}
			c.noteRaw(idx, name)
			mapped, mappedArgs := c.remapAgentToolCall(name, args)
			if _, isManifest := agentToolMap[name]; isManifest && args != "" {
				mappedArgs = remapArgFragment(name, args)
				if mappedArgs == args {
					_, mappedArgs = c.remapAgentToolCall(name, args)
				}
			}
			fn["name"] = mapped
			if mappedArgs != args {
				fn["arguments"] = mappedArgs
			}
			tc["function"] = fn
			kept = append(kept, tc)
			continue
		}
		if c.dropped[idx] {
			continue
		}
		if args != "" {
			if rawName := c.rawByIndex[idx]; rawName != "" {
				if frag := remapArgFragment(rawName, args); frag != args {
					fn["arguments"] = frag
					tc["function"] = fn
				}
			}
		}
		kept = append(kept, tc)
	}
	if len(kept) == 0 {
		delete(fields, "tool_calls")
		return
	}
	fields["tool_calls"] = kept
}

// rewriteChatCompletionBody 改写非流 chat.completion 里的 tool_calls。
// 无策略或非该形状时原样返回。
func rewriteChatCompletionBody(body []byte, c *agentToolReplyContext) []byte {
	if c == nil || !c.active() || len(body) == 0 || body[0] != '{' {
		return body
	}
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return body
	}
	choices, _ := raw["choices"].([]any)
	changed := false
	for _, ch := range choices {
		choice, ok := ch.(map[string]any)
		if !ok {
			continue
		}
		msg, _ := choice["message"].(map[string]any)
		if msg == nil {
			continue
		}
		before := len(toolCallsOf(msg))
		if c.rewriteChatToolCalls(msg) {
			changed = true
			rewriteChatChoiceFinish(choice, len(toolCallsOf(msg)) > 0 && before > 0 && len(toolCallsOf(msg)) > 0)
			if len(toolCallsOf(msg)) == 0 && before > 0 {
				rewriteChatChoiceFinish(choice, false)
			}
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return out
}

func toolCallsOf(msg map[string]any) []any {
	tcs, _ := msg["tool_calls"].([]any)
	return tcs
}

// rewriteChatSSELine 改写一条 chat SSE data 行里的 tool_calls（映射 manifest
// 名/参数，丢掉调用方无法执行的调用）。非 data 行原样返回。
func rewriteChatSSELine(line string, c *agentToolReplyContext) string {
	if c == nil || !c.active() || !strings.HasPrefix(line, "data: ") {
		return line
	}
	payload := strings.TrimSpace(line[6:])
	if payload == "" || payload[0] != '{' {
		return line
	}
	var raw map[string]any
	if json.Unmarshal([]byte(payload), &raw) != nil {
		return line
	}
	choices, _ := raw["choices"].([]any)
	changed := false
	for _, ch := range choices {
		choice, ok := ch.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"delta", "message"} {
			fields, _ := choice[key].(map[string]any)
			if fields == nil {
				continue
			}
			if _, has := fields["tool_calls"]; !has {
				continue
			}
			c.filterChatToolCallDelta(fields)
			changed = true
			if _, still := fields["tool_calls"]; !still {
				rewriteChatChoiceFinish(choice, false)
			}
		}
	}
	if !changed {
		return line
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return line
	}
	return "data: " + string(b)
}

// presentAnthropicStreamLine 在缩短名还原之后，把 manifest tool_use 映射回
// 调用方工具；调用方无法执行的块整行丢弃（返回空串，调用方不写）。
func presentAnthropicStreamLine(line string, rw *responsesNameRewrites, c *agentToolReplyContext) string {
	line = restoreAnthropicStreamLine(line, rw)
	if c == nil || !c.active() || !strings.HasPrefix(line, "data: ") {
		return line
	}
	payload := strings.TrimSpace(line[6:])
	if payload == "" || payload[0] != '{' {
		return line
	}
	var evt map[string]any
	if json.Unmarshal([]byte(payload), &evt) != nil {
		return line
	}
	changed := false
	switch typ, _ := evt["type"].(string); typ {
	case "content_block_start":
		cb, _ := evt["content_block"].(map[string]any)
		if cb == nil || cb["type"] != "tool_use" {
			return line
		}
		idx := intFromAnyIndex(evt["index"])
		name, _ := cb["name"].(string)
		if !c.keepRawName(name) {
			c.noteDrop(idx)
			return ""
		}
		c.noteRaw(idx, name)
		mapped, _ := c.remapAgentToolCall(name, "")
		if mapped != name {
			cb["name"] = mapped
			changed = true
		}
		c.keptTool = true
	case "content_block_delta":
		idx := intFromAnyIndex(evt["index"])
		if c.dropped[idx] {
			return ""
		}
		d, _ := evt["delta"].(map[string]any)
		if d == nil || d["type"] != "input_json_delta" {
			return line
		}
		pj, _ := d["partial_json"].(string)
		if rawName := c.rawByIndex[idx]; rawName != "" && pj != "" {
			if frag := remapArgFragment(rawName, pj); frag != pj {
				d["partial_json"] = frag
				changed = true
			}
		}
	case "content_block_stop":
		if c.dropped[intFromAnyIndex(evt["index"])] {
			return ""
		}
	case "message_delta":
		delta, _ := evt["delta"].(map[string]any)
		if delta == nil || c.keptTool {
			return line
		}
		if sr, _ := delta["stop_reason"].(string); sr == "tool_use" {
			delta["stop_reason"] = "end_turn"
			changed = true
		}
	}
	if !changed {
		return line
	}
	b, err := json.Marshal(evt)
	if err != nil {
		return line
	}
	return "data: " + string(b) + "\n"
}

func intFromAnyIndex(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

// presentAnthropicBody 还原缩短名后，按调用方工具名单过滤/映射非流 tool_use。
func presentAnthropicBody(body []byte, rw *responsesNameRewrites, c *agentToolReplyContext) []byte {
	body = restoreAnthropicBodyNames(body, rw)
	if c == nil || !c.active() || len(body) == 0 || body[0] != '{' {
		return body
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	content, ok := m["content"].([]any)
	if !ok {
		return body
	}
	kept := make([]any, 0, len(content))
	keptTool := false
	changed := false
	for _, raw := range content {
		block, ok := raw.(map[string]any)
		if !ok || block["type"] != "tool_use" {
			kept = append(kept, raw)
			continue
		}
		name, _ := block["name"].(string)
		if !c.keepRawName(name) {
			changed = true
			continue
		}
		mapped, input := c.remapAgentToolInput(name, block["input"])
		if mapped != name || input != block["input"] {
			block["name"] = mapped
			block["input"] = input
			changed = true
		}
		keptTool = true
		kept = append(kept, block)
	}
	if !keptTool {
		if sr, _ := m["stop_reason"].(string); sr == "tool_use" {
			m["stop_reason"] = "end_turn"
			changed = true
		}
	}
	if !changed {
		return body
	}
	m["content"] = kept
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
