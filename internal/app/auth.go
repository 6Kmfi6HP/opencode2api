package app

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/6Kmfi6HP/opencode2api/internal/modelsdev"
)

// ======================== 管理面板认证 ========================

const (
	sessionTTL        = 24 * time.Hour // 会话 24 小时后过期
	sessionMaxAgeSecs = 86400          // 24 小时（cookie MaxAge）
)

type sessionEntry struct {
	createdAt time.Time
	lastUsed  time.Time
}

var (
	adminPassword string
	sessions      = map[string]sessionEntry{}
	sessionsMu    sync.Mutex
)

func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if adminPassword == "" {
			// 无密码=不启用面板鉴权，原样放行。launch 默认 adminPassword=""
			// （server.go/launch.go 尚未写变量）→ /api/* 见 server.go mux 注册。
			next(w, r)
			return
		}
		// 保留原始查询串（如 ?lang=en）：登录页 i18n.js 的语言解析优先读取 URL ?lang=。
		dest := "/login"
		if r.URL.RawQuery != "" {
			dest += "?" + r.URL.RawQuery
		}
		cookie, err := r.Cookie("session")
		if err != nil || cookie.Value == "" {
			http.Redirect(w, r, dest, http.StatusFound)
			return
		}
		sessionsMu.Lock()
		entry, ok := sessions[cookie.Value]
		if ok {
			// 检查是否过期（以最后使用时间为基准，与 cleanupExpiredSessions 保持一致）
			if time.Since(entry.lastUsed) > sessionTTL {
				delete(sessions, cookie.Value)
				ok = false
			} else {
				// 更新最后使用时间（滑动窗口）
				entry.lastUsed = time.Now()
				sessions[cookie.Value] = entry
			}
		}
		sessionsMu.Unlock()
		if !ok {
			http.Redirect(w, r, dest, http.StatusFound)
			return
		}
		next(w, r)
	}
}

// adminAPIEnabled 报告管理 API（/login /logout /api/*）是否注册。
// 仅当 adminPassword 非空时才启用；launch 模式通过不注册这些路由把
// /api/config /api/key_* 等口子全部关闭（避免无密码的本地代理被同网段/同机
// 攻击者改 keypool）。
func adminAPIEnabled() bool {
	return adminPassword != ""
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// generateCSRFToken 创建加密安全的 CSRF token
func generateCSRFToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// isHTTPS 判断请求是否通过 HTTPS（含反向代理场景）。
// 浏览器对 localhost 豁免 Secure cookie 要求，所以 HTTP localhost 也能正常工作；
// 但我们的 Secure 标志只影响是否发送，不影响功能本身，因此简单判断即可。
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	if adminPassword == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			// 传 i18n 字典 key（web/i18n.js），login.html 端用 t(key) 渲染，
			// 服务端不再注入中文文案。
			renderLoginPage(w, "login.err.parse")
			return
		}
		passwordInput := r.FormValue("password")
		if subtle.ConstantTimeCompare([]byte(passwordInput), []byte(adminPassword)) != 1 {
			renderLoginPage(w, "login.err.password")
			return
		}
		token, err := generateToken()
		if err != nil {
			renderLoginPage(w, "login.err.session")
			return
		}
		now := time.Now()
		sessionsMu.Lock()
		sessions[token] = sessionEntry{
			createdAt: now,
			lastUsed:  now,
		}
		sessionsMu.Unlock()
		http.SetCookie(w, &http.Cookie{
			Name:     "session",
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			Secure:   isHTTPS(r),
			SameSite: http.SameSiteStrictMode,
			MaxAge:   sessionMaxAgeSecs,
		})
		// 设置 CSRF token cookie（与 session 分离）
		csrfToken, err := generateCSRFToken()
		if err != nil {
			renderLoginPage(w, "login.err.session")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     "csrf_token",
			Value:    csrfToken,
			Path:     "/",
			HttpOnly: false, // JavaScript 需要读取
			Secure:   isHTTPS(r),
			SameSite: http.SameSiteStrictMode,
			MaxAge:   sessionMaxAgeSecs,
		})
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
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "csrf_token",
		Value:    "",
		Path:     "/",
		HttpOnly: false,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/login", http.StatusFound)
}

// startSessionCleanup 启动后台 goroutine，定期清理过期 session
func startSessionCleanup() {
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			cleanupExpiredSessions()
		}
	}()
}

func cleanupExpiredSessions() {
	now := time.Now()
	sessionsMu.Lock()
	defer sessionsMu.Unlock()

	for token, entry := range sessions {
		// 以最后使用时间为基准（滑动窗口）：活跃会话不因创建早而过期
		if now.Sub(entry.lastUsed) > sessionTTL {
			delete(sessions, token)
		}
	}
}

// requireCSRF 验证 CSRF token（双重提交 cookie 模式）
func requireCSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if adminPassword == "" {
			next(w, r)
			return
		}

		// GET 请求不需要 CSRF 保护
		if r.Method == http.MethodGet {
			next(w, r)
			return
		}

		// /logout 豁免 CSRF（即使被 CSRF 也只是登出用户，无害）
		if r.URL.Path == "/logout" {
			next(w, r)
			return
		}

		// 提取 cookie 中的 CSRF token
		cookie, err := r.Cookie("csrf_token")
		if err != nil || cookie.Value == "" {
			http.Error(w, "CSRF token missing", http.StatusForbidden)
			return
		}

		// 提取 header 中的 CSRF token
		headerToken := r.Header.Get("X-CSRF-Token")
		if headerToken == "" {
			http.Error(w, "CSRF token missing in header", http.StatusForbidden)
			return
		}

		// 常量时间比对 token
		if subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(headerToken)) != 1 {
			http.Error(w, "CSRF token mismatch", http.StatusForbidden)
			return
		}

		next(w, r)
	}
}

// securityHeaders 为所有管理路由添加安全响应头
func securityHeaders(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 防止点击劫持
		w.Header().Set("X-Frame-Options", "DENY")

		// 防止 MIME 嗅探
		w.Header().Set("X-Content-Type-Options", "nosniff")

		// XSS 保护（旧版浏览器）
		w.Header().Set("X-XSS-Protection", "1; mode=block")

		// 内容安全策略：允许 Google Fonts（admin.html/login.html 使用）
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' 'unsafe-inline'; "+
				"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; "+
				"font-src 'self' https://fonts.gstatic.com; "+
				"img-src 'self' data:; "+
				"connect-src 'self'; "+
				"frame-ancestors 'none'")

		// HSTS（仅 HTTPS）
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security",
				"max-age=31536000; includeSubDomains")
		}

		// Referrer 策略
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

		next(w, r)
	}
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
	// AuthRouteAdmin 匹配 adminPassword（panel 密码兼作 API key）。提取时
	// 命中即不再比对真实 sk-，池 selectPoolKey 强制接管（design-keypool §2）。
	AuthRouteAdmin
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
	// admin 密码作为池触发 token：常量时间比对防时序侧信道；命中 → 强制池
	// 接管（Mode=AuthRouteAdmin），不进入下游 sk- 校验。
	if adminPassword != "" && subtle.ConstantTimeCompare([]byte(token), []byte(adminPassword)) == 1 {
		return UpstreamAuth{Mode: AuthRouteAdmin, Source: "admin"}
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

// isValidOpenCodeKey 只认 opencode 自己的 key 前缀（sk- 与 oc_sk- 都属同一
// 发行网关，后者为线上运营实际格式）；Anthropic sk-ant- 及过短占位串
// （no-key-required / placeholder 等）一律拒绝，回落 public。
func isValidOpenCodeKey(token string) bool {
	if strings.HasPrefix(token, "sk-ant-") {
		return false
	}
	if !strings.HasPrefix(token, "sk-") && !strings.HasPrefix(token, "oc_sk-") && !strings.HasPrefix(token, "oc_sk_") {
		return false
	}
	return len(token) > len("sk-")+10 // 至少 3 位前缀 + 11 字节熵，防占位
}

func (auth UpstreamAuth) tier() TierType {
	if auth.Mode == AuthRoutePublic {
		return TierFree
	}
	return TierPaid
}

func (auth UpstreamAuth) authorizationHeader() string {
	return "Bearer " + auth.apiKey()
}

func (auth UpstreamAuth) apiKey() string {
	if auth.Mode == AuthRoutePublic {
		return "public"
	}
	return auth.Token
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

// isFreeModel 判断模型是否属于免费模型：既包括以 "-free" 结尾的显式免费
// 变体，也包括 models.dev 目录中全部费用为零的上游免费模型（如 big-pickle）。
func isFreeModel(modelID string) bool {
	base, _ := stripContextSuffix(modelID)
	if strings.HasSuffix(base, "-free") {
		return true
	}
	return modelsdev.IsFreeModel(base)
}

// publicFacingModelID strips the upstream "-free" suffix for client-visible catalogs.
func publicFacingModelID(modelID string) string {
	if isFreeModel(modelID) {
		return strings.TrimSuffix(modelID, "-free")
	}
	return modelID
}

// mapPublicToFreeModel downgrades a paid model ID to its "-free" variant
// for public/free-tier auth. Context suffixes like "[1m]" are preserved: the
// suffix is stripped before the "-free" lookup, then re-appended to the
// resolved free ID (e.g. "deepseek-v4-flash[1m]" → "deepseek-v4-flash-free[1m]").
func mapPublicToFreeModel(auth UpstreamAuth, modelID string) string {
	base, suffix := stripContextSuffix(modelID)
	if auth.Mode != AuthRoutePublic || isFreeModel(base) {
		return modelID
	}
	if freeID := base + "-free"; modelExistsInCaches(freeID) {
		return freeID + suffix
	}
	return modelID
}

func modelExistsInCaches(modelID string) bool {
	modelMu.RLock()
	defer modelMu.RUnlock()
	return containsModelWithID(modelsCache, modelID) || containsModelWithID(goModelsCache, modelID)
}
