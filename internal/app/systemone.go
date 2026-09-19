package app

import (
	"encoding/json"
	"io"
	"net/http"
)

// systemOneMaxBody 协议计费按 body 走，上限对齐 chat 路径的 10 MiB。
const systemOneMaxBody = 10 * 1024 * 1024

// systemoneHandler serves POST /v1/systemone: TypeSafe System One 协议
// （state + typed questions → structured answers）直通上游
// /zen/v1/systemone。jev-1.13 这类 System One 模型不做文本生成，无法用
// OpenAI Chat / Responses / Anthropic Messages 任一协议驱动，因此单独开
// 入站端点，body 除 model 外原样透传（state 允许 string/object/array；
// questions 的协议细节由上游校验，网关不复制）。
//
// 行为：
//   - model 缺失/空 → 400（上游对缺 model 返回 500 `Model  is not
//     supported`，空 model 又无法做别名/免费档判定，这里提前拦掉）
//   - 免费模型（-free 后缀或 models.dev 零费用入册，如 jev-1.13-free）
//     不套用免费层指纹重做：该门禁只套 chat/completions / messages /
//     responses 三个上游子路径（applyFreeTierFingerprint 内建跳过
//     systemone）；实测 /zen/v1/systemone 带默认请求体即可通过
//   - 非免费 model 直通（上游自行判定档位/限流，如 jev-1.13 当前返回
//     "Rate-limited Zen models require a workspace"）
//   - 上游任意状态码与 body 原样回写
func systemoneHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	auth := extractUpstreamAuth(r)
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, systemOneMaxBody))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	var bodyMap map[string]any
	if err := json.Unmarshal(body, &bodyMap); err != nil || bodyMap == nil {
		writeSystemOne400(w, "invalid JSON body")
		return
	}
	model, _ := bodyMap["model"].(string)
	if model == "" {
		writeSystemOne400(w, "model is required")
		return
	}
	resolvedModel := mapPublicToFreeModel(auth, resolveModelForAuth(auth, model))

	// 复用统一的 (baseURL, 代理客户端) 选择：socks5 轮询 / 多域名 sticky /
	// paid 直连等策略与其它上游路径一致。
	baseURL, client := selectUpstreamTarget(auth, bodyMap, nil, ocSessionID)
	upReq, err := buildOCRequestWithSubpath(resolvedModel, bodyMap, auth, false, baseURL, "systemone", ocSessionID)
	if err != nil {
		http.Error(w, "Failed to build upstream request", http.StatusInternalServerError)
		return
	}
	resp, err := client.Do(upReq.WithContext(r.Context()))
	if err != nil {
		http.Error(w, "Upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, systemOneMaxBody))
	if err != nil {
		http.Error(w, "Failed to read upstream response", http.StatusBadGateway)
		return
	}
	// 上游正常响应用 application/json；校验失败（pydantic detail 数组）
	// 与错误（type:error 包裹）也都是 JSON。统一按 JSON 回写。
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

func writeSystemOne400(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    "invalid_request_error",
			"message": msg,
		},
	})
}
