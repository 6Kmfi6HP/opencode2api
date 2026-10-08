package app

import (
	"strings"

	"github.com/6Kmfi6HP/opencode2api/internal/config"
)

// 机型感知 thinking 能力判定（对齐 Bifrost
// core/providers/anthropic/utils.go 的 Is* / Default* 系列）。全部按裸模型名
// 子串匹配、大小写不敏感，兼容 Bedrock/Vertex/日期后缀形态。网关没有
// models.dev 之外的机型 datasheet 覆盖，这里只用内置子串判定；上游仍负责
// 最终校验。

// isOpus5PlusModel 对齐 Bifrost IsOpus5Plus：Opus 5（及之后 5.x）。匹配
// "opus-5" 以排除 "opus-4-5"，并命中 Bedrock/Vertex/日期后缀形态。
func isOpus5PlusModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "opus-5")
}

// isOpus47PlusModel 对齐 Bifrost IsOpus47Plus：Opus 4.7 及之后（4.7/4.8/5）。
// 这些机型移除了 budget_tokens thinking，且拒绝 temperature/top_p/top_k。
func isOpus47PlusModel(model string) bool {
	m := strings.ToLower(model)
	if !strings.Contains(m, "opus") {
		return false
	}
	return strings.Contains(m, "4-7") || strings.Contains(m, "4.7") ||
		strings.Contains(m, "4-8") || strings.Contains(m, "4.8") ||
		isOpus5PlusModel(m)
}

// isOpus55PlusModel 对齐 Bifrost schemas.IsOpus55Plus：Opus 5.5 起思考永远
// 开启，thinking:{type:"disabled"} 与强制 tool_choice 一律 400。
func isOpus55PlusModel(model string) bool {
	m := strings.ToLower(model)
	if !strings.Contains(m, "opus") {
		return false
	}
	return strings.Contains(m, "5-5") || strings.Contains(m, "5.5")
}

// isSonnet5PlusModel 对齐 Bifrost IsSonnet5Plus：Sonnet 5 起采用 Opus 4.7+
// 的请求面（budget_tokens thinking 移除，adaptive 是唯一 thinking-on 模式）。
// 匹配 "sonnet-5" 排除 "sonnet-4-5"。
func isSonnet5PlusModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "sonnet-5")
}

// isSonnet55PlusModel 对齐 Bifrost IsSonnet55Plus：Sonnet 5.5 起接受
// thinking:{type:"between_tools"} 作为最低思考档（拒绝 "disabled"）。
func isSonnet55PlusModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "sonnet-5-5") || strings.Contains(m, "sonnet-5.5")
}

// isFableFamilyModel 对齐 Bifrost IsFableFamily：Fable/Mythos 系。共享
// Opus 4.7+ 请求面，且额外拒绝 thinking:{type:"disabled"}——思考永远开启，
// 该参数必须整体省略而不是发 disabled。
func isFableFamilyModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "fable") || strings.Contains(m, "mythos")
}

// isMythosPreviewModel 对齐 Bifrost IsMythosPreview：Mythos Preview 是
// Fable/Mythos 系中唯一仍接受 thinking:{type:"enabled",budget_tokens} 的成员，
// 只拒绝 "disabled"。
func isMythosPreviewModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "mythos") && strings.Contains(m, "preview")
}

// adaptiveOnlyThinkingModel 对齐 Bifrost DefaultAdaptiveOnlyThinking：
// budget_tokens thinking 已移除、adaptive 是唯一 thinking-on 模式的机型
// （Opus 4.7+/Sonnet 5+/Fable 系）。
func adaptiveOnlyThinkingModel(model string) bool {
	return isOpus47PlusModel(model) || isSonnet5PlusModel(model) || isFableFamilyModel(model)
}

// supportsAdaptiveThinkingModel 对齐 Bifrost DefaultSupportsAdaptiveThinking：
// thinking.type "adaptive" 被接受的机型（Opus 4.6、Sonnet 4.6、Sonnet 5+、
// Opus 4.7+ 与 Fable/Mythos 系）。
func supportsAdaptiveThinkingModel(model string) bool {
	if adaptiveOnlyThinkingModel(model) {
		return true
	}
	m := strings.ToLower(model)
	if !strings.Contains(m, "4-6") && !strings.Contains(m, "4.6") {
		return false
	}
	return strings.Contains(m, "opus") || strings.Contains(m, "sonnet")
}

// supportsNativeEffortModel 对齐 Bifrost DefaultSupportsNativeEffort：接受
// output_config.effort 参数但不支持 adaptive thinking 的机型（Opus 4.5）。
// 其余接受 effort 的机型都已支持 adaptive，由 adaptive 分支优先命中。
func supportsNativeEffortModel(model string) bool {
	return supportsEffortParameterModel(model) && !supportsAdaptiveThinkingModel(model)
}

// supportsEffortParameterModel 对齐 Bifrost DefaultSupportsNativeEffort 的
// 「接受 effort 参数」半边（Bifrost 以 datasheet 覆盖合并两者，这里只保留
// 子串判定）：Fable/Mythos、Sonnet 5+、Opus 5+、Opus 4.5-4.8、Sonnet 4.6。
// 其余机型（含 Haiku 系）拒绝 effort 参数。
func supportsEffortParameterModel(model string) bool {
	m := strings.ToLower(model)
	if isFableFamilyModel(m) || isSonnet5PlusModel(m) || isOpus5PlusModel(m) {
		return true
	}
	if strings.Contains(m, "haiku") {
		return false
	}
	if strings.Contains(m, "opus") {
		return strings.Contains(m, "4-5") || strings.Contains(m, "4.5") ||
			strings.Contains(m, "4-6") || strings.Contains(m, "4.6") ||
			strings.Contains(m, "4-7") || strings.Contains(m, "4.7") ||
			strings.Contains(m, "4-8") || strings.Contains(m, "4.8")
	}
	if strings.Contains(m, "sonnet") {
		return strings.Contains(m, "4-6") || strings.Contains(m, "4.6")
	}
	return false
}

// supportsBetweenToolsThinkingModel 对齐 Bifrost
// DefaultSupportsBetweenToolsThinking：Sonnet 5.5 接受
// thinking:{type:"between_tools"} 作为最低思考档。
func supportsBetweenToolsThinkingModel(model string) bool {
	return isSonnet55PlusModel(model)
}

// canDisableReasoningModel 对齐 Bifrost DefaultCanDisableReasoning：
// Fable/Mythos、Opus 5.5、Sonnet 5.5 拒绝 thinking:{type:"disabled"}——
// 这些机型的 thinking 参数必须整体省略。
func canDisableReasoningModel(model string) bool {
	return !isFableFamilyModel(model) && !isOpus55PlusModel(model) && !isSonnet55PlusModel(model)
}

// rejectsEnabledThinkingModel 对齐 Bifrost RejectsEnabledThinking：对
// thinking:{type:"enabled"} 400 的机型——adaptive-only 且非 Mythos Preview
// （后者仍接受 enabled）。
func rejectsEnabledThinkingModel(model string) bool {
	return adaptiveOnlyThinkingModel(model) && !isMythosPreviewModel(model)
}

// rejectsDisabledThinkingModel 对齐 Bifrost RejectsDisabledThinking（去
// datasheet 覆盖）：始终在线机型（Fable 系/Opus 5.5/Sonnet 5.5）直接拒绝
// disabled；Opus 5 仅在 effort xhigh/max 时拒绝（effort<=high 时接受）。
// effort 为空（客户端未给 output_config.effort）时按默认档处理——默认档
// 低于 xhigh，disabled 被接受。
func rejectsDisabledThinkingModel(model, effort string) bool {
	if !canDisableReasoningModel(model) {
		return true
	}
	if isOpus5PlusModel(model) {
		switch effort {
		case "xhigh", "max":
			return true
		}
	}
	return false
}

// mapEffortToAnthropicEffort 对齐 Bifrost MapBifrostEffortToAnthropic：
// "minimal" 落 "low"（Anthropic effort 阶梯无 minimal）；"adaptive" 不是
// effort 值（"Don't pass `adaptive` as an `effort` value"），落 "high"
// ——文档语义下等价于省略该参数，思考模式由 thinking 参数本身表达。
func mapEffortToAnthropicEffort(effort string) string {
	switch effort {
	case "minimal":
		return "low"
	case "adaptive":
		return "high"
	}
	return effort
}

// anthropicThinkingForUpstream 按机型构造 chat→anthropic 出站的 thinking
// 对象（对齐 Bifrost core/providers/anthropic/chat.go:771-873）。恒发
// enabled+budget 会被 adaptive-only 机型 400 拒绝——budget_tokens thinking
// 在这些机型上已移除，必须发 adaptive 并把 effort 落到 output_config.effort。
//
// 分支（req.Thinking 是 chat 面透传对象，reasoningEffortFromThinking 把
// budget/effort 折算成 effort 阶梯）：
//   - 请求 thinking.type=between_tools：Sonnet 5.5+ 原样透传；其余机型就近
//     降级——disabled 被接受时发 disabled，始终在线机型（Fable 系/Opus 5.5）
//     省略 thinking。
//   - adaptive-only 机型：adaptive（不带 budget_tokens，上游拒绝该组合），
//     effort 落 output_config.effort；display 默认 "summarized"（这些机型
//     默认省略推理文本，不补 display 客户端看不到思考）。
//   - 其余机型：enabled + budget_tokens（effort 阶梯折算预算）。
//
// 返回 (thinking 对象或 nil, output_config.effort 或 "")；nil 表示省略
// thinking 键。
func anthropicThinkingForUpstream(modelID string, req *OpenAIRequest) (map[string]any, string) {
	if config.ForceDisableThinking() {
		return nil, ""
	}
	thinkingType := ""
	if m, ok := req.Thinking.(map[string]any); ok {
		thinkingType, _ = m["type"].(string)
	}
	effort := req.ReasoningEffort
	if effort == "" {
		effort = reasoningEffortFromThinking(req.Thinking)
	}
	effort = strings.ToLower(strings.TrimSpace(effort))
	effortOn := effort != "" && effort != "none"
	anthropicEffort := ""
	if effortOn {
		anthropicEffort = mapEffortToAnthropicEffort(mappedReasoningEffort(effort))
	}

	// between_tools 请求：按机型就近降级（Bifrost BetweenToolsThinking）。
	if thinkingType == "between_tools" {
		outEffort := ""
		if effortOn && supportsNativeEffortModel(modelID) {
			outEffort = anthropicEffort
		}
		if supportsBetweenToolsThinkingModel(modelID) {
			return map[string]any{"type": "between_tools"}, outEffort
		}
		if !rejectsDisabledThinkingModel(modelID, effort) {
			// "disabled" 被接受：就近降到 disabled。
			return map[string]any{"type": "disabled"}, outEffort
		}
		// 始终在线机型（Fable 系/Opus 5.5/Sonnet 5.5 已在上面命中）：省略
		// thinking，effort 仍可通过 output_config 表达。
		if outEffort != "" {
			return nil, outEffort
		}
		return nil, ""
	}

	// budget/effort 生效：adaptive-only 机型发 adaptive（budget_tokens 已
	// 移除，effort 落 output_config.effort），其余机型发 enabled+budget
	// （Bifrost chat.go:779-803）。
	if effortOn {
		if adaptiveOnlyThinkingModel(modelID) {
			thinking := map[string]any{"type": "adaptive"}
			// Opus 4.7+/Fable 系默认省略推理文本：不补 display 客户端看不到
			// 思考（Bifrost chat.go:869-873）。
			if d := thinkingDisplay(req.Thinking); d != "" {
				thinking["display"] = d
			} else {
				thinking["display"] = "summarized"
			}
			outEffort := ""
			if supportsEffortParameterModel(modelID) {
				outEffort = anthropicEffort
			}
			return thinking, outEffort
		}
		budget := effortToThinkingBudget(mappedReasoningEffort(effort))
		if budget <= 0 {
			// effort 折算不出预算（未知阶梯）：与既有口径一致省略 thinking，
			// 不发没有 budget 的 enabled。
			return nil, ""
		}
		thinking := map[string]any{"type": "enabled", "budget_tokens": budget}
		if d := thinkingDisplay(req.Thinking); d != "" {
			thinking["display"] = d
		}
		return thinking, ""
	}
	return nil, ""
}

// thinkingDisplay 读取 thinking 对象上客户端显式给的 display（"summarized" |
// "omitted"）。非 map 或未给时返回 ""。
func thinkingDisplay(value any) string {
	m, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	d, _ := m["display"].(string)
	return strings.TrimSpace(d)
}
