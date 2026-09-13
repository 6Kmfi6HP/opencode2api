package logging

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/6Kmfi6HP/opencode2api/internal/random"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Configuration globals are bound by flags in the app package (server.go) and
// assigned directly by launch.go's configureLaunchGlobals. Only the file
// rotation knobs stay exported; the log file path, level, and body-summary
// flag are passed to Init as parameters instead.
var (
	Stdout     bool
	MaxSize    int
	MaxBackups int
	MaxAge     int
	Compress   bool
)

var (
	levelVar      = &slog.LevelVar{}
	bodiesMu      sync.RWMutex
	bodiesEnabled bool
	rotator       *lumberjack.Logger

	upstreamErrDedupMu sync.Mutex
	upstreamErrDedup   = map[string]upstreamErrDedupEntry{}
)

type upstreamErrDedupEntry struct {
	last       time.Time
	suppressed int
}

// SummaryExtras carries app-provided helpers used by the request-body
// summarizer. They are injected by the app package at startup to avoid an
// import cycle: the helpers (thinking-state classification and cache_control
// block counting) live in the app package, which imports this one.
type SummaryExtras struct {
	// ThinkingState classifies a request's "thinking" field.
	ThinkingState func(any) string
	// CacheControlCount counts cache_control breakpoints in a decoded body.
	CacheControlCount func(any) int
}

var summaryExtras SummaryExtras

// SetSummaryExtras installs app-provided body-summary helpers. Call once at
// startup, before the first request is handled.
func SetSummaryExtras(x SummaryExtras) {
	summaryExtras = x
}

// SetBodies updates the runtime "log bodies" flag used to enable debug-level
// body summaries.
func SetBodies(enabled bool) {
	bodiesMu.Lock()
	bodiesEnabled = enabled
	bodiesMu.Unlock()
}

// BodiesEnabled reports whether debug-level body summaries are enabled.
func BodiesEnabled() bool {
	bodiesMu.RLock()
	defer bodiesMu.RUnlock()
	return bodiesEnabled
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// SetLevelString parses and applies the log level string ("debug", "info",
// "warn", or "error") at runtime; empty or unknown values fall back to info.
func SetLevelString(s string) {
	levelVar.Set(parseLogLevel(s))
}

// LevelString returns the current log level as a canonical string.
func LevelString() string {
	switch levelVar.Level() {
	case slog.LevelDebug:
		return "debug"
	case slog.LevelWarn:
		return "warn"
	case slog.LevelError:
		return "error"
	default:
		return "info"
	}
}

func redactSecret(s string) string {
	if s == "" {
		return ""
	}
	prefix := s
	if len(prefix) > 6 {
		prefix = prefix[:6]
	}
	return fmt.Sprintf("%s…(len=%d)", prefix, len(s))
}

func isSensitiveLogKey(key string) bool {
	switch strings.ToLower(key) {
	case "authorization", "x-api-key", "password", "token", "api_key", "apikey", "secret":
		return true
	default:
		return false
	}
}

func looksLikeSecretValue(s string) bool {
	trimmed := strings.TrimSpace(s)
	if strings.HasPrefix(trimmed, "Bearer ") {
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "Bearer "))
	}
	return strings.HasPrefix(trimmed, "sk-") && len(trimmed) > 8
}

func redactLogAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey {
		return slog.String("time", a.Value.Time().Format("2006-01-02T15:04:05.000Z07:00"))
	}
	if a.Key == slog.SourceKey {
		return slog.Attr{}
	}
	if a.Value.Kind() == slog.KindString {
		val := a.Value.String()
		if isSensitiveLogKey(a.Key) || looksLikeSecretValue(val) {
			return slog.String(a.Key, redactSecret(val))
		}
	}
	return a
}

// CloseRotator closes and clears the active log rotator, if any.
func CloseRotator() {
	if rotator != nil {
		_ = rotator.Close()
		rotator = nil
	}
}

// Init configures the default logger and returns it. path is the already
// resolved log file path (empty means no file, i.e. stdout only); level and
// bodies seed the runtime log level and body-summary flag.
func Init(path, level string, bodies bool) *slog.Logger {
	SetLevelString(level)
	SetBodies(bodies)

	var writers []io.Writer
	if Stdout {
		writers = append(writers, os.Stdout)
	}

	resolvedPath := ""
	if path != "" {
		absPath, absErr := filepath.Abs(path)
		if absErr != nil {
			absPath = path
		}
		dir := filepath.Dir(absPath)
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			fmt.Fprintf(os.Stderr, "cannot create log directory %s: %v; falling back to stdout\n", dir, mkErr)
			if !Stdout {
				writers = append(writers, os.Stdout)
			}
		} else {
			rotator = &lumberjack.Logger{
				Filename:   absPath,
				MaxSize:    MaxSize,
				MaxBackups: MaxBackups,
				MaxAge:     MaxAge,
				Compress:   Compress,
				LocalTime:  true,
			}
			writers = append(writers, rotator)
			resolvedPath = absPath
		}
	}

	if len(writers) == 0 {
		writers = append(writers, os.Stdout)
	}

	w := io.MultiWriter(writers...)
	handler := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level:       levelVar,
		ReplaceAttr: redactLogAttr,
	})
	logger := slog.New(handler)
	slog.SetDefault(logger)

	attrs := []any{
		"level", LevelString(),
		"stdout", Stdout || resolvedPath == "",
		"log_bodies", BodiesEnabled(),
		"max_size_mb", MaxSize,
		"max_backups", MaxBackups,
		"max_age_days", MaxAge,
		"compress", Compress,
	}
	if resolvedPath != "" {
		attrs = append([]any{"path", resolvedPath}, attrs...)
	} else {
		attrs = append([]any{"path", "stdout"}, attrs...)
	}
	slog.Info("logging configured", attrs...)
	return logger
}

type contextKey string

const reqIDKey contextKey = "request_id"

func requestID(ctx context.Context) string {
	if id, ok := ctx.Value(reqIDKey).(string); ok {
		return id
	}
	return ""
}

// FromContext returns the logger for the given request context, annotated with
// the request ID when one is present.
func FromContext(ctx context.Context) *slog.Logger {
	id := requestID(ctx)
	if id == "" {
		return slog.Default()
	}
	return slog.Default().With("request_id", id)
}

type statusRecorder struct {
	http.ResponseWriter
	status    int
	bytesOut  int
	wroteHead bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHead {
		return
	}
	r.wroteHead = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHead {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytesOut += n
	return n, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Middleware wraps an HTTP handler with request-ID assignment, request
// progress logging, and request-completion logging.
func Middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if reqID == "" {
			reqID = random.String(12)
		}
		ctx := context.WithValue(r.Context(), reqIDKey, reqID)
		r = r.WithContext(ctx)
		w.Header().Set("X-Request-Id", reqID)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		log := FromContext(ctx)
		quiet := r.URL.Path == "/health" || r.URL.Path == "/"
		if quiet {
			log.Debug("request_started",
				"method", r.Method,
				"path", r.URL.Path,
				"remote", r.RemoteAddr,
				"user_agent", r.UserAgent(),
			)
		} else {
			log.Debug("request_started",
				"method", r.Method,
				"path", r.URL.Path,
				"remote", r.RemoteAddr,
				"user_agent", r.UserAgent(),
			)
		}

		next(rec, r)

		durationMs := time.Since(start).Milliseconds()
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", durationMs,
			"bytes_out", rec.bytesOut,
		}
		if quiet {
			log.Debug("request_done", attrs...)
		} else {
			log.Info("request_done", attrs...)
		}
	}
}

// PlanRequest logs a request_plan record with the given structured fields.
func PlanRequest(ctx context.Context, fields map[string]any) {
	attrs := make([]any, 0, len(fields)*2)
	for k, v := range fields {
		attrs = append(attrs, k, v)
	}
	FromContext(ctx).Info("request_plan", attrs...)
}

// LogResult logs a request_result record with the given structured fields.
func LogResult(ctx context.Context, fields map[string]any) {
	attrs := make([]any, 0, len(fields)*2)
	for k, v := range fields {
		attrs = append(attrs, k, v)
	}
	FromContext(ctx).Info("request_result", attrs...)
}

// StreamStats tracks per-stream accounting for the stream_result log record.
type StreamStats struct {
	Start             time.Time
	FirstChunkAt      time.Time
	Chunks            int
	TextChars         int
	ReasoningChars    int
	ToolCallCount     int
	FinishReason      string
	DoneSeen          bool
	PromotedReasoning bool
	SawFinish         bool
}

// NoteChunk records a new stream chunk, stamping the first-chunk time.
func (s *StreamStats) NoteChunk() {
	s.Chunks++
	if s.FirstChunkAt.IsZero() {
		s.FirstChunkAt = time.Now()
	}
}

// ObserveDelta folds a stream delta into the running stats.
func (s *StreamStats) ObserveDelta(delta map[string]any, keepReasoning bool) {
	if delta == nil {
		return
	}
	s.NoteChunk()
	if c, ok := delta["content"].(string); ok {
		s.TextChars += len(c)
	}
	if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
		s.ReasoningChars += len(rc)
		if !keepReasoning {
			// Will be promoted to text by promoteMisplacedReasoning / stream handler.
			content, _ := delta["content"].(string)
			rawTC, hasTC := delta["tool_calls"]
			tcEmpty := true
			if hasTC && rawTC != nil {
				if arr, ok := rawTC.([]any); ok && len(arr) > 0 {
					tcEmpty = false
				}
			}
			if content == "" && tcEmpty {
				s.PromotedReasoning = true
				s.TextChars += len(rc)
			}
		}
	}
	if raw, ok := delta["tool_calls"].([]any); ok {
		for _, item := range raw {
			tc, ok := item.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := tc["function"].(map[string]any)
			name, _ := fn["name"].(string)
			id, _ := tc["id"].(string)
			if name != "" || id != "" {
				s.ToolCallCount++
			}
		}
	}
}

// Log emits the stream_result record for the given protocol.
func (s *StreamStats) Log(ctx context.Context, protocol string) {
	if s.Start.IsZero() {
		s.Start = time.Now()
	}
	firstMs := int64(0)
	if !s.FirstChunkAt.IsZero() {
		firstMs = s.FirstChunkAt.Sub(s.Start).Milliseconds()
	}
	emptyReply := s.TextChars == 0 && s.ToolCallCount == 0
	truncated := !s.DoneSeen && !s.SawFinish
	attrs := []any{
		"protocol", protocol,
		"chunks", s.Chunks,
		"first_chunk_ms", firstMs,
		"duration_ms", time.Since(s.Start).Milliseconds(),
		"text_chars", s.TextChars,
		"reasoning_chars", s.ReasoningChars,
		"tool_call_count", s.ToolCallCount,
		"finish_reason", s.FinishReason,
		"done_seen", s.DoneSeen,
		"truncated", truncated,
		"empty_reply", emptyReply,
		"promoted_reasoning", s.PromotedReasoning,
	}
	log := FromContext(ctx)
	if emptyReply {
		log.Warn("stream_result", attrs...)
		return
	}
	log.Info("stream_result", attrs...)
}

func classifyThinking(v any) string {
	if summaryExtras.ThinkingState != nil {
		return summaryExtras.ThinkingState(v)
	}
	if v == nil {
		return "absent"
	}
	return "present"
}

func countCacheControlBlocks(v any) int {
	if summaryExtras.CacheControlCount != nil {
		return summaryExtras.CacheControlCount(v)
	}
	return 0
}

func summarizeJSONBody(raw []byte, max int) map[string]any {
	if max <= 0 {
		max = 4096
	}
	out := map[string]any{}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		out["parse_error"] = true
		out["bytes"] = len(raw)
		return out
	}
	for _, key := range []string{"model", "stream", "max_tokens", "reasoning_effort", "temperature", "top_p"} {
		if v, ok := obj[key]; ok {
			out[key] = v
		}
	}
	if t, ok := obj["thinking"]; ok {
		out["thinking"] = classifyThinking(t)
	}
	if oc, ok := obj["output_config"].(map[string]any); ok {
		if effort, _ := oc["effort"].(string); effort != "" {
			out["output_config_effort"] = effort
		}
	}
	if _, ok := obj["context_management"]; ok {
		out["context_management"] = true
	}
	if n := countCacheControlBlocks(obj); n > 0 {
		out["cache_control_blocks"] = n
	}
	if msgs, ok := obj["messages"].([]any); ok {
		roles := make([]string, 0, len(msgs))
		chars := make([]int, 0, len(msgs))
		blockTypes := map[string]int{}
		hasRC, hasTC := false, false
		for _, m := range msgs {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			roles = append(roles, role)
			chars = append(chars, contentCharCount(msg["content"]))
			collectBlockTypes(msg["content"], blockTypes)
			if _, ok := msg["reasoning_content"]; ok {
				hasRC = true
			}
			if raw, ok := msg["tool_calls"]; ok && raw != nil {
				hasTC = true
			}
		}
		out["messages_count"] = len(msgs)
		out["message_roles"] = roles
		out["message_chars"] = chars
		if len(blockTypes) > 0 {
			out["content_block_types"] = blockTypes
		}
		out["has_reasoning_content"] = hasRC
		out["has_tool_calls"] = hasTC
	}
	if tools, ok := obj["tools"].([]any); ok {
		out["tools_count"] = len(tools)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return out
	}
	if len(encoded) > max {
		out["truncated"] = true
		out["summary_bytes"] = len(encoded)
		// Drop bulky arrays when over budget.
		delete(out, "message_chars")
		delete(out, "message_roles")
	}
	return out
}

func contentCharCount(content any) int {
	switch v := content.(type) {
	case string:
		return len(v)
	case []any:
		n := 0
		for _, part := range v {
			n += contentCharCount(part)
		}
		return n
	case map[string]any:
		if t, _ := v["text"].(string); t != "" {
			return len(t)
		}
		if t, _ := v["thinking"].(string); t != "" {
			return len(t)
		}
		if c, ok := v["content"]; ok {
			return contentCharCount(c)
		}
	}
	return 0
}

func collectBlockTypes(content any, counts map[string]int) {
	switch v := content.(type) {
	case string:
		if v != "" {
			counts["text"]++
		}
	case []any:
		for _, part := range v {
			collectBlockTypes(part, counts)
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		if typ == "" {
			typ = "object"
		}
		counts[typ]++
	}
}

// MaybeBodySummary logs a debug-level JSON body summary when body summaries
// are enabled and the log level is debug.
func MaybeBodySummary(ctx context.Context, label string, raw []byte) {
	if !BodiesEnabled() || levelVar.Level() > slog.LevelDebug {
		return
	}
	FromContext(ctx).Debug(label, "summary", summarizeJSONBody(raw, 4096))
}

// UpstreamError logs an upstream error with per-model/status/base-URL
// deduplication to keep noisy upstream failures from flooding the log.
func UpstreamError(ctx context.Context, model string, status int, body []byte, baseURL string) {
	key := fmt.Sprintf("%s:%d:%s", model, status, baseURL)
	now := time.Now()
	upstreamErrDedupMu.Lock()
	entry, ok := upstreamErrDedup[key]
	if ok && now.Sub(entry.last) < 10*time.Second {
		entry.suppressed++
		upstreamErrDedup[key] = entry
		suppressed := entry.suppressed
		upstreamErrDedupMu.Unlock()
		FromContext(ctx).Error("upstream error",
			"model", model,
			"base_url", baseURL,
			"status", status,
			"body", truncateForLog(string(body), 512),
			"suppressed", suppressed,
		)
		return
	}
	upstreamErrDedup[key] = upstreamErrDedupEntry{last: now}
	upstreamErrDedupMu.Unlock()
	FromContext(ctx).Error("upstream error",
		"model", model,
		"base_url", baseURL,
		"status", status,
		"body", truncateForLog(string(body), 512),
	)
}

func truncateForLog(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// SummarizeChatResult reduces a Chat completion response body to a compact
// result summary for the request_result log record.
func SummarizeChatResult(body []byte) map[string]any {
	out := map[string]any{
		"has_text":           false,
		"has_reasoning":      false,
		"tool_call_count":    0,
		"promoted_reasoning": false,
		"finish_reason":      "",
		"total_tokens":       0,
		"reasoning_tokens":   0,
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return out
	}
	if usage, ok := raw["usage"].(map[string]any); ok {
		if tt, ok := usage["total_tokens"].(float64); ok {
			out["total_tokens"] = int64(tt)
		}
		if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
			if rt, ok := details["reasoning_tokens"].(float64); ok {
				out["reasoning_tokens"] = int64(rt)
			}
		}
	}
	choices, _ := raw["choices"].([]any)
	if len(choices) == 0 {
		return out
	}
	choice, _ := choices[0].(map[string]any)
	if fr, ok := choice["finish_reason"].(string); ok {
		out["finish_reason"] = fr
	}
	msg, _ := choice["message"].(map[string]any)
	if msg == nil {
		return out
	}
	content, _ := msg["content"].(string)
	rc, _ := msg["reasoning_content"].(string)
	out["has_text"] = content != ""
	out["has_reasoning"] = rc != ""
	if tcs, ok := msg["tool_calls"].([]any); ok {
		out["tool_call_count"] = len(tcs)
	}
	return out
}

// SummarizeClaudeResult reduces a Claude Messages response body to a compact
// result summary for the request_result log record.
func SummarizeClaudeResult(body []byte) map[string]any {
	out := map[string]any{
		"has_text":           false,
		"has_reasoning":      false,
		"tool_call_count":    0,
		"promoted_reasoning": false,
		"stop_reason":        "",
		"total_tokens":       0,
		"reasoning_tokens":   0,
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return out
	}
	if sr, ok := raw["stop_reason"].(string); ok {
		out["stop_reason"] = sr
	}
	if usage, ok := raw["usage"].(map[string]any); ok {
		inTok, _ := usage["input_tokens"].(float64)
		outTok, _ := usage["output_tokens"].(float64)
		out["total_tokens"] = int64(inTok + outTok)
	}
	content, _ := raw["content"].([]any)
	for _, part := range content {
		block, ok := part.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			if t, _ := block["text"].(string); t != "" {
				out["has_text"] = true
			}
		case "thinking":
			if t, _ := block["thinking"].(string); t != "" {
				out["has_reasoning"] = true
			}
		case "tool_use":
			out["tool_call_count"] = out["tool_call_count"].(int) + 1
		}
	}
	return out
}
