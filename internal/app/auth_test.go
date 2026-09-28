package app

import (
	"net/http"
	"testing"
)

func TestIsValidOpenCodeKeyOcSk(t *testing.T) {
	cases := []struct {
		token string
		want  bool
	}{
		{"oc_sk_61bcccca210d_FNPhvD5AIcGQyVk1nNPpPVjeebvek69i", true},
		{"oc_sk-abcdef1234567890", true},
		{"sk-abcdefghijklmnopq", true},
		{"sk-ant-abcdefghijk", false},
		{"sk-", false},
		{"oc_sk-", false},
		{"public", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isValidOpenCodeKey(c.token); got != c.want {
			t.Errorf("isValidOpenCodeKey(%q) = %v, want %v", c.token, got, c.want)
		}
	}
}

// adminPassword 命中时应直接走 AuthRouteAdmin，不再按 sk- 校验；常量时间比对。
func TestExtractUpstreamAuth_AdminPasswordTriggersPool(t *testing.T) {
	old := adminPassword
	adminPassword = "panel-secret"
	defer func() { adminPassword = old }()

	r, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	r.Header.Set("Authorization", "Bearer panel-secret")
	auth := extractUpstreamAuth(r)
	if auth.Mode != AuthRouteAdmin || auth.Source != "admin" || auth.Token != "" {
		t.Fatalf("admin bearer did not map to AuthRouteAdmin: %+v", auth)
	}
	// 不应误命中：完全不一样的 token、多一字节、少一字节
	for _, bad := range []string{"panel-secret2", "panel-secre", "public", ""} {
		r2, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
		if bad != "" {
			r2.Header.Set("Authorization", "Bearer "+bad)
		}
		auth2 := extractUpstreamAuth(r2)
		if auth2.Mode == AuthRouteAdmin {
			t.Errorf("token %q unexpectedly treated as admin", bad)
		}
	}
	// adminPassword 为空时永不触发
	adminPassword = ""
	r3, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	r3.Header.Set("Authorization", "Bearer panel-secret")
	if auth3 := extractUpstreamAuth(r3); auth3.Mode == AuthRouteAdmin {
		t.Fatal("empty adminPassword should never match admin route")
	}
}

// AuthRouteAdmin 在池启用且有候选时必须被 selectPoolKey 接管（不为 public）。
func TestSelectPoolKey_AdminAutoPicked(t *testing.T) {
	keypoolMu.Lock()
	oldCfg, oldEntries, oldState := keypoolCfg, keypoolEntries, keypoolState
	keypoolCfg = KeyPool{Enabled: true, Strategy: "sticky"}
	keypoolEntries = []UpstreamKey{{ID: "k1", Key: "oc_sk_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	keypoolState = map[string]*keypoolEntryState{}
	keypoolMu.Unlock()
	defer func() {
		keypoolMu.Lock()
		keypoolCfg, keypoolEntries, keypoolState = oldCfg, oldEntries, oldState
		keypoolMu.Unlock()
	}()

	auth, keyID, pooled := selectPoolKey(UpstreamAuth{Mode: AuthRouteAdmin, Source: "admin"}, "big-pickle", nil, nil, "", 0)
	if !pooled || keyID != "k1" {
		t.Fatalf("admin auth should be picked from pool: pooled=%v keyID=%q auth=%+v", pooled, keyID, auth)
	}
	if auth.Mode != AuthRouteAdmin {
		t.Fatalf("returned auth should keep Mode=AuthRouteAdmin, got %v", auth.Mode)
	}
}

// 默认 strategy 应为 sticky（缓存亲和），显式配置不被覆盖。
func TestNormalizeKeyPool_DefaultsToSticky(t *testing.T) {
	got := normalizeKeyPool(KeyPool{Enabled: true, Keys: []UpstreamKey{{Key: "oc_sk_abcdefghijklmnopq"}}})
	if got.Strategy != "sticky" {
		t.Fatalf("default strategy = %q, want sticky", got.Strategy)
	}
	got2 := normalizeKeyPool(KeyPool{Enabled: true, Strategy: "round_robin", Keys: []UpstreamKey{{Key: "oc_sk_abcdefghijklmnopq"}}})
	if got2.Strategy != "round_robin" {
		t.Fatalf("explicit strategy overwritten: %q", got2.Strategy)
	}
}
