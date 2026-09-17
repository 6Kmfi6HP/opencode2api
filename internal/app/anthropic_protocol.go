package app

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/6Kmfi6HP/opencode2api/internal/config"
)

// defaultClaudeMaxTokens 是 Anthropic Messages schema 对必填 max_tokens 的
// 兜底值（对齐 sub2api 的 chatcompletions_anthropic_bridge）。客户端省略时
// 补 8192，避免上游原生 Anthropic 端点按 schema 拒绝。
const defaultClaudeMaxTokens = 8192

// minClaudeMaxTokens 是 thinking 模式下 max_tokens 的最小值（Anthropic 要求
// 不低于 budget_tokens 下限 1024 的场景已由上游校验，这里只挡住非法小值）。
const minClaudeMaxTokens = 128

// clampMaxTokens 把 max_tokens 收敛到 [1, cap]；cap<=0 表示无上界。
// convertRequest（chat.go）与 claudeToResponsesBody（claude_responses.go）
// 的 cap 逻辑共用这个 helper。
func clampMaxTokens(v, cap int) int {
	if cap > 0 && v > cap {
		v = cap
	}
	if v < 1 {
		v = 1
	}
	return v
}

// clampClaudeMaxTokens 是 Anthropic 方向的 max_tokens 收敛：schema 上 128
// 是 thinking 兼容的安全下限，先 clampMaxTokens 收 cap 再兜底 128。
func clampClaudeMaxTokens(v, cap int) int {
	if v = clampMaxTokens(v, cap); v < minClaudeMaxTokens {
		v = minClaudeMaxTokens
	}
	return v
}

// clampAnthropicProtocolMaxTokens 是直通路径（raw body map）版本的
// clampMaxTokens：对 bodyMap["max_tokens"]（JSON 数值）做 [128, cap] 就地
// 收敛；缺失/非法时不改（适合 count_tokens 等不强求 max_tokens 的入口 —
// 只降不补，"最多保持原预算"）。messages 直通的 required-schema 补默认
// 逻辑在 forwardClaudeViaAnthropic。返回写回后的 int 值；未改返回 0。
func clampAnthropicProtocolMaxTokens(bodyMap map[string]any, modelID string) int {
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
	clamped := clampMaxTokens(v, config.MaxTokensCapFor(modelID))
	if clamped != v {
		bodyMap["max_tokens"] = clamped
	}
	return clamped
}

// convertClaudeRequest is the request-side protocol boundary. It returns a new
// Chat Completions request and never mutates values owned by the caller.
// skippedServerTools lists Anthropic server-tool names that were not forwarded.
func convertClaudeRequest(req ClaudeRequest) (OpenAIRequest, []string) {
	tools, skipped := claudeToOpenAITools(req.Tools)
	out := OpenAIRequest{
		Model: req.Model, Messages: claudeToOpenAIMessages(req.Messages, req.System),
		Stream: req.Stream, Temperature: req.Temperature, MaxTokens: req.MaxTokens,
		TopP: req.TopP, Tools: tools,
		ToolChoice: convertClaudeToolChoice(req.ToolChoice),
		Thinking:   req.Thinking,
	}
	// Anthropic schema 要求 max_tokens 必填：客户端省略时按 sub2api 补
	// 8192，再按配置 cap 与 thinking 下限 128 收敛。
	if out.MaxTokens == nil {
		v := clampClaudeMaxTokens(defaultClaudeMaxTokens, config.MaxTokensCapFor(out.Model))
		out.MaxTokens = &v
	} else {
		v := clampClaudeMaxTokens(*out.MaxTokens, config.MaxTokensCapFor(out.Model))
		out.MaxTokens = &v
	}
	// Claude Code puts effort in output_config.effort (--effort / CLAUDE_CODE_EFFORT_LEVEL).
	// Map it onto Chat Completions reasoning_effort so upstream mapping still applies.
	if effort := effortFromOutputConfig(req.OutputConfig); effort != "" {
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
	thinkingOn := !config.ForceDisableThinking() &&
		(isThinkingEnabled(out.Thinking) || out.ReasoningEffort != "")
	if thinkingOn {
		stripped := 0
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
		if stripped > 0 {
			slog.Debug("claude thinking enabled: stripped sampling params", "model", out.Model, "count", stripped)
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
	if claudeToolChoiceDisablesParallel(req.ToolChoice) {
		if out.ExtraBody == nil {
			out.ExtraBody = map[string]any{}
		}
		out.ExtraBody["parallel_tool_calls"] = false
	}
	if user := narrowClaudeMetadataUser(req.Metadata); user != "" {
		if out.ExtraBody == nil {
			out.ExtraBody = map[string]any{}
		}
		out.ExtraBody["user"] = user
	}
	return out, skipped
}

// claudeToolChoiceCore 把 Claude tool_choice 转为 OpenAI 风格 tool_choice。
// chatShape 为 true 时产 Chat Completions 形状（name 嵌套于 function 键），
// 为 false 时产 Responses 形状（name 平铺）。未知形状原样透传，不报错。
func claudeToolChoiceCore(choice any, chatShape bool) any {
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

func convertClaudeToolChoice(choice any) any {
	return claudeToolChoiceCore(choice, true)
}

func claudeToolChoiceDisablesParallel(choice any) bool {
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

// narrowClaudeMetadataUser extracts a safe upstream user id from Claude metadata.
// Claude Code packs device_id into user_id JSON; only session_id is forwarded.
func narrowClaudeMetadataUser(metadata any) string {
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
