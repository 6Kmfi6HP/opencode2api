package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
)

// ======================== Anthropic 上游工具名兼容层 ========================
//
// 上游 Anthropic Messages API 强制 tools[].name 等字段满足
// ^[a-zA-Z0-9_-]{1,64}$（超长或含非法字符即 400 invalid_request_error）。
// 客户端（MCP 风格 mcp__server__tool、带点号/空格/Unicode 的 OpenAI 工具名）
// 合法发出的名字可能不满足该约束。本层在 anthropic 上游边界做确定性缩短，
// 并在响应呈现面按相反映射还原，保证：
//   - 无 name 触发的上游 400；
//   - 客户端可见名逐字节不变（所发即所得）；
//   - 映射是纯函数（sha256 派生），跨请求、跨重试、跨网关重启、跨轮历史
//     回放恒稳定。

const anthropicMaxNameLength = 64 // 上游 name 硬上限（字节；缩短后恒纯 ASCII，字节==字符）

const unnamedAnthropicToolName = "unnamed_tool" // 空 name 的合法占位名

// isAnthropicSafeByte 报告 b 是否属于上游允许的字符集 [a-zA-Z0-9_-]。
func isAnthropicSafeByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' ||
		b >= '0' && b <= '9' || b == '_' || b == '-'
}

// isAnthropicToolNameValid 报告 name 是否已满足上游约束。一次字节扫描同时
// 判定字符集与长度：任何非 ASCII 字节直接不合法（比按 rune 计数的语义更严
// ——64 个 CJK rune ≈192 字节在 rune 语义下会漏放行，字节语义下必然走缩短，
// 杜绝上游按字节校验时的潜在 400）。手写循环，不用 regexp。
func isAnthropicToolNameValid(name string) bool {
	if name == "" || len(name) > anthropicMaxNameLength {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isAnthropicSafeByte(name[i]) {
			return false
		}
	}
	return true
}

// foldAnthropicName 把每个 [a-zA-Z0-9_-] 之外的 rune 折为 '_'（多字节 rune
// 折 1 个 '_'）；输出恒纯 ASCII，长度（字节）≤ 原 rune 数。先折叠再截断
// 永不截坏 rune。
func foldAnthropicName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if r < 128 && isAnthropicSafeByte(byte(r)) {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return b.String()
}

// anthropicHashedName 生成恒 64 字节的强制哈希形短名：
//
//	foldAnthropicName(name)[:47] + "-" + hex(sha256(原始全名)前 8 字节)  // 47+1+16=64
//
// 哈希输入取折叠前的原始全名：仅在非法字符处或第 47 字符之后不同的两个名字
// 得到不同短名。对合法字符集的超长 ASCII 名，输出与 shortenResponsesName
// 旧实现逐字节一致（既有透传 fixture 不破）。
func anthropicHashedName(name string) string {
	sum := sha256.Sum256([]byte(name))
	suffix := hex.EncodeToString(sum[:8]) // 16 hex chars
	folded := foldAnthropicName(name)
	if keep := anthropicMaxNameLength - 1 - len(suffix); len(folded) > keep { // 47
		folded = folded[:keep]
	}
	return folded + "-" + suffix
}

// shortenAnthropicToolName 纯函数（确定性、幂等、跨请求/重启稳定）：
//   - 已合法 → 原样返回（直通快路径）；
//   - 折叠后 ≤64 字节 → 折叠名（可读前缀保留，如 "mcp.example.com__tool" →
//     "mcp_example_com__tool"）；
//   - 折叠后 >64 → anthropicHashedName（恒 64）。
//
// 注意空串：folded=="" ≤64 会返回 ""——空名改写由 shortenRecord 处理。
func shortenAnthropicToolName(name string) string {
	if isAnthropicToolNameValid(name) {
		return name
	}
	folded := foldAnthropicName(name)
	if len(folded) <= anthropicMaxNameLength {
		return folded
	}
	return anthropicHashedName(name)
}

// ======================== Anthropic 形请求体遍历 ========================

// anySliceValue 宽松统一两种数组视图：json.Unmarshal 产物 []any 与转换器
// 构造的 []map[string]any（map 是引用类型，原地改写生效）。
func anySliceValue(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case []map[string]any:
		if t == nil {
			return nil, false
		}
		out := make([]any, len(t))
		for i, m := range t {
			out[i] = m
		}
		return out, true
	}
	return nil, false
}

// shortenMcpServerNames 缩短 body["mcp_servers"][].name 并登记
// rw.mcpServers（缩短后 server 名 -> 原始名），供 mcp__<server>__<tool>
// 复合名的双向映射。返回是否发生改动。
func (rw *responsesNameRewrites) shortenMcpServerNames(body map[string]any) bool {
	servers, ok := anySliceValue(body["mcp_servers"])
	if !ok {
		return false
	}
	changed := false
	for _, s := range servers {
		sm, ok := s.(map[string]any)
		if !ok {
			continue
		}
		name, ok := sm["name"].(string)
		if !ok {
			continue
		}
		mapped := rw.shortenRecord(name)
		if mapped != name {
			sm["name"] = mapped
			rw.mcpServers[mapped] = name
			rw.mcpOutbound[name] = mapped
			changed = true
		}
	}
	return changed
}

// mapAnthropicName 单个 name 的出站映射：优先 mcp__<原始 server>__<tool>
// 复合名改写（该 server 已被本请求缩短时，使历史回放与上游按缩短 server 名
// 拼出的复合工具名一致；改写结果走与 shortenRecord 相同的碰撞消歧），
// 否则 shortenRecord。
func (rw *responsesNameRewrites) mapAnthropicName(name string) string {
	if len(rw.mcpOutbound) > 0 && strings.HasPrefix(name, "mcp__") {
		after, _ := strings.CutPrefix(name, "mcp__")
		if server, tool, found := strings.Cut(after, "__"); found {
			if short, ok := rw.mcpOutbound[server]; ok {
				composed := "mcp__" + short + "__" + tool
				if composed == name {
					return name
				}
				if !isAnthropicToolNameValid(composed) || rw.taken[composed] {
					composed = rw.disambiguate(composed)
				}
				rw.inbound[composed] = name
				rw.taken[composed] = true
				return composed
			}
		}
	}
	return rw.shortenRecord(name)
}

// walkAnthropicToolNames 按固定顺序遍历 Anthropic Messages body 中受约束的
// name 位置并写回 mapFn 的返回值：tools[].name（含 tools[].function.name 双
// 形状兜底）→ tool_choice.name（含 function.name 兜底）→ messages[].content[]
// 中 type=="tool_use" 块的 name。缺失 name 键与 type!="tool_use" 块
// （tool_result 按 tool_use_id 关联，无 name 约束）跳过。返回是否发生改动。
func walkAnthropicToolNames(body map[string]any, mapFn func(string) string) bool {
	changed := false
	set := func(name string) string {
		mapped := mapFn(name)
		if mapped != name {
			changed = true
		}
		return mapped
	}
	if tools, ok := anySliceValue(body["tools"]); ok {
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			if name, ok := tm["name"].(string); ok {
				if m := set(name); m != name {
					tm["name"] = m
				}
			}
			if fn, ok := tm["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok {
					if m := set(name); m != name {
						fn["name"] = m
					}
				}
			}
		}
	}
	if tc, ok := body["tool_choice"].(map[string]any); ok {
		if name, ok := tc["name"].(string); ok {
			if m := set(name); m != name {
				tc["name"] = m
			}
		}
		if fn, ok := tc["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				if m := set(name); m != name {
					fn["name"] = m
				}
			}
		}
	}
	if msgs, ok := anySliceValue(body["messages"]); ok {
		for _, msg := range msgs {
			mm, ok := msg.(map[string]any)
			if !ok {
				continue
			}
			content, ok := anySliceValue(mm["content"])
			if !ok {
				continue
			}
			for _, c := range content {
				block, ok := c.(map[string]any)
				if !ok {
					continue
				}
				if typ, _ := block["type"].(string); typ != "tool_use" {
					continue
				}
				if name, ok := block["name"].(string); ok {
					if m := set(name); m != name {
						block["name"] = m
					}
				}
			}
		}
	}
	return changed
}

// visitAnthropicNames 只读遍历 Anthropic Messages body 中全部受约束的 name
// 值（不写回）：tools[].name（含 function.name 兜底）→ tool_choice.name
// （含 function.name 兜底）→ messages[].content[] 的 tool_use 块 name →
// mcp_servers[].name。缺失 name 键与 type!="tool_use" 块跳过。
func visitAnthropicNames(body map[string]any, visit func(name string)) {
	if tools, ok := anySliceValue(body["tools"]); ok {
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			if name, ok := tm["name"].(string); ok {
				visit(name)
			}
			if fn, ok := tm["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok {
					visit(name)
				}
			}
		}
	}
	if tc, ok := body["tool_choice"].(map[string]any); ok {
		if name, ok := tc["name"].(string); ok {
			visit(name)
		}
		if fn, ok := tc["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				visit(name)
			}
		}
	}
	if msgs, ok := anySliceValue(body["messages"]); ok {
		for _, msg := range msgs {
			mm, ok := msg.(map[string]any)
			if !ok {
				continue
			}
			content, ok := anySliceValue(mm["content"])
			if !ok {
				continue
			}
			for _, c := range content {
				block, ok := c.(map[string]any)
				if !ok {
					continue
				}
				if typ, _ := block["type"].(string); typ != "tool_use" {
					continue
				}
				if name, ok := block["name"].(string); ok {
					visit(name)
				}
			}
		}
	}
	if servers, ok := anySliceValue(body["mcp_servers"]); ok {
		for _, s := range servers {
			sm, ok := s.(map[string]any)
			if !ok {
				continue
			}
			if name, ok := sm["name"].(string); ok {
				visit(name)
			}
		}
	}
}

// shortenAnthropicBodyNames 两阶段遍历 Anthropic Messages 请求体中全部受
// ^[a-zA-Z0-9_-]{1,64}$ 约束的 name 字段，统一登记/缩短：
//
//	阶段 1（prepare，只读）：对合法直通名先登记 taken 占位——之后折叠名
//	（如 "a.b" 折叠出的 "a_b"）永不抢注声明过的合法名。
//	阶段 2：mcp_servers[].name 先行（复合名改写依赖 mcpServers），再按固定
//	顺序写回 tools → tool_choice → 历史 tool_use 的 name。
//
// 返回是否发生改动。幂等；全合法 → false 且请求体字节不变（快路径）。
func (rw *responsesNameRewrites) shortenAnthropicBodyNames(body map[string]any) bool {
	visitAnthropicNames(body, func(name string) {
		if isAnthropicToolNameValid(name) {
			rw.taken[name] = true
		}
	})
	changed := rw.shortenMcpServerNames(body)
	if walkAnthropicToolNames(body, rw.mapAnthropicName) {
		changed = true
	}
	return changed
}

// ======================== 收口与还原辅助 ========================

// sanitizeAnthropicUpstreamBody 解析 marshaled Anthropic Messages 请求体 →
// shortenAnthropicBodyNames → 有改动才重 marshal。全合法时原字节返回、映射
// 空（快路径）。供 chat/responses 入站路径包在既有 body builder 输出外
// （builder 零改动）。
func sanitizeAnthropicUpstreamBody(rawBody []byte, restoreStubCase bool) ([]byte, *responsesNameRewrites) {
	rw := newResponsesNameRewrites(restoreStubCase)
	var bodyMap map[string]any
	if err := json.Unmarshal(rawBody, &bodyMap); err != nil {
		return rawBody, rw
	}
	if !rw.shortenAnthropicBodyNames(bodyMap) {
		return rawBody, rw
	}
	if b, err := json.Marshal(bodyMap); err == nil {
		return b, rw
	}
	return rawBody, rw
}

// restoreAnthropicStreamLine 对一行 SSE（含结尾 \n）做 name 还原：仅当它是
// "data: " 帧、事件为 content_block_start / content_block_delta 并携带
// tool_use 的 name 时经 rw.restore 改写该行，其余行（含
// content_block_stop、input_json_delta 的 partial_json 文本、非 data 行）
// 原样返回。rw.restoreNoop 时原样返回，不做 JSON 解析。改写只命中 name
// 字段，不动其它字节（换行风格、字段顺序保持上游原样）。幂等。
func restoreAnthropicStreamLine(line string, rw *responsesNameRewrites) string {
	if rw.restoreNoop() {
		return line
	}
	payload, ok := strings.CutPrefix(line, "data: ")
	if !ok {
		return line
	}
	trimmed := strings.TrimRight(payload, "\r\n")
	if !strings.HasPrefix(trimmed, "{") {
		return line
	}
	var evt map[string]any
	if json.Unmarshal([]byte(trimmed), &evt) != nil {
		return line
	}
	changed := false
	apply := func(cb map[string]any) {
		if typ, _ := cb["type"].(string); typ == "tool_use" {
			if n, _ := cb["name"].(string); n != "" {
				if r := rw.restore(n); r != n {
					cb["name"] = r
					changed = true
				}
			}
		}
	}
	if cb, ok := evt["content_block"].(map[string]any); ok {
		apply(cb)
	}
	// 兜底:某些上游把起始块放在 event.delta.content_block 而非顶层
	// content_block。
	if !changed {
		if delta, ok := evt["delta"].(map[string]any); ok {
			if cb, ok := delta["content_block"].(map[string]any); ok {
				apply(cb)
			}
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

// restoreAnthropicStreamLineCase 兼容包装：签名与行为与旧实现逐位一致
// （free_tier_case_restore_test.go 零改动）——空映射 + 占位名大小写门控。
func restoreAnthropicStreamLineCase(line string, restoreCase bool) string {
	return restoreAnthropicStreamLine(line, newResponsesNameRewrites(restoreCase))
}

// restoreAnthropicBodyNames 对完整 Anthropic Messages JSON body 的 content
// 数组做 tool_use name 还原（非流式直通写给客户端前调用）。restoreNoop 时
// 原样返回，不做 JSON 解析。幂等。
func restoreAnthropicBodyNames(body []byte, rw *responsesNameRewrites) []byte {
	if rw.restoreNoop() {
		return body
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	if restoreAnthropicContentNames(m, rw) {
		if b, err := json.Marshal(m); err == nil {
			return b
		}
	}
	return body
}

// restoreAnthropicContentNames 就地还原 m["content"] 数组中 tool_use 块的
// name。返回是否发生改动。
func restoreAnthropicContentNames(m map[string]any, rw *responsesNameRewrites) bool {
	content, ok := m["content"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, c := range content {
		block, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := block["type"].(string); typ != "tool_use" {
			continue
		}
		if n, _ := block["name"].(string); n != "" {
			if r := rw.restore(n); r != n {
				block["name"] = r
				changed = true
			}
		}
	}
	return changed
}

// restoreAnthropicBodyToolCase 兼容包装：签名与行为与旧实现逐位一致。
func restoreAnthropicBodyToolCase(body []byte, restoreCase bool) []byte {
	return restoreAnthropicBodyNames(body, newResponsesNameRewrites(restoreCase))
}

// restoreAnthropicResponseNames 跨协议非流式还原（anthropic 形响应）：先按
// 单个 Anthropic Messages JSON 还原 content[].tool_use.name；无 content 数组
// 时按 SSE "data: " 行逐行还原 content_block.name / delta.content_block.name
// （上游对非流式请求偶尔回 SSE 的兜底）。restoreNoop 时零解析原样返回；
// 幂等。放在跨协议转换之前，转换器天然携带原名。
func restoreAnthropicResponseNames(body []byte, rw *responsesNameRewrites) []byte {
	if rw.restoreNoop() {
		return body
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err == nil {
		if _, ok := m["content"].([]any); ok {
			if restoreAnthropicContentNames(m, rw) {
				if b, err := json.Marshal(m); err == nil {
					return b
				}
			}
			return body
		}
	}
	lines := strings.Split(string(body), "\n")
	changed := false
	for i, line := range lines {
		if restored := restoreAnthropicStreamLine(line, rw); restored != line {
			lines[i] = restored
			changed = true
		}
	}
	if !changed {
		return body
	}
	return []byte(strings.Join(lines, "\n"))
}

// restoreResponsesBodyNames Responses 形 payload 还原（claude→responses 上游
// 路径）：复用 restoreResponsesPayloadNames（子键已含 "output"），解析 →
// 遍历 → 有命中才重 marshal。restoreNoop 时原样返回。
func restoreResponsesBodyNames(body []byte, rw *responsesNameRewrites) []byte {
	if rw.restoreNoop() {
		return body
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	if !rw.restoreResponsesPayloadNames(m) {
		return body
	}
	if b, err := json.Marshal(m); err == nil {
		return b
	}
	return body
}

// sanitizeResponsesUpstreamBody marshaled Responses 请求体缩短（claude→
// responses 上游路径）。必须 parse-mutate-remarshal：claudeToResponsesBody
// 构造的 tools 是类型化切片，shortenResponsesBodyNames 的 []any 断言对它
// 静默 no-op——先解析成 []any 才能命中。
func sanitizeResponsesUpstreamBody(rawBody []byte, rw *responsesNameRewrites) []byte {
	var bodyMap map[string]any
	if err := json.Unmarshal(rawBody, &bodyMap); err != nil {
		return rawBody
	}
	if !rw.shortenResponsesBodyNames(bodyMap) {
		return rawBody
	}
	if b, err := json.Marshal(bodyMap); err == nil {
		return b
	}
	return rawBody
}

// ======================== ctx 线程化 ========================

type anthropicNameRewritesContextKey struct{}

// withAnthropicNameRewrites 把映射挂到请求 ctx，响应呈现面经
// anthropicNameRewritesFromContext 取回（DriveStreamWithRetry 原样透传 ctx，
// 流式与非流式同源）。
func withAnthropicNameRewrites(ctx context.Context, rw *responsesNameRewrites) context.Context {
	return context.WithValue(ctx, anthropicNameRewritesContextKey{}, rw)
}

// anthropicNameRewritesFromContext 取请求级映射；nil 安全。
func anthropicNameRewritesFromContext(ctx context.Context) *responsesNameRewrites {
	if ctx == nil {
		return nil
	}
	rw, _ := ctx.Value(anthropicNameRewritesContextKey{}).(*responsesNameRewrites)
	return rw
}

// nameRewritesFor 响应呈现面的取用入口：ctx 有映射用之；否则按现状构造
// （占位名大小写门控跟随 UA）——与 pipeAnthropicStream / relayAnthropicBuffered
// 直呼测试（context.Background()）的既有行为逐位一致。
func nameRewritesFor(ctx context.Context) *responsesNameRewrites {
	if rw := anthropicNameRewritesFromContext(ctx); rw != nil {
		return rw
	}
	return newResponsesNameRewrites(shouldRestoreToolCase(ctx))
}

// disambiguate 确定性消歧：首选 anthropicHashedName(name)；若仍被占（sha256
// 冲突，实际不可能）则把 "#k"（k=1,2,...）混入哈希输入重派，上限 100 次，
// 保证终止且同一请求内确定。
func (rw *responsesNameRewrites) disambiguate(name string) string {
	candidate := anthropicHashedName(name)
	if !rw.taken[candidate] {
		return candidate
	}
	for k := 1; k <= 100; k++ {
		candidate = anthropicHashedName(name + "#" + strconv.Itoa(k))
		if !rw.taken[candidate] {
			return candidate
		}
	}
	return candidate
}

// responsesMaxNameLength 是上游 Anthropic 风格 name 字段的硬性上限（字符数）。
const responsesMaxNameLength = 64

// shortenResponsesName 把不满足上游约束（^[a-zA-Z0-9_-]{1,64}$）的 name
// 确定性缩短为 <=64。已合法的名字原样返回；含非法字符（点号/空格/Unicode
// 等）的短名折叠为 '_'；超长名保留原名的尾部相关段（通常是工具动作名），
// 前缀加哈希后缀，保证同名输入稳定得到同一缩短名（跨请求可观察、可缓存），
// 不同名几乎不碰撞（哈希 64 bit）。
func shortenResponsesName(name string) string {
	return shortenAnthropicToolName(name)
}

// responsesNameRewrites 记录出站缩短映射（original -> shortened）与
// 入站还原映射（shortened -> original）。
type responsesNameRewrites struct {
	outbound map[string]string // original -> shortened
	inbound  map[string]string // shortened -> original
	// mcpServers 登记缩短后的 server 名 -> 原始名，供响应侧 mcp__<server>__<tool>
	// 复合名的前缀还原（mcp_servers[].name 被缩短时）。
	mcpServers map[string]string
	// mcpOutbound 登记原始 server 名 -> 缩短后名，供请求侧 mcp__<原始 server>__<tool>
	// 复合名的历史回放改写（与 mcpServers 成对）。
	mcpOutbound map[string]string
	// taken 登记上游侧已占用的名字（合法直通 + 缩短结果），防折叠名与
	// 声明过的合法名碰撞（"a.b" 折叠出的 "a_b" 抢注声明的 "a_b"）。
	taken map[string]bool
	// restoreStubCase 为 true(Claude 系客户端)时 restore fallback 才做
	// 免费层小写占位名的大小写还原;其余保持小写透传。构造时一次性填定,
	// 之后只读,无并发问题。
	restoreStubCase bool
}

func newResponsesNameRewrites(restoreStubCase bool) *responsesNameRewrites {
	return &responsesNameRewrites{
		outbound:        map[string]string{},
		inbound:         map[string]string{},
		mcpServers:      map[string]string{},
		mcpOutbound:     map[string]string{},
		taken:           map[string]bool{},
		restoreStubCase: restoreStubCase,
	}
}

func (rw *responsesNameRewrites) empty() bool {
	return rw == nil || len(rw.outbound) == 0
}

// restoreNoop 还原侧快路径判定：无映射且不做占位名大小写还原 → 响应侧零
// JSON 解析、字节原样。
func (rw *responsesNameRewrites) restoreNoop() bool {
	return rw == nil || (len(rw.inbound) == 0 && !rw.restoreStubCase)
}

// shortenRecord 缩短 name 并登记映射；已合法（直通快路径）时仅登记占用，
// 不进映射（保持 inbound 空映射的快路径语义）。缩短结果与已占用名碰撞时
// 确定性消歧。同一原名在同一请求内恒同一短名（tool_choice/历史与 tools[]
// 必然一致）。
func (rw *responsesNameRewrites) shortenRecord(name string) string {
	if shortened, ok := rw.outbound[name]; ok {
		return shortened // 缓存命中：同一原名恒同一短名
	}
	shortened := shortenResponsesName(name)
	if name == "" {
		// 上游要求 1-64；空名合成合法占位名，还原侧映射回 ""。
		shortened = unnamedAnthropicToolName
	}
	if shortened == name {
		if !rw.taken[name] {
			rw.taken[name] = true // 合法名直通：仅登记占用，不进映射
		}
		return name
	}
	if rw.taken[shortened] {
		// 折叠名与直通名/已登记名碰撞（"a.b" 折叠出的 "a_b" 撞上声明的
		// "a_b"）：确定性消歧为哈希形，双向映射保证两个原名都正确还原。
		shortened = rw.disambiguate(name)
	}
	rw.outbound[name] = shortened
	rw.inbound[shortened] = name
	rw.taken[shortened] = true
	return shortened
}

// restore 把上游响应里的缩短名还原为客户端原始名；未被我们缩短过的名字
// 在 restoreStubCase 为 true(Claude 系客户端)时再退到免费层占位工具的
// 大小写还原（restoreToolNameCase），覆盖只注入了门禁 stub、没有任何缩短
// 映射的原生透传路径；mcp_servers[].name 被本请求缩短时，上游按缩短 server
// 名拼出的 mcp__<short>__<tool> 复合名经前缀映射还原；都不命中时原样返回。
func (rw *responsesNameRewrites) restore(name string) string {
	if rw != nil {
		if original, ok := rw.inbound[name]; ok {
			return original
		}
		if len(rw.mcpServers) > 0 {
			if after, ok := strings.CutPrefix(name, "mcp__"); ok {
				if server, tool, found := strings.Cut(after, "__"); found {
					if orig, ok := rw.mcpServers[server]; ok {
						return "mcp__" + orig + "__" + tool
					}
				}
			}
		}
		if rw.restoreStubCase {
			return restoreToolNameCase(name)
		}
		return name
	}
	return name
}

// shortenResponsesBodyNames 统一处理请求体中所有会出现 name 的位置：
// tools[].name、tools[].function.name、tool_choice.name、input[].name
// （function_call / custom_tool_call 等历史工具调用项）。先做一轮 prepare
// （合法直通名登记 taken，折叠名永不抢注声明过的合法名），再逐位置缩短。
// 返回是否发生改动。
func (rw *responsesNameRewrites) shortenResponsesBodyNames(body map[string]any) bool {
	// prepare：合法直通名先占位 taken（仅登记，不改写）。
	walkResponsesNames(body, func(name string) string {
		if isAnthropicToolNameValid(name) {
			rw.taken[name] = true
		}
		return name
	})
	changed := walkResponsesNames(body, rw.shortenRecord)
	return changed
}

// walkResponsesNames 按 tools → tool_choice → input 的固定顺序遍历 body 中
// 全部 name 位置并写回 mapFn 的返回值。返回是否发生改动。
func walkResponsesNames(body map[string]any, mapFn func(string) string) bool {
	changed := false
	set := func(name string) string {
		mapped := mapFn(name)
		if mapped != name {
			changed = true
		}
		return mapped
	}
	if tools, ok := body["tools"].([]any); ok {
		for i, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			if name, ok := tm["name"].(string); ok && name != "" {
				if shortened := set(name); shortened != name {
					tm["name"] = shortened
					tools[i] = tm
				}
			}
			if fn, ok := tm["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok && name != "" {
					if shortened := set(name); shortened != name {
						fn["name"] = shortened
						tm["function"] = fn
						tools[i] = tm
					}
				}
			}
		}
	}
	if tc, ok := body["tool_choice"]; ok {
		switch v := tc.(type) {
		case map[string]any:
			if name, ok := v["name"].(string); ok && name != "" {
				if shortened := set(name); shortened != name {
					v["name"] = shortened
					body["tool_choice"] = v
				}
			}
			if fn, ok := v["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok && name != "" {
					if shortened := set(name); shortened != name {
						fn["name"] = shortened
						v["function"] = fn
						body["tool_choice"] = v
					}
				}
			}
		}
	}
	if input, ok := body["input"].([]any); ok {
		for i, item := range input {
			im, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if name, ok := im["name"].(string); ok && name != "" {
				if shortened := set(name); shortened != name {
					im["name"] = shortened
					input[i] = im
				}
			}
		}
	}
	return changed
}

// restoreResponsesPayloadNames 把上游响应 JSON（单个 output item、completed
// 事件的 response 对象或整个非流式响应）中出现的缩短名还原为原始名。
// 只还原 rw.inbound 中登记过的名字，最大限度避免误改用户自然语言文本。
// 返回是否发生改动。
func (rw *responsesNameRewrites) restoreResponsesPayloadNames(v any) bool {
	// 只保留 nil 守卫：身份映射也要进入，因为 restore 现在还会兜底做免费层
	// 占位工具的大小写还原（rw.inbound 为空时 restore 退到 restoreToolNameCase）。
	if rw == nil {
		return false
	}
	changed := false
	switch t := v.(type) {
	case map[string]any:
		if name, ok := t["name"].(string); ok && name != "" {
			if original := rw.restore(name); original != name {
				t["name"] = original
				changed = true
			}
		}
		for _, key := range []string{"item", "response", "output", "content"} {
			if child, ok := t[key]; ok {
				if rw.restoreResponsesPayloadNames(child) {
					changed = true
				}
			}
		}
	case []any:
		for _, child := range t {
			if rw.restoreResponsesPayloadNames(child) {
				changed = true
			}
		}
	}
	return changed
}
