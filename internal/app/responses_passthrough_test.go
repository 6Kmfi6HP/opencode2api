package app

import (
	"encoding/json"
	"errors"
	"github.com/6Kmfi6HP/opencode2api/internal/config"
	"github.com/6Kmfi6HP/opencode2api/internal/stats"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	before := int64(0)
	if ms := stats.Snapshot().Models[streamModel]; ms != nil {
		before = ms.TotalTokens
	}
	t.Cleanup(func() {
		delete(stats.Snapshot().Models, streamModel)
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
	after := int64(0)
	if ms := stats.Snapshot().Models[streamModel]; ms != nil {
		after = ms.TotalTokens
	}
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
	// contributor-free 系列（上游 chat/completions 整档 500，只能走原生
	// responses）经模式匹配命中，同样永不剔除
	for _, m := range []string{"muse-spark-1.2-contributor-free", "muse-spark-1.3-contributor-free"} {
		if !isNativeResponsesModel(m) {
			t.Fatalf("contributor-free model %q must be native via static pattern", m)
		}
		for range nativeResponsesEvictAfter {
			markNativeResponsesFailure(m)
		}
		if !isNativeResponsesModel(m) {
			t.Fatalf("contributor-free model %q must never be evicted", m)
		}
	}
	if isNativeResponsesModel("muse-spark-1.3-contributor-preview") {
		t.Fatal("preview suffix must NOT match the contributor pattern (different billing tier)")
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

// ======================== 超长 name 缩短与还原 ========================

func TestShortenResponsesName(t *testing.T) {
	// <=64 的名字原样返回
	short := "mcp__codex_apps__github___create_pull_request"
	if got := shortenResponsesName(short); got != short {
		t.Fatalf("short name changed: %q", got)
	}
	// 66 字符的真实超长插件名（plugin_management___update_app_permissions 形态）
	long := "mcp__codex_apps__plugin_management___update_app_permissionsXYZi000"
	if len([]rune(long)) != 66 {
		t.Fatalf("test fixture must be 66 runes, got %d", len([]rune(long)))
	}
	got := shortenResponsesName(long)
	if n := len([]rune(got)); n != responsesMaxNameLength {
		t.Fatalf("shortened length = %d, want %d: %q", n, responsesMaxNameLength, got)
	}
	// 确定性
	if shortenResponsesName(long) != got {
		t.Fatal("shortenResponsesName must be deterministic")
	}
	// 相似名字产生不同缩短哈希，避免碰撞
	other := "mcp__codex_apps__plugin_management___update_app_permissionsXYZj111"
	if shortenResponsesName(other) == got {
		t.Fatal("different long names must not collide")
	}
	// 多字节字符不在中间被截断
	cjk := "工具名称_" + string(make([]rune, 70)) // 长度保证 >64
	for i, r := range []rune(cjk) {
		_ = i
		_ = r
	}
	cjk = string(append([]rune("工具名-"), []rune(long)...))
	if g := shortenResponsesName(cjk); len([]rune(g)) > responsesMaxNameLength {
		t.Fatalf("multibyte name truncated beyond limit: %q (%d runes)", g, len([]rune(g)))
	}
}

func TestSanitizeResponsesPassthroughBody_ShortensLongToolNames(t *testing.T) {
	long := "mcp__codex_apps__plugin_management___update_app_permissionsXYZi000" // 66
	raw := `{"model":"muse-spark-1.3-contributor","stream":true,"input":[{"type":"function_call","call_id":"c1","name":"` + long + `","arguments":"{}"}],"tool_choice":{"type":"function","name":"` + long + `"},"tools":[{"type":"function","name":"` + long + `","description":"d","parameters":{"type":"object","properties":{},"required":[]}}]}`
	sanitized, rw := sanitizeResponsesPassthroughBody([]byte(raw), "muse-spark-1.3-contributor")
	// 出站请求里不再出现超长名
	if strings.Contains(string(sanitized), long) {
		t.Fatalf("sanitized body still contains long name: %s", sanitized)
	}
	var body map[string]any
	if err := json.Unmarshal(sanitized, &body); err != nil {
		t.Fatalf("sanitized body not JSON: %v", err)
	}
	shortened := shortenResponsesName(long)
	tools := body["tools"].([]any)
	if got := tools[0].(map[string]any)["name"].(string); got != shortened {
		t.Fatalf("tools[0].name = %q, want %q", got, shortened)
	}
	tc := body["tool_choice"].(map[string]any)
	if got := tc["name"].(string); got != shortened {
		t.Fatalf("tool_choice.name = %q, want %q", got, shortened)
	}
	inputItems := body["input"].([]any)
	if got := inputItems[0].(map[string]any)["name"].(string); got != shortened {
		t.Fatalf("input[0].name = %q, want %q", got, shortened)
	}
	// 映射包含 original->shortened 与 shortened->original 双向登记
	if rw.outbound[long] != shortened || rw.inbound[shortened] != long {
		t.Fatalf("rewrites mismatch: outbound=%q inbound=%q", rw.outbound[long], rw.inbound[shortened])
	}
}

func TestResponsesNameRewrites_RestoresShortenedNamesInResponses(t *testing.T) {
	long := "mcp__codex_apps__plugin_management___update_app_permissionsXYZi000"
	shortened := shortenResponsesName(long)
	rw := newResponsesNameRewrites()
	rw.shortenRecord(long)

	// 模拟上游非流式响应
	resp := map[string]any{
		"id": "resp_1",
		"output": []any{
			map[string]any{
				"type": "function_call", "id": "fc_1", "call_id": "c1",
				"name": shortened, "arguments": "{}",
			},
			map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "echo " + shortened}},
			},
		},
	}
	if !rw.restoreResponsesPayloadNames(resp) {
		t.Fatal("expected restore to change response")
	}
	out := resp["output"].([]any)
	if got := out[0].(map[string]any)["name"].(string); got != long {
		t.Fatalf("function_call name not restored: %q", got)
	}
	// 文本里自然出现的相同串不强制保护，但注册名之外的字段必须原样
	if got := out[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string); got != "echo "+shortened {
		t.Fatalf("text content must remain untouched: %q", got)
	}
}

func TestNormalizeResponsesStreamLine_RestoresShortenedNameInEvents(t *testing.T) {
	long := "mcp__codex_apps__plugin_management___update_app_permissionsXYZi000"
	shortened := shortenResponsesName(long)
	rw := newResponsesNameRewrites()
	rw.shortenRecord(long)

	states := map[int]*argsNormState{}
	mapping := map[string]int{}
	// function_call 全量事件（item_added 带 name）应还原 name
	ev := []byte(`data: {"type":"response.output_item.done","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"c1","name":"` + shortened + `","arguments":"{}"}}` + "\n")
	out, ok := normalizeResponsesStreamLine(ev, states, mapping, rw)
	if !ok {
		t.Fatal("event should be marked changed (name restored)")
	}
	if !strings.Contains(string(out), `"name":"`+long+`"`) {
		t.Fatalf("restored long name missing: %s", string(out))
	}
	if strings.Contains(string(out), shortened) {
		t.Fatalf("shortened name should be gone: %s", string(out))
	}

	// output_text delta 永不动（即便文本恰好包含缩短名）
	textEv := []byte(`data: {"type":"response.output_text.delta","output_index":0,"delta":"` + shortened + `"}` + "\n")
	if _, ok := normalizeResponsesStreamLine(textEv, states, mapping, rw); ok {
		t.Fatal("output_text delta must not be rewritten")
	}
}

// ======================== 回放推理密文修复（caller 绑定） ========================

// 上游把 reasoning 密文绑定到发起方（账号 + 出口）；换出口/账号回放时固定报
// "was not issued to this caller"。识别必须限定 400 + 两个关键词，避免把其它
// 400（如 name 超长、effort 非法）误判成需要修复。
func TestIsForeignReasoningEchoError(t *testing.T) {
	foreign := []byte(`{"model":"muse-spark-1.3-contributor","error":{"param":null,"type":"invalid_request_error","message":"Error from provider (Console): Upstream request failed: [invalid_request_error] reasoning ` + "`encrypted_content`" + ` was not issued to this caller"}}`)

	tests := []struct {
		name   string
		status int
		body   []byte
		want   bool
	}{
		{name: "foreign echo 400", status: http.StatusBadRequest, body: foreign, want: true},
		{name: "non-400 status", status: http.StatusInternalServerError, body: foreign, want: false},
		{name: "empty body", status: http.StatusBadRequest, body: nil, want: false},
		{name: "other 400", status: http.StatusBadRequest, body: []byte(`{"error":{"message":"name must be at most 64 characters","type":"invalid_request_error"}}`), want: false},
		{name: "encrypted_content without marker", status: http.StatusBadRequest, body: []byte(`{"error":{"message":"reasoning encrypted_content is invalid"}}`), want: false},
		{name: "marker without encrypted_content", status: http.StatusBadRequest, body: []byte(`{"error":{"message":"item was not issued to this caller"}}`), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isForeignReasoningEchoError(tc.status, tc.body); got != tc.want {
				t.Fatalf("isForeignReasoningEchoError(%d, %s) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// id 与 encrypted_content 必须同时删除：只删其一会换成上游的另一个 400
// （他人签发 / Referenced reasoning item ... not found）。
func TestStripReplayedReasoningEcho(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    string // 期望结果；空字符串表示 body 不变
		changed bool
	}{
		{
			name:    "removes id and encrypted_content keeps summary",
			body:    `{"model":"m","input":[{"type":"message","role":"user","content":"hi"},{"type":"reasoning","id":"rs_foreign","summary":[{"type":"summary_text","text":"old"}],"encrypted_content":"Zm9yZWlnbg=="},{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"}]}`,
			want:    `{"input":[{"content":"hi","role":"user","type":"message"},{"summary":[{"text":"old","type":"summary_text"}],"type":"reasoning"},{"arguments":"{}","call_id":"c1","name":"shell","type":"function_call"}],"model":"m"}`,
			changed: true,
		},
		{
			name:    "removes encrypted_content without id",
			body:    `{"input":[{"type":"reasoning","encrypted_content":"Zm9yZWlnbg=="}]}`,
			want:    `{"input":[{"type":"reasoning"}]}`,
			changed: true,
		},
		{
			name:    "no reasoning items",
			body:    `{"input":[{"type":"message","role":"user","content":"hi"}]}`,
			changed: false,
		},
		{
			name:    "input as string",
			body:    `{"input":"hi"}`,
			changed: false,
		},
		{
			name:    "invalid json",
			body:    `{"input":`,
			changed: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := stripReplayedReasoningEcho([]byte(tc.body))
			if changed != tc.changed {
				t.Fatalf("changed = %v, want %v (body = %s)", changed, tc.changed, got)
			}
			if !changed {
				if string(got) != tc.body {
					t.Fatalf("unchanged body must be returned verbatim, got %s", got)
				}
				return
			}
			var wantMap, gotMap map[string]any
			if err := json.Unmarshal([]byte(tc.want), &wantMap); err != nil {
				t.Fatalf("bad want fixture: %v", err)
			}
			if err := json.Unmarshal(got, &gotMap); err != nil {
				t.Fatalf("unmarshal got: %v", err)
			}
			if !reflect.DeepEqual(gotMap, wantMap) {
				t.Fatalf("repaired body = %s, want %s", got, tc.want)
			}
		})
	}
}

// 会话中途 sticky 出口改绑后，客户端回放上一轮（他人签发）的 reasoning 密文：
// 网关必须剥掉推理回声重发一次，而不是把 400 直接抛给用户。可见对话内容
// （消息、工具调用）一个字都不能改。
func TestResponsesPassthroughRepairsForeignReasoningEcho(t *testing.T) {
	const requestBody = `{"model":"muse-spark-1.3-contributor","store":false,"include":["reasoning.encrypted_content"],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"reasoning","id":"rs_other_caller:rs_1","summary":[{"type":"summary_text","text":"old thinking"}],"encrypted_content":"Zm9yZWlnbg=="},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},{"type":"function_call_output","call_id":"call_1","output":"ok"},{"type":"message","role":"user","content":[{"type":"input_text","text":"go on"}]}]}`

	for _, tc := range []struct {
		name       string
		stream     bool
		okBody     string
		wantInBody string
	}{
		{
			name:       "non-stream",
			stream:     false,
			okBody:     `{"id":"resp_ok","object":"response","status":"completed","output":[]}`,
			wantInBody: `"resp_ok"`,
		},
		{
			name:       "stream",
			stream:     true,
			okBody:     "event: response.completed\ndata: {\"id\":\"resp_ok_stream\"}\n\ndata: [DONE]\n\n",
			wantInBody: "resp_ok_stream",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldModelAlias := getModelKeywordRules()
			applyConfig(AppConfig{})
			t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })

			// 预防性剥离已生效：理论上第一次就成功，不再需要重发。
			transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
				{status: http.StatusOK, body: tc.okBody},
			})

			body := strings.Replace(requestBody, `"store":false`, `"store":false,"stream":`+map[bool]string{true: "true", false: "false"}[tc.stream], 1)
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			rec := httptest.NewRecorder()
			responsesHandler(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (first-try success), body = %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantInBody) {
				t.Fatalf("body = %s, want %s", rec.Body.String(), tc.wantInBody)
			}
			if len(transport.requestedURLs) != 1 {
				t.Fatalf("upstream calls = %#v, want exactly 1 (no repair retry needed)", transport.requestedURLs)
			}

			// 上游收到的请求：reasoning 回声已被预防性剥离，其余 item 原样。
			sent := transport.requestPayloads[0]
			items, _ := sent["input"].([]any)
			if len(items) != 6 {
				t.Fatalf("sent input len = %d, want 6 (item count must not change)", len(items))
			}
			reasoning, _ := items[1].(map[string]any)
			if reasoning["type"] != "reasoning" {
				t.Fatalf("item[1] = %#v, want the reasoning item kept in place", items[1])
			}
			if _, ok := reasoning["id"]; ok {
				t.Fatalf("reasoning id must be stripped preemptively: %#v", reasoning)
			}
			if _, ok := reasoning["encrypted_content"]; ok {
				t.Fatalf("reasoning encrypted_content must be stripped preemptively: %#v", reasoning)
			}
			if summary, _ := reasoning["summary"].([]any); len(summary) != 1 {
				t.Fatalf("summary must be preserved, got %#v", reasoning["summary"])
			}
			// 可见与工具调用 item 原样透传
			if items[0].(map[string]any)["type"] != "message" || items[3].(map[string]any)["arguments"] != `{"cmd":"ls"}` {
				t.Fatalf("neighbouring items changed: %v", items)
			}
		})
	}
}

// 没有可剥离的推理回声（或错误与发起方绑定无关）时保持标准代理语义：
// 单次调用、400 原样透传，不制造第二次请求。
func TestResponsesPassthroughForeignReasoningEchoWithoutEchoStays400(t *testing.T) {
	const rememberedModel = "remembered-foreign-echo-model"

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

	tests := []struct {
		name string
		body string
	}{
		{
			name: "no reasoning items",
			body: `{"model":"` + rememberedModel + `","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
		},
		{
			name: "input as string",
			body: `{"model":"` + rememberedModel + `","input":"hi"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
				{status: http.StatusBadRequest, body: `{"error":{"message":"reasoning ` + "`encrypted_content`" + ` was not issued to this caller"}}`},
			})

			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			responsesHandler(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 passthrough, body = %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "was not issued to this caller") {
				t.Fatalf("body = %s, want upstream error detail", rec.Body.String())
			}
			if got := len(transport.requestedURLs); got != 1 {
				t.Fatalf("upstream calls = %d, want 1 (nothing to repair)", got)
			}
		})
	}
}

// ======================== 回放历史 arguments 归一化 ========================

func TestCoalesceReplayedToolCallArgs_Cases(t *testing.T) {
	mkBody := func(items ...any) map[string]any {
		return map[string]any{"model": "muse-spark-1.3-contributor", "input": items}
	}
	cases := []struct {
		name     string
		items    []any
		wantArgs []string // 与 items 等长；"" 表示不应有 arguments 键
		changed  bool
	}{
		{
			name:     "空串改 {}",
			items:    []any{map[string]any{"type": "function_call", "call_id": "c1", "name": "multi_agent_v1", "arguments": ""}},
			wantArgs: []string{"{}"}, changed: true,
		},
		{
			// 本次事故现场形态：幻觉工具名 + 空 arguments
			name:     "幻觉调用空参数",
			items:    []any{map[string]any{"type": "function_call", "call_id": "c2", "name": "multi_agent_v1", "arguments": ""}},
			wantArgs: []string{"{}"}, changed: true,
		},
		{
			name:     "合法 JSON 不动",
			items:    []any{map[string]any{"type": "function_call", "call_id": "c3", "name": "exec_command", "arguments": `{"cmd":"echo 1.0","yield_time_ms":1000.0}`}},
			wantArgs: []string{`{"cmd":"echo 1.0","yield_time_ms":1000.0}`}, changed: false,
		},
		{
			name:     "缺 arguments 不补",
			items:    []any{map[string]any{"type": "function_call", "call_id": "c4", "name": "exec_command"}},
			wantArgs: []string{""}, changed: false,
		},
		{
			name:     "非串类型 null 改 {}",
			items:    []any{map[string]any{"type": "function_call", "call_id": "c5", "name": "exec_command", "arguments": nil}},
			wantArgs: []string{"{}"}, changed: true,
		},
		{
			name:     "对象序列化为串",
			items:    []any{map[string]any{"type": "custom_tool_call", "call_id": "c6", "name": "ping", "arguments": map[string]any{"a": 1.0}}},
			wantArgs: []string{`{"a":1}`}, changed: true,
		},
		{
			name: "function_call_output 与 message 不动",
			items: []any{
				map[string]any{"type": "function_call_output", "call_id": "c7", "output": "unsupported call: multi_agent_v1"},
				map[string]any{"type": "message", "role": "assistant"},
			},
			wantArgs: []string{"", ""}, changed: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := mkBody(tc.items...)
			got := coalesceReplayedToolCallArgs(body)
			if got != tc.changed {
				t.Fatalf("changed = %v, want %v", got, tc.changed)
			}
			items := body["input"].([]any)
			for i, want := range tc.wantArgs {
				m := items[i].(map[string]any)
				if want == "" {
					if _, exists := m["arguments"]; exists {
						t.Fatalf("item %d: arguments 不应存在或应保持原状", i)
					}
					continue
				}
				if got := m["arguments"]; got != want {
					t.Fatalf("item %d arguments = %#v, want %#v", i, got, want)
				}
			}
		})
	}
}

func TestSanitizeResponsesPassthroughBody_EmptyArgumentsMuseSpark(t *testing.T) {
	raw := []byte(`{
		"model": "muse-spark-1.3-contributor",
		"input": [
			{"type": "message", "role": "user", "content": "继续"},
			{"type": "function_call", "call_id": "call_x", "name": "multi_agent_v1", "arguments": ""},
			{"type": "function_call_output", "call_id": "call_x", "output": "unsupported call: multi_agent_v1"}
		]
	}`)
	out, _ := sanitizeResponsesPassthroughBody(raw, "muse-spark-1.3-contributor")
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("sanitize 输出不是合法 JSON: %v", err)
	}
	items := got["input"].([]any)
	if args := items[1].(map[string]any)["arguments"]; args != "{}" {
		t.Fatalf("空 arguments 未归一化，got %#v", args)
	}
	// output 原样保留，不被改写
	if out0 := items[2].(map[string]any)["output"]; out0 != "unsupported call: multi_agent_v1" {
		t.Fatalf("function_call_output 被误改: %#v", out0)
	}
}

func TestSanitizeResponsesPassthroughBody_NonMuseUnchanged(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":[{"type":"function_call","call_id":"c1","name":"x","arguments":""}]}`)
	out, _ := sanitizeResponsesPassthroughBody(raw, "gpt-5")
	if string(out) != string(raw) {
		t.Fatalf("非 muse-spark 模型请求体被改写")
	}
}

// ======================== 预防性 reasoning 回声剥离（sanitize 路径） ========================

func TestSanitizeResponsesPassthroughBody_PreemptivelyStripsReasoningEcho(t *testing.T) {
	raw := []byte(`{
		"model": "muse-spark-1.3-contributor",
		"input": [
			{"type": "message", "role": "user", "content": "hi"},
			{"type": "reasoning", "id": "rs_foreign", "summary": [{"type": "summary_text", "text": "old"}], "encrypted_content": "Zm9yZWlnbg=="},
			{"type": "function_call", "call_id": "c1", "name": "shell", "arguments": "{}"}
		]
	}`)
	out, _ := sanitizeResponsesPassthroughBody(raw, "muse-spark-1.3-contributor")
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("sanitize 输出非合法 JSON: %v", err)
	}
	items := got["input"].([]any)
	rs := items[1].(map[string]any)
	if _, hasID := rs["id"]; hasID {
		t.Fatalf("reasoning id 未被预防性剥离: %v", rs)
	}
	if _, hasEC := rs["encrypted_content"]; hasEC {
		t.Fatalf("encrypted_content 未被预防性剥离: %v", rs)
	}
	// summary 必须保留（可见推理摘要）
	if s, ok := rs["summary"].([]any); !ok || len(s) != 1 {
		t.Fatalf("reasoning summary 被误删: %v", rs)
	}
	// message / function_call 不动
	if items[0].(map[string]any)["type"] != "message" {
		t.Fatalf("message item 被改动")
	}
	if items[2].(map[string]any)["arguments"] != "{}" {
		t.Fatalf("合法 arguments 被误改")
	}
}

func TestSanitizeResponsesPassthroughBody_NoReasoningEchoUnchanged(t *testing.T) {
	// 无 echo 且无其它修正触发时原样返回（幂等）
	raw := []byte(`{"model":"muse-spark-1.3-contributor","input":[{"type":"message","role":"user","content":"hi"}]}`)
	out, _ := sanitizeResponsesPassthroughBody(raw, "muse-spark-1.3-contributor")
	if string(out) != string(raw) {
		t.Fatalf("无 echo 请求被误改")
	}
}

// ======================== max_output_tokens 直通钳制 ========================

// 全局 cap 生效且原请求缺 max_output_tokens 时注入 = cap。pre-fix 该场景下
// 函数直接早退（非 muse-spark 模型），泄漏 = 上游拿到空值后按自身默认截
// 断，EOF 无 terminal 事件，兜底合成 reason=max_output_tokens 误报。
func TestSanitizeResponsesPassthroughBody_InjectsMaxOutputTokensForNonMuse(t *testing.T) {
	old := config.Get()
	config.Update(func(s *config.Snapshot) {
		s.MaxTokensCap = 128000
		s.MaxTokensCapPerModel = nil
	})
	t.Cleanup(func() { config.Update(func(s *config.Snapshot) { *s = old }) })

	raw := []byte(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":"hi"}]}`)
	out, _ := sanitizeResponsesPassthroughBody(raw, "gpt-5")
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("sanitize 输出非合法 JSON: %v", err)
	}
	if v, ok := intFromAny(got["max_output_tokens"]); !ok || v != 128000 {
		t.Fatalf("cap=128000 应注入 max_output_tokens=128000, got %#v", got["max_output_tokens"])
	}
}

// cap 生效且原请求显式超过 cap 时钳到 cap。
func TestSanitizeResponsesPassthroughBody_ClampsAboveCap(t *testing.T) {
	old := config.Get()
	config.Update(func(s *config.Snapshot) {
		s.MaxTokensCap = 128000
		s.MaxTokensCapPerModel = nil
	})
	t.Cleanup(func() { config.Update(func(s *config.Snapshot) { *s = old }) })

	raw := []byte(`{"model":"gpt-5","max_output_tokens":500000,"input":[{"type":"message","role":"user","content":"hi"}]}`)
	out, _ := sanitizeResponsesPassthroughBody(raw, "gpt-5")
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("sanitize 输出非合法 JSON: %v", err)
	}
	if v, ok := intFromAny(got["max_output_tokens"]); !ok || v != 128000 {
		t.Fatalf("超过 cap 应钳到 128000, got %#v", got["max_output_tokens"])
	}
}

// 原请求值低于下限 128 时抬到 128（与 chat→anthropic 的 resolveMaxTokens 口径一致）。
func TestSanitizeResponsesPassthroughBody_LiftsBelowFloor(t *testing.T) {
	old := config.Get()
	config.Update(func(s *config.Snapshot) {
		s.MaxTokensCap = 128000
		s.MaxTokensCapPerModel = nil
	})
	t.Cleanup(func() { config.Update(func(s *config.Snapshot) { *s = old }) })

	raw := []byte(`{"model":"gpt-5","max_output_tokens":50,"input":[{"type":"message","role":"user","content":"hi"}]}`)
	out, _ := sanitizeResponsesPassthroughBody(raw, "gpt-5")
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("sanitize 输出非合法 JSON: %v", err)
	}
	if v, ok := intFromAny(got["max_output_tokens"]); !ok || v != 128 {
		t.Fatalf("低于下限 128 应抬到 128, got %#v", got["max_output_tokens"])
	}
}

// cap=0 不注入也不覆盖，保持原始行为。
func TestSanitizeResponsesPassthroughBody_NoCapNoTouch(t *testing.T) {
	old := config.Get()
	config.Update(func(s *config.Snapshot) {
		s.MaxTokensCap = 0
		s.MaxTokensCapPerModel = nil
	})
	t.Cleanup(func() { config.Update(func(s *config.Snapshot) { *s = old }) })

	raw := []byte(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":"hi"}]}`)
	out, _ := sanitizeResponsesPassthroughBody(raw, "gpt-5")
	if string(out) != string(raw) {
		t.Fatalf("cap=0 时请求体不应被改写, got %s", out)
	}
}

// muse-spark 模型同样也注入 cap（它在原实现里走的是不同的早退分支）。
func TestSanitizeResponsesPassthroughBody_InjectsForMuseSpark(t *testing.T) {
	old := config.Get()
	config.Update(func(s *config.Snapshot) {
		s.MaxTokensCap = 128000
		s.MaxTokensCapPerModel = nil
	})
	t.Cleanup(func() { config.Update(func(s *config.Snapshot) { *s = old }) })

	raw := []byte(`{"model":"muse-spark-1.3-contributor","input":[{"type":"message","role":"user","content":"hi"}]}`)
	out, _ := sanitizeResponsesPassthroughBody(raw, "muse-spark-1.3-contributor")
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("sanitize 输出非合法 JSON: %v", err)
	}
	if v, ok := intFromAny(got["max_output_tokens"]); !ok || v != 128000 {
		t.Fatalf("muse-spark 模型也应注入 max_output_tokens=128000, got %#v", got["max_output_tokens"])
	}
}

func TestSanitizeResponsesPassthroughBody_EffortNormalizedAddsSummary(t *testing.T) {
	// muse-spark：effort 归一化（max->xhigh）后补默认 summary:auto，否则上游
	// 静默思考、codex 不显示 thinking；客户端已显式给 summary 时不动。
	raw := []byte(`{"model":"m","input":"hi","reasoning":{"effort":"max"}}`)
	fixed, _ := sanitizeResponsesPassthroughBody(raw, "muse-spark-1.3-contributor")
	var body map[string]any
	if err := json.Unmarshal(fixed, &body); err != nil {
		t.Fatal(err)
	}
	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "xhigh" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning = %#v, want {effort:xhigh summary:auto}", body["reasoning"])
	}

	// effort 归一化为空（none）时整段 reasoning 删除，不残留 summary-only。
	raw2 := []byte(`{"model":"m","input":"hi","reasoning":{"effort":"none","summary":"auto"}}`)
	fixed2, _ := sanitizeResponsesPassthroughBody(raw2, "muse-spark-1.3-contributor")
	var body2 map[string]any
	if err := json.Unmarshal(fixed2, &body2); err != nil {
		t.Fatal(err)
	}
	if r, ok := body2["reasoning"]; ok && r != nil {
		t.Fatalf("reasoning 应整体删除, got %#v", body2["reasoning"])
	}
}
