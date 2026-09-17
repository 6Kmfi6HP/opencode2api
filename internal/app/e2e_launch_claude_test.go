package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestE2ELaunchClaude drives a real `opencode2api launch claude` subprocess
// (built from this tree) with the real claude CLI (Claude Code) as the agent
// client, against an in-process fake upstream speaking the Anthropic Messages
// SSE protocol. It asserts the full round trip:
//
//  1. claude in print mode (-p) emits a streaming /v1/messages request; the
//     proxy's protocol_rules ("anthropic/*" → anthropic) forward it natively to
//     the fake /zen/v1/messages endpoint, preserving the anthropics shape
//     (max_tokens present, stream=true, last message role=user with content
//     blocks, anthropic-version + Accept: text/event-stream headers).
//  2. Turn 1: fake upstream streams a tool_use block (read_file
//     {"path":"main.go"}, stop_reason=tool_use). Claude Code does not recognize
//     that (custom, non-builtin) tool name, so it closes the agent loop itself:
//     it emits an assistant echo + a user message carrying a tool_result
//     (is_error=true) block — proving the streamed tool_use SSE frames were
//     consumed end-to-end.
//  3. Turn 2: the fake upstream observes the tool_result block, then streams
//     the text "PROXY_OK" with stop_reason=end_turn + usage, which claude
//     accepts as completion and exits 0.
//  4. The captured SSE event order seen by claude is asserted
//     (message_start → content_block_start → content_block_delta →
//     content_block_stop → message_delta(stop_reason, usage) → message_stop).
//
// Requires the claude CLI on PATH (or ~/.local/bin/claude); skipped otherwise.
// Reuses the process helpers from e2e_launch_codex_test.go (freeTCPPort,
// buildE2EBinary, waitForProxyPort, writeJSONFile, copyFile, e2eChildEnv, tail).
func TestE2ELaunchClaude(t *testing.T) {
	_ = findE2EClaude(t) // skip early when claude CLI is absent
	binPath := buildE2EBinary(t)
	port := freeTCPPort(t)

	fake := newFakeAnthropicUpstream(t)

	tmpDir := t.TempDir()
	if keep := os.Getenv("OPENCODE2API_E2E_KEEP"); keep != "" {
		// 调试开关：保留代理/claude 的 stdout/stderr/proxy.log 到固定目录
		// （t.TempDir() 跑完即删，排障时改用此目录）。
		keepDir, err := os.MkdirTemp(keep, "claude-e2e-*")
		if err != nil {
			t.Fatalf("OPENCODE2API_E2E_KEEP mkdir: %v", err)
		}
		tmpDir = keepDir
		t.Logf("keeping artifacts in %s", keepDir)
	}
	cfgPath := filepath.Join(tmpDir, "config.json")
	cfg := map[string]any{
		"upstream_base_urls": []string{fake.srv.URL},
		"protocol_rules": []map[string]string{
			// The launch --model is the Anthropic-namespaced ID the rule hits first;
			// the wildcard catches Claude Code's small/fast side models (e.g.
			// claude-haiku-4-5 used for title/quota probes) so they also take the
			// anthropic passthrough instead of the chat translations path.
			{"pattern": "anthropic/*", "protocol": "anthropic"},
			{"pattern": "*", "protocol": "anthropic"},
		},
		"text_only_models": []string{},
	}
	writeJSONFile(t, cfgPath, cfg)
	modelsDevCache := filepath.Join(tmpDir, "modelsdev_cache.json")
	copyFile(t, "modelsdev_cache.json", modelsDevCache)

	proxyLogPath := filepath.Join(tmpDir, "opencode2api.log")
	cmd := exec.Command(binPath,
		"launch", "claude",
		"-port", fmt.Sprint(port),
		"--config", cfgPath,
		"--log-file", proxyLogPath,
		"--stats-file", filepath.Join(tmpDir, "stats.json"),
		"--model", "anthropic/claude-opus-4-6",
		"--",
		"-p", "Reply with the word PROXY_OK",
		"--verbose",
		"--dangerously-skip-permissions",
		"--output-format", "text",
	)
	cmd.Dir = tmpDir
	cmd.Env = e2eClaudeChildEnv(tmpDir, modelsDevCache)

	stdoutPath := filepath.Join(tmpDir, "claude.stdout.txt")
	stderrPath := filepath.Join(tmpDir, "claude.stderr.log")
	stdoutFile, err := os.Create(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile

	runErr := make(chan error, 1)
	go func() { runErr <- cmd.Run() }()

	waitForProxyPort(t, port)

	var exitErr error
	select {
	case exitErr = <-runErr:
	case <-time.After(120 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-runErr
		t.Fatalf("launch claude timed out after 120s (see %s, %s, %s)", stdoutPath, stderrPath, proxyLogPath)
	}
	_ = stdoutFile.Close()
	_ = stderrFile.Close()

	stdout, _ := os.ReadFile(stdoutPath)
	stderr, _ := os.ReadFile(stderrPath)
	proxyLog, _ := os.ReadFile(proxyLogPath)
	dump := func() {
		t.Helper()
		t.Logf("claude stdout (last 4KB):\n%s", tail(stdout, 4096))
		t.Logf("claude stderr (last 4KB):\n%s", tail(stderr, 4096))
		t.Logf("proxy log (last 4KB):\n%s", tail(proxyLog, 4096))
	}

	reqs := fake.Requests()
	t.Logf("fake upstream saw %d main /zen/v1/messages request(s), %d probe request(s)",
		len(reqs), fake.probes.Load())
	for i, rq := range reqs {
		t.Logf("turn %d: model=%q stream=%v max_tokens=%v roles=%v stop=%q",
			i, rq.Model, rq.Stream, rq.MaxTokens, rq.Roles, rq.StopReason)
	}

	if exitErr != nil {
		dump()
		t.Fatalf("launch claude exited with error: %v", exitErr)
	}

	if len(reqs) < 2 {
		dump()
		t.Fatalf("fake upstream saw %d main /zen/v1/messages request(s), want >= 2 "+
			"(second turn with tool_result proves claude consumed the streamed tool_use)",
			len(reqs))
	}

	// ---------- Turn 1: canonical Anthropic streaming request + tool_use SSE ----------
	first := reqs[0]
	if first.Path != "/zen/v1/messages" {
		t.Errorf("turn 1: path = %q, want /zen/v1/messages", first.Path)
	}
	if first.Model != "anthropic/claude-opus-4-6" {
		t.Errorf("turn 1: model = %q, want anthropic/claude-opus-4-6 (protocol_rules anthropic/* hit)",
			first.Model)
	}
	if !first.Stream {
		t.Errorf("turn 1: stream = %v, want true", first.Stream)
	}
	if first.MaxTokens <= 0 {
		t.Errorf("turn 1: max_tokens = %v, want > 0 (Anthropic schema requires it)", first.MaxTokens)
	}
	if got := first.AnthropicVersion; got != "2023-06-01" {
		t.Errorf("turn 1: anthropic-version = %q, want 2023-06-01", got)
	}
	if !strings.Contains(first.Accept, "text/event-stream") {
		t.Errorf("turn 1: Accept = %q, want text/event-stream", first.Accept)
	}
	if n := len(first.Roles); n == 0 || first.Roles[n-1] != "user" {
		t.Errorf("turn 1: role sequence = %v, want last role = user", first.Roles)
	}
	assertAnthropicEventsContain(t, "turn 1", first.EventTypes, []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	})
	if !first.SawToolUseBlock {
		t.Errorf("turn 1: no content_block{type:tool_use} observed; tool_use stream not relayed")
	}
	if first.StopReason != "tool_use" {
		t.Errorf("turn 1: message_delta stop_reason = %q, want tool_use", first.StopReason)
	}
	if first.UsageTokens <= 0 {
		t.Errorf("turn 1: usage tokens = %d, want > 0 from message_start/message_delta", first.UsageTokens)
	}

	// ---------- Turn 2: tool_result came back; final text streamed ----------
	second := reqs[1]
	if len(second.Roles) < 2 || second.Roles[len(second.Roles)-1] != "user" {
		t.Errorf("turn 2: role sequence = %v, want last role = user", second.Roles)
	}
	if !second.SawToolResult {
		dump()
		t.Fatalf("turn 2: no tool_result block in last user message; agent loop not closed "+
			"(roles=%v)", second.Roles)
	}
	if second.StopReason != "end_turn" {
		t.Errorf("turn 2: message_delta stop_reason = %q, want end_turn", second.StopReason)
	}
	assertAnthropicEventsContain(t, "turn 2", second.EventTypes, []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	})

	// ---------- claude finished its conversation with PROXY_OK ----------
	if !strings.Contains(string(stdout), "PROXY_OK") {
		dump()
		t.Fatalf("claude stdout did not contain final marker PROXY_OK")
	}
	if exitCode := cmd.ProcessState.ExitCode(); exitCode != 0 {
		dump()
		t.Fatalf("claude exit code = %d, want 0", exitCode)
	}

	t.Logf("OK: %d rounds done (tool round + final end_turn round), PROXY_OK on stdout", len(reqs))
}

// ======================== fake Anthropic upstream ========================

type fakeAnthropicReq struct {
	Path             string
	Model            string
	Stream           bool
	MaxTokens        float64
	AnthropicVersion string
	Accept           string
	Roles            []string
	SawToolUseBlock  bool     // 响应侧：我们写出的 tool_use 帧在流里出现
	SawToolResult    bool     // 请求侧：最后一条 user content 含 tool_result block
	EventTypes       []string // 服务端写出的 SSE event: 顺序
	StopReason       string
	UsageTokens      int // message_start + message_delta 中观察到的最大 token 值
}

type fakeAnthropicUpstream struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	reqs   []fakeAnthropicReq // 只收主对话请求（tools 非空或 messages>1）
	probes atomic.Int32       // Sage/预热等裸单文本请求
}

func newFakeAnthropicUpstream(t *testing.T) *fakeAnthropicUpstream {
	t.Helper()
	f := &fakeAnthropicUpstream{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAnthropicUpstream) Requests() []fakeAnthropicReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]fakeAnthropicReq, len(f.reqs))
	copy(cp, f.reqs)
	return cp
}

func (f *fakeAnthropicUpstream) handle(w http.ResponseWriter, r *http.Request) {
	// 旁路探测路径：模型目录 / count_tokens / 遥测 —— 一律 200 + 最小合法体，
	// 否则 claude 启动预热会反复重试、拖慢主对话。
	switch r.URL.Path {
	case "/zen/v1/models", "/zen/go/v1/models":
		w.Header().Set("Content-Type", "application/json")
		// 返回包含目标模型的清单，让 mapPublicToFreeModel / modelExistsInCaches
		// 保持 model 原样（否则 public key 会被降级成 ...-free）。
		io.WriteString(w, `{"data":[{"id":"anthropic/claude-opus-4-6"},{"id":"claude-haiku-4-5"}]}`)
		return
	case "/zen/v1/messages/count_tokens":
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"input_tokens":42}`)
		return
	default:
		p := r.URL.Path
		if strings.HasSuffix(p, "/event") || strings.HasSuffix(p, "/events") ||
			strings.HasSuffix(p, "/log") {
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	if r.URL.Path != "/zen/v1/messages" {
		f.t.Errorf("unexpected upstream path: %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"type":"error","error":{"type":"not_found_error","message":"unknown path"}}`,
			http.StatusNotFound)
		return
	}

	rec := fakeAnthropicReq{
		Path:             r.URL.Path,
		AnthropicVersion: r.Header.Get("anthropic-version"),
		Accept:           r.Header.Get("Accept"),
	}

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("decode body: %v", err)
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad json"}}`,
			http.StatusBadRequest)
		return
	}
	rec.Model, _ = body["model"].(string)
	rec.Stream, _ = body["stream"].(bool)
	rec.MaxTokens, _ = body["max_tokens"].(float64)
	msgs, _ := body["messages"].([]any)
	for _, m := range msgs {
		if mm, _ := m.(map[string]any); mm != nil {
			rec.Roles = append(rec.Roles, fmt.Sprint(mm["role"]))
		}
	}
	lastBlocks := lastUserContentBlocks(msgs)
	rec.SawToolResult = hasAnthropicBlockType(lastBlocks, "tool_result")

	tools, _ := body["tools"].([]any)
	isProbe := len(tools) == 0 && len(msgs) <= 1
	if isProbe {
		f.probes.Add(1)
		// 预热/标题等 side 请求：一句正常英文 end_turn 打发，不计入轮次。
		f.writeSSE(w, "msg_probe", rec.Model, []sseTextBlock{
			{text: "This is a straightforward check-in with an affirmative pull."},
		}, "end_turn", 3, 16)
		return
	}

	f.mu.Lock()
	f.reqs = append(f.reqs, rec)
	turn := len(f.reqs)
	f.mu.Unlock()

	if turn >= 2 || rec.SawToolResult {
		// ---------- 第二轮：claude 已回送 tool_result；写最终 end_turn 文本 ----------
		f.writeSSE(w, "msg_02", rec.Model, []sseTextBlock{
			{text: "PROXY_OK"},
		}, "end_turn", 24, 1)
		return
	}

	// ---------- 第一轮：tool_use(read_file {path: main.go}) ----------
	f.writeSSEWithToolUse(w, "msg_01", rec.Model, 12, 1)
}

// sseTextBlock 是 fake 上游写流的意图描述：text 走 content_block_delta(text_delta)；
// tool_use 由 writeSSEWithToolUse 单独处理（不走该通道）。
type sseTextBlock struct{ text string }

func (f *fakeAnthropicUpstream) writeSSE(w http.ResponseWriter, id, model string, blocks []sseTextBlock,
	stopReason string, inTok, outTok int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	var sb strings.Builder
	fmt.Fprintf(&sb, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":%q,\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":%d,\"output_tokens\":1}}}\n\n",
		id, model, inTok)
	for i, b := range blocks {
		fmt.Fprintf(&sb, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n", i)
		fmt.Fprintf(&sb, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n",
			i, b.text)
		fmt.Fprintf(&sb, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", i)
	}
	fmt.Fprintf(&sb, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q},\"usage\":{\"input_tokens\":%d,\"output_tokens\":%d}}\n\n",
		stopReason, inTok, outTok)
	sb.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	io.WriteString(w, sb.String())
	if flusher != nil {
		flusher.Flush()
	}

	f.mu.Lock()
	if n := len(f.reqs); n > 0 {
		last := &f.reqs[n-1]
		last.EventTypes = []string{
			"message_start", "content_block_start", "content_block_delta",
			"content_block_stop", "message_delta", "message_stop",
		}
		last.StopReason = stopReason
		last.UsageTokens = inTok + outTok
	}
	f.mu.Unlock()
}

// writeSSEWithToolUse 写一轮 tool_use 块流（read_file {"path":"main.go"}）。
func (f *fakeAnthropicUpstream) writeSSEWithToolUse(w http.ResponseWriter, id, model string, inTok, outTok int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	io.WriteString(w,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\""+id+
			"\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\""+model+
			"\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":"+itoa(inTok)+",\"output_tokens\":1}}}\n\n"+
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_01\",\"name\":\"read_file\",\"input\":{}}}\n\n"+
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"main.go\\\"}\"}}\n\n"+
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
	)
	if flusher != nil {
		flusher.Flush()
	}
	io.WriteString(w,
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"input_tokens\":"+itoa(inTok)+",\"output_tokens\":"+itoa(outTok)+"}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	if flusher != nil {
		flusher.Flush()
	}

	f.mu.Lock()
	if n := len(f.reqs); n > 0 {
		last := &f.reqs[n-1]
		last.EventTypes = []string{
			"message_start", "content_block_start", "content_block_delta",
			"content_block_stop", "message_delta", "message_stop",
		}
		last.StopReason = "tool_use"
		last.SawToolUseBlock = true
		last.UsageTokens = inTok + outTok
	}
	f.mu.Unlock()
}

// ======================== claude-specific helpers ========================

// findE2EClaude resolves the claude CLI absolute path; skips when absent.
func findE2EClaude(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, rel := range []string{".local/bin/claude", ".claude/local/claude"} {
			if info, serr := os.Stat(filepath.Join(home, rel)); serr == nil && !info.IsDir() {
				return filepath.Join(home, rel)
			}
		}
	}
	t.Skip("claude CLI not installed; skipping end-to-end launch test")
	return ""
}

// e2eClaudeChildEnv 继承父进程环境但剪掉会直接劫持 claude 网络栈的变量
// （ANTHROPIC_* / OPENCODE*_PROXY / key），再叠加非交互模式所需的开关。
// launch 本身会把 ANTHROPIC_BASE_URL/ANTHROPIC_API_KEY 重新注入（指向本地代理）。
func e2eClaudeChildEnv(tmpDir, modelsDevCache string) []string {
	skip := []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_OAUTH_TOKEN",
		"ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_API_KEY",
		"OPENCODE_API_KEY", "OPENCODE2API_CONFIG", "OPENCODE2API_PORT",
		"OPENCODE2API_LOG_FILE", "OPENCODE2API_STATS", "OPENCODE2API_STATS_FILE",
		"OPENCODE2API_MODELSDEV_CACHE", "OPENCODE2API_MODELSDEV_CACHE_FILE",
	}
	skipSet := make(map[string]bool, len(skip))
	for _, k := range skip {
		skipSet[k] = true
	}
	env := []string{}
	for _, kv := range os.Environ() {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if skipSet[key] {
			continue
		}
		env = append(env, kv)
	}
	// claude Code 需要写 ~/.claude 会话元数据；继承的 HOME 可能指向真家目录。
	// 测试应保持家目录不动：实在要隔离可另起 fake home，但 claude 在 fake home
	// 下没有 onboarding 记录会走 interactive first-run 流程卡住。继承真 HOME 时
	// --dangerously-skip-permissions + CI 标记加非交互模式已足够稳。
	env = append(env,
		"CI=true",
		"OPENCODE2API_MODELSDEV_CACHE="+modelsDevCache,
		"TMPDIR="+filepath.Join(tmpDir, "claude-tmp"),
		"NO_COLOR=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DISABLE_TELEMETRY=1",
		"DISABLE_AUTOUPDATE=1",
		"DISABLE_NON_ESSENTIAL_MODEL_CALLS=1",
		"DISABLE_ERROR_REPORTING=1",
	)
	_ = os.MkdirAll(filepath.Join(tmpDir, "claude-tmp"), 0o755)
	return env
}

// lastUserContentBlocks 取 messages 里最后一条 role=user 消息的 content blocks
// （content 是字符串则视为单个 text block）。
func lastUserContentBlocks(msgs []any) []any {
	for i := len(msgs) - 1; i >= 0; i-- {
		mm, _ := msgs[i].(map[string]any)
		if mm == nil || mm["role"] != "user" {
			continue
		}
		switch c := mm["content"].(type) {
		case string:
			return []any{map[string]any{"type": "text", "text": c}}
		case []any:
			return c
		}
		return nil
	}
	return nil
}

func hasAnthropicBlockType(blocks []any, want string) bool {
	for _, b := range blocks {
		if m, _ := b.(map[string]any); m != nil && m["type"] == want {
			return true
		}
	}
	return false
}

// assertAnthropicEventsContain 要求 wantOrder 中的 SSE event type 按顺序出现
// （可以有其他类型穿插，如 content_block_delta 重复）。
func assertAnthropicEventsContain(t *testing.T, label string, got []string, wantOrder []string) {
	t.Helper()
	pos := 0
	missing := []string{}
	for _, want := range wantOrder {
		found := false
		for ; pos < len(got); pos++ {
			if got[pos] == want {
				found = true
				pos++
				break
			}
		}
		if !found {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%s: event sequence missing %v; got %v", label, missing, got)
	}
}

func itoa(n int) string { return fmt.Sprint(n) }
