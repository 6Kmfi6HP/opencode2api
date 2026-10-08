package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
)

var (
	// volatileTokenCountRe 匹配客户端逐请求注入的上下文剩余计数块
	// (<total_tokens>N tokens left</total_tokens>,实测每轮值变化)。连同周围
	// 空白一起剥离:客户端每轮以 \n\n 分隔累积追加计数,只剥计数会留下逐轮
	// 累积的空行,字节依旧漂移。
	volatileTokenCountRe = regexp.MustCompile(`(?s)\s*<total_tokens>.*?</total_tokens>\s*`)
	// systemReminderWrapRe 匹配整段仅由 system-reminder 块构成的文本(客户端
	// 注入的上下文,逐请求改写/重排,不能做会话标识)。
	systemReminderWrapRe = regexp.MustCompile(`(?s)^\s*(?:<system-reminder>.*?</system-reminder>\s*)+$`)
)

// stableUserTextFromParts 从 user 消息 content parts 里挑会话标识文本:跳过
// 整段 system-reminder 包裹的注入上下文(Claude Code 把 codebase 上下文、
// attribution、token 计数逐请求注入首条 user 的前部 part),优先真实请求
// 文本;全为注入块时回退首个非空 part 并剥离计数块。
func stableUserTextFromParts(parts []any) string {
	fallback := ""
	for _, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		t, _ := pm["text"].(string)
		if t == "" {
			if it, ok := pm["input_text"].(string); ok {
				t = it
			}
		}
		if t == "" {
			continue
		}
		if fallback == "" {
			fallback = t
		}
		if systemReminderWrapRe.MatchString(t) {
			continue
		}
		return strings.TrimSpace(volatileTokenCountRe.ReplaceAllString(t, ""))
	}
	if fallback != "" {
		return strings.TrimSpace(volatileTokenCountRe.ReplaceAllString(fallback, ""))
	}
	return ""
}

// stripVolatileCountersText 剥离文本里的 <total_tokens> 上下文计数块。
// CLI 把计数以 role=system 消息注入,转换后并入 instructions,值逐请求变化
// 会让 instructions 跨轮漂移、上游前缀缓存从 instructions 就断。
func stripVolatileCountersText(s string) string {
	return volatileTokenCountRe.ReplaceAllString(s, "")
}

// stripVolatileTokenCountersInPlace 原地剥离 user 消息 text 里的 <total_tokens>
// 上下文计数块与累积空行,返回是否有改动。CLI 每轮改写这些计数的值(实测逐
// 请求变化),计数留在消息流里会让上游前缀缓存从首个含计数的消息断掉——
// cached 恒 ≈ instructions+tools,命中率封顶 ~50%。剥离后消息流字节稳定:
// 第 N 轮请求体成为第 N+1 轮请求体的真前缀,前缀缓存覆盖全部历史。计数只是
// 上下文剩余的提示性信息,丢弃不影响任务语义;hand-back 等通知正文保留原位
// 不动。
func stripVolatileTokenCountersInPlace(input []any) bool {
	changed := false
	for _, it := range input {
		item, ok := it.(map[string]any)
		if !ok {
			continue
		}
		// chat 形状下 CLI 把计数以 role=system 消息注入、join 进首条 system
		// 消息(实测 ling-3.1-flash-free 上 system 尾部累积 3 处计数,值逐请求
		// 变化)——system/developer 的字符串 content 同样剥离;assistant 输出
		// 逐请求稳定,不动。
		role, _ := item["role"].(string)
		if role != "user" && role != "system" && role != "developer" {
			continue
		}
		switch content := item["content"].(type) {
		case string:
			rest := strings.TrimSpace(volatileTokenCountRe.ReplaceAllString(content, ""))
			if rest != content {
				item["content"] = rest
				changed = true
			}
		case []any:
			out := content[:0:0]
			for _, p := range content {
				pm, ok := p.(map[string]any)
				if !ok {
					out = append(out, p)
					continue
				}
				// 非文本 part(input_image 等)原样保留。
				t, hasText := pm["text"].(string)
				if !hasText {
					out = append(out, p)
					continue
				}
				// 剥计数块并一律 TrimSpace:客户端每轮往 user part 追加 \n\n
				// (计数本体走 role=system 消息),空行逐轮累积同样漂移。
				rest := strings.TrimSpace(volatileTokenCountRe.ReplaceAllString(t, ""))
				if rest == "" {
					changed = true
					continue
				}
				if rest != t {
					pm["text"] = rest
					changed = true
				}
				out = append(out, pm)
			}
			item["content"] = out
		}
	}
	return changed
}

// stripVolatileCountersInMap 对上游请求体 map(instructions / input / messages
// 三种形状)剥离 <total_tokens> 计数块与末条 user 的 SessionStart hook 注入
// 前缀,返回是否有改动。buildUpstreamBodyFromClaude(chat.go)与发送侧
// buildOCRequestWithSubpathAndState(opencode.go,指纹重做之后)都经此处,
// 一处接入覆盖两条路径。
func stripVolatileCountersInMap(m map[string]any) bool {
	changed := false
	if s, ok := m["instructions"].(string); ok && strings.Contains(s, "<total_tokens>") {
		m["instructions"] = stripVolatileCountersText(s)
		changed = true
	}
	if input, ok := m["input"].([]any); ok {
		if stripVolatileTokenCountersInPlace(input) {
			changed = true
		}
		// hook 前缀须在计数剥离之后处理:计数块可能出现在文本头部、遮住
		// marker,先剥计数 marker 才回到文本开头。
		if stripHookPrefixFromLastUser(input) {
			changed = true
		}
	}
	if msgs, ok := m["messages"].([]any); ok {
		if stripVolatileTokenCountersInPlace(msgs) {
			changed = true
		}
		if stripHookPrefixFromLastUser(msgs) {
			changed = true
		}
	}
	return changed
}

// stripVolatileCountersFromBody 对已 marshal 的上游请求体剥离 <total_tokens>
// 计数块,有改动才重新 marshal。解析失败或无改动时原样返回。
func stripVolatileCountersFromBody(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	if !stripVolatileCountersInMap(m) {
		return body
	}
	b, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return b
}

// hookCtxMarker SessionStart hook 注入块的固定开头模板(后跟空格、换行与连续
// 行首 XML 块,总长实测 5,455-29,931 字节)。
const hookCtxMarker = "SessionStart hook additional context:"

// hookOpenTagRe 匹配行首 XML 开标签并捕获标签名。Go regexp(RE2)不支持反向
// 引用 \1,闭合标签 </tag> 用 strings.Index 按名手工配对。
var hookOpenTagRe = regexp.MustCompile(`^<([A-Za-z][\w-]*)>`)

// hookCloseTagRe 匹配行首 session_* 闭合标签:Data References 引用块插进
// XML 块序列中间会让外层块的闭合标签落单(实测样本引用块后紧跟
// </session_guide>),需随注入一起剥;限定 session_ 前缀防止吃掉正文开头
// 的普通闭合标签。
var hookCloseTagRe = regexp.MustCompile(`^</(session_[A-Za-z][\w-]*)>`)

// hookDataRefMarker 新 CLI 在 XML 块序列中间插入的 "Data References" 折叠
// 引用块的固定开头。
const hookDataRefMarker = "## Data References\n- "

const (
	// hookDataRefMax 引用块最大长度(引用的字节数/文本逐轮变化,超过视为正文)。
	hookDataRefMax = 2048
	// hookBareTextMax XML 序列后裸文本尾段(如 output style 提示)的最大长度,
	// 超过视为正文内容不再剥离。
	hookBareTextMax = 4096
	// hookMinRemaining 剥离后剩余正文的最小长度,低于视为误判返回原文。
	// 入口特征(SessionStart hook 模板/## Data References + — query)本身已
	// 足够强,阈值只需挡住空正文;提得过高会把短指令轮(如 155 字节提问)
	// 的正常剥离误判为误判而放回残块。
	hookMinRemaining = 20
)

// isHookSpaceByte 判断 \s 字符(与 regexp 语义一致的 ASCII 集合)。
func isHookSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// skipHookDataRefBlock 从注入文本当前位置跳过 "## Data References" 折叠引用
// 块:到首个 \n\n、总长 ≤2KB、且含 " — query" 检索特征才剥,防误吞正文里的
// 同名段落。命中返回消费长度(含结尾 \n\n),否则 -1。
func skipHookDataRefBlock(sub string) int {
	if !strings.HasPrefix(sub, hookDataRefMarker) {
		return -1
	}
	end := strings.Index(sub, "\n\n")
	if end < 0 {
		return -1
	}
	block := sub[:end]
	if len(block) > hookDataRefMax || !strings.Contains(block, " — query") {
		return -1
	}
	return end + 2
}

// hookDataRefHead 形态 2 的开头标志:resume/continue hook 的引用块直接开头,
// SessionStart 的 XML 块反而夹在注入序列中段。
const hookDataRefHead = "## Data References"

// hookKnowledgeClose 两种开头形态的公共结尾锚点:整个注入序列以最后一次
// </session_knowledge> + 非空裸文本(output style 提示)+ \n\n 结尾,正文
// 从那之后开始。
const hookKnowledgeClose = "</session_knowledge>"

// hookBareTailHasXML 判断锚点后的裸文本尾段是否含 </session_*> 之外的 XML
// 结构:出现其他 < 开头标签说明锚点误落在正文里,拒绝该锚点。
func hookBareTailHasXML(bare string) bool {
	for i := 0; i < len(bare); {
		if bare[i] != '<' {
			i++
			continue
		}
		if strings.HasPrefix(bare[i:], "</session_") {
			j := strings.IndexByte(bare[i:], '>')
			if j < 0 {
				return true
			}
			i += j + 1
			continue
		}
		return true
	}
	return false
}

// stripHookByTailAnchor 结尾锚点剥离(优先路径):找最后一次
// </session_knowledge>,其后跳过非空裸文本(≤4KB,不含 </session_*> 之外的
// XML 结构)到首个 \n\n(含),剩余为正文。anchored=false 表示锚点缺失或尾段
// 形态不符,调用方回退单元序列贪心;护栏不通过时返回原文。
func stripHookByTailAnchor(s string) (string, bool) {
	idx := strings.LastIndex(s, hookKnowledgeClose)
	if idx < 0 {
		return s, false
	}
	// 锚点必须在文本前 90% 以内:更靠后说明它出现在正文里,剥到尾部会毁掉正文。
	if idx > len(s)*9/10 {
		return s, true
	}
	tail := s[idx+len(hookKnowledgeClose):]
	bareEnd := strings.Index(tail, "\n\n")
	if bareEnd <= 0 || bareEnd > hookBareTextMax {
		return s, false
	}
	if hookBareTailHasXML(tail[:bareEnd]) {
		return s, false
	}
	body := tail[bareEnd+2:]
	// 护栏:剩余正文过短或剥离总量超过原文 80% 视为误判,返回原文
	// (宁可漏剥不可错剥)。
	if len(body) < hookMinRemaining || len(s)-len(body) > len(s)*4/5 {
		return s, true
	}
	return body, true
}

// stripHookByUnitSequence 单元序列贪心剥离(结尾锚点缺失时的回退):依次
// 贪心匹配注入头部之后的单元序列,直到既无 XML 块也无引用块/孤儿闭合标签:
//  1. 行首 XML 块 <tag>...</tag>(按名配对闭合,连同块后空白);
//  2. "## Data References" 折叠引用块(新 CLI 在块序列中间插入,内容
//     逐轮变化,实测紧跟上一块的 \n\n 之后);
//  3. 行首 session_* 孤儿闭合标签(引用块插进外层块中间导致,如实测
//     样本引用块后紧跟 </session_guide>)。
//
// 每个单元消费后连同其后空白一起跳过;记录最后一次空白消费是否含 \n\n
// ——含则注入与正文的分隔符已被消费,其后即正文。护栏同锚点路径。
func stripHookByUnitSequence(s string) string {
	rest := s
	if strings.HasPrefix(s, hookCtxMarker) {
		rest = strings.TrimLeft(s[len(hookCtxMarker):], " \t")
		// 模板为 "context: " + 换行 + XML 块序列,只跳一个换行。
		rest = strings.TrimPrefix(rest, "\r")
		rest = strings.TrimPrefix(rest, "\n")
	}
	origLen := len(s)
	pos := 0
	sepConsumed := false
	matched := false
	for pos < len(rest) {
		sub := rest[pos:]
		if m := hookOpenTagRe.FindStringSubmatch(sub); m != nil {
			closeTag := "</" + m[1] + ">"
			ci := strings.Index(sub[len(m[0]):], closeTag)
			if ci < 0 {
				break // 闭合标签缺失,不构成注入块
			}
			matched = true
			pos += len(m[0]) + ci + len(closeTag)
		} else if n := skipHookDataRefBlock(sub); n >= 0 {
			pos += n
		} else if m := hookCloseTagRe.FindStringSubmatch(sub); m != nil {
			pos += len(m[0])
		} else {
			break
		}
		ws := pos
		for pos < len(rest) && isHookSpaceByte(rest[pos]) {
			pos++
		}
		sepConsumed = strings.Contains(rest[ws:pos], "\n\n")
	}
	if !matched {
		return s
	}
	body := rest[pos:]
	if !sepConsumed {
		// XML 序列后是裸文本段(如 output style 提示),以 \n\n 结尾:剥到首个
		// \n\n 之后;裸文本超过 4KB 视为正文,不再剥。
		if idx := strings.Index(body, "\n\n"); idx > 0 && idx <= hookBareTextMax {
			body = body[idx+2:]
		}
	}
	// 护栏:剩余正文过短或剥离总量超过原文 80% 视为误判,返回原文
	// (宁可漏剥不可错剥)。
	if len(body) < hookMinRemaining || origLen-len(body) > origLen*4/5 {
		return s
	}
	return body
}

// stripHookPrefixFromUserText 剥离 Claude Code hook 注入到 user 消息开头的
// 动态前缀。注入有两种开头形态:SessionStart hook 的 "SessionStart hook
// additional context: " + XML 块序列,以及 resume/continue hook 的
// "## Data References" 引用块直接开头;两者都以最后一次 </session_knowledge>
// + 非空裸文本(output style 提示)+ \n\n 结尾。该注入只存在于当轮请求,
// 客户端重放历史时不带此前缀,导致上游前缀缓存在此分叉。剥离后两轮逐字节
// 一致。返回剥离后的文本;无注入特征时原样返回。
func stripHookPrefixFromUserText(s string) string {
	if !strings.HasPrefix(s, hookCtxMarker) && !strings.HasPrefix(s, hookDataRefHead) {
		return s
	}
	if body, anchored := stripHookByTailAnchor(s); anchored {
		return body
	}
	return stripHookByUnitSequence(s)
}

// stripHookPrefixFromLastUser 对 input(Responses 形状)的最后一条 user 消息
// 剥离 SessionStart hook 注入前缀。返回是否有修改。
// 只动最后一条:注入只 prepend 到当轮 user 消息,历史里重放的旧 user 是干净
// 版本,不动可把误伤面降到最小。content 兼容 string 与 parts 两种形状;parts
// 里只剥第一个带 text 的 part(注入只会 prepend 到最前面,部分路径的 part
// type 是 input_text,故按 text 字段识别而非 type)。
func stripHookPrefixFromLastUser(input []any) bool {
	for i := len(input) - 1; i >= 0; i-- {
		item, ok := input[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := item["role"].(string); role != "user" {
			continue
		}
		changed := false
		switch content := item["content"].(type) {
		case string:
			if rest := stripHookPrefixFromUserText(content); rest != content {
				item["content"] = rest
				changed = true
			}
		case []any:
			for _, p := range content {
				pm, ok := p.(map[string]any)
				if !ok {
					continue
				}
				t, ok := pm["text"].(string)
				if !ok || t == "" {
					continue
				}
				if rest := stripHookPrefixFromUserText(t); rest != t {
					pm["text"] = rest
					changed = true
				}
				break // 注入只 prepend 到最前,只剥第一条 text part
			}
		}
		return changed
	}
	return false
}

// instructionsClipMaxEntries 限制稳定 instructions 注册表条数，防止长期运行
// 内存无界增长；超出后按 FIFO 驱逐最旧条目。
const instructionsClipMaxEntries = 1024

var (
	instructionsMu     sync.Mutex
	instructionsStable = map[string]string{}
	instructionsOrder  []string
)

// instructionsRegistryKey 以稳定头（前 stableInstrHeadSegs 段）哈希 + 首条
// user 文本为键：同一会话两者恒定（键稳定），不同会话/不同用户天然不同，
// 不串。同版本客户端的稳定头在多用户间相同，靠首条 user 文本区分。
func instructionsRegistryKey(stableHead, firstUser string) string {
	sum := sha256.Sum256([]byte(stableHead + "\n\x00\n" + firstUser))
	return hex.EncodeToString(sum[:8])
}

// clipInstructionsToStablePrefix 返回本次应发往上游的 instructions（同会话
// 首轮的完整原文，跨轮字节稳定）与新增尾部（本轮追加的段）。上游 Responses
// 序列中 instructions 在最前，Claude Code 每轮在尾部追加 system 段（实测
// 58→122 段）会让前缀缓存从增长点截断——instructions 之后的 tools（~34K
// tokens）与会话历史全部 miss，cached 恒 ≈ instructions tokens（~22% 上限）。
// 把 instructions 钉在首轮原文、增量挪到序列尾部（并入末条 user 消息），
// instructions+tools 前缀即可跨轮全量命中。
// 注册表未命中或前缀不再匹配（会话 system 重写、换会话撞键）时存入新值并
// 原样返回（不裁剪），行为退化为修复前，不丢上下文。
func clipInstructionsToStablePrefix(instr, firstUser string) (stable, delta string) {
	if instr == "" {
		return "", ""
	}
	key := instructionsRegistryKey(stableInstructionsHead(instr), firstUser)
	instructionsMu.Lock()
	defer instructionsMu.Unlock()
	stored, ok := instructionsStable[key]
	if !ok || !strings.HasPrefix(instr, stored) {
		if !ok {
			instructionsOrder = append(instructionsOrder, key)
			if len(instructionsOrder) > instructionsClipMaxEntries {
				delete(instructionsStable, instructionsOrder[0])
				instructionsOrder = instructionsOrder[1:]
			}
		}
		instructionsStable[key] = instr
		return instr, ""
	}
	return stored, strings.TrimPrefix(instr, stored)
}

// appendInstructionsDeltaToLastUser 把 instructions 增量文本并入 input 末条
// user 消息首个 text part（无 user item 或 content 形状异常时追加独立 user
// item）。放序列尾部：前缀缓存只看公共前缀，尾部易变内容不影响
// instructions+tools 段的命中。
func appendInstructionsDeltaToLastUser(input []any, delta string) []any {
	if delta == "" {
		return input
	}
	for i := len(input) - 1; i >= 0; i-- {
		item, ok := input[i].(map[string]any)
		if !ok || item["role"] != "user" {
			continue
		}
		parts, ok := item["content"].([]any)
		if !ok || len(parts) == 0 {
			item["content"] = []any{responsesTextPart("user", delta)}
			return input
		}
		if first, ok := parts[0].(map[string]any); ok {
			if t, _ := first["text"].(string); t != "" {
				first["text"] = delta + "\n\n" + t
				return input
			}
		}
		item["content"] = append([]any{responsesTextPart("user", delta)}, parts...)
		return input
	}
	return append(input, map[string]any{
		"type":    "message",
		"role":    "user",
		"content": []any{responsesTextPart("user", delta)},
	})
}

// firstUserTextOf 提取 input 首条 user 消息的会话标识文本（区分同版本客户端
// 的不同会话）。跳过整段 system-reminder 注入块与 <total_tokens> 计数——
// 它们逐请求漂移,会让注册表键/pck 每轮换值。
func firstUserTextOf(input []any) string {
	for _, it := range input {
		item, ok := it.(map[string]any)
		if !ok || item["role"] != "user" {
			continue
		}
		if parts, ok := item["content"].([]any); ok {
			if t := stableUserTextFromParts(parts); t != "" {
				return t
			}
		} else if s, ok := item["content"].(string); ok && s != "" {
			return strings.TrimSpace(volatileTokenCountRe.ReplaceAllString(s, ""))
		}
	}
	return ""
}

// clipChatSystemStable 把 chat 形状 body 首条 role=system 消息钉在会话首轮
// 原文上。chat 协议把 system 提示 join 进 messages[0],CLI 每轮往尾部追加
// hand-back 通知等增量(剥计数后仍逐轮增长 ~3K,实测 ling 上游前缀缓存从
// system 增长点截断、tools 与历史全 miss,命中率封顶 ~17%)——钉住首轮原文、
// 增量挪到末条 user 消息后,system+tools 前缀跨轮全量命中。注册表与回退行为
// 同 clipInstructionsToStablePrefix(未命中/前缀不匹配时原样返回,不丢上下文)。
func clipChatSystemStable(m map[string]any) {
	msgs, ok := m["messages"].([]any)
	if !ok {
		return
	}
	for _, it := range msgs {
		item, ok := it.(map[string]any)
		if !ok || item["role"] != "system" {
			continue
		}
		sys, ok := item["content"].(string)
		if !ok || sys == "" {
			continue
		}
		stable, delta := clipInstructionsToStablePrefix(sys, firstUserTextOf(msgs))
		item["content"] = stable
		// TrimPrefix 出的 delta 带前导 \n\n,TrimSpace 避免与 user 消息拼接后
		// 逐轮累积空行。
		if delta = strings.TrimSpace(delta); delta != "" {
			m["messages"] = appendChatDeltaToLastUser(msgs, delta)
		}
		break
	}
}

// appendChatDeltaToLastUser 把 system 增量文本并入 chat 形状末条 user 消息:
// 字符串 content 直接前置;parts content 取首个 text part 前置(保持上游可
// 解析的 part 形状);无 user 消息时追加独立 user item。
func appendChatDeltaToLastUser(msgs []any, delta string) []any {
	for i := len(msgs) - 1; i >= 0; i-- {
		item, ok := msgs[i].(map[string]any)
		if !ok || item["role"] != "user" {
			continue
		}
		switch content := item["content"].(type) {
		case string:
			item["content"] = delta + "\n\n" + content
			return msgs
		case []any:
			if len(content) == 0 {
				item["content"] = []any{map[string]any{"type": "text", "text": delta}}
				return msgs
			}
			if first, ok := content[0].(map[string]any); ok {
				if t, _ := first["text"].(string); t != "" {
					first["text"] = delta + "\n\n" + t
					return msgs
				}
			}
			item["content"] = append([]any{map[string]any{"type": "text", "text": delta}}, content...)
			return msgs
		}
	}
	return append(msgs, map[string]any{
		"role":    "user",
		"content": delta,
	})
}
