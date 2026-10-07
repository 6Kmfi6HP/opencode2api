package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestE2ELaunchCodex drives a real `opencode2api launch codex` subprocess (built
// from this tree, or OPENCODE2API_BIN when prebuilt) with the real codex CLI as
// the agent client, against an in-process fake upstream speaking the OpenAI
// chat completions SSE protocol. /v1/responses 只走翻译路径（原生透传已移除），
// 所以 fake 上游说的是 chat/completions。It asserts the full round trip:
//
//  1. codex emits a streaming /responses request (stream=true, instructions
//     set); the proxy translates it into a /zen/v1/chat/completions request
//     (default chat translation path).
//  2. The proxy converts the chat SSE back into Responses events
//     (response.created → output_item.added → function_call_arguments.delta/
//     done → output_item.done → response.completed with usage) and codex
//     accepts it.
//  3. Second turn: fake upstream streams a tool_call (shell tool), codex
//     executes `hostname` in a read-only sandbox and answers with a tool
//     message echoing the original call_id, which the fake upstream must
//     observe on turn 2 before replying "E2E_FILE_READ_OK".
//  4. codex exits 0 after the final response.
//
// Requires the codex CLI on PATH (or ~/.local/bin/codex); skipped otherwise.
func TestE2ELaunchCodex(t *testing.T) {
	_ = findE2ECodex(t) // skip early when codex CLI is absent
	binPath := buildE2EBinary(t)
	port := freeTCPPort(t)

	fake := newFakeChatUpstream(t)

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")
	cfg := map[string]any{
		"upstream_base_urls": []string{fake.srv.URL},
		// Point at the repo-tracked cache so launch skips the (test-blocked)
		// network fetch of models.dev.
		"text_only_models": []string{},
	}
	writeJSONFile(t, cfgPath, cfg)
	modelsDevCache := filepath.Join(tmpDir, "modelsdev_cache.json")
	copyFile(t, "modelsdev_cache.json", modelsDevCache)

	// codex's launch provider sets requires_openai_auth=true, which makes the
	// CLI consider the user's ~/.codex/auth.json. A ChatGPT login there
	// rejects every non-ChatGPT model, so give the child an isolated
	// CODEX_HOME with an apikey auth record instead.
	codexHome := filepath.Join(tmpDir, "codex-home")
	writeJSONFile(t, filepath.Join(codexHome, "auth.json"), map[string]any{
		"auth_mode":      "apikey",
		"OPENAI_API_KEY": "public",
	})

	prompt := "Use the shell tool to run `hostname`, then reply with exactly E2E_FILE_READ_OK"

	proxyLogPath := filepath.Join(tmpDir, "opencode2api.log")
	cmd := exec.Command(binPath,
		"launch", "codex",
		"--config", cfgPath,
		"--model", "gpt-5-codex",
		"--port", fmt.Sprint(port),
		"--stats-file", filepath.Join(tmpDir, "stats.json"),
		"--log-file", proxyLogPath,
		"--debug",
		"--",
		"exec", "--json",
		"--skip-git-repo-check",
		"--ephemeral",
		"--ignore-user-config",
		"-s", "read-only",
		"-c", `approval_policy="never"`,
		"-c", `preferred_auth_method="apikey"`,
		"-c", `otel.enabled=false`,
		"-C", tmpDir,
		prompt,
	)
	cmd.Env = e2eChildEnv(tmpDir, modelsDevCache, codexHome)

	stdoutPath := filepath.Join(tmpDir, "codex.stdout.jsonl")
	stderrPath := filepath.Join(tmpDir, "codex.stderr.log")
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
	case <-time.After(150 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-runErr
		t.Fatalf("launch codex timed out after 150s (see %s, %s, %s)", stdoutPath, stderrPath, proxyLogPath)
	}
	_ = stdoutFile.Close()
	_ = stderrFile.Close()

	stdout, _ := os.ReadFile(stdoutPath)
	stderr, _ := os.ReadFile(stderrPath)
	proxyLog, _ := os.ReadFile(proxyLogPath)
	dump := func() {
		t.Helper()
		t.Logf("codex stdout (last 4KB):\n%s", tail(stdout, 4096))
		t.Logf("codex stderr (last 4KB):\n%s", tail(stderr, 4096))
		t.Logf("proxy log (last 4KB):\n%s", tail(proxyLog, 4096))
	}

	turns := fake.Turns()
	t.Logf("fake upstream saw %d /zen/v1/chat/completions request(s)", len(turns))
	for i, turn := range turns {
		t.Logf("turn %d: stream=%v model=%q tool_names=%v msg_roles=%v",
			i, turn.Stream, turn.Model, turn.ToolNames, messageRoles(turn.Messages))
	}

	if exitErr != nil {
		dump()
		t.Fatalf("launch codex exited with error: %v", exitErr)
	}

	if len(turns) < 2 {
		dump()
		t.Fatalf("fake upstream saw %d /zen/v1/chat/completions request(s), want >= 2 (second turn proves codex consumed the streamed tool_call and sent the tool result back)", len(turns))
	}

	// Turn 1: the proxy must translate codex's Responses request into a chat
	// completions request (instructions → system message, tools → function
	// tools) and stream it upstream.
	first := turns[0]
	if !first.Stream {
		t.Errorf("turn 1: stream = %v, want true", first.Stream)
	}
	if first.Model != "gpt-5-codex" {
		t.Errorf("turn 1: model = %q, want gpt-5-codex", first.Model)
	}
	if len(first.Messages) == 0 || first.Messages[0].Role != "system" {
		t.Errorf("turn 1: messages = %v, want system message first (codex instructions translated)", messageRoles(first.Messages))
	}
	if len(first.ToolNames) == 0 {
		t.Errorf("turn 1: tools empty; codex function tools must be translated upstream")
	}

	// Turn 2: codex must answer the tool_call under the same call_id; the
	// fake responded with the plain-text message "E2E_FILE_READ_OK".
	second := turns[1]
	toolMsg := firstToolMessage(second.Messages)
	if toolMsg == nil {
		dump()
		t.Fatalf("turn 2: no tool message in chat messages; roles = %v", messageRoles(second.Messages))
	}
	if toolMsg.ToolCallID != fakeCallID {
		t.Errorf("turn 2: tool message tool_call_id = %q, want %q (echo of turn-1 shell call)", toolMsg.ToolCallID, fakeCallID)
	}
	if !strings.Contains(toolMsg.Content, "Process exited with code 0") {
		// `hostname` runs under a read-only macOS sandbox; codex wraps the
		// result as text starting with `Chunk ID` and ending in "Process
		// exited with code 0". A zero exit code proves codex executed the
		// tool_call locally and produced an output.
		t.Errorf("turn 2: tool message content missing zero-exit marker; got %q", truncate(toolMsg.Content, 200))
	}

	if !strings.Contains(string(stdout), "E2E_FILE_READ_OK") {
		dump()
		t.Fatalf("codex stdout did not contain final agent marker E2E_FILE_READ_OK")
	}
	if exitCode := cmd.ProcessState.ExitCode(); exitCode != 0 {
		dump()
		t.Fatalf("codex exit code = %d, want 0", exitCode)
	}
}

// ======================== fake upstream ========================

const fakeCallID = "call_e2e_1"

type fakeChatMessage struct {
	Role       string
	Content    string
	ToolCallID string
	ToolNames  []string // assistant tool_calls function names
}

type fakeChatTurn struct {
	Stream    bool
	Model     string
	Messages  []fakeChatMessage
	ToolNames []string // declared function tools
}

type fakeChatUpstream struct {
	t      *testing.T
	srv    *httptest.Server
	turns  []fakeChatTurn // only appended from handleChatCompletions, read after process exit
	models atomic.Int32
}

func newFakeChatUpstream(t *testing.T) *fakeChatUpstream {
	t.Helper()
	f := &fakeChatUpstream{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// Turns snapshots the recorded requests. Safe to call only after the codex
// subprocess has exited (handlers and the test goroutine no longer race).
func (f *fakeChatUpstream) Turns() []fakeChatTurn {
	cp := make([]fakeChatTurn, len(f.turns))
	copy(cp, f.turns)
	return cp
}

func (f *fakeChatUpstream) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && (r.URL.Path == "/zen/v1/models" || r.URL.Path == "/zen/go/v1/models"):
		f.models.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/zen/go/v1/models" {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		// Two IDs: buildCodexModelCatalogSpecs needs at least the launched
		// model in the startup catalog or codex receives no catalog at all.
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5-codex","object":"model"},{"id":"big-pickle","object":"model"}]}`))
	case r.Method == http.MethodPost && r.URL.Path == "/zen/v1/chat/completions":
		f.handleChatCompletions(w, r)
	default:
		http.Error(w, "unexpected path: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func (f *fakeChatUpstream) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role       string `json:"role"`
			Content    any    `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}

	turn := fakeChatTurn{Stream: req.Stream, Model: req.Model}
	for _, tl := range req.Tools {
		if tl.Function.Name != "" {
			turn.ToolNames = append(turn.ToolNames, tl.Function.Name)
		}
	}
	secondTurn := false
	for _, m := range req.Messages {
		msg := fakeChatMessage{Role: m.Role, ToolCallID: m.ToolCallID}
		if s, ok := m.Content.(string); ok {
			msg.Content = s
		} else if m.Content != nil {
			if raw, err := json.Marshal(m.Content); err == nil {
				msg.Content = string(raw)
			}
		}
		for _, tc := range m.ToolCalls {
			msg.ToolNames = append(msg.ToolNames, tc.Function.Name)
		}
		if m.Role == "tool" {
			secondTurn = true
		}
		turn.Messages = append(turn.Messages, msg)
	}
	f.turns = append(f.turns, turn)

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}
	chunk := func(delta map[string]any, finish string, usage map[string]any) bool {
		c := map[string]any{
			"id":      "chatcmpl_e2e",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   req.Model,
		}
		if usage != nil {
			c["choices"] = []any{}
			c["usage"] = usage
		} else {
			c["choices"] = []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}
		}
		data, err := json.Marshal(c)
		if err != nil {
			f.t.Errorf("marshal SSE chunk: %v", err)
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false // client went away
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	_ = chunk(map[string]any{"role": "assistant"}, "", nil)
	if !secondTurn {
		// codex's shell tool answers as a function tool call named
		// `exec_command` (its ToolSpec name); arguments are a JSON string
		// with { cmd, workdir?, yield_time_ms?, max_output_tokens? }.
		args := `{"cmd":"hostname","workdir":"/tmp","yield_time_ms":10000}`
		// Stream the arguments in two chunks so codex exercises the delta path.
		_ = chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": fakeCallID, "type": "function",
			"function": map[string]any{"name": "exec_command", "arguments": args[:len(args)/2]},
		}}}, "", nil)
		_ = chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index":    0,
			"function": map[string]any{"arguments": args[len(args)/2:]},
		}}}, "", nil)
		_ = chunk(map[string]any{}, "tool_calls", nil)
	} else {
		_ = chunk(map[string]any{"content": "E2E_FILE_READ_OK"}, "", nil)
		_ = chunk(map[string]any{}, "stop", nil)
	}
	_ = chunk(nil, "", map[string]any{
		"prompt_tokens":     1200,
		"completion_tokens": 42,
		"total_tokens":      1242,
	})
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

func messageRoles(msgs []fakeChatMessage) []string {
	roles := make([]string, 0, len(msgs))
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	return roles
}

func firstToolMessage(msgs []fakeChatMessage) *fakeChatMessage {
	for i, m := range msgs {
		if m.Role == "tool" {
			return &msgs[i]
		}
	}
	return nil
}

// ======================== helpers ========================

func findE2ECodex(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("codex"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, c := range []string{
			filepath.Join(home, ".local", "bin", "codex"),
			filepath.Join(home, ".codex", "bin", "codex"),
		} {
			if info, err := os.Stat(c); err == nil && !info.IsDir() {
				return c
			}
		}
	}
	t.Skip("codex CLI not installed; skipping end-to-end launch test")
	return ""
}

// buildE2EBinary compiles cmd/opencode2api once so the launch flow runs in a
// real subprocess exactly as a user would invoke it. Set OPENCODE2API_BIN to a
// prebuilt binary to skip the ~1s build when iterating.
func buildE2EBinary(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("OPENCODE2API_BIN"); bin != "" {
		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("OPENCODE2API_BIN=%q not usable: %v", bin, err)
		}
		return bin
	}
	binPath := filepath.Join(t.TempDir(), "opencode2api")
	build := exec.Command("go", "build", "-o", binPath, "../../cmd/opencode2api")
	out, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("go build opencode2api failed: %v\n%s", err, out)
	}
	return binPath
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// e2eChildEnv mirrors the caller environment minus the user's ambient opencode
// key (launch hard-codes "public" when the flag is absent; OPENCODE_API_KEY
// only overrides when --key is not passed and must not leak a real key into a
// test run against a fake upstream) and pins CODEX_HOME to the isolated home
// so the user's ChatGPT login/MCP config cannot leak into the run.
func e2eChildEnv(tmpDir, modelsDevCache, codexHome string) []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "OPENCODE_API_KEY", "CODEX_API_KEY", "OPENCODE2API_CONFIG",
			"OPENCODE2API_LOG_FILE", "OPENCODE2API_STATS", "OPENCODE2API_STATS_FILE",
			"CODEX_HOME", "OPENAI_API_KEY":
			continue
		}
		env = append(env, kv)
	}
	execDir := filepath.Join(tmpDir, "codex-tmp")
	_ = os.MkdirAll(execDir, 0o755)
	env = append(env,
		"CODEX_HOME="+codexHome,
		"OPENCODE2API_MODELSDEV_CACHE="+modelsDevCache,
		"TMPDIR="+execDir,
		"NO_COLOR=1",
	)
	return env
}

func waitForProxyPort(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	for {
		resp, err := http.Get(url) //nolint:bodyclose // closed below
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy on 127.0.0.1:%d did not become ready", port)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, srcRel, dst string) {
	t.Helper()
	data, err := os.ReadFile(srcRel)
	if err != nil {
		t.Fatalf("read %s: %v", srcRel, err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func tail(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[len(b)-n:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
