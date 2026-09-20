package app

import (
	"encoding/json"
	"fmt"
	"github.com/6Kmfi6HP/opencode2api/internal/bridge"
	"github.com/6Kmfi6HP/opencode2api/internal/util"
	"math"
	"net/http"
)

// normalizeFinishReason maps Anthropic stop reasons onto the closed set used
// by Chat Completions. Thin shell forwarding to bridge.
func normalizeFinishReason(reason string) string {
	return bridge.NormalizeFinishReason(reason)
}

func anthropicUsageToChat(usage map[string]any) map[string]any {
	return bridge.AnthropicUsageToChat(usage)
}

func numberAsFloat(v any) (float64, bool) { return util.NumberAsFloat(v) }

// toString converts a value to string, returning "" for non-strings.
func toString(v any) string { return util.ToString(v) }

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
// is_error:true. Thin shell forwarding to bridge.
func applyErrorPrefix(text string) string { return bridge.ApplyErrorPrefix(text) }
