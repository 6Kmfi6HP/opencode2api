package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/6Kmfi6HP/opencode2api/internal/logging"
)

// ======================== Claude Messages count_tokens ========================
// POST /v1/messages/count_tokens 默认是本地 chars/4 启发式估算，不走上游、
// 不产生 usage；Claude Code 用它做上下文窗口管理与自动压缩，合理估计即可
// （见 docs/claude-messages-compatibility-report.md）。protocol_rules 命中
// anthropic 上游时改为直连 /zen/.../messages/count_tokens 透传取精确计数：
// 上游 2xx 原样转回，任何失败/非 2xx 一律静默回落本地启发式。

const (
	// Content tokens: ~4 chars per token, matching the common BPE
	// compression for English.
	charsPerToken = 4
	// Structural overhead per message, system block, and tool definition.
	messageOverhead = 4
	systemOverhead  = 4
	toolOverhead    = 8
	// Fixed estimates for multimodal blocks (Anthropic's documented
	// approximation for images; flat fallback for documents).
	imageTokens    = 1600
	documentTokens = 3000
)

// estimateClaudeSystemTokens 估算顶层 system（string 或 block 数组）占用；
// block 数组递归走 estimateContentTokens，cache_control/block 元数据经
// jsonString 兜底计入。
func estimateClaudeSystemTokens(system any) int {
	switch v := system.(type) {
	case nil:
		return 0
	case string:
		if v == "" {
			return 0
		}
		return systemOverhead + estimateTextTokens(v)
	case []any:
		if len(v) == 0 {
			return 0
		}
		return systemOverhead + estimateContentTokens(v)
	default:
		return systemOverhead + estimateTextTokens(jsonString(v))
	}
}

// estimateClaudeInputTokens returns a heuristic count of the input tokens a
// Claude Messages request would consume. It reads req without mutating it.
func estimateClaudeInputTokens(req ClaudeRequest) int {
	total := estimateClaudeSystemTokens(req.System)
	for _, msg := range req.Messages {
		total += messageOverhead
		total += estimateContentTokens(msg.Content)
	}
	for _, tool := range req.Tools {
		total += toolOverhead
		if tool.Description != "" {
			total += estimateTextTokens(tool.Description)
		}
		if b, err := json.Marshal(tool.InputSchema); err == nil {
			total += estimateTextTokens(string(b))
		}
	}
	if total <= 0 {
		return 1 // never return zero: a count of 0 would confuse the client
	}
	return total
}

// estimateContentTokens estimates the tokens in a single Anthropic content
// field, which may be a plain string, a block array, or an arbitrary JSON
// value.
func estimateContentTokens(content any) int {
	switch c := content.(type) {
	case string:
		return estimateTextTokens(c)
	case []any:
		total := 0
		for _, item := range c {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := block["type"].(string)
			switch blockType {
			case "text":
				if text, ok := block["text"].(string); ok {
					total += estimateTextTokens(text)
				}
			case "image":
				total += imageTokens
			case "document":
				total += documentTokens
			case "thinking", "redacted_thinking":
				if text, ok := block["thinking"].(string); ok {
					total += estimateTextTokens(text)
				}
				if data, ok := block["data"].(string); ok {
					total += estimateTextTokens(data)
				}
			case "tool_use":
				if name, ok := block["name"].(string); ok {
					total += estimateTextTokens(name)
				}
				if input := block["input"]; input != nil {
					total += estimateTextTokens(jsonString(input))
				}
			case "tool_result":
				if content := block["content"]; content != nil {
					total += estimateContentTokens(content)
				}
				if isErr, _ := block["is_error"].(bool); isErr {
					total += messageOverhead
				}
			default:
				// Unknown block: count its raw JSON to stay safe.
				total += estimateTextTokens(jsonString(block))
			}
		}
		return total
	default:
		if c == nil {
			return 0
		}
		return estimateTextTokens(jsonString(c))
	}
}

// jsonString serializes any value to a JSON string, falling back to "" on
// error. Used only for token estimation.
func jsonString(v any) string {
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return ""
}

// estimateTextTokens counts runes rather than bytes so multibyte text is not
// overcounted; rounds up so a non-empty short string always contributes 1.
func estimateTextTokens(s string) int {
	if s == "" {
		return 0
	}
	n := len([]rune(s))
	tokens := n / charsPerToken
	if n%charsPerToken != 0 {
		tokens++
	}
	return tokens
}

// claudeCountTokensHandler serves POST /v1/messages/count_tokens:protocol_rules
// 命中 anthropic 上游时直连 /zen/.../messages/count_tokens 透传（model 改写为
// 解析后的上游 ID,max_tokens 收敛 [128, cap],不带 stream——计数与输出预算
// 或流无关）,2xx 原样回写（Anthropic 端点本就只回 input_tokens）;传输错误/
// 上游非 2xx 一律回落本地启发式。
func claudeCountTokensHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	auth := extractUpstreamAuth(r)
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	var claudeReq ClaudeRequest
	if err := json.Unmarshal(body, &claudeReq); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    "invalid_request_error",
				"message": "Invalid JSON",
			},
		})
		return
	}
	if claudeReq.Model == "" {
		writeProtocolValidation400(w, "claude", "", "model is required")
		return
	}
	resolvedModel := mapPublicToFreeModel(auth, resolveModelForAuth(auth, claudeReq.Model))

	if proto, matched := matchProtocolRule(resolvedModel); matched && proto == upstreamProtocolAnthropic {
		if status, respBody, ok := forwardCountTokensViaAnthropic(r.Context(), claudeReq, auth, resolvedModel); ok {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(status)
			w.Write(respBody)
			return
		}
	}

	inputTokens := estimateClaudeInputTokens(claudeReq)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]int{"input_tokens": inputTokens})
}

// forwardCountTokensViaAnthropic 把 count_tokens 请求直通上游原生 Anthropic
// count_tokens 端点。透传体只保 max_tokens 一项受限：以原请求为基准走
// clampAnthropicProtocolMaxTokens 收敛([128, cap]),不额外收紧,即"最多保持
// 原预算"。返回 (status, 直通体, 是否已成功写回)—— false 时调用方回落本地
// 启发式。callCountTokensUpstream 是变量以便测试注入传输层错误。
func forwardCountTokensViaAnthropic(ctx context.Context, claudeReq ClaudeRequest, auth UpstreamAuth, resolvedModel string) (int, []byte, bool) {
	log := logging.FromContext(ctx)
	var bodyMap map[string]any
	reqBody, err := json.Marshal(claudeReq)
	if err == nil {
		err = json.Unmarshal(reqBody, &bodyMap)
	}
	if err != nil || bodyMap == nil {
		log.Warn("count_tokens upstream forward skipped: marshal error", "model", resolvedModel, "error", err)
		return 0, nil, false
	}
	bodyMap["model"] = resolvedModel
	clampAnthropicProtocolMaxTokens(bodyMap, resolvedModel)
	forwardBytes, err := json.Marshal(bodyMap)
	if err == nil {
		reqBody = forwardBytes
	}

	rc, status, _, callErr := callCountTokensUpstream(ctx, reqBody, resolvedModel, auth)
	if callErr != nil {
		if rc != nil {
			rc.Close()
		}
		log.Warn("count_tokens upstream transport error; falling back to heuristic",
			"model", resolvedModel, "error", callErr)
		return 0, nil, false
	}
	defer rc.Close()
	respBody, readErr := io.ReadAll(io.LimitReader(rc, 32*1024*1024))
	if readErr != nil || status < 200 || status >= 300 {
		if readErr == nil {
			logging.UpstreamError(ctx, resolvedModel, status, respBody, roundRobinBaseURL())
		} else {
			log.Warn("count_tokens upstream read error; falling back to heuristic",
				"model", resolvedModel, "status", status, "error", readErr)
		}
		return status, respBody, false
	}
	return status, respBody, true
}

// callCountTokensUpstream 实际发起 /zen/.../messages/count_tokens 调用;抽成
// 变量便于测试注入传输层错误（读模型 ID 用的 transport 形态不走 URL 断言）。
var callCountTokensUpstream = func(ctx context.Context, body []byte, modelID string, auth UpstreamAuth) (io.ReadCloser, int, http.Header, error) {
	return callOpenCodeEndpoint(ctx, "messages/count_tokens", body, modelID, auth)
}
