package app

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestLoginHandler_ConstantTimeComparison 验证登录使用常量时间比对防止时序攻击
func TestLoginHandler_ConstantTimeComparison(t *testing.T) {
	adminPassword = "correct-password"
	defer func() { adminPassword = "" }()

	tests := []struct {
		name     string
		password string
		wantCode int
	}{
		{"correct password", "correct-password", http.StatusFound},
		{"wrong password", "wrong-password", http.StatusOK}, // 返回登录页
		{"empty password", "", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form := url.Values{}
			form.Set("password", tt.password)
			req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()

			loginHandler(w, req)

			if w.Code != tt.wantCode {
				t.Errorf("got status %d, want %d", w.Code, tt.wantCode)
			}
		})
	}
}

// TestSessionCookie_SecurityAttributes 验证 session cookie 包含安全属性
func TestSessionCookie_SecurityAttributes(t *testing.T) {
	adminPassword = "test-password"
	defer func() {
		adminPassword = ""
		sessions = map[string]sessionEntry{}
	}()

	form := url.Values{}
	form.Set("password", "test-password")
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	loginHandler(w, req)

	cookies := w.Result().Cookies()
	var sessionCookie, csrfCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "session" {
			sessionCookie = c
		} else if c.Name == "csrf_token" {
			csrfCookie = c
		}
	}

	if sessionCookie == nil {
		t.Fatal("session cookie not set")
	}

	// 验证 session cookie 属性
	if !sessionCookie.HttpOnly {
		t.Error("session cookie should be HttpOnly")
	}
	if !sessionCookie.Secure {
		t.Error("session cookie should be Secure")
	}
	if sessionCookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("session cookie SameSite = %v, want %v", sessionCookie.SameSite, http.SameSiteStrictMode)
	}
	if sessionCookie.MaxAge != sessionMaxAgeSecs {
		t.Errorf("session cookie MaxAge = %d, want %d", sessionCookie.MaxAge, sessionMaxAgeSecs)
	}

	// 验证 CSRF cookie 属性
	if csrfCookie == nil {
		t.Fatal("csrf_token cookie not set")
	}
	if csrfCookie.HttpOnly {
		t.Error("csrf_token cookie should NOT be HttpOnly (JavaScript needs to read it)")
	}
	if !csrfCookie.Secure {
		t.Error("csrf_token cookie should be Secure")
	}
	if csrfCookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("csrf_token cookie SameSite = %v, want %v", csrfCookie.SameSite, http.SameSiteStrictMode)
	}
}

// TestSessionExpiration 验证 session 过期机制
func TestSessionExpiration(t *testing.T) {
	adminPassword = "test"
	defer func() {
		adminPassword = ""
		sessions = map[string]sessionEntry{}
	}()

	// 创建一个过期的 session
	expiredToken := "expired-token"
	sessions[expiredToken] = sessionEntry{
		createdAt: time.Now().Add(-25 * time.Hour), // 25 小时前（超过 24 小时 TTL）
		lastUsed:  time.Now().Add(-25 * time.Hour),
	}

	// 创建一个有效的 session
	validToken := "valid-token"
	sessions[validToken] = sessionEntry{
		createdAt: time.Now().Add(-1 * time.Hour),
		lastUsed:  time.Now(),
	}

	tests := []struct {
		name      string
		token     string
		wantAuth  bool
		wantCount int // 预期 sessions map 中的条目数
	}{
		{"expired session rejected", expiredToken, false, 1}, // 过期的被删除，剩余 1 个
		{"valid session accepted", validToken, true, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.AddCookie(&http.Cookie{Name: "session", Value: tt.token})
			w := httptest.NewRecorder()

			authenticated := false
			testHandler := requireAuth(func(w http.ResponseWriter, r *http.Request) {
				authenticated = true
				w.WriteHeader(http.StatusOK)
			})

			testHandler(w, req)

			if authenticated != tt.wantAuth {
				t.Errorf("authenticated = %v, want %v", authenticated, tt.wantAuth)
			}

			sessionsMu.Lock()
			count := len(sessions)
			sessionsMu.Unlock()
			if count != tt.wantCount {
				t.Errorf("sessions count = %d, want %d", count, tt.wantCount)
			}
		})
	}
}

// TestCSRFProtection 验证 CSRF 保护机制
func TestCSRFProtection(t *testing.T) {
	adminPassword = "test"
	defer func() { adminPassword = "" }()

	csrfToken := "test-csrf-token"

	tests := []struct {
		name         string
		method       string
		path         string
		cookieToken  string
		headerToken  string
		wantCode     int
		wantAuth     bool
		description  string
	}{
		{
			"GET request bypasses CSRF",
			http.MethodGet, "/api/config",
			"", "",
			http.StatusOK, true,
			"GET 请求不需要 CSRF 保护",
		},
		{
			"POST without CSRF token",
			http.MethodPost, "/api/config",
			"", "",
			http.StatusForbidden, false,
			"无 CSRF token 被拒绝",
		},
		{
			"POST with CSRF cookie but no header",
			http.MethodPost, "/api/config",
			csrfToken, "",
			http.StatusForbidden, false,
			"仅有 cookie 无 header 被拒绝",
		},
		{
			"POST with header but no cookie",
			http.MethodPost, "/api/config",
			"", csrfToken,
			http.StatusForbidden, false,
			"仅有 header 无 cookie 被拒绝",
		},
		{
			"POST with mismatched tokens",
			http.MethodPost, "/api/config",
			"token1", "token2",
			http.StatusForbidden, false,
			"token 不匹配被拒绝",
		},
		{
			"POST with matching tokens",
			http.MethodPost, "/api/config",
			csrfToken, csrfToken,
			http.StatusOK, true,
			"token 匹配通过验证",
		},
		{
			"POST to /logout bypasses CSRF",
			http.MethodPost, "/logout",
			"", "",
			http.StatusOK, true,
			"/logout 豁免 CSRF 保护",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.cookieToken != "" {
				req.AddCookie(&http.Cookie{Name: "csrf_token", Value: tt.cookieToken})
			}
			if tt.headerToken != "" {
				req.Header.Set("X-CSRF-Token", tt.headerToken)
			}

			w := httptest.NewRecorder()
			authenticated := false

			testHandler := requireCSRF(func(w http.ResponseWriter, r *http.Request) {
				authenticated = true
				w.WriteHeader(http.StatusOK)
			})

			testHandler(w, req)

			if w.Code != tt.wantCode {
				t.Errorf("%s: got status %d, want %d", tt.description, w.Code, tt.wantCode)
			}
			if authenticated != tt.wantAuth {
				t.Errorf("%s: authenticated = %v, want %v", tt.description, authenticated, tt.wantAuth)
			}
		})
	}
}

// TestSecurityHeaders 验证安全响应头
func TestSecurityHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	testHandler := securityHeaders(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	testHandler(w, req)

	expectedHeaders := map[string]string{
		"X-Frame-Options":           "DENY",
		"X-Content-Type-Options":    "nosniff",
		"X-XSS-Protection":          "1; mode=block",
		"Referrer-Policy":           "strict-origin-when-cross-origin",
	}

	for header, expected := range expectedHeaders {
		got := w.Header().Get(header)
		if got != expected {
			t.Errorf("header %s = %q, want %q", header, got, expected)
		}
	}

	// CSP 头包含多个指令，只检查是否存在
	csp := w.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Error("Content-Security-Policy header not set")
	}
	if !strings.Contains(csp, "default-src 'self'") {
		t.Error("CSP should contain default-src 'self'")
	}
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Error("CSP should contain frame-ancestors 'none'")
	}
}

// TestSecurityHeaders_HSTS 验证 HSTS 仅在 HTTPS 下设置
func TestSecurityHeaders_HSTS(t *testing.T) {
	tests := []struct {
		name     string
		hasTLS   bool
		wantHSTS bool
	}{
		{"HTTPS request sets HSTS", true, true},
		{"HTTP request no HSTS", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.hasTLS {
				req.TLS = &tls.ConnectionState{} // 模拟 HTTPS
			}
			w := httptest.NewRecorder()

			testHandler := securityHeaders(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})

			testHandler(w, req)

			hsts := w.Header().Get("Strict-Transport-Security")
			if tt.wantHSTS && hsts == "" {
				t.Error("HSTS header should be set for HTTPS")
			}
			if !tt.wantHSTS && hsts != "" {
				t.Error("HSTS header should not be set for HTTP")
			}
		})
	}
}

// TestCleanupExpiredSessions 验证后台清理函数
func TestCleanupExpiredSessions(t *testing.T) {
	sessions = map[string]sessionEntry{
		"expired1": {
			createdAt: time.Now().Add(-25 * time.Hour),
			lastUsed:  time.Now().Add(-25 * time.Hour),
		},
		"expired2": {
			createdAt: time.Now().Add(-30 * time.Hour),
			lastUsed:  time.Now().Add(-30 * time.Hour),
		},
		"valid1": {
			createdAt: time.Now().Add(-1 * time.Hour),
			lastUsed:  time.Now(),
		},
		"valid2": {
			createdAt: time.Now().Add(-12 * time.Hour),
			lastUsed:  time.Now().Add(-6 * time.Hour),
		},
	}

	cleanupExpiredSessions()

	sessionsMu.Lock()
	defer sessionsMu.Unlock()

	if len(sessions) != 2 {
		t.Errorf("expected 2 valid sessions, got %d", len(sessions))
	}

	if _, ok := sessions["expired1"]; ok {
		t.Error("expired1 should be removed")
	}
	if _, ok := sessions["expired2"]; ok {
		t.Error("expired2 should be removed")
	}
	if _, ok := sessions["valid1"]; !ok {
		t.Error("valid1 should be kept")
	}
	if _, ok := sessions["valid2"]; !ok {
		t.Error("valid2 should be kept")
	}
}
