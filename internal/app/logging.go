package app

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

	"gopkg.in/natefinch/lumberjack.v2"
)

var (
	logLevel      string
	logFile       string
	logStdout     bool
	logMaxSize    int
	logMaxBackups int
	logMaxAge     int
	logCompress   bool
	logBodies     bool

	logLevelVar      = &slog.LevelVar{}
	logBodiesMu      sync.RWMutex
	logBodiesEnabled bool
	logRotator       *lumberjack.Logger

	upstreamErrDedupMu sync.Mutex
	upstreamErrDedup   = map[string]upstreamErrDedupEntry{}
)

type upstreamErrDedupEntry struct {
	last       time.Time
	suppressed int
}

func setLogBodies(enabled bool) {
	logBodiesMu.Lock()
	logBodiesEnabled = enabled
	logBodiesMu.Unlock()
}

func getLogBodies() bool {
	logBodiesMu.RLock()
	defer logBodiesMu.RUnlock()
	return logBodiesEnabled
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

func setLogLevelString(s string) {
	logLevel = strings.ToLower(strings.TrimSpace(s))
	if logLevel == "" {
		logLevel = "info"
	}
	logLevelVar.Set(parseLogLevel(logLevel))
}

func getLogLevelString() string {
	switch logLevelVar.Level() {
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

// resolvedLogPath returns the absolute path of the active log file, or
// "(stdout)" when no file is configured. Used for user-facing status lines.
func resolvedLogPath() string {
	if logFile == "" {
		return "(stdout)"
	}
	abs, err := filepath.Abs(logFile)
	if err != nil {
		return logFile
	}
	return abs
}

func closeLogRotator() {
	if logRotator != nil {
		_ = logRotator.Close()
		logRotator = nil
	}
}

func initLogger() *slog.Logger {
	if debugMode && strings.EqualFold(logLevel, "info") {
		logLevel = "debug"
	}
	setLogLevelString(logLevel)
	setLogBodies(logBodies)

	var writers []io.Writer
	if logStdout {
		writers = append(writers, os.Stdout)
	}

	resolvedPath := ""
	if logFile != "" {
		absPath, absErr := filepath.Abs(logFile)
		if absErr != nil {
			absPath = logFile
		}
		dir := filepath.Dir(absPath)
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			fmt.Fprintf(os.Stderr, "cannot create log directory %s: %v; falling back to stdout\n", dir, mkErr)
			if !logStdout {
				writers = append(writers, os.Stdout)
			}
		} else {
			logRotator = &lumberjack.Logger{
				Filename:   absPath,
				MaxSize:    logMaxSize,
				MaxBackups: logMaxBackups,
				MaxAge:     logMaxAge,
				Compress:   logCompress,
				LocalTime:  true,
			}
			writers = append(writers, logRotator)
			resolvedPath = absPath
		}
	}

	if len(writers) == 0 {
		writers = append(writers, os.Stdout)
	}

	w := io.MultiWriter(writers...)
	handler := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level:       logLevelVar,
		ReplaceAttr: redactLogAttr,
	})
	logger := slog.New(handler)
	slog.SetDefault(logger)

	attrs := []any{
		"level", getLogLevelString(),
		"stdout", logStdout || resolvedPath == "",
		"log_bodies", getLogBodies(),
		"max_size_mb", logMaxSize,
		"max_backups", logMaxBackups,
		"max_age_days", logMaxAge,
		"compress", logCompress,
	}
	if resolvedPath != "" {
		attrs = append([]any{"path", resolvedPath}, attrs...)
	} else {
		attrs = append([]any{"path", "stdout"}, attrs...)
	}
	slog.Info("logging configured", attrs...)
	return logger
}

func reqLogger(ctx context.Context) *slog.Logger {
	id := getReqID(ctx)
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

func loggingMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if reqID == "" {
			reqID = randomString(12)
		}
		ctx := context.WithValue(r.Context(), reqIDKey, reqID)
		r = r.WithContext(ctx)
		w.Header().Set("X-Request-Id", reqID)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		log := reqLogger(ctx)
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

func authModeString(mode AuthRouteMode) string {
	switch mode {
	case AuthRouteGo:
		return "go"
	case AuthRouteZen:
		return "zen"
	case AuthRouteAuto:
		return "auto"
	default:
		return "public"
	}
}

func thinkingState(value any) string {
	if value == nil {
		return "absent"
	}
	if isThinkingDisabled(value) {
		return "disabled"
	}
	if m, ok := value.(map[string]any); ok {
		if t, _ := m["type"].(string); t == "adaptive" {
			return "adaptive"
		}
	}
	if isThinkingEnabled(value) {
		return "enabled"
	}
	return "present"
}

func mappedReasoningEffort(in string) string {
	if in == "" {
		return ""
	}
	effortMap := getReasoningEffortMap()
	if mapped, ok := effortMap[in]; ok {
		return mapped
	}
	return in
}

func logRequestPlan(ctx context.Context, fields map[string]any) {
	attrs := make([]any, 0, len(fields)*2)
	for k, v := range fields {
		attrs = append(attrs, k, v)
	}
	reqLogger(ctx).Info("request_plan", attrs...)
}

func logRequestResult(ctx context.Context, fields map[string]any) {
	attrs := make([]any, 0, len(fields)*2)
	for k, v := range fields {
		attrs = append(attrs, k, v)
	}
	reqLogger(ctx).Info("request_result", attrs...)
}

type streamResultStats struct {
	start             time.Time
	firstChunkAt      time.Time
	chunks            int
	textChars         int
	reasoningChars    int
	toolCallCount     int
	finishReason      string
	doneSeen          bool
	promotedReasoning bool
	sawFinish         bool
}

func (s *streamResultStats) noteChunk() {
	s.chunks++
	if s.firstChunkAt.IsZero() {
		s.firstChunkAt = time.Now()
	}
}

func (s *streamResultStats) observeDelta(delta map[string]any, keepReasoning bool) {
	if delta == nil {
		return
	}
	s.noteChunk()
	if c, ok := delta["content"].(string); ok {
		s.textChars += len(c)
	}
	if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
		s.reasoningChars += len(rc)
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
				s.promotedReasoning = true
				s.textChars += len(rc)
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
				s.toolCallCount++
			}
		}
	}
}

func (s *streamResultStats) log(ctx context.Context, protocol string) {
	if s.start.IsZero() {
		s.start = time.Now()
	}
	firstMs := int64(0)
	if !s.firstChunkAt.IsZero() {
		firstMs = s.firstChunkAt.Sub(s.start).Milliseconds()
	}
	emptyReply := s.textChars == 0 && s.toolCallCount == 0
	truncated := !s.doneSeen && !s.sawFinish
	attrs := []any{
		"protocol", protocol,
		"chunks", s.chunks,
		"first_chunk_ms", firstMs,
		"duration_ms", time.Since(s.start).Milliseconds(),
		"text_chars", s.textChars,
		"reasoning_chars", s.reasoningChars,
		"tool_call_count", s.toolCallCount,
		"finish_reason", s.finishReason,
		"done_seen", s.doneSeen,
		"truncated", truncated,
		"empty_reply", emptyReply,
		"promoted_reasoning", s.promotedReasoning,
	}
	log := reqLogger(ctx)
	if emptyReply {
		log.Warn("stream_result", attrs...)
		return
	}
	log.Info("stream_result", attrs...)
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
		out["thinking"] = thinkingState(t)
	}
	if oc, ok := obj["output_config"].(map[string]any); ok {
		if effort, _ := oc["effort"].(string); effort != "" {
			out["output_config_effort"] = effort
		}
	}
	if _, ok := obj["context_management"]; ok {
		out["context_management"] = true
	}
	if n := countCacheControlInValue(obj); n > 0 {
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

func maybeLogBodySummary(ctx context.Context, label string, raw []byte) {
	if !getLogBodies() || logLevelVar.Level() > slog.LevelDebug {
		return
	}
	reqLogger(ctx).Debug(label, "summary", summarizeJSONBody(raw, 4096))
}

func logUpstreamError(ctx context.Context, model string, status int, body []byte, baseURL string) {
	key := fmt.Sprintf("%s:%d:%s", model, status, baseURL)
	now := time.Now()
	upstreamErrDedupMu.Lock()
	entry, ok := upstreamErrDedup[key]
	if ok && now.Sub(entry.last) < 10*time.Second {
		entry.suppressed++
		upstreamErrDedup[key] = entry
		suppressed := entry.suppressed
		upstreamErrDedupMu.Unlock()
		reqLogger(ctx).Error("upstream error",
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
	reqLogger(ctx).Error("upstream error",
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

func summarizeChatResult(body []byte) map[string]any {
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

func summarizeClaudeResult(body []byte) map[string]any {
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
