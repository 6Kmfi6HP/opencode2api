package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/6Kmfi6HP/opencode2api/internal/logging"
	"github.com/6Kmfi6HP/opencode2api/internal/modelsdev"
	"github.com/6Kmfi6HP/opencode2api/internal/random"
)

// ======================== 随机 ID ========================

func randomString(n int) string {
	return random.String(n)
}

func randomHex(n int) string {
	return random.Hex(n)
}

// ======================== OpenCode 会话 ========================

const (
	headerOpencodeSession  = "x-opencode-session"
	headerOCSessionID      = "x-session-id"
	headerOCSessionAffnity = "x-session-affinity"
)

// newOCSessionShapeID 生成上游免费层校验格式的 26 字符 ID 主体:
// 12 位小写 hex + 14 位 base62（lite 版 opencode2api-lite.go L498-514）。
// 上游对免费层校验 session 必须匹配 ^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$,否则拒绝:
// "OpenCode's free tier can only be used from within OpenCode"。
// 取舍: 先前实现把时间戳反转编码进 hex 前缀(TypeID-descending),以贴近真实
// 客户端的排序行为;lite 实测该字段仅做格式正则校验、不含时间语义,纯随机即可。
// 这里按 lite 简化为全随机,不再保留 descending 版本作双头之一——真正的
// 兼容手段是新旧 session 头(x-session-id / x-opencode-session)同时发送。
func newOCSessionShapeID() string {
	const hexChars = "0123456789abcdef"
	const alnumChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	var b [26]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 不可用时退化为时间戳混合,仍保持格式合规(长度/字符集正确)
		var seed [8]byte
		binary.BigEndian.PutUint64(seed[:], uint64(time.Now().UnixNano()))
		for i := range b {
			b[i] = seed[i%8] ^ byte(i*37)
		}
	}
	for i := 0; i < 12; i++ {
		b[i] = hexChars[b[i]%16]
	}
	for i := 12; i < 26; i++ {
		b[i] = alnumChars[b[i]%byte(len(alnumChars))]
	}
	return string(b[:])
}

func newOCSessionID() string {
	return "ses_" + newOCSessionShapeID()
}

func newOCRequestID() string {
	return "msg_" + newOCSessionShapeID()
}

type opencodeSessionContextKey struct{}

type opencodeUpstreamHeadersContextKey struct{}

func sessionFromRequestContext(ctx context.Context, fallback string) string {
	if ctx == nil {
		return fallback
	}
	if session, ok := ctx.Value(opencodeSessionContextKey{}).(string); ok && strings.TrimSpace(session) != "" {
		return strings.TrimSpace(session)
	}
	return fallback
}

func withSessionFromRequest(r *http.Request) *http.Request {
	headers := r.Header.Clone()
	session := strings.TrimSpace(headers.Get(headerOpencodeSession))
	ctx := context.WithValue(r.Context(), opencodeUpstreamHeadersContextKey{}, headers)
	if session == "" {
		return r.WithContext(ctx)
	}
	return r.WithContext(context.WithValue(ctx, opencodeSessionContextKey{}, session))
}

func upstreamHeadersFromContext(ctx context.Context) http.Header {
	if ctx == nil {
		return nil
	}
	headers, _ := ctx.Value(opencodeUpstreamHeadersContextKey{}).(http.Header)
	return headers
}

// normalizedTransportScope derives the routing scope for sticky upstream
// selection. The scope is intentionally opaque and hashed below: the downstream
// client's own x-opencode-session may contain identity-ish data and must not
// be used verbatim as a local log/routing key.
func normalizedTransportScope(session string) string {
	session = strings.TrimSpace(session)
	if session == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(session))
	return fmt.Sprintf("%x", sum[:8])
}

var (
	ocSessionID string
	ocProjectID string
	ocClientVer string
	ocOnce      sync.Once
)

const (
	// ocMinFreeTierVersion 是上游免费层声明的最低客户端版本;低于它时上游
	// 返回 426 UpgradeRequired("OpenCode 1.18.0 or newer is required").
	// 取舍(2026-09-18 实测校准): lite opencode2api-lite.go L486-489 记为
	// minOCVersion = "1.17.0",但 issue #19 的实测消融
	// (docs/labs/2026-09-18-fingerprint-ablation.md E17/E18/U1-U5)表明
	// 上游阈值实际已上移到 1.18.0 —— 1.17.0/1.17.9 均 426,1.18.0+ 才 200。
	// 故下限采实测值 1.18.0 而非 lite 的 1.17.0。
	ocMinFreeTierVersion = "1.18.0"
	// ocDefaultFreeTierVersion 是 npm 拉取失败时的回退版本,高于下限
	// 且贴近实测当前上游最新版(2026-09-18 npm latest = 1.18.31),留一份冗余。
	ocDefaultFreeTierVersion = "1.18.31"
)

// normalizeOCVersion 保证 UA 版本号不低于 ocMinFreeTierVersion,
// 避免上游对低版本 UA 返回 426 Upgrade Required(对齐 lite L516-522)。
func normalizeOCVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return ocDefaultFreeTierVersion
	}
	if compareOCVersion(version, ocMinFreeTierVersion) < 0 {
		return ocMinFreeTierVersion
	}
	return version
}

// compareOCVersion 逐段比较点分十进制版本号;a<b 返回 -1,相等 0,a>b 返回 1。
// 切分容忍预发布后缀(如 1.17.0-beta 仅首段数字有效)。
func compareOCVersion(a, b string) int {
	an, bn := ocVersionNumbers(a), ocVersionNumbers(b)
	for i := 0; i < len(an) || i < len(bn); i++ {
		var av, bv int
		if i < len(an) {
			av = an[i]
		}
		if i < len(bn) {
			bv = bn[i]
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

func ocVersionNumbers(v string) []int {
	parts := strings.FieldsFunc(v, func(r rune) bool {
		return r == '.' || r == '-' || r == '+'
	})
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		nums = append(nums, n)
	}
	return nums
}

func fetchOCVersion() string {
	req, _ := http.NewRequest("GET", "https://registry.npmjs.org/opencode-ai/latest", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := getHTTPClient().Do(req)
	if err != nil {
		return ocDefaultFreeTierVersion
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var info struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(body, &info) == nil && info.Version != "" {
		return normalizeOCVersion(info.Version)
	}
	return ocDefaultFreeTierVersion
}

func initOCSession() {
	ocOnce.Do(func() {
		ocClientVer = fetchOCVersion()
		ocSessionID = newOCSessionID()
		ocProjectID = randomHex(40)
		slog.Info("opencode version", "version", ocClientVer)
		slog.Info("session initialized", "session_id", ocSessionID)
		slog.Info("project initialized", "project_id", ocProjectID)
	})
}

func refreshOCSession() {
	ocClientVer = fetchOCVersion()
	ocSessionID = newOCSessionID()
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
	req, _ := http.NewRequest("GET", roundRobinBaseURL()+"/zen/v1/models", nil)
	req.Header.Set("Authorization", "Bearer public")
	session := strings.TrimSpace(sessionFromRequestContext(nil, ocSessionID))
	if session == "" {
		session = ocSessionID
	}
	if session == "" {
		session = newOCSessionID()
	}
	// 新门禁头 x-session-id 与旧 x-opencode-session 同值双发,理由见
	// buildOCRequestWithSubpath 中的取舍注释。
	req.Header.Set("x-opencode-session", session)
	req.Header.Set(headerOCSessionID, session)
	req.Header.Set(headerOCSessionAffnity, session)
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
	req, _ := http.NewRequest("GET", roundRobinBaseURL()+"/zen/go/v1/models", nil)
	req.Header.Set("Authorization", "Bearer public")
	session := strings.TrimSpace(sessionFromRequestContext(nil, ocSessionID))
	if session == "" {
		session = ocSessionID
	}
	if session == "" {
		session = newOCSessionID()
	}
	// 新门禁头 x-session-id 与旧 x-opencode-session 同值双发,理由见
	// buildOCRequestWithSubpath 中的取舍注释。
	req.Header.Set("x-opencode-session", session)
	req.Header.Set(headerOCSessionID, session)
	req.Header.Set(headerOCSessionAffnity, session)
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

func isModelInZenCatalog(modelID string) bool {
	modelMu.RLock()
	defer modelMu.RUnlock()
	return containsModelWithID(modelsCache, modelID)
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

// startModelRefresh 定时刷新模型列表（OpenCode 模型每 10 分钟，models.dev 每 1 小时）
func startModelRefresh() {
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		modelsDevTicker := time.NewTicker(1 * time.Hour)
		defer modelsDevTicker.Stop()

		for {
			select {
			case <-ticker.C:
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
			case <-modelsDevTicker.C:
				if _, err := modelsdev.RefreshCatalog(); err != nil {
					slog.Error("models.dev catalog refresh failed", "error", err)
				} else {
					slog.Info("models.dev catalog auto-refreshed")
				}
			}
		}
	}()
}

// left untouched.

func buildOCRequest(modelID string, bodyMap map[string]any, auth UpstreamAuth) (*http.Request, error) {
	baseURL, _ := selectUpstreamTarget(auth, bodyMap, nil, ocSessionID)
	return buildOCRequestWithSubpath(modelID, bodyMap, auth, auth.shouldUseGoEndpoint(modelID), baseURL, "chat/completions", ocSessionID)
}

func buildOCRequestWithEndpoint(modelID string, bodyMap map[string]any, auth UpstreamAuth, useGoEndpoint bool, baseURL string) (*http.Request, error) {
	return buildOCRequestWithSubpath(modelID, bodyMap, auth, useGoEndpoint, baseURL, "chat/completions", ocSessionID)
}

func buildOCRequestWithSubpath(modelID string, bodyMap map[string]any, auth UpstreamAuth, useGoEndpoint bool, baseURL string, subpath string, ocSession string) (*http.Request, error) {
	bodyMap["model"] = modelID
	// 上游 2026-09-18 实测门禁(docs/labs/2026-09-18-fingerprint-ablation.md):
	// 是否执行免费层指纹重做**只看解析后的上游模型是否免费**,与客户端
	// Authorization 是 "Bearer public" 还是真实 sk- key 无关(sk- key 下
	// 免费模型一样按同一指纹校验,muse-spark-*-contributor-free 的 500 则是
	// 档位对 public key 整档拒,与指纹无关)。三个上游协议 subpath
	// (chat/completions / messages / responses)统一走同一层,不再仅限
	// chat/completions。客户端语义上的非流式由 callOpenCodeAPI 的本地聚合
	// 还原(见 aggregateOpenAIStream)。
	applyFreeTierFingerprint(bodyMap, subpath, modelID)
	tryBody, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, err
	}
	var upstreamURL string
	if useGoEndpoint {
		upstreamURL = "https://opencode.ai/zen/go/v1/" + subpath
	} else {
		upstreamURL = baseURL + "/zen/v1/" + subpath
	}
	// 上游偶发对无 Accept-Encoding 的 Go 默认 gzip 响应不吐 identity,
	// 客户端拿到的会是乱码 gzip 二进制;显式 identity 避免歧义。
	req, err := http.NewRequest("POST", upstreamURL, bytes.NewReader(tryBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth.authorizationHeader())
	if subpath == "messages" {
		req.Header.Set("x-api-key", auth.apiKey())
	}
	// UA 对齐 lite L1931: ai-sdk/runtime 后缀贴近真实 opencode 客户端。
	uaVersion := ocClientVer
	if strings.TrimSpace(uaVersion) == "" {
		uaVersion = ocDefaultFreeTierVersion
	}
	uaVersion = normalizeOCVersion(uaVersion)
	req.Header.Set("User-Agent", fmt.Sprintf("opencode/%s ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14", uaVersion))
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-project", ocProjectID)
	if subpath == "messages" {
		// Anthropic Messages 上游需要版本头；流式时 Accept 切为 SSE。
		req.Header.Set("anthropic-version", "2023-06-01")
		if stream, _ := bodyMap["stream"].(bool); stream {
			req.Header.Set("Accept", "text/event-stream")
		} else {
			req.Header.Set("Accept", "application/json")
		}
	} else {
		req.Header.Set("Accept", "application/json")
	}
	session := sessionFromRequestContext(nil, ocSession)
	if strings.TrimSpace(session) == "" {
		session = ocSessionID
	}
	if strings.TrimSpace(session) == "" {
		session = newOCSessionID()
	}
	session = strings.TrimSpace(session)
	// 取舍(待主代理实测裁剪): lite 注释称上游已改查 x-session-id、旧
	// x-opencode-session 失效,且 lite 每请求发随机 id;本仓库有 sticky
	// egress 设计(同一会话固定出口代理,靠稳定 session 维持 prompt 缓存命中),
	// 不能逐请求随机。这里双头同值: 新 x-session-id/x-session-affinity 供新门禁,
	// 旧 x-opencode-session 保留以兼容仍只认旧头的上游部署;两头发稳定值即可
	// 同时满足"新头生效"与"旧部署不回归"。
	req.Header.Set("x-opencode-session", session)
	req.Header.Set(headerOCSessionID, session)
	req.Header.Set(headerOCSessionAffnity, session)
	req.Header.Set("x-opencode-request", newOCRequestID())
	if subpath != "messages" {
		req.Header.Set("Accept", "application/json")
	}
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

// callOpenCodeEndpoint 统一封装所有对上游 /zen/v1/* 和 /zen/go/v1/* 端点的 HTTP 调用，
// 包含重试机制、SOCKS5 会话粘性与轮换、多域名轮换、错误归一与结构化日志输出。
func callOpenCodeEndpoint(ctx context.Context, endpointSubpath string, upstreamBody []byte, modelID string, auth UpstreamAuth) (io.ReadCloser, int, http.Header, error) {
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
	log := logging.FromContext(ctx)

	var lastErr error
	var retryCount int
	var lastBody []byte
	var lastStatus int
	var lastHeader http.Header
	var lastBaseURL string
	maxAttempts := maxUpstreamRetries
	if max401Retries > maxAttempts {
		maxAttempts = max401Retries
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// 仅"裸进程内回退值"才现造一次会话,避免同一请求的重试在
		// newOCSessionID() 兜底下换 session 破坏 sticky egress。
		ocSession := sessionFromRequestContext(ctx, ocSessionID)
		if strings.TrimSpace(ocSession) == "" {
			ocSession = newOCSessionID()
		}
		upstreamHeaders := upstreamHeadersFromContext(ctx)
		baseURL, client := selectUpstreamTarget(auth, bodyMap, upstreamHeaders, normalizedTransportScope(ocSession))
		lastBaseURL = baseURL
		up, err := buildOCRequestWithSubpath(modelID, bodyMap, auth, useGoEndpoint, baseURL, endpointSubpath, ocSession)
		if err != nil {
			return nil, 500, nil, err
		}
		up = up.WithContext(ctx)
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
				"base_url", baseURL,
				"surface", surface,
				"status", 0,
				"duration_ms", durationMs,
				"attempt_index", attempt,
				"retry_reason", retryReason,
				"error", err.Error(),
			)
			if canRetry {
				client.CloseIdleConnections()
				invalidateUpstreamTarget(auth, bodyMap, upstreamHeaders, sessionFromRequestContext(ctx, ocSessionID))
				retryCount++
				continue
			}
			break
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			log.Info("upstream_attempt",
				"try_model", modelID,
				"base_url", baseURL,
				"surface", surface,
				"status", resp.StatusCode,
				"duration_ms", durationMs,
				"attempt_index", attempt,
			)
			log.Info("upstream_result",
				"models_tried", []string{modelID},
				"base_url", baseURL,
				"retries", retryCount,
				"final_status", resp.StatusCode,
				"fallback_used", false,
			)
			return resp.Body, resp.StatusCode, resp.Header, nil
		}
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
		logging.UpstreamError(ctx, modelID, resp.StatusCode, errBody, baseURL)
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
			"base_url", baseURL,
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
		// 免费层 429 按出口 IP 限流,5xx 也可能是出口问题:
		// 重试前切断 sticky,让同一会话换到下一个出口。
		invalidateUpstreamTarget(auth, bodyMap, upstreamHeaders, sessionFromRequestContext(ctx, ocSessionID))
		client.CloseIdleConnections()
		retryCount++
	}
	log.Info("upstream_result",
		"models_tried", []string{modelID},
		"base_url", lastBaseURL,
		"retries", retryCount,
		"final_status", lastStatus,
		"fallback_used", false,
	)
	if lastStatus != 0 {
		return io.NopCloser(bytes.NewReader(lastBody)), lastStatus, lastHeader, nil
	}
	if lastErr != nil {
		return nil, 0, nil, lastErr
	}
	return nil, 0, nil, fmt.Errorf("upstream request failed")
}

// callOpenCodeAnthropicEndpoint 把请求发往上游原生 Anthropic Messages 端点
// （/zen/v1/messages 或 /zen/go/v1/messages），重试/粘性出口/多域名轮换与
// 结构化日志全部沿用 callOpenCodeEndpoint。
func callOpenCodeAnthropicEndpoint(ctx context.Context, upstreamBody []byte, modelID string, auth UpstreamAuth) (io.ReadCloser, int, http.Header, error) {
	return callOpenCodeEndpoint(ctx, "messages", upstreamBody, modelID, auth)
}

func callOpenCodeAPI(ctx context.Context, upstreamBody []byte, modelID string, auth UpstreamAuth) ([]byte, int, http.Header, error) {
	rc, status, header, err := callOpenCodeEndpoint(ctx, "chat/completions", upstreamBody, modelID, auth)
	if err != nil || status < 200 || status >= 300 {
		var errBody []byte
		if rc != nil {
			errBody, _ = io.ReadAll(rc)
			rc.Close()
		}
		if err == nil {
			err = fmt.Errorf("upstream error")
		}
		// 包装上游错误体，让下游 writeUpstreamError 能透出原始 message 而非
		// 笼统 "upstream error"。
		err = &upstreamBodyError{msg: err.Error(), body: errBody}
		return errBody, status, header, err
	}
	defer rc.Close()

	b, readErr := io.ReadAll(rc)
	if readErr != nil {
		return nil, 0, nil, readErr
	}
	// 顺序: 先判 Anthropic(含 SSE),再谈 OpenAI 聚合——免费层被强制
	// stream:true 后上游既可能按模型返回 OpenAI SSE,也可能仍回 Anthropic
	// 格式(部分 claude 系),前者需本地聚合,后者走既有转换,
	// convertAnthropicToOpenAI 自带 SSE 解析,别互相污染。
	if isAnthropicFormat(b) {
		converted, convErr := convertAnthropicToOpenAI(b, modelID)
		if convErr != nil {
			// Only anthropicProtocolError errors carry type/message;
			// non-typed conversion errors stay generic so
			// writeUpstreamError emits a safe default.
			return nil, http.StatusBadGateway, nil, convErr
		}
		b = converted
	} else {
		// OpenAI SSE(因强制 stream:true)聚合为完整 chat.completion;
		// 已是 JSON 或空体时 aggregateOpenAIStream 原样返回,天然幂等。
		b = aggregateOpenAIStream(b, modelID)
	}
	b = convertRawToolCallsInBody(b)
	return b, status, header, nil
}

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

// upstreamBodyError 携带上游 HTTP 错误体的 error，供 writeUpstreamError 在
// 无类型化错误（非 anthropicProtocolError）时也能透出原始 body 中的错误信息
// （message / type），而不是笼统的 "upstream error"。
type upstreamBodyError struct {
	msg  string
	body []byte
}

func (e *upstreamBodyError) Error() string { return e.msg }

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
	// 携带上游错误体时优先提取其 message（次选 type），避免仅返回 generic
	// "upstream error" 丢失根因（例如上游对 muse-spark contributor 档位整档
	// 500 的具体 JSON）。
	var ube *upstreamBodyError
	if errors.As(err, &ube) && message == "upstream error" {
		if msg := extractUpstreamErrorMessage(ube.body); msg != "" {
			message = msg
		}
		if et := extractUpstreamErrorType(ube.body); et != "" {
			errType = et
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

// extractUpstreamErrorMessage / extractUpstreamErrorType 从上游 JSON 错误体
// 提取 message / type 字段，OpenAI ({error:{message}}) 与 Anthropic
// ({type:"error",error:{...}}) 形状都兼容；非 JSON 或字段缺失时返回 ""。
func extractUpstreamErrorMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return ""
	}
	if em, ok := raw["error"].(map[string]any); ok {
		if m, ok := em["message"].(string); ok && m != "" {
			return m
		}
	}
	if m, ok := raw["message"].(string); ok && m != "" {
		return m
	}
	return ""
}

func extractUpstreamErrorType(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return ""
	}
	if em, ok := raw["error"].(map[string]any); ok {
		if t, ok := em["type"].(string); ok && t != "" {
			return t
		}
	}
	if t, ok := raw["type"].(string); ok && t != "" && t != "error" {
		return t
	}
	return ""
}

func callOpenCodeAPIStream(ctx context.Context, upstreamBody []byte, modelID string, auth UpstreamAuth) (io.ReadCloser, int, http.Header, error) {
	rc, status, header, err := callOpenCodeEndpoint(ctx, "chat/completions", upstreamBody, modelID, auth)
	if err != nil || status < 200 || status >= 300 {
		if status == 0 {
			status = 500
		}
		return rc, status, header, err
	}
	return wrapRawSSE(rc), status, header, nil
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
