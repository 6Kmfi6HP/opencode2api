package app

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
)

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

// firstUserTextOf 提取 input 首条 user 消息的首个 text part，作为注册表键的
// 一部分（区分同版本客户端的不同会话）。
func firstUserTextOf(input []any) string {
	for _, it := range input {
		item, ok := it.(map[string]any)
		if !ok || item["role"] != "user" {
			continue
		}
		if parts, ok := item["content"].([]any); ok {
			for _, p := range parts {
				if pm, ok := p.(map[string]any); ok {
					if t, _ := pm["text"].(string); t != "" {
						return t
					}
				}
			}
		}
	}
	return ""
}
