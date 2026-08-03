package main

import (
	"encoding/json"
	"strings"
)

// convertClaudeRequest is the request-side protocol boundary. It returns a new
// Chat Completions request and never mutates values owned by the caller.
// skippedServerTools lists Anthropic server-tool names that were not forwarded.
func convertClaudeRequest(req ClaudeRequest) (OpenAIRequest, []string) {
	tools, skipped := claudeToOpenAITools(req.Tools)
	out := OpenAIRequest{
		Model: req.Model, Messages: claudeToOpenAIMessages(req.Messages, req.System),
		Stream: req.Stream, Temperature: req.Temperature, MaxTokens: req.MaxTokens,
		TopP: req.TopP, Tools: tools,
		ToolChoice: convertClaudeToolChoice(req.ToolChoice),
		Thinking:   req.Thinking,
	}
	// Claude Code puts effort in output_config.effort (--effort / CLAUDE_CODE_EFFORT_LEVEL).
	// Map it onto Chat Completions reasoning_effort so upstream mapping still applies.
	if effort := effortFromOutputConfig(req.OutputConfig); effort != "" {
		out.ReasoningEffort = effort
	}
	// Normalize adaptive thinking to an enabled object so budget/effort fields survive.
	if m, ok := req.Thinking.(map[string]any); ok {
		if t, _ := m["type"].(string); t == "adaptive" {
			normalized := map[string]any{"type": "enabled"}
			for _, key := range []string{"budget_tokens", "effort"} {
				if v, exists := m[key]; exists && v != nil {
					normalized[key] = v
				}
			}
			if effort := out.ReasoningEffort; effort != "" {
				if _, exists := normalized["effort"]; !exists {
					normalized["effort"] = effort
				}
			}
			out.Thinking = normalized
		}
	}
	if req.TopK != nil {
		if out.ExtraBody == nil {
			out.ExtraBody = map[string]any{}
		}
		out.ExtraBody["top_k"] = *req.TopK
	}
	if len(req.StopSequences) > 0 {
		if out.ExtraBody == nil {
			out.ExtraBody = map[string]any{}
		}
		out.ExtraBody["stop"] = append([]string(nil), req.StopSequences...)
	}
	if claudeToolChoiceDisablesParallel(req.ToolChoice) {
		if out.ExtraBody == nil {
			out.ExtraBody = map[string]any{}
		}
		out.ExtraBody["parallel_tool_calls"] = false
	}
	if user := narrowClaudeMetadataUser(req.Metadata); user != "" {
		if out.ExtraBody == nil {
			out.ExtraBody = map[string]any{}
		}
		out.ExtraBody["user"] = user
	}
	return out, skipped
}

func convertClaudeToolChoice(choice any) any {
	m, ok := choice.(map[string]any)
	if !ok {
		return choice
	}
	switch m["type"] {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if name, ok := m["name"].(string); ok && name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	return choice
}

func claudeToolChoiceDisablesParallel(choice any) bool {
	m, ok := choice.(map[string]any)
	if !ok {
		return false
	}
	v, ok := m["disable_parallel_tool_use"]
	if !ok {
		return false
	}
	switch b := v.(type) {
	case bool:
		return b
	default:
		return false
	}
}

// narrowClaudeMetadataUser extracts a safe upstream user id from Claude metadata.
// Claude Code packs device_id into user_id JSON; only session_id is forwarded.
func narrowClaudeMetadataUser(metadata any) string {
	m, ok := metadata.(map[string]any)
	if !ok {
		return ""
	}
	user, ok := m["user_id"].(string)
	if !ok || user == "" {
		return ""
	}
	user = strings.TrimSpace(user)
	if strings.HasPrefix(user, "{") {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(user), &parsed); err == nil {
			if sid, ok := parsed["session_id"].(string); ok && sid != "" {
				return sid
			}
			return ""
		}
	}
	return user
}
