package main

// convertClaudeRequest is the request-side protocol boundary. It returns a new
// Chat Completions request and never mutates values owned by the caller.
func convertClaudeRequest(req ClaudeRequest) OpenAIRequest {
	out := OpenAIRequest{
		Model: req.Model, Messages: claudeToOpenAIMessages(req.Messages, req.System),
		Stream: req.Stream, Temperature: req.Temperature, MaxTokens: req.MaxTokens,
		TopP: req.TopP, Tools: claudeToOpenAITools(req.Tools),
		ToolChoice: convertClaudeToolChoice(req.ToolChoice),
	}
	if len(req.StopSequences) > 0 {
		out.ExtraBody = map[string]any{"stop": append([]string(nil), req.StopSequences...)}
	}
	if metadata, ok := req.Metadata.(map[string]any); ok {
		if user, ok := metadata["user_id"].(string); ok && user != "" {
			if out.ExtraBody == nil {
				out.ExtraBody = map[string]any{}
			}
			out.ExtraBody["user"] = user
		}
	}
	return out
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
