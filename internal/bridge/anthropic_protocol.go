package bridge

import (
	"encoding/json"
	"strings"
)

// This file holds the pure Anthropic-protocol request-boundary logic moved out
// of internal/app/anthropic_protocol.go (and the pure Claude -> OpenAI message/
// tool converters it depends on, moved out of internal/app/claude.go). None of
// these functions perform I/O, logging, metrics, or config reads; the max_tokens
// cap arrives as a value parameter. The app layer keeps same-name lowercase
// forwarding shells so existing *_test.go files compile unchanged.

// DefaultClaudeMaxTokens 是 Anthropic Messages schema 对必填 max_tokens 的
// 兜底值（对齐 sub2api 的 chatcompletions_anthropic_bridge）。客户端省略时
// 补 8192，避免上游原生 Anthropic 端点按 schema 拒绝。
const DefaultClaudeMaxTokens = 8192

// MinClaudeMaxTokens 是 thinking 模式下 max_tokens 的最小值（Anthropic 要求
// 不低于 budget_tokens 下限 1024 的场景已由上游校验，这里只挡住非法小值）。
const MinClaudeMaxTokens = 128

// ClampMaxTokens 把 max_tokens 收敛到 [1, cap]；cap<=0 表示无上界。
// convertRequest 的 cap 逻辑与 ClampClaudeMaxTokens 共用这个 helper。
// (Moved from app/anthropic_protocol.go clampMaxTokens.)
func ClampMaxTokens(v, cap int) int {
	if cap > 0 && v > cap {
		v = cap
	}
	if v < 1 {
		v = 1
	}
	return v
}

// ClampClaudeMaxTokens 是 Anthropic 方向的 max_tokens 收敛：schema 上 128
// 是 thinking 兼容的安全下限，先 ClampMaxTokens 收 cap 再兜底 128。
// (Moved from app/anthropic_protocol.go clampClaudeMaxTokens.)
func ClampClaudeMaxTokens(v, cap int) int {
	if v = ClampMaxTokens(v, cap); v < MinClaudeMaxTokens {
		v = MinClaudeMaxTokens
	}
	return v
}

// ClampAnthropicProtocolMaxTokens 是直通路径（raw body map）版本的
// ClampClaudeMaxTokens：对 bodyMap["max_tokens"]（JSON 数值）做 [128, cap]
// 就地收敛；缺失/非法时不改（适合 count_tokens 等不强求 max_tokens 的入口
// —— 只降不补，"最多保持原预算"）。messages 直通的 required-schema 补默认
// 逻辑在 forwardClaudeViaAnthropic。返回写回后的 int 值；未改返回 0。
// cap 由调用方按 modelID 解析（config.MaxTokensCapFor）后注入。
// (Moved from app/anthropic_protocol.go clampAnthropicProtocolMaxTokens.)
func ClampAnthropicProtocolMaxTokens(bodyMap map[string]any, cap int) int {
	if bodyMap == nil {
		return 0
	}
	v := 0
	switch raw := bodyMap["max_tokens"].(type) {
	case float64:
		v = int(raw)
	case int:
		v = raw
	case int64:
		v = int(raw)
	case json.Number:
		if n, err := raw.Int64(); err == nil {
			v = int(n)
		}
	default:
		return 0
	}
	if v <= 0 {
		return 0
	}
	clamped := ClampClaudeMaxTokens(v, cap)
	if clamped != v {
		bodyMap["max_tokens"] = clamped
	}
	return clamped
}

// ConvertClaudeRequest is the request-side protocol boundary. It returns a new
// Chat Completions request and never mutates values owned by the caller.
// skippedServerTools lists Anthropic server-tool names that were not forwarded.
// maxTokensCap 与 forceDisableThinking 由调用方按 config 解析后注入；thinking
// 与采样参数互斥剥离在本层做（stripped>0 时 strippedCount 返回剥离条数，
// 由 app 壳记 debug 日志）。
// (Moved from app/anthropic_protocol.go convertClaudeRequest.)
func ConvertClaudeRequest(forceDisableThinking bool, maxTokensCap int, req ClaudeRequest) (OpenAIRequest, []string, int) {
	tools, skipped := ClaudeToOpenAITools(req.Tools)
	out := OpenAIRequest{
		Model: req.Model, Messages: ClaudeToOpenAIMessages(req.Messages, req.System),
		Stream: req.Stream, Temperature: req.Temperature, MaxTokens: req.MaxTokens,
		TopP: req.TopP, Tools: tools,
		ToolChoice: ConvertClaudeToolChoice(req.ToolChoice),
		Thinking:   req.Thinking,
	}
	// Anthropic schema 要求 max_tokens 必填：客户端省略时按 sub2api 补
	// 8192，再按配置 cap 与 thinking 下限 128 收敛。
	if out.MaxTokens == nil {
		v := ClampClaudeMaxTokens(DefaultClaudeMaxTokens, maxTokensCap)
		out.MaxTokens = &v
	} else {
		v := ClampClaudeMaxTokens(*out.MaxTokens, maxTokensCap)
		out.MaxTokens = &v
	}
	// Claude Code puts effort in output_config.effort (--effort / CLAUDE_CODE_EFFORT_LEVEL).
	// Map it onto Chat Completions reasoning_effort so upstream mapping still applies.
	if effort := EffortFromOutputConfig(req.OutputConfig); effort != "" {
		out.ReasoningEffort = effort
	}
	// Normalize adaptive thinking to an enabled object so budget/effort fields survive.
	if m, ok := req.Thinking.(map[string]any); ok {
		if t, _ := m["type"].(string); t == "adaptive" {
			normalized := map[string]any{"type": "enabled"}
			for _, key := range []string{"budget_tokens", "effort"} {
				if v, exists := m[key]; exists && v != nil {
					normalized[key] = v
				}
			}
			if effort := out.ReasoningEffort; effort != "" {
				if _, exists := normalized["effort"]; !exists {
					normalized["effort"] = effort
				}
			}
			out.Thinking = normalized
		}
	}
	// thinking 与采样参数互斥（对齐 sub2api）：上游 Anthropic thinking 模式
	// 与 gpt-5 族 responses 都不接受 temperature/top_p/top_k，开启 thinking
	// 时剥离，避免上游 400。effort 经 output_config 或
	// thinking.effort/budget 推导出来也算 thinking 生效；
	// force_disable_thinking 时上游链路强制 thinking off，采样参数保留。
	// req.TopK 是本地副本，剥离后置 nil，下方 top_k 透传分支据此跳过。
	thinkingOn := !forceDisableThinking &&
		(IsThinkingEnabled(out.Thinking) || out.ReasoningEffort != "")
	stripped := 0
	if thinkingOn {
		if out.Temperature != nil {
			out.Temperature = nil
			stripped++
		}
		if out.TopP != nil {
			out.TopP = nil
			stripped++
		}
		if req.TopK != nil {
			stripped++
			req.TopK = nil
		}
	}
	if req.TopK != nil {
		if out.ExtraBody == nil {
			out.ExtraBody = map[string]any{}
		}
		out.ExtraBody["top_k"] = *req.TopK
	}
	if len(req.StopSequences) > 0 {
		if out.ExtraBody == nil {
			out.ExtraBody = map[string]any{}
		}
		out.ExtraBody["stop"] = append([]string(nil), req.StopSequences...)
	}
	if ClaudeToolChoiceDisablesParallel(req.ToolChoice) {
		if out.ExtraBody == nil {
			out.ExtraBody = map[string]any{}
		}
		out.ExtraBody["parallel_tool_calls"] = false
	}
	if user := NarrowClaudeMetadataUser(req.Metadata); user != "" {
		if out.ExtraBody == nil {
			out.ExtraBody = map[string]any{}
		}
		out.ExtraBody["user"] = user
	}
	return out, skipped, stripped
}

// ClaudeToolChoiceCore 把 Claude tool_choice 转为 OpenAI 风格 tool_choice。
// chatShape 为 true 时产 Chat Completions 形状（name 嵌套于 function 键），
// 为 false 时产 Responses 形状（name 平铺）。未知形状原样透传，不报错。
// (Moved from app/anthropic_protocol.go claudeToolChoiceCore.)
func ClaudeToolChoiceCore(choice any, chatShape bool) any {
	m, ok := choice.(map[string]any)
	if !ok {
		return choice
	}
	switch m["type"] {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if name, ok := m["name"].(string); ok && name != "" {
			if chatShape {
				return map[string]any{"type": "function", "function": map[string]any{"name": name}}
			}
			return map[string]any{"type": "function", "name": name}
		}
		if !chatShape {
			return "auto"
		}
	}
	return choice
}

// ConvertClaudeToolChoice 把 Claude tool_choice 转为 Chat Completions 形状。
// (Moved from app/anthropic_protocol.go convertClaudeToolChoice.)
func ConvertClaudeToolChoice(choice any) any {
	return ClaudeToolChoiceCore(choice, true)
}

// ClaudeToolChoiceDisablesParallel 报告 Claude tool_choice 是否要求关闭并行
// 工具调用（disable_parallel_tool_use=true）。
// (Moved from app/anthropic_protocol.go claudeToolChoiceDisablesParallel.)
func ClaudeToolChoiceDisablesParallel(choice any) bool {
	m, ok := choice.(map[string]any)
	if !ok {
		return false
	}
	v, ok := m["disable_parallel_tool_use"]
	if !ok {
		return false
	}
	switch b := v.(type) {
	case bool:
		return b
	default:
		return false
	}
}

// NarrowClaudeMetadataUser extracts a safe upstream user id from Claude metadata.
// Claude Code packs device_id into user_id JSON; only session_id is forwarded.
// (Moved from app/anthropic_protocol.go narrowClaudeMetadataUser.)
func NarrowClaudeMetadataUser(metadata any) string {
	m, ok := metadata.(map[string]any)
	if !ok {
		return ""
	}
	user, ok := m["user_id"].(string)
	if !ok || user == "" {
		return ""
	}
	user = strings.TrimSpace(user)
	if strings.HasPrefix(user, "{") {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(user), &parsed); err == nil {
			if sid, ok := parsed["session_id"].(string); ok && sid != "" {
				return sid
			}
			return ""
		}
	}
	return user
}
