package app

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
)

// normalizeFinishReason maps Anthropic stop reasons onto the closed set used
// by Chat Completions.
func normalizeFinishReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence", "stop":
		return "stop"
	case "max_tokens", "length":
		return "length"
	case "tool_use", "tool_calls", "function_call":
		return "tool_calls"
	case "refusal", "content_filter":
		return "content_filter"
	default:
		return reason
	}
}

func anthropicUsageToChat(usage map[string]any) map[string]any {
	if usage == nil {
		return nil
	}
	out := make(map[string]any, len(usage)+3)
	for k, v := range usage {
		out[k] = v
	}
	if v, ok := usage["input_tokens"]; ok {
		out["prompt_tokens"] = v
	}
	if v, ok := usage["output_tokens"]; ok {
		out["completion_tokens"] = v
	}
	if p, pok := numberAsFloat(out["prompt_tokens"]); pok {
		if c, cok := numberAsFloat(out["completion_tokens"]); cok {
			out["total_tokens"] = p + c
		}
	}
	delete(out, "input_tokens")
	delete(out, "output_tokens")
	return out
}

func numberAsFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// toString converts a value to string, returning "" for non-strings.
func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// validateTemperature checks an optional temperature against an inclusive
// [min, max] range. nil (absent) and any in-range value (including the
// bounds and explicit zero) are accepted; negative, above-max, NaN and Inf
// values are rejected. It never clamps. An empty message means valid.
func validateTemperature(t *float64, min, max float64) string {
	if t == nil {
		return ""
	}
	v := *t
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Sprintf("temperature must be a finite number; got %v", v)
	}
	if v < min || v > max {
		return fmt.Sprintf("temperature must be between %g and %g; got %v", min, max, v)
	}
	return ""
}

// writeProtocolValidation400 writes a protocol-shaped HTTP 400 error for a
// request-side validation failure. protocol is "chat", "responses", or
// "claude". param is the offending field name (Chat/Responses only).
// Streaming requests also receive a plain JSON 400 before any SSE.
func writeProtocolValidation400(w http.ResponseWriter, protocol, param, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	switch protocol {
	case "claude":
		json.NewEncoder(w).Encode(map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    "invalid_request_error",
				"message": message,
			},
		})
	default: // "chat", "responses"
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"type":    "invalid_request_error",
				"message": message,
				"param":   param,
			},
		})
	}
}

// validateRequestTemperature is the shared entry point used by all three
// handlers. It returns true when the request is valid (nil or in-range) and
// writes a protocol-shaped 400 (returning false) otherwise.
func validateRequestTemperature(w http.ResponseWriter, t *float64, protocol string, min, max float64) bool {
	if msg := validateTemperature(t, min, max); msg != "" {
		writeProtocolValidation400(w, protocol, "temperature", msg)
		return false
	}
	return true
}

// applyErrorPrefix prepends the stable "Error: " marker used by both the
// Anthropic Messages and Responses request paths when a tool_result carries
// is_error:true. It avoids producing a duplicate prefix when the output text
// already starts with "Error:" (e.g. an upstream that echoes the error).
func applyErrorPrefix(text string) string {
	if strings.HasPrefix(text, "Error:") {
		return text
	}
	return "Error: " + text
}
