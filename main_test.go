package main

import (
	"encoding/json"
	"io"
	"net/http"
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

type fakeUpstreamResponse struct {
	status int
	body   string
	header http.Header
}

type fakeRetryTransport struct {
	t               *testing.T
	responses       []fakeUpstreamResponse
	requestedModels []string
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
	oldOCClientVer := ocClientVer
	oldOCSessionID := ocSessionID
	oldOCProjectID := ocProjectID
	oldActiveSocks5 := activeSocks5
	oldSocks5Client := socks5Client
	oldSocks5ClientAddr := socks5ClientAddr

	transport := &fakeRetryTransport{
		t:         t,
		responses: append([]fakeUpstreamResponse(nil), responses...),
	}
	httpClient = &http.Client{Transport: transport}

	modelMu.Lock()
	modelsCache = []ModelInfo{{ID: "fallback-model"}}
	modelMu.Unlock()

	socks5Mu.Lock()
	activeSocks5 = ""
	socks5Client = nil
	socks5ClientAddr = ""
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
		modelMu.Unlock()
		socks5Mu.Lock()
		activeSocks5 = oldActiveSocks5
		socks5Client = oldSocks5Client
		socks5ClientAddr = oldSocks5ClientAddr
		socks5Mu.Unlock()
		ocOnce = sync.Once{}
		ocClientVer = oldOCClientVer
		ocSessionID = oldOCSessionID
		ocProjectID = oldOCProjectID
	})

	return transport
}

func TestCallOpenCodeAPIRetries4xxAndClosesConnectionBeforeRetry(t *testing.T) {
	tests := []struct {
		name        string
		stream      bool
		responses   []fakeUpstreamResponse
		wantStatus  int
		wantBody    string
		wantModels  []string
		wantCloses  int
		requestBody string
	}{
		{
			name:   "non-stream retries 401",
			stream: false,
			responses: []fakeUpstreamResponse{
				{status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`},
				{status: http.StatusOK, body: `{"id":"chatcmpl_test","choices":[]}`},
			},
			wantStatus:  http.StatusOK,
			wantBody:    `{"id":"chatcmpl_test","choices":[]}`,
			wantModels:  []string{"primary-model", "fallback-model"},
			wantCloses:  1,
			requestBody: `{"model":"primary-model","messages":[]}`,
		},
		{
			name:   "stream retries 429",
			stream: true,
			responses: []fakeUpstreamResponse{
				{status: http.StatusTooManyRequests, body: `{"error":"rate_limited"}`},
				{status: http.StatusOK, body: "data: ok\n\n"},
			},
			wantStatus:  http.StatusOK,
			wantBody:    "data: ok\n\n",
			wantModels:  []string{"primary-model", "fallback-model"},
			wantCloses:  1,
			requestBody: `{"model":"primary-model","messages":[],"stream":true}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := installFakeOpenCodeClient(t, tt.responses)

			var (
				body   []byte
				status int
				err    error
			)
			if tt.stream {
				var respBody io.ReadCloser
				respBody, status, _, err = callOpenCodeAPIStream([]byte(tt.requestBody), "primary-model", "public")
				if respBody != nil {
					defer respBody.Close()
				}
				if err == nil {
					body, err = io.ReadAll(respBody)
				}
			} else {
				body, status, _, err = callOpenCodeAPI([]byte(tt.requestBody), "primary-model", "public")
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

func TestCallOpenCodeAPIExhausted4xxReturnsLastUpstreamResponse(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{
			status: http.StatusUnauthorized,
			body:   `{"error":"unauthorized"}`,
			header: http.Header{"X-Upstream-Error": []string{"first"}},
		},
		{
			status: http.StatusForbidden,
			body:   `{"error":"forbidden"}`,
			header: http.Header{"X-Upstream-Error": []string{"last"}},
		},
	})

	body, status, header, err := callOpenCodeAPI([]byte(`{"model":"primary-model","messages":[]}`), "primary-model", "public")
	if err == nil {
		t.Fatal("callOpenCodeAPI() error = nil, want upstream error")
	}
	if status != http.StatusForbidden {
		t.Fatalf("callOpenCodeAPI() status = %d, want %d", status, http.StatusForbidden)
	}
	if string(body) != `{"error":"forbidden"}` {
		t.Fatalf("callOpenCodeAPI() body = %s, want final upstream body", string(body))
	}
	if header.Get("X-Upstream-Error") != "last" {
		t.Fatalf("final header = %q, want last", header.Get("X-Upstream-Error"))
	}
	wantModels := []string{"primary-model", "fallback-model"}
	if !reflect.DeepEqual(transport.requestedModels, wantModels) {
		t.Fatalf("requested models = %#v, want %#v", transport.requestedModels, wantModels)
	}
	if transport.closeIdleCalls != 1 {
		t.Fatalf("CloseIdleConnections calls = %d, want 1", transport.closeIdleCalls)
	}
}
