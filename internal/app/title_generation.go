package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// OpenCode's title call is agent traffic with a known prompt and no tools. The
// free tier accepts that shape, but rejects it once an agent tool list is
// attached. Keep title calls out of the manifest path and leave tool_choice
// alone: the upstream expects the caller's tool-less shape.
const titlePromptV1 = "generate a title for this conversation"

var titlePromptsV2 = []string{
	"you name conversations so the user can find them later in a list of past sessions",
	"you generate a descriptive title for this conversation",
	"you decide whether the session title still matches the conversation",
	"reply with a title and nothing else",
	"reply with a title for it and nothing else",
}

const titleMaxMessages = 3

// isTitleGenerationRequest reports whether payload is OpenCode's session-title
// call rather than agent traffic. The shape check keeps ordinary tool-less
// chat requests (which are also common) from being mistaken for titles.
func isTitleGenerationRequest(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	if tools, ok := payload["tools"].([]any); ok && len(tools) > 0 {
		return false
	}
	if functions, ok := payload["functions"].([]any); ok && len(functions) > 0 {
		return false
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 || len(messages) > titleMaxMessages {
		return false
	}
	matched := false
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		role = strings.ToLower(strings.TrimSpace(role))
		if role == "assistant" || role == "tool" || role == "function" {
			return false
		}
		text := strings.ToLower(strings.TrimSpace(titleContentText(message["content"])))
		if text == "" {
			continue
		}
		switch role {
		case "system", "developer":
			for _, prompt := range titlePromptsV2 {
				if strings.Contains(text, prompt) {
					matched = true
					break
				}
			}
		case "user":
			if strings.HasPrefix(text, titlePromptV1) {
				matched = true
			}
		}
	}
	return matched
}

// titleContentText folds the string and text-part content shapes into one
// string. Title calls only carry text; non-text parts are ignored.
func titleContentText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, raw := range c {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			text, _ := part["text"].(string)
			if text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString(" ")
			}
			b.WriteString(text)
		}
		return b.String()
	}
	return ""
}

const (
	defaultTitle      = "New Session"
	titleMaxWords     = 6
	titleMaxRunes     = 40
	titleCompletionID = "chatcmpl-title-"
)

// synthesizeTitle derives a short title from the first user turn. The title
// call is cosmetic: when the upstream refuses it, returning a plausible local
// title is better than surfacing a 429 for a request the user never sees.
func synthesizeTitle(payload map[string]any) string {
	userText := ""
	messages, _ := payload["messages"].([]any)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); !strings.EqualFold(strings.TrimSpace(role), "user") {
			continue
		}
		if userText = strings.TrimSpace(titleContentText(message["content"])); userText != "" {
			break
		}
	}
	// The 1.x title call puts the prompt on its own line before the transcript.
	if strings.HasPrefix(strings.ToLower(userText), titlePromptV1) {
		if index := strings.Index(userText, "\n"); index >= 0 {
			userText = strings.TrimSpace(userText[index+1:])
		}
	}
	firstLine, _, _ := strings.Cut(userText, "\n")
	words := strings.Fields(firstLine)
	if len(words) > titleMaxWords {
		words = words[:titleMaxWords]
	}
	title := strings.Join(words, " ")
	if utf8.RuneCountInString(title) > titleMaxRunes {
		title = string([]rune(title)[:titleMaxRunes])
	}
	title = strings.Trim(title, "\"' \t\r\n.")
	if title == "" {
		return defaultTitle
	}
	return title
}

// writeSynthesizedTitle answers a title request the upstream rejected with its
// locally derived title. It writes the same wire shape the caller asked for:
// a Chat completion JSON body or an SSE stream ending in [DONE].
func writeSynthesizedTitle(w http.ResponseWriter, model, title string, stream bool) {
	created := time.Now().Unix()
	id := titleCompletionID + randomHex(8)
	usage := map[string]any{
		"prompt_tokens":     0,
		"completion_tokens": utf8.RuneCountInString(title),
		"total_tokens":      utf8.RuneCountInString(title),
	}
	completion := func(delta map[string]any, finishReason any) map[string]any {
		return map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			}},
		}
	}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      id,
			"object":  "chat.completion",
			"created": created,
			"model":   model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": title},
				"finish_reason": "stop",
			}},
			"usage": usage,
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	for _, chunk := range []map[string]any{
		completion(map[string]any{"role": "assistant", "content": title}, nil),
		completion(map[string]any{}, "stop"),
	} {
		_, _ = w.Write([]byte("data: "))
		_ = encoder.Encode(chunk)
		_, _ = w.Write([]byte("\n"))
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// writeTitleFallbackIfNeeded answers an upstream 429 for a recognized title
// call locally. It returns false for everything else, leaving the normal error
// path intact.
func writeTitleFallbackIfNeeded(w http.ResponseWriter, model string, rawBody []byte, status int, stream bool) bool {
	if status != http.StatusTooManyRequests {
		return false
	}
	var payload map[string]any
	if json.Unmarshal(rawBody, &payload) != nil || !isTitleGenerationRequest(payload) {
		return false
	}
	writeSynthesizedTitle(w, model, synthesizeTitle(payload), stream)
	return true
}
