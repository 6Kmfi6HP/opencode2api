package app

import (
	"strings"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/domain"
)

func TestProtocolPatternMatches(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		model   string
		want    bool
	}{
		{"exact match", "claude-sonnet-4.6", "claude-sonnet-4.6", true},
		{"case insensitive", "Claude-Sonnet-4.6", "claude-sonnet-4.6", true},
		{"model case insensitive", "claude-sonnet-4.6", "Claude-Sonnet-4.6", true},
		{"exact mismatch", "claude-sonnet-4.6", "claude-opus-4.6", false},
		{"trailing wildcard prefix", "claude-*", "claude-sonnet-4.6", true},
		{"trailing wildcard no match", "gpt-*", "claude-sonnet-4.6", false},
		{"global wildcard", "*", "anything", true},
		{"global wildcard empty model", "*", "", false},
		{"empty pattern", "", "model", false},
		{"mid wildcard rejected upstream, not matcher's problem", "gpt*x", "gpt-5.6-x", false},
		{"wildcard needs prefix", "*-free", "m-free", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := protocolPatternMatches(tt.pattern, tt.model); got != tt.want {
				t.Fatalf("protocolPatternMatches(%q, %q) = %v, want %v", tt.pattern, tt.model, got, tt.want)
			}
		})
	}
}

func TestNormalizeProtocolPattern(t *testing.T) {
	if got, err := normalizeProtocolPattern("  Claude-* "); err != nil || got != "claude-*" {
		t.Fatalf("normalize = %q, err = %v, want claude-*", got, err)
	}
	for _, p := range []string{"", "  ", "a b", "a\tb", "a**b", "a*b*", "mid*dle"} {
		if _, err := normalizeProtocolPattern(p); err == nil {
			t.Fatalf("normalize(%q) = nil error, want error", p)
		}
	}
	long := strings.Repeat("a", 129)
	if _, err := normalizeProtocolPattern(long); err == nil {
		t.Fatal("128+ char pattern should be rejected")
	}
	if _, err := normalizeProtocolPattern(strings.Repeat("a", 128)); err != nil {
		t.Fatalf("128 char pattern should pass: %v", err)
	}
}

func TestValidateProtocolRules(t *testing.T) {
	valid := []domain.ProtocolRule{
		{Pattern: "claude-*", Protocol: "anthropic"},
		{Pattern: "gpt-*", Protocol: "responses"},
		{Pattern: "glm-5.3", Protocol: "chat_completions"},
	}
	got, err := validateProtocolRules(valid)
	if err != nil {
		t.Fatalf("valid rules rejected: %v", err)
	}
	if len(got) != 3 || got[0].Pattern != "claude-*" {
		t.Fatalf("normalized rules = %#v", got)
	}

	invalid := []struct {
		name  string
		rules []domain.ProtocolRule
	}{
		{"bad protocol", []domain.ProtocolRule{{Pattern: "x", Protocol: "grpc"}}},
		{"empty protocol", []domain.ProtocolRule{{Pattern: "x"}}},
		{"empty pattern", []domain.ProtocolRule{{Protocol: "anthropic"}}},
		{"double star", []domain.ProtocolRule{{Pattern: "a**", Protocol: "anthropic"}}},
		{"mid star", []domain.ProtocolRule{{Pattern: "a*b", Protocol: "anthropic"}}},
		{"whitespace", []domain.ProtocolRule{{Pattern: "a b", Protocol: "anthropic"}}},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := validateProtocolRules(tt.rules); err == nil {
				t.Fatalf("expected error for %v", tt.rules)
			}
		})
	}

	var tooMany []domain.ProtocolRule
	for i := 0; i < maxProtocolRules+1; i++ {
		tooMany = append(tooMany, domain.ProtocolRule{Pattern: "m" + string(rune('a'+i%26)) + string(rune('0'+i/26)), Protocol: "anthropic"})
	}
	if _, err := validateProtocolRules(tooMany); err == nil {
		t.Fatal(">64 rules should be rejected")
	}
}

func TestValidateProtocolRules_ErrorMentionsIndex(t *testing.T) {
	rules := []domain.ProtocolRule{
		{Pattern: "claude-*", Protocol: "anthropic"},
		{Pattern: "gpt-*", Protocol: "nope"},
	}
	_, err := validateProtocolRules(rules)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "protocol_rules[1]") {
		t.Fatalf("error should mention index 1: %v", err)
	}
}

func TestMatchProtocolRule_FirstMatchWinsAndSuffixStrip(t *testing.T) {
	old := getProtocolRules()
	t.Cleanup(func() { setProtocolRules(old) })
	setProtocolRules([]domain.ProtocolRule{
		{Pattern: "claude-*", Protocol: "anthropic"},
		{Pattern: "claude-haiku", Protocol: "chat_completions"},
		{Pattern: "gpt-*", Protocol: "responses"},
	})

	if got, ok := matchProtocolRule("claude-sonnet-4.6"); !ok || got != upstreamProtocolAnthropic {
		t.Fatalf("claude-sonnet-4.6 = %v,%v want anthropic", got, ok)
	}
	// [1m] 上下文后缀剥离后仍命中精确/前缀规则。
	if got, ok := matchProtocolRule("claude-sonnet-4.6[1m]"); !ok || got != upstreamProtocolAnthropic {
		t.Fatalf("claude-sonnet-4.6[1m] = %v,%v want anthropic (suffix stripped)", got, ok)
	}
	if got, ok := matchProtocolRule("gpt-5.6"); !ok || got != upstreamProtocolResponses {
		t.Fatalf("gpt-5.6 = %v,%v want responses", got, ok)
	}
	if _, ok := matchProtocolRule("deepseek-v4-flash"); ok {
		t.Fatal("unmatched model should return ok=false")
	}
}

func TestResolveUpstreamProtocol_Priority(t *testing.T) {
	oldRules := getProtocolRules()
	t.Cleanup(func() { setProtocolRules(oldRules) })
	const probe = "probe-model-xyz"

	// 无规则 + 无记忆 → chat。
	setProtocolRules(nil)
	nativeResponsesModels.Lock()
	delete(nativeResponsesModels.ids, probe)
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, probe)
		nativeResponsesModels.Unlock()
	})
	if got := resolveUpstreamProtocol(probe); got != upstreamProtocolChat {
		t.Fatalf("no rule no memory = %v, want chat", got)
	}

	// 无规则 + 有记忆 → responses。
	nativeResponsesModels.Lock()
	nativeResponsesModels.ids[probe] = true
	nativeResponsesModels.Unlock()
	if got := resolveUpstreamProtocol(probe); got != upstreamProtocolResponses {
		t.Fatalf("memory hit = %v, want responses", got)
	}

	// 规则 > 记忆。
	setProtocolRules([]domain.ProtocolRule{{Pattern: "probe-*", Protocol: "anthropic"}})
	if got := resolveUpstreamProtocol(probe); got != upstreamProtocolAnthropic {
		t.Fatalf("rule overrides memory = %v, want anthropic", got)
	}
}

func TestCompileProtocolRulesLenient(t *testing.T) {
	rules := []domain.ProtocolRule{
		{Pattern: "claude-*", Protocol: "anthropic"},
		{Pattern: "bad pattern with space", Protocol: "anthropic"},
		{Pattern: "gpt-*", Protocol: "responses"},
		{Pattern: "x", Protocol: "bogus"},
	}
	got := compileProtocolRulesLenient(rules)
	if len(got) != 2 {
		t.Fatalf("lenient compile kept %d rules, want 2: %#v", len(got), got)
	}
	if got[0].Pattern != "claude-*" || got[1].Pattern != "gpt-*" {
		t.Fatalf("kept wrong rules: %#v", got)
	}
}

func TestApplyConfig_ProtocolRules(t *testing.T) {
	old := getProtocolRules()
	t.Cleanup(func() {
		applyConfig(AppConfig{})
		setProtocolRules(old)
	})

	applyConfig(AppConfig{ProtocolRules: []domain.ProtocolRule{
		{Pattern: "Claude-*", Protocol: "anthropic"},
		{Pattern: "bad pattern", Protocol: "bogus"},
	}})
	rules := getProtocolRules()
	if len(rules) != 1 || rules[0].Pattern != "claude-*" || rules[0].Protocol != "anthropic" {
		t.Fatalf("applyConfig normalized rules = %#v", rules)
	}
	if got, ok := matchProtocolRule("claude-haiku-4.6"); !ok || got != upstreamProtocolAnthropic {
		t.Fatalf("match after applyConfig = %v,%v", got, ok)
	}

	// nil 不清空（POST 未携带字段时保持原值）。
	applyConfig(AppConfig{})
	if len(getProtocolRules()) != 1 {
		t.Fatalf("nil ProtocolRules should keep rules, got %#v", getProtocolRules())
	}
}
