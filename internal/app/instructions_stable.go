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
// 三种形状)剥离 <total_tokens> 计数块,返回是否有改动。
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
	}
	if msgs, ok := m["messages"].([]any); ok {
		if stripVolatileTokenCountersInPlace(msgs) {
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
