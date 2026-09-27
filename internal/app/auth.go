package app

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"

	"github.com/6Kmfi6HP/opencode2api/internal/modelsdev"
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
			// 无密码=不启用面板鉴权，原样放行。launch 默认 adminPassword=""
			// （server.go/launch.go 尚未写变量）→ /api/* 见 server.go mux 注册。
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
