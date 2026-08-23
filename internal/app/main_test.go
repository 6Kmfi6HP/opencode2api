package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestVersionStringIncludesBuildMetadata(t *testing.T) {
	oldVersion, oldCommit, oldDate := version, commit, date
	t.Cleanup(func() {
		version, commit, date = oldVersion, oldCommit, oldDate
	})

	version = "v1.2.3"
	commit = "abc1234"
	date = "2026-06-04T00:00:00Z"

	got := versionString()
	for _, want := range []string{"opencode2api", "v1.2.3", "abc1234", "2026-06-04T00:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Fatalf("versionString() = %q, want it to contain %q", got, want)
		}
	}
}

func TestVersionStringFormat(t *testing.T) {
	got := versionString()
	if !strings.HasPrefix(got, "opencode2api ") {
		t.Fatalf("versionString() = %q, want prefix 'opencode2api '", got)
	}
	if !strings.Contains(got, "(commit=") || !strings.Contains(got, ", date=") {
		t.Fatalf("versionString() = %q, want commit and date info", got)
	}
}

type fakeUpstreamResponse struct {
	status int
	body   string
	header http.Header
}

type fakeRetryTransport struct {
	t               *testing.T
	responses       []fakeUpstreamResponse
	requestedModels []string
	requestedURLs   []string
	requestPayloads []map[string]any
	closeIdleCalls  int
}

func (f *fakeRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(f.responses) == 0 {
		f.t.Fatalf("unexpected request to %s", req.URL.String())
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		f.t.Fatalf("read request body: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		f.t.Fatalf("unmarshal request body: %v", err)
	}
	model, _ := payload["model"].(string)
	f.requestedModels = append(f.requestedModels, model)
	f.requestedURLs = append(f.requestedURLs, req.URL.String())
	f.requestPayloads = append(f.requestPayloads, payload)

	next := f.responses[0]
	f.responses = f.responses[1:]
	header := next.header
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{
		StatusCode: next.status,
		Header:     header.Clone(),
		Body:       io.NopCloser(strings.NewReader(next.body)),
		Request:    req,
	}, nil
}

func (f *fakeRetryTransport) CloseIdleConnections() {
	f.closeIdleCalls++
}

func installFakeOpenCodeClient(t *testing.T, responses []fakeUpstreamResponse) *fakeRetryTransport {
	t.Helper()

	oldHTTPClient := httpClient
	oldModelsCache := modelsCache
	oldGoModelsCache := goModelsCache
	oldOCClientVer := ocClientVer
	oldOCSessionID := ocSessionID
	oldOCProjectID := ocProjectID
	oldActiveSocks5 := activeSocks5
	oldSocks5Client := socks5Client
	oldSocks5ClientAddr := socks5ClientAddr
	oldSocks5PaidDirect := socks5PaidDirect
	oldSocks5Proxies := socks5Proxies
	oldUpstreamBaseURLs := upstreamBaseURLs

	transport := &fakeRetryTransport{
		t:         t,
		responses: append([]fakeUpstreamResponse(nil), responses...),
	}
	httpClient = &http.Client{Transport: transport}

	modelMu.Lock()
	modelsCache = []ModelInfo{{ID: "fallback-model-free"}}
	goModelsCache = nil
	modelMu.Unlock()

	socks5Mu.Lock()
	activeSocks5 = ""
	socks5Client = nil
	socks5ClientAddr = ""
	socks5PaidDirect = false
	socks5Proxies = nil
	upstreamBaseURLs = normalizeBaseURLs(nil)
	socks5Mu.Unlock()

	ocOnce = sync.Once{}
	ocOnce.Do(func() {})
	ocClientVer = "test-version"
	ocSessionID = "ses_test"
	ocProjectID = "project_test"

	t.Cleanup(func() {
		httpClient = oldHTTPClient
		modelMu.Lock()
		modelsCache = oldModelsCache
		goModelsCache = oldGoModelsCache
		modelMu.Unlock()
		socks5Mu.Lock()
		activeSocks5 = oldActiveSocks5
		socks5Client = oldSocks5Client
		socks5ClientAddr = oldSocks5ClientAddr
		socks5PaidDirect = oldSocks5PaidDirect
		socks5Proxies = oldSocks5Proxies
		upstreamBaseURLs = oldUpstreamBaseURLs
		socks5Mu.Unlock()
		ocOnce = sync.Once{}
		ocClientVer = oldOCClientVer
		ocSessionID = oldOCSessionID
		ocProjectID = oldOCProjectID
	})

	return transport
}

func TestCallOpenCodeAPIRetriesSameModelWithoutFallback(t *testing.T) {
	tests := []struct {
		name        string
		stream      bool
		responses   []fakeUpstreamResponse
		wantStatus  int
		wantBody    string
		wantModels  []string
		wantCloses  int
		requestBody string
		auth        UpstreamAuth
	}{
		{
			name:   "non-stream retries 401 on same model then succeeds",
			stream: false,
			responses: []fakeUpstreamResponse{
				{status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`},
				{status: http.StatusOK, body: `{"id":"chatcmpl_test","choices":[]}`},
			},
			wantStatus:  http.StatusOK,
			wantBody:    `{"id":"chatcmpl_test","choices":[]}`,
			wantModels:  []string{"primary-model", "primary-model"},
			wantCloses:  1,
			requestBody: `{"model":"primary-model","messages":[]}`,
			auth:        UpstreamAuth{Mode: AuthRoutePublic},
		},
		{
			name:   "stream retries 429 on same model then succeeds",
			stream: true,
			responses: []fakeUpstreamResponse{
				{status: http.StatusTooManyRequests, body: `{"error":"rate_limited"}`},
				{status: http.StatusOK, body: "data: ok\n\n"},
			},
			wantStatus:  http.StatusOK,
			wantBody:    "data: ok\n\n",
			wantModels:  []string{"primary-model", "primary-model"},
			wantCloses:  1,
			requestBody: `{"model":"primary-model","messages":[],"stream":true}`,
			auth:        UpstreamAuth{Mode: AuthRoutePublic},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := installFakeOpenCodeClient(t, tt.responses)
			modelMu.Lock()
			modelsCache = []ModelInfo{{ID: "primary-model"}, {ID: "fallback-model-free"}}
			modelMu.Unlock()

			var (
				body   []byte
				status int
				err    error
			)
			if tt.stream {
				var respBody io.ReadCloser
				respBody, status, _, err = callOpenCodeAPIStream(context.Background(), []byte(tt.requestBody), "primary-model", tt.auth)
				if respBody != nil {
					defer respBody.Close()
				}
				if err == nil {
					body, err = io.ReadAll(respBody)
				}
			} else {
				body, status, _, err = callOpenCodeAPI(context.Background(), []byte(tt.requestBody), "primary-model", tt.auth)
			}
			if err != nil {
				t.Fatalf("upstream call error = %v", err)
			}
			if status != tt.wantStatus {
				t.Fatalf("upstream call status = %d, want %d", status, tt.wantStatus)
			}
			if string(body) != tt.wantBody {
				t.Fatalf("upstream call body = %q, want %q", string(body), tt.wantBody)
			}
			if !reflect.DeepEqual(transport.requestedModels, tt.wantModels) {
				t.Fatalf("requested models = %#v, want %#v", transport.requestedModels, tt.wantModels)
			}
			if transport.closeIdleCalls != tt.wantCloses {
				t.Fatalf("CloseIdleConnections calls = %d, want %d", transport.closeIdleCalls, tt.wantCloses)
			}
		})
	}
}

func TestCallOpenCodeAPIKeyedAuthDoesNotCrossModelFallback(t *testing.T) {
	tests := []struct {
		name   string
		stream bool
	}{
		{name: "non-stream", stream: false},
		{name: "stream", stream: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			creditsBody := `{"type":"error","error":{"type":"CreditsError","message":"Insufficient balance. Manage your billing here: https://opencode.ai/workspace/billing"}}`
			transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
				{status: http.StatusUnauthorized, body: creditsBody},
			})
			modelMu.Lock()
			modelsCache = []ModelInfo{{ID: "claude-sonnet-4-5"}, {ID: "claude-opus-5"}, {ID: "fallback-model-free"}}
			goModelsCache = []ModelInfo{{ID: "go-only-model"}, {ID: "shared-model"}}
			modelMu.Unlock()

			auth := UpstreamAuth{Mode: AuthRouteAuto, Token: "sk-validkey0123456789abcdef"}
			body := []byte(`{"model":"claude-sonnet-4-5","messages":[]}`)
			if tt.stream {
				body = []byte(`{"model":"claude-sonnet-4-5","messages":[],"stream":true}`)
				respBody, status, _, err := callOpenCodeAPIStream(context.Background(), body, "claude-sonnet-4-5", auth)
				if respBody != nil {
					defer respBody.Close()
					got, _ := io.ReadAll(respBody)
					if string(got) != creditsBody {
						t.Fatalf("stream body = %s, want credits error", string(got))
					}
				}
				if err != nil {
					t.Fatalf("callOpenCodeAPIStream() error = %v", err)
				}
				if status != http.StatusUnauthorized {
					t.Fatalf("callOpenCodeAPIStream() status = %d, want %d", status, http.StatusUnauthorized)
				}
			} else {
				got, status, _, err := callOpenCodeAPI(context.Background(), body, "claude-sonnet-4-5", auth)
				if err == nil {
					t.Fatal("callOpenCodeAPI() error = nil, want upstream error")
				}
				if status != http.StatusUnauthorized {
					t.Fatalf("callOpenCodeAPI() status = %d, want %d", status, http.StatusUnauthorized)
				}
				if string(got) != creditsBody {
					t.Fatalf("callOpenCodeAPI() body = %s, want credits error", string(got))
				}
			}

			wantModels := []string{"claude-sonnet-4-5"}
			if !reflect.DeepEqual(transport.requestedModels, wantModels) {
				t.Fatalf("requested models = %#v, want %#v (no cross-model fallback, no credits retry)", transport.requestedModels, wantModels)
			}
			wantURL := "https://opencode.ai/zen/v1/chat/completions"
			if !reflect.DeepEqual(transport.requestedURLs, []string{wantURL}) {
				t.Fatalf("requested URLs = %#v, want single %q", transport.requestedURLs, wantURL)
			}
		})
	}
}

func TestCallOpenCodeAPIGoEndpointKeepsSurfaceWithoutFallback(t *testing.T) {
	tests := []struct {
		name   string
		stream bool
	}{
		{name: "non-stream", stream: false},
		{name: "stream", stream: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
				{status: http.StatusOK, body: `{"id":"chatcmpl_test","choices":[]}`},
			})
			modelMu.Lock()
			modelsCache = []ModelInfo{{ID: "shared-model"}}
			goModelsCache = []ModelInfo{{ID: "go-only-model"}, {ID: "shared-model"}}
			modelMu.Unlock()

			auth := UpstreamAuth{Mode: AuthRouteAuto, Token: "sk-validkey0123456789abcdef"}
			body := []byte(`{"model":"go-only-model","messages":[]}`)
			if tt.stream {
				body = []byte(`{"model":"go-only-model","messages":[],"stream":true}`)
				respBody, status, _, err := callOpenCodeAPIStream(context.Background(), body, "go-only-model", auth)
				if respBody != nil {
					defer respBody.Close()
				}
				if err != nil {
					t.Fatalf("callOpenCodeAPIStream() error = %v", err)
				}
				if status != http.StatusOK {
					t.Fatalf("callOpenCodeAPIStream() status = %d, want %d", status, http.StatusOK)
				}
			} else {
				_, status, _, err := callOpenCodeAPI(context.Background(), body, "go-only-model", auth)
				if err != nil {
					t.Fatalf("callOpenCodeAPI() error = %v", err)
				}
				if status != http.StatusOK {
					t.Fatalf("callOpenCodeAPI() status = %d, want %d", status, http.StatusOK)
				}
			}

			wantURL := "https://opencode.ai/zen/go/v1/chat/completions"
			if !reflect.DeepEqual(transport.requestedURLs, []string{wantURL}) {
				t.Fatalf("requested URLs = %#v, want %q", transport.requestedURLs, wantURL)
			}
			if !reflect.DeepEqual(transport.requestedModels, []string{"go-only-model"}) {
				t.Fatalf("requested models = %#v, want [go-only-model]", transport.requestedModels)
			}
		})
	}
}

func TestIsNonRetryableUpstreamError(t *testing.T) {
	credits := []byte(`{"type":"error","error":{"type":"CreditsError","message":"Insufficient balance"}}`)
	if !isNonRetryableUpstreamError(http.StatusUnauthorized, credits) {
		t.Fatal("CreditsError should be non-retryable")
	}
	plain := []byte(`{"error":"unauthorized"}`)
	if isNonRetryableUpstreamError(http.StatusUnauthorized, plain) {
		t.Fatal("plain 401 without credits payload should not be classified as non-retryable billing")
	}
}

func TestGetHTTPClientForTierSocks5PaidDirectDefaultUsesProxy(t *testing.T) {
	oldHTTPClient := httpClient
	oldProxies := socks5Proxies
	oldActive := activeSocks5
	oldPaidDirect := socks5PaidDirect
	oldClient := socks5Client
	oldClientAddr := socks5ClientAddr
	t.Cleanup(func() {
		httpClient = oldHTTPClient
		socks5Mu.Lock()
		socks5Proxies = oldProxies
		activeSocks5 = oldActive
		socks5PaidDirect = oldPaidDirect
		socks5Client = oldClient
		socks5ClientAddr = oldClientAddr
		socks5Mu.Unlock()
	})

	httpClient = &http.Client{Timeout: 1}
	socks5Mu.Lock()
	socks5Proxies = []Socks5Proxy{{Addr: "127.0.0.1:1080", Name: "test"}}
	activeSocks5 = "127.0.0.1:1080"
	socks5PaidDirect = false
	socks5Client = nil
	socks5ClientAddr = ""
	socks5Mu.Unlock()

	paid := getHTTPClientForTier(TierPaid)
	free := getHTTPClientForTier(TierFree)
	if paid == httpClient {
		t.Fatal("default socks5_paid_direct=false should send paid traffic through SOCKS5")
	}
	if free == httpClient {
		t.Fatal("free traffic should use SOCKS5 when active_socks5 is set")
	}
	if paid != free {
		t.Fatal("paid and free should share the cached SOCKS5 client when paid_direct is false")
	}

	socks5Mu.Lock()
	socks5PaidDirect = true
	socks5Mu.Unlock()
	if getHTTPClientForTier(TierPaid) != httpClient {
		t.Fatal("socks5_paid_direct=true should keep paid traffic on the direct client")
	}
	if getHTTPClientForTier(TierFree) == httpClient {
		t.Fatal("free traffic should still use SOCKS5 when paid_direct is true")
	}
}

func TestCallOpenCodeAPIExhausted4xxReturnsRequestedModelError(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{
			status: http.StatusUnauthorized,
			body:   `{"error":"unauthorized-1"}`,
			header: http.Header{"X-Upstream-Error": []string{"first"}},
		},
		{
			status: http.StatusUnauthorized,
			body:   `{"error":"unauthorized-2"}`,
			header: http.Header{"X-Upstream-Error": []string{"second"}},
		},
		{
			status: http.StatusUnauthorized,
			body:   `{"error":"unauthorized-3"}`,
			header: http.Header{"X-Upstream-Error": []string{"last"}},
		},
	})
	modelMu.Lock()
	modelsCache = []ModelInfo{{ID: "primary-model"}, {ID: "fallback-model-free"}}
	modelMu.Unlock()

	body, status, header, err := callOpenCodeAPI(context.Background(), []byte(`{"model":"primary-model","messages":[]}`), "primary-model", UpstreamAuth{Mode: AuthRoutePublic})
	if err == nil {
		t.Fatal("callOpenCodeAPI() error = nil, want upstream error")
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("callOpenCodeAPI() status = %d, want %d", status, http.StatusUnauthorized)
	}
	if string(body) != `{"error":"unauthorized-3"}` {
		t.Fatalf("callOpenCodeAPI() body = %s, want final same-model retry body", string(body))
	}
	if header.Get("X-Upstream-Error") != "last" {
		t.Fatalf("final header = %q, want last", header.Get("X-Upstream-Error"))
	}
	wantModels := []string{"primary-model", "primary-model", "primary-model"}
	if !reflect.DeepEqual(transport.requestedModels, wantModels) {
		t.Fatalf("requested models = %#v, want %#v", transport.requestedModels, wantModels)
	}
	if transport.closeIdleCalls != 2 {
		t.Fatalf("CloseIdleConnections calls = %d, want 2", transport.closeIdleCalls)
	}
}

func TestBuildOCRequestRoutesSharedAndGoOnlyModelsByAuthMode(t *testing.T) {
	oldModelsCache := modelsCache
	oldGoModelsCache := goModelsCache
	modelMu.Lock()
	modelsCache = []ModelInfo{
		{ID: "glm-5.2"},
		{ID: "gpt-5.5"},
	}
	goModelsCache = []ModelInfo{
		{ID: "glm-5.2"},
		{ID: "kimi-k2.7-code"},
	}
	modelMu.Unlock()
	t.Cleanup(func() {
		modelMu.Lock()
		modelsCache = oldModelsCache
		goModelsCache = oldGoModelsCache
		modelMu.Unlock()
	})

	tests := []struct {
		name    string
		auth    UpstreamAuth
		modelID string
		wantURL string
	}{
		{
			name:    "public stays on zen free surface",
			auth:    UpstreamAuth{Mode: AuthRoutePublic},
			modelID: "deepseek-v4-flash-free",
			wantURL: "https://opencode.ai/zen/v1/chat/completions",
		},
		{
			name:    "bare key keeps shared model on zen",
			auth:    UpstreamAuth{Mode: AuthRouteAuto, Token: "sk-auto"},
			modelID: "glm-5.2",
			wantURL: "https://opencode.ai/zen/v1/chat/completions",
		},
		{
			name:    "go prefix sends shared model to go surface",
			auth:    UpstreamAuth{Mode: AuthRouteGo, Token: "sk-go"},
			modelID: "glm-5.2",
			wantURL: "https://opencode.ai/zen/go/v1/chat/completions",
		},
		{
			name:    "bare key still reaches go only models",
			auth:    UpstreamAuth{Mode: AuthRouteAuto, Token: "sk-auto"},
			modelID: "kimi-k2.7-code",
			wantURL: "https://opencode.ai/zen/go/v1/chat/completions",
		},
		{
			name:    "zen prefix forces zen surface",
			auth:    UpstreamAuth{Mode: AuthRouteZen, Token: "sk-zen"},
			modelID: "glm-5.2",
			wantURL: "https://opencode.ai/zen/v1/chat/completions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildOCRequest(tt.modelID, map[string]any{"messages": []any{}}, tt.auth)
			if err != nil {
				t.Fatalf("buildOCRequest() error = %v", err)
			}
			if got := req.URL.String(); got != tt.wantURL {
				t.Fatalf("buildOCRequest() URL = %q, want %q", got, tt.wantURL)
			}
			wantAuth := "Bearer public"
			if tt.auth.Mode != AuthRoutePublic {
				wantAuth = "Bearer " + tt.auth.Token
			}
			if got := req.Header.Get("Authorization"); got != wantAuth {
				t.Fatalf("buildOCRequest() Authorization = %q, want %q", got, wantAuth)
			}
		})
	}
}

func TestListModelsHandlerSeparatesPublicZenAndGoCatalogs(t *testing.T) {
	oldModelsCache := modelsCache
	oldGoModelsCache := goModelsCache
	oldModelsLoaded := modelsLoaded
	oldModelAlias := getModelKeywordRules()
	modelMu.Lock()
	modelsCache = []ModelInfo{
		{ID: "deepseek-v4-flash-free"},
		{ID: "glm-5.2"},
		{ID: "gpt-5.5"},
	}
	goModelsCache = []ModelInfo{
		{ID: "glm-5.2"},
		{ID: "kimi-k2.7-code"},
	}
	modelsLoaded = true
	modelMu.Unlock()
	configMu.Lock()
	modelAliasRules = nil
	configMu.Unlock()
	t.Cleanup(func() {
		modelMu.Lock()
		modelsCache = oldModelsCache
		goModelsCache = oldGoModelsCache
		modelsLoaded = oldModelsLoaded
		modelMu.Unlock()
		applyConfig(AppConfig{ModelAlias: oldModelAlias})
	})

	tests := []struct {
		name       string
		authHeader string
		wantIDs    []string
	}{
		{
			name:    "public only sees free zen models",
			wantIDs: []string{"deepseek-v4-flash"},
		},
		{
			name:       "bare zen key sees zen catalog only",
			authHeader: "Bearer sk-auto0123456789abcdef",
			wantIDs:    []string{"deepseek-v4-flash", "glm-5.2", "gpt-5.5"},
		},
		{
			name:       "go prefix sees free and go catalog",
			authHeader: "Bearer go:sk-go0123456789abcdef",
			wantIDs:    []string{"deepseek-v4-flash", "glm-5.2", "kimi-k2.7-code"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()

			listModelsHandler(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("listModelsHandler() status = %d, want %d", rec.Code, http.StatusOK)
			}
			var payload struct {
				Data []ModelInfo `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("unmarshal models response: %v", err)
			}
			gotIDs := make([]string, 0, len(payload.Data))
			for _, model := range payload.Data {
				gotIDs = append(gotIDs, model.ID)
			}
			if !reflect.DeepEqual(gotIDs, tt.wantIDs) {
				t.Fatalf("listModelsHandler() ids = %#v, want %#v", gotIDs, tt.wantIDs)
			}
		})
	}
}

func TestListModelsHandlerReplacesMappedModelIDsWithAliases(t *testing.T) {
	oldModelsCache := modelsCache
	oldGoModelsCache := goModelsCache
	oldModelsLoaded := modelsLoaded
	oldModelAlias := getModelKeywordRules()
	modelMu.Lock()
	modelsCache = []ModelInfo{
		{ID: "deepseek-v4-flash-free", Object: "model", OwnedBy: "opencode"},
		{ID: "gpt-5.5", Object: "model", OwnedBy: "opencode"},
	}
	goModelsCache = nil
	modelsLoaded = true
	modelMu.Unlock()
	applyConfig(AppConfig{ModelAlias: ModelAliasList{{Keyword: "deepseek-v4-flash", Target: "deepseek-v4-flash-free", MatchType: MatchExact, Enabled: true}}})
	t.Cleanup(func() {
		modelMu.Lock()
		modelsCache = oldModelsCache
		goModelsCache = oldGoModelsCache
		modelsLoaded = oldModelsLoaded
		modelMu.Unlock()
		applyConfig(AppConfig{ModelAlias: oldModelAlias})
	})

	for _, tt := range []struct {
		name       string
		authHeader string
		wantIDs    []string
	}{
		{
			name:    "public sees free alias instead of upstream name",
			wantIDs: []string{"deepseek-v4-flash"},
		},
		{
			name:       "authenticated catalog replaces upstream name",
			authHeader: "Bearer sk-auto0123456789abcdef",
			wantIDs:    []string{"deepseek-v4-flash", "gpt-5.5"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()

			listModelsHandler(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("listModelsHandler() status = %d, want %d", rec.Code, http.StatusOK)
			}
			var payload struct {
				Data []ModelInfo `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("unmarshal models response: %v", err)
			}
			gotIDs := make([]string, 0, len(payload.Data))
			for _, model := range payload.Data {
				gotIDs = append(gotIDs, model.ID)
			}
			if !reflect.DeepEqual(gotIDs, tt.wantIDs) {
				t.Fatalf("listModelsHandler() ids = %#v, want %#v", gotIDs, tt.wantIDs)
			}
		})
	}
}

func TestListModelsHandlerStripsFreeSuffixWithoutAlias(t *testing.T) {
	oldModelsCache := modelsCache
	oldGoModelsCache := goModelsCache
	oldModelsLoaded := modelsLoaded
	oldModelAlias := getModelKeywordRules()
	modelMu.Lock()
	modelsCache = []ModelInfo{
		{ID: "mimo-v2.5-free", Object: "model", OwnedBy: "opencode"},
	}
	goModelsCache = nil
	modelsLoaded = true
	modelMu.Unlock()
	configMu.Lock()
	modelAliasRules = nil
	configMu.Unlock()
	t.Cleanup(func() {
		modelMu.Lock()
		modelsCache = oldModelsCache
		goModelsCache = oldGoModelsCache
		modelsLoaded = oldModelsLoaded
		modelMu.Unlock()
		applyConfig(AppConfig{ModelAlias: oldModelAlias})
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	listModelsHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var payload struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data) != 1 || payload.Data[0].ID != "mimo-v2.5" {
		t.Fatalf("ids = %#v, want [mimo-v2.5]", payload.Data)
	}
}

func TestResolveModelMapsStrippedFreeNameBackToUpstream(t *testing.T) {
	oldModelsCache := modelsCache
	oldGoModelsCache := goModelsCache
	oldModelAlias := getModelKeywordRules()
	modelMu.Lock()
	modelsCache = []ModelInfo{{ID: "mimo-v2.5-free"}}
	goModelsCache = nil
	modelMu.Unlock()
	configMu.Lock()
	modelAliasRules = nil
	configMu.Unlock()
	t.Cleanup(func() {
		modelMu.Lock()
		modelsCache = oldModelsCache
		goModelsCache = oldGoModelsCache
		modelMu.Unlock()
		applyConfig(AppConfig{ModelAlias: oldModelAlias})
	})

	if got := resolveModel("mimo-v2.5"); got != "mimo-v2.5-free" {
		t.Fatalf("resolveModel(mimo-v2.5) = %q, want mimo-v2.5-free", got)
	}
	if got := resolveModel("mimo-v2.5-free"); got != "mimo-v2.5-free" {
		t.Fatalf("resolveModel(mimo-v2.5-free) = %q, want unchanged", got)
	}
}

func TestResolveModelForAuthPrefersGoModelOverMatchingFreeAlias(t *testing.T) {
	oldModelsCache := modelsCache
	oldGoModelsCache := goModelsCache
	oldModelAlias := getModelKeywordRules()
	modelMu.Lock()
	modelsCache = []ModelInfo{
		{ID: "deepseek-v4-flash"},
		{ID: "deepseek-v4-flash-free"},
		{ID: "free-only-free"},
		{ID: "zen-target"},
	}
	goModelsCache = []ModelInfo{{ID: "deepseek-v4-flash"}, {ID: "go-target"}}
	modelMu.Unlock()
	applyConfig(AppConfig{ModelAlias: ModelAliasList{
		{Keyword: "deepseek-v4-flash", Target: "deepseek-v4-flash-free", MatchType: MatchExact, Enabled: true},
		{Keyword: "free-only", Target: "free-only-free", MatchType: MatchExact, Enabled: true},
		{Keyword: "custom-alias", Target: "go-target", MatchType: MatchExact, Enabled: true},
		{Keyword: "zen-alias", Target: "zen-target", MatchType: MatchExact, Enabled: true},
	}})
	t.Cleanup(func() {
		modelMu.Lock()
		modelsCache = oldModelsCache
		goModelsCache = oldGoModelsCache
		modelMu.Unlock()
		applyConfig(AppConfig{ModelAlias: oldModelAlias})
	})

	tests := []struct {
		name   string
		auth   UpstreamAuth
		model  string
		want   string
		wantGo bool
	}{
		{
			name:   "go mode keeps exact go model instead of matching free alias",
			auth:   UpstreamAuth{Mode: AuthRouteGo},
			model:  "deepseek-v4-flash",
			want:   "deepseek-v4-flash",
			wantGo: true,
		},
		{
			name:   "auto mode keeps existing free alias behavior",
			auth:   UpstreamAuth{Mode: AuthRouteAuto},
			model:  "deepseek-v4-flash",
			want:   "deepseek-v4-flash-free",
			wantGo: false,
		},
		{
			name:   "zen mode keeps exact zen model instead of matching free alias",
			auth:   UpstreamAuth{Mode: AuthRouteZen},
			model:  "deepseek-v4-flash",
			want:   "deepseek-v4-flash",
			wantGo: false,
		},
		{
			name:   "go mode uses free alias when exact model is absent from go catalog",
			auth:   UpstreamAuth{Mode: AuthRouteGo},
			model:  "free-only",
			want:   "free-only-free",
			wantGo: false,
		},
		{
			name:   "zen mode uses free alias when exact model is absent from zen catalog",
			auth:   UpstreamAuth{Mode: AuthRouteZen},
			model:  "free-only",
			want:   "free-only-free",
			wantGo: false,
		},
		{
			name:   "go mode preserves ordinary aliases",
			auth:   UpstreamAuth{Mode: AuthRouteGo},
			model:  "custom-alias",
			want:   "go-target",
			wantGo: true,
		},
		{
			name:   "zen mode preserves ordinary aliases",
			auth:   UpstreamAuth{Mode: AuthRouteZen},
			model:  "zen-alias",
			want:   "zen-target",
			wantGo: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveModelForAuth(tt.auth, tt.model)
			if got != tt.want {
				t.Fatalf("resolveModelForAuth(%v, %q) = %q, want %q", tt.auth.Mode, tt.model, got, tt.want)
			}
			if gotGo := tt.auth.shouldUseGoEndpoint(got); gotGo != tt.wantGo {
				t.Fatalf("shouldUseGoEndpoint(%q) = %v, want %v", got, gotGo, tt.wantGo)
			}
		})
	}
}

func TestMapPublicToFreeModel(t *testing.T) {
	oldModelsCache := modelsCache
	oldGoModelsCache := goModelsCache
	modelMu.Lock()
	modelsCache = []ModelInfo{{ID: "deepseek-v4-flash"}, {ID: "deepseek-v4-flash-free"}, {ID: "mimo-v2.5-free"}}
	goModelsCache = nil
	modelMu.Unlock()
	t.Cleanup(func() {
		modelMu.Lock()
		modelsCache = oldModelsCache
		goModelsCache = oldGoModelsCache
		modelMu.Unlock()
	})

	public := UpstreamAuth{Mode: AuthRoutePublic}
	keyed := UpstreamAuth{Mode: AuthRouteAuto, Token: "sk-validkey0123456789abcdef"}

	tests := []struct {
		name  string
		auth  UpstreamAuth
		model string
		want  string
	}{
		{"public paid with free variant maps to free", public, "deepseek-v4-flash", "deepseek-v4-flash-free"},
		{"public free model stays", public, "deepseek-v4-flash-free", "deepseek-v4-flash-free"},
		{"public model without free variant stays", public, "laguna-s-2.1", "laguna-s-2.1"},
		{"keyed paid model stays", keyed, "deepseek-v4-flash", "deepseek-v4-flash"},
		{"empty model stays", public, "", ""},
	}
	for _, tt := range tests {
		if got := mapPublicToFreeModel(tt.auth, tt.model); got != tt.want {
			t.Errorf("%s: mapPublicToFreeModel(%v, %q) = %q, want %q", tt.name, tt.auth.Mode, tt.model, got, tt.want)
		}
	}
}

func TestExtractUpstreamAuthKeyValidation(t *testing.T) {
	tests := []struct {
		name       string
		authHeader string
		apiKey     string
		wantMode   AuthRouteMode
		wantToken  string
		wantSource string
	}{
		{"no header", "", "", AuthRoutePublic, "", "none"},
		{"bearer empty", "Bearer ", "", AuthRoutePublic, "", "none"},
		{"bearer public", "Bearer public", "", AuthRoutePublic, "", "authorization"},
		{"bearer no-key-required placeholder", "Bearer no-key-required", "", AuthRoutePublic, "", "authorization"},
		{"bearer random non-key", "Bearer abc123xyz", "", AuthRoutePublic, "", "authorization"},
		{"valid sk key", "Bearer sk-validkey0123456789abcdef", "", AuthRouteAuto, "sk-validkey0123456789abcdef", "authorization"},
		{"go prefix with sk key", "Bearer go:sk-gokey0123456789abcdef", "", AuthRouteGo, "sk-gokey0123456789abcdef", "authorization"},
		{"zen prefix with sk key", "Bearer zen:sk-zenkey0123456789abcdef", "", AuthRouteZen, "sk-zenkey0123456789abcdef", "authorization"},
		{"go prefix with placeholder falls to public", "Bearer go:no-key-required", "", AuthRoutePublic, "", "authorization"},
		{"bare sk- with no suffix is invalid", "Bearer sk-", "", AuthRoutePublic, "", "authorization"},
		{"x-api-key valid", "", "sk-validkey0123456789abcdef", AuthRouteAuto, "sk-validkey0123456789abcdef", "x-api-key"},
		{"x-api-key sk-local short", "", "sk-local", AuthRoutePublic, "", "x-api-key"},
		{"x-api-key sk-ant rejected", "", "sk-ant-api03-abcdefghijklmnopqrstuvwxyz", AuthRoutePublic, "", "x-api-key"},
		{"x-api-key go prefix", "", "go:sk-gokey0123456789abcdef", AuthRouteGo, "sk-gokey0123456789abcdef", "x-api-key"},
		{"x-api-key zen prefix", "", "zen:sk-zenkey0123456789abcdef", AuthRouteZen, "sk-zenkey0123456789abcdef", "x-api-key"},
		{"bearer wins over x-api-key", "Bearer sk-validkey0123456789abcdef", "sk-otherkey0123456789abcdef", AuthRouteAuto, "sk-validkey0123456789abcdef", "authorization"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			if tt.apiKey != "" {
				req.Header.Set("x-api-key", tt.apiKey)
			}
			auth := extractUpstreamAuth(req)
			if auth.Mode != tt.wantMode {
				t.Fatalf("mode = %v, want %v", auth.Mode, tt.wantMode)
			}
			if auth.Token != tt.wantToken {
				t.Fatalf("token = %q, want %q", auth.Token, tt.wantToken)
			}
			if auth.Source != tt.wantSource {
				t.Fatalf("source = %q, want %q", auth.Source, tt.wantSource)
			}
		})
	}
}
