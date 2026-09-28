package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func resetPool(t *testing.T) {
	t.Helper()
	setKeyPool(KeyPool{})
	t.Cleanup(func() { setKeyPool(KeyPool{}) })
}

func TestKeyPool_RoundRobin(t *testing.T) {
	resetPool(t)
	// Strategy 默认是 "sticky"（design P0 更新）；本测试显式 round_robin 以验证老逻辑。
	setKeyPool(KeyPool{Enabled: true, Strategy: "round_robin", Keys: []UpstreamKey{{Key: "k-a"}, {Key: "k-b"}, {Key: "k-c"}}})
	auth := UpstreamAuth{Mode: AuthRouteAuto, Token: "client"}
	var got []string
	for range 6 {
		_, id, ok := selectPoolKey(auth, "m", nil, nil, "")
		if !ok {
			t.Fatal("want ok")
		}
		got = append(got, id)
	}
	want := []string{"k1", "k2", "k3", "k1", "k2", "k3"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order[%d] = %s, want %s (%v)", i, got[i], want[i], got)
		}
	}
	// Pooled auth replaces client token.
	a, _, _ := selectPoolKey(auth, "m", nil, nil, "")
	if a.Token == "client" {
		t.Fatal("pooled auth must replace client token")
	}
}

func TestKeyPool_Weighted(t *testing.T) {
	resetPool(t)
	setKeyPool(KeyPool{Enabled: true, Strategy: "weighted", Keys: []UpstreamKey{
		{ID: "a", Key: "ka", Weight: 3},
		{ID: "b", Key: "kb", Weight: 1},
	}})
	auth := UpstreamAuth{Mode: AuthRouteAuto, Token: "c"}
	counts := map[string]int{}
	for range 8 {
		_, id, ok := selectPoolKey(auth, "m", nil, nil, "")
		if !ok {
			t.Fatal("want ok")
		}
		counts[id]++
	}
	if counts["a"] != 6 || counts["b"] != 2 {
		t.Fatalf("weighted distribution = %v, want a=6 b=2", counts)
	}
}

func TestKeyPool_Sticky(t *testing.T) {
	resetPool(t)
	setKeyPool(KeyPool{Enabled: true, Strategy: "sticky", Keys: []UpstreamKey{{Key: "ka"}, {Key: "kb"}}})
	a1 := UpstreamAuth{Mode: AuthRouteAuto, Token: "user1"}
	a2 := UpstreamAuth{Mode: AuthRouteAuto, Token: "user2"}
	_, id1a, _ := selectPoolKey(a1, "m", nil, nil, "")
	_, id1b, _ := selectPoolKey(a1, "m", nil, nil, "")
	if id1a != id1b {
		t.Fatal("sticky must return same key for same session")
	}
	// 同一 token、不同客户端会话必须散开（会话级负载均衡）。
	hdr := func(sess string) http.Header {
		return http.Header{headerClaudeSession: []string{sess}}
	}
	seen := map[string]bool{id1a: true}
	for _, sess := range []string{"sess-a", "sess-b", "sess-c", "sess-d", "sess-e", "sess-f", "sess-g", "sess-h"} {
		_, id, _ := selectPoolKey(a1, "m", nil, hdr(sess), "")
		seen[id] = true
	}
	if len(seen) < 2 {
		t.Fatalf("same token with different sessions must spread across pool, got only %v", seen)
	}
	// 同一会话必须稳定命中同一 key（会话内缓存亲和）。
	_, idA1, _ := selectPoolKey(a1, "m", nil, hdr("sess-a"), "")
	_, idA2, _ := selectPoolKey(a1, "m", nil, hdr("sess-a"), "")
	if idA1 != idA2 {
		t.Fatal("sticky must return same key for same session headers")
	}
	// failover 重试必须跳离首选 key。
	_, idR, _ := selectPoolKey(a1, "m", nil, hdr("sess-a"), "", 1)
	if idR == idA1 {
		t.Fatalf("pool retry should hash away from first-attempt key %q", idA1)
	}
	if got := stickySessionBase(a1, nil, nil, "", false); got != "tok:user1" {
		t.Fatalf("stickySessionBase = %q", got)
	}
	if got, want := stickySessionBase(a1, nil, hdr("sess-a"), "", false), "tok:user1|cli:"+hashSessionRouteKey("sess-a"); got != want {
		t.Fatalf("stickySessionBase with session = %q, want %q", got, want)
	}
	if got := stickySessionBase(UpstreamAuth{}, nil, nil, "", false); got != "sess:" {
		t.Fatalf("token-less fallback = %q", got)
	}
	if got := stickySessionBase(a1, nil, hdr("sess-a"), "", true); got != "tok:user1|cli:"+hashSessionRouteKey("sess-a")+keyPoolRetrySuffix {
		t.Fatalf("stickySessionBase retry = %q", got)
	}
	// Admin（无 token）请求：不同网关会话必须散开（会话后缀驱动均衡）。
	admin := UpstreamAuth{Mode: AuthRouteAdmin, Source: "admin"}
	seenAdmin := map[string]bool{}
	for _, sc := range []string{"scope-1", "scope-2", "scope-3", "scope-4", "scope-5", "scope-6", "scope-7", "scope-8"} {
		_, id, _ := selectPoolKey(admin, "m", nil, nil, sc)
		seenAdmin[id] = true
	}
	if len(seenAdmin) < 2 {
		t.Fatalf("token-less admin requests with different scopes must spread, got only %v", seenAdmin)
	}
	// 同一 token-less 会话必须稳定命中同一 key。
	_, idS1, _ := selectPoolKey(admin, "m", nil, nil, "scope-1")
	_, idS2, _ := selectPoolKey(admin, "m", nil, nil, "scope-1")
	if idS1 != idS2 {
		t.Fatal("sticky must return same key for same token-less scope")
	}
	_ = a2
}

func TestKeyPool_Disabled(t *testing.T) {
	resetPool(t)
	setKeyPool(KeyPool{Enabled: false, Keys: []UpstreamKey{{Key: "ka"}}})
	if poolEnabled() {
		t.Fatal("pool must be disabled")
	}
	if _, _, ok := selectPoolKey(UpstreamAuth{Mode: AuthRouteAuto, Token: "c"}, "m", nil, nil, ""); ok {
		t.Fatal("disabled pool must return ok=false")
	}
	setKeyPool(KeyPool{Enabled: true})
	if poolEnabled() {
		t.Fatal("empty pool must report disabled")
	}
}

func TestKeyPool_CooldownSkip(t *testing.T) {
	resetPool(t)
	setKeyPool(KeyPool{Enabled: true, CooldownSecs: 60, Keys: []UpstreamKey{{ID: "a", Key: "ka"}, {ID: "b", Key: "kb"}}})
	reportKeyResult("a", 429, nil)
	auth := UpstreamAuth{Mode: AuthRouteAuto, Token: "c"}
	for range 4 {
		_, id, ok := selectPoolKey(auth, "m", nil, nil, "")
		if !ok || id != "b" {
			t.Fatalf("cooling key must be skipped, got %q ok=%v", id, ok)
		}
	}
	// All cooling → earliest expiry still serves.
	reportKeyResult("b", 500, nil)
	if _, _, ok := selectPoolKey(auth, "m", nil, nil, ""); !ok {
		t.Fatal("all-cooldown must fall back to earliest expiry, not fail")
	}
	// Success clears fails.
	reportKeyResult("a", 200, nil)
	st := keyPoolStatus()
	for _, r := range st {
		if r.ID == "a" && r.ConsecutiveFails != 0 {
			t.Fatalf("success must clear fails: %+v", r)
		}
	}
	// 401 → long cooldown.
	reportKeyResult("a", 401, nil)
	st = keyPoolStatus()
	for _, r := range st {
		if r.ID == "a" && !r.InCooldown {
			t.Fatal("401 must trigger long cooldown")
		}
	}
	// Billing body → long cooldown.
	reportKeyResult("b", 402, nil, []byte(`{"error":{"type":"CreditsError","message":"x"}}`))
	// transport error with blacklist_after=1 → long cooldown path exercised.
	setKeyPool(KeyPool{Enabled: true, CooldownSecs: 60, BlacklistAfter: 1, Keys: []UpstreamKey{{ID: "a", Key: "ka"}}})
	reportKeyResult("a", 0, errors.New("dial"))
	st = keyPoolStatus()
	if !st[0].InCooldown {
		t.Fatal("transport error must cool down")
	}
}

func TestKeyPool_GroupFilter(t *testing.T) {
	resetPool(t)
	setKeyPool(KeyPool{Enabled: true, Keys: []UpstreamKey{
		{ID: "z", Key: "kz", Group: "zen"},
		{ID: "g", Key: "kg", Group: "go"},
		{ID: "both", Key: "kb"},
	}})
	zenAuth := UpstreamAuth{Mode: AuthRouteZen, Token: "c"}
	goAuth := UpstreamAuth{Mode: AuthRouteGo, Token: "c"}
	// zen surface: go-only key must never be picked.
	for range 10 {
		_, id, ok := selectPoolKey(zenAuth, "some-model", nil, nil, "")
		if !ok {
			t.Fatal("want ok")
		}
		if id == "g" {
			t.Fatal("zen surface picked go-only key")
		}
	}
	// Seed catalogs: "go-only-model" exists only in the go catalog.
	oldModels, oldGo := modelsCache, goModelsCache
	modelMu.Lock()
	modelsCache = []ModelInfo{{ID: "shared-model"}}
	goModelsCache = []ModelInfo{{ID: "shared-model"}, {ID: "go-only-model"}}
	modelMu.Unlock()
	t.Cleanup(func() {
		modelMu.Lock()
		modelsCache, goModelsCache = oldModels, oldGo
		modelMu.Unlock()
	})
	goModel := "go-only-model"
	for range 10 {
		_, id, ok := selectPoolKey(goAuth, goModel, nil, nil, "")
		if !ok {
			t.Fatal("want ok")
		}
		if id == "z" {
			t.Fatal("go surface picked zen-only key")
		}
	}
}

func TestKeyPool_PublicNeverPooled(t *testing.T) {
	resetPool(t)
	setKeyPool(KeyPool{Enabled: true, Keys: []UpstreamKey{{Key: "ka"}}})
	if _, _, ok := selectPoolKey(UpstreamAuth{Mode: AuthRoutePublic}, "m", nil, nil, ""); ok {
		t.Fatal("public must never use pool")
	}
}

func TestKeyPool_StringShorthand(t *testing.T) {
	var p KeyPool
	if err := json.Unmarshal([]byte(`{"enabled":true,"keys":["sk-a",{"id":"x","key":"sk-b"}]}`), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Keys) != 2 || p.Keys[0].Key != "sk-a" || p.Keys[1].ID != "x" {
		t.Fatalf("shorthand unmarshal: %+v", p.Keys)
	}
}

func TestKeyPool_NormalizeDedup(t *testing.T) {
	p := normalizeKeyPool(KeyPool{Keys: []UpstreamKey{
		{Key: "ka"},
		{Key: "kb"},
		{ID: "k1", Key: "dup"},
		{Key: ""},
		{ID: "z", Key: "kz", Weight: 0, Group: " ZEN "},
	}})
	if len(p.Keys) != 3 {
		t.Fatalf("want 3 entries, got %+v", p.Keys)
	}
	ids := map[string]bool{}
	for _, k := range p.Keys {
		if ids[k.ID] {
			t.Fatalf("dup id %q", k.ID)
		}
		ids[k.ID] = true
		if k.Weight < 1 {
			t.Fatalf("weight default missing: %+v", k)
		}
	}
	if p.Keys[2].Group != "zen" {
		t.Fatalf("group normalize: %+v", p.Keys[2])
	}
}

func TestKeyPool_FailoverBillingLongCooldown(t *testing.T) {
	resetPool(t)
	setKeyPool(KeyPool{Enabled: true, CooldownSecs: 60, Keys: []UpstreamKey{{ID: "a", Key: "ka"}, {ID: "b", Key: "kb"}}})
	reportKeyResult("a", 402, nil, []byte(`{"error":{"type":"CreditsError","message":"insufficient credits"}}`))
	st := keyPoolStatus()
	for _, r := range st {
		if r.ID == "a" {
			if !r.InCooldown {
				t.Fatal("billing error must trigger cooldown")
			}
			if r.CooldownRemainingSecs < 500 {
				t.Fatalf("billing error must use 15min cooldown, got %d secs", r.CooldownRemainingSecs)
			}
		}
	}
}

func TestKeyPool_Failover429CooldownSecs(t *testing.T) {
	resetPool(t)
	setKeyPool(KeyPool{Enabled: true, CooldownSecs: 60, Keys: []UpstreamKey{{ID: "a", Key: "ka"}}})
	reportKeyResult("a", 429, nil)
	st := keyPoolStatus()
	if !st[0].InCooldown {
		t.Fatal("429 must trigger cooldown")
	}
	if st[0].CooldownRemainingSecs > 65 || st[0].CooldownRemainingSecs <= 0 {
		t.Fatalf("429 must use cooldown_secs window, got %d", st[0].CooldownRemainingSecs)
	}
}

func TestKeyPool_FailoverSuccessClearsFails(t *testing.T) {
	resetPool(t)
	setKeyPool(KeyPool{Enabled: true, CooldownSecs: 60, Keys: []UpstreamKey{{ID: "a", Key: "ka"}}})
	reportKeyResult("a", 500, nil)
	reportKeyResult("a", 200, nil)
	st := keyPoolStatus()
	if st[0].ConsecutiveFails != 0 {
		t.Fatalf("success must clear fails: %+v", st[0])
	}
}

func TestKeyPool_FailoverAttemptsExhausted(t *testing.T) {
	resetPool(t)
	setKeyPool(KeyPool{Enabled: true, MaxRetries: 2, Keys: []UpstreamKey{{ID: "a", Key: "ka"}}})
	if keypoolMaxAttempts() != 3 {
		t.Fatalf("default pool attempts must be 3, got %d", keypoolMaxAttempts())
	}
	if keypoolAttemptsExhausted(0) || keypoolAttemptsExhausted(1) {
		t.Fatal("attempts 0,1 must not be exhausted with max 3")
	}
	if !keypoolAttemptsExhausted(2) {
		t.Fatal("attempt 2 must be exhausted with max 3")
	}
}
