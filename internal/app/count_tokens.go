package app

import (
	"encoding/json"
	"io"
	"net/http"
)

// ======================== Claude Messages count_tokens ========================
//
// POST /v1/messages/count_tokens is a local heuristic estimate of the input
// token count. It never calls the upstream and never incurs usage; Claude Code
// uses it for context-window management and auto-compaction, so a reasonable
// estimate is sufficient (see docs/claude-messages-compatibility-report.md).

const (
	// Content tokens: ~4 chars per token, matching the common BPE
	// compression for English.
	charsPerToken = 4
	// Structural overhead per message, system block, and tool definition.
	messageOverhead = 4
	systemOverhead  = 4
	toolOverhead    = 8
	// Fixed estimates for multimodal blocks (Anthropic's documented
	// approximation for images; flat fallback for documents).
	imageTokens    = 1600
	documentTokens = 3000
)

// estimateClaudeInputTokens returns a heuristic count of the input tokens a
// Claude Messages request would consume. It reads req without mutating it.
func estimateClaudeInputTokens(req ClaudeRequest) int {
	total := 0
	if sys := extractClaudeSystemText(req.System); sys != "" {
		// The system block carries structural overhead of its own.
		total = systemOverhead + estimateTextTokens(sys)
	}
	for _, msg := range req.Messages {
		total += messageOverhead
		total += estimateContentTokens(msg.Content)
	}
	for _, tool := range req.Tools {
		total += toolOverhead
		if tool.Description != "" {
			total += estimateTextTokens(tool.Description)
		}
		if b, err := json.Marshal(tool.InputSchema); err == nil {
			total += estimateTextTokens(string(b))
		}
	}
	if total <= 0 {
		return 1 // never return zero: a count of 0 would confuse the client
	}
	return total
}

// estimateContentTokens estimates the tokens in a single Anthropic content
// field, which may be a plain string, a block array, or an arbitrary JSON
// value.
func estimateContentTokens(content any) int {
	switch c := content.(type) {
	case string:
		return estimateTextTokens(c)
	case []any:
		total := 0
		for _, item := range c {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := block["type"].(string)
			switch blockType {
			case "text":
				if text, ok := block["text"].(string); ok {
					total += estimateTextTokens(text)
				}
			case "image":
				total += imageTokens
			case "document":
				total += documentTokens
			case "thinking", "redacted_thinking":
				if text, ok := block["thinking"].(string); ok {
					total += estimateTextTokens(text)
				}
				if data, ok := block["data"].(string); ok {
					total += estimateTextTokens(data)
				}
			case "tool_use":
				if name, ok := block["name"].(string); ok {
					total += estimateTextTokens(name)
				}
				if input := block["input"]; input != nil {
					total += estimateTextTokens(jsonString(input))
				}
			case "tool_result":
				if content := block["content"]; content != nil {
					total += estimateContentTokens(content)
				}
				if isErr, _ := block["is_error"].(bool); isErr {
					total += messageOverhead
				}
			default:
				// Unknown block: count its raw JSON to stay safe.
				total += estimateTextTokens(jsonString(block))
			}
		}
		return total
	default:
		if c == nil {
			return 0
		}
		return estimateTextTokens(jsonString(c))
	}
}

// jsonString serializes any value to a JSON string, falling back to "" on
// error. Used only for token estimation.
func jsonString(v any) string {
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return ""
}

// estimateTextTokens counts runes rather than bytes so multibyte text is not
// overcounted; rounds up so a non-empty short string always contributes 1.
func estimateTextTokens(s string) int {
	if s == "" {
		return 0
	}
	n := len([]rune(s))
	tokens := n / charsPerToken
	if n%charsPerToken != 0 {
		tokens++
	}
	return tokens
}

// claudeCountTokensHandler serves POST /v1/messages/count_tokens with a local
// heuristic estimate, without touching the upstream.
func claudeCountTokensHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	var claudeReq ClaudeRequest
	if err := json.Unmarshal(body, &claudeReq); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    "invalid_request_error",
				"message": "Invalid JSON",
			},
		})
		return
	}
	if claudeReq.Model == "" {
		writeProtocolValidation400(w, "claude", "", "model is required")
		return
	}

	inputTokens := estimateClaudeInputTokens(claudeReq)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]int{"input_tokens": inputTokens})
}