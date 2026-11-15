package app

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
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
	// wireToClient maps every opaque alias to the caller's original tool name.
	// Both sides derive the alias from the declared name, so this stays local
	// to the reply context.
	wireToClient map[string]string
	// toolSchemas 记录调用方各声明工具名的 parameters/input_schema 属性键，
	// 判定参数名是否真需要映射或丢弃，防止按路径一刀切误改同名异构参数
	//（如 Anthropic 路线下的 pi 使用 path 而非 file_path）。
	toolSchemas map[string]map[string]bool
	// dropped / rawByIndex 是流式回复的 per-index 状态：名字块决定丢弃，
	// 后续参数块跟随。单请求单流，就地改写。
	dropped       map[int]bool
	rawByIndex    map[int]string
	droppedIDs    map[string]bool
	itemIDToIndex map[string]int
	argPending    map[int]string
	argEscaped    map[int]bool
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
		if declared, ok := c.offeredLower[lowerASCII(spec.Name)]; ok {
			return declared, true
		}
	}
	return spec.Name, true
}

func (c *agentToolReplyContext) targetToolProps(manifestName string) map[string]bool {
	if c == nil || len(c.toolSchemas) == 0 {
		return nil
	}
	if target, ok := c.callerToolName(manifestName); ok {
		if props := c.toolSchemas[target]; len(props) > 0 {
			return props
		}
	}
	if props := c.toolSchemas[manifestName]; len(props) > 0 {
		return props
	}
	return nil
}

// shouldRemapArg 判断 manifest 工具的参数 from 是否应当在回复时改名为 *to
// （to==nil 表示丢弃）。调用方声明了目标工具的 schema 时按实际属性键判定，
// 仅当调用方期望 *to 且未声明 from 时才改名；未提供 schema 时回落到 viaMap。
func (c *agentToolReplyContext) shouldRemapArg(manifestName, from string, to *string) bool {
	if c == nil {
		return false
	}
	props := c.targetToolProps(manifestName)
	if to == nil {
		if len(props) > 0 {
			return !props[from]
		}
		target, _ := c.callerToolName(manifestName)
		return c.viaMap || (target != "" && lowerASCII(target) != manifestName)
	}
	if *to == from {
		return false
	}
	if len(props) > 0 {
		return props[*to] && !props[from]
	}
	return c.viaMap
}

type argRemapPair struct {
	from string
	to   string
}

func (c *agentToolReplyContext) argRemapPairs(manifestName string) []argRemapPair {
	spec, ok := agentToolMap[manifestName]
	if !ok || len(spec.Args) == 0 {
		return nil
	}
	var pairs []argRemapPair
	for from, to := range spec.Args {
		if to == nil || !c.shouldRemapArg(manifestName, from, to) {
			continue
		}
		pairs = append(pairs, argRemapPair{from: from, to: *to})
	}
	if len(pairs) > 1 {
		sort.Slice(pairs, func(i, j int) bool {
			return pairs[i].from < pairs[j].from
		})
	}
	return pairs
}

func (c *agentToolReplyContext) hasArgRemaps(manifestName string) bool {
	spec, ok := agentToolMap[manifestName]
	if !ok || len(spec.Args) == 0 {
		return false
	}
	for from, to := range spec.Args {
		if c.shouldRemapArg(manifestName, from, to) {
			return true
		}
	}
	return false
}

// keepRawName 报告上游原始工具名是否允许出现在回复里。无上下文时全部保留
// （只映射不丢弃）；bridged（调用方无工具）全部丢弃；其余只保留调用方声明
// 过的名字——manifest 名在其映射目标被声明时也算声明过。
func (c *agentToolReplyContext) keepRawName(rawName string) bool {
	if c == nil {
		return true
	}
	if original, ok := c.wireToClient[rawName]; ok {
		return c.callerHasTools && c.offered[original]
	}
	if isCallerToolAlias(rawName) {
		return false
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
	if c != nil {
		if original, ok := c.wireToClient[rawName]; ok {
			return original, rawArgs
		}
	}
	spec, ok := agentToolMap[rawName]
	if !ok {
		return rawName, rawArgs
	}
	name, _ := c.callerToolName(rawName)
	if rawArgs == "" {
		return name, rawArgs
	}
	if c == nil || !c.hasArgRemaps(rawName) {
		return name, rawArgs
	}
	var args map[string]any
	if json.Unmarshal([]byte(rawArgs), &args) != nil || args == nil {
		return name, "{}"
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		if mapped, exists := spec.Args[k]; exists && c.shouldRemapArg(rawName, k, mapped) {
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
	if c != nil {
		if original, ok := c.wireToClient[rawName]; ok {
			return original, input
		}
	}
	spec, ok := agentToolMap[rawName]
	if !ok {
		return rawName, input
	}
	name, _ := c.callerToolName(rawName)
	args, ok := input.(map[string]any)
	if !ok || args == nil {
		return name, input
	}
	if c == nil || !c.hasArgRemaps(rawName) {
		return name, input
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		if mapped, exists := spec.Args[k]; exists && c.shouldRemapArg(rawName, k, mapped) {
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

// ======================== 请求工具名与 schema 收集 ========================

func schemaPropertyNames(schema any) map[string]bool {
	m, ok := schema.(map[string]any)
	if !ok || m == nil {
		return nil
	}
	props, ok := m["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		return nil
	}
	out := make(map[string]bool, len(props))
	for k := range props {
		if k != "" {
			out[k] = true
		}
	}
	return out
}

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

func offeredToolSchemas(tools []Tool) map[string]map[string]bool {
	if len(tools) == 0 {
		return nil
	}
	out := make(map[string]map[string]bool, len(tools))
	for _, t := range tools {
		if t.Function.Name == "" {
			continue
		}
		if props := schemaPropertyNames(t.Function.Parameters); len(props) > 0 {
			out[t.Function.Name] = props
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

func offeredClaudeToolSchemas(tools []ClaudeTool) map[string]map[string]bool {
	if len(tools) == 0 {
		return nil
	}
	out := make(map[string]map[string]bool, len(tools))
	for _, t := range tools {
		if t.Name == "" {
			continue
		}
		if props := schemaPropertyNames(t.InputSchema); len(props) > 0 {
			out[t.Name] = props
		}
	}
	return out
}

// offeredResponsesToolNames 收集 Responses 请求声明的工具名（顶层 name、
// function.name 以及 shell / apply_patch / namespace 转换后的有效工具名都收）。
func offeredResponsesToolNames(tools []ResponsesTool) map[string]bool {
	converted := convertResponsesTools(tools)
	out := make(map[string]bool, len(tools)+len(converted))
	for _, t := range tools {
		if t.Name != "" {
			out[t.Name] = true
		}
		if t.Function != nil && t.Function.Name != "" {
			out[t.Function.Name] = true
		}
	}
	for _, ct := range converted {
		if ct.Function.Name != "" {
			out[ct.Function.Name] = true
		}
	}
	return out
}

func offeredResponsesToolSchemas(tools []ResponsesTool) map[string]map[string]bool {
	if len(tools) == 0 {
		return nil
	}
	converted := convertResponsesTools(tools)
	out := make(map[string]map[string]bool, len(tools)+len(converted))
	for _, t := range tools {
		if t.Name != "" {
			if props := schemaPropertyNames(t.Parameters); len(props) > 0 {
				out[t.Name] = props
			}
		}
		if t.Function != nil && t.Function.Name != "" {
			if props := schemaPropertyNames(t.Function.Parameters); len(props) > 0 {
				out[t.Function.Name] = props
			}
		}
	}
	for _, ct := range converted {
		if ct.Function.Name != "" {
			if props := schemaPropertyNames(ct.Function.Parameters); len(props) > 0 {
				if _, exists := out[ct.Function.Name]; !exists {
					out[ct.Function.Name] = props
				}
			}
		}
	}
	return out
}

// newAgentToolReplyContext 构造回复侧策略。offered 为空且 callerHasTools 为
// false 时表示 bridged（全部丢弃）；两者都空时返回 nil（无策略，只映射）。
func newAgentToolReplyContext(offered map[string]bool, viaMap, callerHasTools bool) *agentToolReplyContext {
	return newAgentToolReplyContextWithSchemas(offered, nil, viaMap, callerHasTools)
}

func newAgentToolReplyContextWithSchemas(offered map[string]bool, schemas map[string]map[string]bool, viaMap, callerHasTools bool) *agentToolReplyContext {
	if len(offered) == 0 && callerHasTools {
		return nil
	}
	if len(offered) == 0 && !callerHasTools {
		return &agentToolReplyContext{viaMap: viaMap, wireToClient: map[string]string{}}
	}
	lower := make(map[string]string, len(offered))
	wire := make(map[string]string, len(offered))
	for name := range offered {
		lower[lowerASCII(name)] = name
		if alias := callerToolAlias(name); alias != "" {
			wire[alias] = name
		}
	}
	return &agentToolReplyContext{
		viaMap:         viaMap,
		callerHasTools: callerHasTools,
		offered:        offered,
		offeredLower:   lower,
		wireToClient:   wire,
		toolSchemas:    schemas,
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
	return attachFreeTierToolReplyWithSchemas(ctx, modelID, offered, nil, viaMap)
}

func attachFreeTierToolReplyWithSchemas(ctx context.Context, modelID string, offered map[string]bool, schemas map[string]map[string]bool, viaMap bool) context.Context {
	if !isFreeModel(modelID) {
		return ctx
	}
	return withAgentToolReply(ctx, newAgentToolReplyContextWithSchemas(offered, schemas, viaMap, len(offered) > 0))
}

func (c *agentToolReplyContext) noteDrop(idx int, ids ...string) {
	if c.dropped == nil {
		c.dropped = map[int]bool{}
	}
	c.dropped[idx] = true
	for _, id := range ids {
		if id == "" {
			continue
		}
		if c.droppedIDs == nil {
			c.droppedIDs = map[string]bool{}
		}
		c.droppedIDs[id] = true
	}
}

func (c *agentToolReplyContext) isDropped(idx int, ids ...string) bool {
	if c == nil {
		return false
	}
	if idx >= 0 && c.dropped[idx] {
		return true
	}
	for _, id := range ids {
		if id != "" && c.droppedIDs[id] {
			return true
		}
	}
	return false
}

func (c *agentToolReplyContext) noteRaw(idx int, name string, ids ...string) {
	if c.rawByIndex == nil {
		c.rawByIndex = map[int]string{}
	}
	if name != "" {
		c.rawByIndex[idx] = name
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if c.itemIDToIndex == nil {
			c.itemIDToIndex = map[string]int{}
		}
		c.itemIDToIndex[id] = idx
	}
}

// remapArgStream 按 JSON 键位置（未转义的 "<from>" + 可选空白 + ":"）把流式参数
// 片段里的 manifest 参数名改写成调用方参数名，跨分片缓冲可能命中键名的尾部前缀
// 直到确认为键或非键为止，避免半片拆分失效及误改字符串字面值。
func (c *agentToolReplyContext) remapArgStream(idx int, manifestName, fragment string) string {
	if c == nil || fragment == "" {
		return fragment
	}
	pairs := c.argRemapPairs(manifestName)
	if len(pairs) == 0 {
		if pending := c.flushArgPending(idx); pending != "" {
			return pending + fragment
		}
		return fragment
	}
	pending := c.argPending[idx]
	if pending != "" {
		delete(c.argPending, idx)
	}
	s := pending + fragment
	var out strings.Builder
	pos := 0
	if c.argEscaped[idx] && len(s) > 0 {
		out.WriteByte(s[0])
		delete(c.argEscaped, idx)
		pos = 1
	}
	for pos < len(s) {
		ch := s[pos]
		if ch == '\\' {
			out.WriteByte(ch)
			if pos+1 < len(s) {
				out.WriteByte(s[pos+1])
				pos += 2
				continue
			}
			if c.argEscaped == nil {
				c.argEscaped = map[int]bool{}
			}
			c.argEscaped[idx] = true
			pos++
			break
		}
		if ch != '"' {
			out.WriteByte(ch)
			pos++
			continue
		}
		rem := s[pos:]
		holdTrailing := false
		matched := false
		for _, p := range pairs {
			pat := `"` + p.from + `"`
			if len(rem) < len(pat) {
				if strings.HasPrefix(pat, rem) {
					holdTrailing = true
					break
				}
				continue
			}
			if !strings.HasPrefix(rem, pat) {
				continue
			}
			k := len(pat)
			for k < len(rem) && (rem[k] == ' ' || rem[k] == '\t' || rem[k] == '\r' || rem[k] == '\n') {
				k++
			}
			if k == len(rem) {
				holdTrailing = true
				break
			}
			if rem[k] == ':' {
				out.WriteByte('"')
				out.WriteString(p.to)
				out.WriteByte('"')
				out.WriteString(rem[len(pat) : k+1])
				pos += k + 1
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		if holdTrailing {
			if c.argPending == nil {
				c.argPending = map[int]string{}
			}
			c.argPending[idx] = rem
			break
		}
		out.WriteByte('"')
		pos++
	}
	return out.String()
}

func (c *agentToolReplyContext) flushArgPending(idx int) string {
	if c == nil {
		return ""
	}
	pending := c.argPending[idx]
	delete(c.argPending, idx)
	delete(c.argEscaped, idx)
	return pending
}

func (c *agentToolReplyContext) flushAllArgPending() string {
	if c == nil || len(c.argPending) == 0 {
		return ""
	}
	var b strings.Builder
	keys := make([]int, 0, len(c.argPending))
	for idx := range c.argPending {
		keys = append(keys, idx)
	}
	sort.Ints(keys)
	for _, idx := range keys {
		b.WriteString(c.flushArgPending(idx))
	}
	return b.String()
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
			mapped, _ := c.remapAgentToolCall(name, "")
			mappedArgs := args
			if args != "" {
				mappedArgs = c.remapArgStream(idx, name, args)
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
				if frag := c.remapArgStream(idx, rawName, args); frag != args {
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
		if fr, _ := choice["finish_reason"].(string); fr != "" {
			if len(c.argPending) > 0 {
				keys := make([]int, 0, len(c.argPending))
				for idx := range c.argPending {
					keys = append(keys, idx)
				}
				sort.Ints(keys)
				var extra []any
				for _, idx := range keys {
					if flushed := c.flushArgPending(idx); flushed != "" {
						extra = append(extra, map[string]any{
							"index":    idx,
							"function": map[string]any{"arguments": flushed},
						})
					}
				}
				if len(extra) > 0 {
					delta, _ := choice["delta"].(map[string]any)
					if delta == nil {
						delta = map[string]any{}
						choice["delta"] = delta
					}
					existing, _ := delta["tool_calls"].([]any)
					delta["tool_calls"] = append(existing, extra...)
					changed = true
				}
			}
			if fr == "tool_calls" && len(c.dropped) > 0 && len(c.rawByIndex) == 0 {
				rewriteChatChoiceFinish(choice, false)
				changed = true
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
		mapped, mappedInput, inputChanged := c.remapAgentToolInputChanged(name, cb["input"])
		if mapped != name {
			cb["name"] = mapped
			changed = true
		}
		if inputChanged {
			cb["input"] = mappedInput
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
			if frag := c.remapArgStream(idx, rawName, pj); frag != pj {
				d["partial_json"] = frag
				changed = true
			}
		}
	case "content_block_stop":
		idx := intFromAnyIndex(evt["index"])
		if c.dropped[idx] {
			return ""
		}
		if pending := c.flushArgPending(idx); pending != "" {
			if b, err := json.Marshal(map[string]any{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": pending},
			}); err == nil {
				return "data: " + string(b) + "\n\nevent: content_block_stop\n" + line
			}
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

func (c *agentToolReplyContext) remapAgentToolInputChanged(rawName string, input any) (string, any, bool) {
	if c != nil {
		if original, ok := c.wireToClient[rawName]; ok {
			return original, input, false
		}
	}
	spec, ok := agentToolMap[rawName]
	if !ok {
		return rawName, input, false
	}
	name, _ := c.callerToolName(rawName)
	args, ok := input.(map[string]any)
	if !ok || args == nil {
		return name, input, false
	}
	if c == nil || !c.hasArgRemaps(rawName) {
		return name, input, false
	}
	out := make(map[string]any, len(args))
	changed := false
	for k, v := range args {
		if mapped, exists := spec.Args[k]; exists && c.shouldRemapArg(rawName, k, mapped) {
			changed = true
			if mapped == nil {
				continue
			}
			out[*mapped] = v
			continue
		}
		out[k] = v
	}
	if !changed {
		return name, input, false
	}
	return name, out, true
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
		mapped, input, inputChanged := c.remapAgentToolInputChanged(name, block["input"])
		if mapped != name || inputChanged {
			block["name"] = mapped
			if inputChanged {
				block["input"] = input
			}
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

// ======================== Responses 形回复改写 ========================

func responsesToolItemRawName(item map[string]any) (string, bool) {
	typ, _ := item["type"].(string)
	switch typ {
	case "function_call", "tool_call", "custom_tool_call":
		name, _ := item["name"].(string)
		return name, name != ""
	case "apply_patch_call":
		return "apply_patch", true
	case "shell_call":
		return "shell", true
	default:
		return "", false
	}
}

func (c *agentToolReplyContext) rewriteResponsesOutputSlice(output []any) ([]any, bool) {
	if c == nil || !c.active() || len(output) == 0 {
		return output, false
	}
	kept := make([]any, 0, len(output))
	changed := false
	for _, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		rawName, isTool := responsesToolItemRawName(item)
		if !isTool {
			kept = append(kept, raw)
			continue
		}
		if !c.keepRawName(rawName) {
			changed = true
			continue
		}
		argKey := "arguments"
		args, _ := item["arguments"].(string)
		if args == "" {
			if inp, ok := item["input"].(string); ok && inp != "" {
				argKey = "input"
				args = inp
			}
		}
		mappedName, mappedArgs := c.remapAgentToolCall(rawName, args)
		if mappedName != rawName {
			item["name"] = mappedName
			changed = true
		}
		if mappedArgs != args {
			item[argKey] = mappedArgs
			changed = true
		}
		kept = append(kept, item)
	}
	if !changed {
		return output, false
	}
	return kept, true
}

func rewriteResponsesOutputBody(body []byte, c *agentToolReplyContext) []byte {
	if c == nil || !c.active() || len(body) == 0 || body[0] != '{' {
		return body
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	output, ok := m["output"].([]any)
	if !ok {
		return body
	}
	kept, changed := c.rewriteResponsesOutputSlice(output)
	if !changed {
		return body
	}
	m["output"] = kept
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// rewriteResponsesSSELine 在原生 Responses SSE 透传出口过滤/还原 manifest 与别名
// 工具调用。返回 (rewrittenLine, dropLine)；未改动且未丢弃时返回 (nil, false)。
func rewriteResponsesSSELine(line []byte, c *agentToolReplyContext) ([]byte, bool) {
	if c == nil || !c.active() {
		return nil, false
	}
	var prefix string
	var rest []byte
	switch {
	case bytes.HasPrefix(line, []byte("data: ")):
		prefix = "data: "
		rest = line[len("data: "):]
	case bytes.HasPrefix(line, []byte("data:")):
		prefix = "data:"
		rest = line[len("data:"):]
	default:
		return nil, false
	}
	payload := bytes.TrimSpace(rest)
	if len(payload) == 0 || payload[0] != '{' {
		return nil, false
	}
	var evt map[string]any
	if json.Unmarshal(payload, &evt) != nil {
		return nil, false
	}
	outputIndex := -1
	if v, ok := evt["output_index"].(float64); ok {
		outputIndex = int(v)
	}
	itemID, _ := evt["item_id"].(string)
	resolveOI := func() int {
		if outputIndex >= 0 {
			return outputIndex
		}
		if itemID != "" {
			if idx, ok := c.itemIDToIndex[itemID]; ok {
				return idx
			}
		}
		return -1
	}

	changed := false
	switch typ, _ := evt["type"].(string); typ {
	case "response.output_item.added", "response.output_item.done":
		item, _ := evt["item"].(map[string]any)
		if item == nil {
			return nil, false
		}
		rawName, isTool := responsesToolItemRawName(item)
		if !isTool {
			return nil, false
		}
		id, _ := item["id"].(string)
		callID, _ := item["call_id"].(string)
		oi := outputIndex
		if oi < 0 {
			if v, ok := item["output_index"].(float64); ok {
				oi = int(v)
			}
		}
		if !c.keepRawName(rawName) {
			c.noteDrop(oi, id, callID)
			return nil, true
		}
		c.noteRaw(oi, rawName, id, callID)
		argKey := "arguments"
		args, _ := item["arguments"].(string)
		if args == "" {
			if inp, ok := item["input"].(string); ok && inp != "" {
				argKey = "input"
				args = inp
			}
		}
		mappedName, mappedArgs := c.remapAgentToolCall(rawName, "")
		if args != "" {
			mappedName, mappedArgs = c.remapAgentToolCall(rawName, args)
		}
		if mappedName != rawName {
			item["name"] = mappedName
			changed = true
		}
		if mappedArgs != args {
			item[argKey] = mappedArgs
			changed = true
		}
	case "response.function_call_arguments.delta", "response.tool_call_arguments.delta", "response.custom_tool_call_input.delta":
		oi := resolveOI()
		if c.isDropped(oi, itemID) {
			return nil, true
		}
		delta, _ := evt["delta"].(string)
		if rawName := c.rawByIndex[oi]; rawName != "" && delta != "" {
			if frag := c.remapArgStream(oi, rawName, delta); frag != delta {
				evt["delta"] = frag
				changed = true
			}
		}
	case "response.function_call_arguments.done", "response.tool_call_arguments.done", "response.custom_tool_call_input.done":
		oi := resolveOI()
		if c.isDropped(oi, itemID) {
			return nil, true
		}
		c.flushArgPending(oi)
		argKey := "arguments"
		fullArgs, _ := evt["arguments"].(string)
		if fullArgs == "" {
			if inp, ok := evt["input"].(string); ok && inp != "" {
				argKey = "input"
				fullArgs = inp
			}
		}
		if rawName := c.rawByIndex[oi]; rawName != "" && fullArgs != "" {
			if _, mappedArgs := c.remapAgentToolCall(rawName, fullArgs); mappedArgs != fullArgs {
				evt[argKey] = mappedArgs
				changed = true
			}
		}
	case "response.completed", "response.incomplete", "response.done":
		if resp, ok := evt["response"].(map[string]any); ok && resp != nil {
			if output, ok := resp["output"].([]any); ok {
				if kept, outChanged := c.rewriteResponsesOutputSlice(output); outChanged {
					resp["output"] = kept
					changed = true
				}
			}
		}
	}
	if !changed {
		return nil, false
	}
	nb, err := json.Marshal(evt)
	if err != nil {
		return nil, false
	}
	return append([]byte(prefix), append(nb, '\n')...), false
}
