package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
)

// ======================== Responses 原生透传（探测 + 记忆） ========================
//
// 有些模型的上游 /zen/v1/chat/completions 通道不可用，但原生
// /zen/v1/responses 端点正常（如 muse-spark-1.3-contributor）。
//
// 策略：网关默认仍走 chat 翻译路径；一旦翻译路径失败，自动把客户端的
// Responses 请求原样透传到上游原生 responses 端点。透传成功后在内存中
// 记住该模型（key 为解析后的上游模型 ID），下次直接透传、不再转换。
// 记忆只存内存，进程重启后失效（重新探测即可）。

var nativeResponsesModels = struct {
	sync.RWMutex
	ids map[string]bool
}{ids: map[string]bool{}}

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
}

// shouldProbeNativeResponses 决定翻译路径失败后是否值得探测上游原生
// responses：传输错误与上游 4xx/5xx 值得探测；但上游 200 包体仅是本地
// 转换失败的类型化错误（*anthropicProtocolError，如 overloaded_error）
// 带有明确错误信息，必须按原有逻辑返回，不做透传。
func shouldProbeNativeResponses(status int, err error) bool {
	if err == nil {
		return status < 200 || status >= 300
	}
	var ape *anthropicProtocolError
	if errors.As(err, &ape) {
		return false
	}
	return true
}

// passthroughNativeResponses 把客户端的 Responses 请求体原样转发到上游原生
// /zen/v1/responses 端点（仅把 model 替换为解析后的上游模型 ID），并把上游
// 响应原样写回客户端。成功返回 true（此时已记住该模型）；任何失败返回
// false，调用方继续走原有错误处理，不写任何响应。
func passthroughNativeResponses(ctx context.Context, w http.ResponseWriter, auth UpstreamAuth, modelID string, rawBody []byte, stream bool) bool {
	initOCSession()

	var bodyMap map[string]any
	if err := json.Unmarshal(rawBody, &bodyMap); err != nil {
		return false
	}
	bodyMap["model"] = modelID
	tryBody, err := json.Marshal(bodyMap)
	if err != nil {
		return false
	}

	useGo := auth.shouldUseGoEndpoint(modelID)
	surface := "zen"
	if useGo {
		surface = "go"
	}
	baseURL, client := selectUpstreamTarget(auth, bodyMap)
	upstreamURL := baseURL + "/zen/v1/responses"
	if useGo {
		upstreamURL = "https://opencode.ai/zen/go/v1/responses"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(tryBody))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth.authorizationHeader())
	req.Header.Set("User-Agent", fmt.Sprintf("opencode/%s", ocClientVer))
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-project", ocProjectID)
	req.Header.Set("x-opencode-session", ocSessionID)
	req.Header.Set("x-opencode-request", "req_"+randomString(24))
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("responses passthrough transport error", "model", modelID, "base_url", baseURL, "surface", surface, "error", err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		slog.Warn("responses passthrough upstream error", "model", modelID, "base_url", baseURL, "surface", surface, "status", resp.StatusCode, "body", string(errBody))
		return false
	}

	// 上游 2xx：记住该模型，下次直接透传。
	rememberNativeResponsesModel(modelID)
	slog.Info("responses_passthrough",
		"model", modelID,
		"base_url", baseURL,
		"surface", surface,
		"stream", stream,
		"status", resp.StatusCode,
	)

	if stream {
		// SSE 原样透传。
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.Copy(w, resp.Body)
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		return false
	}
	// Responses 协议 usage 口径：input/output/total_tokens。
	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			it, _ := u["input_tokens"].(float64)
			ot, _ := u["output_tokens"].(float64)
			tt, _ := u["total_tokens"].(float64)
			if tt > 0 {
				recordTokenUsage(modelID, int64(it), int64(ot), int64(tt))
				recordCacheUsage(modelID, u)
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBody)
	return true
}
