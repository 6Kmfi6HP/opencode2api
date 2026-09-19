package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSystemoneHandler_ForwardsFreeModel 免费模型直通：不做指纹重做
// （bodyMap 里不出现 tools/stream/stream_options——applyFreeTierFingerprint
// 只套 chat/completions|messages|responses 三个上游子路径），body 除 model
// 外原样，2xx body 与状态码回写。
func TestSystemoneHandler_ForwardsFreeModel(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   `{"model":"jev-1.13-free","answers":{"is_spam":{"type":"noul","noul":0.06}},"usage":{"input_tokens":275,"output_tokens":20},"cost":"0"}`,
	}})

	req := httptest.NewRequest(http.MethodPost, "/v1/systemone",
		strings.NewReader(`{"state":{"chat":"hello"},"model":"jev-1.13-free","questions":{"is_spam":{"type":"noul","instructions":"spam?"}}}`))
	rec := httptest.NewRecorder()
	systemoneHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if answers, ok := resp["answers"].(map[string]any); !ok || answers["is_spam"] == nil {
		t.Fatalf("answers missing: %v", resp)
	}

	if len(transport.requestedURLs) != 1 || !strings.HasSuffix(transport.requestedURLs[0], "/zen/v1/systemone") {
		t.Fatalf("URLs = %#v, want single /zen/v1/systemone call", transport.requestedURLs)
	}
	payload := transport.requestPayloads[0]
	if payload["model"] != "jev-1.13-free" {
		t.Fatalf("model = %v", payload["model"])
	}
	// 指纹重做不应作用于 systemone 子路径。
	if _, ok := payload["tools"]; ok {
		t.Fatalf("tools leaked into systemone body: %v", payload["tools"])
	}
	if _, ok := payload["stream"]; ok {
		t.Fatalf("stream leaked into systemone body: %v", payload["stream"])
	}
	if _, ok := payload["stream_options"]; ok {
		t.Fatalf("stream_options leaked into systemone body: %v", payload["stream_options"])
	}
	// state 按原样透传（结构化对象，不是字符串）。
	if state, ok := payload["state"].(map[string]any); !ok || state["chat"] != "hello" {
		t.Fatalf("state = %v", payload["state"])
	}
	// questions 原样透传。
	if qs, ok := payload["questions"].(map[string]any); !ok || qs["is_spam"] == nil {
		t.Fatalf("questions = %v", payload["questions"])
	}
}

// TestSystemoneHandler_MissingModel 缺 model 直接 400（上游对缺 model 会回
// "Model  is not supported" 且我们无法做别名/免费判定）。
func TestSystemoneHandler_MissingModel(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{})

	req := httptest.NewRequest(http.MethodPost, "/v1/systemone",
		strings.NewReader(`{"state":"hi","questions":{}}`))
	rec := httptest.NewRecorder()
	systemoneHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
}

// TestSystemoneHandler_InvalidJSON 非 JSON body 400。
func TestSystemoneHandler_InvalidJSON(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{})

	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(`not-json`))
	rec := httptest.NewRecorder()
	systemoneHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestSystemoneHandler_ForwardsUpstreamError 上游非 2xx 原样回写状态码与
// body——错误细节（pydantic detail 数组 / type:error 包裹）交由上游协议
// 定义，网关不翻译。
func TestSystemoneHandler_ForwardsUpstreamError(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusInternalServerError,
		body:   `{"type":"error","error":{"type":"ModelError","message":"Model  is not supported"}}`,
	}})

	req := httptest.NewRequest(http.MethodPost, "/v1/systemone",
		strings.NewReader(`{"state":"hi","model":"jev-1.13-free","questions":{"q":{"type":"noul","instructions":"?"}}}`))
	rec := httptest.NewRecorder()
	systemoneHandler(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not supported") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if len(transport.requestedURLs) != 1 {
		t.Fatalf("requests = %d, want 1", len(transport.requestedURLs))
	}
}

// TestSystemoneHandler_PaidModelPassthrough 非免费 model 原样发给上游，
// 不做别名改写，也不做免费档判定——上游自行拒绝或受理。
func TestSystemoneHandler_PaidModelPassthrough(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusInternalServerError,
		body:   `{"error":{"type":"server_error","message":"Error from provider (Console): Upstream request failed: Rate-limited Zen models require a workspace"}}`,
	}})

	req := httptest.NewRequest(http.MethodPost, "/v1/systemone",
		strings.NewReader(`{"state":"hi","model":"jev-1.13","questions":{"q":{"type":"noul","instructions":"?"}}}`))
	rec := httptest.NewRecorder()
	systemoneHandler(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := transport.requestPayloads[0]["model"]; got != "jev-1.13" {
		t.Fatalf("model = %v, want jev-1.13", got)
	}
}
