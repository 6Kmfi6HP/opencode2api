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
// Responses SSE protocol. It asserts the full round trip:
//
//  1. codex emits a streaming /responses request (stream=true, instructions set,
//     store=false, include contains reasoning.encrypted_content); the proxy
//     forwards it natively (protocol_rules "*" → responses passthrough).
//  2. The proxy relays the Response SSE event sequence untouched
//     (response.created → output_item.added → content_part/delta/done →
//     response.completed with usage) and codex accepts it.
//  3. Second turn: fake upstream streams a function_call (shell tool), codex
//     executes `cat /etc/hostname` in a read-only sandbox and answers with a
//     function_call_output echoing the original call_id, which the fake
//     upstream must observe on turn 2 before replying "E2E_FILE_READ_OK".
//  4. codex exits 0 after the final response.completed.
//
// Requires the codex CLI on PATH (or ~/.local/bin/codex); skipped otherwise.
func TestE2ELaunchCodex(t *testing.T) {
	_ = findE2ECodex(t) // skip early when codex CLI is absent
	binPath := buildE2EBinary(t)
	port := freeTCPPort(t)

	fake := newFakeResponsesUpstream(t)

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")
	cfg := map[string]any{
		"upstream_base_urls": []string{fake.srv.URL},
		"protocol_rules": []map[string]string{
			{"pattern": "*", "protocol": "responses"},
		},
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
	t.Logf("fake upstream saw %d /zen/v1/responses request(s)", len(turns))
	for i, turn := range turns {
		t.Logf("turn %d: stream=%v store=%v include=%v instructions_len=%d input_types=%v msg_roles=%v",
			i, turn.Stream, ptrBoolStr(turn.Store), turn.Include, len(turn.Instructions),
			turn.InputTypes, turn.FunctionOutputs)
	}

	if exitErr != nil {
		dump()
		t.Fatalf("launch codex exited with error: %v", exitErr)
	}

	if len(turns) < 2 {
		dump()
		t.Fatalf("fake upstream saw %d /zen/v1/responses request(s), want >= 2 (second turn proves codex consumed the streamed function_call and sent function_call_output back)", len(turns))
	}

	// Turn 1: codex must send a canonical Responses streaming request.
	first := turns[0]
	if !first.Stream {
		t.Errorf("turn 1: stream = %v, want true", first.Stream)
	}
	if strings.TrimSpace(first.Instructions) == "" {
		t.Error("turn 1: instructions empty; codex always sends its coding-agent system prompt")
	}
	if first.Store == nil || *first.Store {
		t.Errorf("turn 1: store = %v, want explicit false (codex is stateless)", ptrBoolStr(first.Store))
	}
	if !containsStr(first.Include, "reasoning.encrypted_content") {
		t.Errorf("turn 1: include = %v, want reasoning.encrypted_content", first.Include)
	}
	if !containsStr(first.InputTypes, "message") {
		t.Errorf("turn 1: input item types = %v, want at least one message item", first.InputTypes)
	}

	// The SSE sequence codex consumed from the proxy is captured verbatim on
	// the fake upstream side; assert the codex-required event chain shape.
	assertEventsContain(t, "turn 1", first.Events, []string{
		"response.created",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	})
	if first.CompletedUsage == nil {
		t.Error("turn 1: response.completed carried no usage")
	}

	// Turn 2: codex must answer the function_call under the same call_id;
	// the fake responded with the plain-text message "E2E_FILE_READ_OK".
	second := turns[1]
	if len(second.FunctionOutputs) == 0 {
		dump()
		t.Fatalf("turn 2: no function_call_output item in input; input types = %v", second.InputTypes)
	}
	out := second.FunctionOutputs[0]
	if out.CallID != fakeCallID {
		t.Errorf("turn 2: function_call_output.call_id = %q, want %q (echo of turn-1 shell call)", out.CallID, fakeCallID)
	}
	if !strings.Contains(out.Output, "Process exited with code 0") {
		// `hostname` runs under a read-only macOS sandbox; codex wraps the
		// result as text starting with `Chunk ID` and ending in "Process
		// exited with code 0". A zero exit code proves codex executed the
		// function_call locally and produced an output.
		t.Errorf("turn 2: function_call_output.output missing zero-exit marker; got %q", truncate(out.Output, 200))
	}
	assertEventsContain(t, "turn 2", second.Events, []string{
		"response.created",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.output_item.done",
		"response.completed",
	})
	for i, evt := range second.Events {
		if evt.Type == "response.output_text.delta" {
			if _, ok := evt.Raw["logprobs"]; !ok {
				t.Errorf("turn 2 event %d (response.output_text.delta) missing logprobs field", i)
			}
		}
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

type fakeSSEEvent struct {
	Seq  int            `json:"sequence_number"`
	Type string         `json:"type"`
	Raw  map[string]any `json:"-"`
}

type fakeFunctionCallOutput struct {
	CallID string
	Output string
}

type fakeTurn struct {
	Stream          bool
	Store           *bool
	Include         []string
	Instructions    string
	InputTypes      []string
	MessageRoles    []string
	FunctionOutputs []fakeFunctionCallOutput
	Events          []fakeSSEEvent
	CompletedUsage  map[string]any
	Model           string
}

type fakeResponsesUpstream struct {
	t      *testing.T
	srv    *httptest.Server
	turns  []fakeTurn // only appended from handleResponses, read after process exit
	models atomic.Int32
}

func newFakeResponsesUpstream(t *testing.T) *fakeResponsesUpstream {
	t.Helper()
	f := &fakeResponsesUpstream{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// Turns snapshots the recorded requests. Safe to call only after the codex
// subprocess has exited (handlers and the test goroutine no longer race).
func (f *fakeResponsesUpstream) Turns() []fakeTurn {
	cp := make([]fakeTurn, len(f.turns))
	copy(cp, f.turns)
	return cp
}

func (f *fakeResponsesUpstream) handle(w http.ResponseWriter, r *http.Request) {
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
	case r.Method == http.MethodPost && r.URL.Path == "/zen/v1/responses":
		f.handleResponses(w, r)
	default:
		http.Error(w, "unexpected path: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func (f *fakeResponsesUpstream) handleResponses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}

	turn := fakeTurn{}
	turn.Model, _ = req["model"].(string)
	turn.Stream, _ = req["stream"].(bool)
	if s, ok := req["store"].(bool); ok {
		turn.Store = &s
	}
	turn.Instructions, _ = req["instructions"].(string)
	if inc, ok := req["include"].([]any); ok {
		for _, v := range inc {
			if s, ok := v.(string); ok {
				turn.Include = append(turn.Include, s)
			}
		}
	}
	secondTurn := false
	if input, ok := req["input"].([]any); ok {
		for _, item := range input {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			turn.InputTypes = append(turn.InputTypes, typ)
			switch typ {
			case "message":
				if role, ok := m["role"].(string); ok {
					turn.MessageRoles = append(turn.MessageRoles, role)
				}
			case "function_call_output", "local_shell_call_output":
				secondTurn = true
				out := fakeFunctionCallOutput{}
				out.CallID, _ = m["call_id"].(string)
				if v, ok := m["output"].(string); ok {
					out.Output = v
				} else if raw, err := json.Marshal(m["output"]); err == nil && len(raw) > 0 && string(raw) != "null" {
					out.Output = string(raw)
				}
				turn.FunctionOutputs = append(turn.FunctionOutputs, out)
			}
		}
	}

	events, completedResponse := buildTurnSSE(turn, secondTurn)
	for _, evt := range events {
		turn.Events = append(turn.Events, fakeSSEEvent{Seq: seqOf(evt), Type: typeOf(evt), Raw: evt})
	}
	if u, ok := completedResponse["usage"].(map[string]any); ok {
		turn.CompletedUsage = u
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
	for _, evt := range events {
		name, _ := evt["type"].(string)
		data, err := json.Marshal(evt)
		if err != nil {
			f.t.Errorf("marshal SSE event: %v", err)
			return
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data); err != nil {
			return // client went away
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

// buildTurnSSE renders a canonical Responses streaming session for one turn.
// Second turns (tool result submitted) answer with a plain assistant message;
// the first turn issues a single local_shell_call (codex's native shell tool,
// mapped to "local_shell_call" items by codex's ResponseItem serde).
func buildTurnSSE(turn fakeTurn, secondTurn bool) (events []map[string]any, completedResponse map[string]any) {
	respID := fmt.Sprintf("resp_e2e_%d", len(turn.InputTypes))
	if secondTurn {
		respID += "_t2"
	}
	createdAt := time.Now().Unix()
	model := turn.Model
	if model == "" {
		model = "gpt-5-codex"
	}
	seq := 0
	next := func() int { seq++; return seq }

	responseShell := func(status string) map[string]any {
		return map[string]any{
			"id":         respID,
			"object":     "response",
			"created_at": createdAt,
			"status":     status,
			"background": false,
			"error":      nil,
			"model":      model,
			"output":     []any{},
		}
	}
	appendEvt := func(name string, payload map[string]any) {
		payload["type"] = name
		payload["sequence_number"] = next()
		events = append(events, payload)
	}

	appendEvt("response.created", map[string]any{"response": responseShell("in_progress")})
	appendEvt("response.in_progress", map[string]any{"response": responseShell("in_progress")})

	var output []any
	if !secondTurn {
		// codex's shell tool ships as a `function_call` named `exec_command`
		// (its ToolSpec name); arguments are a JSON string with
		// { cmd, workdir?, yield_time_ms?, max_output_tokens? }.
		fcID := "fc_" + fakeCallID
		// `hostname` is a single binary with no /etc/hostname dependency,
		// available and readable under codex's read-only macOS sandbox.
		args := `{"cmd":"hostname","workdir":"/tmp","yield_time_ms":10000}`
		itemInProgress := map[string]any{
			"id": fcID, "type": "function_call", "status": "in_progress",
			"call_id": fakeCallID, "name": "exec_command", "arguments": "",
		}
		appendEvt("response.output_item.added", map[string]any{"output_index": 0, "item": itemInProgress})
		// Stream the arguments in two chunks so codex exercises the delta path.
		appendEvt("response.function_call_arguments.delta", map[string]any{
			"item_id": fcID, "output_index": 0, "delta": args[:len(args)/2],
		})
		appendEvt("response.function_call_arguments.delta", map[string]any{
			"item_id": fcID, "output_index": 0, "delta": args[len(args)/2:],
		})
		appendEvt("response.function_call_arguments.done", map[string]any{
			"item_id": fcID, "output_index": 0, "name": "exec_command", "arguments": args,
		})
		itemDone := map[string]any{
			"id": fcID, "type": "function_call", "status": "completed",
			"call_id": fakeCallID, "name": "exec_command", "arguments": args,
		}
		appendEvt("response.output_item.done", map[string]any{"output_index": 0, "item": itemDone})
		output = append(output, itemDone)
	} else {
		msgID := respID + "_msg0"
		text := "E2E_FILE_READ_OK"
		msgInProgress := map[string]any{
			"id": msgID, "type": "message", "status": "in_progress",
			"role": "assistant", "content": []any{},
		}
		appendEvt("response.output_item.added", map[string]any{"output_index": 0, "item": msgInProgress})
		part := map[string]any{
			"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{},
		}
		appendEvt("response.content_part.added", map[string]any{
			"item_id": msgID, "output_index": 0, "content_index": 0, "part": part,
		})
		appendEvt("response.output_text.delta", map[string]any{
			"item_id": msgID, "output_index": 0, "content_index": 0,
			"delta": text, "logprobs": []any{},
		})
		appendEvt("response.output_text.done", map[string]any{
			"item_id": msgID, "output_index": 0, "content_index": 0,
			"text": text, "logprobs": []any{},
		})
		partDone := map[string]any{
			"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{},
		}
		appendEvt("response.content_part.done", map[string]any{
			"item_id": msgID, "output_index": 0, "content_index": 0, "part": partDone,
		})
		msgDone := map[string]any{
			"id": msgID, "type": "message", "status": "completed",
			"role": "assistant", "content": []any{partDone},
		}
		appendEvt("response.output_item.done", map[string]any{"output_index": 0, "item": msgDone})
		output = append(output, msgDone)
	}

	completedResponse = responseShell("completed")
	completedResponse["output"] = output
	completedResponse["usage"] = map[string]any{
		"input_tokens":          1200,
		"input_tokens_details":  map[string]any{"cached_tokens": 256},
		"output_tokens":         42,
		"output_tokens_details": map[string]any{"reasoning_tokens": 16},
		"total_tokens":          1242,
	}
	appendEvt("response.completed", map[string]any{"response": completedResponse})
	return events, completedResponse
}

func seqOf(evt map[string]any) int {
	if v, ok := evt["sequence_number"].(int); ok {
		return v
	}
	return -1
}

func typeOf(evt map[string]any) string {
	s, _ := evt["type"].(string)
	return s
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

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func assertEventsContain(t *testing.T, label string, events []fakeSSEEvent, wantOrder []string) {
	t.Helper()
	pos := 0
	missing := []string{}
	for _, want := range wantOrder {
		found := false
		for ; pos < len(events); pos++ {
			if events[pos].Type == want {
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
		got := make([]string, 0, len(events))
		for _, e := range events {
			got = append(got, e.Type)
		}
		t.Errorf("%s: event sequence missing %v; got %v", label, missing, got)
	}
	for i, e := range events {
		if e.Seq <= 0 {
			t.Errorf("%s event %d (%s): sequence_number missing or non-positive", label, i, e.Type)
		}
	}
}

func ptrBoolStr(b *bool) any {
	if b == nil {
		return "nil"
	}
	return *b
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
