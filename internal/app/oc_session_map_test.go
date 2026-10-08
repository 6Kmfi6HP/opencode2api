package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// withOCSessionMapEnv 保存并替换映射表开关与表内容,结束后恢复。enabled=false
// 时设 OPENCODE2API_OC_SESSION_MAP=off(回退全局单一 session);enabled=true
// 时清掉该 env(默认开启)。
func withOCSessionMapEnv(t *testing.T, enabled bool) {
	t.Helper()
	old, had := os.LookupEnv("OPENCODE2API_OC_SESSION_MAP")
	if enabled {
		_ = os.Unsetenv("OPENCODE2API_OC_SESSION_MAP")
	} else {
		_ = os.Setenv("OPENCODE2API_OC_SESSION_MAP", "off")
	}
	ocSessionMapMu.Lock()
	oldMap := ocSessionMap
	ocSessionMap = map[string]*ocSessionMapping{}
	ocSessionMapMu.Unlock()
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("OPENCODE2API_OC_SESSION_MAP", old)
		} else {
			_ = os.Unsetenv("OPENCODE2API_OC_SESSION_MAP")
		}
		ocSessionMapMu.Lock()
		ocSessionMap = oldMap
		ocSessionMapMu.Unlock()
	})
}

// clearStickyEntriesForTest 清空 sticky 出口绑定: sticky entry 缓存的是创建
// 时刻的 httpClient,映射测试连续多次解析会命中旧 fake 客户端导致 502。
func clearStickyEntriesForTest() {
	stickyMu.Lock()
	stickyEntries = map[string]*stickyProxyEntry{}
	stickyMu.Unlock()
}

// resolveUpstreamSessionForTest 通过 callOpenCodeEndpoint 发一次假上游请求,
// 返回上游实际收到的 x-opencode-session 值(headers 非空时先经
// withClientHeadersFromRequest 快照进 context,与真实入口一致)。
func resolveUpstreamSessionForTest(t *testing.T, headers http.Header, bodyJSON string) string {
	t.Helper()
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: `{"id":"chatcmpl_t","choices":[]}`},
	})
	clearStickyEntriesForTest()
	ctx := context.Background()
	if headers != nil {
		r := (&http.Request{Header: headers.Clone()}).WithContext(ctx)
		ctx = withClientHeadersFromRequest(r).Context()
	}
	rc, status, _, err := callOpenCodeEndpoint(ctx, "chat/completions", []byte(bodyJSON), "fallback-model-free", UpstreamAuth{Mode: AuthRoutePublic})
	if err != nil || status != http.StatusOK {
		t.Fatalf("callOpenCodeEndpoint status = %d, err = %v", status, err)
	}
	rc.Close()
	if len(transport.requestHeaders) != 1 {
		t.Fatalf("want 1 upstream attempt, got %d", len(transport.requestHeaders))
	}
	return transport.requestHeaders[0].Get(headerOpencodeSession)
}

var ocSessionRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// TestOCSessionMap_StableForSameDownstreamKey 同一下游身份两次解析必须命中
// 同一专属 session(稳定),且不是全局 ses_test、符合上游 ses_ 格式。
func TestOCSessionMap_StableForSameDownstreamKey(t *testing.T) {
	withOCSessionMapEnv(t, true)
	setDefaultOCSessionStateForTest()
	cases := []struct {
		name    string
		headers http.Header
		body    string
	}{
		{"claude session header", http.Header{"X-Claude-Code-Session-Id": []string{"down-a"}}, `{"model":"fallback-model-free","messages":[]}`},
		{"codex thread header", http.Header{"Thread-Id": []string{"down-b"}}, `{"model":"fallback-model-free","messages":[]}`},
		{"body user field", nil, `{"model":"fallback-model-free","messages":[],"user":"down-c"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			first := resolveUpstreamSessionForTest(t, c.headers, c.body)
			second := resolveUpstreamSessionForTest(t, c.headers, c.body)
			if first == "" || first != second {
				t.Fatalf("same downstream key must resolve to one session, got %q vs %q", first, second)
			}
			if first == "ses_test" {
				t.Fatalf("mapped session must not be the global one, got %q", first)
			}
			if !ocSessionRe.MatchString(first) {
				t.Fatalf("mapped session %q does not match upstream client format", first)
			}
		})
	}
}

// TestOCSessionMap_IsolatesDifferentSessions 不同下游会话头(含下划线拼写与
// body user)必须各自分到不同专属 session;同值不同大小写的头命中同一映射。
func TestOCSessionMap_IsolatesDifferentSessions(t *testing.T) {
	withOCSessionMapEnv(t, true)
	setDefaultOCSessionStateForTest()
	cases := []struct {
		name    string
		headers http.Header
	}{
		{"claude session", http.Header{"X-Claude-Code-Session-Id": []string{"down-1"}}},
		{"thread hyphen", http.Header{"Thread-Id": []string{"down-2"}}},
		{"thread underscore", http.Header{"Thread_Id": []string{"down-3"}}},
		{"session hyphen", http.Header{"Session-Id": []string{"down-4"}}},
		{"session underscore", http.Header{"Session_Id": []string{"down-5"}}},
	}
	seen := map[string]string{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sess := resolveUpstreamSessionForTest(t, c.headers, `{"model":"fallback-model-free","messages":[]}`)
			if prev, dup := seen[sess]; dup {
				t.Fatalf("session %q shared between %q and %q", sess, prev, c.name)
			}
			seen[sess] = c.name
		})
	}
	// 大小写不敏感: 同值头换拼写仍命中同一映射键。
	canon := resolveUpstreamSessionForTest(t, http.Header{"X-Claude-Code-Session-Id": []string{"down-mix"}}, `{"model":"fallback-model-free","messages":[]}`)
	lower := resolveUpstreamSessionForTest(t, http.Header{"x-claude-code-session-id": []string{"down-mix"}}, `{"model":"fallback-model-free","messages":[]}`)
	if canon != lower {
		t.Fatalf("case-insensitive header spelling must share one mapping, got %q vs %q", canon, lower)
	}
}

// TestOCSessionMap_ExplicitSessionWins 下游显式 x-opencode-session 优先于
// 映射并原样透传,不产生映射条目。
func TestOCSessionMap_ExplicitSessionWins(t *testing.T) {
	withOCSessionMapEnv(t, true)
	setDefaultOCSessionStateForTest()
	// 真实入口的 http.Header 键已规范化,这里用规范拼写让
	// withClientHeadersFromRequest 的 Get 命中。
	headers := http.Header{
		"X-Opencode-Session":       []string{"ses_explicit1234567890abcdef"},
		"X-Claude-Code-Session-Id": []string{"down-explicit"},
	}
	sess := resolveUpstreamSessionForTest(t, headers, `{"model":"fallback-model-free","messages":[]}`)
	if sess != "ses_explicit1234567890abcdef" {
		t.Fatalf("explicit x-opencode-session must pass through verbatim, got %q", sess)
	}
	if n := ocSessionMapSize(); n != 0 {
		t.Fatalf("explicit session must not create mapping entries, got %d", n)
	}
}

// TestOCSessionMap_NoSignalFallsBackToGlobal 无任何下游信号走全局 session,
// 映射表保持为空。
func TestOCSessionMap_NoSignalFallsBackToGlobal(t *testing.T) {
	withOCSessionMapEnv(t, true)
	setDefaultOCSessionStateForTest()
	sess := resolveUpstreamSessionForTest(t, nil, `{"model":"fallback-model-free","messages":[]}`)
	if sess != "ses_test" {
		t.Fatalf("no-signal request must use the global session, got %q", sess)
	}
	if n := ocSessionMapSize(); n != 0 {
		t.Fatalf("no-signal request must not create mapping entries, got %d", n)
	}
}

// TestOCSessionMap_DisabledByEnv OPENCODE2API_OC_SESSION_MAP=off 回退现状:
// 全部流量用全局 session,不进映射。
func TestOCSessionMap_DisabledByEnv(t *testing.T) {
	withOCSessionMapEnv(t, false)
	setDefaultOCSessionStateForTest()
	sess := resolveUpstreamSessionForTest(t, http.Header{"X-Claude-Code-Session-Id": []string{"down-off"}}, `{"model":"fallback-model-free","messages":[]}`)
	if sess != "ses_test" {
		t.Fatalf("disabled mapping must fall back to the global session, got %q", sess)
	}
	if n := ocSessionMapSize(); n != 0 {
		t.Fatalf("disabled mapping must not create entries, got %d", n)
	}
}

// TestDownstreamSessionKey_Signals 键派生单元表: 头信号哈希、body user 兜底、
// token 作用域前缀防跨账号串扰、无信号返回空。
func TestDownstreamSessionKey_Signals(t *testing.T) {
	hex16 := `[0-9a-f]{16}`
	cases := []struct {
		name    string
		auth    UpstreamAuth
		body    map[string]any
		headers http.Header
		want    string
	}{
		{"header signal hashed", UpstreamAuth{}, nil, http.Header{"X-Claude-Code-Session-Id": []string{"s"}}, "^" + hex16 + "$"},
		{"underscore header matches", UpstreamAuth{}, nil, http.Header{"Thread_Id": []string{"s"}}, "^" + hex16 + "$"},
		{"body user hashed", UpstreamAuth{}, map[string]any{"user": "u"}, nil, "^" + hex16 + "$"},
		{"header wins over user", UpstreamAuth{}, map[string]any{"user": "u"}, http.Header{"Session-Id": []string{"s"}}, "^" + hex16 + "$"},
		{"no signal empty", UpstreamAuth{}, map[string]any{}, nil, "^$"},
		{"blank user ignored", UpstreamAuth{}, map[string]any{"user": "  "}, nil, "^$"},
		{"token scoped", UpstreamAuth{Token: "sk-1"}, map[string]any{"user": "u"}, nil, `^tok:` + hex16 + `\|` + hex16 + `$`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := downstreamSessionKey(c.auth, c.body, c.headers)
			if !regexp.MustCompile(c.want).MatchString(got) {
				t.Fatalf("downstreamSessionKey = %q, want match %q", got, c.want)
			}
		})
	}
	// 同值不同大小写的头 → 同一 key(复用 sessionHeaderValue 的大小写不敏感)。
	canon := downstreamSessionKey(UpstreamAuth{}, nil, http.Header{"X-Claude-Code-Session-Id": []string{"s"}})
	lower := downstreamSessionKey(UpstreamAuth{}, nil, http.Header{"x-claude-code-session-id": []string{"s"}})
	if canon != lower || canon == "" {
		t.Fatalf("header case must not change the key, got %q vs %q", canon, lower)
	}
	// 同一下游信号挂不同账号 token → 不同 key(防跨账号同名串扰)。
	tok1 := downstreamSessionKey(UpstreamAuth{Token: "sk-1"}, map[string]any{"user": "u"}, nil)
	tok2 := downstreamSessionKey(UpstreamAuth{Token: "sk-2"}, map[string]any{"user": "u"}, nil)
	if tok1 == tok2 {
		t.Fatalf("same signal under different tokens must not collide, got %q", tok1)
	}
}

// TestOCSessionMap_LRUEvictsLeastRecentlyUsed 满时淘汰最久未用条目,表保持
// 有界 256(对齐 stickyMaxEntries)。
func TestOCSessionMap_LRUEvictsLeastRecentlyUsed(t *testing.T) {
	withOCSessionMapEnv(t, true)
	ocSessionMapMu.Lock()
	ocSessionMap = map[string]*ocSessionMapping{}
	base := time.Now().Add(-time.Minute)
	for i := 0; i < ocSessionMapMaxEntries; i++ {
		ocSessionMap[fmt.Sprintf("k%03d", i)] = &ocSessionMapping{sessionID: newOCSessionID(), lastUsed: base}
	}
	ocSessionMap["k_lru"] = &ocSessionMapping{sessionID: "ses_oldest0000000000ab", lastUsed: base.Add(-time.Hour)}
	ocSessionMapMu.Unlock()

	got := lookupOrCreateOCSession("k_new")
	if !ocSessionRe.MatchString(got) {
		t.Fatalf("new mapping session %q has wrong format", got)
	}
	ocSessionMapMu.Lock()
	_, lruGone := ocSessionMap["k_lru"]
	_, newPresent := ocSessionMap["k_new"]
	n := len(ocSessionMap)
	ocSessionMapMu.Unlock()
	if lruGone {
		t.Fatalf("least recently used entry must be evicted first")
	}
	if !newPresent {
		t.Fatalf("new entry must be inserted after eviction")
	}
	if n != ocSessionMapMaxEntries {
		t.Fatalf("map size = %d, want bounded %d", n, ocSessionMapMaxEntries)
	}
}

// TestOCSessionMap_TTLSlidingRenewal TTL 滑动续期: 未过期条目 lookup 后续期
// 并保持同一 session;超期条目懒清理后重建为新 session。
func TestOCSessionMap_TTLSlidingRenewal(t *testing.T) {
	withOCSessionMapEnv(t, true)
	ocSessionMapMu.Lock()
	ocSessionMap = map[string]*ocSessionMapping{}
	ocSessionMapMu.Unlock()

	first := lookupOrCreateOCSession("k_ttl")
	ocSessionMapMu.Lock()
	ocSessionMap["k_ttl"].lastUsed = time.Now().Add(-ocSessionMapTTL + time.Minute)
	ocSessionMapMu.Unlock()
	if renewed := lookupOrCreateOCSession("k_ttl"); renewed != first {
		t.Fatalf("entry within sliding TTL must keep its session, got %q vs %q", first, renewed)
	}

	ocSessionMapMu.Lock()
	ocSessionMap["k_ttl"].lastUsed = time.Now().Add(-ocSessionMapTTL - time.Minute)
	ocSessionMapMu.Unlock()
	if rebuilt := lookupOrCreateOCSession("k_ttl"); rebuilt == first {
		t.Fatalf("expired entry must be rebuilt with a new session, got %q", rebuilt)
	}
}

// TestOCSessionMap_RefreshClears refreshOCSession 换全局世代时清空映射表。
func TestOCSessionMap_RefreshClears(t *testing.T) {
	withOCSessionMapEnv(t, true)
	// 假上游让 fetchOCVersion 拿默认版本(bodyless GET 被 fake 拒),刷新全程不触网。
	installFakeOpenCodeClient(t, nil)
	lookupOrCreateOCSession("k_refresh")
	if n := ocSessionMapSize(); n != 1 {
		t.Fatalf("seeded map size = %d, want 1", n)
	}
	refreshOCSession()
	if n := ocSessionMapSize(); n != 0 {
		t.Fatalf("refreshOCSession must clear the mapping table, got %d entries", n)
	}
}

// TestOCSessionMap_ConcurrentLookupSameKey 并发同键解析必须拿到同一专属
// session(锁内创建防并发重复),-race 下跑。
func TestOCSessionMap_ConcurrentLookupSameKey(t *testing.T) {
	withOCSessionMapEnv(t, true)
	const n = 32
	results := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = lookupOrCreateOCSession("k_race")
		}(i)
	}
	wg.Wait()
	for i, s := range results {
		if s != results[0] || s == "" {
			t.Fatalf("concurrent lookup %d = %q, want %q", i, s, results[0])
		}
	}
}

// TestOCSessionMap_SystemOneHashScope 顺手统一后 systemone 路径传给
// selectUpstreamTarget 的 ocScope 是哈希形式(不再透传裸 sessionID):
// 观察点为 sticky 出口表的实际 key。
func TestOCSessionMap_SystemOneHashScope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":[]}`))
	}))
	defer upstream.Close()
	withStickyProxyEnv(t, nil, "", true, false)
	withBaseURLs(t, []string{upstream.URL})
	setDefaultOCSessionStateForTest()

	w := httptest.NewRecorder()
	systemoneHandler(w, httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(`{"model":"jev-1.13-free"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("systemone handler status = %d, body %s", w.Code, w.Body.String())
	}

	stickyMu.Lock()
	keys := make([]string, 0, len(stickyEntries))
	for k := range stickyEntries {
		keys = append(keys, k)
	}
	stickyMu.Unlock()
	if len(keys) != 1 {
		t.Fatalf("want 1 sticky entry, got %v", keys)
	}
	if !regexp.MustCompile(`^cli://public-shared\|oc:[0-9a-f]{16}$`).MatchString(keys[0]) {
		t.Fatalf("systemone ocScope must be hashed, got sticky key %q", keys[0])
	}
}

// TestOCSessionMap_BuildOCRequestHashScope 统一后 buildOCRequest(无请求
// 上下文的 models 构建路径)传 selectUpstreamTarget 的 ocScope 同为哈希形式。
func TestOCSessionMap_BuildOCRequestHashScope(t *testing.T) {
	withStickyProxyEnv(t, nil, "", true, false)
	withBaseURLs(t, []string{"https://opencode.ai", "https://zen.example"})
	setDefaultOCSessionStateForTest()
	clearStickyEntriesForTest()

	req, err := buildOCRequest("fallback-model-free", map[string]any{"messages": []any{}}, UpstreamAuth{})
	if err != nil {
		t.Fatal(err)
	}
	if req == nil {
		t.Fatal("request must be built")
	}

	stickyMu.Lock()
	keys := make([]string, 0, len(stickyEntries))
	for k := range stickyEntries {
		keys = append(keys, k)
	}
	stickyMu.Unlock()
	if len(keys) != 1 {
		t.Fatalf("want 1 sticky entry, got %v", keys)
	}
	if !regexp.MustCompile(`^cli://public-shared\|oc:[0-9a-f]{16}$`).MatchString(keys[0]) {
		t.Fatalf("buildOCRequest ocScope must be hashed, got sticky key %q", keys[0])
	}
}
