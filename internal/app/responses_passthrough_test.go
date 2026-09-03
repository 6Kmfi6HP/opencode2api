package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 翻译路径失败时自动透传上游原生 responses，成功后记住该模型，
// 下次直接透传不再转换。
func TestResponsesPassthroughFallbackAndMemory(t *testing.T) {
	const probeModel = "passthrough-probe-model-xyz"

	// 隔离全局状态：清空别名规则、清理记忆。
	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })
	nativeResponsesModels.Lock()
	delete(nativeResponsesModels.ids, probeModel)
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, probeModel)
		nativeResponsesModels.Unlock()
	})

	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusInternalServerError, body: `{"type":"error"}`},
		{status: http.StatusInternalServerError, body: `{"type":"error"}`},
		{status: http.StatusInternalServerError, body: `{"type":"error"}`},
		{status: http.StatusOK, body: `{"id":"resp_probe1","object":"response","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12}}`},
		{status: http.StatusOK, body: `{"id":"resp_probe2","object":"response","status":"completed","output":[]}`},
	})

	doRequest := func() (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses",
			strings.NewReader(`{"model":"`+probeModel+`","input":"hi","stream":false}`))
		rec := httptest.NewRecorder()
		responsesHandler(rec, req)
		return rec.Code, rec.Body.String()
	}

	// 第一次：chat 翻译路径 500 x3 后回退透传成功。
	code, body := doRequest()
	if code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200, body = %s", code, body)
	}
	if !strings.Contains(body, "resp_probe1") {
		t.Fatalf("first request body = %s, want passthrough response", body)
	}
	if !isNativeResponsesModel(probeModel) {
		t.Fatalf("model %q not remembered after successful passthrough", probeModel)
	}
	if len(transport.requestedURLs) != 4 {
		t.Fatalf("requested URLs after first request = %#v, want 3 chat + 1 responses", transport.requestedURLs)
	}
	for _, u := range transport.requestedURLs[:3] {
		if !strings.HasSuffix(u, "/zen/v1/chat/completions") {
			t.Fatalf("unexpected translated attempt URL = %s", u)
		}
	}
	if !strings.HasSuffix(transport.requestedURLs[3], "/zen/v1/responses") {
		t.Fatalf("passthrough URL = %s, want suffix /zen/v1/responses", transport.requestedURLs[3])
	}

	// 第二次：已记住，直接透传，不再走 chat 翻译。
	code, body = doRequest()
	if code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200, body = %s", code, body)
	}
	if !strings.Contains(body, "resp_probe2") {
		t.Fatalf("second request body = %s, want passthrough response", body)
	}
	if len(transport.requestedURLs) != 5 {
		t.Fatalf("requested URLs after second request = %#v, want exactly one more passthrough", transport.requestedURLs)
	}
	if !strings.HasSuffix(transport.requestedURLs[4], "/zen/v1/responses") {
		t.Fatalf("second passthrough URL = %s, want suffix /zen/v1/responses", transport.requestedURLs[4])
	}
}

// 上游 200 但包体是类型化错误（Anthropic error 格式）时，必须原样返回
// 错误信息，不得透传探测（fake 只准备了一个响应，透传会直接 Fatal）。
func TestResponsesPassthroughSkipsTypedConversionError(t *testing.T) {
	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   `{"type":"error","error":{"type":"overloaded_error","message":"Server overloaded"}}`,
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"primary-model","input":[]}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil {
		t.Fatal("error object missing")
	}
	if errObj["type"] != "overloaded_error" || errObj["message"] != "Server overloaded" {
		t.Fatalf("error object = %v, want overloaded_error/Server overloaded", errObj)
	}
	if isNativeResponsesModel("primary-model") {
		t.Fatal("typed conversion error must not be remembered for passthrough")
	}
}
