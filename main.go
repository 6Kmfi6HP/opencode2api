package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var httpClient = &http.Client{
	Timeout: 300 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	},
}

var (
	version = "v0.4.0"
	commit  = "none"
	date    = "unknown"
)

func versionString() string {
	return fmt.Sprintf("opencode2api %s (commit=%s, date=%s)", version, commit, date)
}

// ======================== SOCKS5 代理 ========================

type Socks5Proxy struct {
	Addr     string `json:"addr"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Name     string `json:"name,omitempty"`
}

func socks5Dial(proxy Socks5Proxy) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, target string) (net.Conn, error) {
		conn, err := net.DialTimeout("tcp", proxy.Addr, 10*time.Second)
		if err != nil {
			return nil, fmt.Errorf("socks5 connect to %s: %w", proxy.Addr, err)
		}
		deadline := time.Now().Add(15 * time.Second)
		conn.SetDeadline(deadline)

		// 认证方法协商
		auth := byte(0x00) // no auth
		if proxy.Username != "" {
			auth = 0x02 // username/password
		}
		if _, err := conn.Write([]byte{0x05, 0x01, auth}); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 handshake write: %w", err)
		}
		buf := make([]byte, 2)
		if _, err := io.ReadFull(conn, buf); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 handshake read: %w", err)
		}
		if buf[0] != 0x05 {
			conn.Close()
			return nil, fmt.Errorf("socks5: not socks5 protocol")
		}

		// 用户名/密码认证
		if buf[1] == 0x02 {
			if proxy.Username == "" {
				conn.Close()
				return nil, fmt.Errorf("socks5: server requires auth but no credentials")
			}
			ulen := len(proxy.Username)
			plen := len(proxy.Password)
			authBuf := make([]byte, 3+ulen+plen)
			authBuf[0] = 0x01
			authBuf[1] = byte(ulen)
			copy(authBuf[2:], proxy.Username)
			authBuf[2+ulen] = byte(plen)
			copy(authBuf[3+ulen:], proxy.Password)
			if _, err := conn.Write(authBuf); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5 auth write: %w", err)
			}
			authResp := make([]byte, 2)
			if _, err := io.ReadFull(conn, authResp); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5 auth read: %w", err)
			}
			if authResp[1] != 0x00 {
				conn.Close()
				return nil, fmt.Errorf("socks5: auth failed")
			}
		} else if buf[1] != 0x00 {
			conn.Close()
			return nil, fmt.Errorf("socks5: unsupported auth method 0x%02x", buf[1])
		}

		// CONNECT 请求
		host, portStr, err := net.SplitHostPort(target)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5: invalid target %s: %w", target, err)
		}
		port := 0
		fmt.Sscanf(portStr, "%d", &port)

		req := []byte{0x05, 0x01, 0x00} // VER, CMD=CONNECT, RSV
		ip := net.ParseIP(host)
		if ip != nil {
			if ip4 := ip.To4(); ip4 != nil {
				req = append(req, 0x01) // IPv4
				req = append(req, ip4...)
			} else {
				req = append(req, 0x04) // IPv6
				req = append(req, ip.To16()...)
			}
		} else {
			if len(host) > 255 {
				conn.Close()
				return nil, fmt.Errorf("socks5: hostname too long")
			}
			req = append(req, 0x03) // Domain
			req = append(req, byte(len(host)))
			req = append(req, []byte(host)...)
		}
		req = append(req, byte(port>>8), byte(port))

		if _, err := conn.Write(req); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 connect write: %w", err)
		}

		// 读取响应
		resp := make([]byte, 4)
		if _, err := io.ReadFull(conn, resp); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 connect read: %w", err)
		}
		if resp[1] != 0x00 {
			conn.Close()
			return nil, fmt.Errorf("socks5: connect failed, status 0x%02x", resp[1])
		}

		// 读取绑定地址
		switch resp[3] {
		case 0x01: // IPv4
			if _, err := io.ReadFull(conn, make([]byte, 4+2)); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5: read bind ipv4: %w", err)
			}
		case 0x03: // Domain
			dlen := make([]byte, 1)
			if _, err := io.ReadFull(conn, dlen); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5: read bind domain len: %w", err)
			}
			if _, err := io.ReadFull(conn, make([]byte, int(dlen[0])+2)); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5: read bind domain: %w", err)
			}
		case 0x04: // IPv6
			if _, err := io.ReadFull(conn, make([]byte, 16+2)); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5: read bind ipv6: %w", err)
			}
		default:
			conn.Close()
			return nil, fmt.Errorf("socks5: unknown address type 0x%02x", resp[3])
		}

		conn.SetDeadline(time.Time{})
		return conn, nil
	}
}

var (
	socks5Proxies    []Socks5Proxy
	activeSocks5     string // 启用的代理 Addr，空表示直连，__round_robin__ 表示轮询
	socks5PaidDirect bool   // true=带 key/付费直连；false/缺省=全部走代理
	socks5Mu         sync.RWMutex
)

const socks5RR = "__round_robin__"

var socks5RRIndex uint32

var (
	socks5Client     *http.Client // 缓存的 SOCKS5 客户端
	socks5ClientAddr string       // 缓存对应的代理地址
)

func getHTTPClient() *http.Client {
	socks5Mu.RLock()
	defer socks5Mu.RUnlock()

	if activeSocks5 == "" {
		return httpClient
	}

	var proxy Socks5Proxy
	var useRR bool

	if activeSocks5 == socks5RR {
		if len(socks5Proxies) == 0 {
			return httpClient
		}
		idx := atomic.AddUint32(&socks5RRIndex, 1) % uint32(len(socks5Proxies))
		proxy = socks5Proxies[idx]
		useRR = true
	} else {
		if socks5Client != nil && socks5ClientAddr == activeSocks5 {
			return socks5Client
		}

		var found bool
		for i := range socks5Proxies {
			if socks5Proxies[i].Addr == activeSocks5 {
				proxy = socks5Proxies[i]
				found = true
				break
			}
		}
		if !found {
			return httpClient
		}
	}

	dial := socks5Dial(proxy)
	client := &http.Client{
		Timeout: 300 * time.Second,
		Transport: &http.Transport{
			DialContext:         dial,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	if !useRR {
		socks5Client = client
		socks5ClientAddr = activeSocks5
	}
	return client
}

// getHTTPClientForTier 按认证层级选择 HTTP 客户端。
// 默认（socks5_paid_direct 未填或 false）：只要配置了 active_socks5，付费/带 key 与 public 都走代理。
// socks5_paid_direct=true 时恢复旧行为：付费层直连，仅免费层走代理。
func getHTTPClientForTier(tier TierType) *http.Client {
	if tier == TierPaid && getSocks5PaidDirect() {
		return httpClient
	}
	return getHTTPClient()
}

func getSocks5PaidDirect() bool {
	socks5Mu.RLock()
	defer socks5Mu.RUnlock()
	return socks5PaidDirect
}

// ======================== 随机 ID ========================

func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = letters[b[i]%byte(len(letters))]
	}
	return string(b)
}

func randomHex(n int) string {
	const hex = "0123456789abcdef"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = hex[b[i]%byte(len(hex))]
	}
	return string(b)
}

// ======================== OpenCode 会话 ========================

var (
	ocSessionID  string
	ocProjectID  string
	ocClientVer  string
	ocOnce       sync.Once
	requestCount atomic.Int64
)

func fetchOCVersion() string {
	req, _ := http.NewRequest("GET", "https://registry.npmjs.org/opencode-ai/latest", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := getHTTPClient().Do(req)
	if err != nil {
		return "1.15.3"
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var info struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(body, &info) == nil && info.Version != "" {
		return info.Version
	}
	return "1.15.3"
}

func initOCSession() {
	ocOnce.Do(func() {
		ocClientVer = fetchOCVersion()
		ocSessionID = "ses_" + randomString(24)
		ocProjectID = randomHex(40)
		slog.Info("opencode version", "version", ocClientVer)
		slog.Info("session initialized", "session_id", ocSessionID)
		slog.Info("project initialized", "project_id", ocProjectID)
	})
}

func refreshOCSession() {
	ocClientVer = fetchOCVersion()
	ocSessionID = "ses_" + randomString(24)
	ocProjectID = randomHex(40)
	slog.Info("session refreshed", "version", ocClientVer, "session_id", ocSessionID)
	// 重置 Once 以便后续 initOCSession 调用直接通过
	ocOnce = sync.Once{}
}

// ======================== 模型 ========================

type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

var (
	modelsCache   []ModelInfo
	goModelsCache []ModelInfo
	modelMu       sync.RWMutex
	modelsLoaded  bool
)

func fetchModels() ([]ModelInfo, error) {
	req, _ := http.NewRequest("GET", "https://opencode.ai/zen/v1/models", nil)
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("x-opencode-session", ocSessionID)
	resp, err := getHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	var models []ModelInfo
	now := time.Now().Unix()
	for _, m := range result.Data {
		models = append(models, ModelInfo{ID: m.ID, Object: "model", Created: now, OwnedBy: "opencode"})
	}
	return models, nil
}

func fetchGoModels() ([]ModelInfo, error) {
	req, _ := http.NewRequest("GET", "https://opencode.ai/zen/go/v1/models", nil)
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("x-opencode-session", ocSessionID)
	resp, err := getHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	var models []ModelInfo
	now := time.Now().Unix()
	for _, m := range result.Data {
		models = append(models, ModelInfo{ID: m.ID, Object: "model", Created: now, OwnedBy: "opencode"})
	}
	return models, nil
}

func containsModelWithID(models []ModelInfo, modelID string) bool {
	for _, model := range models {
		if model.ID == modelID {
			return true
		}
	}
	return false
}

func isModelInGoCatalog(modelID string) bool {
	modelMu.RLock()
	defer modelMu.RUnlock()
	return containsModelWithID(goModelsCache, modelID)
}

func isGoCatalogOnlyModel(modelID string) bool {
	modelMu.RLock()
	defer modelMu.RUnlock()
	return containsModelWithID(goModelsCache, modelID) && !containsModelWithID(modelsCache, modelID)
}

func getModelIDs() []string {
	modelMu.RLock()
	defer modelMu.RUnlock()
	ids := make([]string, len(modelsCache))
	for i, m := range modelsCache {
		ids[i] = m.ID
	}
	return ids
}

func getGoModelIDs() []string {
	modelMu.RLock()
	defer modelMu.RUnlock()
	ids := make([]string, len(goModelsCache))
	for i, m := range goModelsCache {
		ids[i] = m.ID
	}
	return ids
}

// isNonRetryableUpstreamError reports billing/credits failures that must not
// trigger retries.
func isNonRetryableUpstreamError(status int, body []byte) bool {
	if status != http.StatusUnauthorized && status != http.StatusPaymentRequired && status != http.StatusForbidden {
		return false
	}
	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	errType := strings.ToLower(strings.TrimSpace(payload.Error.Type))
	if errType == "" {
		errType = strings.ToLower(strings.TrimSpace(payload.Type))
	}
	if errType == "creditserror" || errType == "insufficient_quota" || errType == "billing_error" {
		return true
	}
	msg := strings.ToLower(payload.Error.Message)
	return strings.Contains(msg, "insufficient balance") || strings.Contains(msg, "insufficient credits")
}

// startModelRefresh 定时刷新模型列表（每 10 分钟）
func startModelRefresh() {
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			fetched, err := fetchModels()
			if err == nil && len(fetched) > 0 {
				modelMu.Lock()
				modelsCache = fetched
				modelsLoaded = true
				modelMu.Unlock()
				slog.Info("models auto-refreshed", "count", len(fetched))
			} else if err != nil {
				slog.Error("free models refresh failed", "error", err)
			}

			goFetched, goErr := fetchGoModels()
			if goErr == nil && len(goFetched) > 0 {
				modelMu.Lock()
				goModelsCache = goFetched
				modelMu.Unlock()
				slog.Info("go catalog auto-refreshed", "count", len(goFetched))
			} else if goErr != nil {
				slog.Error("go catalog refresh failed", "error", goErr)
			}
		}
	}()
}

// ======================== 结构化日志 ========================

type contextKey string

const reqIDKey contextKey = "request_id"

func getReqID(ctx context.Context) string {
	if id, ok := ctx.Value(reqIDKey).(string); ok {
		return id
	}
	return ""
}

// ======================== 配置 ========================

var (
	port                 string
	configPath           = "config.json"
	modelAlias           = map[string]string{}
	reasoningEffortMap   = map[string]string{}
	forceDisableThinking bool
	maxTokensCap         int
	maxTokensCapPerModel = map[string]int{}
	debugMode            bool
	configMu             sync.RWMutex
	storedResponses      = map[string]StoredResponseState{}
	storedResponsesMu    sync.RWMutex
)

// ======================== 管理面板认证 ========================

var (
	adminPassword string
	sessions      = map[string]struct{}{}
	sessionsMu    sync.Mutex
)

func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if adminPassword == "" {
			next(w, r)
			return
		}
		cookie, err := r.Cookie("session")
		if err != nil || cookie.Value == "" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		sessionsMu.Lock()
		_, ok := sessions[cookie.Value]
		sessionsMu.Unlock()
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	if adminPassword == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			renderLoginPage(w, "表单解析失败")
			return
		}
		if r.FormValue("password") != adminPassword {
			renderLoginPage(w, "密码错误")
			return
		}
		token, err := generateToken()
		if err != nil {
			renderLoginPage(w, "创建会话失败")
			return
		}
		sessionsMu.Lock()
		sessions[token] = struct{}{}
		sessionsMu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "session", Value: token, Path: "/", HttpOnly: true})
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	renderLoginPage(w, "")
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	cookie, err := r.Cookie("session")
	if err == nil && cookie.Value != "" {
		sessionsMu.Lock()
		delete(sessions, cookie.Value)
		sessionsMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "session", Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusFound)
}

// ======================== Token 统计 ========================

type ModelStats struct {
	RequestCount     int64 `json:"request_count"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type TokenStatsData struct {
	TotalRequests int64                  `json:"total_requests"`
	Models        map[string]*ModelStats `json:"models"`
}

var (
	tokenStats     = &TokenStatsData{Models: map[string]*ModelStats{}}
	tokenStatsMu   sync.Mutex
	tokenStatsPath = "stats.json"
)

// ======================== 数据模型 ========================

type OpenAIRequest struct {
	Model           string         `json:"model"`
	Messages        []Message      `json:"messages"`
	Stream          bool           `json:"stream"`
	Temperature     *float64       `json:"temperature,omitempty"`
	MaxTokens       *int           `json:"max_tokens,omitempty"`
	TopP            *float64       `json:"top_p,omitempty"`
	Thinking        any            `json:"thinking,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
	ExtraBody       map[string]any `json:"extra_body,omitempty"`
	Tools           []Tool         `json:"tools,omitempty"`
	ToolChoice      any            `json:"tool_choice,omitempty"`
}

type Message struct {
	Role             string     `json:"role,omitempty"`
	Content          any        `json:"content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	Name             string     `json:"name,omitempty"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type AppConfig struct {
	ModelAlias           map[string]string `json:"model_alias"`
	ReasoningEffortMap   map[string]string `json:"reasoning_effort_map"`
	ForceDisableThinking bool              `json:"force_disable_thinking"`
	// MaxTokensCap is the global default upper bound for max_tokens sent
	// upstream. 0 (default) means no global cap. Per-model values in
	// MaxTokensCapPerModel take precedence.
	MaxTokensCap int `json:"max_tokens_cap,omitempty"`
	// MaxTokensCapPerModel overrides the global cap for specific models.
	// A value of 0 for a model disables the cap for that model.
	MaxTokensCapPerModel map[string]int `json:"max_tokens_cap_per_model,omitempty"`
	Socks5Proxies        []Socks5Proxy  `json:"socks5_proxies,omitempty"`
	ActiveSocks5         string         `json:"active_socks5,omitempty"`
	// Socks5PaidDirect controls whether keyed/paid upstream calls bypass SOCKS5.
	// Omitted or false (default): all traffic uses the active proxy.
	// true: paid/keyed traffic goes direct; only public/free uses SOCKS5.
	Socks5PaidDirect bool `json:"socks5_paid_direct,omitempty"`
}

// ======================== Claude Messages API 类型 ========================

type ClaudeRequest struct {
	Model             string          `json:"model"`
	Messages          []ClaudeMessage `json:"messages"`
	System            any             `json:"system,omitempty"`
	MaxTokens         *int            `json:"max_tokens,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	TopK              *int            `json:"top_k,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	Tools             []ClaudeTool    `json:"tools,omitempty"`
	ToolChoice        any             `json:"tool_choice,omitempty"`
	StopSequences     []string        `json:"stop_sequences,omitempty"`
	Metadata          any             `json:"metadata,omitempty"`
	Thinking          any             `json:"thinking,omitempty"`
	OutputConfig      any             `json:"output_config,omitempty"`
	ContextManagement any             `json:"context_management,omitempty"`
}

type ClaudeMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type ClaudeContent struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     any    `json:"input,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"`
}

type ClaudeTool struct {
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	InputSchema  any    `json:"input_schema"`
	Type         string `json:"type,omitempty"`
	CacheControl any    `json:"cache_control,omitempty"`
}

type ClaudeResponse struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Role       string          `json:"role"`
	Content    []ClaudeContent `json:"content"`
	Model      string          `json:"model"`
	StopReason string          `json:"stop_reason"`
	Usage      ClaudeUsage     `json:"usage,omitempty"`
}

type ClaudeUsage map[string]any

// ======================== Responses API 类型 ========================

type ResponsesAPIRequest struct {
	Model              string          `json:"model"`
	Input              any             `json:"input"`
	Messages           []Message       `json:"messages,omitempty"`
	Instructions       string          `json:"instructions,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	MaxTokens          *int            `json:"max_output_tokens,omitempty"`
	TopP               *float64        `json:"top_p,omitempty"`
	FrequencyPenalty   *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty    *float64        `json:"presence_penalty,omitempty"`
	Reasoning          ReasonEffort    `json:"reasoning,omitempty"`
	Include            []string        `json:"include,omitempty"`
	Store              *bool           `json:"store,omitempty"`
	Tools              []ResponsesTool `json:"tools,omitempty"`
	ToolChoice         any             `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls,omitempty"`
	Stop               any             `json:"stop,omitempty"`
	User               string          `json:"user,omitempty"`
	StreamOptions      any             `json:"stream_options,omitempty"`
	Metadata           any             `json:"metadata,omitempty"`
	Text              any             `json:"text,omitempty"`
	Truncation        string          `json:"truncation,omitempty"`
	ServiceTier       string          `json:"service_tier,omitempty"`
	PromptCacheKey    string          `json:"prompt_cache_key,omitempty"`
	SafetyIdentifier  any             `json:"safety_identifier,omitempty"`
	TopLogprobs       *int            `json:"top_logprobs,omitempty"`
}

type ResponsesTool struct {
	Type            string         `json:"type"`
	Name            string         `json:"name,omitempty"`
	Description     string         `json:"description,omitempty"`
	Parameters      map[string]any `json:"parameters,omitempty"`
	Function        *ToolFunction  `json:"function,omitempty"`
	ServerLabel     string         `json:"server_label,omitempty"`
	ServerURL       string         `json:"server_url,omitempty"`
	ConnectorID     string         `json:"connector_id,omitempty"`
	Authorization   string         `json:"authorization,omitempty"`
	AllowedTools    []string       `json:"allowed_tools,omitempty"`
	RequireApproval any            `json:"require_approval,omitempty"`
}

type ReasonEffort struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
	Mode    string `json:"mode,omitempty"`
}

type StoredResponseState struct {
	Model        string          `json:"model"`
	Instructions string          `json:"instructions,omitempty"`
	Tools        []ResponsesTool `json:"tools,omitempty"`
	ToolChoice   any             `json:"tool_choice,omitempty"`
	Output       []any           `json:"output,omitempty"`
}

// ======================== 配置管理 ========================

func loadConfig(path string) AppConfig {
	var cfg AppConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		slog.Warn("config parse failed", "error", err)
	}
	return cfg
}

func saveConfig(path string, cfg AppConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func applyConfig(cfg AppConfig) {
	configMu.Lock()
	defer configMu.Unlock()
	if cfg.ModelAlias != nil {
		modelAlias = cfg.ModelAlias
	}
	if cfg.ReasoningEffortMap != nil {
		reasoningEffortMap = cfg.ReasoningEffortMap
	}
	forceDisableThinking = cfg.ForceDisableThinking
	maxTokensCap = cfg.MaxTokensCap
	if cfg.MaxTokensCapPerModel != nil {
		maxTokensCapPerModel = cfg.MaxTokensCapPerModel
	}

	socks5Mu.Lock()
	if cfg.Socks5Proxies != nil {
		socks5Proxies = cfg.Socks5Proxies
	}
	if activeSocks5 != cfg.ActiveSocks5 {
		activeSocks5 = cfg.ActiveSocks5
		socks5Client = nil
		socks5ClientAddr = ""
		atomic.StoreUint32(&socks5RRIndex, 0)
	}
	socks5PaidDirect = cfg.Socks5PaidDirect
	socks5Mu.Unlock()

}

func resolveModel(model string) string {
	m := strings.TrimSpace(model)
	configMu.RLock()
	alias, ok := modelAlias[m]
	configMu.RUnlock()
	if ok {
		return alias
	}
	// Clients see free models without the "-free" suffix from /v1/models.
	// Map the display name back to the upstream free ID when that is the only match.
	if m != "" && !isFreeModel(m) {
		freeID := m + "-free"
		if !modelExistsInCaches(m) && modelExistsInCaches(freeID) {
			return freeID
		}
	}
	return m
}

func getForceDisableThinking() bool {
	configMu.RLock()
	defer configMu.RUnlock()
	return forceDisableThinking
}

func getReasoningEffortMap() map[string]string {
	configMu.RLock()
	defer configMu.RUnlock()
	cp := make(map[string]string, len(reasoningEffortMap))
	for k, v := range reasoningEffortMap {
		cp[k] = v
	}
	return cp
}

// getMaxTokensCapForModel returns the effective max_tokens cap for the given
// model: the per-model value if set, otherwise the global default. A return
// value of 0 means no cap (max_tokens is forwarded as-is).
func getMaxTokensCapForModel(model string) int {
	configMu.RLock()
	defer configMu.RUnlock()
	if cap, ok := maxTokensCapPerModel[model]; ok {
		return cap
	}
	return maxTokensCap
}

// ======================== Token 统计 ========================

func loadTokenStats() {
	data, err := os.ReadFile(tokenStatsPath)
	if err != nil {
		return
	}
	var st TokenStatsData
	if err := json.Unmarshal(data, &st); err != nil {
		return
	}
	tokenStatsMu.Lock()
	if st.Models == nil {
		st.Models = map[string]*ModelStats{}
	}
	tokenStats = &st
	tokenStatsMu.Unlock()
}

func saveTokenStats() {
	tokenStatsMu.Lock()
	data, err := json.MarshalIndent(tokenStats, "", "  ")
	tokenStatsMu.Unlock()
	if err != nil {
		return
	}
	os.WriteFile(tokenStatsPath, data, 0644)
}

func recordTokenUsage(model string, promptTokens, completionTokens, totalTokens int64) {
	tokenStatsMu.Lock()
	tokenStats.TotalRequests++
	ms, ok := tokenStats.Models[model]
	if !ok {
		ms = &ModelStats{}
		tokenStats.Models[model] = ms
	}
	ms.RequestCount++
	ms.PromptTokens += promptTokens
	ms.CompletionTokens += completionTokens
	ms.TotalTokens += totalTokens
	tokenStatsMu.Unlock()
	go saveTokenStats()
}

// ======================== Thinking/Reasoning 判断 ========================

func isThinkingEnabled(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		t, _ := v["type"].(string)
		// Claude Code sends adaptive thinking with --effort / CLAUDE_CODE_EFFORT_LEVEL.
		return t == "enabled" || t == "adaptive"
	case bool:
		return v
	default:
		return false
	}
}

// effortFromOutputConfig reads Claude Code's output_config.effort
// (set by --effort / CLAUDE_CODE_EFFORT_LEVEL).
func effortFromOutputConfig(value any) string {
	m, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	effort, _ := m["effort"].(string)
	return strings.TrimSpace(effort)
}

func isThinkingDisabled(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		t, _ := v["type"].(string)
		return t == "disabled"
	case bool:
		return !v
	default:
		return false
	}
}

// buildUpstreamThinking preserves budget_tokens / effort fields when present.
func buildUpstreamThinking(value any) map[string]any {
	out := map[string]any{"type": "enabled"}
	m, ok := value.(map[string]any)
	if !ok {
		return out
	}
	for _, key := range []string{"budget_tokens", "effort"} {
		if v, exists := m[key]; exists && v != nil {
			out[key] = v
		}
	}
	return out
}

// reasoningEffortFromThinking maps Anthropic-style budget_tokens onto an
// OpenAI-compatible reasoning_effort when the client did not set one explicitly.
func reasoningEffortFromThinking(value any) string {
	m, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	if effort, ok := m["effort"].(string); ok && effort != "" {
		return effort
	}
	var budget float64
	switch v := m["budget_tokens"].(type) {
	case float64:
		budget = v
	case int:
		budget = float64(v)
	case int64:
		budget = float64(v)
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return ""
		}
		budget = f
	default:
		return ""
	}
	switch {
	case budget <= 0:
		return ""
	case budget < 2048:
		return "low"
	case budget < 8192:
		return "medium"
	case budget < 16384:
		return "high"
	default:
		return "xhigh"
	}
}

func wantsReasoning(req *OpenAIRequest) bool {
	if getForceDisableThinking() {
		return false
	}
	if isThinkingDisabled(req.Thinking) {
		return false
	}
	if isThinkingEnabled(req.Thinking) {
		return true
	}
	if req.ExtraBody != nil {
		if isThinkingDisabled(req.ExtraBody["thinking"]) {
			return false
		}
		if isThinkingEnabled(req.ExtraBody["thinking"]) {
			return true
		}
	}
	return true
}

// ======================== 消息处理 ========================
// normalizeContent 是 dumb pipe 透传：保留 string 与 []any 两种入参形状
// （其它非常规类型走 json.Marshal 兜底），不解析或过滤任何 multimodal part。
// 能力协商由 opencode 客户端 + 上游负责；这里既不"硬降级"也不"补全"。
func normalizeContent(content any) any {
	if content == nil {
		return nil
	}
	if s, ok := content.(string); ok {
		return s
	}
	if arr, ok := content.([]any); ok {
		return arr
	}
	b, err := json.Marshal(content)
	if err != nil {
		return nil
	}
	return string(b)
}

func fixToolCallGaps(messages []Message) []Message {
	toolResponses := map[string]*Message{}
	for i := range messages {
		if messages[i].Role == "tool" && messages[i].ToolCallID != "" {
			toolResponses[messages[i].ToolCallID] = &messages[i]
		}
	}
	fixed := make([]Message, 0, len(messages)+len(messages)/4)
	emitted := map[string]bool{}
	for _, msg := range messages {
		if msg.Role == "tool" && msg.ToolCallID != "" {
			if emitted[msg.ToolCallID] {
				continue
			}
		}
		fixed = append(fixed, msg)
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				if resp, found := toolResponses[tc.ID]; found {
					fixed = append(fixed, *resp)
				} else {
					fixed = append(fixed, Message{Role: "tool", ToolCallID: tc.ID, Content: "Tool call result not available"})
				}
				emitted[tc.ID] = true
			}
		}
	}
	return fixed
}

func ensureReasoningContent(messages []Message, thinking bool) []Message {
	if !thinking {
		return messages
	}
	for i := range messages {
		if messages[i].Role == "assistant" && messages[i].ReasoningContent == nil {
			empty := ""
			messages[i].ReasoningContent = &empty
		}
	}
	return messages
}

func convertMessagesForUpstream(messages []Message) []map[string]any {
	converted := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		clean := map[string]any{}
		if msg.Role != "" {
			clean["role"] = msg.Role
		}
		content := normalizeContent(msg.Content)
		reasoningContent := msg.ReasoningContent
		if content != nil {
			clean["content"] = content
		}
		if reasoningContent != nil {
			clean["reasoning_content"] = *reasoningContent
		}
		if len(msg.ToolCalls) > 0 {
			clean["tool_calls"] = msg.ToolCalls
		}
		if msg.ToolCallID != "" {
			clean["tool_call_id"] = msg.ToolCallID
		}
		if msg.Name != "" {
			clean["name"] = msg.Name
		}
		converted = append(converted, clean)
	}
	return converted
}

// ======================== 完整请求转换（含 thinking/reasoning_effort/ExtraBody） ========================

func convertRequest(req *OpenAIRequest) map[string]any {
	converted := map[string]any{
		"model":    req.Model,
		"messages": convertMessagesForUpstream(req.Messages),
		"stream":   req.Stream,
	}
	if req.Temperature != nil {
		converted["temperature"] = *req.Temperature
	}
	if req.MaxTokens != nil {
		v := *req.MaxTokens
		if cap := getMaxTokensCapForModel(req.Model); cap > 0 && v > cap {
			v = cap
		}
		converted["max_tokens"] = v
	}
	if req.TopP != nil {
		converted["top_p"] = *req.TopP
	}
	if len(req.Tools) > 0 {
		converted["tools"] = req.Tools
	}
	if req.ToolChoice != nil {
		converted["tool_choice"] = req.ToolChoice
	}
	// 处理思维模式 — 仅当用户显式指定时才发送，避免 MiniMax 等模型报错
	if getForceDisableThinking() || isThinkingDisabled(req.Thinking) {
		converted["thinking"] = map[string]string{"type": "disabled"}
	} else if req.Thinking != nil && isThinkingEnabled(req.Thinking) {
		converted["thinking"] = buildUpstreamThinking(req.Thinking)
	} else if req.ExtraBody != nil {
		if isThinkingDisabled(req.ExtraBody["thinking"]) {
			converted["thinking"] = map[string]string{"type": "disabled"}
		} else if isThinkingEnabled(req.ExtraBody["thinking"]) {
			converted["thinking"] = buildUpstreamThinking(req.ExtraBody["thinking"])
		}
	}
	// 处理 reasoning_effort（含从 thinking.budget_tokens 推导）
	effort := req.ReasoningEffort
	if effort == "" && !isThinkingDisabled(req.Thinking) {
		effort = reasoningEffortFromThinking(req.Thinking)
	}
	if !getForceDisableThinking() && effort != "" {
		effortMap := getReasoningEffortMap()
		if mapped, ok := effortMap[effort]; ok {
			converted["reasoning_effort"] = mapped
		} else {
			converted["reasoning_effort"] = effort
		}
	}
	// 合并 ExtraBody
	if req.ExtraBody != nil {
		for k, v := range req.ExtraBody {
			if _, exists := converted[k]; !exists {
				converted[k] = v
			}
		}
	}
	return converted
}

func buildUpstreamBody(req *OpenAIRequest) []byte {
	converted := convertRequest(req)
	b, err := json.Marshal(converted)
	if err != nil {
		slog.Error("marshal upstream body failed", "error", err)
	}
	return b
}

// ======================== Anthropic 格式兼容 ========================

func isAnthropicFormat(body []byte) bool {
	var obj map[string]any
	if json.Unmarshal(body, &obj) == nil {
		if typ, _ := obj["type"].(string); typ == "message" {
			return true
		}
	}
	lines := bytes.Split(body, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		// Support "data: " prefixed SSE lines.
		if bytes.HasPrefix(line, []byte("data: ")) {
			line = bytes.TrimSpace(line[6:])
		} else if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(line[5:])
		}
		if len(line) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		typ, _ := event["type"].(string)
		switch typ {
		case "message_start", "content_block_start", "content_block_delta",
			"content_block_stop", "message_delta", "message_stop", "ping",
			"error":
			return true
		}
		return false
	}
	return false
}

// anthropicBlockState tracks per-index content block reconstruction.
type anthropicBlockState struct {
	blockType     string
	id            string
	name          string
	signature     string
	data          string
	textBuilder   strings.Builder
	thinkBuilder  strings.Builder
	jsonBuilder   strings.Builder
	initialInput  any
	sawInputDelta bool
	started       bool
	stopped       bool
}

// mergeUsageMaps merges src into dst. Anthropic usage values are snapshots /
// cumulative: a field present in src always replaces the value in dst (including
// 0). Nested maps are recursively merged. Fields absent from src are retained.
func mergeUsageMaps(dst any, src map[string]any) map[string]any {
	if src == nil {
		if dm, ok := dst.(map[string]any); ok {
			return dm
		}
		return nil
	}
	var result map[string]any
	if dm, ok := dst.(map[string]any); ok {
		result = make(map[string]any, len(dm))
		for k, v := range dm {
			result[k] = v
		}
	} else {
		result = map[string]any{}
	}
	for k, v := range src {
		if existing, ok := result[k]; ok {
			if srcMap, ok := v.(map[string]any); ok {
				if existing != nil {
					result[k] = mergeUsageMaps(existing, srcMap)
					continue
				}
			}
		}
		result[k] = v
	}
	return result
}

// parseAnthropicSSE reconstructs content blocks from Anthropic SSE events.
// It manages parallel blocks by index, handles text_delta, thinking_delta,
// signature_delta, and input_json_delta. Returns the reconstructed message,
// ordered content blocks (sorted by numeric index ascending), and an error
// if the stream is malformed/truncated.
//
// Supported line formats:
//   - raw JSON per line (one event per line)
//   - standard SSE: "data: <json>", "event: <name>", comment lines starting
//     with ":" (ignored as metadata)
//
// Malformed/truncated conditions that return an error:
//   - missing message_stop
//   - error event from upstream
//   - malformed event JSON
//   - delta for an unknown/un-started index
//   - content_block_stop for an unknown/un-started index
//   - duplicate content_block_start for the same index
//   - message_stop with unclosed (not-yet-stopped) blocks
//   - malformed tool_use input JSON
func parseAnthropicSSE(body []byte) (map[string]any, []map[string]any, error) {
	lines := bytes.Split(body, []byte("\n"))
	var anthropicMsg map[string]any
	blocks := map[int]*anthropicBlockState{}
	sawMessageStop := false
	messageStartCount := 0

	for _, rawLine := range lines {
		line := bytes.TrimSpace(rawLine)
		if len(line) == 0 {
			continue
		}
		// Standard SSE metadata lines: "event: ...", "id: ...", comment ": ..."
		if bytes.HasPrefix(line, []byte("event:")) ||
			bytes.HasPrefix(line, []byte("id:")) ||
			bytes.HasPrefix(line, []byte(":")) {
			continue
		}
		// Support "data: " prefixed SSE lines.
		if bytes.HasPrefix(line, []byte("data: ")) {
			line = bytes.TrimSpace(line[6:])
		} else if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(line[5:])
		}
		if len(line) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, nil, fmt.Errorf("malformed SSE event JSON: %w", err)
		}
		typ, _ := event["type"].(string)

		// After message_stop, only ping events and comment/metadata lines
		// are allowed. Comment/metadata lines are already filtered above.
		// Any other event is an error.
		if sawMessageStop && typ != "ping" {
			return nil, nil, fmt.Errorf("unexpected event %q after message_stop", typ)
		}

		switch typ {
		case "message_start":
			messageStartCount++
			if messageStartCount > 1 {
				return nil, nil, fmt.Errorf("multiple message_start events in SSE stream")
			}
			m, ok := event["message"].(map[string]any)
			if !ok || m == nil {
				return nil, nil, fmt.Errorf("message_start missing non-nil message object")
			}
			anthropicMsg = m
		case "content_block_start":
			if messageStartCount == 0 {
				return nil, nil, fmt.Errorf("content_block_start before message_start")
			}
			idx, ok := extractBlockIndex(event)
			if !ok {
				return nil, nil, fmt.Errorf("content_block_start missing valid non-negative integer index")
			}
			if existing, ok := blocks[idx]; ok && existing.started {
				return nil, nil, fmt.Errorf("duplicate content_block_start for index %d", idx)
			}
			cb, _ := event["content_block"].(map[string]any)
			cbType, _ := cb["type"].(string)
			if cbType == "" {
				return nil, nil, fmt.Errorf("content_block_start missing content_block type")
			}
			if cbType != "text" && cbType != "thinking" && cbType != "redacted_thinking" && cbType != "tool_use" {
				return nil, nil, fmt.Errorf("content_block_start unsupported type %q", cbType)
			}
			st := &anthropicBlockState{blockType: cbType, started: true}
			if cb != nil {
				if id, ok := cb["id"].(string); ok {
					st.id = id
				}
				if name, ok := cb["name"].(string); ok {
					st.name = name
				}
				// tool_use must have a non-empty name.
				if cbType == "tool_use" && st.name == "" {
					return nil, nil, fmt.Errorf("tool_use content_block_start missing non-empty name")
				}
				if sig, ok := cb["signature"].(string); ok {
					st.signature = sig
				}
				if d, ok := cb["data"].(string); ok {
					st.data = d
				}
				// Preserve initial text if provided.
				if t, ok := cb["text"].(string); ok && t != "" {
					st.textBuilder.WriteString(t)
				}
				if t, ok := cb["thinking"].(string); ok && t != "" {
					st.thinkBuilder.WriteString(t)
				}
				// Preserve initial input if provided as a non-empty value.
				// In Anthropic SSE, content_block_start.input is typically {}
				// and the actual input arrives via input_json_delta partials.
				// Store separately so initial input and partial deltas are not
				// concatenated into invalid JSON.
				if input, ok := cb["input"]; ok && input != nil {
					if inputStr, ok := input.(string); ok && inputStr != "" {
						st.initialInput = inputStr
					} else if m, ok := input.(map[string]any); ok && len(m) > 0 {
						st.initialInput = input
					}
				}
			}
			blocks[idx] = st
		case "content_block_delta":
			if messageStartCount == 0 {
				return nil, nil, fmt.Errorf("content_block_delta before message_start")
			}
			idx, ok := extractBlockIndex(event)
			if !ok {
				return nil, nil, fmt.Errorf("content_block_delta missing valid non-negative integer index")
			}
			st, ok := blocks[idx]
			if !ok || !st.started {
				return nil, nil, fmt.Errorf("content_block_delta for unknown index %d", idx)
			}
			if st.stopped {
				return nil, nil, fmt.Errorf("content_block_delta for already-stopped index %d", idx)
			}
			delta, ok := event["delta"].(map[string]any)
			if !ok || delta == nil {
				return nil, nil, fmt.Errorf("content_block_delta for index %d missing delta object", idx)
			}
			dt, _ := delta["type"].(string)
			if dt == "" {
				return nil, nil, fmt.Errorf("content_block_delta for index %d missing delta type", idx)
			}
			switch dt {
			case "text_delta":
				if t, ok := delta["text"].(string); ok {
					st.textBuilder.WriteString(t)
				}
			case "thinking_delta":
				if t, ok := delta["thinking"].(string); ok {
					st.thinkBuilder.WriteString(t)
				}
			case "signature_delta":
				if sig, ok := delta["signature"].(string); ok {
					st.signature += sig
				}
			case "input_json_delta":
				if partial, ok := delta["partial_json"].(string); ok {
					st.jsonBuilder.WriteString(partial)
					st.sawInputDelta = true
				}
			default:
				// Unknown delta type: ignore.
			}
		case "content_block_stop":
			if messageStartCount == 0 {
				return nil, nil, fmt.Errorf("content_block_stop before message_start")
			}
			idx, ok := extractBlockIndex(event)
			if !ok {
				return nil, nil, fmt.Errorf("content_block_stop missing valid non-negative integer index")
			}
			st, ok := blocks[idx]
			if !ok || !st.started {
				return nil, nil, fmt.Errorf("content_block_stop for unknown index %d", idx)
			}
			if st.stopped {
				return nil, nil, fmt.Errorf("duplicate content_block_stop for index %d", idx)
			}
			st.stopped = true
		case "message_delta":
			if messageStartCount == 0 {
				return nil, nil, fmt.Errorf("message_delta before message_start")
			}
			if anthropicMsg == nil {
				anthropicMsg = map[string]any{}
			}
			if delta, ok := event["delta"].(map[string]any); ok {
				if stop, ok := delta["stop_reason"].(string); ok {
					anthropicMsg["stop_reason"] = stop
				}
			}
			// message_delta usage is at the event top level (Anthropic spec).
			if usage, ok := event["usage"].(map[string]any); ok {
				anthropicMsg["usage"] = mergeUsageMaps(anthropicMsg["usage"], usage)
			} else if delta, ok := event["delta"].(map[string]any); ok {
				if usage, ok := delta["usage"].(map[string]any); ok {
					anthropicMsg["usage"] = mergeUsageMaps(anthropicMsg["usage"], usage)
				}
			}
		case "message_stop":
			// Validate at message_stop time: must have message_start,
			// all blocks stopped, and non-empty stop_reason.
			if messageStartCount == 0 {
				return nil, nil, fmt.Errorf("message_stop before message_start")
			}
			for idx, st := range blocks {
				if st != nil && st.started && !st.stopped {
					return nil, nil, fmt.Errorf("message_stop with unclosed block at index %d", idx)
				}
			}
			stopReason, _ := anthropicMsg["stop_reason"].(string)
			if stopReason == "" {
				return nil, nil, fmt.Errorf("message_stop without stop_reason")
			}
			sawMessageStop = true
		case "error":
			errType := "api_error"
			errMsg := "upstream Anthropic error"
			if errMap, ok := event["error"].(map[string]any); ok {
				if t, ok := errMap["type"].(string); ok && t != "" {
					errType = t
				}
				if m, ok := errMap["message"].(string); ok && m != "" {
					errMsg = m
				}
			}
			return nil, nil, &anthropicProtocolError{errType: errType, message: errMsg}
		default:
			// Unknown event type: ignore.
		}
	}

	if messageStartCount == 0 {
		return nil, nil, fmt.Errorf("anthropic SSE stream missing message_start")
	}
	if !sawMessageStop {
		return nil, nil, fmt.Errorf("anthropic SSE stream ended without message_stop")
	}

	// Build ordered content blocks sorted by numeric index ascending.
	indices := make([]int, 0, len(blocks))
	for idx := range blocks {
		indices = append(indices, idx)
	}
	sort.Ints(indices)

	var contentBlocks []map[string]any
	for _, idx := range indices {
		st := blocks[idx]
		if st == nil {
			continue
		}
		switch st.blockType {
		case "text":
			contentBlocks = append(contentBlocks, map[string]any{
				"type": "text",
				"text": st.textBuilder.String(),
			})
		case "thinking":
			blk := map[string]any{
				"type":     "thinking",
				"thinking": st.thinkBuilder.String(),
			}
			if st.signature != "" {
				blk["signature"] = st.signature
			}
			contentBlocks = append(contentBlocks, blk)
		case "redacted_thinking":
			blk := map[string]any{
				"type": "redacted_thinking",
			}
			if st.data != "" {
				blk["data"] = st.data
			}
			contentBlocks = append(contentBlocks, blk)
		case "tool_use":
			var input any
			if st.sawInputDelta {
				// Parse accumulated partial JSON deltas.
				inputStr := st.jsonBuilder.String()
				if inputStr != "" {
					var parsed any
					if err := json.Unmarshal([]byte(inputStr), &parsed); err != nil {
						return nil, nil, fmt.Errorf("malformed tool_use input JSON for index %d: %w", idx, err)
					}
					input = parsed
				} else {
					input = map[string]any{}
				}
			} else if st.initialInput != nil {
				// Use initial input from content_block_start.
				if inputStr, ok := st.initialInput.(string); ok {
					var parsed any
					if err := json.Unmarshal([]byte(inputStr), &parsed); err != nil {
						return nil, nil, fmt.Errorf("malformed tool_use initial input for index %d: %w", idx, err)
					}
					input = parsed
				} else {
					input = st.initialInput
				}
			} else {
				input = map[string]any{}
			}
			blk := map[string]any{
				"type":  "tool_use",
				"input": input,
			}
			if st.id != "" {
				blk["id"] = st.id
			}
			if st.name != "" {
				blk["name"] = st.name
			}
			contentBlocks = append(contentBlocks, blk)
		default:
			// Unknown block type: skip.
		}
	}

	if anthropicMsg == nil {
		anthropicMsg = map[string]any{}
	}
	return anthropicMsg, contentBlocks, nil
}

// extractBlockIndex extracts a non-negative integer index from an SSE event.
// Returns false if the index is missing, not a JSON number, is a float with
// a fractional part, is negative, NaN, Inf, or exceeds the platform's int range.
// The platform maxInt is checked BEFORE the int(f) conversion to avoid
// undefined/saturating behavior on overflow.
func extractBlockIndex(event map[string]any) (int, bool) {
	rawIdx, ok := event["index"]
	if !ok || rawIdx == nil {
		return 0, false
	}
	f, ok := rawIdx.(float64)
	if !ok {
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	if math.Trunc(f) != f || f < 0 {
		return 0, false
	}
	// Check platform int range BEFORE converting, to avoid overflow.
	// On 64-bit: maxInt = 2^63-1; on 32-bit: maxInt = 2^31-1.
	// float64 can't represent all int64 values, so also cap at 2^53
	// (where all integers are exactly representable as float64).
	maxInt := float64(1<<(strconv.IntSize-1) - 1)
	if f > maxInt {
		return 0, false
	}
	// Also reject values above the float64 exact-integer upper bound.
	if f > float64(1<<53) {
		return 0, false
	}
	idx := int(f)
	if float64(idx) != f {
		return 0, false
	}
	return idx, true
}

// deterministicResponseID returns a deterministic response ID for the given
// prefix and upstream ID. If id already has the prefix (with a non-empty
// suffix), it is kept as-is. Otherwise, a stable hex digest derived from
// sha256(id) is appended to the prefix so the same input always produces the
// same output. An empty id gets a random suffix (callers should cache).
func deterministicResponseID(prefix, id string) string {
	if strings.HasPrefix(id, prefix) && len(id) > len(prefix) {
		return id
	}
	if id == "" {
		return prefix + randomString(24)
	}
	h := sha256.Sum256([]byte(id))
	return prefix + hex.EncodeToString(h[:16])
}

// normalizeChatResponseID ensures a Chat response ID has the chatcmpl- prefix.
func normalizeChatResponseID(id string) string {
	return deterministicResponseID("chatcmpl-", id)
}

// normalizeResponsesID ensures a Responses response ID has the resp_ prefix.
func normalizeResponsesID(id string) string {
	return deterministicResponseID("resp_", id)
}

// normalizeClaudeMessageID ensures a Claude message ID has the msg_ prefix.
func normalizeClaudeMessageID(id string) string {
	return deterministicResponseID("msg_", id)
}

// buildOpenAIResponse constructs a Chat Completions response from an Anthropic
// message and ordered content blocks. Text blocks are concatenated into a
// content string (preserving original order), thinking goes to
// reasoning_content, tool_use blocks populate tool_calls. A private field
// _opencode2api_anthropic_content preserves the original ordered blocks for
// Claude roundtrip; convertResponse strips it before responding to clients.
func buildOpenAIResponse(anthropicMsg map[string]any, contentBlocks []map[string]any, modelID string) ([]byte, error) {
	if anthropicMsg == nil {
		return nil, fmt.Errorf("no Anthropic message to convert")
	}
	now := time.Now().Unix()
	role, _ := anthropicMsg["role"].(string)
	if role == "" {
		role = "assistant"
	}
	finishReason, _ := anthropicMsg["stop_reason"].(string)
	finishReason = normalizeFinishReason(finishReason)

	var textBuilder strings.Builder
	var reasoningContent string
	var toolCalls []map[string]any
	hasNonText := false

	for _, blk := range contentBlocks {
		bt, _ := blk["type"].(string)
		switch bt {
		case "text":
			if t, ok := blk["text"].(string); ok {
				textBuilder.WriteString(t)
			}
		case "thinking":
			hasNonText = true
			if t, ok := blk["thinking"].(string); ok {
				if reasoningContent != "" {
					reasoningContent += "\n"
				}
				reasoningContent += t
			}
		case "redacted_thinking":
			hasNonText = true
		case "tool_use":
			hasNonText = true
			input := blk["input"]
			if input == nil {
				input = map[string]any{}
			}
			argsJSON, _ := json.Marshal(input)
			toolID, _ := blk["id"].(string)
			if toolID == "" {
				toolID = "toolu_" + randomString(12)
				blk["id"] = toolID
			}
			toolName, _ := blk["name"].(string)
			toolCalls = append(toolCalls, map[string]any{
				"id":   toolID,
				"type": "function",
				"function": map[string]any{
					"name":      toolName,
					"arguments": string(argsJSON),
				},
			})
		default:
			// Unknown non-empty block type: preserve for private roundtrip.
			if bt != "" {
				hasNonText = true
			}
		}
	}

	msg := map[string]any{"role": role}

	// Determine content: if only text blocks, use a string for compatibility.
	textStr := textBuilder.String()
	if !hasNonText {
		msg["content"] = textStr
	} else {
		if textStr != "" {
			msg["content"] = textStr
		} else {
			msg["content"] = nil
		}
	}

	if reasoningContent != "" {
		msg["reasoning_content"] = reasoningContent
	}

	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}

	// Private field for Claude roundtrip: preserves original ordered blocks
	// whenever any non-text native block exists (thinking, redacted_thinking,
	// tool_use). Generated tool IDs are written back to the blocks so that
	// Claude roundtrip associations are consistent.
	if hasNonText {
		privateBlocks := make([]map[string]any, 0, len(contentBlocks))
		for _, blk := range contentBlocks {
			privateBlocks = append(privateBlocks, blk)
		}
		msg["_opencode2api_anthropic_content"] = privateBlocks
	}

	choice := map[string]any{
		"index":         0,
		"message":       msg,
		"finish_reason": finishReason,
	}

	resp := map[string]any{
		"id":      normalizeChatResponseID(toString(anthropicMsg["id"])),
		"object":  "chat.completion",
		"created": now,
		"model":   modelID,
		"choices": []map[string]any{choice},
	}
	if usage, ok := anthropicMsg["usage"].(map[string]any); ok {
		resp["usage"] = anthropicUsageToChat(usage)
	}
	result, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Chat response: %w", err)
	}
	return result, nil
}

// convertAnthropicMessageToOpenAI converts a native Anthropic message JSON
// (non-streaming) to Chat Completions format. Returns an error on malformed input.
func convertAnthropicMessageToOpenAI(msg map[string]any, modelID string) ([]byte, error) {
	if msg == nil {
		return nil, fmt.Errorf("no Anthropic message to convert")
	}
	if msg["model"] == nil {
		msg["model"] = modelID
	}
	// Direct non-stream message requires a content array.
	content, ok := msg["content"].([]any)
	if !ok {
		return nil, fmt.Errorf("anthropic message missing content array")
	}
	var contentBlocks []map[string]any
	for _, c := range content {
		block, ok := c.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("anthropic message content contains non-object block")
		}
		bt, _ := block["type"].(string)
		switch bt {
		case "text", "thinking", "redacted_thinking":
			// Supported types.
		case "tool_use":
			// tool_use must have a non-empty name.
			name, _ := block["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("tool_use block missing non-empty name")
			}
			// tool_use input must be JSON-marshalable.
			if input, exists := block["input"]; exists && input != nil {
				if _, err := json.Marshal(input); err != nil {
					return nil, fmt.Errorf("tool_use input not JSON-marshalable: %w", err)
				}
			}
		default:
			if bt == "" {
				return nil, fmt.Errorf("anthropic message content block missing type")
			}
			// Unknown non-empty block type: keep in private blocks for
			// potential roundtrip; public Chat ignores it.
		}
		contentBlocks = append(contentBlocks, block)
	}
	// Direct non-stream message requires a non-empty stop_reason.
	stopReason, _ := msg["stop_reason"].(string)
	if stopReason == "" {
		return nil, fmt.Errorf("anthropic message missing stop_reason")
	}
	return buildOpenAIResponse(msg, contentBlocks, modelID)
}

// convertAnthropicToOpenAI detects whether the upstream body is a native
// Anthropic message (JSON) or SSE stream, and converts it to Chat Completions.
// Returns an error if the body is malformed, truncated, or contains an error event.
func convertAnthropicToOpenAI(body []byte, modelID string) ([]byte, error) {
	var singleMsg map[string]any
	if json.Unmarshal(body, &singleMsg) == nil {
		if typ, _ := singleMsg["type"].(string); typ == "message" {
			return convertAnthropicMessageToOpenAI(singleMsg, modelID)
		}
		// Could be a single error object.
		if typ, _ := singleMsg["type"].(string); typ == "error" {
			errType := "api_error"
			errMsg := "upstream Anthropic error"
			if errMap, ok := singleMsg["error"].(map[string]any); ok {
				if t, ok := errMap["type"].(string); ok && t != "" {
					errType = t
				}
				if m, ok := errMap["message"].(string); ok && m != "" {
					errMsg = m
				}
			}
			return nil, &anthropicProtocolError{errType: errType, message: errMsg}
		}
	}
	msg, contentBlocks, err := parseAnthropicSSE(body)
	if err != nil {
		return nil, err
	}
	if msg["model"] == nil {
		msg["model"] = modelID
	}
	return buildOpenAIResponse(msg, contentBlocks, modelID)
}

// ======================== 响应清理 ========================

func cleanNulls(m map[string]any) {
	for k, v := range m {
		if v == nil {
			delete(m, k)
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			delete(m, k)
		}
	}
}

// promoteMisplacedReasoning moves reasoning_content into content when upstream
// put the visible answer in reasoning_content (opencode-go #37635). Only runs
// when content is empty and the chunk has no tool_calls, so genuine CoT that
// precedes tool calls is left alone when keepReasoning is true.
func promoteMisplacedReasoning(fields map[string]any, keepReasoning bool) bool {
	rc, _ := fields["reasoning_content"].(string)
	if rc == "" {
		return false
	}
	if raw, ok := fields["tool_calls"]; ok && raw != nil {
		if arr, ok := raw.([]any); ok && len(arr) > 0 {
			return false
		}
	}
	content, _ := fields["content"].(string)
	if content != "" {
		return false
	}
	if keepReasoning {
		// Preserve CoT for thinking blocks / clients that read reasoning_content.
		return false
	}
	fields["content"] = rc
	delete(fields, "reasoning_content")
	return true
}

func cleanStreamDelta(delta map[string]any, keepReasoning bool) {
	_ = promoteMisplacedReasoning(delta, keepReasoning)
	if v, ok := delta["content"]; ok && v == nil {
		delete(delta, "content")
	}
	if s, ok := delta["content"].(string); ok && s == "" {
		delete(delta, "content")
	}
	if !keepReasoning {
		delete(delta, "reasoning_content")
	} else {
		if v, ok := delta["reasoning_content"]; ok && v == nil {
			delete(delta, "reasoning_content")
		}
		if s, ok := delta["reasoning_content"].(string); ok && s == "" {
			delete(delta, "reasoning_content")
		}
	}
	if s, ok := delta["role"].(string); ok && s == "" {
		delete(delta, "role")
	}
}

// convertStreamChunkWithUsage 转换流式 chunk 并同时提取 usage，避免二次解析
func convertStreamChunkWithUsage(line string, keepReasoning bool) (string, map[string]any) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "data: [DONE]" || trimmed == "[DONE]" {
		return line, nil
	}
	if !strings.HasPrefix(line, "data: ") {
		return line, nil
	}
	data := line[6:]
	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return line, nil
	}

	// 提取 usage
	var usage map[string]any
	if u, ok := raw["usage"].(map[string]any); ok {
		usage = u
	}

	choices, ok := raw["choices"].([]any)
	if !ok || len(choices) == 0 {
		// Chat Completions deliberately uses an empty choices array for the
		// terminal usage chunk. It is part of the client-visible stream.
		if id, ok := raw["id"].(string); ok && id != "" {
			raw["id"] = normalizeChatResponseID(id)
		}
		delete(raw, "cost")
		converted, err := json.Marshal(raw)
		if err != nil {
			return line, usage
		}
		return "data: " + string(converted), usage
	}
	for i, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if delta, ok := choice["delta"].(map[string]any); ok {
			cleanStreamDelta(delta, keepReasoning)
			choice["delta"] = delta
		}
		if msg, ok := choice["message"].(map[string]any); ok {
			cleanNulls(msg)
			promoteMisplacedReasoning(msg, keepReasoning)
			if !keepReasoning {
				delete(msg, "reasoning_content")
			}
			delete(msg, "_opencode2api_anthropic_content")
			choice["message"] = msg
		}
		if v, ok := choice["logprobs"]; ok && v == nil {
			delete(choice, "logprobs")
		}
		if v, ok := choice["finish_reason"]; ok && v == nil {
			delete(choice, "finish_reason")
		}
		if s, ok := choice["finish_reason"].(string); ok && s == "" {
			delete(choice, "finish_reason")
		}
		choices[i] = choice
	}
	raw["choices"] = choices
	if v, ok := raw["usage"]; ok && v == nil {
		delete(raw, "usage")
	}
	if id, ok := raw["id"].(string); ok && id != "" {
		raw["id"] = normalizeChatResponseID(id)
	}
	delete(raw, "cost")
	converted, err := json.Marshal(raw)
	if err != nil {
		return line, usage
	}
	return "data: " + string(converted), usage
}

func convertResponse(data []byte, keepReasoning bool) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		slog.Warn("convertResponse unmarshal failed", "error", err)
		return data, nil
	}
	if id, ok := raw["id"].(string); ok && id != "" {
		raw["id"] = normalizeChatResponseID(id)
	}
	if choices, ok := raw["choices"].([]any); ok {
		for i, c := range choices {
			if choice, ok := c.(map[string]any); ok {
				if msg, ok := choice["message"].(map[string]any); ok {
					cleanNulls(msg)
					promoteMisplacedReasoning(msg, keepReasoning)
					if !keepReasoning {
						delete(msg, "reasoning_content")
					}
					// Strip private Anthropic roundtrip field so it never
					// leaks to Chat Completions consumers.
					delete(msg, "_opencode2api_anthropic_content")
					choice["message"] = msg
				}
				if v, ok := choice["logprobs"]; ok && v == nil {
					delete(choice, "logprobs")
				}
				choices[i] = choice
			}
		}
		raw["choices"] = choices
	}
	delete(raw, "cost")
	return json.Marshal(raw)
}

// ======================== 认证层级 ========================

type TierType int

const (
	TierFree TierType = iota
	TierPaid
)

type AuthRouteMode int

const (
	AuthRoutePublic AuthRouteMode = iota
	AuthRouteAuto
	AuthRouteZen
	AuthRouteGo
)

type UpstreamAuth struct {
	Token  string
	Mode   AuthRouteMode
	Source string // authorization | x-api-key | none
}

func extractUpstreamAuth(r *http.Request) UpstreamAuth {
	token := ""
	source := "none"
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		token = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		source = "authorization"
	}
	if token == "" {
		if key := strings.TrimSpace(r.Header.Get("x-api-key")); key != "" {
			token = key
			source = "x-api-key"
		}
	}
	if token == "" || token == "public" {
		src := source
		if token == "" {
			src = "none"
		}
		return UpstreamAuth{Mode: AuthRoutePublic, Source: src}
	}
	// go:/zen: 前缀路由：去掉前缀后剩余部分仍需是有效 key（sk- 开头）
	if rest, ok := strings.CutPrefix(token, "go:"); ok && isValidOpenCodeKey(rest) {
		return UpstreamAuth{Token: rest, Mode: AuthRouteGo, Source: source}
	}
	if rest, ok := strings.CutPrefix(token, "zen:"); ok && isValidOpenCodeKey(rest) {
		return UpstreamAuth{Token: rest, Mode: AuthRouteZen, Source: source}
	}
	// 只有 sk- 开头的才是有效 key，其余（no-key-required 等占位符）一律走 public
	if isValidOpenCodeKey(token) {
		return UpstreamAuth{Token: token, Mode: AuthRouteAuto, Source: source}
	}
	return UpstreamAuth{Mode: AuthRoutePublic, Source: source}
}

// 只认 sk- 开头的 opencode key；Anthropic sk-ant-* 不能转发上游。
func isValidOpenCodeKey(token string) bool {
	if strings.HasPrefix(token, "sk-ant-") {
		return false
	}
	return strings.HasPrefix(token, "sk-") && len(token) > 15
}

func (auth UpstreamAuth) tier() TierType {
	if auth.Mode == AuthRoutePublic {
		return TierFree
	}
	return TierPaid
}

func (auth UpstreamAuth) authorizationHeader() string {
	if auth.Mode == AuthRoutePublic {
		return "Bearer public"
	}
	return "Bearer " + auth.Token
}

func (auth UpstreamAuth) shouldUseGoCatalog() bool {
	return auth.Mode == AuthRouteGo
}

func (auth UpstreamAuth) shouldUseGoEndpoint(modelID string) bool {
	switch auth.Mode {
	case AuthRouteGo:
		return isModelInGoCatalog(modelID)
	case AuthRouteAuto:
		return isGoCatalogOnlyModel(modelID)
	default:
		return false
	}
}

// isFreeModel 判断模型是否属于免费模型（以 -free 结尾）
func isFreeModel(modelID string) bool {
	return strings.HasSuffix(modelID, "-free")
}

// publicFacingModelID strips the upstream "-free" suffix for client-visible catalogs.
func publicFacingModelID(modelID string) string {
	if isFreeModel(modelID) {
		return strings.TrimSuffix(modelID, "-free")
	}
	return modelID
}

// mapPublicToFreeModel downgrades paid model IDs to their "-free" variants for
// keyless (public tier) requests, so deepseek-v4-flash reaches the free tier as
// deepseek-v4-flash-free instead of failing upstream with a missing key. Keyed
// tiers keep the exact requested model; models without a known free variant are
// left untouched.
func mapPublicToFreeModel(auth UpstreamAuth, modelID string) string {
	if auth.Mode != AuthRoutePublic || isFreeModel(modelID) {
		return modelID
	}
	if freeID := modelID + "-free"; modelExistsInCaches(freeID) {
		return freeID
	}
	return modelID
}

func modelExistsInCaches(modelID string) bool {
	modelMu.RLock()
	defer modelMu.RUnlock()
	return containsModelWithID(modelsCache, modelID) || containsModelWithID(goModelsCache, modelID)
}

func buildOCRequest(modelID string, bodyMap map[string]any, auth UpstreamAuth) (*http.Request, error) {
	return buildOCRequestWithEndpoint(modelID, bodyMap, auth, auth.shouldUseGoEndpoint(modelID))
}

func buildOCRequestWithEndpoint(modelID string, bodyMap map[string]any, auth UpstreamAuth, useGoEndpoint bool) (*http.Request, error) {
	bodyMap["model"] = modelID
	tryBody, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, err
	}
	var upstreamURL string
	if useGoEndpoint {
		upstreamURL = "https://opencode.ai/zen/go/v1/chat/completions"
	} else {
		upstreamURL = "https://opencode.ai/zen/v1/chat/completions"
	}
	req, err := http.NewRequest("POST", upstreamURL, bytes.NewReader(tryBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth.authorizationHeader())
	req.Header.Set("User-Agent", fmt.Sprintf("opencode/%s", ocClientVer))
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-project", ocProjectID)
	req.Header.Set("x-opencode-session", ocSessionID)
	req.Header.Set("x-opencode-request", "req_"+randomString(24))
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func shouldRetryUpstreamStatus(status int) bool {
	// 仅重试可恢复的临时性错误（始终同模型重试，不换模型）
	switch status {
	case http.StatusUnauthorized, // 401 认证过期或 token 未同步
		http.StatusTooManyRequests,    // 429 限流
		http.StatusBadGateway,         // 502
		http.StatusServiceUnavailable, // 503
		http.StatusGatewayTimeout:     // 504
		return true
	}
	// 其他 5xx 也重试，但 4xx 中只有 401 和 429 重试
	return status >= 500 && status < 600
}

const (
	maxUpstreamRetries = 3
	max401Retries      = 3
)

func maxAttemptsForUpstreamStatus(status int) int {
	if status == http.StatusUnauthorized {
		return max401Retries
	}
	return maxUpstreamRetries
}

func callOpenCodeAPI(ctx context.Context, upstreamBody []byte, modelID string, auth UpstreamAuth) ([]byte, int, http.Header, error) {
	initOCSession()

	var bodyMap map[string]any
	if err := json.Unmarshal(upstreamBody, &bodyMap); err != nil {
		return nil, 500, nil, fmt.Errorf("invalid request body")
	}
	useGoEndpoint := auth.shouldUseGoEndpoint(modelID)
	surface := "zen"
	if useGoEndpoint {
		surface = "go"
	}
	log := reqLogger(ctx)

	var lastErr error
	var retryCount int
	var lastBody []byte
	var lastStatus int
	var lastHeader http.Header
	maxAttempts := maxUpstreamRetries
	if max401Retries > maxAttempts {
		maxAttempts = max401Retries
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		up, err := buildOCRequestWithEndpoint(modelID, bodyMap, auth, useGoEndpoint)
		if err != nil {
			return nil, 500, nil, err
		}
		client := getHTTPClientForTier(auth.tier())
		attemptStart := time.Now()
		resp, err := client.Do(up)
		durationMs := time.Since(attemptStart).Milliseconds()
		if err != nil {
			lastErr = err
			lastStatus = 0
			retryReason := "transport_error"
			canRetry := attempt+1 < maxUpstreamRetries
			if !canRetry {
				retryReason = ""
			}
			log.Info("upstream_attempt",
				"try_model", modelID,
				"surface", surface,
				"status", 0,
				"duration_ms", durationMs,
				"attempt_index", attempt,
				"retry_reason", retryReason,
				"error", err.Error(),
			)
			if canRetry {
				client.CloseIdleConnections()
				retryCount++
				continue
			}
			break
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			b, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				return nil, 0, nil, readErr
			}
			if isAnthropicFormat(b) {
				converted, convErr := convertAnthropicToOpenAI(b, modelID)
				if convErr != nil {
					log.Info("upstream_attempt",
						"try_model", modelID,
						"surface", surface,
						"status", resp.StatusCode,
						"duration_ms", durationMs,
						"attempt_index", attempt,
					)
					// Only anthropicProtocolError errors carry type/message;
					// non-typed conversion errors stay generic so
					// writeUpstreamError emits a safe default.
					return nil, http.StatusBadGateway, nil, convErr
				}
				b = converted
			}
			log.Info("upstream_attempt",
				"try_model", modelID,
				"surface", surface,
				"status", resp.StatusCode,
				"duration_ms", durationMs,
				"attempt_index", attempt,
			)
			log.Info("upstream_result",
				"models_tried", []string{modelID},
				"retries", retryCount,
				"final_status", resp.StatusCode,
				"fallback_used", false,
			)
			return b, resp.StatusCode, resp.Header, nil
		}
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		logUpstreamError(ctx, modelID, resp.StatusCode, errBody)
		nonRetryable := isNonRetryableUpstreamError(resp.StatusCode, errBody)
		canRetry := !nonRetryable && shouldRetryUpstreamStatus(resp.StatusCode) && attempt+1 < maxAttemptsForUpstreamStatus(resp.StatusCode)
		retryReason := ""
		if canRetry {
			retryReason = fmt.Sprintf("status_%d", resp.StatusCode)
		}
		if nonRetryable {
			retryReason = "non_retryable_upstream"
		}
		log.Info("upstream_attempt",
			"try_model", modelID,
			"surface", surface,
			"status", resp.StatusCode,
			"duration_ms", durationMs,
			"attempt_index", attempt,
			"retry_reason", retryReason,
		)
		lastBody = errBody
		lastStatus = resp.StatusCode
		lastHeader = resp.Header
		lastErr = fmt.Errorf("upstream error")
		if !canRetry {
			break
		}
		client.CloseIdleConnections()
		retryCount++
	}
	log.Info("upstream_result",
		"models_tried", []string{modelID},
		"retries", retryCount,
		"final_status", lastStatus,
		"fallback_used", false,
	)
	return lastBody, lastStatus, lastHeader, lastErr
}

// anthropicProtocolError is a typed error that carries Anthropic error
// type/message through a local protocol conversion failure. Use errors.As
// to extract it; do not parse error strings.
type anthropicProtocolError struct {
	errType string
	message string
}

func (e *anthropicProtocolError) Error() string {
	if e.errType != "" {
		return e.errType + ": " + e.message
	}
	return e.message
}

// writeUpstreamError writes a protocol-shaped error response for each
// downstream protocol (chat, claude, responses). Only local Anthropic
// protocol conversion errors (anthropicProtocolError via errors.As) expose
// the upstream type/message; all other errors (transport, build, etc.) get
// a generic "upstream_error" type with a stable safe message. The error's
// Error() string is never exposed. Invalid HTTP status (0, etc.) is
// normalized to 502.
func writeUpstreamError(w http.ResponseWriter, status int, err error, protocol string) {
	if status < 100 || status >= 600 {
		status = http.StatusBadGateway
	}

	errType := "upstream_error"
	message := "upstream error"

	var ape *anthropicProtocolError
	if errors.As(err, &ape) {
		if ape.errType != "" {
			errType = ape.errType
		}
		if ape.message != "" {
			message = ape.message
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	switch protocol {
	case "claude":
		json.NewEncoder(w).Encode(map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    errType,
				"message": message,
			},
		})
	case "responses":
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"type":    errType,
				"message": message,
			},
		})
	default: // "chat"
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"type":    errType,
				"message": message,
			},
		})
	}
}

func callOpenCodeAPIStream(ctx context.Context, upstreamBody []byte, modelID string, auth UpstreamAuth) (io.ReadCloser, int, http.Header, error) {
	initOCSession()

	var bodyMap map[string]any
	if err := json.Unmarshal(upstreamBody, &bodyMap); err != nil {
		return nil, 500, nil, fmt.Errorf("invalid request body")
	}
	useGoEndpoint := auth.shouldUseGoEndpoint(modelID)
	surface := "zen"
	if useGoEndpoint {
		surface = "go"
	}
	log := reqLogger(ctx)

	var lastBody []byte
	var lastStatus int
	var lastHeader http.Header
	var retryCount int
	maxAttempts := maxUpstreamRetries
	if max401Retries > maxAttempts {
		maxAttempts = max401Retries
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		up, err := buildOCRequestWithEndpoint(modelID, bodyMap, auth, useGoEndpoint)
		if err != nil {
			return nil, 500, nil, err
		}
		client := getHTTPClientForTier(auth.tier())
		attemptStart := time.Now()
		resp, err := client.Do(up)
		durationMs := time.Since(attemptStart).Milliseconds()
		if err != nil {
			retryReason := "transport_error"
			canRetry := attempt+1 < maxUpstreamRetries
			if !canRetry {
				retryReason = ""
			}
			log.Info("upstream_attempt",
				"try_model", modelID,
				"surface", surface,
				"status", 0,
				"duration_ms", durationMs,
				"attempt_index", attempt,
				"retry_reason", retryReason,
				"error", err.Error(),
			)
			if canRetry {
				client.CloseIdleConnections()
				retryCount++
				continue
			}
			break
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			log.Info("upstream_attempt",
				"try_model", modelID,
				"surface", surface,
				"status", resp.StatusCode,
				"duration_ms", durationMs,
				"attempt_index", attempt,
			)
			log.Info("upstream_result",
				"models_tried", []string{modelID},
				"retries", retryCount,
				"final_status", resp.StatusCode,
				"fallback_used", false,
			)
			return resp.Body, resp.StatusCode, resp.Header, nil
		}
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		logUpstreamError(ctx, modelID, resp.StatusCode, errBody)
		nonRetryable := isNonRetryableUpstreamError(resp.StatusCode, errBody)
		canRetry := !nonRetryable && shouldRetryUpstreamStatus(resp.StatusCode) && attempt+1 < maxAttemptsForUpstreamStatus(resp.StatusCode)
		retryReason := ""
		if canRetry {
			retryReason = fmt.Sprintf("status_%d", resp.StatusCode)
		}
		if nonRetryable {
			retryReason = "non_retryable_upstream"
		}
		log.Info("upstream_attempt",
			"try_model", modelID,
			"surface", surface,
			"status", resp.StatusCode,
			"duration_ms", durationMs,
			"attempt_index", attempt,
			"retry_reason", retryReason,
		)
		lastBody = errBody
		lastStatus = resp.StatusCode
		lastHeader = resp.Header
		if !canRetry {
			break
		}
		client.CloseIdleConnections()
		retryCount++
	}
	log.Info("upstream_result",
		"models_tried", []string{modelID},
		"retries", retryCount,
		"final_status", lastStatus,
		"fallback_used", false,
	)
	if lastStatus != 0 {
		return io.NopCloser(bytes.NewReader(lastBody)), lastStatus, lastHeader, nil
	}
	return nil, 500, nil, fmt.Errorf("upstream request failed")
}

// ======================== 安全响应头过滤 ========================

var safeResponseHeaders = map[string]bool{
	"Content-Type":          true,
	"X-RateLimit-Limit":     true,
	"X-RateLimit-Remaining": true,
	"X-RateLimit-Reset":     true,
}

func filterResponseHeaders(h http.Header) http.Header {
	filtered := make(http.Header)
	for k, v := range h {
		if safeResponseHeaders[k] {
			filtered[k] = v
		}
	}
	return filtered
}

// ======================== Chat Completions Handler ========================

func chatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	auth := extractUpstreamAuth(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	cnt := requestCount.Add(1)
	maybeLogBodySummary(r.Context(), "chat completion request body", body)
	_ = cnt

	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	modelIn := req.Model
	req.Model = resolveModel(req.Model)
	if req.Model == "" {
		modelIDs := getModelIDs()
		if len(modelIDs) > 0 {
			req.Model = modelIDs[0]
		} else {
			req.Model = "deepseek-v4-flash-free"
		}
	}
	req.Model = mapPublicToFreeModel(auth, req.Model)
	if !validateRequestTemperature(w, req.Temperature, "chat", 0, 2) {
		return
	}

	// 多模态路由：检测到图片时转发到配置的上游

	req.Messages = fixToolCallGaps(req.Messages)
	keepReasoning := wantsReasoning(&req)
	req.Messages = ensureReasoningContent(req.Messages, keepReasoning)
	if req.Stream {
		if req.ExtraBody == nil {
			req.ExtraBody = map[string]any{}
		}
		req.ExtraBody["stream_options"] = map[string]any{"include_usage": true}
	}
	effortIn := req.ReasoningEffort
	if effortIn == "" && !isThinkingDisabled(req.Thinking) {
		effortIn = reasoningEffortFromThinking(req.Thinking)
	}
	upstreamSurface := "zen"
	if auth.shouldUseGoEndpoint(req.Model) {
		upstreamSurface = "go"
	}
	logRequestPlan(r.Context(), map[string]any{
		"protocol":             "chat",
		"model_in":             modelIn,
		"model_resolved":       req.Model,
		"auth_mode":            authModeString(auth.Mode),
		"auth_source":          auth.Source,
		"has_key":              auth.Token != "",
		"upstream_surface":     upstreamSurface,
		"stream":               req.Stream,
		"keep_reasoning":       keepReasoning,
		"thinking":             thinkingState(req.Thinking),
		"reasoning_effort_in":  effortIn,
		"reasoning_effort_out": mappedReasoningEffort(effortIn),
		"tools_count":          len(req.Tools),
		"messages_count":       len(req.Messages),
		"max_tokens":           req.MaxTokens,
		"max_tokens_cap":       getMaxTokensCapForModel(req.Model),
	})
	upstreamBody := buildUpstreamBody(&req)

	if req.Stream {
		upResp, status, _, err := callOpenCodeAPIStream(r.Context(), upstreamBody, req.Model, auth)
		if err != nil || status < 200 || status >= 300 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if upResp != nil {
				errBody, _ := io.ReadAll(upResp)
				if len(errBody) > 0 {
					w.Write(errBody)
					return
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error", "type": "upstream_error"}})
			return
		}
		defer upResp.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		reader := bufio.NewReader(upResp)
		stats := &streamResultStats{start: time.Now()}
		doneSeen := false
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					break
				}
				reqLogger(r.Context()).Error("stream read error", "error", err)
				// 发送错误事件通知客户端
				w.Write([]byte("data: {\"error\":\"stream read error\"}\n\n"))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				stats.log(r.Context(), "chat")
				return
			}
			if doneSeen {
				continue
			}
			trimmed := strings.TrimSpace(line)
			if trimmed == "data: [DONE]" {
				doneSeen = true
				stats.doneSeen = true
				w.Write([]byte("data: [DONE]\n\n"))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				continue
			}

			if strings.HasPrefix(line, "data: ") {
				var raw map[string]any
				if json.Unmarshal([]byte(line[6:]), &raw) == nil {
					if choices, ok := raw["choices"].([]any); ok && len(choices) > 0 {
						if choice, ok := choices[0].(map[string]any); ok {
							if delta, ok := choice["delta"].(map[string]any); ok {
								stats.observeDelta(delta, keepReasoning)
							}
							if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
								stats.finishReason = fr
								stats.sawFinish = true
							}
						}
					}
				}
			}

			out, usage := convertStreamChunkWithUsage(line, keepReasoning)
			if out == "" {
				// 空choices chunk，但可能有 usage
				if usage != nil {
					pt, _ := usage["prompt_tokens"].(float64)
					ct, _ := usage["completion_tokens"].(float64)
					tt, _ := usage["total_tokens"].(float64)
					if tt > 0 {
						recordTokenUsage(req.Model, int64(pt), int64(ct), int64(tt))
					}
				}
				continue
			}

			// 提取 usage（已在 convertStreamChunkWithUsage 中解析）
			if usage != nil && !doneSeen {
				pt, _ := usage["prompt_tokens"].(float64)
				ct, _ := usage["completion_tokens"].(float64)
				tt, _ := usage["total_tokens"].(float64)
				if tt > 0 {
					recordTokenUsage(req.Model, int64(pt), int64(ct), int64(tt))
				}
			}

			w.Write([]byte(out))
			w.Write([]byte("\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		stats.log(r.Context(), "chat")
		return
	}

	respBody, status, _, err := callOpenCodeAPI(r.Context(), upstreamBody, req.Model, auth)
	if err != nil || status < 200 || status >= 300 {
		if err != nil {
			writeUpstreamError(w, status, err, "chat")
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if len(respBody) > 0 {
				w.Write(respBody)
			} else {
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error", "type": "upstream_error"}})
			}
		}
		return
	}
	outBody := respBody
	convertedResp, err := convertResponse(respBody, keepReasoning)
	if err == nil {
		outBody = convertedResp
	}
	result := summarizeChatResult(outBody)
	if !keepReasoning {
		var before map[string]any
		if json.Unmarshal(respBody, &before) == nil {
			if choices, ok := before["choices"].([]any); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]any); ok {
					if msg, ok := choice["message"].(map[string]any); ok {
						content, _ := msg["content"].(string)
						rc, _ := msg["reasoning_content"].(string)
						if content == "" && rc != "" {
							result["promoted_reasoning"] = true
						}
					}
				}
			}
		}
	}
	logRequestResult(r.Context(), result)
	// Record token usage
	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			pt, _ := u["prompt_tokens"].(float64)
			ct, _ := u["completion_tokens"].(float64)
			tt, _ := u["total_tokens"].(float64)
			if tt > 0 {
				recordTokenUsage(req.Model, int64(pt), int64(ct), int64(tt))
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(outBody)
}

// ======================== Models Handler ========================

func listModelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	modelMu.RLock()
	loaded, models := modelsLoaded, modelsCache
	modelMu.RUnlock()
	if !loaded || len(models) == 0 {
		fetched, err := fetchModels()
		if err == nil && len(fetched) > 0 {
			modelMu.Lock()
			modelsCache = fetched
			modelsLoaded = true
			models = modelsCache
			modelMu.Unlock()
		}
	}
	if len(models) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "无法获取模型列表，请检查上游服务是否可用",
		})
		return
	}
	// 保存别名快照；目录权限仍按真实上游模型判断，最后再替换为客户端可见名称。
	configMu.RLock()
	aliases := make(map[string]string, len(modelAlias))
	for alias, upstream := range modelAlias {
		aliases[alias] = upstream
	}
	configMu.RUnlock()

	auth := extractUpstreamAuth(r)
	var combinedModels []ModelInfo
	switch {
	case auth.shouldUseGoCatalog():
		modelMu.RLock()
		combinedModels = make([]ModelInfo, 0, len(models)+len(goModelsCache))
		for _, model := range models {
			if isFreeModel(model.ID) {
				combinedModels = append(combinedModels, model)
			}
		}
		for _, goModel := range goModelsCache {
			if !containsModelWithID(combinedModels, goModel.ID) {
				combinedModels = append(combinedModels, goModel)
			}
		}
		modelMu.RUnlock()
	case auth.Mode == AuthRoutePublic:
		combinedModels = models
		filtered := make([]ModelInfo, 0, len(combinedModels))
		for _, m := range combinedModels {
			if isFreeModel(m.ID) {
				filtered = append(filtered, m)
			}
		}
		if len(filtered) > 0 {
			combinedModels = filtered
		}
	default:
		combinedModels = models
	}
	allModels := replaceModelIDsWithAliases(combinedModels, aliases)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   allModels,
	})
}

func replaceModelIDsWithAliases(models []ModelInfo, aliases map[string]string) []ModelInfo {
	aliasesByUpstream := make(map[string][]string, len(aliases))
	for alias, upstream := range aliases {
		alias = strings.TrimSpace(alias)
		upstream = strings.TrimSpace(upstream)
		if alias == "" || upstream == "" {
			continue
		}
		aliasesByUpstream[upstream] = append(aliasesByUpstream[upstream], alias)
	}
	for upstream := range aliasesByUpstream {
		sort.Strings(aliasesByUpstream[upstream])
	}

	result := make([]ModelInfo, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		visibleIDs := aliasesByUpstream[model.ID]
		if len(visibleIDs) == 0 {
			visibleIDs = []string{publicFacingModelID(model.ID)}
		}
		for _, visibleID := range visibleIDs {
			if _, exists := seen[visibleID]; exists {
				continue
			}
			visibleModel := model
			visibleModel.ID = visibleID
			if visibleID != model.ID {
				visibleModel.OwnedBy = "alias"
			}
			result = append(result, visibleModel)
			seen[visibleID] = struct{}{}
		}
	}
	return result
}

// ======================== Claude Messages API ========================

func extractClaudeSystemText(system any) string {
	if system == nil {
		return ""
	}
	switch v := system.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if block, ok := item.(map[string]any); ok {
				if block["type"] == "text" {
					if text, ok := block["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func cleanJsonSchema(schema any) any {
	m, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	clean := make(map[string]any, len(m))
	for k, v := range m {
		// Annotation-only keys are omitted for upstream compatibility. Constraint
		// keys such as additionalProperties and format are preserved.
		if k == "$schema" || k == "title" || k == "examples" {
			continue
		}
		switch child := v.(type) {
		case map[string]any:
			clean[k] = cleanJsonSchema(child)
		case []any:
			copyArray := make([]any, len(child))
			for i, elem := range child {
				copyArray[i] = cleanJsonSchema(elem)
			}
			clean[k] = copyArray
		default:
			clean[k] = v
		}
	}
	return clean
}

func claudeImageBlockToOpenAI(block map[string]any) (map[string]any, bool) {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return nil, false
	}
	srcType, _ := source["type"].(string)
	mediaType, _ := source["media_type"].(string)
	data, _ := source["data"].(string)
	url, _ := source["url"].(string)
	if srcType == "url" && url != "" {
		return map[string]any{"type": "image_url", "image_url": map[string]string{"url": url}}, true
	}
	if srcType == "base64" && data != "" {
		if mediaType == "" {
			mediaType = "image/png"
		}
		return map[string]any{
			"type": "image_url",
			"image_url": map[string]string{
				"url": "data:" + mediaType + ";base64," + data,
			},
		}, true
	}
	return nil, false
}

// claudeDocumentBlockToOpenAI maps an Anthropic document content block to a
// Chat Completions file content part. It supports source.type=base64
// (media_type, default application/pdf) and source.type=url. A filename is
// preserved from the block/title when available; no protocol ID is generated.
// Returns (nil, false) when the document lacks a usable payload so the caller
// can surface a structured 400 instead of serializing the wrapper as text.
func claudeDocumentBlockToOpenAI(block map[string]any) (map[string]any, bool) {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return nil, false
	}
	srcType, _ := source["type"].(string)
	mediaType, _ := source["media_type"].(string)
	if mediaType == "" {
		mediaType = "application/pdf"
	}
	data, _ := source["data"].(string)
	url, _ := source["url"].(string)

	file := map[string]any{}
	if filename, ok := block["filename"].(string); ok && filename != "" {
		file["filename"] = filename
	} else if title, ok := block["title"].(string); ok && title != "" {
		file["filename"] = title
	}

	switch srcType {
	case "base64":
		if data == "" {
			return nil, false
		}
		file["file_data"] = "data:" + mediaType + ";base64," + data
		return map[string]any{"type": "file", "file": file}, true
	case "url":
		if url == "" {
			return nil, false
		}
		file["file_data"] = url
		return map[string]any{"type": "file", "file": file}, true
	}
	return nil, false
}

func extractClaudeContentText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if block["type"] == "text" {
				if text, ok := block["text"].(string); ok && text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func claudeToOpenAIMessages(claudeMsgs []ClaudeMessage, system any) []Message {
	var systemParts []string
	if sysText := extractClaudeSystemText(system); sysText != "" {
		systemParts = append(systemParts, sysText)
	}

	var body []Message
	for _, msg := range claudeMsgs {
		if msg.Role == "system" {
			if text := extractClaudeContentText(msg.Content); text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		}
		switch content := msg.Content.(type) {
		case string:
			body = append(body, Message{Role: msg.Role, Content: content})
		case []any:
			var orderedContent []any
			var reasoningParts []string
			var toolCalls []ToolCall
			var toolResults []Message
			var followupAttachments []any
			for _, item := range content {
				block, ok := item.(map[string]any)
				if !ok {
					continue
				}
				blockType, _ := block["type"].(string)
				switch blockType {
				case "text":
					if text, ok := block["text"].(string); ok && text != "" {
						orderedContent = append(orderedContent, map[string]any{"type": "text", "text": text})
					}
				case "image":
					if part, ok := claudeImageBlockToOpenAI(block); ok {
						orderedContent = append(orderedContent, part)
					}
				case "document":
					if part, ok := claudeDocumentBlockToOpenAI(block); ok {
						orderedContent = append(orderedContent, part)
					}
				case "thinking":
					if thinking, ok := block["thinking"].(string); ok && thinking != "" {
						reasoningParts = append(reasoningParts, thinking)
					}
				case "tool_use":
					id, _ := block["id"].(string)
					name, _ := block["name"].(string)
					var args string
					switch input := block["input"].(type) {
					case string:
						args = input
					default:
						if input != nil {
							b, _ := json.Marshal(input)
							args = string(b)
						}
					}
					if args == "" {
						args = "{}"
					}
					toolCalls = append(toolCalls, ToolCall{
						ID:   id,
						Type: "function",
						Function: FunctionCall{
							Name:      name,
							Arguments: args,
						},
					})
				case "tool_result":
					toolUseID, _ := block["tool_use_id"].(string)
					var resultText string
					var attachmentParts []any // local per-block image/document parts in original order
					switch c := block["content"].(type) {
					case string:
						resultText = c
					case []any:
						var parts []string
						for _, p := range c {
							pb, ok := p.(map[string]any)
							if !ok {
								continue
							}
							switch pb["type"] {
							case "text":
								if t, ok := pb["text"].(string); ok {
									parts = append(parts, t)
								}
							case "image":
								if part, ok := claudeImageBlockToOpenAI(pb); ok {
									attachmentParts = append(attachmentParts, part)
								}
							case "document":
								if part, ok := claudeDocumentBlockToOpenAI(pb); ok {
									attachmentParts = append(attachmentParts, part)
								}
							}
						}
						resultText = strings.Join(parts, "\n")
					default:
						if c != nil {
							b, _ := json.Marshal(c)
							resultText = string(b)
						}
					}
					// Annotate based on this block's own attachments, not a
					// global accumulator, so parallel tool_results are labeled
					// independently.
					if len(attachmentParts) > 0 {
						if resultText != "" {
							resultText += "\n"
						}
						var labels []string
						for _, ap := range attachmentParts {
							if m, ok := ap.(map[string]any); ok {
								if m["type"] == "image_url" {
									labels = append(labels, "[image attached]")
								} else if m["type"] == "file" {
									labels = append(labels, "[document attached]")
								}
							}
						}
						resultText += strings.Join(labels, "\n")
						followupAttachments = append(followupAttachments, attachmentParts...)
					}
					if isError, _ := block["is_error"].(bool); isError {
						resultText = applyErrorPrefix(resultText)
					}
					toolResults = append(toolResults, Message{
						Role:       "tool",
						ToolCallID: toolUseID,
						Content:    resultText,
					})
				}
			}
			om := Message{Role: msg.Role}
			if len(orderedContent) > 0 {
				om.Content = orderedContent
			} else if len(toolCalls) == 0 {
				om.Content = ""
			}
			if len(reasoningParts) > 0 {
				rc := strings.Join(reasoningParts, "\n")
				om.ReasoningContent = &rc
			}
			if len(toolCalls) > 0 {
				om.ToolCalls = toolCalls
			}
			// Anthropic requires tool_result blocks to precede ordinary user
			// content. Preserve that order when translating them to Chat
			// Completions' separate tool messages.
			if msg.Role == "user" {
				body = append(body, toolResults...)
				if len(followupAttachments) > 0 {
					body = append(body, Message{Role: "user", Content: followupAttachments})
				}
			}
			if len(orderedContent) > 0 || len(reasoningParts) > 0 || len(toolCalls) > 0 || len(toolResults) == 0 {
				body = append(body, om)
			}
			if msg.Role != "user" {
				body = append(body, toolResults...)
				if len(followupAttachments) > 0 {
					body = append(body, Message{Role: "user", Content: followupAttachments})
				}
			}
		default:
			b, _ := json.Marshal(content)
			body = append(body, Message{Role: msg.Role, Content: string(b)})
		}
	}

	var messages []Message
	if len(systemParts) > 0 {
		messages = append(messages, Message{Role: "system", Content: strings.Join(systemParts, "\n\n")})
	}
	messages = append(messages, body...)
	return messages
}

func claudeToOpenAITools(claudeTools []ClaudeTool) ([]Tool, []string) {
	tools := make([]Tool, 0, len(claudeTools))
	var skipped []string
	for _, ct := range claudeTools {
		// Server tools (web_search_*, etc.) carry a vendor type and no client schema.
		// Emitting them as empty function tools would invite bogus model calls.
		if ct.Type != "" && ct.InputSchema == nil {
			skipped = append(skipped, ct.Name)
			continue
		}
		params := ct.InputSchema
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		params = cleanJsonSchema(params)
		paramsMap, ok := params.(map[string]any)
		if !ok {
			paramsMap = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, Tool{
			Type: "function",
			Function: ToolFunction{
				Name:        ct.Name,
				Description: ct.Description,
				Parameters:  paramsMap,
			},
		})
	}
	return tools, skipped
}

func countClaudeSystemParts(msgs []ClaudeMessage, system any) int {
	n := 0
	if extractClaudeSystemText(system) != "" {
		n++
	}
	for _, msg := range msgs {
		if msg.Role == "system" && extractClaudeContentText(msg.Content) != "" {
			n++
		}
	}
	return n
}

func countAnthropicBetas(header string) int {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	n := 0
	for _, part := range strings.Split(header, ",") {
		if strings.TrimSpace(part) != "" {
			n++
		}
	}
	return n
}

// countCacheControlInValue counts cache_control breakpoints on content
// blocks within system, message content, and tool_result content arrays. It
// recurses into all values except input_schema and tool_use input, so a
// property named cache_control inside a schema or input object is not
// falsely counted as a breakpoint.
func countCacheControlInValue(v any) int {
	switch x := v.(type) {
	case map[string]any:
		n := 0
		if _, ok := x["cache_control"]; ok {
			n++
		}
		for key, child := range x {
			// Skip input_schema and input — cache_control inside these is a
			// schema/input property, not a content-block breakpoint.
			if key == "input_schema" || key == "input" {
				continue
			}
			n += countCacheControlInValue(child)
		}
		return n
	case []any:
		n := 0
		for _, child := range x {
			n += countCacheControlInValue(child)
		}
		return n
	default:
		return 0
	}
}

func countClaudeCacheControlBlocks(req ClaudeRequest) int {
	n := countCacheControlInValue(req.System)
	for _, msg := range req.Messages {
		n += countCacheControlInValue(msg.Content)
	}
	// Count actual tool-level cache_control breakpoints on tool definitions,
	// not properties named cache_control inside input_schema.
	for _, tool := range req.Tools {
		if tool.CacheControl != nil {
			n++
		}
	}
	return n
}

// countClaudeThinkingSignatures counts non-empty signature fields on
// thinking content blocks at the top level of each message's content array.
// Only actual message content blocks are counted — not nested values inside
// tool_use input or other objects that happen to have type:"thinking" and a
// signature key. The signature content itself is never recorded; only the
// count is exposed in request_plan for observability. These signatures have
// no Chat Completions equivalent and are dropped upstream.
func countClaudeThinkingSignatures(msgs []ClaudeMessage) int {
	var n int
	for _, msg := range msgs {
		blocks, ok := msg.Content.([]any)
		if !ok {
			continue
		}
		for _, item := range blocks {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := block["type"].(string); t == "thinking" {
				if sig, ok := block["signature"].(string); ok && sig != "" {
					n++
				}
			}
		}
	}
	return n
}

// claudeUnsupportedBlockTypes lists Anthropic content block types that are
// dropped without a structured upstream representation. document is handled
// as a best-effort file part (see claudeDocumentBlockToOpenAI) and is not
// listed here so it is not counted as unsupported.
var claudeUnsupportedBlockTypes = map[string]struct{}{
	"redacted_thinking":      {},
	"search_result":          {},
	"server_tool_use":        {},
	"web_search_tool_result": {},
	"container_upload":       {},
}

func scanClaudeUnsupportedBlocks(msgs []ClaudeMessage) map[string]int {
	counts := map[string]int{}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if t, _ := x["type"].(string); t != "" {
				if _, ok := claudeUnsupportedBlockTypes[t]; ok {
					counts[t]++
				}
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	for _, msg := range msgs {
		walk(msg.Content)
	}
	if len(counts) == 0 {
		return nil
	}
	return counts
}

func openAIToClaudeResponse(chatBody []byte, model string, wantReasoning bool) []byte {
	var chat struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Choices []struct {
			Message struct {
				Content          string     `json:"content"`
				ReasoningContent string     `json:"reasoning_content"`
				ToolCalls        []ToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		slog.Warn("openAIToClaudeResponse unmarshal failed", "error", err)
	}

	content := []ClaudeContent{}
	stopReason := "end_turn"

	if len(chat.Choices) > 0 {
		msg := chat.Choices[0].Message
		fr := chat.Choices[0].FinishReason

		// Try to read private ordered Anthropic content blocks first.
		var rawMsg map[string]any
		privateBlocks := []map[string]any(nil)
		if json.Unmarshal(chatBody, &rawMsg) == nil {
			if choices, ok := rawMsg["choices"].([]any); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]any); ok {
					if m, ok := choice["message"].(map[string]any); ok {
						if pb, ok := m["_opencode2api_anthropic_content"].([]any); ok {
							for _, item := range pb {
								if blk, ok := item.(map[string]any); ok {
									privateBlocks = append(privateBlocks, blk)
								}
							}
						}
					}
				}
			}
		}

		if len(privateBlocks) > 0 {
			// Consume private ordered blocks in array order.
			for _, blk := range privateBlocks {
				bt, _ := blk["type"].(string)
				switch bt {
				case "text":
					text, _ := blk["text"].(string)
					content = append(content, ClaudeContent{
						Type: "text",
						Text: text,
					})
				case "thinking":
					if wantReasoning {
						thinking, _ := blk["thinking"].(string)
						cc := ClaudeContent{
							Type:     "thinking",
							Thinking: thinking,
						}
						if sig, ok := blk["signature"].(string); ok && sig != "" {
							cc.Signature = sig
						}
						content = append(content, cc)
					}
				case "redacted_thinking":
					if wantReasoning {
						cc := ClaudeContent{
							Type: "redacted_thinking",
						}
						if d, ok := blk["data"].(string); ok && d != "" {
							cc.Data = d
						}
						content = append(content, cc)
					}
				case "tool_use":
					id, _ := blk["id"].(string)
					name, _ := blk["name"].(string)
					input := blk["input"]
					if input == nil {
						input = map[string]any{}
					}
					content = append(content, ClaudeContent{
						Type:  "tool_use",
						ID:    id,
						Name:  name,
						Input: input,
					})
				}
			}
		} else {
			// Fallback: string content + reasoning_content + tool_calls.
			if wantReasoning && msg.ReasoningContent != "" {
				content = append(content, ClaudeContent{
					Type:     "thinking",
					Thinking: msg.ReasoningContent,
				})
			}
			text := msg.Content
			// #37635: Go gateway often puts the whole answer in reasoning_content.
			// Promote to text when content is empty so Claude Code does not see an
			// empty end_turn and exit the agent loop.
			if text == "" && msg.ReasoningContent != "" && len(msg.ToolCalls) == 0 {
				text = msg.ReasoningContent
			}
			if text != "" {
				content = append(content, ClaudeContent{
					Type: "text",
					Text: text,
				})
			}
			for _, tc := range msg.ToolCalls {
				var input any
				json.Unmarshal([]byte(tc.Function.Arguments), &input)
				if input == nil {
					input = map[string]any{}
				}
				content = append(content, ClaudeContent{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: input,
				})
			}
		}

		switch fr {
		case "stop":
			stopReason = "end_turn"
		case "length":
			stopReason = "max_tokens"
		case "tool_calls", "function_call":
			stopReason = "tool_use"
		case "content_filter":
			stopReason = "refusal"
		}
	}

	if len(content) == 0 {
		content = append(content, ClaudeContent{Type: "text", Text: ""})
	}

	// Response ID: keep upstream ID only if it is a valid msg_ ID;
	// otherwise generate a new msg_ ID. Never leak chatcmpl/resp IDs.
	respID := normalizeClaudeMessageID(chat.ID)

	resp := ClaudeResponse{
		ID:         respID,
		Type:       "message",
		Role:       "assistant",
		Content:    content,
		Model:      model,
		StopReason: stopReason,
	}
	if chat.Usage != nil {
		resp.Usage = buildClaudeMessageUsage(chat.Usage)
	}
	result, _ := json.Marshal(resp)
	return result
}

func toFloat64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}

func usageIntField(fields map[string]any, key string) (int, bool) {
	if fields == nil {
		return 0, false
	}
	value, ok := fields[key]
	if !ok || value == nil {
		return 0, false
	}
	return int(toFloat64(value)), true
}

func usageMapField(fields map[string]any, key string) (map[string]any, bool) {
	if fields == nil {
		return nil, false
	}
	value, ok := fields[key]
	if !ok || value == nil {
		return nil, false
	}
	mapped, ok := value.(map[string]any)
	return mapped, ok
}

func buildClaudeUsageCore(upstreamUsage map[string]any) ClaudeUsage {
	if len(upstreamUsage) == 0 {
		return nil
	}

	usage := ClaudeUsage{}
	if value, ok := usageIntField(upstreamUsage, "prompt_tokens"); ok {
		usage["input_tokens"] = value
	}
	if value, ok := usageIntField(upstreamUsage, "input_tokens"); ok {
		if _, exists := usage["input_tokens"]; !exists {
			usage["input_tokens"] = value
		}
	}
	if value, ok := usageIntField(upstreamUsage, "completion_tokens"); ok {
		usage["output_tokens"] = value
	}
	if value, ok := usageIntField(upstreamUsage, "output_tokens"); ok {
		if _, exists := usage["output_tokens"]; !exists {
			usage["output_tokens"] = value
		}
	}
	if value, ok := usageIntField(upstreamUsage, "cache_creation_input_tokens"); ok {
		usage["cache_creation_input_tokens"] = value
	}
	if value, ok := usageIntField(upstreamUsage, "cache_read_input_tokens"); ok {
		usage["cache_read_input_tokens"] = value
	} else if promptDetails, ok := usageMapField(upstreamUsage, "prompt_tokens_details"); ok {
		if value, ok := usageIntField(promptDetails, "cached_tokens"); ok {
			usage["cache_read_input_tokens"] = value
		}
	}
	if outputDetails, ok := usageMapField(upstreamUsage, "output_tokens_details"); ok {
		usage["output_tokens_details"] = outputDetails
	} else if outputDetails, ok := usageMapField(upstreamUsage, "completion_tokens_details"); ok {
		usage["output_tokens_details"] = outputDetails
	}
	if serverToolUse, ok := usageMapField(upstreamUsage, "server_tool_use"); ok {
		usage["server_tool_use"] = serverToolUse
	}
	if len(usage) == 0 {
		return nil
	}
	return usage
}

func buildClaudeMessageUsage(upstreamUsage map[string]any) ClaudeUsage {
	usage := buildClaudeUsageCore(upstreamUsage)
	if usage == nil {
		usage = ClaudeUsage{}
	}
	if cacheCreation, ok := usageMapField(upstreamUsage, "cache_creation"); ok {
		usage["cache_creation"] = cacheCreation
	}
	if serviceTier, ok := upstreamUsage["service_tier"].(string); ok && serviceTier != "" {
		usage["service_tier"] = serviceTier
	}
	if inferenceGeo, ok := upstreamUsage["inference_geo"].(string); ok && inferenceGeo != "" {
		usage["inference_geo"] = inferenceGeo
	}
	if _, exists := usage["input_tokens"]; !exists {
		usage["input_tokens"] = 0
	}
	if _, exists := usage["output_tokens"]; !exists {
		usage["output_tokens"] = 0
	}
	return usage
}

func buildClaudeDeltaUsage(upstreamUsage map[string]any) ClaudeUsage {
	usage := buildClaudeUsageCore(upstreamUsage)
	if usage == nil {
		usage = ClaudeUsage{}
	}
	if _, exists := usage["output_tokens"]; !exists {
		usage["output_tokens"] = 0
	}
	return usage
}

func claudeMessagesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	auth := extractUpstreamAuth(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	cnt := requestCount.Add(1)
	maybeLogBodySummary(r.Context(), "claude messages request body", body)
	_ = cnt

	var claudeReq ClaudeRequest
	if err := json.Unmarshal(body, &claudeReq); err != nil {
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"Invalid JSON"}}`, http.StatusBadRequest)
		return
	}
	modelIn := claudeReq.Model
	claudeReq.Model = resolveModel(claudeReq.Model)
	claudeReq.Model = mapPublicToFreeModel(auth, claudeReq.Model)
	if !validateRequestTemperature(w, claudeReq.Temperature, "claude", 0, 1) {
		return
	}
	if msg := validateClaudeDocumentBlocks(claudeReq.Messages); msg != "" {
		writeProtocolValidation400(w, "claude", "", msg)
		return
	}

	// 多模态路由

	chatReq, skippedServerTools := convertClaudeRequest(claudeReq)
	chatReq.Messages = fixToolCallGaps(chatReq.Messages)
	if claudeReq.Stream {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["stream_options"] = map[string]any{"include_usage": true}
	}

	// Keep CoT by default so Claude Code still sees thinking blocks. Only drop
	// reasoning when force-disabled or the client explicitly disables thinking.
	// Empty-reply protection is handled by promoteMisplacedReasoning (!keep)
	// and emitEmptyTextFallback (keep + no text/tool_use).
	wantReasoning := !getForceDisableThinking()
	if claudeReq.Thinking != nil && isThinkingDisabled(claudeReq.Thinking) {
		wantReasoning = false
	}
	keepReasoning := wantReasoning
	chatReq.Messages = ensureReasoningContent(chatReq.Messages, keepReasoning)

	effortIn := chatReq.ReasoningEffort
	if effortIn == "" && !isThinkingDisabled(claudeReq.Thinking) {
		effortIn = reasoningEffortFromThinking(claudeReq.Thinking)
	}
	upstreamSurface := "zen"
	if auth.shouldUseGoEndpoint(chatReq.Model) {
		upstreamSurface = "go"
	}
	systemMerged := countClaudeSystemParts(claudeReq.Messages, claudeReq.System) > 1
	plan := map[string]any{
		"protocol":                "claude",
		"model_in":                modelIn,
		"model_resolved":          chatReq.Model,
		"auth_mode":               authModeString(auth.Mode),
		"auth_source":             auth.Source,
		"has_key":                 auth.Token != "",
		"upstream_surface":        upstreamSurface,
		"stream":                  claudeReq.Stream,
		"keep_reasoning":          keepReasoning,
		"thinking":                thinkingState(claudeReq.Thinking),
		"reasoning_effort_in":     effortIn,
		"reasoning_effort_out":    mappedReasoningEffort(effortIn),
		"tools_count":             len(chatReq.Tools),
		"messages_count":          len(chatReq.Messages),
		"system_merged":           systemMerged,
		"context_management":      claudeReq.ContextManagement != nil,
		"cache_control_blocks":    countClaudeCacheControlBlocks(claudeReq),
		"history_signature_count": countClaudeThinkingSignatures(claudeReq.Messages),
		"client_beta_count":       countAnthropicBetas(r.Header.Get("anthropic-beta")),
		"unsupported_blocks":      scanClaudeUnsupportedBlocks(claudeReq.Messages),
		"max_tokens":              chatReq.MaxTokens,
		"max_tokens_cap":          getMaxTokensCapForModel(chatReq.Model),
	}
	if len(skippedServerTools) > 0 {
		plan["skipped_server_tools"] = skippedServerTools
	}
	logRequestPlan(r.Context(), plan)

	upstreamBody := buildUpstreamBody(&chatReq)

	if claudeReq.Stream {
		upResp, status, _, err := callOpenCodeAPIStream(r.Context(), upstreamBody, chatReq.Model, auth)
		if err != nil || status < 200 || status >= 300 {
			errResp := map[string]any{
				"type":  "error",
				"error": map[string]string{"type": "api_error", "message": "upstream error"},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(errResp)
			return
		}
		defer upResp.Close()
		claudeStreamHandler(r.Context(), w, upResp, claudeReq.Model, keepReasoning)
		return
	}

	respBody, status, _, err := callOpenCodeAPI(r.Context(), upstreamBody, chatReq.Model, auth)
	if err != nil || status < 200 || status >= 300 {
		if err != nil {
			writeUpstreamError(w, status, err, "claude")
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if len(respBody) > 0 {
				w.Write(respBody)
			} else {
				json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": "upstream error"}})
			}
		}
		return
	}

	claudeRespBody := openAIToClaudeResponse(respBody, claudeReq.Model, wantReasoning)
	result := summarizeClaudeResult(claudeRespBody)
	if !wantReasoning {
		var before map[string]any
		if json.Unmarshal(respBody, &before) == nil {
			if choices, ok := before["choices"].([]any); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]any); ok {
					if msg, ok := choice["message"].(map[string]any); ok {
						content, _ := msg["content"].(string)
						rc, _ := msg["reasoning_content"].(string)
						if content == "" && rc != "" {
							result["promoted_reasoning"] = true
						}
					}
				}
			}
		}
	}
	logRequestResult(r.Context(), result)

	// Record token usage
	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			pt, _ := u["prompt_tokens"].(float64)
			ct, _ := u["completion_tokens"].(float64)
			tt, _ := u["total_tokens"].(float64)
			if tt > 0 {
				recordTokenUsage(claudeReq.Model, int64(pt), int64(ct), int64(tt))
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	maybeLogBodySummary(r.Context(), "claude response body", claudeRespBody)
	w.Write(claudeRespBody)
}

var claudeKeepaliveInterval = 15 * time.Second

func claudeStreamHandler(ctx context.Context, w http.ResponseWriter, respBody io.ReadCloser, model string, keepReasoning bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(respBody)
	stats := &streamResultStats{start: time.Now()}

	msgID := fmt.Sprintf("msg_%s", randomString(24))
	blockIndex := 0
	thinkingBlockOpen := false
	textBlockOpen := false
	toolCallAccumulator := map[int]map[string]string{}
	toolBlockIndices := map[int]int{}
	toolCallOrder := []int{}
	messageStartSent := false
	finished := false
	stopReason := "end_turn"
	fullUsage := map[string]any{}
	// Accumulates reasoning when keepReasoning so we can fall back to a text
	// block if the stream never produces content/tool_use (#37635).
	reasoningFallback := strings.Builder{}

	// --- Reader goroutine -> channel so the main loop can select on
	// ticker/read/context without blocking, and so context cancellation
	// unblocks the reader via Close. ---
	type readResult struct {
		line string
		err  error
	}
	readCh := make(chan readResult)
	readerDone := make(chan struct{})
	readerExited := make(chan struct{})

	go func() {
		defer close(readerExited)
		for {
			line, err := reader.ReadString('\n')
			select {
			case readCh <- readResult{line: line, err: err}:
			case <-readerDone:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	keepaliveInterval := claudeKeepaliveInterval
	if keepaliveInterval <= 0 {
		keepaliveInterval = 15 * time.Second
	}
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()

	defer func() {
		if len(fullUsage) > 0 {
			pt, _ := fullUsage["prompt_tokens"].(float64)
			ct, _ := fullUsage["completion_tokens"].(float64)
			tt, _ := fullUsage["total_tokens"].(float64)
			if tt > 0 {
				recordTokenUsage(model, int64(pt), int64(ct), int64(tt))
			}
		}
		stats.toolCallCount = len(toolCallOrder)
		stats.log(ctx, "claude")
	}()
	// Reader cleanup: signal goroutine, unblock any pending read, wait for exit.
	defer func() {
		close(readerDone)
		respBody.Close()
		<-readerExited
	}()

	emitClaudeEvent := func(event string, data any) {
		jsonData, err := json.Marshal(data)
		if err != nil {
			reqLogger(ctx).Error("marshal SSE event failed", "error", err)
			return
		}
		w.Write([]byte("event: " + event + "\n"))
		w.Write([]byte("data: " + string(jsonData) + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	emitClaudeError := func(msg string) {
		emitClaudeEvent("error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": msg,
			},
		})
	}

	closeThinkingBlock := func() {
		if !thinkingBlockOpen {
			return
		}
		emitClaudeEvent("content_block_stop", map[string]any{
			"type":          "content_block_stop",
			"index":         blockIndex - 1,
			"content_block": map[string]any{"type": "thinking"},
		})
		thinkingBlockOpen = false
	}

	closeTextBlock := func() {
		if !textBlockOpen {
			return
		}
		emitClaudeEvent("content_block_stop", map[string]any{
			"type":          "content_block_stop",
			"index":         blockIndex - 1,
			"content_block": map[string]any{"type": "text"},
		})
		textBlockOpen = false
	}

	ensureMessageStart := func() {
		if messageStartSent {
			return
		}
		messageStartSent = true
		emitClaudeEvent("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":          msgID,
				"type":        "message",
				"role":        "assistant",
				"content":     []any{},
				"model":       model,
				"stop_reason": nil,
				"usage":       buildClaudeMessageUsage(fullUsage),
			},
		})
		emitClaudeEvent("ping", map[string]any{"type": "ping"})
	}

	emitTextDelta := func(contentStr string) {
		if contentStr == "" {
			return
		}
		stats.textChars += len(contentStr)
		closeThinkingBlock()
		if !textBlockOpen {
			emitClaudeEvent("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": blockIndex,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			})
			textBlockOpen = true
			blockIndex++
		}
		emitClaudeEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": blockIndex - 1,
			"delta": map[string]any{
				"type": "text_delta",
				"text": contentStr,
			},
		})
	}

	emitEmptyTextFallback := func() {
		if textBlockOpen || len(toolCallOrder) > 0 {
			return
		}
		fallback := reasoningFallback.String()
		if fallback == "" {
			return
		}
		stats.promotedReasoning = true
		emitTextDelta(fallback)
	}

	finalizeContentBlocks := func() {
		emitEmptyTextFallback()
		closeThinkingBlock()
		closeTextBlock()
		for _, idx := range toolCallOrder {
			acc := toolCallAccumulator[idx]
			emitClaudeEvent("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": toolBlockIndices[idx],
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    acc["id"],
					"name":  acc["name"],
					"input": map[string]any{},
				},
			})
		}
	}

loop:
	for {
		select {
		case <-ctx.Done():
			// Client cancelled: quiet exit, no error writes.
			return
		case <-ticker.C:
			// Keepalive ping — before the first upstream token this is the
			// only thing the client receives; do NOT fake message_start.
			emitClaudeEvent("ping", map[string]any{"type": "ping"})
		case result := <-readCh:
			// bufio.ReadString may return both a non-empty line and an error
			// (e.g. the last line without a trailing newline + io.EOF). Process
			// the line first, then handle the accompanying error via pendingErr.
			pendingErr := result.err

			line := result.line
			trimmed := strings.TrimSpace(line)
			if trimmed == "data: [DONE]" || trimmed == "[DONE]" {
				stats.doneSeen = true
				if !finished {
					emitClaudeError("stream ended with [DONE] but no finish_reason")
					return
				}
				break loop
			}
			if strings.HasPrefix(line, "data: ") {
				payload := line[6:]
				if strings.TrimSpace(payload) != "" {
					var chunk map[string]any
					if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
						emitClaudeError("stream received malformed JSON data")
						return
					} else {
						// In-band error from upstream.
						if errVal, ok := chunk["error"]; ok && errVal != nil {
							errMsg := "upstream stream error"
							if errMap, ok := errVal.(map[string]any); ok {
								if m, ok := errMap["message"].(string); ok && m != "" {
									errMsg = m
								}
							} else if errStr, ok := errVal.(string); ok && errStr != "" {
								errMsg = errStr
							}
							emitClaudeError(errMsg)
							return
						} else {

							if usage, ok := chunk["usage"].(map[string]any); ok {
								fullUsage = mergeUsageMaps(fullUsage, usage)
							}

							choices, ok := chunk["choices"].([]any)
							if !ok || len(choices) == 0 {
								// Usage-only trailing chunk (OpenAI stream_options.include_usage).
							} else {
								choice, _ := choices[0].(map[string]any)
								delta, _ := choice["delta"].(map[string]any)
								finishReason, _ := choice["finish_reason"].(string)
								stats.noteChunk()

								ensureMessageStart()

								// After finish_reason, ignore further content deltas but keep reading
								// so a later usage-only chunk can populate fullUsage.
								if !finished {
									if rc, ok := delta["reasoning_content"]; ok {
										rcStr, _ := rc.(string)
										if rcStr != "" {
											stats.reasoningChars += len(rcStr)
											if keepReasoning {
												reasoningFallback.WriteString(rcStr)
												closeTextBlock()
												if !thinkingBlockOpen {
													emitClaudeEvent("content_block_start", map[string]any{
														"type":  "content_block_start",
														"index": blockIndex,
														"content_block": map[string]any{
															"type":     "thinking",
															"thinking": "",
														},
													})
													thinkingBlockOpen = true
													blockIndex++
												}
												emitClaudeEvent("content_block_delta", map[string]any{
													"type":  "content_block_delta",
													"index": blockIndex - 1,
													"delta": map[string]any{
														"type":     "thinking_delta",
														"thinking": rcStr,
													},
												})
											} else {
												// Thinking not requested: promote misplaced CoT to visible text (#37635).
												stats.promotedReasoning = true
												emitTextDelta(rcStr)
											}
										}
									}

									if c, ok := delta["content"]; ok && c != nil {
										contentStr, _ := c.(string)
										if contentStr != "" {
											emitTextDelta(contentStr)
										}
									}

									if rawToolCalls, ok := delta["tool_calls"].([]any); ok {
										for _, rawTC := range rawToolCalls {
											tc, ok := rawTC.(map[string]any)
											if !ok {
												continue
											}
											idxFloat, _ := tc["index"].(float64)
											upstreamIndex := int(idxFloat)

											closeThinkingBlock()
											closeTextBlock()

											if _, exists := toolCallAccumulator[upstreamIndex]; !exists {
												callID, _ := tc["id"].(string)
												if callID == "" {
													callID = "toolu_" + randomString(12)
												}
												fn, _ := tc["function"].(map[string]any)
												name, _ := fn["name"].(string)
												toolCallAccumulator[upstreamIndex] = map[string]string{
													"id":   callID,
													"name": name,
													"args": "",
												}
												toolCallOrder = append(toolCallOrder, upstreamIndex)
												toolBlockIndices[upstreamIndex] = blockIndex
												emitClaudeEvent("content_block_start", map[string]any{
													"type":  "content_block_start",
													"index": blockIndex,
													"content_block": map[string]any{
														"type":  "tool_use",
														"id":    callID,
														"name":  name,
														"input": map[string]any{},
													},
												})
												blockIndex++
											}

											fn, _ := tc["function"].(map[string]any)
											if argDelta, ok := fn["arguments"].(string); ok && argDelta != "" {
												toolCallAccumulator[upstreamIndex]["args"] += argDelta
												emitClaudeEvent("content_block_delta", map[string]any{
													"type":  "content_block_delta",
													"index": toolBlockIndices[upstreamIndex],
													"delta": map[string]any{
														"type":         "input_json_delta",
														"partial_json": argDelta,
													},
												})
											}
										}
									}

									if finishReason == "stop" || finishReason == "length" || finishReason == "tool_calls" || finishReason == "function_call" || finishReason == "content_filter" {
										stats.finishReason = finishReason
										stats.sawFinish = true
										finished = true
										finalizeContentBlocks()

										stopReason = "end_turn"
										switch finishReason {
										case "length":
											stopReason = "max_tokens"
										case "tool_calls", "function_call":
											stopReason = "tool_use"
										case "content_filter":
											stopReason = "refusal"
										}
										// Do not emit message_delta/stop yet: OpenAI-compatible upstreams often
										// send the usage-only chunk after finish_reason when include_usage=true.
									}
								}
							}
						}
					}
				}
			}

			// Now handle a pending error from the read.
			if pendingErr != nil {
				if pendingErr == io.EOF {
					if !finished {
						emitClaudeError("stream ended without finish_reason")
						return
					}
					break loop
				}
				reqLogger(ctx).Error("stream read error", "error", pendingErr)
				emitClaudeError("stream read error")
				return
			}
		}
	}

	// Reached only when finished is true (valid finish_reason seen).
	ensureMessageStart()
	emitClaudeEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason},
		"usage": buildClaudeDeltaUsage(fullUsage),
	})
	emitClaudeEvent("message_stop", map[string]any{"type": "message_stop"})
}

func indexOfInt(slice []int, val int) int {
	for i, v := range slice {
		if v == val {
			return i
		}
	}
	return 0
}

// ======================== Responses API ========================

func responsesInputToMessages(input any, instructions string) []Message {
	var messages []Message
	if instructions != "" {
		messages = append(messages, Message{Role: "system", Content: instructions})
	}
	switch v := input.(type) {
	case string:
		messages = append(messages, Message{Role: "user", Content: v})
	case []any:
		functionOutputs := collectFunctionOutputs(v)
		// Pre-collect call IDs present in this input array so output items
		// whose matching call is also present are not independently appended
		// (the call branch emits the paired tool message). Standalone outputs
		// (e.g. previous-response-id replay) have no matching call and still
		// append independently. Uses the same call-ID extraction rule as the
		// call branch: call_id → id → nested tool_use.id.
		callIDsPresent := map[string]bool{}
		for _, item := range v {
			elem, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch elem["type"] {
			case "function_call", "tool_call", "apply_patch_call", "shell_call":
				cid, _ := elem["call_id"].(string)
				if cid == "" {
					cid, _ = elem["id"].(string)
				}
				if cid == "" {
					if tu, ok := elem["tool_use"].(map[string]any); ok {
						cid, _ = tu["id"].(string)
					}
				}
				if cid != "" {
					callIDsPresent[cid] = true
				}
			}
		}
		for _, item := range v {
			switch elem := item.(type) {
			case string:
				messages = append(messages, Message{Role: "user", Content: elem})
			case map[string]any:
				itemType, _ := elem["type"].(string)
				switch itemType {
				case "function_call", "tool_call", "apply_patch_call", "shell_call":
					callID, _ := elem["call_id"].(string)
					if callID == "" {
						callID, _ = elem["id"].(string)
					}
					name, _ := elem["name"].(string)
					if name == "" {
						switch itemType {
						case "apply_patch_call":
							name = "apply_patch"
						case "shell_call":
							name = "shell"
						}
					}
					args, _ := elem["arguments"].(string)
					if name == "" {
						if tu, ok := elem["tool_use"].(map[string]any); ok {
							name, _ = tu["name"].(string)
							callID, _ = tu["id"].(string)
							if a, ok := tu["arguments"].(string); ok {
								args = a
							} else if inp, ok := tu["input"]; ok {
								b, _ := json.Marshal(inp)
								args = string(b)
							}
						}
					}
					if args == "" {
						args = buildBuiltInToolCallArguments(itemType, elem)
					}
					if args == "" {
						args = "{}"
					}
					messages = append(messages, Message{
						Role:    "assistant",
						Content: "",
						ToolCalls: []ToolCall{{
							ID:   callID,
							Type: "function",
							Function: FunctionCall{
								Name:      name,
								Arguments: args,
							},
						}},
					})
					if callID != "" {
						// Map presence (not value=="") decides whether a payload
						// was provided: an empty string is a legitimate output.
						output, hasOutput := functionOutputs[callID]
						if !hasOutput {
							output = "[tool output missing]"
						}
						messages = append(messages, Message{Role: "tool", ToolCallID: callID, Content: output})
					}
				case "function_call_output", "tool_result", "apply_patch_call_output", "shell_call_output":
					callID, _ := elem["call_id"].(string)
					if callID == "" {
						callID, _ = elem["tool_use_id"].(string)
					}
					if callID != "" {
						// If the matching call item is also present in this
						// input array, skip independent emission — the call
						// branch will emit the paired assistant+tool messages,
						// preventing a leading duplicate tool message when
						// output precedes call. Standalone outputs (no matching
						// call, e.g. previous-response-id replay) still append
						// independently.
						if callIDsPresent[callID] {
							continue
						}
						// Map presence (not value=="") decides whether a payload
						// was provided: an empty string is a legitimate output.
						output, hasOutput := functionOutputs[callID]
						if !hasOutput {
							// Fallback for items not collected (e.g. output
							// field absent on a standard *_call_output). Use
							// the single normalizer so Anthropic-style content
							// is honored and the raw tool_result wrapper JSON
							// is never emitted.
							text, present := normalizeToolResultOutput(elem)
							if present {
								output = text
								hasOutput = true
							}
						}
						if !hasOutput {
							output = "[tool output missing]"
						}
						messages = append(messages, Message{Role: "tool", ToolCallID: callID, Content: output})
					}
					continue
				case "reasoning":
					if text := extractTextFromContentParts(elem["summary"]); text != "" {
						messages = append(messages, Message{Role: "assistant", Content: "", ReasoningContent: &text})
					}
					continue
				case "message", "":
					role := "user"
					if r, ok := elem["role"].(string); ok && r != "" {
						role = r
					}
					if role == "developer" {
						role = "system"
					}
					content := responsesContentToMessageContent(elem["content"])
					messages = append(messages, Message{Role: role, Content: content})
				case "input_file":
					// Top-level input_file item (file upload). Map to a user
					// message carrying a structured file part. Malformed items
					// (no payload) are rejected earlier by the handler, so a
					// failure here is dropped rather than serialized as text.
					if file, ok := responsesInputFileToFile(elem); ok {
						messages = append(messages, Message{
							Role:    "user",
							Content: []any{map[string]any{"type": "file", "file": file}},
						})
					}
					continue
				default:
					role := "user"
					if r, ok := elem["role"].(string); ok && r != "" {
						role = r
					}
					content := responsesContentToMessageContent(elem["content"])
					emptyContent := false
					switch v := content.(type) {
					case nil:
						emptyContent = true
					case string:
						emptyContent = v == ""
					case []any:
						emptyContent = len(v) == 0
					}
					if emptyContent {
						b, err := json.Marshal(elem)
						if err != nil {
							continue
						}
						content = string(b)
					}
					messages = append(messages, Message{Role: role, Content: content})
				}
			default:
				b, _ := json.Marshal(elem)
				messages = append(messages, Message{Role: "user", Content: string(b)})
			}
		}
	default:
		b, _ := json.Marshal(v)
		messages = append(messages, Message{Role: "user", Content: string(b)})
	}
	return messages
}

func convertResponsesTools(tools []ResponsesTool) []Tool {
	converted := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		fn, ok := responsesToolFunction(tool)
		if !ok {
			continue
		}
		converted = append(converted, Tool{Type: "function", Function: fn})
	}
	return converted
}

func responsesToolFunction(tool ResponsesTool) (ToolFunction, bool) {
	switch tool.Type {
	case "function":
		fn := ToolFunction{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  tool.Parameters,
		}
		if tool.Function != nil {
			fn = *tool.Function
		}
		if fn.Parameters == nil {
			fn.Parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		return fn, true
	case "apply_patch":
		return ToolFunction{
			Name:        "apply_patch",
			Description: "Create, update, or delete files using a structured patch operation or unified diff.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"input": map[string]any{
						"type":        "string",
						"description": "Patch diff or patch instructions to apply.",
					},
					"operation": map[string]any{
						"type":        "object",
						"description": "Structured patch operation, including file action and diff payload.",
					},
				},
			},
		}, true
	case "shell":
		return ToolFunction{
			Name:        "shell",
			Description: "Run a shell command in the local workspace and return stdout, stderr, and exit details.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "Shell command to execute.",
					},
					"timeout_ms": map[string]any{
						"type":        "integer",
						"description": "Optional timeout in milliseconds.",
					},
					"working_directory": map[string]any{
						"type":        "string",
						"description": "Optional working directory for the command.",
					},
					"max_output_tokens": map[string]any{
						"type":        "integer",
						"description": "Optional output budget hint.",
					},
				},
				"required": []string{"command"},
			},
		}, true
	default:
		return ToolFunction{}, false
	}
}

func responsesToolName(tool ResponsesTool) string {
	switch tool.Type {
	case "function":
		if tool.Function != nil && tool.Function.Name != "" {
			return tool.Function.Name
		}
		return tool.Name
	case "apply_patch":
		return "apply_patch"
	case "shell":
		return "shell"
	default:
		return ""
	}
}

func responsesToolKindMap(tools []ResponsesTool) map[string]string {
	kinds := make(map[string]string, len(tools))
	for _, tool := range tools {
		name := responsesToolName(tool)
		if name == "" {
			continue
		}
		kinds[name] = tool.Type
	}
	return kinds
}

// includeHas reports whether the include array contains the given key.
func includeHas(include []string, key string) bool {
	for _, v := range include {
		if v == key {
			return true
		}
	}
	return false
}

func toolCallOutputType(name string, kinds map[string]string) string {
	switch kinds[name] {
	case "apply_patch":
		return "apply_patch_call"
	case "shell":
		return "shell_call"
	default:
		return "function_call"
	}
}

func convertResponsesToolChoice(choice any) any {
	if choice == nil {
		return nil
	}
	choiceMap, ok := choice.(map[string]any)
	if !ok {
		return choice
	}
	if choiceMap["type"] == "function" {
		if name, ok := choiceMap["name"].(string); ok && name != "" {
			return map[string]any{
				"type":     "function",
				"function": map[string]any{"name": name},
			}
		}
	}
	if choiceType, ok := choiceMap["type"].(string); ok {
		switch choiceType {
		case "apply_patch", "shell":
			return map[string]any{
				"type":     "function",
				"function": map[string]any{"name": choiceType},
			}
		}
	}
	return choice
}

// toolResultOutputKind marks the output item types that carry a tool/function
// output payload. tool_result is the Anthropic-style alias accepted by the
// Responses entrypoint in addition to the standard *_call_output types.
var toolResultOutputKind = map[string]struct{}{
	"function_call_output":    {},
	"apply_patch_call_output": {},
	"shell_call_output":       {},
	"tool_result":             {},
}

func collectFunctionOutputs(items []any) map[string]string {
	outputs := map[string]string{}
	for _, item := range items {
		elem, ok := item.(map[string]any)
		if !ok {
			continue
		}
		itemType, _ := elem["type"].(string)
		if _, ok := toolResultOutputKind[itemType]; !ok {
			continue
		}
		// Standard Responses items use call_id; Anthropic-style tool_result
		// uses tool_use_id when call_id is absent.
		callID, _ := elem["call_id"].(string)
		if callID == "" {
			callID, _ = elem["tool_use_id"].(string)
		}
		if callID == "" {
			continue
		}
		text, present := normalizeToolResultOutput(elem)
		if present {
			outputs[callID] = text
		}
		// When no payload is present, the key is left absent so the caller
		// surfaces "[tool output missing]" — the raw wrapper JSON is never
		// stored as the output.
	}
	return outputs
}

// normalizeToolResultOutput is the single helper that extracts a textual
// output from a tool/function output item. It prefers the standard `output`
// field; for Anthropic-style tool_result it reads `content` when `output` is
// absent. content supports a string, a string array, or an array of
// {type:"text"|"input_text"|"output_text", text:"..."} blocks joined by
// newlines in original order. The boolean reports whether a payload was
// present (an empty string is a legitimate provided output).
func normalizeToolResultOutput(elem map[string]any) (string, bool) {
	var text string
	present := false
	// Standard `output` field takes priority.
	if v, ok := elem["output"]; ok && v != nil {
		switch s := v.(type) {
		case string:
			text = s
		default:
			b, _ := json.Marshal(v)
			text = string(b)
		}
		present = true
	} else if c, ok := elem["content"]; ok && c != nil {
		// Anthropic-style tool_result uses `content`.
		text = joinToolResultContent(c)
		present = true
	}
	if !present {
		return "", false
	}
	// Apply is_error prefix here so the collected map already carries error
	// semantics, independent of call/output ordering in the array.
	if isError, _ := elem["is_error"].(bool); isError {
		text = applyErrorPrefix(text)
	}
	return text, true
}

// joinToolResultContent renders an Anthropic tool_result content value to text.
func joinToolResultContent(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			pb, ok := p.(map[string]any)
			if !ok {
				if s, ok := p.(string); ok {
					parts = append(parts, s)
				}
				continue
			}
			switch pb["type"] {
			case "text", "input_text", "output_text":
				if t, ok := pb["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		if c != nil {
			b, _ := json.Marshal(c)
			return string(b)
		}
		return ""
	}
}

func parseJSONString(input string) any {
	var parsed any
	if input == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(input), &parsed); err != nil {
		return nil
	}
	return parsed
}

func buildBuiltInToolCallArguments(itemType string, elem map[string]any) string {
	if arguments, ok := elem["arguments"].(string); ok && arguments != "" {
		return arguments
	}

	payload := map[string]any{}
	switch itemType {
	case "apply_patch_call":
		if input, ok := elem["input"].(string); ok && input != "" {
			payload["input"] = input
		}
		if operation, ok := elem["operation"]; ok && operation != nil {
			payload["operation"] = operation
		}
	case "shell_call":
		for _, key := range []string{"command", "timeout_ms", "working_directory", "max_output_tokens"} {
			if value, ok := elem[key]; ok && value != nil {
				payload[key] = value
			}
		}
	}
	if len(payload) == 0 {
		payload = elem
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func buildResponseToolCallItem(tc ToolCall, outputType string) map[string]any {
	switch outputType {
	case "apply_patch_call":
		item := map[string]any{
			"id":      "apc_" + tc.ID,
			"type":    outputType,
			"status":  "completed",
			"call_id": tc.ID,
		}
		if parsed, ok := parseJSONString(tc.Function.Arguments).(map[string]any); ok {
			for key, value := range parsed {
				item[key] = value
			}
		} else if tc.Function.Arguments != "" {
			item["arguments"] = tc.Function.Arguments
		}
		return item
	case "shell_call":
		item := map[string]any{
			"id":      "shc_" + tc.ID,
			"type":    outputType,
			"status":  "completed",
			"call_id": tc.ID,
		}
		if parsed, ok := parseJSONString(tc.Function.Arguments).(map[string]any); ok {
			for key, value := range parsed {
				item[key] = value
			}
		} else if tc.Function.Arguments != "" {
			item["arguments"] = tc.Function.Arguments
		}
		return item
	default:
		return map[string]any{
			"id":        "fc_" + tc.ID,
			"type":      "function_call",
			"status":    "completed",
			"arguments": tc.Function.Arguments,
			"call_id":   tc.ID,
			"name":      tc.Function.Name,
		}
	}
}

func cloneJSONValue[T any](value T) T {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var cloned T
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		return value
	}
	return cloned
}

func storeResponseState(response map[string]any, req ResponsesAPIRequest) {
	if req.Store != nil && !*req.Store {
		return
	}
	responseID, _ := response["id"].(string)
	if responseID == "" {
		return
	}
	output, _ := response["output"].([]any)
	storedResponsesMu.Lock()
	storedResponses[responseID] = StoredResponseState{
		Model:        req.Model,
		Instructions: req.Instructions,
		Tools:        cloneJSONValue(req.Tools),
		ToolChoice:   cloneJSONValue(req.ToolChoice),
		Output:       cloneJSONValue(output),
	}
	storedResponsesMu.Unlock()
}

func loadResponseState(responseID string) (StoredResponseState, bool) {
	storedResponsesMu.RLock()
	defer storedResponsesMu.RUnlock()
	state, ok := storedResponses[responseID]
	if !ok {
		return StoredResponseState{}, false
	}
	return cloneJSONValue(state), true
}

func extractTextFromContentParts(content any) string {
	parts, ok := content.([]any)
	if !ok {
		if s, ok := content.(string); ok {
			return s
		}
		return ""
	}
	var texts []string
	for _, p := range parts {
		if part, ok := p.(map[string]any); ok {
			if part["type"] == "input_text" || part["type"] == "output_text" {
				if t, ok := part["text"].(string); ok {
					texts = append(texts, t)
				}
			}
		}
	}
	return strings.Join(texts, "\n")
}

func convertResponsesContentPart(part map[string]any) (map[string]any, bool) {
	partType, _ := part["type"].(string)
	switch partType {
	case "input_text", "output_text", "text":
		text, _ := part["text"].(string)
		if text == "" {
			return nil, false
		}
		return map[string]any{
			"type": "text",
			"text": text,
		}, true
	case "input_image":
		imageURL, _ := part["image_url"].(string)
		if imageURL == "" {
			return nil, false
		}
		imageURLValue := map[string]any{
			"url": imageURL,
		}
		if detail, ok := part["detail"].(string); ok && detail != "" {
			imageURLValue["detail"] = detail
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": imageURLValue,
		}, true
	case "input_file":
		file, ok := responsesInputFileToFile(part)
		if !ok {
			return nil, false
		}
		return map[string]any{"type": "file", "file": file}, true
	default:
		return nil, false
	}
}

// validateClaudeDocumentBlocks scans Anthropic Messages content for document
// blocks that lack a usable source payload. It inspects top-level message
// content blocks and nested tool_result.content blocks, but never descends
// into tool_use input, document source, schemas, or arbitrary domain data.
func validateClaudeDocumentBlocks(msgs []ClaudeMessage) string {
	for _, msg := range msgs {
		if m := validateClaudeDocumentBlocksContent(msg.Content); m != "" {
			return m
		}
	}
	return ""
}

// validateClaudeDocumentBlocksContent inspects a content array's top-level
// blocks. For tool_result blocks, it recurses into the tool_result's own
// content array (which may contain document blocks). It never recurses into
// tool_use input or other arbitrary map values.
func validateClaudeDocumentBlocksContent(content any) string {
	blocks, ok := content.([]any)
	if !ok {
		return ""
	}
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		bt, _ := block["type"].(string)
		if bt == "document" {
			if _, ok := claudeDocumentBlockToOpenAI(block); !ok {
				return "document is missing a usable source payload"
			}
		}
		if bt == "tool_result" {
			// tool_result content is itself a content array that may contain
			// document blocks. Recurse into it, but not into any other fields.
			if m := validateClaudeDocumentBlocksContent(block["content"]); m != "" {
				return m
			}
		}
	}
	return ""
}

// validateResponsesFileItems scans Responses input for input_file content
// parts that are recognized as file inputs but lack any usable payload. It
// inspects top-level items and message content arrays only — it
// never descends into function/tool arguments, nested tool_use.input, file
// payload objects, metadata, or arbitrary maps. Returns a non-empty message
// when a malformed file item is found.
func validateResponsesFileItems(input any) string {
	switch v := input.(type) {
	case []any:
		for _, item := range v {
			if msg := validateResponsesFileItem(item); msg != "" {
				return msg
			}
		}
	}
	return ""
}

// validateResponsesFileItem validates a single top-level input item or a
// content part within a message content array. File validation applies only
// to official input paths: top-level input_file items and message content
// arrays. Output/tool_result content arrays are not validated for file
// inputs — they use text shapes only (normalizeToolResultOutput supports
// strings and text/input_text/output_text blocks).
func validateResponsesFileItem(item any) string {
	elem, ok := item.(map[string]any)
	if !ok {
		return ""
	}
	itemType, _ := elem["type"].(string)
	// Top-level input_file item or input_file content part.
	if itemType == "input_file" {
		if _, ok := responsesInputFileToFile(elem); !ok {
			return "input_file is missing file_data, file_id, and file_url"
		}
		return ""
	}
	// For message items, recurse into the content array (content parts).
	if itemType == "message" || itemType == "" {
		if content, ok := elem["content"].([]any); ok {
			for _, part := range content {
				if msg := validateResponsesFileItem(part); msg != "" {
					return msg
				}
			}
		}
		return ""
	}
	// All other item types (function_call, tool_call, tool_result,
	// apply_patch_call, shell_call, reasoning, *_call_output, etc.) are not
	// inspected — their arguments/input/content fields are not file inputs.
	return ""
}

// responsesInputFileToFile extracts a Chat Completions file object from a
// Responses input_file content part. It accepts the official common flat
// fields (file_data, file_id, file_url, filename) as well as a nested
// input_file object. Only known fields are selected — unknown/extension
// fields are never copied. file_url (nested or flat) maps to Chat
// file.file_data on a best-effort basis. Empty strings are not valid
// payloads. The official Chat file object has only file_data/file_id/filename.
// Returns (file, true) when a usable payload exists; (nil, false) otherwise.
func responsesInputFileToFile(part map[string]any) (map[string]any, bool) {
	file := map[string]any{}

	// Helper: read a non-empty string from a map by key.
	nonEmptyStr := func(m map[string]any, key string) (string, bool) {
		if v, ok := m[key].(string); ok && v != "" {
			return v, true
		}
		return "", false
	}

	// Nested input_file object: {"type":"input_file","input_file":{...}}.
	// Only select known fields; do not wholesale-copy.
	if nested, ok := part["input_file"].(map[string]any); ok {
		if v, ok := nonEmptyStr(nested, "file_data"); ok {
			file["file_data"] = v
		}
		if v, ok := nonEmptyStr(nested, "file_id"); ok {
			file["file_id"] = v
		}
		// nested file_url maps to file.file_data (best-effort).
		if v, ok := nonEmptyStr(nested, "file_url"); ok {
			file["file_data"] = v
		}
		if v, ok := nonEmptyStr(nested, "filename"); ok {
			file["filename"] = v
		}
	}

	// Flat fields take priority over nested values.
	if v, ok := nonEmptyStr(part, "file_data"); ok {
		file["file_data"] = v
	}
	if v, ok := nonEmptyStr(part, "file_id"); ok {
		file["file_id"] = v
	}
	// Flat file_url maps to file.file_data (best-effort).
	if v, ok := nonEmptyStr(part, "file_url"); ok {
		file["file_data"] = v
	}
	if v, ok := nonEmptyStr(part, "filename"); ok {
		file["filename"] = v
	}

	// A usable payload requires at least one of file_data / file_id.
	if _, hasData := file["file_data"]; !hasData {
		if _, hasID := file["file_id"]; !hasID {
			return nil, false
		}
	}
	return file, true
}

func responsesContentToMessageContent(content any) any {
	if content == nil {
		return nil
	}
	if s, ok := content.(string); ok {
		return s
	}

	parts, ok := content.([]any)
	if !ok {
		b, err := json.Marshal(content)
		if err != nil {
			return nil
		}
		return string(b)
	}

	convertedParts := make([]any, 0, len(parts))
	texts := make([]string, 0, len(parts))
	onlyTextParts := true

	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		convertedPart, ok := convertResponsesContentPart(part)
		if !ok {
			text := extractTextFromContentParts([]any{part})
			if text == "" {
				b, err := json.Marshal(part)
				if err != nil {
					continue
				}
				text = string(b)
			}
			convertedParts = append(convertedParts, map[string]any{
				"type": "text",
				"text": text,
			})
			texts = append(texts, text)
			continue
		}

		if convertedPart["type"] != "text" {
			onlyTextParts = false
		}
		if text, ok := convertedPart["text"].(string); ok && text != "" {
			texts = append(texts, text)
		}
		convertedParts = append(convertedParts, convertedPart)
	}

	if len(convertedParts) == 0 {
		return ""
	}
	if onlyTextParts {
		return strings.Join(texts, "\n")
	}
	return convertedParts
}

func chatContentToResponsesContent(content any) ([]any, string) {
	switch v := content.(type) {
	case nil:
		return nil, ""
	case string:
		if v == "" {
			return nil, ""
		}
		return []any{map[string]any{
			"type":        "output_text",
			"text":        v,
			"annotations": []any{},
			"logprobs":    []any{},
		}}, v
	case []any:
		parts := make([]any, 0, len(v))
		texts := make([]string, 0, len(v))
		for _, rawPart := range v {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			partType, _ := part["type"].(string)
			switch partType {
			case "text", "input_text", "output_text":
				text, _ := part["text"].(string)
				if text == "" {
					continue
				}
				annotations, ok := part["annotations"]
				if !ok {
					annotations = []any{}
				}
				logprobs, ok := part["logprobs"]
				if !ok {
					logprobs = []any{}
				}
				texts = append(texts, text)
				parts = append(parts, map[string]any{
					"type":        "output_text",
					"text":        text,
					"annotations": annotations,
					"logprobs":    logprobs,
				})
			}
		}
		return parts, strings.Join(texts, "\n")
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, ""
		}
		text := string(b)
		return []any{map[string]any{
			"type":        "output_text",
			"text":        text,
			"annotations": []any{},
			"logprobs":    []any{},
		}}, text
	}
}

func responsesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	auth := extractUpstreamAuth(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	cnt := requestCount.Add(1)
	maybeLogBodySummary(r.Context(), "responses request body", body)
	_ = cnt

	var respReq ResponsesAPIRequest
	if err := json.Unmarshal(body, &respReq); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	modelIn := respReq.Model
	respReq.Model = resolveModel(respReq.Model)
	if !validateRequestTemperature(w, respReq.Temperature, "responses", 0, 2) {
		return
	}
	if msg := validateResponsesFileItems(respReq.Input); msg != "" {
		writeProtocolValidation400(w, "responses", "input_file", msg)
		return
	}
	// Note: respReq.Messages (nonstandard compatibility field) is forwarded
	// as-is using Chat content shapes; Responses-style input_file parts are
	// not validated or converted there. Use the official `input` field for
	// input_file support.
	previousState, hasPreviousState := StoredResponseState{}, false
	if respReq.PreviousResponseID != "" {
		previousState, hasPreviousState = loadResponseState(respReq.PreviousResponseID)
		if respReq.Model == "" && previousState.Model != "" {
			respReq.Model = previousState.Model
		}
		if len(respReq.Tools) == 0 && len(previousState.Tools) > 0 {
			respReq.Tools = previousState.Tools
		}
		if respReq.ToolChoice == nil && previousState.ToolChoice != nil {
			respReq.ToolChoice = previousState.ToolChoice
		}
	}
	if respReq.Model == "" {
		modelIDs := getModelIDs()
		if len(modelIDs) > 0 {
			respReq.Model = modelIDs[0]
		} else {
			respReq.Model = "deepseek-v4-flash-free"
		}
	}
	respReq.Model = mapPublicToFreeModel(auth, respReq.Model)

	// 多模态路由

	messages := respReq.Messages
	if len(messages) == 0 {
		if hasPreviousState && len(previousState.Output) > 0 {
			messages = append(messages, responsesInputToMessages(previousState.Output, "")...)
		}
		messages = append(messages, responsesInputToMessages(respReq.Input, respReq.Instructions)...)
	} else if respReq.Instructions != "" {
		messages = append([]Message{{Role: "system", Content: respReq.Instructions}}, messages...)
	}

	chatReq := OpenAIRequest{
		Model:    respReq.Model,
		Messages: messages,
		Stream:   respReq.Stream,
	}
	if respReq.Stream {
		chatReq.ExtraBody = map[string]any{
			"stream_options": map[string]any{"include_usage": true},
		}
	}
	if respReq.Temperature != nil {
		chatReq.Temperature = respReq.Temperature
	}
	if respReq.MaxTokens != nil {
		chatReq.MaxTokens = respReq.MaxTokens
	}
	if respReq.TopP != nil {
		chatReq.TopP = respReq.TopP
	}
	if len(respReq.Tools) > 0 {
		chatReq.Tools = convertResponsesTools(respReq.Tools)
	}
	if respReq.ToolChoice != nil {
		chatReq.ToolChoice = convertResponsesToolChoice(respReq.ToolChoice)
	}
	if respReq.ParallelToolCalls != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["parallel_tool_calls"] = *respReq.ParallelToolCalls
	}
	if respReq.Stop != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["stop"] = respReq.Stop
	}
	if respReq.FrequencyPenalty != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["frequency_penalty"] = *respReq.FrequencyPenalty
	}
	if respReq.PresencePenalty != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["presence_penalty"] = *respReq.PresencePenalty
	}
	if respReq.User != "" {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["user"] = respReq.User
	}
	if respReq.Text != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["response_format"] = respReq.Text
	}
	if respReq.Truncation != "" {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["truncation"] = respReq.Truncation
	}
	if respReq.ServiceTier != "" {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["service_tier"] = respReq.ServiceTier
	}
	if respReq.PromptCacheKey != "" {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["prompt_cache_key"] = respReq.PromptCacheKey
	}
	if respReq.SafetyIdentifier != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["safety_identifier"] = respReq.SafetyIdentifier
	}
	if respReq.TopLogprobs != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["top_logprobs"] = *respReq.TopLogprobs
	}
	if respReq.StreamOptions != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		streamOptions, ok := respReq.StreamOptions.(map[string]any)
		if !ok {
			streamOptions = map[string]any{}
		}
		if _, exists := streamOptions["include_usage"]; !exists && respReq.Stream {
			streamOptions["include_usage"] = true
		}
		chatReq.ExtraBody["stream_options"] = streamOptions
	}
	// 将 Responses API reasoning.effort 映射到 Chat Completions
	if !getForceDisableThinking() && respReq.Reasoning.Effort != "" {
		if respReq.Reasoning.Effort != "none" {
			chatReq.ReasoningEffort = respReq.Reasoning.Effort
		}
	}

	wantReasoning := !getForceDisableThinking()
	chatReq.Messages = fixToolCallGaps(chatReq.Messages)
	keepReasoning := wantsReasoning(&chatReq)
	chatReq.Messages = ensureReasoningContent(chatReq.Messages, keepReasoning)

	effortIn := chatReq.ReasoningEffort
	if effortIn == "" {
		effortIn = respReq.Reasoning.Effort
	}
	upstreamSurface := "zen"
	if auth.shouldUseGoEndpoint(chatReq.Model) {
		upstreamSurface = "go"
	}
	logRequestPlan(r.Context(), map[string]any{
		"protocol":             "responses",
		"model_in":             modelIn,
		"model_resolved":       chatReq.Model,
		"auth_mode":            authModeString(auth.Mode),
		"auth_source":          auth.Source,
		"has_key":              auth.Token != "",
		"upstream_surface":     upstreamSurface,
		"stream":               respReq.Stream,
		"keep_reasoning":       keepReasoning,
		"thinking":             thinkingState(nil),
		"reasoning_effort_in":  effortIn,
		"reasoning_effort_out": mappedReasoningEffort(effortIn),
		"tools_count":          len(respReq.Tools),
		"messages_count":       len(chatReq.Messages),
		"max_tokens":           chatReq.MaxTokens,
		"max_tokens_cap":       getMaxTokensCapForModel(chatReq.Model),
	})

	upstreamBody := buildUpstreamBody(&chatReq)

	if respReq.Stream {
		upResp, status, _, err := callOpenCodeAPIStream(r.Context(), upstreamBody, chatReq.Model, auth)
		if err != nil || status < 200 || status >= 300 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if upResp != nil {
				errBody, _ := io.ReadAll(upResp)
				if len(errBody) > 0 {
					w.Write(errBody)
					return
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error"}})
			return
		}
		defer upResp.Close()

		resp := &http.Response{
			StatusCode: status,
			Body:       upResp,
			Header:     make(http.Header),
		}
		responsesStreamHandler(w, r, resp, chatReq.Model, chatReq.Model, wantReasoning, respReq.Tools, respReq.ToolChoice, respReq)
		return
	}

	respBody, status, _, err := callOpenCodeAPI(r.Context(), upstreamBody, chatReq.Model, auth)
	if err != nil || status < 200 || status >= 300 {
		if err != nil {
			writeUpstreamError(w, status, err, "responses")
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if len(respBody) > 0 {
				w.Write(respBody)
			} else {
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error"}})
			}
		}
		return
	}

	responsesBody := convertChatToResponses(respBody, chatReq.Model, wantReasoning, respReq.Tools, respReq.ToolChoice, respReq.Include)
	var responseMap map[string]any
	if json.Unmarshal(responsesBody, &responseMap) == nil {
		applyResponsesRequestEcho(responseMap, respReq)
		if enriched, marshalErr := json.Marshal(responseMap); marshalErr == nil {
			responsesBody = enriched
		}
		storeResponseState(responseMap, respReq)
	}

	result := summarizeChatResult(respBody)
	logRequestResult(r.Context(), result)

	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			pt, _ := u["prompt_tokens"].(float64)
			ct, _ := u["completion_tokens"].(float64)
			tt, _ := u["total_tokens"].(float64)
			if tt > 0 {
				recordTokenUsage(chatReq.Model, int64(pt), int64(ct), int64(tt))
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	maybeLogBodySummary(r.Context(), "responses response body", responsesBody)
	w.Write(responsesBody)
}

// ======================== Responses Stream Handler ========================

func responsesStreamHandler(w http.ResponseWriter, r *http.Request, resp *http.Response, model string, _ string, wantReasoning bool, tools []ResponsesTool, toolChoice any, originalReq ResponsesAPIRequest) {
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(resp.Body)
	stats := &streamResultStats{start: time.Now()}

	responseID := "resp_" + time.Now().Format("20060102150405") + "_" + randomString(8)
	reasoningID := "rs_" + responseID
	msgID := "msg_" + responseID + "_0"
	createdAt := time.Now().Unix()
	seq := 0

	reasoningStarted := false
	reasoningDone := false
	messageStarted := false
	messageDone := false
	fullReasoning := ""
	fullText := ""
	fullRefusal := ""
	refusalStarted := false
	totalUsage := map[string]any{}
	createdSent := false
	terminalStatus := "completed"
	terminalEvent := "response.completed"
	itemStatus := "completed"
	finished := false
	toolCalls := map[int]map[string]any{}
	toolOrder := []int{}
	toolKinds := responsesToolKindMap(tools)
	indexAllocator := outputIndexAllocator{}
	reasoningOutputIndex := -1
	messageIndex := -1

	// --- Reader goroutine -> channel so the main loop can select on
	// read/context without blocking, and context cancellation unblocks the
	// reader via Close. ---
	type readResult struct {
		line string
		err  error
	}
	readCh := make(chan readResult)
	readerDone := make(chan struct{})
	readerExited := make(chan struct{})

	go func() {
		defer close(readerExited)
		for {
			line, err := reader.ReadString('\n')
			select {
			case readCh <- readResult{line: line, err: err}:
			case <-readerDone:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	defer func() {
		stats.textChars = len(fullText)
		stats.reasoningChars = len(fullReasoning)
		stats.toolCallCount = len(toolOrder)
		stats.log(ctx, "responses")
	}()
	// Reader cleanup: signal goroutine, unblock any pending read, wait for exit.
	defer func() {
		close(readerDone)
		resp.Body.Close()
		<-readerExited
	}()

	messageOutputIndex := func() int {
		if messageIndex < 0 {
			messageIndex = indexAllocator.Allocate()
		}
		return messageIndex
	}

	reasoningItem := func(status string) map[string]any {
		item := map[string]any{
			"id":      reasoningID,
			"type":    "reasoning",
			"summary": []any{},
		}
		if status != "" {
			item["status"] = status
		}
		if status == "completed" && includeHas(originalReq.Include, "reasoning.encrypted_content") {
			item["encrypted_content"] = ""
		}
		if fullReasoning != "" {
			item["summary"] = []any{map[string]any{"type": "summary_text", "text": fullReasoning}}
		}
		return item
	}

	messageItem := func(status string) map[string]any {
		content := []any{}
		if fullRefusal != "" {
			content = append(content, map[string]any{
				"type":    "refusal",
				"refusal": fullRefusal,
			})
		}
		content = append(content, map[string]any{
			"type":        "output_text",
			"annotations": []any{},
			"logprobs":    []any{},
			"text":        fullText,
		})
		return map[string]any{
			"id":      msgID,
			"type":    "message",
			"status":  status,
			"content": content,
			"role":    "assistant",
		}
	}

	emitReasoningDone := func() {
		if !reasoningStarted || reasoningDone {
			return
		}
		seq++
		emitSSEEvent(w, flusher, "response.reasoning_summary_text.done", map[string]any{
			"type":            "response.reasoning_summary_text.done",
			"sequence_number": seq,
			"item_id":         reasoningID,
			"output_index":    reasoningOutputIndex,
			"summary_index":   0,
			"text":            fullReasoning,
		})
		seq++
		emitSSEEvent(w, flusher, "response.reasoning_summary_part.done", map[string]any{
			"type":            "response.reasoning_summary_part.done",
			"sequence_number": seq,
			"item_id":         reasoningID,
			"output_index":    reasoningOutputIndex,
			"summary_index":   0,
			"part":            map[string]any{"type": "summary_text", "text": fullReasoning},
		})
		seq++
		emitSSEEvent(w, flusher, "response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
			"sequence_number": seq,
			"output_index":    reasoningOutputIndex,
			"item":            reasoningItem(itemStatus),
		})
		reasoningDone = true
	}

	emitMessageDone := func() {
		if !messageStarted || messageDone {
			return
		}
		idx := messageOutputIndex()
		seq++
		emitSSEEvent(w, flusher, "response.output_text.done", map[string]any{
			"type":            "response.output_text.done",
			"sequence_number": seq,
			"item_id":         msgID,
			"output_index":    idx,
			"content_index":   0,
			"text":            fullText,
			"logprobs":        []any{},
		})
		seq++
		emitSSEEvent(w, flusher, "response.content_part.done", map[string]any{
			"type":            "response.content_part.done",
			"sequence_number": seq,
			"item_id":         msgID,
			"output_index":    idx,
			"content_index":   0,
			"part":            map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": fullText},
		})
		seq++
		emitSSEEvent(w, flusher, "response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
			"sequence_number": seq,
			"output_index":    idx,
			"item":            messageItem(itemStatus),
		})
		messageDone = true
	}

	emitRefusalDone := func() {
		if !refusalStarted {
			return
		}
		idx := messageOutputIndex()
		seq++
		emitSSEEvent(w, flusher, "response.refusal.done", map[string]any{
			"type":            "response.refusal.done",
			"sequence_number": seq,
			"item_id":         msgID,
			"output_index":    idx,
			"content_index":   0,
			"refusal":         fullRefusal,
		})
	}

	emitToolCallDone := func(idx int, call map[string]any) {
		if done, _ := call["done"].(bool); done {
			return
		}
		call["done"] = true
		itemID, _ := call["item_id"].(string)
		callID, _ := call["call_id"].(string)
		name, _ := call["name"].(string)
		args, _ := call["arguments"].(string)
		seq++
		emitSSEEvent(w, flusher, "response.function_call_arguments.done", map[string]any{
			"type":            "response.function_call_arguments.done",
			"sequence_number": seq,
			"item_id":         itemID,
			"output_index":    idx,
			"name":            name,
			"arguments":       args,
		})
		seq++
		itemType, _ := call["item_type"].(string)
		if itemType == "" {
			itemType = "function_call"
		}
		item := buildResponseToolCallItem(ToolCall{ID: callID, Function: FunctionCall{Name: name, Arguments: args}}, itemType)
		item["status"] = itemStatus
		emitSSEEvent(w, flusher, "response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
			"sequence_number": seq,
			"output_index":    idx,
			"item":            item,
		})
	}

	ensureCreated := func(chunk map[string]any) {
		if createdSent {
			return
		}
		if chunk != nil {
			if id, ok := chunk["id"].(string); ok && id != "" {
				responseID = normalizeResponsesID(id)
				reasoningID = "rs_" + responseID + "_0"
				msgID = "msg_" + responseID + "_0"
			}
			if created, ok := chunk["created"].(float64); ok {
				createdAt = int64(created)
			}
		}
		seq++
		emitSSEEvent(w, flusher, "response.created", map[string]any{
			"type":            "response.created",
			"sequence_number": seq,
			"response":        map[string]any{"id": responseID, "object": "response", "created_at": createdAt, "status": "in_progress", "background": false, "error": nil, "output": []any{}},
		})
		seq++
		emitSSEEvent(w, flusher, "response.in_progress", map[string]any{
			"type":            "response.in_progress",
			"sequence_number": seq,
			"response":        map[string]any{"id": responseID, "object": "response", "created_at": createdAt, "status": "in_progress"},
		})
		createdSent = true
	}

	emitResponseFailed := func(msg string) {
		ensureCreated(nil)
		failedResponse := map[string]any{
			"id":         responseID,
			"object":     "response",
			"created_at": createdAt,
			"status":     "failed",
			"background": false,
			"error": map[string]any{
				"code":    "server_error",
				"message": msg,
			},
			"incomplete_details": nil,
			"model":              model,
			"output":             []any{},
		}
		applyResponsesRequestEcho(failedResponse, originalReq)
		seq++
		emitSSEEvent(w, flusher, "response.failed", map[string]any{
			"type":            "response.failed",
			"sequence_number": seq,
			"response":        failedResponse,
		})
		if flusher != nil {
			flusher.Flush()
		}
	}

loop:
	for {
		select {
		case <-ctx.Done():
			// Client cancelled: quiet exit, no error writes.
			return
		case result := <-readCh:
			// bufio.ReadString may return both a non-empty line and an error
			// (e.g. the last line without a trailing newline + io.EOF). Process
			// the line first, then handle the accompanying error via pendingErr.
			pendingErr := result.err

			line := result.line
			trimmed := strings.TrimSpace(line)
			if trimmed == "data: [DONE]" || trimmed == "[DONE]" {
				stats.doneSeen = true
				if !finished {
					emitResponseFailed("stream ended with [DONE] but no finish_reason")
					return
				}
				break loop
			}
			if strings.HasPrefix(line, "data: ") {
				payload := line[6:]
				if strings.TrimSpace(payload) != "" {
					var chunk map[string]any
					if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
						emitResponseFailed("stream received malformed JSON data")
						return
					} else {
						// In-band error from upstream.
						if errVal, ok := chunk["error"]; ok && errVal != nil {
							errMsg := "upstream stream error"
							if errMap, ok := errVal.(map[string]any); ok {
								if m, ok := errMap["message"].(string); ok && m != "" {
									errMsg = m
								}
							} else if errStr, ok := errVal.(string); ok && errStr != "" {
								errMsg = errStr
							}
							emitResponseFailed(errMsg)
							return
						} else {
							stats.noteChunk()
							ensureCreated(chunk)
							choices, ok := chunk["choices"].([]any)
							if !ok || len(choices) == 0 {
								if usage, ok := chunk["usage"].(map[string]any); ok {
									totalUsage = usage
								}
							} else {
								choice, _ := choices[0].(map[string]any)
								delta, _ := choice["delta"].(map[string]any)
								finishReason, _ := choice["finish_reason"].(string)
								if finishReason != "" {
									stats.finishReason = finishReason
									stats.sawFinish = true
								}

								if !finished {
									if rc, ok := delta["reasoning_content"]; ok && wantReasoning {
										rcStr, _ := rc.(string)
										if rcStr != "" {
											if !reasoningStarted {
												reasoningOutputIndex = indexAllocator.Allocate()
												seq++
												emitSSEEvent(w, flusher, "response.output_item.added", map[string]any{
													"type":            "response.output_item.added",
													"sequence_number": seq,
													"output_index":    reasoningOutputIndex,
													"item":            reasoningItem("in_progress"),
												})
												seq++
												emitSSEEvent(w, flusher, "response.reasoning_summary_part.added", map[string]any{
													"type":            "response.reasoning_summary_part.added",
													"sequence_number": seq,
													"item_id":         reasoningID,
													"output_index":    reasoningOutputIndex,
													"summary_index":   0,
													"part":            map[string]any{"type": "summary_text", "text": ""},
												})
												reasoningStarted = true
											}
											fullReasoning += rcStr
											seq++
											emitSSEEvent(w, flusher, "response.reasoning_summary_text.delta", map[string]any{
												"type":            "response.reasoning_summary_text.delta",
												"sequence_number": seq,
												"item_id":         reasoningID,
												"output_index":    reasoningOutputIndex,
												"summary_index":   0,
												"delta":           rcStr,
											})
										}
									}

									contentStr := ""
									if c, ok := delta["content"]; ok && c != nil {
										contentStr, _ = c.(string)
									}
									// #37635: when thinking is not kept, promote misplaced reasoning to visible text.
									if contentStr == "" && !wantReasoning {
										if rc, ok := delta["reasoning_content"].(string); ok {
											if rc != "" {
												stats.promotedReasoning = true
											}
											contentStr = rc
										}
									}
									if contentStr != "" {
										// The terminal finish reason determines the item's final status. Keep the
										// reasoning item open until that reason is known so a truncation cannot
										// first announce it as completed.
										if !messageStarted {
											idx := messageOutputIndex()
											seq++
											emitSSEEvent(w, flusher, "response.output_item.added", map[string]any{
												"type":            "response.output_item.added",
												"sequence_number": seq,
												"output_index":    idx,
												"item":            map[string]any{"id": msgID, "type": "message", "status": "in_progress", "content": []any{}, "role": "assistant"},
											})
											seq++
											emitSSEEvent(w, flusher, "response.content_part.added", map[string]any{
												"type":            "response.content_part.added",
												"sequence_number": seq,
												"item_id":         msgID,
												"output_index":    idx,
												"content_index":   0,
												"part":            map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": ""},
											})
											messageStarted = true
										}
										fullText += contentStr
										seq++
										emitSSEEvent(w, flusher, "response.output_text.delta", map[string]any{
											"type":            "response.output_text.delta",
											"sequence_number": seq,
											"item_id":         msgID,
											"output_index":    messageOutputIndex(),
											"content_index":   0,
											"delta":           contentStr,
											"logprobs":        []any{},
										})
									}

									if refusalStr, ok := delta["refusal"].(string); ok && refusalStr != "" {
										if !refusalStarted {
											refusalStarted = true
										}
										fullRefusal += refusalStr
										seq++
										emitSSEEvent(w, flusher, "response.refusal.delta", map[string]any{
											"type":            "response.refusal.delta",
											"sequence_number": seq,
											"item_id":         msgID,
											"output_index":    messageOutputIndex(),
											"content_index":   0,
											"delta":           refusalStr,
										})
									}

									rawToolCalls, _ := delta["tool_calls"].([]any)
									for _, rawToolCall := range rawToolCalls {
										tc, ok := rawToolCall.(map[string]any)
										if !ok {
											continue
										}
										idxFloat, _ := tc["index"].(float64)
										upstreamIndex := int(idxFloat)
										call, exists := toolCalls[upstreamIndex]
										if !exists {
											outputIndex := indexAllocator.Allocate()
											callID, _ := tc["id"].(string)
											if callID == "" {
												callID = "call_" + randomString(12)
											}
											fn, _ := tc["function"].(map[string]any)
											name, _ := fn["name"].(string)
											itemType := toolCallOutputType(name, toolKinds)
											call = map[string]any{
												"output_index": outputIndex,
												"item_id":      "fc_" + callID,
												"call_id":      callID,
												"name":         name,
												"arguments":    "",
												"done":         false,
												"item_type":    itemType,
											}
											toolCalls[upstreamIndex] = call
											toolOrder = append(toolOrder, upstreamIndex)
											seq++
											emitSSEEvent(w, flusher, "response.output_item.added", map[string]any{
												"type":            "response.output_item.added",
												"sequence_number": seq,
												"output_index":    outputIndex,
												"item": map[string]any{
													"id":        call["item_id"],
													"type":      itemType,
													"status":    "in_progress",
													"arguments": "",
													"call_id":   callID,
													"name":      name,
												},
											})
										}
										fn, _ := tc["function"].(map[string]any)
										if name, _ := fn["name"].(string); name != "" {
											call["name"] = name
											if call["item_type"] == "function_call" {
												call["item_type"] = toolCallOutputType(name, toolKinds)
											}
										}
										if argDelta, _ := fn["arguments"].(string); argDelta != "" {
											call["arguments"] = call["arguments"].(string) + argDelta
											seq++
											emitSSEEvent(w, flusher, "response.function_call_arguments.delta", map[string]any{
												"type":            "response.function_call_arguments.delta",
												"sequence_number": seq,
												"item_id":         call["item_id"],
												"output_index":    call["output_index"],
												"delta":           argDelta,
											})
										}
									}

									if usage, ok := chunk["usage"].(map[string]any); ok {
										totalUsage = usage
									}
									if finishReason == "stop" || finishReason == "length" || finishReason == "tool_calls" || finishReason == "function_call" || finishReason == "content_filter" {
										finished = true
										if finishReason == "length" {
											terminalStatus = "incomplete"
											terminalEvent = "response.incomplete"
											itemStatus = "incomplete"
										}
										// Do not emit done events yet: a trailing error
										// must still produce response.failed without any
										// status=completed item.done. Done events are
										// emitted only after the loop exits cleanly.
									}
								} else {
									// After finish_reason, only look for usage-only trailing chunks.
									if usage, ok := chunk["usage"].(map[string]any); ok {
										totalUsage = usage
									}
								}
							}
						}
					}
				}
			}

			// Now handle a pending error from the read.
			if pendingErr != nil {
				if pendingErr == io.EOF {
					if !finished {
						emitResponseFailed("stream ended without finish_reason")
						return
					}
					break loop
				}
				reqLogger(ctx).Error("stream read error", "error", pendingErr)
				emitResponseFailed("stream read error")
				return
			}
		}
	}

	// Reached only when finished is true.
	emitReasoningDone()
	emitRefusalDone()
	if !messageStarted && len(toolCalls) == 0 {
		idx := messageOutputIndex()
		seq++
		emitSSEEvent(w, flusher, "response.output_item.added", map[string]any{
			"type":            "response.output_item.added",
			"sequence_number": seq,
			"output_index":    idx,
			"item":            map[string]any{"id": msgID, "type": "message", "status": "in_progress", "content": []any{}, "role": "assistant"},
		})
		seq++
		emitSSEEvent(w, flusher, "response.content_part.added", map[string]any{
			"type":            "response.content_part.added",
			"sequence_number": seq,
			"item_id":         msgID,
			"output_index":    idx,
			"content_index":   0,
			"part":            map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": ""},
		})
		messageStarted = true
	}
	emitMessageDone()
	for _, idx := range toolOrder {
		emitToolCallDone(toolCalls[idx]["output_index"].(int), toolCalls[idx])
	}

	output := make([]any, indexAllocator.Len())
	if reasoningStarted {
		output[reasoningOutputIndex] = reasoningItem(itemStatus)
	}
	if messageStarted {
		output[messageIndex] = messageItem(itemStatus)
	}
	for _, idx := range toolOrder {
		call := toolCalls[idx]
		itemType, _ := call["item_type"].(string)
		if itemType == "" {
			itemType = "function_call"
		}
		item := buildResponseToolCallItem(ToolCall{
			ID: call["call_id"].(string),
			Function: FunctionCall{
				Name:      call["name"].(string),
				Arguments: call["arguments"].(string),
			},
		}, itemType)
		item["status"] = itemStatus
		output[call["output_index"].(int)] = item
	}

	completedResponse := map[string]any{
		"id":                 responseID,
		"object":             "response",
		"created_at":         createdAt,
		"status":             terminalStatus,
		"background":         false,
		"error":              nil,
		"incomplete_details": nil,
		"model":              model,
		"output":             output,
	}
	if terminalStatus == "incomplete" {
		completedResponse["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	applyResponsesRequestEcho(completedResponse, originalReq)
	if len(tools) > 0 {
		completedResponse["tools"] = tools
	}
	if toolChoice != nil {
		completedResponse["tool_choice"] = toolChoice
	}

	if len(totalUsage) > 0 {
		usage := map[string]any{}
		if v, ok := totalUsage["prompt_tokens"]; ok {
			usage["input_tokens"] = v
		}
		if v, ok := totalUsage["prompt_tokens_details"]; ok {
			usage["input_tokens_details"] = v
		} else {
			usage["input_tokens_details"] = map[string]any{"cached_tokens": 0}
		}
		if v, ok := totalUsage["completion_tokens"]; ok {
			usage["output_tokens"] = v
		}
		if v, ok := totalUsage["completion_tokens_details"]; ok {
			usage["output_tokens_details"] = v
		}
		if v, ok := totalUsage["total_tokens"]; ok {
			usage["total_tokens"] = v
		}
		if v, ok := totalUsage["input_tokens"]; ok && usage["input_tokens"] == nil {
			usage["input_tokens"] = v
		}
		if v, ok := totalUsage["output_tokens"]; ok && usage["output_tokens"] == nil {
			usage["output_tokens"] = v
		}
		completedResponse["usage"] = usage
	}

	if totalUsage != nil {
		pt, _ := totalUsage["prompt_tokens"].(float64)
		ct, _ := totalUsage["completion_tokens"].(float64)
		tt, _ := totalUsage["total_tokens"].(float64)
		if tt > 0 {
			recordTokenUsage(model, int64(pt), int64(ct), int64(tt))
		}
	}

	seq++
	emitSSEEvent(w, flusher, terminalEvent, map[string]any{
		"type":            terminalEvent,
		"sequence_number": seq,
		"response":        completedResponse,
	})

	if flusher != nil {
		flusher.Flush()
	}
	storeResponseState(completedResponse, originalReq)
}

func convertChatToResponses(chatBody []byte, model string, wantReasoning bool, tools []ResponsesTool, toolChoice any, include []string) []byte {
	var chat struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          any        `json:"content"`
				Refusal          string     `json:"refusal"`
				ReasoningContent string     `json:"reasoning_content"`
				ToolCalls        []ToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		slog.Warn("convertChatToResponses unmarshal failed", "error", err)
	}

	reasoning := ""
	finishReason := ""
	var toolCalls []ToolCall
	messageContent := []any(nil)
	toolKinds := responsesToolKindMap(tools)
	if len(chat.Choices) > 0 {
		messageContent, _ = chatContentToResponsesContent(chat.Choices[0].Message.Content)
		if refusal := chat.Choices[0].Message.Refusal; refusal != "" {
			messageContent = []any{map[string]any{"type": "refusal", "refusal": refusal}}
		}
		rc := chat.Choices[0].Message.ReasoningContent
		if wantReasoning {
			reasoning = rc
		}
		toolCalls = chat.Choices[0].Message.ToolCalls
		finishReason = chat.Choices[0].FinishReason
		if len(messageContent) == 0 && rc != "" && len(toolCalls) == 0 {
			messageContent, _ = chatContentToResponsesContent(rc)
		}
	}

	outcome := responsesOutcome(finishReason)
	status := outcome.Status
	normalizedID := normalizeResponsesID(chat.ID)
	responses := map[string]any{
		"id":                 normalizedID,
		"object":             "response",
		"status":             status,
		"background":         false,
		"error":              nil,
		"incomplete_details": outcome.IncompleteDetails,
		"model":              model,
		"created_at":         chat.Created,
	}
	if len(tools) > 0 {
		responses["tools"] = tools
	}
	if toolChoice != nil {
		responses["tool_choice"] = toolChoice
	}
	outputID := "msg_" + normalizedID + "_0"
	output := []any{}
	if reasoning != "" {
		reasoningItem := map[string]any{
			"id":      "rs_" + normalizedID,
			"type":    "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": reasoning}},
		}
		if includeHas(include, "reasoning.encrypted_content") {
			reasoningItem["encrypted_content"] = ""
		}
		output = append(output, reasoningItem)
	}
	if len(messageContent) > 0 {
		output = append(output, map[string]any{
			"id":      outputID,
			"type":    "message",
			"status":  status,
			"role":    "assistant",
			"content": messageContent,
		})
	}
	for _, tc := range toolCalls {
		item := buildResponseToolCallItem(tc, toolCallOutputType(tc.Function.Name, toolKinds))
		item["status"] = status
		output = append(output, item)
	}
	responses["output"] = output
	if chat.Usage != nil {
		usage := map[string]any{}
		if v, ok := chat.Usage["prompt_tokens"]; ok {
			usage["input_tokens"] = v
		}
		if v, ok := chat.Usage["prompt_tokens_details"]; ok {
			usage["input_tokens_details"] = v
		} else {
			usage["input_tokens_details"] = map[string]any{"cached_tokens": 0}
		}
		if v, ok := chat.Usage["completion_tokens"]; ok {
			usage["output_tokens"] = v
		}
		if v, ok := chat.Usage["completion_tokens_details"]; ok {
			usage["output_tokens_details"] = v
		}
		if v, ok := chat.Usage["total_tokens"]; ok {
			usage["total_tokens"] = v
		}
		if v, ok := chat.Usage["input_tokens"]; ok && usage["input_tokens"] == nil {
			usage["input_tokens"] = v
		}
		if v, ok := chat.Usage["output_tokens"]; ok && usage["output_tokens"] == nil {
			usage["output_tokens"] = v
		}
		responses["usage"] = usage
	}

	result, _ := json.Marshal(responses)
	return result
}

func emitSSEEvent(w http.ResponseWriter, flusher http.Flusher, event string, data map[string]any) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		slog.Error("marshal SSE event failed", "error", err)
		return
	}
	w.Write([]byte("event: " + event + "\n"))
	w.Write([]byte("data: " + string(jsonData) + "\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

// ======================== Admin 管理页面 ========================

func reloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	refreshOCSession()
	fetched, err := fetchModels()
	if err == nil && len(fetched) > 0 {
		modelMu.Lock()
		modelsCache = fetched
		modelsLoaded = true
		modelMu.Unlock()
		slog.Info("free models refreshed", "count", len(fetched))
	}
	goFetched, goErr := fetchGoModels()
	if goErr == nil && len(goFetched) > 0 {
		modelMu.Lock()
		goModelsCache = goFetched
		modelMu.Unlock()
		slog.Info("go catalog refreshed", "count", len(goFetched))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"session": ocSessionID,
		"free":    len(modelsCache),
		"go":      len(goModelsCache),
	})

}
func adminConfigHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		configMu.RLock()
		cfg := AppConfig{ModelAlias: modelAlias, ReasoningEffortMap: reasoningEffortMap, ForceDisableThinking: forceDisableThinking, MaxTokensCap: maxTokensCap, MaxTokensCapPerModel: maxTokensCapPerModel}
		configMu.RUnlock()
		socks5Mu.RLock()
		cfg.Socks5Proxies = socks5Proxies
		cfg.ActiveSocks5 = activeSocks5
		cfg.Socks5PaidDirect = socks5PaidDirect
		socks5Mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model_alias":              cfg.ModelAlias,
			"reasoning_effort_map":     cfg.ReasoningEffortMap,
			"force_disable_thinking":   cfg.ForceDisableThinking,
			"max_tokens_cap":           cfg.MaxTokensCap,
			"max_tokens_cap_per_model": cfg.MaxTokensCapPerModel,
			"socks5_proxies":           cfg.Socks5Proxies,
			"active_socks5":            cfg.ActiveSocks5,
			"socks5_paid_direct":       cfg.Socks5PaidDirect,
			"log_level":                getLogLevelString(),
			"log_bodies":               getLogBodies(),
		})
	case http.MethodPost:
		var payload struct {
			AppConfig
			LogLevel  *string `json:"log_level,omitempty"`
			LogBodies *bool   `json:"log_bodies,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, `{"error":"Invalid JSON"}`, http.StatusBadRequest)
			return
		}
		if err := saveConfig(configPath, payload.AppConfig); err != nil {
			http.Error(w, `{"error":"Failed to save config"}`, http.StatusInternalServerError)
			return
		}
		applyConfig(payload.AppConfig)
		if payload.LogLevel != nil {
			setLogLevelString(*payload.LogLevel)
		}
		if payload.LogBodies != nil {
			setLogBodies(*payload.LogBodies)
		}
		if debugMode {
			slog.Info("config updated",
				"aliases", len(payload.ModelAlias),
				"effort_map", len(payload.ReasoningEffortMap),
				"force_disable", payload.ForceDisableThinking,
				"max_tokens_cap", payload.MaxTokensCap,
				"max_tokens_cap_per_model", len(payload.MaxTokensCapPerModel),
				"log_level", getLogLevelString(),
				"log_bodies", getLogBodies(),
			)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func adminStatsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		tokenStatsMu.Lock()
		data, err := json.Marshal(tokenStats)
		tokenStatsMu.Unlock()
		if err != nil {
			http.Error(w, `{"error":"marshal error"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	case http.MethodDelete:
		tokenStatsMu.Lock()
		tokenStats = &TokenStatsData{Models: map[string]*ModelStats{}}
		tokenStatsMu.Unlock()
		saveTokenStats()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func adminPageHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(adminHTML))
}

func renderLoginPage(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(adminLoginHTML))
	if msg != "" {
		w.Write([]byte("<script>document.addEventListener('DOMContentLoaded',function(){var m=document.getElementById('login-msg');if(m){m.textContent='" + msg + "';m.style.display='block'}})</script>"))
	}
}

const adminLoginHTML = `<!DOCTYPE html>
<html lang="zh" data-theme="light">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>登录 — OPENCODE TO API</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=Noto+Sans+SC:wght@300;400;500;600;700&family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet">
<style>
:root{--bg:#f4f6fa;--surface:#fff;--border:#e2e6ed;--text:#1a1d26;--text-sec:#6a7180;--accent:#6c8aff;--accent-hover:#5a78f0;--radius:12px;--radius-sm:8px;--font:'Noto Sans SC',system-ui,-apple-system,sans-serif;--mono:'JetBrains Mono',Consolas,monospace}
[data-theme="dark"]{--bg:#0c0e14;--surface:#14161e;--border:#252835;--text:#e8eaf0;--text-sec:#8b90a5;--accent:#6c8aff;--accent-hover:#5a78f0}
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:var(--font);background:var(--bg);color:var(--text);font-size:14px;line-height:1.6;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:20px}
body::before{content:'';position:fixed;top:-50%;left:-50%;width:200%;height:200%;background:radial-gradient(ellipse at 30% 20%,rgba(108,138,255,.04) 0%,transparent 50%),radial-gradient(ellipse at 70% 80%,rgba(61,214,140,.03) 0%,transparent 50%);pointer-events:none;z-index:0}
.container{max-width:400px;width:100%;position:relative;z-index:1}
.card{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);padding:36px 32px 32px}
.logo{display:flex;align-items:center;gap:10px;margin-bottom:6px}
.logo-mark{width:36px;height:36px;background:linear-gradient(135deg,var(--accent),#8b6cff);border-radius:10px;display:flex;align-items:center;justify-content:center;font-size:20px;color:#fff;flex-shrink:0}
.logo-text{font-size:20px;font-weight:700;letter-spacing:-.5px;background:linear-gradient(135deg,var(--text),var(--text-sec));-webkit-background-clip:text;-webkit-text-fill-color:transparent}
.logo-sub{font-size:12px;color:var(--text-sec);margin-top:2px}
.subtitle{font-size:13px;color:var(--text-sec);margin-bottom:28px;margin-top:4px}
.field{margin-bottom:16px}
.field label{display:block;font-size:12px;font-weight:500;color:var(--text-sec);margin-bottom:6px;letter-spacing:.3px}
.field input{width:100%;padding:10px 14px;border:1px solid var(--border);border-radius:var(--radius-sm);font-size:14px;font-family:var(--mono);background:var(--surface);color:var(--text);transition:border-color .15s,box-shadow .15s}
.field input:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 3px rgba(108,138,255,.1)}
.msg{display:none;background:rgba(240,96,96,.1);color:#d64545;padding:10px 14px;border-radius:var(--radius-sm);margin-bottom:16px;font-size:13px;text-align:center;border:1px solid rgba(240,96,96,.2)}
[data-theme="dark"] .msg{color:#f06060}
.btn{width:100%;padding:10px;border:none;border-radius:var(--radius-sm);font-size:14px;font-weight:600;cursor:pointer;font-family:var(--font);background:var(--accent);color:#fff;transition:background .15s}
.btn:hover{background:var(--accent-hover)}
.theme-bar{display:flex;justify-content:space-between;align-items:center;margin-bottom:24px}
.theme-toggle{background:transparent;border:1px solid var(--border);border-radius:var(--radius-sm);padding:6px 12px;cursor:pointer;font-size:13px;color:var(--text-sec);font-family:var(--font);transition:all .15s}
.theme-toggle:hover{border-color:var(--accent);color:var(--accent)}
@media(max-width:500px){.card{padding:24px 20px}}
</style>
</head>
<body>
<div class="container">
<div class="card">
<div class="theme-bar">
<div class="logo">
<div class="logo-mark">⌨</div>
<div>
<div class="logo-text">OPENCODE TO API</div>
<div class="logo-sub">管理面板</div>
</div>
</div>
<button class="theme-toggle" onclick="toggleTheme()">☀</button>
</div>
<div class="subtitle">请输入管理密码以继续</div>
<div class="msg" id="login-msg"></div>
<form method="post" action="/login">
<div class="field">
<label for="pwd">密码</label>
<input id="pwd" name="password" type="password" placeholder="输入管理密码" autocomplete="current-password" required>
</div>
<button class="btn" type="submit">登录</button>
</form>
</div>
</div>
<script>
(function(){var t=localStorage.getItem('theme');if(t==='dark'){document.documentElement.setAttribute('data-theme','dark')}})();
function toggleTheme(){var d=document.documentElement;var n=d.getAttribute('data-theme')==='dark'?'light':'dark';if(n==='dark')d.setAttribute('data-theme','dark');else d.removeAttribute('data-theme');localStorage.setItem('theme',n);document.querySelector('.theme-toggle').textContent=n==='dark'?'🌙':'☀'}
</script>
</body>
</html>`

const adminHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>OPENCODE TO API 管理面板</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=Noto+Sans+SC:wght@300;400;500;600;700&family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet">
<style>
:root {
  --bg: #f4f6fa;
  --surface: #ffffff;
  --surface-2: #f0f2f7;
  --border: #e2e6ed;
  --border-light: #d0d4df;
  --text: #1a1d26;
  --text-sec: #6a7180;
  --text-ter: #9ca3b0;
  --accent: #6c8aff;
  --accent-dim: rgba(108,138,255,.08);
  --accent-hover: #5a78f0;
  --green: #22a85a;
  --green-dim: rgba(34,168,90,.08);
  --green-hover: #1d9850;
  --orange: #d9600a;
  --orange-dim: rgba(217,96,10,.08);
  --orange-hover: #c45507;
  --red: #dc2626;
  --red-dim: rgba(220,38,38,.08);
  --radius: 12px;
  --radius-sm: 8px;
  --font: 'Noto Sans SC', system-ui, -apple-system, sans-serif;
  --mono: 'JetBrains Mono', Consolas, monospace;
  --glow-a: rgba(108,138,255,.03);
  --glow-b: rgba(61,214,140,.02);
  --stats-total-bg: #f0f2f7;
}
[data-theme="dark"] {
  --bg: #0c0e14;
  --surface: #14161e;
  --surface-2: #1a1d27;
  --border: #252835;
  --border-light: #2e3142;
  --text: #e8eaf0;
  --text-sec: #8b90a5;
  --text-ter: #5c6080;
  --accent: #6c8aff;
  --accent-dim: rgba(108,138,255,.12);
  --accent-hover: #5a78f0;
  --green: #3dd68c;
  --green-dim: rgba(61,214,140,.12);
  --green-hover: #30c47a;
  --orange: #f0a050;
  --orange-dim: rgba(240,160,80,.12);
  --orange-hover: #e09040;
  --red: #f06060;
  --red-dim: rgba(240,96,96,.12);
  --glow-a: rgba(108,138,255,.04);
  --glow-b: rgba(61,214,140,.03);
  --stats-total-bg: var(--surface-2);
}
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:var(--font);background:var(--bg);color:var(--text);font-size:14px;line-height:1.6;min-height:100vh}
body::before{content:'';position:fixed;top:-50%;left:-50%;width:200%;height:200%;background:radial-gradient(ellipse at 30% 20%,var(--glow-a) 0%,transparent 50%),radial-gradient(ellipse at 70% 80%,var(--glow-b) 0%,transparent 50%);pointer-events:none;z-index:0}
.container{max-width:1020px;margin:0 auto;padding:32px 24px;position:relative;z-index:1}
header{display:flex;align-items:flex-end;gap:16px;margin-bottom:28px;padding-bottom:20px;border-bottom:1px solid var(--border);justify-content:space-between}
.logo{display:flex;align-items:center;gap:10px}
.logo-mark{width:36px;height:36px;background:linear-gradient(135deg,var(--accent),#8b6cff);border-radius:10px;display:flex;align-items:center;justify-content:center;font-size:20px;color:#fff;flex-shrink:0}
.logo-text{font-size:22px;font-weight:700;letter-spacing:-.5px;background:linear-gradient(135deg,var(--text),var(--text-sec));-webkit-background-clip:text;-webkit-text-fill-color:transparent}
.logo-sub{font-size:12.5px;color:var(--text-ter);margin-bottom:2px}
.card{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);padding:22px 24px;transition:border-color .2s}
.card:hover{border-color:var(--border-light)}
.card h2{font-size:13px;font-weight:600;margin-bottom:16px;letter-spacing:.2px;display:flex;align-items:center;gap:8px;color:var(--text-sec);text-transform:uppercase}
.card h2 .dot{width:6px;height:6px;border-radius:50%;flex-shrink:0}
.config-grid{display:grid;grid-template-columns:2fr 3fr;gap:16px;margin-top:16px}
.config-grid .card{margin-bottom:0}
.full-row{grid-column:1/-1}
.form-group{margin-bottom:14px}
.form-group:last-child{margin-bottom:0}
.form-group label{display:block;font-size:11.5px;font-weight:500;color:var(--text-ter);margin-bottom:5px;letter-spacing:.4px;text-transform:uppercase}
.form-group input[type="text"],.form-group input[type="url"],.form-group input[type="password"],.form-group textarea,.form-group select,.m-select{width:100%;padding:8px 12px;border:1px solid var(--border);border-radius:var(--radius-sm);font-size:13px;font-family:var(--mono);background:var(--surface-2);color:var(--text);transition:border-color .15s,box-shadow .15s}
.form-group input:focus,.form-group textarea:focus,.form-group select:focus,.m-select:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 3px var(--accent-dim)}
.form-group .hint{font-size:11px;color:var(--text-ter);margin-top:4px;line-height:1.4}
.actions{display:flex;gap:8px;margin-top:14px;flex-wrap:wrap}
.btn{padding:8px 16px;border-radius:var(--radius-sm);font-size:12.5px;font-weight:500;cursor:pointer;border:none;transition:all .15s;font-family:var(--font);white-space:nowrap}
.btn-primary{background:var(--accent-dim);color:var(--accent)}
.btn-primary:hover{background:var(--accent);color:#fff}
.btn-default{background:var(--surface-2);color:var(--text-sec);border:1px solid var(--border)}
.btn-default:hover{border-color:var(--border-light);color:var(--text)}
.btn-success{background:var(--green-dim);color:var(--green)}
.btn-success:hover{background:var(--green);color:#fff}
.btn-warning{background:var(--orange-dim);color:var(--orange)}
.btn-warning:hover{background:var(--orange);color:#fff}
.btn-danger{background:var(--red-dim);color:var(--red)}
.btn-danger:hover{background:var(--red);color:#fff}
.tbl{width:100%;border-collapse:collapse;font-size:12.5px}
.tbl th{text-align:left;font-weight:500;color:var(--text-ter);padding:8px 10px;border-bottom:1px solid var(--border);font-size:11px;letter-spacing:.4px;text-transform:uppercase;white-space:nowrap}
.tbl td{padding:7px 10px;border-bottom:1px solid var(--border)}
.tbl tr:last-child td{border-bottom:none}
.tbl input{width:100%;padding:6px 10px;border:1px solid var(--border);border-radius:6px;font-size:12.5px;font-family:var(--mono);background:var(--surface-2);color:var(--text);transition:border-color .15s,box-shadow .15s}
.tbl input:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 2px var(--accent-dim)}
.tbl .m-select{padding:6px 10px;font-size:12.5px}
.tbl th:last-child{width:52px}
.tbl td:last-child{white-space:nowrap;text-align:center}
#statsTable th:last-child{width:auto}
#statsTable td:last-child{text-align:left;white-space:nowrap}
.tbl .btn{padding:4px 10px;font-size:11px;white-space:nowrap}
#statsTable td:first-child{font-weight:500;color:var(--text)}
#statsTable td:not(:first-child){font-family:var(--mono);color:var(--text-sec);text-align:left}
#statsTable tbody tr:hover{background:var(--surface-2)}
#statsTable thead+tbody tr:last-child td{font-weight:600;color:var(--text);background:var(--stats-total-bg);border-top:1px solid var(--border-light)}
.stats-header{display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:8px;margin-bottom:12px}
.stats-header .btns{display:flex;gap:6px;align-items:center}
#toast{position:fixed;top:20px;right:20px;padding:12px 20px;border-radius:var(--radius-sm);font-size:13px;font-weight:500;color:#fff;opacity:0;transition:opacity .25s,transform .25s;z-index:999;transform:translateY(-8px);pointer-events:none;backdrop-filter:blur(8px)}
#toast.success{background:rgba(61,214,140,.85)}
#toast.error{background:rgba(240,96,96,.85)}
#toast.show{opacity:1;transform:translateY(0)}
.empty-hint{color:var(--text-ter);font-size:13px;padding:28px;text-align:center}
.think-row{display:flex;align-items:center;gap:10px;padding:8px 12px;background:var(--surface-2);border:1px solid var(--border);border-radius:var(--radius-sm);margin-bottom:12px;transition:border-color .15s}
.think-row:hover{border-color:var(--border-light)}
.think-row input[type="checkbox"]{width:16px;height:16px;accent-color:var(--accent);cursor:pointer}
.think-row label{font-size:13px;font-weight:500;cursor:pointer;margin:0;color:var(--text)}
.think-row .hint{font-size:11px;color:var(--text-ter);margin:0 0 0 auto;white-space:nowrap}
@media(max-width:700px){.config-grid{grid-template-columns:1fr}.container{padding:16px 12px}header{flex-direction:column;align-items:flex-start;gap:8px}}
.theme-toggle{background:var(--surface-2);border:1px solid var(--border);border-radius:var(--radius-sm);padding:6px 12px;cursor:pointer;font-size:18px;display:flex;align-items:center;justify-content:center;transition:all .15s;color:var(--text-sec);flex-shrink:0;line-height:1}
.theme-toggle:hover{border-color:var(--border-light);color:var(--text)}
</style>
</head>
<body>
<div class="container">
<header>
<div class="logo">
<div class="logo-mark">⌨</div>
<div>
<div class="logo-text">OPENCODE TO API</div>
<div class="logo-sub">OpenCode 免费 API → 兼容格式代理</div>
</div>
</div>
<div style="display:flex;align-items:center;gap:8px">
<button class="theme-toggle" onclick="toggleTheme()" title="切换主题">☀</button>
<form method="post" action="/logout" style="margin:0"><button class="theme-toggle" type="submit" title="退出登录" style="font-size:14px">退出</button></form>
</div>
</header>

<div class="card">
<div class="stats-header">
<h2><span class="dot" style="background:var(--green)"></span>Token 统计</h2>
<div class="btns">
<button class="btn btn-success" onclick="reloadConfig()">刷新</button>
<button class="btn btn-danger" onclick="resetStats()">清空统计</button>
<span id="resetStatus" style="font-size:11px;color:var(--text-ter)"></span>
</div>
</div>
<div id="statsContent" style="font-size:12.5px">
<div class="empty-hint">加载中...</div>
</div>
</div>

<div class="config-grid">
<div class="card">
<h2><span class="dot" style="background:var(--orange)"></span>推理力度映射</h2>
<div style="margin-bottom:12px">
<table class="tbl" id="effortTable">
<thead><tr><th style="width:35%">请求值</th><th style="width:42%">映射值</th><th style="width:23%"></th></tr></thead>
<tbody></tbody>
</table>
</div>
<div class="think-row">
<input type="checkbox" id="force_disable_thinking">
<label for="force_disable_thinking">强制禁用思考模式</label>
<span class="hint">移除所有推理内容</span>
</div>
<div class="actions">
<button class="btn btn-primary" onclick="addEffortRow()">添加映射</button>
<button class="btn btn-success" onclick="saveConfig()">保存全部</button>
</div>
</div>

<div class="card">
<h2><span class="dot" style="background:#e85d75"></span>max_tokens 上限</h2>
<div class="form-group">
<label>全局默认上限</label>
<input type="number" id="maxTokensCap" class="m-input" min="0" placeholder="0 = 不限制" style="width:140px">
<span class="hint">超过此值的 max_tokens 会被截断到此值（0 = 不限制）</span>
</div>
<div style="margin-bottom:12px">
<table class="tbl" id="capTable">
<thead><tr><th style="width:55%">模型</th><th style="width:30%">上限</th><th style="width:15%"></th></tr></thead>
<tbody></tbody>
</table>
</div>
<div class="actions">
<button class="btn btn-primary" onclick="addCapRow()">添加模型上限</button>
<button class="btn btn-success" onclick="saveConfig()">保存全部</button>
</div>
</div>

<div class="card">
<h2><span class="dot" style="background:var(--accent)"></span>模型映射</h2>
<div style="margin-bottom:12px">
<table class="tbl" id="aliasTable">
<thead><tr><th style="width:35%">别名（请求名）</th><th style="width:42%">实际模型（上游名）</th><th style="width:23%"></th></tr></thead>
<tbody></tbody>
</table>
</div>
<div class="actions">
<button class="btn btn-primary" onclick="addAliasRow()">添加别名</button>
<button class="btn btn-success" onclick="saveConfig()">保存全部</button>
</div>
</div>

<div class="card full-row">
<h2><span class="dot" style="background:var(--accent)"></span>SOCKS5 代理</h2>
<div style="margin-bottom:12px">
<table class="tbl" id="socks5Table">
<thead><tr><th style="width:25%">名称</th><th style="width:28%">地址</th><th style="width:17%">用户名</th><th style="width:17%">密码</th><th style="width:13%"></th></tr></thead>
<tbody></tbody>
</table>
</div>
<div class="form-group">
<label>启用代理</label>
<select id="activeSocks5" class="m-select">
<option value="">直连（不使用代理）</option>
</select>
</div>
<label class="check"><input type="checkbox" id="socks5_paid_direct"> 带 key / 付费请求直连（不走 SOCKS5）</label>
<p style="margin:6px 0 12px;color:var(--muted);font-size:13px">默认关闭：只要启用了代理，public 与带 key 请求都走 SOCKS5。</p>
<div class="actions">
<button class="btn btn-primary" onclick="addSocks5Row()">添加代理</button>
<button class="btn btn-success" onclick="saveConfig()">保存全部</button>
</div>
</div>
</div>
</div>
<div id="toast"></div>
<script>
let aliasData={},effortData={},modelList=[],socks5Data=[],capData={};
function toggleTheme(){const d=document.documentElement;const cur=d.getAttribute('data-theme');const next=cur==='dark'?null:'dark';if(next)d.setAttribute('data-theme',next);else d.removeAttribute('data-theme');localStorage.setItem('theme',next||'light');document.querySelector('.theme-toggle').textContent=next==='dark'?'🌙':'☀'}
(function(){const t=localStorage.getItem('theme');if(t==='dark'){document.documentElement.setAttribute('data-theme','dark');document.addEventListener('DOMContentLoaded',()=>{const b=document.querySelector('.theme-toggle');if(b)b.textContent='🌙'})}})();
function reloadConfig(){const sy=window.scrollY;fetch('/api/reload',{method:'POST'}).then(r=>r.json()).then(d=>{showToast('会话已刷新，模型 '+d.models+' 个','success')}).catch(()=>{}).finally(()=>{loadConfig();loadStats();setTimeout(()=>window.scrollTo(0,sy),100)})}
async function loadConfig(){const sy=window.scrollY;try{const r=await fetch('/api/config');const cfg=await r.json();document.getElementById('force_disable_thinking').checked=cfg.force_disable_thinking||false;document.getElementById('socks5_paid_direct').checked=!!cfg.socks5_paid_direct;aliasData=cfg.model_alias||{};effortData=cfg.reasoning_effort_map||{};socks5Data=cfg.socks5_proxies||[];capData=cfg.max_tokens_cap_per_model||{};document.getElementById('maxTokensCap').value=cfg.max_tokens_cap||'';const mr=await fetch('/v1/models');const md=await mr.json();modelList=(md.data||[]).map(m=>m.id).sort();renderAliasTable();renderEffortTable();renderSocks5Table();renderCapTable();document.getElementById('activeSocks5').value=cfg.active_socks5||'';setTimeout(()=>window.scrollTo(0,sy),0)}catch(e){showToast('失败: '+e.message,'error')}}
function renderAliasTable(){const tb=document.querySelector('#aliasTable tbody');const ks=Object.keys(aliasData);if(!ks.length){tb.innerHTML='<tr><td colspan="3" class="empty-hint">暂无别名配置</td></tr>';return}tb.innerHTML=ks.map(k=>'<tr><td><input value="'+esc(k)+'" data-field="key"></td><td>'+modelSelectHtml(aliasData[k])+'</td><td><button class="btn btn-danger" onclick="delAlias(this)">删除</button></td></tr>').join('')}
function modelSelectHtml(selected){let h='<select data-field="val" class="m-select">';h+='<option value="">-- 选择模型 --</option>';for(const m of modelList){h+='<option value="'+esc(m)+'"'+(selected===m?' selected':'')+'>'+esc(m)+'</option>'}h+='</select>';return h}
function addAliasRow(){const tb=document.querySelector('#aliasTable tbody');if(tb.querySelector('.empty-hint'))tb.innerHTML='';tb.insertAdjacentHTML('beforeend','<tr><td><input value="" placeholder="例如: gpt-5.5" data-field="key"></td><td>'+modelSelectHtml('')+'</td><td><button class="btn btn-danger" onclick="delAlias(this)">删除</button></td></tr>')}
function delAlias(btn){const row=btn.closest('tr');const ki=row.querySelector('[data-field="key"]');if(ki&&ki.value&&aliasData[ki.value])delete aliasData[ki.value];row.remove();if(!Object.keys(aliasData).length)document.querySelector('#aliasTable tbody').innerHTML='<tr><td colspan="3" class="empty-hint">暂无别名配置</td></tr>'}
function collectAliases(){const r={};document.querySelectorAll('#aliasTable tbody tr').forEach(tr=>{const k=tr.querySelector('[data-field="key"]'),v=tr.querySelector('[data-field="val"]');if(k&&k.value.trim())r[k.value.trim()]=v?v.value.trim():''});aliasData=r;return r}
function renderEffortTable(){const tb=document.querySelector('#effortTable tbody');const ks=Object.keys(effortData);if(!ks.length){tb.innerHTML='<tr><td colspan="3" class="empty-hint">暂无映射配置</td></tr>';return}tb.innerHTML=ks.map(k=>'<tr><td><input value="'+esc(k)+'" data-field="key"></td><td><input value="'+esc(effortData[k])+'" data-field="val"></td><td><button class="btn btn-danger" onclick="delEffort(this)">删除</button></td></tr>').join('')}
function addEffortRow(){const tb=document.querySelector('#effortTable tbody');if(tb.querySelector('.empty-hint'))tb.innerHTML='';tb.insertAdjacentHTML('beforeend','<tr><td><input value="" placeholder="例如: low" data-field="key"></td><td><input value="" placeholder="例如: high" data-field="val"></td><td><button class="btn btn-danger" onclick="delEffort(this)">删除</button></td></tr>')}
function delEffort(btn){const row=btn.closest('tr');const ki=row.querySelector('[data-field="key"]');if(ki&&ki.value&&effortData[ki.value])delete effortData[ki.value];row.remove();if(!Object.keys(effortData).length)document.querySelector('#effortTable tbody').innerHTML='<tr><td colspan="3" class="empty-hint">暂无映射配置</td></tr>'}
function collectEfforts(){const r={};document.querySelectorAll('#effortTable tbody tr').forEach(tr=>{const k=tr.querySelector('[data-field="key"]'),v=tr.querySelector('[data-field="val"]');if(k&&k.value.trim())r[k.value.trim()]=v?v.value.trim():''});effortData=r;return r}
function renderSocks5Table(){const tb=document.querySelector('#socks5Table tbody');if(!socks5Data.length){tb.innerHTML='<tr><td colspan="5" class="empty-hint">暂无代理配置</td></tr>';return}tb.innerHTML=socks5Data.map((p,i)=>'<tr><td><input value="'+esc(p.name||'')+'" data-field="name"></td><td><input value="'+esc(p.addr)+'" data-field="addr" placeholder="例如: 127.0.0.1:1080"></td><td><input value="'+esc(p.username||'')+'" data-field="username"></td><td><input value="'+esc(p.password||'')+'" data-field="password" type="password"></td><td><button class="btn btn-danger" onclick="delSocks5('+i+')">删除</button></td></tr>').join('');renderSocks5Select()}
function addSocks5Row(){const tb=document.querySelector('#socks5Table tbody');if(tb.querySelector('.empty-hint'))tb.innerHTML='';socks5Data.push({addr:'',name:''});renderSocks5Table()}
function delSocks5(i){socks5Data.splice(i,1);renderSocks5Table()}
function collectSocks5(){const r=[];document.querySelectorAll('#socks5Table tbody tr').forEach(tr=>{const a=tr.querySelector('[data-field="addr"]');if(a&&a.value.trim())r.push({addr:a.value.trim(),name:(tr.querySelector('[data-field="name"]')||{}).value?.trim()||'',username:(tr.querySelector('[data-field="username"]')||{}).value?.trim()||'',password:(tr.querySelector('[data-field="password"]')||{}).value?.trim()||''})});socks5Data=r;return r}
function renderSocks5Select(){const sel=document.getElementById('activeSocks5');const cur=sel.value;sel.innerHTML='<option value="">直连（不使用代理）</option>';socks5Data.forEach(p=>{if(p.addr){const label=p.name?p.name+' ('+p.addr+')':p.addr;const opt=document.createElement('option');opt.value=p.addr;opt.textContent=label;sel.appendChild(opt)}});if(socks5Data.length>=2){const opt=document.createElement('option');opt.value='__round_robin__';opt.textContent='轮询（自动切换）';sel.appendChild(opt)}sel.value=cur;if(!sel.value)sel.value='';}
async function saveConfig(){collectAliases();collectEfforts();collectSocks5();collectCaps();const cfg={model_alias:aliasData,reasoning_effort_map:effortData,force_disable_thinking:document.getElementById('force_disable_thinking').checked,max_tokens_cap:parseInt(document.getElementById('maxTokensCap').value)||0,max_tokens_cap_per_model:capData,socks5_proxies:socks5Data,active_socks5:document.getElementById('activeSocks5').value,socks5_paid_direct:document.getElementById('socks5_paid_direct').checked};try{const r=await fetch('/api/config',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(cfg)});if(!r.ok)throw new Error(await r.text());showToast('配置已保存','success');loadConfig()}catch(e){showToast('保存失败: '+e.message,'error')}}
function renderCapTable(){const tb=document.querySelector('#capTable tbody');const ks=Object.keys(capData);if(!ks.length){tb.innerHTML='<tr><td colspan="3" class="empty-hint">暂无模型上限配置</td></tr>';return}tb.innerHTML=ks.map(k=>'<tr><td>'+modelSelectHtml(k)+'</td><td><input type="number" value="'+capData[k]+'" data-field="cap" min="0" style="width:100px"></td><td><button class="btn btn-danger" onclick="delCap(this)">删除</button></td></tr>').join('')}
function addCapRow(){const tb=document.querySelector('#capTable tbody');const tr=document.createElement('tr');tr.innerHTML='<td>'+modelSelectHtml('')+'</td><td><input type="number" value="0" data-field="cap" min="0" style="width:100px"></td><td><button class="btn btn-danger" onclick="delCap(this)">删除</button></td>';tb.appendChild(tr);if(tb.querySelector('.empty-hint'))tb.innerHTML=''}
function collectCaps(){capData={};document.querySelectorAll('#capTable tbody tr').forEach(tr=>{const sel=tr.querySelector('[data-field=key]');const inp=tr.querySelector('[data-field=cap]');if(sel&&inp){const k=sel.value;if(k){const v=parseInt(inp.value)||0;capData[k]=v}}})}
function delCap(btn){btn.closest('tr').remove();if(!document.querySelector('#capTable tbody tr'))renderCapTable()}
function esc(s){const d=document.createElement('div');d.textContent=s;return d.innerHTML}
function showToast(msg,t){const e=document.getElementById('toast');e.textContent=msg;e.className=t+' show';clearTimeout(e._tid);e._tid=setTimeout(()=>e.classList.remove('show'),2500)}
async function resetStats(){if(!confirm('确认清空所有 Token 统计？\n此操作不可撤销。'))return;const s=document.getElementById('resetStatus');s.textContent='清空中...';try{const r=await fetch('/api/stats',{method:'DELETE'});if(!r.ok)throw new Error(await r.text());document.getElementById('statsContent').innerHTML='<div class="empty-hint">暂无数据</div>';s.textContent='已清空';setTimeout(()=>s.textContent='',2000)}catch(e){s.textContent='失败: '+e.message}}
async function loadStats(){try{const r=await fetch('/api/stats');const d=await r.json();const ms=d.models||{};const ks=Object.keys(ms);let h='<table class="tbl" id="statsTable"><thead><tr><th>模型</th><th>请求数</th><th>输入 Token</th><th>输出 Token</th><th>总计 Token</th></tr></thead><tbody>';if(!ks.length){h+='<tr><td colspan="5" class="empty-hint">暂无数据</td></tr>'}else{let tr=0,pt=0,ct=0,tt=0;for(const k of ks){const m=ms[k];h+='<tr><td>'+esc(k)+'</td><td>'+fmt(m.request_count)+'</td><td>'+fmt(m.prompt_tokens)+'</td><td>'+fmt(m.completion_tokens)+'</td><td>'+fmt(m.total_tokens)+'</td></tr>';tr+=m.request_count;pt+=m.prompt_tokens;ct+=m.completion_tokens;tt+=m.total_tokens}h+='<tr><td>总计</td><td>'+fmt(tr)+'</td><td>'+fmt(pt)+'</td><td>'+fmt(ct)+'</td><td>'+fmt(tt)+'</td></tr>'}h+='</tbody></table>';document.getElementById('statsContent').innerHTML=h}catch(e){document.getElementById('statsContent').innerHTML='<div class="empty-hint">加载失败</div>'}}
function fmt(n){return n.toString().replace(/\B(?=(\d{3})+(?!\d))/g,',')}window.onload=function(){loadConfig();loadStats()};setInterval(loadStats,5000);document.addEventListener('visibilitychange',function(){if(!document.hidden)loadStats()});
</script>
</body>
</html>`

// ======================== Main ========================

func main() {
	var showVersion bool
	flag.StringVar(&port, "port", "8000", "服务端口")
	flag.StringVar(&configPath, "config", "config.json", "配置文件路径")
	flag.StringVar(&adminPassword, "password", "123456", "管理面板密码（留空则不启用登录验证）")
	flag.BoolVar(&debugMode, "debug", false, "启用调试日志")
	flag.StringVar(&logLevel, "log-level", "info", "日志级别: debug/info/warn/error")
	flag.StringVar(&logFile, "log-file", "opencode2api.log", "日志文件路径")
	flag.BoolVar(&logStdout, "log-stdout", true, "是否同时写 stdout")
	flag.IntVar(&logMaxSize, "log-max-size", 100, "单日志文件最大 MB，超过即轮换")
	flag.IntVar(&logMaxBackups, "log-max-backups", 7, "保留的旧日志文件个数")
	flag.IntVar(&logMaxAge, "log-max-age", 14, "旧日志保留天数")
	flag.BoolVar(&logCompress, "log-compress", true, "轮换后 gzip 压缩")
	flag.BoolVar(&logBodies, "log-bodies", false, "Debug 下记录截断的 body 摘要")
	flag.BoolVar(&showVersion, "version", false, "显示版本信息")
	flag.Parse()

	initLogger()
	defer closeLogRotator()

	if showVersion {
		fmt.Println(versionString())
		return
	}

	cfg := loadConfig(configPath)
	applyConfig(cfg)
	if err := saveConfig(configPath, cfg); err != nil {
		slog.Warn("failed to save config", "path", configPath, "error", err)
	}

	loadTokenStats()
	slog.Info("config loaded", "path", configPath)
	initOCSession()
	models, err := fetchModels()
	if err != nil {
		slog.Warn("failed to fetch models on startup", "error", err)
	} else {
		modelMu.Lock()
		modelsCache = models
		modelsLoaded = true
		modelMu.Unlock()
		slog.Info("models loaded", "count", len(models))
	}

	goModels, goErr := fetchGoModels()
	if goErr != nil {
		slog.Warn("failed to fetch go catalog on startup", "error", goErr)
	} else {
		modelMu.Lock()
		goModelsCache = goModels
		modelMu.Unlock()
		slog.Info("go catalog loaded", "count", len(goModels))
	}
	startModelRefresh()
	slog.Info("server starting",
		"port", port,
		"log_level", getLogLevelString(),
		"models", len(getModelIDs()),
		"aliases", len(modelAlias),
	)
	if adminPassword != "" {
		slog.Info("admin panel enabled", "url", fmt.Sprintf("http://localhost:%s/", port))
	} else {
		slog.Info("admin panel disabled (no password)")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", loggingMiddleware(chatCompletionsHandler))
	mux.HandleFunc("/v1/responses", loggingMiddleware(responsesHandler))
	mux.HandleFunc("/v1/messages", loggingMiddleware(claudeMessagesHandler))
	mux.HandleFunc("/v1/models", loggingMiddleware(listModelsHandler))
	mux.HandleFunc("/login", loggingMiddleware(loginHandler))
	mux.HandleFunc("/logout", loggingMiddleware(logoutHandler))
	mux.HandleFunc("/api/config", loggingMiddleware(requireAuth(adminConfigHandler)))
	mux.HandleFunc("/api/stats", loggingMiddleware(requireAuth(adminStatsHandler)))
	mux.HandleFunc("/api/reload", loggingMiddleware(requireAuth(reloadHandler)))
	mux.HandleFunc("/health", loggingMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))
	mux.HandleFunc("/", loggingMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			requireAuth(adminPageHandler)(w, r)
			return
		}
		http.NotFound(w, r)
	}))

	addr := ":" + port
	server := &http.Server{Addr: addr, Handler: mux}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("listening", "addr", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server terminated", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
	}
}
