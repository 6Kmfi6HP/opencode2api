package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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
	version = "dev"
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
	socks5Proxies []Socks5Proxy
	activeSocks5  string // 启用的代理 Addr，空表示直连，__round_robin__ 表示轮询
	socks5Mu      sync.RWMutex
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
		log.Printf("OpenCode Version: %s", ocClientVer)
		log.Printf("Session: %s", ocSessionID)
		log.Printf("Project: %s", ocProjectID)
	})
}

func refreshOCSession() {
	ocClientVer = fetchOCVersion()
	ocSessionID = "ses_" + randomString(24)
	ocProjectID = randomHex(40)
	log.Printf("会话已刷新: version=%s session=%s", ocClientVer, ocSessionID)
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
	modelsCache  []ModelInfo
	modelMu      sync.RWMutex
	modelsLoaded bool
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

func getModelIDs() []string {
	modelMu.RLock()
	defer modelMu.RUnlock()
	ids := make([]string, len(modelsCache))
	for i, m := range modelsCache {
		ids[i] = m.ID
	}
	return ids
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
				log.Printf("模型列表已自动刷新: %d 个模型", len(fetched))
			} else if err != nil {
				log.Printf("模型列表刷新失败: %v", err)
			}
		}
	}()
}

// ======================== 配置 ========================

var (
	port                 string
	configPath           = "config.json"
	modelAlias           = map[string]string{}
	reasoningEffortMap   = map[string]string{}
	forceDisableThinking bool
	debugMode            bool
	configMu             sync.RWMutex
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
	MaxTokens       int            `json:"max_tokens,omitempty"`
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
	Socks5Proxies        []Socks5Proxy     `json:"socks5_proxies,omitempty"`
	ActiveSocks5         string            `json:"active_socks5,omitempty"`
}

// ======================== Claude Messages API 类型 ========================

type ClaudeRequest struct {
	Model       string          `json:"model"`
	Messages    []ClaudeMessage `json:"messages"`
	System      any             `json:"system,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Tools       []ClaudeTool    `json:"tools,omitempty"`
	ToolChoice  any             `json:"tool_choice,omitempty"`
	Metadata    any             `json:"metadata,omitempty"`
	Thinking    any             `json:"thinking,omitempty"`
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
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     any    `json:"input,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"`
}

type ClaudeTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema"`
}

type ClaudeResponse struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Role       string          `json:"role"`
	Content    []ClaudeContent `json:"content"`
	Model      string          `json:"model"`
	StopReason string          `json:"stop_reason"`
	Usage      *ClaudeUsage    `json:"usage,omitempty"`
}

type ClaudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ======================== Responses API 类型 ========================

type ResponsesAPIRequest struct {
	Model             string          `json:"model"`
	Input             any             `json:"input"`
	Messages          []Message       `json:"messages,omitempty"`
	Instructions      string          `json:"instructions,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	Temperature       float64         `json:"temperature,omitempty"`
	MaxTokens         int             `json:"max_output_tokens,omitempty"`
	TopP              float64         `json:"top_p,omitempty"`
	FrequencyPenalty  float64         `json:"frequency_penalty,omitempty"`
	PresencePenalty   float64         `json:"presence_penalty,omitempty"`
	Reasoning         ReasonEffort    `json:"reasoning,omitempty"`
	Include           []string        `json:"include,omitempty"`
	Store             *bool           `json:"store,omitempty"`
	Tools             []ResponsesTool `json:"tools,omitempty"`
	ToolChoice        any             `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	Stop              any             `json:"stop,omitempty"`
	User              string          `json:"user,omitempty"`
	StreamOptions     any             `json:"stream_options,omitempty"`
	Metadata          any             `json:"metadata,omitempty"`
}

type ResponsesTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Function    *ToolFunction  `json:"function,omitempty"`
}

type ReasonEffort struct {
	Effort string `json:"effort,omitempty"`
}

// ======================== 配置管理 ========================

func loadConfig(path string) AppConfig {
	var cfg AppConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("警告: 配置文件解析失败: %v", err)
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
		return t == "enabled"
	case bool:
		return v
	default:
		return false
	}
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
	if req.MaxTokens != 0 {
		converted["max_tokens"] = req.MaxTokens
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
		converted["thinking"] = map[string]string{"type": "enabled"}
	} else if req.ExtraBody != nil {
		if isThinkingDisabled(req.ExtraBody["thinking"]) {
			converted["thinking"] = map[string]string{"type": "disabled"}
		} else if isThinkingEnabled(req.ExtraBody["thinking"]) {
			converted["thinking"] = map[string]string{"type": "enabled"}
		}
	}
	// 处理 reasoning_effort
	if !getForceDisableThinking() && req.ReasoningEffort != "" {
		effortMap := getReasoningEffortMap()
		if mapped, ok := effortMap[req.ReasoningEffort]; ok {
			converted["reasoning_effort"] = mapped
		} else {
			converted["reasoning_effort"] = req.ReasoningEffort
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
		log.Printf("Error marshaling upstream body: %v", err)
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
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		typ, _ := event["type"].(string)
		switch typ {
		case "message_start", "content_block_start", "content_block_delta",
			"content_block_stop", "message_delta", "message_stop", "ping":
			return true
		}
		return false
	}
	return false
}

func parseAnthropicSSE(body []byte) (map[string]any, string, []map[string]any) {
	lines := bytes.Split(body, []byte("\n"))
	var anthropicMsg map[string]any
	var textBuilder, currentToolInputBuilder strings.Builder
	var currentToolUse map[string]any
	var toolUseBlocks []map[string]any
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		typ, _ := event["type"].(string)
		switch typ {
		case "message_start":
			if m, ok := event["message"].(map[string]any); ok {
				anthropicMsg = m
			}
		case "content_block_start":
			if cb, ok := event["content_block"].(map[string]any); ok {
				if cbType, _ := cb["type"].(string); cbType == "tool_use" {
					currentToolUse = cb
					currentToolInputBuilder.Reset()
				}
			}
		case "content_block_delta":
			if delta, ok := event["delta"].(map[string]any); ok {
				if t, ok := delta["text"].(string); ok {
					textBuilder.WriteString(t)
				}
				if dt, _ := delta["type"].(string); dt == "input_json_delta" {
					if partial, ok := delta["partial_json"].(string); ok {
						currentToolInputBuilder.WriteString(partial)
					}
				}
			}
		case "content_block_stop":
			if currentToolUse != nil {
				inputStr := currentToolInputBuilder.String()
				var input any = inputStr
				var parsed any
				if json.Unmarshal([]byte(inputStr), &parsed) == nil {
					input = parsed
				}
				currentToolUse["input"] = input
				toolUseBlocks = append(toolUseBlocks, currentToolUse)
				currentToolUse = nil
			}
		case "message_delta":
			if delta, ok := event["delta"].(map[string]any); ok {
				if anthropicMsg == nil {
					anthropicMsg = map[string]any{}
				}
				if stop, ok := delta["stop_reason"].(string); ok {
					anthropicMsg["stop_reason"] = stop
				}
				if usage, ok := delta["usage"].(map[string]any); ok {
					anthropicMsg["usage"] = usage
				}
			}
		case "message_stop":
		case "error":
			return nil, "", nil
		}
	}
	return anthropicMsg, textBuilder.String(), toolUseBlocks
}

func buildOpenAIResponse(anthropicMsg map[string]any, text string, toolUseBlocks []map[string]any, modelID string) []byte {
	if anthropicMsg == nil {
		return nil
	}
	now := time.Now().Unix()
	role, _ := anthropicMsg["role"].(string)
	if role == "" {
		role = "assistant"
	}
	finishReason, _ := anthropicMsg["stop_reason"].(string)
	if finishReason == "tool_use" {
		finishReason = "tool_calls"
	}
	choice := map[string]any{
		"index":         0,
		"message":       map[string]any{"role": role, "content": text},
		"finish_reason": finishReason,
	}
	if len(toolUseBlocks) > 0 {
		var toolCalls []map[string]any
		for _, tb := range toolUseBlocks {
			toolInput := tb["input"]
			argsJSON, _ := json.Marshal(toolInput)
			toolCalls = append(toolCalls, map[string]any{
				"id":   tb["id"],
				"type": "function",
				"function": map[string]any{
					"name":      tb["name"],
					"arguments": string(argsJSON),
				},
			})
		}
		choice["message"].(map[string]any)["tool_calls"] = toolCalls
		if text == "" {
			choice["message"].(map[string]any)["content"] = nil
		}
	}
	resp := map[string]any{
		"id":      anthropicMsg["id"],
		"object":  "chat.completion",
		"created": now,
		"model":   modelID,
		"choices": []map[string]any{choice},
	}
	if usage, ok := anthropicMsg["usage"]; ok {
		resp["usage"] = usage
	}
	result, _ := json.Marshal(resp)
	return result
}

func convertAnthropicMessageToOpenAI(msg map[string]any, modelID string) []byte {
	if msg["model"] == nil {
		msg["model"] = modelID
	}
	var textBuilder strings.Builder
	var toolUses []map[string]any
	if content, ok := msg["content"].([]any); ok {
		for _, c := range content {
			if block, ok := c.(map[string]any); ok {
				switch block["type"] {
				case "text":
					if t, ok := block["text"].(string); ok {
						textBuilder.WriteString(t)
					}
				case "tool_use":
					toolUses = append(toolUses, block)
				}
			}
		}
	}
	return buildOpenAIResponse(msg, textBuilder.String(), toolUses, modelID)
}

func convertAnthropicToOpenAI(body []byte, modelID string) []byte {
	var singleMsg map[string]any
	if json.Unmarshal(body, &singleMsg) == nil {
		if typ, _ := singleMsg["type"].(string); typ == "message" {
			return convertAnthropicMessageToOpenAI(singleMsg, modelID)
		}
	}
	msg, text, toolUses := parseAnthropicSSE(body)
	if msg == nil {
		return body
	}
	if msg["model"] == nil {
		msg["model"] = modelID
	}
	return buildOpenAIResponse(msg, text, toolUses, modelID)
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

func cleanStreamDelta(delta map[string]any, keepReasoning bool) {
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
		return "", usage
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
			if !keepReasoning {
				delete(msg, "reasoning_content")
			}
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
		log.Printf("Warning: convertResponse unmarshal failed: %v", err)
		return data, nil
	}
	if choices, ok := raw["choices"].([]any); ok {
		for i, c := range choices {
			if choice, ok := c.(map[string]any); ok {
				if msg, ok := choice["message"].(map[string]any); ok {
					cleanNulls(msg)
					if !keepReasoning {
						delete(msg, "reasoning_content")
					}
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
	if usage, ok := raw["usage"].(map[string]any); ok {
		cleanU := map[string]any{
			"prompt_tokens":     usage["prompt_tokens"],
			"completion_tokens": usage["completion_tokens"],
			"total_tokens":      usage["total_tokens"],
		}
		raw["usage"] = cleanU
	}
	delete(raw, "cost")
	delete(raw, "system_fingerprint")
	return json.Marshal(raw)
}

// ======================== 认证层级 ========================

type TierType int

const (
	TierFree TierType = iota
	TierPaid
)

// tierUpstreamURL 返回对应层级的 upstream URL
func (t TierType) upstreamURL() string {
	if t == TierPaid {
		return "https://opencode.ai/zen/go/v1/chat/completions"
	}
	return "https://opencode.ai/zen/v1/chat/completions"
}

// tierModelsURL 返回对应层级的模型列表 URL
func (t TierType) modelsURL() string {
	if t == TierPaid {
		return "https://opencode.ai/zen/go/v1/models"
	}
	return "https://opencode.ai/zen/v1/models"
}

// extractAuthTier 从请求中提取 Bearer token 并判断层级
func extractAuthTier(r *http.Request) (bearerToken string, tier TierType) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return "", TierFree
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	if token == "public" {
		return token, TierFree
	}
	return token, TierPaid
}

// isFreeModel 判断模型是否属于免费模型（以 -free 结尾）
func isFreeModel(modelID string) bool {
	return strings.HasSuffix(modelID, "-free")
}

func buildOCRequest(modelID string, bodyMap map[string]any, bearerToken string) (*http.Request, error) {
	bodyMap["model"] = modelID
	delete(bodyMap, "reasoning_effort")
	tryBody, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, err
	}
	// 根据 bearerToken 决定层级和上游 URL
	tier := TierFree
	authValue := "Bearer public"
	if bearerToken != "" && bearerToken != "public" {
		tier = TierPaid
		authValue = "Bearer " + bearerToken
	}
	req, err := http.NewRequest("POST", tier.upstreamURL(), bytes.NewReader(tryBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authValue)
	req.Header.Set("User-Agent", fmt.Sprintf("opencode/%s", ocClientVer))
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-project", ocProjectID)
	req.Header.Set("x-opencode-session", ocSessionID)
	req.Header.Set("x-opencode-request", "req_"+randomString(24))
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func shouldRetryUpstreamStatus(status int) bool {
	// 仅重试可恢复的临时性错误
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

func callOpenCodeAPI(upstreamBody []byte, modelID string, bearerToken string) ([]byte, int, http.Header, error) {
	initOCSession()
	modelIDs := getModelIDs()
	modelsToTry := []string{modelID}
	for _, m := range modelIDs {
		if m != modelID {
			modelsToTry = append(modelsToTry, m)
		}
	}
	if len(modelsToTry) == 0 {
		modelsToTry = []string{modelID}
	}

	// 循环外解析一次
	var bodyMap map[string]any
	if err := json.Unmarshal(upstreamBody, &bodyMap); err != nil {
		return nil, 500, nil, fmt.Errorf("invalid request body")
	}

	var lastErr error
	var retryCount int
	var retry401Count int
	var lastBody []byte
	var lastStatus int
	var lastHeader http.Header
	for i, tryModel := range modelsToTry {
		up, err := buildOCRequest(tryModel, bodyMap, bearerToken)
		if err != nil {
			lastErr = err
			continue
		}
		client := getHTTPClient()
		resp, err := client.Do(up)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			b, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				return nil, 0, nil, readErr
			}
			if isAnthropicFormat(b) {
				b = convertAnthropicToOpenAI(b, tryModel)
			}
			return b, resp.StatusCode, resp.Header, nil
		}
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if debugMode {
			log.Printf("[upstream error] model=%s status=%d body=%s", tryModel, resp.StatusCode, string(errBody))
		}
		lastBody = errBody
		lastStatus = resp.StatusCode
		lastHeader = resp.Header
		lastErr = fmt.Errorf("upstream error")
		if shouldRetryUpstreamStatus(resp.StatusCode) && i < len(modelsToTry)-1 {
			client.CloseIdleConnections()
			if resp.StatusCode == http.StatusUnauthorized {
				retry401Count++
				if retry401Count >= max401Retries {
					break
				}
			} else {
				retryCount++
				if retryCount >= maxUpstreamRetries {
					break
				}
			}
			continue
		}
		break
	}
	return lastBody, lastStatus, lastHeader, lastErr
}

func callOpenCodeAPIStream(upstreamBody []byte, modelID string, bearerToken string) (io.ReadCloser, int, http.Header, error) {
	initOCSession()
	modelIDs := getModelIDs()
	modelsToTry := []string{modelID}
	for _, m := range modelIDs {
		if m != modelID {
			modelsToTry = append(modelsToTry, m)
		}
	}
	if len(modelsToTry) == 0 {
		modelsToTry = []string{modelID}
	}

	var bodyMap map[string]any
	if err := json.Unmarshal(upstreamBody, &bodyMap); err != nil {
		return nil, 500, nil, fmt.Errorf("invalid request body")
	}

	var lastBody []byte
	var lastStatus int
	var lastHeader http.Header
	var retryCount int
	var retry401Count int
	for i, tryModel := range modelsToTry {
		up, err := buildOCRequest(tryModel, bodyMap, bearerToken)
		if err != nil {
			continue
		}
		client := getHTTPClient()
		resp, err := client.Do(up)
		if err != nil {
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp.Body, resp.StatusCode, resp.Header, nil
		}
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if debugMode {
			log.Printf("[upstream error] model=%s status=%d body=%s", tryModel, resp.StatusCode, string(errBody))
		}
		lastBody = errBody
		lastStatus = resp.StatusCode
		lastHeader = resp.Header
		if shouldRetryUpstreamStatus(resp.StatusCode) && i < len(modelsToTry)-1 {
			client.CloseIdleConnections()
			if resp.StatusCode == http.StatusUnauthorized {
				retry401Count++
				if retry401Count >= max401Retries {
					break
				}
			} else {
				retryCount++
				if retryCount >= maxUpstreamRetries {
					break
				}
			}
			continue
		}
		// 返回错误体供下游透传
		return io.NopCloser(bytes.NewReader(lastBody)), lastStatus, lastHeader, nil
	}
	if lastStatus != 0 {
		return io.NopCloser(bytes.NewReader(lastBody)), lastStatus, lastHeader, nil
	}
	return nil, 500, nil, fmt.Errorf("all models failed")
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
	bearerToken, _ := extractAuthTier(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	cnt := requestCount.Add(1)
	if debugMode {
		log.Printf("[request #%d] POST /v1/chat/completions\n%s", cnt, string(body))
	}

	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.Model = resolveModel(req.Model)
	if req.Model == "" {
		modelIDs := getModelIDs()
		if len(modelIDs) > 0 {
			req.Model = modelIDs[0]
		} else {
			req.Model = "deepseek-v4-flash-free"
		}
	}

	// 多模态路由：检测到图片时转发到配置的上游

	req.Messages = fixToolCallGaps(req.Messages)
	keepReasoning := wantsReasoning(&req)
	req.Messages = ensureReasoningContent(req.Messages, keepReasoning)
	upstreamBody := buildUpstreamBody(&req)

	if req.Stream {
		upResp, status, _, err := callOpenCodeAPIStream(upstreamBody, req.Model, bearerToken)
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
		doneSeen := false
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					break
				}
				log.Printf("Error reading stream: %v", err)
				// 发送错误事件通知客户端
				w.Write([]byte("data: {\"error\":\"stream read error\"}\n\n"))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				return
			}
			if doneSeen {
				continue
			}
			trimmed := strings.TrimSpace(line)
			if trimmed == "data: [DONE]" {
				doneSeen = true
				w.Write([]byte("data: [DONE]\n\n"))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				continue
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
		return
	}

	respBody, status, _, err := callOpenCodeAPI(upstreamBody, req.Model, bearerToken)
	if err != nil || status < 200 || status >= 300 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if len(respBody) > 0 {
			w.Write(respBody)
		} else {
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error", "type": "upstream_error"}})
		}
		return
	}
	outBody := respBody
	convertedResp, err := convertResponse(respBody, keepReasoning)
	if err == nil {
		outBody = convertedResp
	}
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
	// 追加别名模型
	configMu.RLock()
	aliases := make([]string, 0, len(modelAlias))
	for k := range modelAlias {
		aliases = append(aliases, k)
	}
	configMu.RUnlock()
	now := time.Now().Unix()
	aliasModels := make([]ModelInfo, 0, len(aliases))
	for _, alias := range aliases {
		aliasModels = append(aliasModels, ModelInfo{ID: alias, Object: "model", Created: now, OwnedBy: "alias"})
	}
	allModels := append(models, aliasModels...)

	// 根据认证层级过滤模型
	bearerToken, _ := extractAuthTier(r)
	if bearerToken == "" || bearerToken == "public" {
		filtered := make([]ModelInfo, 0, len(allModels))
		for _, m := range allModels {
			if isFreeModel(m.ID) {
				filtered = append(filtered, m)
			}
		}
		if len(filtered) > 0 {
			allModels = filtered
		}
		// 如果没有 -free 模型，返回空列表
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   allModels,
	})
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
	delete(m, "$schema")
	delete(m, "title")
	delete(m, "examples")
	delete(m, "additionalProperties")
	if m["type"] == "string" {
		delete(m, "format")
	}
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			m[k] = cleanJsonSchema(sub)
		}
		if arr, ok := v.([]any); ok {
			for i, elem := range arr {
				if sub, ok := elem.(map[string]any); ok {
					arr[i] = cleanJsonSchema(sub)
				}
			}
			m[k] = arr
		}
	}
	return m
}

func claudeToOpenAIMessages(claudeMsgs []ClaudeMessage, system any) []Message {
	var messages []Message
	if sysText := extractClaudeSystemText(system); sysText != "" {
		messages = append(messages, Message{Role: "system", Content: sysText})
	}
	for _, msg := range claudeMsgs {
		switch content := msg.Content.(type) {
		case string:
			messages = append(messages, Message{Role: msg.Role, Content: content})
		case []any:
			var textParts []string
			var reasoningParts []string
			var toolCalls []ToolCall
			var toolResults []Message
			var imageParts []map[string]any
			for _, item := range content {
				block, ok := item.(map[string]any)
				if !ok {
					continue
				}
				blockType, _ := block["type"].(string)
				switch blockType {
				case "text":
					if text, ok := block["text"].(string); ok && text != "" {
						textParts = append(textParts, text)
					}
				case "image":
					source, _ := block["source"].(map[string]any)
					if source != nil {
						srcType, _ := source["type"].(string)
						mediaType, _ := source["media_type"].(string)
						data, _ := source["data"].(string)
						if srcType == "base64" && data != "" {
							if mediaType == "" {
								mediaType = "image/png"
							}
							imageParts = append(imageParts, map[string]any{
								"type": "image_url",
								"image_url": map[string]string{
									"url": "data:" + mediaType + ";base64," + data,
								},
							})
						}
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
					switch c := block["content"].(type) {
					case string:
						resultText = c
					case []any:
						var parts []string
						for _, p := range c {
							if pb, ok := p.(map[string]any); ok && pb["type"] == "text" {
								if t, ok := pb["text"].(string); ok {
									parts = append(parts, t)
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
					toolResults = append(toolResults, Message{
						Role:       "tool",
						ToolCallID: toolUseID,
						Content:    resultText,
					})
				}
			}
			om := Message{Role: msg.Role}
			if len(imageParts) > 0 {
				var contentArr []any
				for _, img := range imageParts {
					contentArr = append(contentArr, img)
				}
				if len(textParts) > 0 {
					contentArr = append(contentArr, map[string]any{
						"type": "text",
						"text": strings.Join(textParts, "\n"),
					})
				}
				om.Content = contentArr
			} else if len(textParts) > 0 {
				om.Content = strings.Join(textParts, "\n")
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
			messages = append(messages, om)
			messages = append(messages, toolResults...)
		default:
			b, _ := json.Marshal(content)
			messages = append(messages, Message{Role: msg.Role, Content: string(b)})
		}
	}
	return messages
}

func claudeToOpenAITools(claudeTools []ClaudeTool) []Tool {
	tools := make([]Tool, 0, len(claudeTools))
	for _, ct := range claudeTools {
		params := ct.InputSchema
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		params = cleanJsonSchema(params)
		tools = append(tools, Tool{
			Type: "function",
			Function: ToolFunction{
				Name:        ct.Name,
				Description: ct.Description,
				Parameters:  params.(map[string]any),
			},
		})
	}
	return tools
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
		log.Printf("Warning: openAIToClaudeResponse unmarshal failed: %v", err)
	}

	content := []ClaudeContent{}
	stopReason := "end_turn"

	if len(chat.Choices) > 0 {
		msg := chat.Choices[0].Message
		fr := chat.Choices[0].FinishReason
		if wantReasoning && msg.ReasoningContent != "" {
			content = append(content, ClaudeContent{
				Type:     "thinking",
				Thinking: msg.ReasoningContent,
			})
		}
		if msg.Content != "" {
			content = append(content, ClaudeContent{
				Type: "text",
				Text: msg.Content,
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
		switch fr {
		case "stop":
			stopReason = "end_turn"
		case "length":
			stopReason = "max_tokens"
		case "tool_calls", "function_call":
			stopReason = "tool_use"
		}
	}

	if len(content) == 0 {
		content = append(content, ClaudeContent{Type: "text", Text: ""})
	}

	resp := ClaudeResponse{
		ID:         fmt.Sprintf("msg_%s", randomString(24)),
		Type:       "message",
		Role:       "assistant",
		Content:    content,
		Model:      model,
		StopReason: stopReason,
	}
	if chat.Usage != nil {
		inputTokens, _ := chat.Usage["prompt_tokens"]
		outputTokens, _ := chat.Usage["completion_tokens"]
		resp.Usage = &ClaudeUsage{
			InputTokens:  int(toFloat64(inputTokens)),
			OutputTokens: int(toFloat64(outputTokens)),
		}
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

func claudeMessagesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	bearerToken, _ := extractAuthTier(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	cnt := requestCount.Add(1)
	if debugMode {
		log.Printf("[request #%d] POST /v1/messages\n%s", cnt, string(body))
	}

	var claudeReq ClaudeRequest
	if err := json.Unmarshal(body, &claudeReq); err != nil {
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"Invalid JSON"}}`, http.StatusBadRequest)
		return
	}
	claudeReq.Model = resolveModel(claudeReq.Model)

	// 多模态路由

	messages := claudeToOpenAIMessages(claudeReq.Messages, claudeReq.System)
	messages = fixToolCallGaps(messages)

	chatReq := OpenAIRequest{
		Model:    claudeReq.Model,
		Messages: messages,
		Stream:   claudeReq.Stream,
	}
	if claudeReq.MaxTokens > 0 {
		chatReq.MaxTokens = claudeReq.MaxTokens
	}
	if claudeReq.Temperature != nil {
		chatReq.Temperature = claudeReq.Temperature
	}
	if claudeReq.TopP != nil {
		chatReq.TopP = claudeReq.TopP
	}
	if len(claudeReq.Tools) > 0 {
		chatReq.Tools = claudeToOpenAITools(claudeReq.Tools)
		chatReq.ToolChoice = "auto"
	}

	wantReasoning := !getForceDisableThinking()
	if claudeReq.Thinking != nil {
		if isThinkingDisabled(claudeReq.Thinking) {
			wantReasoning = false
		}
	}
	keepReasoning := wantReasoning
	chatReq.Messages = ensureReasoningContent(chatReq.Messages, keepReasoning)

	upstreamBody := buildUpstreamBody(&chatReq)

	if claudeReq.Stream {
		upResp, status, _, err := callOpenCodeAPIStream(upstreamBody, chatReq.Model, bearerToken)
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
		claudeStreamHandler(w, upResp, claudeReq.Model, keepReasoning)
		return
	}

	respBody, status, _, err := callOpenCodeAPI(upstreamBody, chatReq.Model, bearerToken)
	if err != nil || status < 200 || status >= 300 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if len(respBody) > 0 {
			w.Write(respBody)
		} else {
			json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": "upstream error"}})
		}
		return
	}

	claudeRespBody := openAIToClaudeResponse(respBody, claudeReq.Model, wantReasoning)

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
	if debugMode {
		log.Printf("[client response]\n%s", string(claudeRespBody))
	}
	w.Write(claudeRespBody)
}

func claudeStreamHandler(w http.ResponseWriter, respBody io.ReadCloser, model string, keepReasoning bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(respBody)

	msgID := fmt.Sprintf("msg_%s", randomString(24))
	blockIndex := 0
	thinkingBlockOpen := false
	textBlockOpen := false
	toolCallAccumulator := map[int]map[string]string{}
	toolCallOrder := []int{}
	messageStartSent := false
	fullUsage := map[string]any{}
	defer func() {
		if len(fullUsage) > 0 {
			pt, _ := fullUsage["prompt_tokens"].(float64)
			ct, _ := fullUsage["completion_tokens"].(float64)
			tt, _ := fullUsage["total_tokens"].(float64)
			if tt > 0 {
				recordTokenUsage(model, int64(pt), int64(ct), int64(tt))
			}
		}
	}()

	emitClaudeEvent := func(event string, data any) {
		jsonData, err := json.Marshal(data)
		if err != nil {
			log.Printf("Error marshaling Claude SSE event: %v", err)
			return
		}
		w.Write([]byte("event: " + event + "\n"))
		w.Write([]byte("data: " + string(jsonData) + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
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

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Printf("Error reading stream: %v", err)
			break
		}
		if debugMode && strings.HasPrefix(line, "data: ") {
			log.Printf("[upstream raw chunk] %s", strings.TrimSpace(line[6:]))
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "data: [DONE]" || trimmed == "[DONE]" {
			break
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var chunk map[string]any
		if err := json.Unmarshal([]byte(line[6:]), &chunk); err != nil {
			continue
		}

		choices, ok := chunk["choices"].([]any)
		if !ok || len(choices) == 0 {
			if usage, ok := chunk["usage"].(map[string]any); ok {
				fullUsage = usage
			}
			continue
		}

		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		finishReason, _ := choice["finish_reason"].(string)

		if !messageStartSent {
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
					"usage":       map[string]any{"input_tokens": 0, "output_tokens": 0},
				},
			})
			emitClaudeEvent("ping", map[string]any{"type": "ping"})
		}

		if rc, ok := delta["reasoning_content"]; ok && keepReasoning {
			rcStr, _ := rc.(string)
			if rcStr != "" {
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
			}
		}

		if c, ok := delta["content"]; ok && c != nil {
			contentStr, _ := c.(string)
			if contentStr != "" {
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
						"index": blockIndex - 1,
						"delta": map[string]any{
							"type":         "input_json_delta",
							"partial_json": argDelta,
						},
					})
				}
			}
		}

		if usage, ok := chunk["usage"].(map[string]any); ok {
			fullUsage = usage
		}

		if finishReason == "stop" || finishReason == "length" || finishReason == "tool_calls" || finishReason == "function_call" || finishReason == "content_filter" {
			closeThinkingBlock()
			closeTextBlock()

			for _, idx := range toolCallOrder {
				acc := toolCallAccumulator[idx]
				emitClaudeEvent("content_block_stop", map[string]any{
					"type":  "content_block_stop",
					"index": blockIndex - len(toolCallOrder) + indexOfInt(toolCallOrder, idx),
					"content_block": map[string]any{
						"type":  "tool_use",
						"id":    acc["id"],
						"name":  acc["name"],
						"input": map[string]any{},
					},
				})
			}

			stopReason := "end_turn"
			switch finishReason {
			case "length":
				stopReason = "max_tokens"
			case "tool_calls", "function_call":
				stopReason = "tool_use"
			}

			usage := map[string]any{}
			if len(fullUsage) > 0 {
				usage["input_tokens"] = fullUsage["prompt_tokens"]
				usage["output_tokens"] = fullUsage["completion_tokens"]
			} else {
				usage["input_tokens"] = 0
				usage["output_tokens"] = 0
			}

			emitClaudeEvent("message_delta", map[string]any{
				"type": "message_delta",
				"delta": map[string]any{
					"stop_reason": stopReason,
				},
				"usage": map[string]any{
					"output_tokens": usage["output_tokens"],
				},
			})
			emitClaudeEvent("message_stop", map[string]any{
				"type": "message_stop",
			})
			return
		}
	}

	closeThinkingBlock()
	closeTextBlock()
	emitClaudeEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{"output_tokens": 0},
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
		for _, item := range v {
			switch elem := item.(type) {
			case string:
				messages = append(messages, Message{Role: "user", Content: elem})
			case map[string]any:
				itemType, _ := elem["type"].(string)
				switch itemType {
				case "function_call", "tool_call":
					callID, _ := elem["call_id"].(string)
					if callID == "" {
						callID, _ = elem["id"].(string)
					}
					name, _ := elem["name"].(string)
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
						output := functionOutputs[callID]
						if output == "" {
							output = "[tool output missing]"
						}
						messages = append(messages, Message{Role: "tool", ToolCallID: callID, Content: output})
					}
				case "function_call_output", "tool_result":
					callID, _ := elem["call_id"].(string)
					if callID == "" {
						callID, _ = elem["tool_use_id"].(string)
					}
					if callID != "" {
						output := functionOutputs[callID]
						if output == "" {
							switch o := elem["output"].(type) {
							case string:
								output = o
							default:
								if o != nil {
									b, _ := json.Marshal(o)
									output = string(b)
								}
							}
						}
						if output == "" {
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
					text := extractTextFromContentParts(elem["content"])
					messages = append(messages, Message{Role: role, Content: text})
				default:
					role := "user"
					if r, ok := elem["role"].(string); ok && r != "" {
						role = r
					}
					text := extractTextFromContentParts(elem["content"])
					if text == "" {
						b, _ := json.Marshal(elem)
						text = string(b)
					}
					messages = append(messages, Message{Role: role, Content: text})
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
		if tool.Type != "function" {
			continue
		}
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
		converted = append(converted, Tool{Type: "function", Function: fn})
	}
	return converted
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
	return choice
}

func collectFunctionOutputs(items []any) map[string]string {
	outputs := map[string]string{}
	for _, item := range items {
		elem, ok := item.(map[string]any)
		if !ok || elem["type"] != "function_call_output" {
			continue
		}
		callID, _ := elem["call_id"].(string)
		if callID == "" {
			continue
		}
		switch v := elem["output"].(type) {
		case string:
			outputs[callID] = v
		default:
			b, _ := json.Marshal(v)
			outputs[callID] = string(b)
		}
	}
	return outputs
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

func responsesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	bearerToken, _ := extractAuthTier(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	cnt := requestCount.Add(1)
	if debugMode {
		log.Printf("[request #%d] POST /v1/responses\n%s", cnt, string(body))
	}

	var respReq ResponsesAPIRequest
	if err := json.Unmarshal(body, &respReq); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	respReq.Model = resolveModel(respReq.Model)
	if respReq.Model == "" {
		modelIDs := getModelIDs()
		if len(modelIDs) > 0 {
			respReq.Model = modelIDs[0]
		} else {
			respReq.Model = "deepseek-v4-flash-free"
		}
	}

	// 多模态路由

	messages := respReq.Messages
	if len(messages) == 0 {
		messages = responsesInputToMessages(respReq.Input, respReq.Instructions)
	} else if respReq.Instructions != "" {
		messages = append([]Message{{Role: "system", Content: respReq.Instructions}}, messages...)
	}

	chatReq := OpenAIRequest{
		Model:    respReq.Model,
		Messages: messages,
		Stream:   respReq.Stream,
	}
	if respReq.Temperature != 0 {
		chatReq.Temperature = &respReq.Temperature
	}
	if respReq.MaxTokens != 0 {
		chatReq.MaxTokens = respReq.MaxTokens
	}
	if respReq.TopP != 0 {
		chatReq.TopP = &respReq.TopP
	}
	if len(respReq.Tools) > 0 {
		chatReq.Tools = convertResponsesTools(respReq.Tools)
	}
	if respReq.ToolChoice != nil {
		chatReq.ToolChoice = convertResponsesToolChoice(respReq.ToolChoice)
	}
	if respReq.ParallelToolCalls != nil {
		chatReq.ExtraBody = map[string]any{"parallel_tool_calls": *respReq.ParallelToolCalls}
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

	upstreamBody := buildUpstreamBody(&chatReq)

	if respReq.Stream {
		upResp, status, _, err := callOpenCodeAPIStream(upstreamBody, chatReq.Model, bearerToken)
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
		responsesStreamHandler(w, r, resp, chatReq.Model, chatReq.Model, wantReasoning, chatReq.Tools, chatReq.ToolChoice)
		return
	}

	respBody, status, _, err := callOpenCodeAPI(upstreamBody, chatReq.Model, bearerToken)
	if err != nil || status < 200 || status >= 300 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if len(respBody) > 0 {
			w.Write(respBody)
		} else {
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error"}})
		}
		return
	}

	responsesBody := convertChatToResponses(respBody, chatReq.Model, wantReasoning, chatReq.Tools, chatReq.ToolChoice)

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
	if debugMode {
		log.Printf("[responses response]\n%s", string(responsesBody))
	}
	w.Write(responsesBody)
}

// ======================== Responses Stream Handler ========================

func responsesStreamHandler(w http.ResponseWriter, _ *http.Request, resp *http.Response, model string, _ string, wantReasoning bool, tools []Tool, toolChoice any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(resp.Body)

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
	totalUsage := map[string]any{}
	createdSent := false
	toolCalls := map[int]map[string]any{}
	toolOrder := []int{}

	messageOutputIndex := func() int {
		if reasoningStarted {
			return 1
		}
		return 0
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
		if status == "completed" {
			item["encrypted_content"] = ""
		}
		if fullReasoning != "" {
			item["summary"] = []any{map[string]any{"type": "summary_text", "text": fullReasoning}}
		}
		return item
	}

	messageItem := func(status string) map[string]any {
		content := []any{map[string]any{
			"type":        "output_text",
			"annotations": []any{},
			"logprobs":    []any{},
			"text":        fullText,
		}}
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
			"output_index":    0,
			"summary_index":   0,
			"text":            fullReasoning,
		})
		seq++
		emitSSEEvent(w, flusher, "response.reasoning_summary_part.done", map[string]any{
			"type":            "response.reasoning_summary_part.done",
			"sequence_number": seq,
			"item_id":         reasoningID,
			"output_index":    0,
			"summary_index":   0,
			"part":            map[string]any{"type": "summary_text", "text": fullReasoning},
		})
		seq++
		emitSSEEvent(w, flusher, "response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
			"sequence_number": seq,
			"output_index":    0,
			"item":            reasoningItem("completed"),
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
			"item":            messageItem("completed"),
		})
		messageDone = true
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
			"arguments":       args,
		})
		seq++
		emitSSEEvent(w, flusher, "response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
			"sequence_number": seq,
			"output_index":    idx,
			"item": map[string]any{
				"id":        itemID,
				"type":      "function_call",
				"status":    "completed",
				"arguments": args,
				"call_id":   callID,
				"name":      name,
			},
		})
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Printf("Error reading stream: %v", err)
			return
		}
		if debugMode && strings.HasPrefix(line, "data: ") {
			log.Printf("[upstream raw chunk] %s", strings.TrimSpace(line[6:]))
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "data: [DONE]" || trimmed == "[DONE]" {
			break
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var chunk map[string]any
		if err := json.Unmarshal([]byte(line[6:]), &chunk); err != nil {
			continue
		}
		if !createdSent {
			if id, ok := chunk["id"].(string); ok && id != "" {
				responseID = id
				reasoningID = "rs_" + responseID + "_0"
				msgID = "msg_" + responseID + "_0"
			}
			if created, ok := chunk["created"].(float64); ok {
				createdAt = int64(created)
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
		choices, ok := chunk["choices"].([]any)
		if !ok || len(choices) == 0 {
			if usage, ok := chunk["usage"].(map[string]any); ok {
				totalUsage = usage
			}
			continue
		}

		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		finishReason, _ := choice["finish_reason"].(string)

		if rc, ok := delta["reasoning_content"]; ok && wantReasoning {
			rcStr, _ := rc.(string)
			if rcStr != "" {
				if !reasoningStarted {
					seq++
					emitSSEEvent(w, flusher, "response.output_item.added", map[string]any{
						"type":            "response.output_item.added",
						"sequence_number": seq,
						"output_index":    0,
						"item":            reasoningItem("in_progress"),
					})
					seq++
					emitSSEEvent(w, flusher, "response.reasoning_summary_part.added", map[string]any{
						"type":            "response.reasoning_summary_part.added",
						"sequence_number": seq,
						"item_id":         reasoningID,
						"output_index":    0,
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
					"output_index":    0,
					"summary_index":   0,
					"delta":           rcStr,
				})
			}
		}

		contentStr := ""
		if c, ok := delta["content"]; ok && c != nil {
			contentStr, _ = c.(string)
		}
		if contentStr != "" {
			emitReasoningDone()
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
				outputIndex := messageOutputIndex()
				if messageStarted {
					outputIndex++
				}
				outputIndex += len(toolOrder)
				callID, _ := tc["id"].(string)
				if callID == "" {
					callID = "call_" + randomString(12)
				}
				fn, _ := tc["function"].(map[string]any)
				name, _ := fn["name"].(string)
				call = map[string]any{
					"output_index": outputIndex,
					"item_id":      "fc_" + callID,
					"call_id":      callID,
					"name":         name,
					"arguments":    "",
					"done":         false,
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
						"type":      "function_call",
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
		if finishReason == "stop" || finishReason == "length" || finishReason == "content_filter" {
			emitReasoningDone()
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
		}
	}

	emitReasoningDone()
	emitMessageDone()
	for _, idx := range toolOrder {
		emitToolCallDone(toolCalls[idx]["output_index"].(int), toolCalls[idx])
	}

	output := []any{}
	if reasoningStarted {
		output = append(output, reasoningItem("completed"))
	}
	if messageStarted {
		output = append(output, messageItem("completed"))
	}
	for _, idx := range toolOrder {
		call := toolCalls[idx]
		output = append(output, map[string]any{
			"id":        call["item_id"],
			"type":      "function_call",
			"status":    "completed",
			"arguments": call["arguments"],
			"call_id":   call["call_id"],
			"name":      call["name"],
		})
	}

	completedResponse := map[string]any{
		"id":                 responseID,
		"object":             "response",
		"created_at":         createdAt,
		"status":             "completed",
		"background":         false,
		"error":              nil,
		"incomplete_details": nil,
		"model":              model,
		"output":             output,
	}
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
	emitSSEEvent(w, flusher, "response.completed", map[string]any{
		"type":            "response.completed",
		"sequence_number": seq,
		"response":        completedResponse,
	})

	if flusher != nil {
		flusher.Flush()
	}
}

func convertChatToResponses(chatBody []byte, model string, wantReasoning bool, tools []Tool, toolChoice any) []byte {
	var chat struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string     `json:"content"`
				ReasoningContent string     `json:"reasoning_content"`
				ToolCalls        []ToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		log.Printf("Warning: convertChatToResponses unmarshal failed: %v", err)
	}

	text := ""
	reasoning := ""
	finishReason := ""
	var toolCalls []ToolCall
	if len(chat.Choices) > 0 {
		text = chat.Choices[0].Message.Content
		if wantReasoning {
			reasoning = chat.Choices[0].Message.ReasoningContent
		}
		toolCalls = chat.Choices[0].Message.ToolCalls
		finishReason = chat.Choices[0].FinishReason
	}

	status := "completed"
	if finishReason == "length" {
		status = "incomplete"
	}

	responses := map[string]any{
		"id":                 chat.ID,
		"object":             "response",
		"status":             status,
		"background":         false,
		"error":              nil,
		"incomplete_details": nil,
		"model":              model,
		"created_at":         chat.Created,
	}
	if len(tools) > 0 {
		responses["tools"] = tools
	}
	if toolChoice != nil {
		responses["tool_choice"] = toolChoice
	}
	outputID := "msg_" + chat.ID + "_0"
	output := []any{}
	if reasoning != "" {
		output = append(output, map[string]any{
			"id":                "rs_" + chat.ID,
			"type":              "reasoning",
			"encrypted_content": "",
			"summary":           []any{map[string]any{"type": "summary_text", "text": reasoning}},
		})
	}
	if text != "" {
		output = append(output, map[string]any{
			"id":     outputID,
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []any{map[string]any{
				"type":        "output_text",
				"text":        text,
				"annotations": []any{},
				"logprobs":    []any{},
			}},
		})
	}
	for _, tc := range toolCalls {
		output = append(output, map[string]any{
			"id":        "fc_" + tc.ID,
			"type":      "function_call",
			"status":    "completed",
			"arguments": tc.Function.Arguments,
			"call_id":   tc.ID,
			"name":      tc.Function.Name,
		})
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
		log.Printf("Error marshaling SSE event: %v", err)
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
		log.Printf("模型列表已刷新: %d 个模型", len(fetched))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"session": ocSessionID,
		"models":  len(modelsCache),
	})
}

func adminConfigHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		configMu.RLock()
		cfg := AppConfig{ModelAlias: modelAlias, ReasoningEffortMap: reasoningEffortMap, ForceDisableThinking: forceDisableThinking}
		configMu.RUnlock()
		socks5Mu.RLock()
		cfg.Socks5Proxies = socks5Proxies
		cfg.ActiveSocks5 = activeSocks5
		socks5Mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cfg)
	case http.MethodPost:
		var cfg AppConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, `{"error":"Invalid JSON"}`, http.StatusBadRequest)
			return
		}
		if err := saveConfig(configPath, cfg); err != nil {
			http.Error(w, `{"error":"Failed to save config"}`, http.StatusInternalServerError)
			return
		}
		applyConfig(cfg)
		if debugMode {
			log.Printf("Config updated: aliases=%d, effort_map=%d, force_disable=%v", len(cfg.ModelAlias), len(cfg.ReasoningEffortMap), cfg.ForceDisableThinking)
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
<div class="actions">
<button class="btn btn-primary" onclick="addSocks5Row()">添加代理</button>
<button class="btn btn-success" onclick="saveConfig()">保存全部</button>
</div>
</div>
</div>
</div>
<div id="toast"></div>
<script>
let aliasData={},effortData={},modelList=[],socks5Data=[];
function toggleTheme(){const d=document.documentElement;const cur=d.getAttribute('data-theme');const next=cur==='dark'?null:'dark';if(next)d.setAttribute('data-theme',next);else d.removeAttribute('data-theme');localStorage.setItem('theme',next||'light');document.querySelector('.theme-toggle').textContent=next==='dark'?'🌙':'☀'}
(function(){const t=localStorage.getItem('theme');if(t==='dark'){document.documentElement.setAttribute('data-theme','dark');document.addEventListener('DOMContentLoaded',()=>{const b=document.querySelector('.theme-toggle');if(b)b.textContent='🌙'})}})();
function reloadConfig(){const sy=window.scrollY;fetch('/api/reload',{method:'POST'}).then(r=>r.json()).then(d=>{showToast('会话已刷新，模型 '+d.models+' 个','success')}).catch(()=>{}).finally(()=>{loadConfig();loadStats();setTimeout(()=>window.scrollTo(0,sy),100)})}
async function loadConfig(){const sy=window.scrollY;try{const r=await fetch('/api/config');const cfg=await r.json();document.getElementById('force_disable_thinking').checked=cfg.force_disable_thinking||false;aliasData=cfg.model_alias||{};effortData=cfg.reasoning_effort_map||{};socks5Data=cfg.socks5_proxies||[];const mr=await fetch('/v1/models');const md=await mr.json();modelList=(md.data||[]).map(m=>m.id).sort();renderAliasTable();renderEffortTable();renderSocks5Table();document.getElementById('activeSocks5').value=cfg.active_socks5||'';setTimeout(()=>window.scrollTo(0,sy),0)}catch(e){showToast('失败: '+e.message,'error')}}
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
async function saveConfig(){collectAliases();collectEfforts();collectSocks5();const cfg={model_alias:aliasData,reasoning_effort_map:effortData,force_disable_thinking:document.getElementById('force_disable_thinking').checked,socks5_proxies:socks5Data,active_socks5:document.getElementById('activeSocks5').value};try{const r=await fetch('/api/config',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(cfg)});if(!r.ok)throw new Error(await r.text());showToast('配置已保存','success');loadConfig()}catch(e){showToast('保存失败: '+e.message,'error')}}
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
	flag.BoolVar(&showVersion, "version", false, "显示版本信息")
	flag.Parse()

	if showVersion {
		fmt.Println(versionString())
		return
	}

	cfg := loadConfig(configPath)
	applyConfig(cfg)
	if err := saveConfig(configPath, cfg); err != nil {
		log.Printf("警告: 无法保存配置: %v", err)
	}

	loadTokenStats()
	log.Printf("配置已从 %s 加载", configPath)
	initOCSession()
	models, err := fetchModels()
	if err != nil {
		log.Printf("警告: 无法获取模型列表: %v", err)
	} else {
		modelMu.Lock()
		modelsCache = models
		modelsLoaded = true
		modelMu.Unlock()
		log.Printf("已加载 %d 个模型:", len(models))
		for _, m := range models {
			log.Printf("  - %s", m.ID)
		}
	}
	startModelRefresh()
	log.Printf("OPENCODE TO API 代理服务器")
	log.Printf("===================")
	log.Printf("端口:     %s", port)
	log.Printf("上游:     https://opencode.ai/zen/v1/chat/completions (API) 或 /go/ (付费)")
	log.Printf("模型：  %d 个模型已加载", len(getModelIDs()))
	log.Printf("别名：  %d", len(modelAlias))
	if adminPassword != "" {
		log.Printf("管理面板: http://localhost:%s/ （密码认证已启用）", port)
	} else {
		log.Printf("管理面板: http://localhost:%s/ （无密码）", port)
	}
	log.Printf("===================")
	http.HandleFunc("/v1/chat/completions", chatCompletionsHandler)
	http.HandleFunc("/v1/responses", responsesHandler)
	http.HandleFunc("/v1/messages", claudeMessagesHandler)
	http.HandleFunc("/v1/models", listModelsHandler)
	http.HandleFunc("/login", loginHandler)
	http.HandleFunc("/logout", logoutHandler)
	http.HandleFunc("/api/config", requireAuth(adminConfigHandler))
	http.HandleFunc("/api/stats", requireAuth(adminStatsHandler))
	http.HandleFunc("/api/reload", requireAuth(reloadHandler))
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			requireAuth(adminPageHandler)(w, r)
			return
		}
		http.NotFound(w, r)
	})
	addr := ":" + port
	log.Printf("服务器启动在 %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}
