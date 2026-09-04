package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
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
//（function_call/tool_call/shell/apply_patch/custom_tool_call 等）的 arguments
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


// ======================== Responses 原生透传（探测 + 记忆） ========================
//
// 有些模型的上游 /zen/v1/chat/completions 通道不可用，但原生
// /zen/v1/responses 端点正常（如 muse-spark-1.3-contributor）。
//
// 策略：网关默认仍走 chat 翻译路径；一旦翻译路径失败，自动把客户端的
// Responses 请求原样透传到上游原生 responses 端点。透传成功后在内存中
// 记住该模型（key 为解析后的上游模型 ID），下次直接透传、不再转换。
// 记忆只存内存，进程重启后失效（重新探测即可，静态预置模型除外）。
//
// 职责分离：
//   - 模型注册表（is/remember/mark + 静态预置 + 故障剔除）：只管“走哪条路”；
//   - probeNativeResponses：chat 失败后的投机探测，成功才写回并记忆；
//   - forwardNativeResponses：已确认模型的保真反向代理，原样透传上游状态码；
//   - relayResponsesToClient：统一的流式/非流式写回（实时 Flush、Usage 统计、
//     响应头过滤、会话状态保存）。

var nativeResponsesModels = struct {
	sync.RWMutex
	ids map[string]bool
}{ids: map[string]bool{}}

// defaultNativeResponsesModels 静态预置已知仅支持原生 responses 端点的模型，
// 免除冷启动时“先失败重试多次再探测”的惩罚。静态模型不会被故障剔除。
var defaultNativeResponsesModels = map[string]bool{
	"muse-spark-1.3-contributor": true,
}

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
func isNativeResponsesModel(modelID string) bool {
	if modelID == "" {
		return false
	}
	nativeResponsesModels.RLock()
	defer nativeResponsesModels.RUnlock()
	return nativeResponsesModels.ids[modelID]
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
// 必须按原有逻辑返回，不做透传。传输层错误（无 HTTP 状态）同样不探测。
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

// probeNativeResponses 专用于 chat 翻译路径失败后的投机探测：仅当上游原生
// responses 返回 2xx 时才把响应写回客户端并记住该模型；任何失败都返回 false
// 且不写任何响应，调用方保留原翻译路径的错误原样返回。
// sanitizeResponsesPassthroughBody 对原生透传体做 lenient 归一化，避免上游
// 严格校验 400（如 required 缺 key、reasoning.effort 非法），绝不因不支持返回 400。
// 合法请求归一化后等价（幂等），可安全用于保真透传。
func sanitizeResponsesPassthroughBody(rawBody []byte, modelID string) []byte {
	if !isMuseSparkModel(modelID) {
		return rawBody
	}
	var body map[string]any
	if err := json.Unmarshal(rawBody, &body); err != nil {
		return rawBody
	}
	changed := false
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
		if e, _ := r["effort"].(string); e != "" {
			if ne := normalizeResponsesEffort(e); ne != e {
				if ne == "" {
					delete(body, "reasoning")
				} else {
					r["effort"] = ne
					body["reasoning"] = r
				}
				changed = true
			}
		}
	}
	if !changed {
		return rawBody
	}
	if b, err := json.Marshal(body); err == nil {
		return b
	}
	return rawBody
}

func probeNativeResponses(ctx context.Context, w http.ResponseWriter, auth UpstreamAuth, modelID string, rawBody []byte, stream bool, req ResponsesAPIRequest) bool {
	rawBody = sanitizeResponsesPassthroughBody(rawBody, modelID)
	rc, status, header, err := callOpenCodeEndpoint(ctx, "responses", rawBody, modelID, auth)
	if err != nil || status < 200 || status >= 300 {
		if rc != nil {
			rc.Close()
		}
		return false
	}
	defer rc.Close()

	rememberNativeResponsesModel(modelID)
	reqLogger(ctx).Info("responses_probe_succeeded", "model", modelID, "stream", stream)

	relayResponsesToClient(ctx, w, rc, status, header, modelID, stream, req)
	return true
}

// forwardNativeResponses 处理已确认原生模型的标准反向代理转发：无论上游返回
// 2xx、4xx 还是 5xx，均保真透传状态码与错误信息（符合标准代理语义）。仅在
// 真正的传输层错误（无法拿到上游响应）时返回 false，调用方兜底写 502。
func forwardNativeResponses(ctx context.Context, w http.ResponseWriter, auth UpstreamAuth, modelID string, rawBody []byte, stream bool, req ResponsesAPIRequest) bool {
	rawBody = sanitizeResponsesPassthroughBody(rawBody, modelID)
	rc, status, header, err := callOpenCodeEndpoint(ctx, "responses", rawBody, modelID, auth)
	if err != nil {
		markNativeResponsesFailure(modelID)
		return false
	}
	defer rc.Close()

	if status >= 500 {
		markNativeResponsesFailure(modelID)
	}

	relayResponsesToClient(ctx, w, rc, status, header, modelID, stream, req)
	return true
}

// passthroughNativeResponses 兼容别名：等价于投机探测（成功才写回并记忆）。
// 已确认模型的转发请使用 forwardNativeResponses（保真透传上游状态码）。
func passthroughNativeResponses(ctx context.Context, w http.ResponseWriter, auth UpstreamAuth, modelID string, rawBody []byte, stream bool) bool {
	var req ResponsesAPIRequest
	_ = json.Unmarshal(rawBody, &req)
	return probeNativeResponses(ctx, w, auth, modelID, rawBody, stream, req)
}

// relayResponsesToClient 统一负责流式与非流式的保真透传：过滤后的安全响应头、
// 流式实时 Flush、流/非流双路 Token 统计、成功响应的会话状态保存。
func relayResponsesToClient(ctx context.Context, w http.ResponseWriter, rc io.Reader, status int, header http.Header, modelID string, stream bool, req ResponsesAPIRequest) {
	// 拷贝上游安全响应头（X-RateLimit-* 等），客户端可见剩余额度与重置时间。
	for k, v := range filterResponseHeaders(header) {
		w.Header()[k] = v
	}

	if stream && status >= 200 && status < 300 {
		relayResponsesStream(ctx, w, rc, status, modelID, req)
		return
	}

	respBody, err := io.ReadAll(io.LimitReader(rc, 32*1024*1024))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"failed to read upstream body"}}`))
		return
	}

	// 非流式成功响应：仅 muse-spark 归一化 function_call 参数中的整数浮点
	//（如 1000.0->1000），避免 Codex 等严格客户端反序列化失败。字符串内数字不动。
	if status >= 200 && status < 300 && isMuseSparkModel(modelID) {
		var tmp map[string]any
		if json.Unmarshal(respBody, &tmp) == nil {
			if output, ok := tmp["output"].([]any); ok {
				if normalizeResponseOutputArguments(output) {
					if nb, merr := json.Marshal(tmp); merr == nil {
						respBody = nb
					}
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

// relayResponsesStream 逐行透传 SSE 并在每个事件行后 Flush，保证打字机效果；
// 同时从 response.completed / usage 事件中提取 usage 做 Token 统计，并保存
// 完整响应对象以维持 previous_response_id 会话链条。
func relayResponsesStream(ctx context.Context, w http.ResponseWriter, rc io.Reader, status int, modelID string, req ResponsesAPIRequest) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(status)

	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	_ = ctx
	reader := bufio.NewReader(rc)
	var lastUsage map[string]any
	var lastResponse map[string]any
	// sawData：是否转发过至少一条 data 行；doneSeen：上游是否已发送
	// `data: [DONE]` 哨兵；writeFailed：客户端已断开，无需再补写。
	sawData := false
	doneSeen := false
	writeFailed := false

	argStates := map[int]*argsNormState{}
	argItemToOutput := map[string]int{}
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			outLine := line
			// 流式参数归一化（仅 muse-spark）：只处理 arguments 增量与 completed，
			// output_text 等文本增量绝不动（避免改写 echo 1.0 等可见输出）。
			if isMuseSparkModel(modelID) {
				if normalized, ok := normalizeResponsesStreamLine(line, argStates, argItemToOutput); ok {
					outLine = normalized
				}
			}
			if _, werr := w.Write(outLine); werr != nil {
				writeFailed = true
				break
			}
			// 逐行 Flush：标准 SSE 用空行分隔事件，非标准单换行输出也能
			// 被及时推送，避免 io.Copy 式 32KB 缓冲卡死打字机效果。
			if flusher != nil {
				flusher.Flush()
			}
			trimmed := bytes.TrimSpace(outLine)
			if bytes.Equal(trimmed, []byte("data: [DONE]")) || bytes.Equal(trimmed, []byte("[DONE]")) {
				doneSeen = true
			}
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				sawData = true
			}
			if usage, response := extractStreamEventUsage(outLine); usage != nil || response != nil {
				if usage != nil {
					lastUsage = usage
				}
				if response != nil {
					lastResponse = response
				}
			}
		}
		if err != nil {
			break
		}
	}
	if flusher != nil {
		flusher.Flush()
	}

	if lastUsage != nil {
		recordResponsesUsage(modelID, lastUsage)
	}
	if lastResponse != nil {
		if _, hasID := lastResponse["id"].(string); hasID {
			storeResponseState(lastResponse, req)
		}
	}

	// 部分上游（如 muse-spark 系，见 responses.go 对 muse-spark 的容错注释）
	// 在原生 responses 流结束时只发事件不发 `data: [DONE]` 哨兵，期望该哨兵
	// 的客户端会报 "SSE stream ended without [DONE]"。在干净 EOF、已转发过
	// 事件且上游未发送哨兵时补发一行：以 response.completed 判定结束的客户端
	// 不会读到这行，以 [DONE] 为终止标志的客户端借此正常退出读循环。
	// 客户端断开或零事件空流不补发（空流补发会掩盖上游异常）。
	if !writeFailed && sawData && !doneSeen {
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// normalizeResponsesStreamLine 归一化单行 SSE data 事件中的 function_call 参数，
// 返回归一化后的完整行与是否改动。output_text 类文本增量永不动。
func normalizeResponsesStreamLine(line []byte, argStates map[int]*argsNormState, argItemToOutput map[string]int) ([]byte, bool) {
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
	it, _ := u["input_tokens"].(float64)
	ot, _ := u["output_tokens"].(float64)
	tt, _ := u["total_tokens"].(float64)
	if tt <= 0 {
		return
	}
	recordTokenUsage(modelID, int64(it), int64(ot), int64(tt))
	recordCacheUsage(modelID, u)
}
