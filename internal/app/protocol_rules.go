package app

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/6Kmfi6HP/opencode2api/internal/domain"
)

// ======================== 上游协议路由规则 ========================
//
// 按"模型模式 → 上游原生协议"把请求分流到 OpenCode Zen 的三种原生端点：
//   - chat_completions（默认兜底）：/zen/v1/chat/completions
//   - anthropic：/zen/v1/messages（或 /zen/go/v1/messages）
//   - responses：/zen/v1/responses
//
// 规则语法对齐 sub2api 的 OpenCodeGoProtocolRule：精确 ID 或单尾 "*" 通配，
// 大小写不敏感，first match wins，未命中兜底 chat_completions。

type upstreamProtocol string

const (
	upstreamProtocolChat      upstreamProtocol = "chat_completions"
	upstreamProtocolAnthropic upstreamProtocol = "anthropic"
	upstreamProtocolResponses upstreamProtocol = "responses"
)

const (
	maxProtocolRules         = 64
	maxProtocolPatternLength = 128
)

// upstreamProtocolSubpath 返回协议对应的 callOpenCodeEndpoint subpath。
func (p upstreamProtocol) subpath() string {
	switch p {
	case upstreamProtocolAnthropic:
		return "messages"
	case upstreamProtocolResponses:
		return "responses"
	default:
		return "chat/completions"
	}
}

func isValidUpstreamProtocol(p string) bool {
	switch upstreamProtocol(strings.TrimSpace(p)) {
	case upstreamProtocolChat, upstreamProtocolAnthropic, upstreamProtocolResponses:
		return true
	default:
		return false
	}
}

// normalizeProtocolPattern 校验并规范化单个模式：小写、非空、≤128 字符、
// 不含空白、至多一个 "*" 且必须在尾部（对齐参考实现）。
func normalizeProtocolPattern(pattern string) (string, error) {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	if len(pattern) > maxProtocolPatternLength {
		return "", fmt.Errorf("pattern is too long")
	}
	if strings.ContainsAny(pattern, " \t") {
		return "", fmt.Errorf("pattern must not contain whitespace")
	}
	star := strings.Count(pattern, "*")
	if star > 1 || (star == 1 && !strings.HasSuffix(pattern, "*")) {
		return "", fmt.Errorf("pattern may use a single trailing * wildcard")
	}
	return pattern, nil
}

// validateProtocolRules 严格校验整张规则表（管理面板 POST 用）：任一条非法
// 即返回带下标的错误，不做部分采纳。
func validateProtocolRules(rules []domain.ProtocolRule) ([]domain.ProtocolRule, error) {
	if len(rules) > maxProtocolRules {
		return nil, fmt.Errorf("protocol_rules supports at most %d entries", maxProtocolRules)
	}
	clean := make([]domain.ProtocolRule, 0, len(rules))
	for i, rule := range rules {
		pattern, err := normalizeProtocolPattern(rule.Pattern)
		if err != nil {
			return nil, fmt.Errorf("protocol_rules[%d]: %w", i, err)
		}
		protocol := strings.TrimSpace(rule.Protocol)
		if !isValidUpstreamProtocol(protocol) {
			return nil, fmt.Errorf("protocol_rules[%d]: protocol must be chat_completions, anthropic, or responses", i)
		}
		clean = append(clean, domain.ProtocolRule{Pattern: pattern, Protocol: protocol})
	}
	return clean, nil
}

// compileProtocolRulesLenient 宽松编译整张规则表（config.json 加载用）：
// 非法条目剔除并告警，不阻断启动（对齐 compileKeywordRules 风格）。
func compileProtocolRulesLenient(rules []domain.ProtocolRule) []domain.ProtocolRule {
	clean := make([]domain.ProtocolRule, 0, len(rules))
	for _, rule := range rules {
		pattern, err := normalizeProtocolPattern(rule.Pattern)
		if err != nil {
			slog.Warn("invalid protocol rule skipped", "pattern", rule.Pattern, "error", err)
			continue
		}
		protocol := strings.TrimSpace(rule.Protocol)
		if !isValidUpstreamProtocol(protocol) {
			slog.Warn("invalid protocol rule skipped", "pattern", rule.Pattern, "protocol", rule.Protocol)
			continue
		}
		clean = append(clean, domain.ProtocolRule{Pattern: pattern, Protocol: protocol})
	}
	return clean
}

// protocolRules 存当前生效规则表；与 modelAliasRules 共用 configMu。
var protocolRules []domain.ProtocolRule

func setProtocolRules(rules []domain.ProtocolRule) {
	cp := make([]domain.ProtocolRule, len(rules))
	copy(cp, rules)
	configMu.Lock()
	protocolRules = cp
	configMu.Unlock()
}

func getProtocolRules() []domain.ProtocolRule {
	configMu.RLock()
	defer configMu.RUnlock()
	cp := make([]domain.ProtocolRule, len(protocolRules))
	copy(cp, protocolRules)
	return cp
}

// protocolPatternMatches 报告模式是否命中模型 ID：双方小写比较；"*" 全匹配；
// 尾 "*" 前缀匹配；否则精确相等。
func protocolPatternMatches(pattern, modelID string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if pattern == "" || modelID == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(modelID, strings.TrimSuffix(pattern, "*"))
	}
	return modelID == pattern
}

// matchProtocolRule 按声明顺序匹配规则表；未命中返回 ("", false)。
// 匹配对象是 stripContextSuffix 之后的基名，使 "claude-sonnet-4.6" 精确规则
// 同时命中 "claude-sonnet-4.6[1m]"。
func matchProtocolRule(modelID string) (upstreamProtocol, bool) {
	base, _ := stripContextSuffix(modelID)
	rules := getProtocolRules()
	for _, rule := range rules {
		if protocolPatternMatches(rule.Pattern, base) {
			return upstreamProtocol(strings.TrimSpace(rule.Protocol)), true
		}
	}
	return "", false
}

// resolveUpstreamProtocol 解析最终上游协议：显式规则 > native-responses 运行时
// 记忆 > 默认 chat。用于 claude/responses 入站（这两处今天已有记忆入口分派）。
func resolveUpstreamProtocol(modelID string) upstreamProtocol {
	if proto, matched := matchProtocolRule(modelID); matched {
		return proto
	}
	if isNativeResponsesModel(modelID) {
		return upstreamProtocolResponses
	}
	return upstreamProtocolChat
}

// matchProtocolRuleModelOnly 是 matchProtocolRule 的单值形态，供 switch 分派
// 使用：规则未命中返回 zero 值 chat（与兜底语义一致）。
func matchProtocolRuleModelOnly(modelID string) upstreamProtocol {
	proto, _ := matchProtocolRule(modelID)
	return proto
}
