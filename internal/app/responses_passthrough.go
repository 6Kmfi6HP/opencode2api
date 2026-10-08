package app

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/6Kmfi6HP/opencode2api/internal/config"
	"github.com/6Kmfi6HP/opencode2api/internal/logging"
	"github.com/6Kmfi6HP/opencode2api/internal/stats"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// ======================== function_call 参数浮点归一化 ========================
//
// 部分上游模型（如 muse-spark）对整数参数输出浮点字面量（如 1000.0），而
// Codex 等 Rust 客户端以 usize 反序列化，遇到浮点直接报错：
// "failed to parse function arguments: invalid type: floating point `1000.0`,
// expected usize"。这里做 best-effort 归一化：把 JSON 字符串外的整数浮点
// （1000.0 / 1000.00，后接 , } ] 空白 : 或结尾）改写为整数，字符串内的
// 内容（如 echo 1.0）绝不动。仅用于透传写回，不因不支持返回 400。

type argsNormState struct {
	inString bool
	escaped  bool
}

func isArgsDelim(c byte) bool {
	switch c {
	case ',', '}', ']', ':', ' ', '\t', '\n', '\r':
		return true
	default:
		return false
	}
}

// normalizeArgsFragment 归一化一段 arguments JSON 片段（完整或流式增量均可），
// 并滚动更新跨分片的字符串状态。调用方按 output_index 为每个工具调用维护
// 独立的 state，避免分片边界误判字符串内外。
func normalizeArgsFragment(s string, st *argsNormState) string {
	inString := false
	escaped := false
	if st != nil {
		inString = st.inString
		escaped = st.escaped
	}
	var out []byte
	out = make([]byte, 0, len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		if inString {
			out = append(out, c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			i++
			continue
		}
		// 字符串外
		if c == '"' {
			inString = true
			out = append(out, c)
			i++
			continue
		}
		if c == '-' || (c >= '0' && c <= '9') {
			j := i
			if s[j] == '-' {
				j++
				if j >= len(s) || s[j] < '0' || s[j] > '9' {
					out = append(out, c)
					i++
					continue
				}
			}
			k := j
			for k < len(s) && s[k] >= '0' && s[k] <= '9' {
				k++
			}
			if k < len(s) && s[k] == '.' {
				m := k + 1
				for m < len(s) && s[m] >= '0' && s[m] <= '9' {
					m++
				}
				frac := ""
				if m > k+1 {
					frac = s[k+1 : m]
				}
				allZero := len(frac) > 0
				for p := 0; p < len(frac); p++ {
					if frac[p] != '0' {
						allZero = false
						break
					}
				}
				var next byte
				hasNext := m < len(s)
				if hasNext {
					next = s[m]
				}
				// 小数部分全零且后接分隔符/结尾（排除 1.05 / 1.0e3 等真浮点/科学计数）。
				if allZero && (!hasNext || isArgsDelim(next)) {
					out = append(out, s[i:k]...)
					i = m
					continue
				}
			}
			// 非整数浮点或普通数字：原样拷贝数字前缀，后续字符主循环处理。
			for i < k {
				out = append(out, s[i])
				i++
			}
			continue
		}
		out = append(out, c)
		i++
	}
	if st != nil {
		st.inString = inString
		st.escaped = escaped
	}
	return string(out)
}

// normalizeArgumentsString 归一化完整 arguments JSON 字符串（无状态便捷封装）。
func normalizeArgumentsString(s string) string {
	if s == "" {
		return s
	}
	st := &argsNormState{}
	return normalizeArgsFragment(s, st)
}

// normalizeResponseOutputArguments 归一化响应 output 数组中各工具调用
// （function_call/tool_call/shell/apply_patch/custom_tool_call 等）的 arguments
// 或 input 字符串，返回是否改动。output_text 类可见文本绝不动。
func normalizeResponseOutputArguments(output []any) bool {
	changed := false
	for _, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		t, _ := item["type"].(string)
		switch t {
		case "function_call", "tool_call", "shell_call", "apply_patch_call",
			"custom_tool_call", "local_shell_call", "mcp_call":
		default:
			continue
		}
		for _, key := range []string{"arguments", "input"} {
			args, _ := item[key].(string)
			if args == "" {
				continue
			}
			if norm := normalizeArgumentsString(args); norm != args {
				item[key] = norm
				changed = true
			}
		}
	}
	return changed
}

// ======================== 超长 name 缩短（Anthropic 风格 64 上限） ========================
//
// muse-spark 上游的工具/name 字段沿用 Anthropic Messages 的 64 字符上限
// （超过即 400 invalid_request_error: `name` must be at most 64 characters）。
// Codex 桌面端会把已启用的连接器插件按
// mcp__codex_apps__<plugin>___<tool> 的完整拼法注入 Responses tools
// （如 mcp__codex_apps__plugin_management___update_app_permissions 恰好 66
// 字符），透传后被上游拒绝。这里做 lenient 缩短（前缀 + 短哈希，保持惟一与
// 确定性），并在把上游响应转发回客户端时按相反映射还原，保证客户端看到的
// function_call 名字仍是原始长名字。仅 muse-spark 系模型启用，不影响其它
// 上游与存储的会话状态（responsesHandler 用的是原始请求体）。

// responsesNameKey 记录请求中一类 name 字段的位置，用于统一遍历。
// 这里不需要结构体，使用函数指针即可。
// 短名与 restore 机制在 name_compat.go（c498cb0 并入）；registry 在 native_responses_registry.go。

// ======================== 回放历史 arguments 空串归一化 ========================
//
// muse-spark 偶发会发出 arguments 为空串的 function_call（幻觉工具名时尤其
// 如此），客户端原样回放到下一轮 input，上游在服务端反序列化这段历史时按
// `arguments` must be valid JSON 拒绝整个请求（空串/非串都不是合法 JSON
// 文本）。可见文本与 function_call_output 完全不动（output 靠 call_id
// 绑定，客户端 UI 不读 arguments）；合法 JSON 串原样通过，幂等。
func coalesceReplayedToolCallArgs(body map[string]any) bool {
	items, ok := body["input"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, raw := range items {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		t, _ := m["type"].(string)
		switch t {
		case "function_call", "custom_tool_call", "local_shell_call", "mcp_call":
		default:
			continue
		}
		v, exists := m["arguments"]
		if !exists {
			continue
		}
		if s, ok := v.(string); ok {
			if s == "" {
				m["arguments"] = "{}"
				changed = true
				continue
			}
			var parsed any
			if json.Unmarshal([]byte(s), &parsed) != nil {
				m["arguments"] = "{}"
				changed = true
			}
			continue
		}
		switch v.(type) {
		case map[string]any, []any:
			if b, err := json.Marshal(v); err == nil {
				m["arguments"] = string(b)
				changed = true
			}
		default:
			m["arguments"] = "{}"
			changed = true
		}
	}
	return changed
}

// clampPassThroughMaxTokens 把 max_output_tokens 钳制到 [128, cap]；cap>0 且
// cap<128 时以 cap 作为硬上限（配置者显式限满须尊重）。仅 cap>0 时调用。
func clampPassThroughMaxTokens(v, cap int) int {
	if v > cap {
		return cap
	}
	if v < 128 {
		if cap < 128 {
			return cap
		}
		return 128
	}
	return v
}

// sanitizeResponsesPassthroughBody 对原生透传体做 lenient 归一化，避免上游
// 严格校验 400（如 required 缺 key、reasoning.effort 非法、name 超长），
// 绝不因不支持返回 400。合法请求归一化后等价（幂等），可安全用于保真透传。
// 返回归一化后的请求体以及名字缩短映射（用于把上游响应里的缩短名还原回
// 客户端原始名）。非 muse-spark 模型或无法解析时，rewrites 为空但不返回 nil
// 指针，调用方恒可用。
func sanitizeResponsesPassthroughBody(rawBody []byte, modelID string) ([]byte, *responsesNameRewrites) {
	rewrites := newResponsesNameRewrites(false)
	var body map[string]any
	if err := json.Unmarshal(rawBody, &body); err != nil {
		return rawBody, rewrites
	}
	changed := false
	if tokCap := config.MaxTokensCapFor(modelID); tokCap > 0 {
		if v, ok := intFromAny(body["max_output_tokens"]); !ok || v <= 0 {
			body["max_output_tokens"] = clampPassThroughMaxTokens(tokCap, tokCap)
			changed = true
		} else if cv := clampPassThroughMaxTokens(v, tokCap); cv != v {
			body["max_output_tokens"] = cv
			changed = true
		}
	}
	if !isMuseSparkModel(modelID) {
		if changed {
			if b, err := json.Marshal(body); err == nil {
				return b, rewrites
			}
		}
		return rawBody, rewrites
	}
	if tools, ok := body["tools"].([]any); ok {
		for i, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			// 两种形状：{parameters:{...}} 与 {function:{parameters:{...}}}。
			if params, ok := tm["parameters"].(map[string]any); ok {
				tm["parameters"] = normalizeResponsesToolParameters(params)
				tools[i] = tm
				changed = true
				continue
			}
			if fn, ok := tm["function"].(map[string]any); ok {
				if params, ok := fn["parameters"].(map[string]any); ok {
					fn["parameters"] = normalizeResponsesToolParameters(params)
					tm["function"] = fn
					tools[i] = tm
					changed = true
				} else if _, hasParams := fn["parameters"]; !hasParams {
					// 缺 parameters 时补最小可用 shapes，避免上游 400。
					fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}}
					tm["function"] = fn
					tools[i] = tm
					changed = true
				}
			}
		}
		if changed {
			body["tools"] = tools
		}
	}
	if r, ok := body["reasoning"].(map[string]any); ok {
		e, _ := r["effort"].(string)
		effortChanged := false
		if e != "" {
			if ne := normalizeResponsesEffort(e); ne != e {
				effortChanged = true
				if ne == "" {
					// effort 不在白名单且无法归一化（含 none）：整段 reasoning
					// 省略；残留的 summary-only 请求对上游无意义且可能 400。
					delete(body, "reasoning")
				} else {
					r["effort"] = ne
					body["reasoning"] = r
				}
			}
		}
		if effortChanged {
			changed = true
		}
		if _, reasonKept := body["reasoning"]; reasonKept && e != "" {
			// effort 有效（含归一化后）：只带 effort 时 muse-spark 静默思考、
			// summary 恒空，补默认 summary:auto 让客户端可见；客户端已显式
			// 给出 summary 值时尊重原样。
			if _, hasSummary := r["summary"]; !hasSummary {
				r["summary"] = "auto"
				body["reasoning"] = r
				changed = true
			}
		}
	}
	if rwChanged := rewrites.shortenResponsesBodyNames(body); rwChanged {
		changed = true
	}
	if coalesceReplayedToolCallArgs(body) {
		changed = true
	}
	// 预防性剥离回放 reasoning 回声：密文绑定发起方账号+出口，sticky 出口
	// 轮换/重启后必然 400；每轮出站前剥掉，不等上游拒绝再重发。summary 等
	// 可见内容保留，请求语义不变。
	if stripReplayedReasoningEchoFromBody(body) {
		changed = true
	}
	if !changed {
		return rawBody, rewrites
	}
	if b, err := json.Marshal(body); err == nil {
		return b, rewrites
	}
	return rawBody, rewrites
}

// ======================== 回放推理密文修复（发起方绑定） ========================
//
// 上游把 reasoning 的 encrypted_content 绑定到「发起方」（账号 + 出口）：同一段
// 密文换一个出口/账号回放会被 400 拒绝（reasoning `encrypted_content` was not
// issued to this caller）。客户端多轮对话会把上一轮收到的 reasoning item 原样
// 回传，而网关的 sticky 出口/域名随时可能改绑（sticky TTL 过期、429/5xx 重试后
// 失效重绑、进程重启），于是会话中途就会撞上该 400，且此后每一轮都会复现。
//
// 处理：只在该 400 出现时把回放的推理回声（id + encrypted_content）剥掉后重发
// 一次。可见对话（消息、工具调用）完全不受影响，只是不再回放旧推理密文；上游
// 会为当前发起方重新签发推理内容。

// upstreamForeignReasoningEchoMarker 是上游拒绝他人推理密文时的固定措辞。
const upstreamForeignReasoningEchoMarker = "was not issued to this caller"

// isForeignReasoningEchoError 报告上游 400 是否由「回放的 reasoning 密文不属于
// 当前发起方」引起。
func isForeignReasoningEchoError(status int, body []byte) bool {
	if status != http.StatusBadRequest || len(body) == 0 {
		return false
	}
	msg := strings.ToLower(string(body))
	return strings.Contains(msg, "encrypted_content") &&
		strings.Contains(msg, upstreamForeignReasoningEchoMarker)
}

// stripReplayedReasoningEcho 删除请求体 input 中回放的 reasoning 回声字段。
// id 与 encrypted_content 都绑定发起方，必须同时删除：
//   - 只删 id：密文仍被判定为他人签发，同一个 400 不变；
//   - 只删密文：上游改为按 id 找不到该 item（Referenced reasoning item ... 400）。
//
// 其它字段（如 summary）与其它 item 原样保留，请求语义不变。
// stripReplayedReasoningEchoFromBody 在已解析 body 上原地剥离回放 reasoning
// 回声（id + encrypted_content 成对删除）。
func stripReplayedReasoningEchoFromBody(body map[string]any) bool {
	items, ok := body["input"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if itemType, _ := m["type"].(string); itemType != "reasoning" {
			continue
		}
		if _, ok := m["encrypted_content"]; ok {
			delete(m, "encrypted_content")
			changed = true
		}
		if _, ok := m["id"].(string); ok {
			delete(m, "id")
			changed = true
		}
	}
	return changed
}

func stripReplayedReasoningEcho(rawBody []byte) ([]byte, bool) {
	var body map[string]any
	if err := json.Unmarshal(rawBody, &body); err != nil {
		return rawBody, false
	}
	if !stripReplayedReasoningEchoFromBody(body) {
		return rawBody, false
	}
	fixed, err := json.Marshal(body)
	if err != nil {
		return rawBody, false
	}
	return fixed, true
}

// passthroughMaxOutputTokens 从归一化后的透传体读出 max_output_tokens，供日志
// 观测用；未设置或非正返回 0。
func passthroughMaxOutputTokens(rawBody []byte) int {
	var body map[string]any
	if err := json.Unmarshal(rawBody, &body); err != nil {
		return 0
	}
	if v, ok := intFromAny(body["max_output_tokens"]); ok && v > 0 {
		return v
	}
	return 0
}

// callResponsesWithEchoRepair 调用上游 responses 端点；若上游因回放的推理密文
// 不属于当前发起方而 400，则剥掉推理回声后原样重发一次。重发拿不到响应时返回
// 第一次的上游错误，客户端看到的失败原因保持真实。
func callResponsesWithEchoRepair(ctx context.Context, auth UpstreamAuth, modelID string, rawBody []byte) (io.ReadCloser, int, http.Header, error) {
	rc, status, header, err := callOpenCodeEndpoint(ctx, "responses", rawBody, modelID, auth)
	if err != nil || status != http.StatusBadRequest {
		return rc, status, header, err
	}

	errBody, readErr := io.ReadAll(io.LimitReader(rc, 64*1024))
	_ = rc.Close()
	// 未识别或无需修复时按原样回放错误体。
	replayUpstreamError := func() io.ReadCloser { return io.NopCloser(bytes.NewReader(errBody)) }
	if readErr != nil || !isForeignReasoningEchoError(status, errBody) {
		return replayUpstreamError(), status, header, nil
	}
	repaired, changed := stripReplayedReasoningEcho(rawBody)
	if !changed {
		return replayUpstreamError(), status, header, nil
	}

	logging.FromContext(ctx).Info("responses reasoning echo repair",
		"model", modelID, "reason", "foreign_encrypted_content")
	repairedRC, repairedStatus, repairedHeader, repairedErr := callOpenCodeEndpoint(ctx, "responses", repaired, modelID, auth)
	if repairedErr != nil {
		if repairedRC != nil {
			_ = repairedRC.Close()
		}
		return replayUpstreamError(), status, header, nil
	}
	return repairedRC, repairedStatus, repairedHeader, nil
}

func probeNativeResponses(ctx context.Context, w http.ResponseWriter, auth UpstreamAuth, modelID string, rawBody []byte, stream bool, req ResponsesAPIRequest) bool {
	rawBody, rewrites := sanitizeResponsesPassthroughBody(rawBody, modelID)
	rewrites.restoreStubCase = shouldRestoreToolCase(ctx)
	logging.FromContext(ctx).Info("responses passthrough probe max_output_tokens",
		"model", modelID,
		"max_output_tokens", passthroughMaxOutputTokens(rawBody),
	)
	rc, status, header, err := callResponsesWithEchoRepair(ctx, auth, modelID, rawBody)
	if err != nil || status < 200 || status >= 300 {
		if rc != nil {
			rc.Close()
		}
		return false
	}
	defer rc.Close()

	rememberNativeResponsesModel(modelID)
	logging.FromContext(ctx).Info("responses_probe_succeeded", "model", modelID, "stream", stream)

	relayResponsesToClient(ctx, w, rc, status, header, modelID, stream, req, rewrites, auth, rawBody, func(c context.Context, b []byte) (io.ReadCloser, int, http.Header, error) {
		return callResponsesWithEchoRepair(c, auth, modelID, b)
	})
	return true
}

// forwardNativeResponses 处理已确认原生模型的标准反向代理转发：无论上游返回
// 2xx、4xx 还是 5xx，均保真透传状态码与错误信息（符合标准代理语义）。仅在
// 真正的传输层错误（无法拿到上游响应）时返回 false，调用方兜底写 502。
func forwardNativeResponses(ctx context.Context, w http.ResponseWriter, auth UpstreamAuth, modelID string, rawBody []byte, stream bool, req ResponsesAPIRequest) bool {
	rawBody, rewrites := sanitizeResponsesPassthroughBody(rawBody, modelID)
	rewrites.restoreStubCase = shouldRestoreToolCase(ctx)
	logging.FromContext(ctx).Info("responses passthrough max_output_tokens",
		"model", modelID,
		"max_output_tokens", passthroughMaxOutputTokens(rawBody),
		"cap", config.MaxTokensCapFor(modelID),
		"auth_source", auth.Source,
	)
	rc, status, header, err := callResponsesWithEchoRepair(ctx, auth, modelID, rawBody)
	if err != nil {
		markNativeResponsesFailure(modelID)
		return false
	}
	defer rc.Close()

	if status >= 500 {
		markNativeResponsesFailure(modelID)
	}

	relayResponsesToClient(ctx, w, rc, status, header, modelID, stream, req, rewrites, auth, rawBody, func(c context.Context, b []byte) (io.ReadCloser, int, http.Header, error) {
		return callResponsesWithEchoRepair(c, auth, modelID, b)
	})
	return true
}

// relayResponsesToClient 统一负责流式与非流式的保真透传：过滤后的安全响应头、
// 流式实时 Flush、流/非流双路 Token 统计、成功响应的会话状态保存。
func relayResponsesToClient(ctx context.Context, w http.ResponseWriter, rc io.Reader, status int, header http.Header, modelID string, stream bool, req ResponsesAPIRequest, rewrites *responsesNameRewrites, auth UpstreamAuth, rawBody []byte, upstreamCall func(context.Context, []byte) (io.ReadCloser, int, http.Header, error)) {
	// 拷贝上游安全响应头（X-RateLimit-* 等），客户端可见剩余额度与重置时间。
	for k, v := range filterResponseHeaders(header) {
		w.Header()[k] = v
	}

	if stream && status >= 200 && status < 300 {
		// 用 DriveStreamWithRetry 在 peek 失败时切换 key 重试,空流/超时/上游
		// 错误帧都被翻译为可重试的 errStreamIncompleteNoCommit。首轮复用
		// 调用方已打开的 rc,后续重试走 upstreamCall 让 key pool 换下一把 key。
		pending := rc.(io.ReadCloser)
		callOnce := func(c context.Context) (io.ReadCloser, int, error) {
			if pending != nil {
				r := pending
				pending = nil
				return r, status, nil
			}
			nrc, nstatus, _, nerr := upstreamCall(c, rawBody)
			return nrc, nstatus, nerr
		}
		runOnce := func(c context.Context, w http.ResponseWriter, nrc io.Reader, _ []streamReadResult, _ *streamReader) (bool, error) {
			return relayResponsesStream(c, w, nrc, status, modelID, req, rewrites, auth, rawBody, upstreamCall)
		}
		committed, driveErr := DriveStreamWithRetry(ctx, w, ResponsesProtocolHooks, callOnce, runOnce)
		if !committed && driveErr != nil {
			logging.FromContext(ctx).Warn("relayResponsesStream exhausted retries", "model", modelID, "err", driveErr)
		}
		return
	}

	respBody, err := io.ReadAll(io.LimitReader(rc, 32*1024*1024))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"failed to read upstream body"}}`))
		return
	}

	// 免费层上游强制 stream:true，客户端非流时先从 SSE 提取 response JSON
	//（幂等：非 SSE 开头原样返回）。
	if status >= 200 && status < 300 {
		respBody = extractResponsesJsonFromSse(respBody)
	}

	// 非流式成功响应：仅 muse-spark 归一化 function_call 参数中的整数浮点
	//（如 1000.0->1000），避免 Codex 等严格客户端反序列化失败。字符串内数字不动。
	if status >= 200 && status < 300 && isMuseSparkModel(modelID) {
		var tmp map[string]any
		if json.Unmarshal(respBody, &tmp) == nil {
			changed := false
			if output, ok := tmp["output"].([]any); ok {
				if normalizeResponseOutputArguments(output) {
					changed = true
				}
			}
			if rewrites.restoreResponsesPayloadNames(tmp) {
				changed = true
			}
			if changed {
				if nb, merr := json.Marshal(tmp); merr == nil {
					respBody = nb
				}
			}
		}
	}

	if ct := header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_, _ = w.Write(respBody)

	// 仅在成功时解析 Token 消耗并保存会话状态（previous_response_id 链条）。
	if status == http.StatusOK {
		var respMap map[string]any
		if json.Unmarshal(respBody, &respMap) == nil {
			recordResponsesUsage(modelID, respMap["usage"])
			storeResponseState(respMap, req)
		}
	}
}

// passthroughLineProducedContent 报告单行透传 SSE 是否携带可交付内容：
// 非空 output_text.delta、具名 tool 系 output_item.done、含内容的终态
// output。peek 回放与主循环共用（布尔幂等，只用于零内容 kill 判定）。
func passthroughLineProducedContent(line []byte) bool {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data: ")) {
		return false
	}
	payload := bytes.TrimSpace(trimmed[len("data: "):])
	if len(payload) == 0 || payload[0] != '{' {
		return false
	}
	var evt map[string]any
	if json.Unmarshal(payload, &evt) != nil {
		return false
	}
	switch typ, _ := evt["type"].(string); typ {
	case "response.output_text.delta":
		if d, _ := evt["delta"].(string); d != "" {
			return true
		}
	case "response.output_item.done":
		if item, ok := evt["item"].(map[string]any); ok {
			return responsesOutputItemsHaveContent([]any{item})
		}
	case "response.completed", "response.incomplete":
		if resp, ok := evt["response"].(map[string]any); ok {
			return responsesOutputHasContent(resp)
		}
	}
	return false
}

// relayResponsesStream 逐行透传 SSE 并在每个事件行后 Flush，保证打字机效果；
// 同时从 response.completed / usage 事件中提取 usage 做 Token 统计，并保存
// 完整响应对象以维持 previous_response_id 会话链条。
//
// 返回 (true, nil)：已 commit 且本轮处理完毕（写完或合成了收尾），调用方
// 无需重试。返回 (false, err)：peek 窗口内未 commit（空流 / EOF / 错误帧 /
// 首字节超时），由 DriveStreamWithRetry 决定是否换 key 重发。已 commit 后
// 主循环返回 (true, nil)——半截流由 continuation / 末尾 [DONE] 兜底处理。
func relayResponsesStream(ctx context.Context, w http.ResponseWriter, rc io.Reader, status int, modelID string, req ResponsesAPIRequest, rewrites *responsesNameRewrites, auth UpstreamAuth, rawBody []byte, upstreamCall func(context.Context, []byte) (io.ReadCloser, int, http.Header, error)) (bool, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// peek 首帧：在 WriteHeader 之前约束 commit 边界，空流 / EOF / 错误帧 /
	// 首字节超时返回 errStreamIncompleteNoCommit,由调用方驱动重试。宽限窗
	// 产出感知（对齐 claude_responses）：壳帧与零内容终态不判产出 commit，
	// 限速 kill 的空流在未写字节前可换 key 重试。
	peek := PeekFirstFrameWithGrace(ctx, rc, time.Duration(config.StreamFirstByteTimeoutMs())*time.Millisecond, time.Duration(responsesShellGraceMs())*time.Millisecond, ResponsesProtocolHooks)
	if peek.Err != nil {
		return false, peek.Err
	}

	w.WriteHeader(status)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	// byte-level 透传：peek 已消费的字节保持原样直写 w,与主循环读取的
	// 后续字节拼成完整 SSE 流。
	// 关键不变量:WriteHeader(status) 已发出 —— 此后绝对不能再返回
	// (false, ...) 让 Drive 二次 WriteHeader。flush 写错(连接已断)也
	// 同样视为已 commit。
	if err := FlushPeekedBytes(w, peek.Consumed); err != nil {
		return true, err
	}
	if flusher != nil {
		flusher.Flush()
	}

	// 续用 peek 内部的 reader（它的 bufio 可能已预读后续行）。
	sr := peek.Reader
	if sr == nil {
		// EOF 收尾的 peek 没留下 reader——上游已 EOF,主循环立即结束。
		sr = newStreamReader(ctx, rc, 0)
	}
	defer sr.Close()

	var lastUsage map[string]any
	var lastResponse map[string]any
	sawData := len(peek.Consumed) > 0
	doneSeen := false
	writeFailed := false
	terminalSeen := false
	// producedContent 是否见过可交付内容（跨续写轮累计）：非空
	// output_text.delta / 具名 tool item / 含内容的终态 output。零内容
	// incomplete 到达且全程无产出时说明生成被杀在不可见阶段，走 error
	// 而非直写/续写（对齐 claude_responses 零内容检查）。
	producedContent := false
	// emptyKill 标记本轮终态为零内容 kill：跳过直写与续写，发 error + DONE。
	emptyKill := false
	// peek 阶段已看到完整帧，但终端事件（completed/failed/incomplete / [DONE]）
	// 要逐行扫过目前 consumed 才知道——这里只预先回填终端标志，让末尾判定
	// 与原有逻辑一致。
	for _, res := range peek.Consumed {
		trimmed := bytes.TrimSpace([]byte(res.line))
		if bytes.Equal(trimmed, []byte("data: [DONE]")) || bytes.Equal(trimmed, []byte("[DONE]")) {
			doneSeen = true
			continue
		}
		if !bytes.HasPrefix(trimmed, []byte("data: ")) {
			continue
		}
		payload := trimmed[6:]
		if len(payload) == 0 || payload[0] != '{' {
			continue
		}
		var evt map[string]any
		if json.Unmarshal(payload, &evt) != nil {
			continue
		}
		if _, response := extractStreamEventUsage([]byte(res.line)); response != nil {
			lastResponse = response
			if s, _ := response["status"].(string); s == "completed" || s == "failed" || s == "incomplete" {
				terminalSeen = true
			}
		}
		if passthroughLineProducedContent([]byte(res.line)) {
			producedContent = true
		}
	}

	argStates := map[int]*argsNormState{}
	argItemToOutput := map[string]int{}

	// 续写状态
	const maxContinuations = 3
	var accumulatedOutput []any
	var contUsage map[string]any
	currentRC := rc.(io.ReadCloser)
	isTruncatedByMaxTokens := false
	// pendingTerminalLine 保存本轮的终结事件行（incomplete + max_output_tokens 时暂缓写入）
	var pendingTerminalLine []byte

	// 首轮主循环复用 peek 的 streamReader；续写轮重新发起 upstreamCall 后,
	// 用新响应的 rc 新建 streamReader。
	currentReader := sr

	for round := 0; round <= maxContinuations && !doneSeen && !writeFailed; round++ {
		var contLastResponse map[string]any
		isTruncatedByMaxTokens = false
		pendingTerminalLine = nil

		reader := currentReader
		needClose := false
		if round > 0 {
			// 续写轮：用新 rc 起一个 streamReader(它的内部协程按行投递,与
			// 主循环的 select 模型对齐),不再用裸 bufio.NewReader——前者同时
			// 兼容 rc 后台 Close 触发 EOF。本轮结束后立刻 Close,不堆积协程。
			reader = newStreamReader(ctx, currentRC, 0)
			needClose = true
		}

	lineLoop:
		for {
			var (
				line string
				err  error
			)
			select {
			case <-ctx.Done():
				// 已写过 WriteHeader + peeked 字节,不能再返回 (false, ...) 让
				// Drive 二次 WriteHeader / retry。返回 (true, ...) 透传 ctx
				// 错误,Drive 看到 committed=true 立即结束循环。
				return true, ctx.Err()
			case res := <-reader.Read():
				line = res.line
				err = res.err
			}
			if len(line) > 0 {
				lineBytes := []byte(line)
				outLine := lineBytes
				if isMuseSparkModel(modelID) {
					if normalized, ok := normalizeResponsesStreamLine(lineBytes, argStates, argItemToOutput, rewrites); ok {
						outLine = normalized
					}
				}
				// 内容跟踪（含 peek 期 serializer 已写过的字节：主循环重放
				// 时再次统计是幂等的布尔量，只用于零内容 kill 判定）。
				if !producedContent && passthroughLineProducedContent(outLine) {
					producedContent = true
				}
				trimmed := bytes.TrimSpace(outLine)
				isDoneSentinel := bytes.Equal(trimmed, []byte("data: [DONE]")) || bytes.Equal(trimmed, []byte("[DONE]"))
				if isDoneSentinel && !terminalSeen {
					if err != nil {
						break lineLoop
					}
					continue
				}
				if isDoneSentinel {
					doneSeen = true
				}

				// 检测终结事件：判断是否为 incomplete + max_output_tokens
				isTerminalLine := false
				isIncompleteMaxTokens := false
				termIsIncomplete := false
				var termResp map[string]any
				if bytes.HasPrefix(trimmed, []byte("data: ")) {
					payload := trimmed[6:]
					if len(payload) > 0 && payload[0] == '{' {
						var evt map[string]any
						if json.Unmarshal(payload, &evt) == nil {
							if typ, _ := evt["type"].(string); typ == "response.completed" || typ == "response.failed" || typ == "response.incomplete" {
								isTerminalLine = true
								if typ == "response.incomplete" {
									termIsIncomplete = true
									termResp, _ = evt["response"].(map[string]any)
									if resp, ok := evt["response"].(map[string]any); ok {
										if details, ok := resp["incomplete_details"].(map[string]any); ok {
											if reason, _ := details["reason"].(string); reason == "max_output_tokens" {
												isIncompleteMaxTokens = true
											}
										}
									}
								}
							}
						}
					}
				}

				// 零内容 kill：incomplete 到达时本轮无产出、终态 output 无
				// 内容、累计 output 亦无内容——生成被杀在不可见阶段。不直写
				// 也不续写（从空 output 续写只会烧轮次仍是空），记标记后跳
				// 出，由外层发 error + DONE。
				if isTerminalLine && termIsIncomplete && !producedContent &&
					!responsesOutputHasContent(termResp) &&
					!responsesOutputItemsHaveContent(accumulatedOutput) {
					emptyKill = true
					break lineLoop
				}

				// 终结事件：incomplete+max_output_tokens 时暂缓写入，其他直写
				if isTerminalLine && isIncompleteMaxTokens {
					pendingTerminalLine = outLine
					isTruncatedByMaxTokens = true
				} else if !isTerminalLine {
					if _, werr := w.Write(outLine); werr != nil {
						writeFailed = true
						break lineLoop
					}
					if flusher != nil {
						flusher.Flush()
					}
				} else {
					// completed / failed / 其他 incomplete：直写
					if _, werr := w.Write(outLine); werr != nil {
						writeFailed = true
						break lineLoop
					}
					if flusher != nil {
						flusher.Flush()
					}
				}

				sawData = true

				if usage, response := extractStreamEventUsage(outLine); usage != nil || response != nil {
					if usage != nil {
						lastUsage = usage
						if cacheDebugEnabled() {
							b, _ := json.Marshal(usage)
							slog.Info("cache_debug_stream_usage", "model", modelID, "usage", string(b))
						}
					}
					if response != nil {
						if round == 0 {
							lastResponse = response
						} else {
							contLastResponse = response
						}
						if s, _ := response["status"].(string); s == "completed" || s == "failed" || s == "incomplete" {
							terminalSeen = true
						}
					}
				}
			}
			if err != nil {
				break lineLoop
			}
		}
		if needClose {
			reader.Close()
		}
		if flusher != nil {
			flusher.Flush()
		}
		_ = currentRC.Close()

		// 零内容 kill：不直写空 incomplete、不续写，直接发 error + DONE，
		// 客户端感知失败重试该回合（对齐翻译路径零内容检查）。
		if emptyKill {
			errPayload := map[string]any{
				"type": "error",
				"error": map[string]any{
					"message": "upstream returned incomplete with no content (generation killed)",
					"type":    "upstream_truncated",
				},
			}
			if b, err := json.Marshal(errPayload); err == nil {
				_, _ = w.Write([]byte("event: error\ndata: " + string(b) + "\n\n"))
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			doneSeen = true
			logging.FromContext(ctx).Warn("responses passthrough zero-content incomplete, sent error",
				"model", modelID, "round", round)
			break
		}

		if round == 0 {
			// 首回合：保存 output 供后续续写引用
			if lastResponse != nil {
				if output, ok := lastResponse["output"].([]any); ok {
					accumulatedOutput = output
				}
				if usage, ok := lastResponse["usage"].(map[string]any); ok {
					contUsage = usage
				}
			}
			if !isTruncatedByMaxTokens {
				// 未截断（completed/failed/其他 incomplete）：正常结束
				break
			}
			// 截断：不发 incomplete，续写
		} else {
			// 续写轮
			if contLastResponse != nil {
				if output, ok := contLastResponse["output"].([]any); ok && len(output) > 0 {
					accumulatedOutput = append(accumulatedOutput, output...)
				}
				if usage, ok := contLastResponse["usage"].(map[string]any); ok {
					contUsage = usage
				}
			}
			// 补发被暂缓的终结事件（用新的 response 对象）
			if pendingTerminalLine != nil {
				// 解析 pendingTerminalLine 中的 response 对象
				trimmed := bytes.TrimSpace(pendingTerminalLine)
				if bytes.HasPrefix(trimmed, []byte("data: ")) {
					payload := trimmed[6:]
					var evt map[string]any
					if json.Unmarshal(payload, &evt) == nil {
						if resp, ok := evt["response"].(map[string]any); ok {
							// 替换 response 的 output / status / incomplete_details / usage
							resp["output"] = accumulatedOutput
							resp["status"] = "completed"
							resp["incomplete_details"] = nil
							if contUsage != nil {
								resp["usage"] = contUsage
							}
							evt["type"] = "response.completed"
							evt["response"] = resp
							if b, merr := json.Marshal(evt); merr == nil {
								_, _ = w.Write([]byte("event: response.completed\ndata: " + string(b) + "\n\n"))
								if flusher != nil {
									flusher.Flush()
								}
							}
						}
					}
				}
			}
			break
		}

		// 达到最大续写次数仍截断
		if round == maxContinuations {
			logging.FromContext(ctx).Warn("max continuations reached, sending incomplete", "model", modelID)
			// 补发被暂缓的 incomplete（原始内容）
			if pendingTerminalLine != nil {
				_, _ = w.Write(pendingTerminalLine)
				if flusher != nil {
					flusher.Flush()
				}
			}
			break
		}

		// 发起续写请求
		contBody, contErr := buildContinuationBody(rawBody, accumulatedOutput, modelID)
		if contErr != nil {
			logging.FromContext(ctx).Warn("failed to build continuation body", "error", contErr)
			if pendingTerminalLine != nil {
				_, _ = w.Write(pendingTerminalLine)
				if flusher != nil {
					flusher.Flush()
				}
			}
			break
		}
		newRC, newStatus, _, contCallErr := upstreamCall(ctx, contBody)
		if contCallErr != nil || newStatus < 200 || newStatus >= 300 {
			if newRC != nil {
				_ = newRC.Close()
			}
			logging.FromContext(ctx).Warn("continuation request failed", "round", round+1, "status", newStatus)
			if pendingTerminalLine != nil {
				_, _ = w.Write(pendingTerminalLine)
				if flusher != nil {
					flusher.Flush()
				}
			}
			break
		}
		currentRC = newRC
		terminalSeen = false
	}

	// 用量只记录一次：优先续写轮的合并值，否则为首轮值
	if contUsage != nil {
		recordResponsesUsage(modelID, contUsage)
	} else if lastUsage != nil {
		recordResponsesUsage(modelID, lastUsage)
	}
	if lastResponse != nil {
		if _, hasID := lastResponse["id"].(string); hasID {
			storeResponseState(lastResponse, req)
		}
	}

	// EOF 兜底：若整个流一次终结事件都没出现（response.completed/failed/
	// incomplete),且上游也未发 [DONE],补一条 response.incomplete 保证
	// 客户端正常结束——既不发 completed 假信号，也补上等 [DONE] 的客户端所盼。
	if !writeFailed && sawData && !doneSeen {
		if !terminalSeen {
			incomplete := map[string]any{
				"type": "response.incomplete",
				"response": map[string]any{
					"object":             "response",
					"status":             "incomplete",
					"incomplete_details": map[string]any{"reason": "max_output_tokens"},
					"output":             []any{},
				},
			}
			if lastResponse != nil {
				if id, _ := lastResponse["id"].(string); id != "" {
					incomplete["response"].(map[string]any)["id"] = id
				}
			}
			if b, err := json.Marshal(incomplete); err == nil {
				_, _ = w.Write([]byte("event: response.incomplete\ndata: " + string(b) + "\n\n"))
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	return true, nil
}

// buildContinuationBody 构造续写请求：input 设为已收到的 output 数组，
// max_output_tokens 保持 cap，其余字段从原始请求复制。
func buildContinuationBody(rawBody []byte, accumulatedOutput []any, modelID string) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(rawBody, &body); err != nil {
		return nil, err
	}
	// Responses API 续写语义：input = 原始 input + 已生成 output（assistant 已说的话）
	// 见 https://platform.openai.com/docs/api-reference/responses/object#responses/object-previous_response_id
	// 上游要求 input 非空；后续 item 追加在末尾作为续写上下文。
	originalInput := body["input"]
	if originalInput == nil {
		originalInput = ""
	}
	items := []any{}
	if arr, ok := originalInput.([]any); ok {
		items = append(items, arr...)
	} else if s, ok := originalInput.(string); ok && s != "" {
		items = append(items, map[string]any{"role": "user", "content": s})
	}
	// 把已生成的 output（reasoning + message + tool_calls）追加为续写上下文
	items = append(items, accumulatedOutput...)
	// 末尾加一个续写提示，让模型继续未完成的内容
	// 必须在 output 之后追加，避免 undoing 已有 output 的 sequence_number 连续性
	items = append(items, map[string]any{"role": "user", "content": "Please continue."})
	body["input"] = items
	tokCap := config.MaxTokensCapFor(modelID)
	if tokCap > 0 {
		body["max_output_tokens"] = tokCap
	}
	body["stream"] = true
	return json.Marshal(body)
}

// normalizeResponsesStreamLine 归一化单行 SSE data 事件中的 function_call 参数，
// 返回归一化后的完整行与是否改动。output_text 类文本增量永不动。
func normalizeResponsesStreamLine(line []byte, argStates map[int]*argsNormState, argItemToOutput map[string]int, rewrites *responsesNameRewrites) ([]byte, bool) {
	if !bytes.HasPrefix(line, []byte("data:")) && !bytes.HasPrefix(line, []byte("data: ")) {
		return nil, false
	}
	// 保留原始行尾（\n / \r\n）与 data 前缀风格。
	prefix := "data: "
	rest := line
	if bytes.HasPrefix(line, []byte("data: ")) {
		prefix = "data: "
		rest = line[len("data: "):]
	} else {
		// "data:" 后无空格的非标准形态
		prefix = "data:"
		rest = line[len("data:"):]
	}
	payload := bytes.TrimSpace(rest)
	if len(payload) == 0 || payload[0] != '{' {
		return nil, false
	}
	// 去掉行尾换行后再解析，避免 \r 干扰。
	payloadTrim := bytes.TrimRight(payload, "\r\n")
	var evt map[string]any
	if json.Unmarshal(payloadTrim, &evt) != nil {
		return nil, false
	}
	typ, _ := evt["type"].(string)
	// 可见文本/推理/语音增量绝不动（避免改写 echo 1.0 等可见输出）。
	switch typ {
	case "response.output_text.delta", "response.refusal.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.audio.delta", "response.audio_transcript.delta":
		return nil, false
	}
	outputIndex := -1
	if v, ok := evt["output_index"].(float64); ok {
		outputIndex = int(v)
	}
	itemID, _ := evt["item_id"].(string)
	changed := false

	// output_item.added/done：登记映射，并归一化 item 内自带的参数全量。
	if item, ok := evt["item"].(map[string]any); ok && item != nil {
		if id, _ := item["id"].(string); id != "" && outputIndex >= 0 {
			argItemToOutput[id] = outputIndex
			if callID, _ := item["call_id"].(string); callID != "" {
				argItemToOutput[callID] = outputIndex
			}
		}
		for _, key := range []string{"arguments", "input"} {
			if s, _ := item[key].(string); s != "" {
				if norm := normalizeArgumentsString(s); norm != s {
					item[key] = norm
					changed = true
				}
			}
		}
	}
	// done 类事件顶层 arguments/input 全量（如 function_call_arguments.done）。
	for _, key := range []string{"arguments", "input"} {
		if s, _ := evt[key].(string); s != "" {
			// 顶层全量字符串用无状态归一化（与分片状态无关，避免污染）。
			if norm := normalizeArgumentsString(s); norm != s {
				evt[key] = norm
				changed = true
			}
		}
	}
	// 增量 delta：用按 output 分片的状态归一化（跨分片字符串跟踪）。
	if delta, _ := evt["delta"].(string); delta != "" {
		oi := outputIndex
		if oi < 0 && itemID != "" {
			if mapped, ok := argItemToOutput[itemID]; ok {
				oi = mapped
			}
		}
		if oi < 0 {
			oi = -1 // 共享 fallback，顺序到达时等价
		}
		st, ok := argStates[oi]
		if !ok {
			st = &argsNormState{}
			argStates[oi] = st
		}
		if norm := normalizeArgsFragment(delta, st); norm != delta {
			evt["delta"] = norm
			changed = true
		}
	}
	// completed/incomplete 内嵌全量 output。
	if resp, ok := evt["response"].(map[string]any); ok && resp != nil {
		if output, ok := resp["output"].([]any); ok && len(output) > 0 {
			if normalizeResponseOutputArguments(output) {
				changed = true
			}
		}
	}
	if rewrites.restoreResponsesPayloadNames(evt) {
		changed = true
	}
	if !changed {
		return nil, false
	}
	nb, err := json.Marshal(evt)
	if err != nil {
		return nil, false
	}
	return append([]byte(prefix), append(nb, '\n')...), true
}

// extractStreamEventUsage 解析单行 SSE 事件，返回其中携带的 usage 与完整
// response 对象（任一不存在则对应返回 nil）。兼容两种形态：
//   - response.completed 事件：{"type":"...","response":{"id":...,"usage":{...}}}
//   - 直接 usage 事件：{"usage":{...}}
func extractStreamEventUsage(line []byte) (usage map[string]any, response map[string]any) {
	if !bytes.HasPrefix(line, []byte("data: ")) {
		return nil, nil
	}
	payload := bytes.TrimSpace(line[len("data: "):])
	if len(payload) == 0 || payload[0] != '{' {
		return nil, nil // data: [DONE] 等非 JSON 负载
	}
	var eventObj map[string]any
	if json.Unmarshal(payload, &eventObj) != nil {
		return nil, nil
	}
	if rObj, ok := eventObj["response"].(map[string]any); ok {
		response = rObj
		if u, ok := rObj["usage"].(map[string]any); ok {
			usage = u
		}
		return usage, response
	}
	if u, ok := eventObj["usage"].(map[string]any); ok {
		usage = u
	}
	return usage, nil
}

// recordResponsesUsage 按 Responses 协议 usage 口径记录 Token 与缓存统计。
// 非标准形态（缺字段/零值）直接忽略，调用方无需额外分支。
func recordResponsesUsage(modelID string, usage any) {
	u, ok := usage.(map[string]any)
	if !ok {
		return
	}
	t := stats.TokenUsage{}.FromMap(u)
	if t.TotalTokens <= 0 {
		return
	}
	stats.RecordTokenUsage(modelID, t.PromptTokens, t.CompletionTokens, t.TotalTokens)
	stats.RecordCacheUsage(modelID, u)
}
