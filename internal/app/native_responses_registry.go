package app

// ======================== 原生 responses 上游注册表（探测 + 记忆） ========================
//
// 有些模型的上游 /zen/v1/chat/completions 通道不可用，但原生
// /zen/v1/responses 端点正常（如 muse-spark-1.3-contributor）。
//
// /v1/responses 入站只走翻译路径（原生透传已移除）；本注册表供 claude/chat
// 入站在 chat 翻译失败后回退探测上游原生 responses 端点（翻译转发，见
// claude_responses.go / chat_to_responses_upstream.go）。探测成功后在内存中
// 记住该模型（key 为解析后的上游模型 ID），下次直接走 responses 上游、不再
// 先撞 chat 失败。记忆只存内存，进程重启后失效（重新探测即可，静态预置模型
// 除外）。
//
// 职责分离：
//   - 模型注册表（is/remember/mark + 静态预置 + 故障剔除）：只管“走哪条路”；
//   - shouldProbeNativeResponses：chat 翻译失败后是否值得回退探测的策略判定。

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
)

var nativeResponsesModels = struct {
	sync.RWMutex
	ids map[string]bool
}{ids: map[string]bool{}}

// defaultNativeResponsesModels 静态预置已知仅支持原生 responses 端点的模型，
// 免除冷启动时“先失败重试多次再探测”的惩罚。静态模型不会被故障剔除。
//
// key 用 "muse-spark-*-contributor" 通配：上游（opencode console 仓
// zen/util/handler.go）把 contributor 系列的 chat/completions 通道整档
// 500（与指纹无关，四件齐+stream:true 仍 500），但 /zen/v1/responses 正常；
// 付费档与 -free 档同机型同通道策略，故统一走原生 responses。
var defaultNativeResponsesModels = map[string]bool{
	"muse-spark-1.2-contributor":      true,
	"muse-spark-1.3-contributor":      true,
	"muse-spark-1.2-contributor-free": true,
	"muse-spark-1.3-contributor-free": true,
}
var defaultNativeResponsesPatterns = []string{"muse-spark-*-contributor", "muse-spark-*-contributor-free"}

// nativeResponsesFailures 记录动态记住模型的连续透传失败次数，用于故障自愈。
// 只有动态学习到的模型会被剔除；静态预置模型与配置下发的模型不受影响。
var nativeResponsesFailures = struct {
	sync.Mutex
	counts map[string]int
}{counts: map[string]int{}}

// nativeResponsesEvictAfter 动态模型连续透传失败达到该阈值时自动剔除记忆。
const nativeResponsesEvictAfter = 5

func init() {
	ensureNativeResponsesDefaults()
}

// ensureNativeResponsesDefaults 把静态预置模型载入记忆（幂等，可重复调用）。
func ensureNativeResponsesDefaults() {
	nativeResponsesModels.Lock()
	defer nativeResponsesModels.Unlock()
	for m := range defaultNativeResponsesModels {
		nativeResponsesModels.ids[m] = true
	}
}

// setNativeResponsesModels 把配置下发的原生模型合并进记忆（只增不减，避免
// 配置热加载冲掉运行时动态学习到的模型）。
func setNativeResponsesModels(models []string) {
	ensureNativeResponsesDefaults()
	if len(models) == 0 {
		return
	}
	nativeResponsesModels.Lock()
	defer nativeResponsesModels.Unlock()
	for _, m := range models {
		if m != "" {
			nativeResponsesModels.ids[m] = true
		}
	}
}

// isNativeResponsesModel 报告该上游模型是否已被记住走原生 responses 透传。
// 除逐个记住的模型 ID 外，还命中静态预置通配（如 muse-spark-*-contributor
// 全系列：上游对 contributor 机型整档封 chat/completions，见上方注释）。
func isNativeResponsesModel(modelID string) bool {
	if modelID == "" {
		return false
	}
	base, _ := stripContextSuffix(modelID)
	for _, pattern := range defaultNativeResponsesPatterns {
		if protocolPatternMatches(pattern, base) {
			return true
		}
	}
	nativeResponsesModels.RLock()
	defer nativeResponsesModels.RUnlock()
	return nativeResponsesModels.ids[modelID] || nativeResponsesModels.ids[base]
}

// rememberNativeResponsesModel 记住该上游模型走原生 responses 透传。
func rememberNativeResponsesModel(modelID string) {
	if modelID == "" {
		return
	}
	nativeResponsesModels.Lock()
	defer nativeResponsesModels.Unlock()
	if !nativeResponsesModels.ids[modelID] {
		nativeResponsesModels.ids[modelID] = true
		slog.Info("responses passthrough remembered", "model", modelID)
	}
	nativeResponsesFailures.Lock()
	delete(nativeResponsesFailures.counts, modelID)
	nativeResponsesFailures.Unlock()
}

// markNativeResponsesFailure 记录已记住模型的透传失败；动态模型连续失败达到
// 阈值后自动从记忆中剔除（故障自愈），下次回落到常规 chat 翻译路径。
func markNativeResponsesFailure(modelID string) {
	if modelID == "" {
		return
	}
	if defaultNativeResponsesModels[modelID] {
		return
	}
	if base, _ := stripContextSuffix(modelID); defaultNativeResponsesModels[base] {
		return
	}
	for _, pattern := range defaultNativeResponsesPatterns {
		if base, _ := stripContextSuffix(modelID); protocolPatternMatches(pattern, base) {
			return
		}
	}
	nativeResponsesFailures.Lock()
	nativeResponsesFailures.counts[modelID]++
	n := nativeResponsesFailures.counts[modelID]
	nativeResponsesFailures.Unlock()
	if n < nativeResponsesEvictAfter {
		return
	}
	nativeResponsesModels.Lock()
	delete(nativeResponsesModels.ids, modelID)
	nativeResponsesModels.Unlock()
	nativeResponsesFailures.Lock()
	delete(nativeResponsesFailures.counts, modelID)
	nativeResponsesFailures.Unlock()
	slog.Warn("model evicted from native responses registry after consecutive failures", "model", modelID)
}

// shouldProbeNativeResponses 决定翻译路径失败后是否值得探测上游原生
// responses。仅在明确指示“端点/模型不受支持”（404、501、502、500）时探测；
// 严禁在 401（凭据失效）、403、429（限流）、400（客户端参数错误）等场景下
// 盲目探测，避免把单次失败放大成重试风暴。上游 200 包体仅是本地转换失败的
// 类型化错误（*anthropicProtocolError，如 overloaded_error）带有明确错误信息，
// 必须按原有逻辑返回，不做回退探测。传输层错误（无 HTTP 状态）同样不探测。
func shouldProbeNativeResponses(status int, err error) bool {
	if err != nil {
		var ape *anthropicProtocolError
		if errors.As(err, &ape) {
			return false
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false
		}
		// callOpenCodeAPI 在上游 HTTP 错误时同样会合成一个 err，因此不能
		// 仅凭 err != nil 就拒绝：有明确 HTTP 状态时继续按状态码判定；
		// 无状态（status == 0）的纯传输错误则不探测。
		if status == 0 {
			return false
		}
	}
	switch status {
	case http.StatusNotFound, // 404：chat 端点无此模型/路由
		http.StatusNotImplemented,      // 501：后端未实现 chat 通道
		http.StatusBadGateway,          // 502：网关路由失败
		http.StatusInternalServerError: // 500：部分后端用 500 表示模型不支持
		return true
	case http.StatusUnauthorized, // 401：凭据失效，探测必然同样失败
		http.StatusForbidden,       // 403：无权限
		http.StatusTooManyRequests, // 429：限流，探测会加剧雪崩
		http.StatusBadRequest:      // 400：客户端参数错误，重打无意义
		return false
	default:
		return false
	}
}
