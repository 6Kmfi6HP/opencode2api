package app

import (
	"encoding/json"
	"errors"
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

// 验证 passthrough 探测过程中遇到 5xx 可重试错误时会进行重试与轮换。
func TestResponsesPassthroughProbeRetriesAndRotates(t *testing.T) {
	const probeModel = "probe-retry-model"

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

	// 前 3 次: chat 翻译路径 500 x 3
	// 第 4 次: responses 探测第一次返回 503
	// 第 5 次: responses 探测重试返回 200 成功
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusInternalServerError, body: `{"type":"error"}`},
		{status: http.StatusInternalServerError, body: `{"type":"error"}`},
		{status: http.StatusInternalServerError, body: `{"type":"error"}`},
		{status: http.StatusServiceUnavailable, body: `{"type":"error"}`},
		{status: http.StatusOK, body: `{"id":"resp_retry_ok","object":"response","status":"completed","output":[]}`},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"`+probeModel+`","input":"hi","stream":false}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "resp_retry_ok") {
		t.Fatalf("body = %s, want resp_retry_ok", rec.Body.String())
	}
	if !isNativeResponsesModel(probeModel) {
		t.Fatalf("model %q should be remembered", probeModel)
	}
	if len(transport.requestedURLs) != 5 {
		t.Fatalf("requested URLs len = %d, want 5 (3 chat + 2 responses)", len(transport.requestedURLs))
	}
	if !strings.HasSuffix(transport.requestedURLs[3], "/zen/v1/responses") || !strings.HasSuffix(transport.requestedURLs[4], "/zen/v1/responses") {
		t.Fatalf("calls 3 and 4 should be /zen/v1/responses, got %v", transport.requestedURLs[3:])
	}
	if transport.closeIdleCalls < 1 {
		t.Fatalf("closeIdleCalls = %d, want >= 1 (rotation occurred)", transport.closeIdleCalls)
	}
}

// 验证已记住的模型在透传时遇到 429 会重试轮换并成功。
func TestResponsesPassthroughRememberedModelRetriesOn429(t *testing.T) {
	const rememberedModel = "remembered-429-model"

	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })
	nativeResponsesModels.Lock()
	nativeResponsesModels.ids[rememberedModel] = true
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, rememberedModel)
		nativeResponsesModels.Unlock()
	})

	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusTooManyRequests, body: `{"error":"rate limited"}`},
		{status: http.StatusOK, body: `{"id":"resp_429_recovered","object":"response","status":"completed","output":[]}`},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"`+rememberedModel+`","input":"hi","stream":false}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "resp_429_recovered") {
		t.Fatalf("body = %s, want resp_429_recovered", rec.Body.String())
	}
	if len(transport.requestedURLs) != 2 {
		t.Fatalf("requested URLs len = %d, want 2", len(transport.requestedURLs))
	}
	for _, u := range transport.requestedURLs {
		if !strings.HasSuffix(u, "/zen/v1/responses") {
			t.Fatalf("expected responses URL, got %s", u)
		}
	}
}

// 验证已记住的模型在透传遇到传输网络错误 (transport error) 时会重试并成功。
func TestResponsesPassthroughTransportErrorRetry(t *testing.T) {
	const rememberedModel = "remembered-transport-err-model"

	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })
	nativeResponsesModels.Lock()
	nativeResponsesModels.ids[rememberedModel] = true
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, rememberedModel)
		nativeResponsesModels.Unlock()
	})

	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{err: errors.New("connection reset by peer")},
		{status: http.StatusOK, body: `{"id":"resp_transport_recovered","object":"response","status":"completed","output":[]}`},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"`+rememberedModel+`","input":"hi","stream":false}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "resp_transport_recovered") {
		t.Fatalf("body = %s, want resp_transport_recovered", rec.Body.String())
	}
	if len(transport.requestedURLs) != 2 {
		t.Fatalf("requested URLs len = %d, want 2", len(transport.requestedURLs))
	}
}

// 验证重试次数耗尽时返回 502。
func TestResponsesPassthroughExhaustedRetries(t *testing.T) {
	const rememberedModel = "remembered-exhausted-model"

	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })
	nativeResponsesModels.Lock()
	nativeResponsesModels.ids[rememberedModel] = true
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, rememberedModel)
		nativeResponsesModels.Unlock()
	})

	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusBadGateway, body: `{"type":"error"}`},
		{status: http.StatusBadGateway, body: `{"type":"error"}`},
		{status: http.StatusBadGateway, body: `{"type":"error"}`},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"`+rememberedModel+`","input":"hi","stream":false}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if len(transport.requestedURLs) != 3 {
		t.Fatalf("requested URLs len = %d, want 3 retries", len(transport.requestedURLs))
	}
}

// 验证不可重试的状态码（例如 404）单次失败后立即退出，不浪费重试配额。
// 已确认模型的转发是保真反向代理：上游状态码与错误体原样透传给客户端。
func TestResponsesPassthroughNonRetryableStatus(t *testing.T) {
	const rememberedModel = "remembered-404-model"

	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })
	nativeResponsesModels.Lock()
	nativeResponsesModels.ids[rememberedModel] = true
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, rememberedModel)
		nativeResponsesModels.Unlock()
	})

	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusNotFound, body: `{"error":"not found"}`},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"`+rememberedModel+`","input":"hi","stream":false}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (fidelity passthrough)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("body = %s, want upstream error body", rec.Body.String())
	}
	if len(transport.requestedURLs) != 1 {
		t.Fatalf("requested URLs len = %d, want exactly 1 attempt", len(transport.requestedURLs))
	}
}

// 验证流式请求 (stream: true) 在遇到错误时能正常重试轮换后成功返回 SSE。
func TestResponsesPassthroughStreamRetriesAndSucceeds(t *testing.T) {
	const streamModel = "remembered-stream-model"

	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })
	nativeResponsesModels.Lock()
	nativeResponsesModels.ids[streamModel] = true
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, streamModel)
		nativeResponsesModels.Unlock()
	})

	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusInternalServerError, body: `{"type":"error"}`},
		{status: http.StatusOK, body: "event: response.completed\ndata: {\"id\":\"resp_stream_ok\"}\n\n"},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"`+streamModel+`","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(rec.Body.String(), "resp_stream_ok") {
		t.Fatalf("body = %s, want resp_stream_ok", rec.Body.String())
	}
	if len(transport.requestedURLs) != 2 {
		t.Fatalf("requested URLs len = %d, want 2", len(transport.requestedURLs))
	}
}

// 验证配置多 upstreamBaseURLs 时，passthrough 重试会在不同 base 之间轮换。
func TestResponsesPassthroughMultiBaseRotation(t *testing.T) {
	const multiBaseModel = "multi-base-model"

	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })
	nativeResponsesModels.Lock()
	nativeResponsesModels.ids[multiBaseModel] = true
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, multiBaseModel)
		nativeResponsesModels.Unlock()
	})

	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusServiceUnavailable, body: `{"type":"error"}`},
		{status: http.StatusOK, body: `{"id":"resp_multibase_ok","object":"response","status":"completed","output":[]}`},
	})
	withBaseURLs(t, []string{"https://base-a.test", "https://base-b.test"})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"`+multiBaseModel+`","input":"hi","stream":false}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if len(transport.requestedURLs) != 2 {
		t.Fatalf("requested URLs len = %d, want 2", len(transport.requestedURLs))
	}
	url1 := transport.requestedURLs[0]
	url2 := transport.requestedURLs[1]
	if url1 == url2 {
		t.Fatalf("expected rotation across baseURLs, but both were %s", url1)
	}
	if (!strings.HasPrefix(url1, "https://base-a.test") && !strings.HasPrefix(url1, "https://base-b.test")) ||
		(!strings.HasPrefix(url2, "https://base-a.test") && !strings.HasPrefix(url2, "https://base-b.test")) {
		t.Fatalf("URLs should use configured bases: %s, %s", url1, url2)
	}
}

// 已确认模型的 400 业务错误必须保真透传（状态码 + 错误体），不得掩盖为 502，
// 且模型记忆不受影响（客户端参数错误不是模型故障）。
func TestResponsesPassthroughForwardsClientError(t *testing.T) {
	const rememberedModel = "remembered-400-model"

	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })
	nativeResponsesModels.Lock()
	nativeResponsesModels.ids[rememberedModel] = true
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, rememberedModel)
		nativeResponsesModels.Unlock()
	})

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusBadRequest, body: `{"error":{"message":"invalid temperature","type":"invalid_request_error"}}`},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"`+rememberedModel+`","input":"hi","stream":false}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (fidelity passthrough), body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid temperature") {
		t.Fatalf("body = %s, want upstream error detail", rec.Body.String())
	}
	if !isNativeResponsesModel(rememberedModel) {
		t.Fatal("client-side 400 must not evict the model from memory")
	}
}

// chat 翻译路径 429 时不得探测 responses（避免限流雪崩）：请求全部走 chat，
// 且不产生任何 /responses 调用。
func TestResponsesPassthroughNoProbeOnRateLimit(t *testing.T) {
	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })

	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusTooManyRequests, body: `{"error":"rate limited"}`},
		{status: http.StatusTooManyRequests, body: `{"error":"rate limited"}`},
		{status: http.StatusTooManyRequests, body: `{"error":"rate limited"}`},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"ratelimited-model","input":"hi","stream":false}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429, body = %s", rec.Code, rec.Body.String())
	}
	for _, u := range transport.requestedURLs {
		if strings.HasSuffix(u, "/zen/v1/responses") {
			t.Fatalf("must not probe responses on 429, urls = %#v", transport.requestedURLs)
		}
		if !strings.HasSuffix(u, "/zen/v1/chat/completions") {
			t.Fatalf("unexpected URL = %s", u)
		}
	}
	if isNativeResponsesModel("ratelimited-model") {
		t.Fatal("429 response must not be remembered for passthrough")
	}
}

// chat 翻译路径 401 时不得探测 responses（凭据失效必然同样失败）。
func TestResponsesPassthroughNoProbeOnUnauthorized(t *testing.T) {
	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })

	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusUnauthorized, body: `{"error":"bad credentials"}`},
		{status: http.StatusUnauthorized, body: `{"error":"bad credentials"}`},
		{status: http.StatusUnauthorized, body: `{"error":"bad credentials"}`},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"unauth-model","input":"hi","stream":false}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body = %s", rec.Code, rec.Body.String())
	}
	for _, u := range transport.requestedURLs {
		if strings.HasSuffix(u, "/zen/v1/responses") {
			t.Fatalf("must not probe responses on 401, urls = %#v", transport.requestedURLs)
		}
	}
	if isNativeResponsesModel("unauth-model") {
		t.Fatal("401 response must not be remembered for passthrough")
	}
}

// 流式透传必须实时 Flush（打字机效果）、原样 relay SSE，并记录尾部 usage。
func TestResponsesPassthroughStreamRecordsUsage(t *testing.T) {
	const streamModel = "remembered-stream-usage-model"

	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })
	nativeResponsesModels.Lock()
	nativeResponsesModels.ids[streamModel] = true
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, streamModel)
		nativeResponsesModels.Unlock()
	})
	tokenStatsMu.Lock()
	before := int64(0)
	if ms := tokenStats.Models[streamModel]; ms != nil {
		before = ms.TotalTokens
	}
	tokenStatsMu.Unlock()
	t.Cleanup(func() {
		tokenStatsMu.Lock()
		delete(tokenStats.Models, streamModel)
		tokenStatsMu.Unlock()
	})

	sse := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream_usage\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n\n"
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: sse},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"`+streamModel+`","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if !rec.Flushed {
		t.Fatal("streaming relay must flush to the client")
	}
	// 上游未发 [DONE] 哨兵时，relay 会补发一行 data: [DONE]（见
	// relayResponsesStream 尾部注释），因此期望 body = sse + 哨兵。
	if rec.Body.String() != sse+"data: [DONE]\n\n" {
		t.Fatalf("stream body = %q, want verbatim relay + trailing [DONE]", rec.Body.String())
	}
	tokenStatsMu.Lock()
	after := int64(0)
	if ms := tokenStats.Models[streamModel]; ms != nil {
		after = ms.TotalTokens
	}
	tokenStatsMu.Unlock()
	if after-before != 15 {
		t.Fatalf("streamed usage delta = %d, want 15", after-before)
	}
	if _, ok := loadResponseState("resp_stream_usage"); !ok {
		t.Fatal("streamed completed response state not stored")
	}
}

// 上游原生 responses 流不发 [DONE] 哨兵时（muse-spark 系行为，见
// responses.go 容错注释），relay 必须在干净 EOF 后补发 data: [DONE]，
// 否则期望哨兵的客户端报 "SSE stream ended without [DONE]"。
func TestResponsesPassthroughStreamAppendsMissingDone(t *testing.T) {
	const model = "no-done-sentinel-model"

	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })
	nativeResponsesModels.Lock()
	nativeResponsesModels.ids[model] = true
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, model)
		nativeResponsesModels.Unlock()
	})

	sse := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_no_done\",\"status\":\"completed\"}}\n\n"
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: sse},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"`+model+`","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	want := sse + "data: [DONE]\n\n"
	if rec.Body.String() != want {
		t.Fatalf("stream body = %q, want %q", rec.Body.String(), want)
	}
}

// 上游已发送 [DONE] 哨兵时，relay 必须原样透传且不得重复补发。
func TestResponsesPassthroughStreamKeepsExistingDone(t *testing.T) {
	const model = "with-done-sentinel-model"

	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })
	nativeResponsesModels.Lock()
	nativeResponsesModels.ids[model] = true
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, model)
		nativeResponsesModels.Unlock()
	})

	sse := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_with_done\",\"status\":\"completed\"}}\n\n" +
		"data: [DONE]\n\n"
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: sse},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"`+model+`","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != sse {
		t.Fatalf("stream body = %q, want verbatim %q", got, sse)
	}
	if n := strings.Count(rec.Body.String(), "[DONE]"); n != 1 {
		t.Fatalf("body contains %d [DONE] markers, want exactly 1:\n%s", n, rec.Body.String())
	}
}

// 零事件空流（上游发了头就 EOF）不补发哨兵，避免掩盖上游异常。
func TestResponsesPassthroughStreamEmptyBodyNoDone(t *testing.T) {
	const model = "empty-stream-model"

	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })
	nativeResponsesModels.Lock()
	nativeResponsesModels.ids[model] = true
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, model)
		nativeResponsesModels.Unlock()
	})

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: ""},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"`+model+`","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)

	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("empty upstream stream must not get a synthesized [DONE]:\n%s", rec.Body.String())
	}
}

// 静态预置的原生模型首包即直接透传，不走 chat 翻译（无冷启动惩罚）。
func TestResponsesPassthroughStaticPresetSkipsTranslation(t *testing.T) {
	const staticModel = "muse-spark-1.3-contributor"
	if !isNativeResponsesModel(staticModel) {
		t.Fatalf("static model %q should be native by default", staticModel)
	}

	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })

	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: `{"id":"resp_static_ok","object":"response","status":"completed","output":[]}`},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"`+staticModel+`","input":"hi","stream":false}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if len(transport.requestedURLs) != 1 {
		t.Fatalf("requested URLs = %#v, want exactly 1 direct passthrough", transport.requestedURLs)
	}
	if !strings.HasSuffix(transport.requestedURLs[0], "/zen/v1/responses") {
		t.Fatalf("URL = %s, want suffix /zen/v1/responses", transport.requestedURLs[0])
	}
}

// 动态模型连续透传失败达到阈值后自动剔除（故障自愈）；静态模型永不剔除。
func TestNativeResponsesFailureEviction(t *testing.T) {
	const flaky = "flaky-dynamic-model-xyz"
	nativeResponsesModels.Lock()
	nativeResponsesModels.ids[flaky] = true
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, flaky)
		nativeResponsesModels.Unlock()
	})
	for i := 0; i < nativeResponsesEvictAfter; i++ {
		markNativeResponsesFailure(flaky)
	}
	if isNativeResponsesModel(flaky) {
		t.Fatal("dynamic model should be evicted after consecutive failures")
	}
	markNativeResponsesFailure("muse-spark-1.3-contributor")
	markNativeResponsesFailure("muse-spark-1.3-contributor")
	if !isNativeResponsesModel("muse-spark-1.3-contributor") {
		t.Fatal("static model must never be evicted")
	}
}

// extractStreamEventUsage 兼容 response.completed 与裸 usage 两种形态，
// 忽略 [DONE] 与非 data 行。
func TestExtractStreamEventUsage(t *testing.T) {
	line := []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"usage\":{\"input_tokens\":3,\"output_tokens\":4,\"total_tokens\":7}}}\n")
	usage, resp := extractStreamEventUsage(line)
	if usage == nil || usage["total_tokens"] != float64(7) {
		t.Fatalf("usage = %v, want total_tokens 7", usage)
	}
	if resp == nil || resp["id"] != "r1" {
		t.Fatalf("response = %v, want id r1", resp)
	}

	usage, resp = extractStreamEventUsage([]byte("data: {\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}\n"))
	if usage == nil || usage["total_tokens"] != float64(3) {
		t.Fatalf("bare usage = %v, want total_tokens 3", usage)
	}
	if resp != nil {
		t.Fatalf("response = %v, want nil", resp)
	}

	if u, r := extractStreamEventUsage([]byte("data: [DONE]\n")); u != nil || r != nil {
		t.Fatalf("DONE line must be ignored, got %v %v", u, r)
	}
	if u, r := extractStreamEventUsage([]byte(": keep-alive\n")); u != nil || r != nil {
		t.Fatalf("comment line must be ignored, got %v %v", u, r)
	}
}
