package app

import (
	"log/slog"

	"github.com/6Kmfi6HP/opencode2api/internal/bridge"
	"github.com/6Kmfi6HP/opencode2api/internal/config"
)

// defaultClaudeMaxTokens 是 Anthropic Messages schema 对必填 max_tokens 的
// 兜底值（对齐 sub2api 的 chatcompletions_anthropic_bridge）。客户端省略时
// 补 8192，避免上游原生 Anthropic 端点按 schema 拒绝。常量定义已迁入 bridge；
// 此处为同包别名，保持 app 内引用与测试不变。
const defaultClaudeMaxTokens = bridge.DefaultClaudeMaxTokens

// minClaudeMaxTokens 是 thinking 模式下 max_tokens 的最小值。常量定义已迁入
// bridge；此处为同包别名。
const minClaudeMaxTokens = bridge.MinClaudeMaxTokens

// clampMaxTokens 把 max_tokens 收敛到 [1, cap]；cap<=0 表示无上界。
// convertRequest（chat.go）的 cap 逻辑与 clampClaudeMaxTokens 共用这个
// helper。薄壳转发到 bridge。
func clampMaxTokens(v, cap int) int {
	return bridge.ClampMaxTokens(v, cap)
}

// clampClaudeMaxTokens 是 Anthropic 方向的 max_tokens 收敛：schema 上 128
// 是 thinking 兼容的安全下限，先 clampMaxTokens 收 cap 再兜底 128。
// 薄壳转发到 bridge。
func clampClaudeMaxTokens(v, cap int) int {
	return bridge.ClampClaudeMaxTokens(v, cap)
}

// clampAnthropicProtocolMaxTokens 是直通路径（raw body map）版本的
// clampClaudeMaxTokens：对 bodyMap["max_tokens"]（JSON 数值）做 [128, cap]
// 就地收敛；缺失/非法时不改（适合 count_tokens 等不强求 max_tokens 的入口
// —— 只降不补，"最多保持原预算"）。messages 直通的 required-schema 补默认
// 逻辑在 forwardClaudeViaAnthropic。返回写回后的 int 值；未改返回 0。
// 纯核已迁入 bridge.ClampAnthropicProtocolMaxTokens；本壳按 modelID 解析 cap 注入。
func clampAnthropicProtocolMaxTokens(bodyMap map[string]any, modelID string) int {
	return bridge.ClampAnthropicProtocolMaxTokens(bodyMap, config.MaxTokensCapFor(modelID))
}

// convertClaudeRequest is the request-side protocol boundary. It returns a new
// Chat Completions request and never mutates values owned by the caller.
// skippedServerTools lists Anthropic server-tool names that were not forwarded.
// 纯核已迁入 bridge.ConvertClaudeRequest；本壳注入 force-disable-thinking 与
// max_tokens cap，并把剥离的采样参数计数还原为原 debug 日志。
func convertClaudeRequest(req ClaudeRequest) (OpenAIRequest, []string) {
	out, skipped, stripped := bridge.ConvertClaudeRequest(config.ForceDisableThinking(), config.MaxTokensCapFor(req.Model), req)
	if stripped > 0 {
		slog.Debug("claude thinking enabled: stripped sampling params", "model", out.Model, "count", stripped)
	}
	return out, skipped
}

// claudeToolChoiceCore 把 Claude tool_choice 转为 OpenAI 风格 tool_choice。
// chatShape 为 true 时产 Chat Completions 形状（name 嵌套于 function 键），
// 为 false 时产 Responses 形状（name 平铺）。未知形状原样透传，不报错。
// 薄壳转发到 bridge。
func claudeToolChoiceCore(choice any, chatShape bool) any {
	return bridge.ClaudeToolChoiceCore(choice, chatShape)
}

// convertClaudeToolChoice 把 Claude tool_choice 转为 Chat Completions 形状。
// 薄壳转发到 bridge。
func convertClaudeToolChoice(choice any) any {
	return bridge.ConvertClaudeToolChoice(choice)
}

// claudeToolChoiceDisablesParallel 报告 Claude tool_choice 是否要求关闭并行
// 工具调用。薄壳转发到 bridge。
func claudeToolChoiceDisablesParallel(choice any) bool {
	return bridge.ClaudeToolChoiceDisablesParallel(choice)
}

// narrowClaudeMetadataUser extracts a safe upstream user id from Claude metadata.
// Claude Code packs device_id into user_id JSON; only session_id is forwarded.
// 薄壳转发到 bridge。
func narrowClaudeMetadataUser(metadata any) string {
	return bridge.NarrowClaudeMetadataUser(metadata)
}
