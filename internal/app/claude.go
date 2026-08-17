package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ======================== Claude Messages API ========================

func extractClaudeSystemText(system any) string {
	if system == nil {
		return ""
	}
	switch v := system.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if block, ok := item.(map[string]any); ok {
				if block["type"] == "text" {
					if text, ok := block["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func cleanJsonSchema(schema any) any {
	m, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	clean := make(map[string]any, len(m))
	for k, v := range m {
		// Annotation-only keys are omitted for upstream compatibility. Constraint
		// keys such as additionalProperties and format are preserved.
		if k == "$schema" || k == "title" || k == "examples" {
			continue
		}
		switch child := v.(type) {
		case map[string]any:
			clean[k] = cleanJsonSchema(child)
		case []any:
			copyArray := make([]any, len(child))
			for i, elem := range child {
				copyArray[i] = cleanJsonSchema(elem)
			}
			clean[k] = copyArray
		default:
			clean[k] = v
		}
	}
	return clean
}

func claudeImageBlockToOpenAI(block map[string]any) (map[string]any, bool) {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return nil, false
	}
	srcType, _ := source["type"].(string)
	mediaType, _ := source["media_type"].(string)
	data, _ := source["data"].(string)
	url, _ := source["url"].(string)
	if srcType == "url" && url != "" {
		return map[string]any{"type": "image_url", "image_url": map[string]string{"url": url}}, true
	}
	if srcType == "base64" && data != "" {
		if mediaType == "" {
			mediaType = "image/png"
		}
		return map[string]any{
			"type": "image_url",
			"image_url": map[string]string{
				"url": "data:" + mediaType + ";base64," + data,
			},
		}, true
	}
	return nil, false
}

// can surface a structured 400 instead of serializing the wrapper as text.
func claudeDocumentBlockToOpenAI(block map[string]any) (map[string]any, bool) {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return nil, false
	}
	srcType, _ := source["type"].(string)
	mediaType, _ := source["media_type"].(string)
	if mediaType == "" {
		mediaType = "application/pdf"
	}
	data, _ := source["data"].(string)
	url, _ := source["url"].(string)

	file := map[string]any{}
	if filename, ok := block["filename"].(string); ok && filename != "" {
		file["filename"] = filename
	} else if title, ok := block["title"].(string); ok && title != "" {
		file["filename"] = title
	}

	switch srcType {
	case "base64":
		if data == "" {
			return nil, false
		}
		file["file_data"] = "data:" + mediaType + ";base64," + data
		return map[string]any{"type": "file", "file": file}, true
	case "url":
		if url == "" {
			return nil, false
		}
		file["file_data"] = url
		return map[string]any{"type": "file", "file": file}, true
	}
	return nil, false
}

func extractClaudeContentText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if block["type"] == "text" {
				if text, ok := block["text"].(string); ok && text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func claudeToOpenAIMessages(claudeMsgs []ClaudeMessage, system any) []Message {
	var systemParts []string
	if sysText := extractClaudeSystemText(system); sysText != "" {
		systemParts = append(systemParts, sysText)
	}

	var body []Message
	for _, msg := range claudeMsgs {
		if msg.Role == "system" {
			if text := extractClaudeContentText(msg.Content); text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		}
		switch content := msg.Content.(type) {
		case string:
			body = append(body, Message{Role: msg.Role, Content: content})
		case []any:
			var orderedContent []any
			var reasoningParts []string
			var toolCalls []ToolCall
			var toolResults []Message
			var followupAttachments []any
			for _, item := range content {
				block, ok := item.(map[string]any)
				if !ok {
					continue
				}
				blockType, _ := block["type"].(string)
				switch blockType {
				case "text":
					if text, ok := block["text"].(string); ok && text != "" {
						orderedContent = append(orderedContent, map[string]any{"type": "text", "text": text})
					}
				case "image":
					if part, ok := claudeImageBlockToOpenAI(block); ok {
						orderedContent = append(orderedContent, part)
					}
				case "document":
					if part, ok := claudeDocumentBlockToOpenAI(block); ok {
						orderedContent = append(orderedContent, part)
					}
				case "thinking":
					if thinking, ok := block["thinking"].(string); ok && thinking != "" {
						reasoningParts = append(reasoningParts, thinking)
					}
				case "tool_use":
					id, _ := block["id"].(string)
					name, _ := block["name"].(string)
					var args string
					switch input := block["input"].(type) {
					case string:
						args = input
					default:
						if input != nil {
							b, _ := json.Marshal(input)
							args = string(b)
						}
					}
					if args == "" {
						args = "{}"
					}
					toolCalls = append(toolCalls, ToolCall{
						ID:   id,
						Type: "function",
						Function: FunctionCall{
							Name:      name,
							Arguments: args,
						},
					})
				case "tool_result":
					toolUseID, _ := block["tool_use_id"].(string)
					var resultText string
					var attachmentParts []any // local per-block image/document parts in original order
					switch c := block["content"].(type) {
					case string:
						resultText = c
					case []any:
						var parts []string
						for _, p := range c {
							pb, ok := p.(map[string]any)
							if !ok {
								continue
							}
							switch pb["type"] {
							case "text":
								if t, ok := pb["text"].(string); ok {
									parts = append(parts, t)
								}
							case "image":
								if part, ok := claudeImageBlockToOpenAI(pb); ok {
									attachmentParts = append(attachmentParts, part)
								}
							case "document":
								if part, ok := claudeDocumentBlockToOpenAI(pb); ok {
									attachmentParts = append(attachmentParts, part)
								}
							}
						}
						resultText = strings.Join(parts, "\n")
					default:
						if c != nil {
							b, _ := json.Marshal(c)
							resultText = string(b)
						}
					}
					// Annotate based on this block's own attachments, not a
					// global accumulator, so parallel tool_results are labeled
					// independently.
					if len(attachmentParts) > 0 {
						if resultText != "" {
							resultText += "\n"
						}
						var labels []string
						for _, ap := range attachmentParts {
							if m, ok := ap.(map[string]any); ok {
								if m["type"] == "image_url" {
									labels = append(labels, "[image attached]")
								} else if m["type"] == "file" {
									labels = append(labels, "[document attached]")
								}
							}
						}
						resultText += strings.Join(labels, "\n")
						followupAttachments = append(followupAttachments, attachmentParts...)
					}
					if isError, _ := block["is_error"].(bool); isError {
						resultText = applyErrorPrefix(resultText)
					}
					toolResults = append(toolResults, Message{
						Role:       "tool",
						ToolCallID: toolUseID,
						Content:    resultText,
					})
				}
			}
			om := Message{Role: msg.Role}
			if len(orderedContent) > 0 {
				om.Content = orderedContent
			} else if len(toolCalls) == 0 {
				om.Content = ""
			}
			if len(reasoningParts) > 0 {
				rc := strings.Join(reasoningParts, "\n")
				om.ReasoningContent = &rc
			}
			if len(toolCalls) > 0 {
				om.ToolCalls = toolCalls
			}
			// Anthropic requires tool_result blocks to precede ordinary user
			// content. Preserve that order when translating them to Chat
			// Completions' separate tool messages.
			if msg.Role == "user" {
				body = append(body, toolResults...)
				if len(followupAttachments) > 0 {
					body = append(body, Message{Role: "user", Content: followupAttachments})
				}
			}
			if len(orderedContent) > 0 || len(reasoningParts) > 0 || len(toolCalls) > 0 || len(toolResults) == 0 {
				body = append(body, om)
			}
			if msg.Role != "user" {
				body = append(body, toolResults...)
				if len(followupAttachments) > 0 {
					body = append(body, Message{Role: "user", Content: followupAttachments})
				}
			}
		default:
			b, _ := json.Marshal(content)
			body = append(body, Message{Role: msg.Role, Content: string(b)})
		}
	}

	var messages []Message
	if len(systemParts) > 0 {
		messages = append(messages, Message{Role: "system", Content: strings.Join(systemParts, "\n\n")})
	}
	messages = append(messages, body...)
	return messages
}

func claudeToOpenAITools(claudeTools []ClaudeTool) ([]Tool, []string) {
	tools := make([]Tool, 0, len(claudeTools))
	var skipped []string
	for _, ct := range claudeTools {
		// Server tools (web_search_*, etc.) carry a vendor type and no client schema.
		// Emitting them as empty function tools would invite bogus model calls.
		if ct.Type != "" && ct.InputSchema == nil {
			skipped = append(skipped, ct.Name)
			continue
		}
		params := ct.InputSchema
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		params = cleanJsonSchema(params)
		paramsMap, ok := params.(map[string]any)
		if !ok {
			paramsMap = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, Tool{
			Type: "function",
			Function: ToolFunction{
				Name:        ct.Name,
				Description: ct.Description,
				Parameters:  paramsMap,
			},
		})
	}
	return tools, skipped
}

func countClaudeSystemParts(msgs []ClaudeMessage, system any) int {
	n := 0
	if extractClaudeSystemText(system) != "" {
		n++
	}
	for _, msg := range msgs {
		if msg.Role == "system" && extractClaudeContentText(msg.Content) != "" {
			n++
		}
	}
	return n
}

func countAnthropicBetas(header string) int {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	n := 0
	for _, part := range strings.Split(header, ",") {
		if strings.TrimSpace(part) != "" {
			n++
		}
	}
	return n
}

// falsely counted as a breakpoint.
func countCacheControlInValue(v any) int {
	switch x := v.(type) {
	case map[string]any:
		n := 0
		if _, ok := x["cache_control"]; ok {
			n++
		}
		for key, child := range x {
			// Skip input_schema and input — cache_control inside these is a
			// schema/input property, not a content-block breakpoint.
			if key == "input_schema" || key == "input" {
				continue
			}
			n += countCacheControlInValue(child)
		}
		return n
	case []any:
		n := 0
		for _, child := range x {
			n += countCacheControlInValue(child)
		}
		return n
	default:
		return 0
	}
}

func countClaudeCacheControlBlocks(req ClaudeRequest) int {
	n := countCacheControlInValue(req.System)
	for _, msg := range req.Messages {
		n += countCacheControlInValue(msg.Content)
	}
	// Count actual tool-level cache_control breakpoints on tool definitions,
	// not properties named cache_control inside input_schema.
	for _, tool := range req.Tools {
		if tool.CacheControl != nil {
			n++
		}
	}
	return n
}

// no Chat Completions equivalent and are dropped upstream.
func countClaudeThinkingSignatures(msgs []ClaudeMessage) int {
	var n int
	for _, msg := range msgs {
		blocks, ok := msg.Content.([]any)
		if !ok {
			continue
		}
		for _, item := range blocks {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := block["type"].(string); t == "thinking" {
				if sig, ok := block["signature"].(string); ok && sig != "" {
					n++
				}
			}
		}
	}
	return n
}

// listed here so it is not counted as unsupported.
var claudeUnsupportedBlockTypes = map[string]struct{}{
	"redacted_thinking":               {},
	"search_result":                   {},
	"server_tool_use":                 {},
	"web_search_tool_result":          {},
	"container_upload":                {},
	"code_execution_tool_use":         {},
	"code_execution_tool_result":      {},
	"mcp_tool_use":                    {},
	"mcp_tool_result":                 {},
	"bash_code_execution_tool_result": {},
	"web_fetch_tool_result":           {},
	"tool_reference":                  {},
}

func scanClaudeUnsupportedBlocks(msgs []ClaudeMessage) map[string]int {
	counts := map[string]int{}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if t, _ := x["type"].(string); t != "" {
				if _, ok := claudeUnsupportedBlockTypes[t]; ok {
					counts[t]++
				}
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	for _, msg := range msgs {
		walk(msg.Content)
	}
	if len(counts) == 0 {
		return nil
	}
	return counts
}

func openAIToClaudeResponse(chatBody []byte, model string, wantReasoning bool) []byte {
	var chat struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Choices []struct {
			Message struct {
				Content          string     `json:"content"`
				ReasoningContent string     `json:"reasoning_content"`
				ToolCalls        []ToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		slog.Warn("openAIToClaudeResponse unmarshal failed", "error", err)
	}

	content := []ClaudeContent{}
	stopReason := "end_turn"

	if len(chat.Choices) > 0 {
		msg := chat.Choices[0].Message
		fr := chat.Choices[0].FinishReason

		// Try to read private ordered Anthropic content blocks first.
		var rawMsg map[string]any
		privateBlocks := []map[string]any(nil)
		if json.Unmarshal(chatBody, &rawMsg) == nil {
			if choices, ok := rawMsg["choices"].([]any); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]any); ok {
					if m, ok := choice["message"].(map[string]any); ok {
						if pb, ok := m["_opencode2api_anthropic_content"].([]any); ok {
							for _, item := range pb {
								if blk, ok := item.(map[string]any); ok {
									privateBlocks = append(privateBlocks, blk)
								}
							}
						}
					}
				}
			}
		}

		if len(privateBlocks) > 0 {
			// Consume private ordered blocks in array order.
			for _, blk := range privateBlocks {
				bt, _ := blk["type"].(string)
				switch bt {
				case "text":
					text, _ := blk["text"].(string)
					content = append(content, ClaudeContent{
						Type: "text",
						Text: text,
					})
				case "thinking":
					if wantReasoning {
						thinking, _ := blk["thinking"].(string)
						cc := ClaudeContent{
							Type:     "thinking",
							Thinking: thinking,
						}
						if sig, ok := blk["signature"].(string); ok && sig != "" {
							cc.Signature = sig
						}
						content = append(content, cc)
					}
				case "redacted_thinking":
					if wantReasoning {
						cc := ClaudeContent{
							Type: "redacted_thinking",
						}
						if d, ok := blk["data"].(string); ok && d != "" {
							cc.Data = d
						}
						content = append(content, cc)
					}
				case "tool_use":
					id, _ := blk["id"].(string)
					name, _ := blk["name"].(string)
					input := blk["input"]
					if input == nil {
						input = map[string]any{}
					}
					content = append(content, ClaudeContent{
						Type:  "tool_use",
						ID:    id,
						Name:  name,
						Input: input,
					})
				}
			}
		} else {
			// Fallback: string content + reasoning_content + tool_calls.
			if wantReasoning && msg.ReasoningContent != "" {
				content = append(content, ClaudeContent{
					Type:     "thinking",
					Thinking: msg.ReasoningContent,
				})
			}
			text := msg.Content
			// #37635: Go gateway often puts the whole answer in reasoning_content.
			// Promote to text when content is empty so Claude Code does not see an
			// empty end_turn and exit the agent loop.
			if text == "" && msg.ReasoningContent != "" && len(msg.ToolCalls) == 0 {
				text = msg.ReasoningContent
			}
			if text != "" {
				content = append(content, ClaudeContent{
					Type: "text",
					Text: text,
				})
			}
			for _, tc := range msg.ToolCalls {
				var input any
				json.Unmarshal([]byte(tc.Function.Arguments), &input)
				if input == nil {
					input = map[string]any{}
				}
				content = append(content, ClaudeContent{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: input,
				})
			}
		}

		switch fr {
		case "stop":
			stopReason = "end_turn"
		case "length":
			stopReason = "max_tokens"
		case "tool_calls", "function_call":
			stopReason = "tool_use"
		case "content_filter":
			stopReason = "refusal"
		}
	}

	if len(content) == 0 {
		content = append(content, ClaudeContent{Type: "text", Text: ""})
	}

	// Response ID: keep upstream ID only if it is a valid msg_ ID;
	// otherwise generate a new msg_ ID. Never leak chatcmpl/resp IDs.
	respID := normalizeClaudeMessageID(chat.ID)

	resp := ClaudeResponse{
		ID:           respID,
		Type:         "message",
		Role:         "assistant",
		Content:      content,
		Model:        model,
		StopReason:   stopReason,
		StopSequence: nil,
	}
	if chat.Usage != nil {
		resp.Usage = buildClaudeMessageUsage(chat.Usage)
	}
	result, _ := json.Marshal(resp)
	return result
}

func toFloat64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}

func usageIntField(fields map[string]any, key string) (int, bool) {
	if fields == nil {
		return 0, false
	}
	value, ok := fields[key]
	if !ok || value == nil {
		return 0, false
	}
	return int(toFloat64(value)), true
}

func usageMapField(fields map[string]any, key string) (map[string]any, bool) {
	if fields == nil {
		return nil, false
	}
	value, ok := fields[key]
	if !ok || value == nil {
		return nil, false
	}
	mapped, ok := value.(map[string]any)
	return mapped, ok
}

func buildClaudeUsageCore(upstreamUsage map[string]any) ClaudeUsage {
	if len(upstreamUsage) == 0 {
		return nil
	}

	usage := ClaudeUsage{}
	if value, ok := usageIntField(upstreamUsage, "prompt_tokens"); ok {
		usage["input_tokens"] = value
	}
	if value, ok := usageIntField(upstreamUsage, "input_tokens"); ok {
		if _, exists := usage["input_tokens"]; !exists {
			usage["input_tokens"] = value
		}
	}
	if value, ok := usageIntField(upstreamUsage, "completion_tokens"); ok {
		usage["output_tokens"] = value
	}
	if value, ok := usageIntField(upstreamUsage, "output_tokens"); ok {
		if _, exists := usage["output_tokens"]; !exists {
			usage["output_tokens"] = value
		}
	}
	if value, ok := usageIntField(upstreamUsage, "cache_creation_input_tokens"); ok {
		usage["cache_creation_input_tokens"] = value
	}
	if value, ok := usageIntField(upstreamUsage, "cache_read_input_tokens"); ok {
		usage["cache_read_input_tokens"] = value
	} else if promptDetails, ok := usageMapField(upstreamUsage, "prompt_tokens_details"); ok {
		if value, ok := usageIntField(promptDetails, "cached_tokens"); ok {
			usage["cache_read_input_tokens"] = value
		}
	}
	if outputDetails, ok := usageMapField(upstreamUsage, "output_tokens_details"); ok {
		usage["output_tokens_details"] = outputDetails
	} else if outputDetails, ok := usageMapField(upstreamUsage, "completion_tokens_details"); ok {
		usage["output_tokens_details"] = outputDetails
	}
	if serverToolUse, ok := usageMapField(upstreamUsage, "server_tool_use"); ok {
		usage["server_tool_use"] = serverToolUse
	}
	if len(usage) == 0 {
		return nil
	}
	return usage
}

func buildClaudeMessageUsage(upstreamUsage map[string]any) ClaudeUsage {
	usage := buildClaudeUsageCore(upstreamUsage)
	if usage == nil {
		usage = ClaudeUsage{}
	}
	if cacheCreation, ok := usageMapField(upstreamUsage, "cache_creation"); ok {
		usage["cache_creation"] = cacheCreation
	}
	if serviceTier, ok := upstreamUsage["service_tier"].(string); ok && serviceTier != "" {
		usage["service_tier"] = serviceTier
	}
	if inferenceGeo, ok := upstreamUsage["inference_geo"].(string); ok && inferenceGeo != "" {
		usage["inference_geo"] = inferenceGeo
	}
	if _, exists := usage["input_tokens"]; !exists {
		usage["input_tokens"] = 0
	}
	if _, exists := usage["output_tokens"]; !exists {
		usage["output_tokens"] = 0
	}
	return usage
}

func buildClaudeDeltaUsage(upstreamUsage map[string]any) ClaudeUsage {
	usage := buildClaudeUsageCore(upstreamUsage)
	if usage == nil {
		usage = ClaudeUsage{}
	}
	if _, exists := usage["output_tokens"]; !exists {
		usage["output_tokens"] = 0
	}
	return usage
}

func claudeMessagesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	auth := extractUpstreamAuth(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	cnt := requestCount.Add(1)
	maybeLogBodySummary(r.Context(), "claude messages request body", body)
	_ = cnt

	var claudeReq ClaudeRequest
	if err := json.Unmarshal(body, &claudeReq); err != nil {
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"Invalid JSON"}}`, http.StatusBadRequest)
		return
	}
	modelIn := claudeReq.Model
	claudeReq.Model = resolveModelForAuth(auth, claudeReq.Model)
	claudeReq.Model = mapPublicToFreeModel(auth, claudeReq.Model)
	if !validateRequestTemperature(w, claudeReq.Temperature, "claude", 0, 1) {
		return
	}
	if msg := validateClaudeDocumentBlocks(claudeReq.Messages); msg != "" {
		writeProtocolValidation400(w, "claude", "", msg)
		return
	}

	// 多模态路由

	chatReq, skippedServerTools := convertClaudeRequest(claudeReq)
	chatReq.Messages = fixToolCallGaps(chatReq.Messages)
	if claudeReq.Stream {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["stream_options"] = map[string]any{"include_usage": true}
	}

	// Keep CoT by default so Claude Code still sees thinking blocks. Only drop
	// reasoning when force-disabled or the client explicitly disables thinking.
	// Empty-reply protection is handled by promoteMisplacedReasoning (!keep)
	// and emitEmptyTextFallback (keep + no text/tool_use).
	wantReasoning := !getForceDisableThinking()
	if claudeReq.Thinking != nil && isThinkingDisabled(claudeReq.Thinking) {
		wantReasoning = false
	}
	keepReasoning := wantReasoning
	chatReq.Messages = ensureReasoningContent(chatReq.Messages, keepReasoning)

	effortIn := chatReq.ReasoningEffort
	if effortIn == "" && !isThinkingDisabled(claudeReq.Thinking) {
		effortIn = reasoningEffortFromThinking(claudeReq.Thinking)
	}
	upstreamSurface := "zen"
	if auth.shouldUseGoEndpoint(chatReq.Model) {
		upstreamSurface = "go"
	}
	systemMerged := countClaudeSystemParts(claudeReq.Messages, claudeReq.System) > 1
	plan := map[string]any{
		"protocol":                "claude",
		"model_in":                modelIn,
		"model_resolved":          chatReq.Model,
		"auth_mode":               authModeString(auth.Mode),
		"auth_source":             auth.Source,
		"has_key":                 auth.Token != "",
		"upstream_surface":        upstreamSurface,
		"stream":                  claudeReq.Stream,
		"keep_reasoning":          keepReasoning,
		"thinking":                thinkingState(claudeReq.Thinking),
		"reasoning_effort_in":     effortIn,
		"reasoning_effort_out":    mappedReasoningEffort(effortIn),
		"tools_count":             len(chatReq.Tools),
		"messages_count":          len(chatReq.Messages),
		"system_merged":           systemMerged,
		"context_management":      claudeReq.ContextManagement != nil,
		"cache_control_blocks":    countClaudeCacheControlBlocks(claudeReq),
		"history_signature_count": countClaudeThinkingSignatures(claudeReq.Messages),
		"client_beta_count":       countAnthropicBetas(r.Header.Get("anthropic-beta")),
		"unsupported_blocks":      scanClaudeUnsupportedBlocks(claudeReq.Messages),
		"max_tokens":              chatReq.MaxTokens,
		"max_tokens_cap":          getMaxTokensCapForModel(chatReq.Model),
	}
	if len(skippedServerTools) > 0 {
		plan["skipped_server_tools"] = skippedServerTools
	}
	logRequestPlan(r.Context(), plan)

	upstreamBody := buildUpstreamBody(&chatReq)

	if claudeReq.Stream {
		upResp, status, _, err := callOpenCodeAPIStream(r.Context(), upstreamBody, chatReq.Model, auth)
		if err != nil || status < 200 || status >= 300 {
			errResp := map[string]any{
				"type":  "error",
				"error": map[string]string{"type": "api_error", "message": "upstream error"},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(errResp)
			return
		}
		defer upResp.Close()
		claudeStreamHandler(r.Context(), w, upResp, claudeReq.Model, keepReasoning)
		return
	}

	respBody, status, _, err := callOpenCodeAPI(r.Context(), upstreamBody, chatReq.Model, auth)
	if err != nil || status < 200 || status >= 300 {
		if err != nil {
			writeUpstreamError(w, status, err, "claude")
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if len(respBody) > 0 {
				w.Write(respBody)
			} else {
				json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": "upstream error"}})
			}
		}
		return
	}

	claudeRespBody := openAIToClaudeResponse(respBody, claudeReq.Model, wantReasoning)
	result := summarizeClaudeResult(claudeRespBody)
	if !wantReasoning {
		var before map[string]any
		if json.Unmarshal(respBody, &before) == nil {
			if choices, ok := before["choices"].([]any); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]any); ok {
					if msg, ok := choice["message"].(map[string]any); ok {
						content, _ := msg["content"].(string)
						rc, _ := msg["reasoning_content"].(string)
						if content == "" && rc != "" {
							result["promoted_reasoning"] = true
						}
					}
				}
			}
		}
	}
	logRequestResult(r.Context(), result)

	// Record token usage
	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			pt, _ := u["prompt_tokens"].(float64)
			ct, _ := u["completion_tokens"].(float64)
			tt, _ := u["total_tokens"].(float64)
			if tt > 0 {
				recordTokenUsage(claudeReq.Model, int64(pt), int64(ct), int64(tt))
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	maybeLogBodySummary(r.Context(), "claude response body", claudeRespBody)
	w.Write(claudeRespBody)
}

var claudeKeepaliveInterval = 15 * time.Second

func claudeStreamHandler(ctx context.Context, w http.ResponseWriter, respBody io.ReadCloser, model string, keepReasoning bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(respBody)
	stats := &streamResultStats{start: time.Now()}

	msgID := fmt.Sprintf("msg_%s", randomString(24))
	blockIndex := 0
	thinkingBlockOpen := false
	textBlockOpen := false
	toolCallAccumulator := map[int]map[string]string{}
	toolBlockIndices := map[int]int{}
	toolCallOrder := []int{}
	messageStartSent := false
	finished := false
	stopReason := "end_turn"
	fullUsage := map[string]any{}
	// Accumulates reasoning when keepReasoning so we can fall back to a text
	// block if the stream never produces content/tool_use (#37635).
	reasoningFallback := strings.Builder{}

	// --- Reader goroutine -> channel so the main loop can select on
	// ticker/read/context without blocking, and so context cancellation
	// unblocks the reader via Close. ---
	type readResult struct {
		line string
		err  error
	}
	readCh := make(chan readResult)
	readerDone := make(chan struct{})
	readerExited := make(chan struct{})

	go func() {
		defer close(readerExited)
		for {
			line, err := reader.ReadString('\n')
			select {
			case readCh <- readResult{line: line, err: err}:
			case <-readerDone:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	keepaliveInterval := claudeKeepaliveInterval
	if keepaliveInterval <= 0 {
		keepaliveInterval = 15 * time.Second
	}
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()

	defer func() {
		if len(fullUsage) > 0 {
			pt, _ := fullUsage["prompt_tokens"].(float64)
			ct, _ := fullUsage["completion_tokens"].(float64)
			tt, _ := fullUsage["total_tokens"].(float64)
			if tt > 0 {
				recordTokenUsage(model, int64(pt), int64(ct), int64(tt))
			}
		}
		stats.toolCallCount = len(toolCallOrder)
		stats.log(ctx, "claude")
	}()
	// Reader cleanup: signal goroutine, unblock any pending read, wait for exit.
	defer func() {
		close(readerDone)
		respBody.Close()
		<-readerExited
	}()

	emitClaudeEvent := func(event string, data any) {
		jsonData, err := json.Marshal(data)
		if err != nil {
			reqLogger(ctx).Error("marshal SSE event failed", "error", err)
			return
		}
		w.Write([]byte("event: " + event + "\n"))
		w.Write([]byte("data: " + string(jsonData) + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	emitClaudeError := func(msg string) {
		emitClaudeEvent("error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": msg,
			},
		})
	}

	closeThinkingBlock := func() {
		if !thinkingBlockOpen {
			return
		}
		emitClaudeEvent("content_block_stop", map[string]any{
			"type":          "content_block_stop",
			"index":         blockIndex - 1,
			"content_block": map[string]any{"type": "thinking"},
		})
		thinkingBlockOpen = false
	}

	closeTextBlock := func() {
		if !textBlockOpen {
			return
		}
		emitClaudeEvent("content_block_stop", map[string]any{
			"type":          "content_block_stop",
			"index":         blockIndex - 1,
			"content_block": map[string]any{"type": "text"},
		})
		textBlockOpen = false
	}

	ensureMessageStart := func() {
		if messageStartSent {
			return
		}
		messageStartSent = true
		emitClaudeEvent("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            msgID,
				"type":          "message",
				"role":          "assistant",
				"content":       []any{},
				"model":         model,
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         buildClaudeMessageUsage(fullUsage),
			},
		})
		emitClaudeEvent("ping", map[string]any{"type": "ping"})
	}

	emitTextDelta := func(contentStr string) {
		if contentStr == "" {
			return
		}
		stats.textChars += len(contentStr)
		closeThinkingBlock()
		if !textBlockOpen {
			emitClaudeEvent("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": blockIndex,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			})
			textBlockOpen = true
			blockIndex++
		}
		emitClaudeEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": blockIndex - 1,
			"delta": map[string]any{
				"type": "text_delta",
				"text": contentStr,
			},
		})
	}

	emitEmptyTextFallback := func() {
		if textBlockOpen || len(toolCallOrder) > 0 {
			return
		}
		fallback := reasoningFallback.String()
		if fallback == "" {
			return
		}
		stats.promotedReasoning = true
		emitTextDelta(fallback)
	}

	finalizeContentBlocks := func() {
		emitEmptyTextFallback()
		closeThinkingBlock()
		closeTextBlock()
		for _, idx := range toolCallOrder {
			acc := toolCallAccumulator[idx]
			emitClaudeEvent("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": toolBlockIndices[idx],
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    acc["id"],
					"name":  acc["name"],
					"input": map[string]any{},
				},
			})
		}
	}

loop:
	for {
		select {
		case <-ctx.Done():
			// Client cancelled: quiet exit, no error writes.
			return
		case <-ticker.C:
			// Keepalive ping — before the first upstream token this is the
			// only thing the client receives; do NOT fake message_start.
			emitClaudeEvent("ping", map[string]any{"type": "ping"})
		case result := <-readCh:
			// bufio.ReadString may return both a non-empty line and an error
			// (e.g. the last line without a trailing newline + io.EOF). Process
			// the line first, then handle the accompanying error via pendingErr.
			pendingErr := result.err

			line := result.line
			trimmed := strings.TrimSpace(line)
			if trimmed == "data: [DONE]" || trimmed == "[DONE]" {
				stats.doneSeen = true
				if !finished {
					emitClaudeError("stream ended with [DONE] but no finish_reason")
					return
				}
				break loop
			}
			if strings.HasPrefix(line, "data: ") {
				payload := line[6:]
				if strings.TrimSpace(payload) != "" {
					var chunk map[string]any
					if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
						emitClaudeError("stream received malformed JSON data")
						return
					} else {
						// In-band error from upstream.
						if errVal, ok := chunk["error"]; ok && errVal != nil {
							errMsg := "upstream stream error"
							if errMap, ok := errVal.(map[string]any); ok {
								if m, ok := errMap["message"].(string); ok && m != "" {
									errMsg = m
								}
							} else if errStr, ok := errVal.(string); ok && errStr != "" {
								errMsg = errStr
							}
							emitClaudeError(errMsg)
							return
						} else {

							if usage, ok := chunk["usage"].(map[string]any); ok {
								fullUsage = mergeUsageMaps(fullUsage, usage)
							}

							choices, ok := chunk["choices"].([]any)
							if !ok || len(choices) == 0 {
								// Usage-only trailing chunk (OpenAI stream_options.include_usage).
							} else {
								choice, _ := choices[0].(map[string]any)
								delta, _ := choice["delta"].(map[string]any)
								finishReason, _ := choice["finish_reason"].(string)
								stats.noteChunk()

								ensureMessageStart()

								// After finish_reason, ignore further content deltas but keep reading
								// so a later usage-only chunk can populate fullUsage.
								if !finished {
									if rc, ok := delta["reasoning_content"]; ok {
										rcStr, _ := rc.(string)
										if rcStr != "" {
											stats.reasoningChars += len(rcStr)
											if keepReasoning {
												reasoningFallback.WriteString(rcStr)
												closeTextBlock()
												if !thinkingBlockOpen {
													emitClaudeEvent("content_block_start", map[string]any{
														"type":  "content_block_start",
														"index": blockIndex,
														"content_block": map[string]any{
															"type":     "thinking",
															"thinking": "",
														},
													})
													thinkingBlockOpen = true
													blockIndex++
												}
												emitClaudeEvent("content_block_delta", map[string]any{
													"type":  "content_block_delta",
													"index": blockIndex - 1,
													"delta": map[string]any{
														"type":     "thinking_delta",
														"thinking": rcStr,
													},
												})
											} else {
												// Thinking not requested: promote misplaced CoT to visible text (#37635).
												stats.promotedReasoning = true
												emitTextDelta(rcStr)
											}
										}
									}

									if c, ok := delta["content"]; ok && c != nil {
										contentStr, _ := c.(string)
										if contentStr != "" {
											emitTextDelta(contentStr)
										}
									}

									if rawToolCalls, ok := delta["tool_calls"].([]any); ok {
										for _, rawTC := range rawToolCalls {
											tc, ok := rawTC.(map[string]any)
											if !ok {
												continue
											}
											idxFloat, _ := tc["index"].(float64)
											upstreamIndex := int(idxFloat)

											closeThinkingBlock()
											closeTextBlock()

											if _, exists := toolCallAccumulator[upstreamIndex]; !exists {
												callID, _ := tc["id"].(string)
												if callID == "" {
													callID = "toolu_" + randomString(12)
												}
												fn, _ := tc["function"].(map[string]any)
												name, _ := fn["name"].(string)
												toolCallAccumulator[upstreamIndex] = map[string]string{
													"id":   callID,
													"name": name,
													"args": "",
												}
												toolCallOrder = append(toolCallOrder, upstreamIndex)
												toolBlockIndices[upstreamIndex] = blockIndex
												emitClaudeEvent("content_block_start", map[string]any{
													"type":  "content_block_start",
													"index": blockIndex,
													"content_block": map[string]any{
														"type":  "tool_use",
														"id":    callID,
														"name":  name,
														"input": map[string]any{},
													},
												})
												blockIndex++
											}

											fn, _ := tc["function"].(map[string]any)
											if argDelta, ok := fn["arguments"].(string); ok && argDelta != "" {
												toolCallAccumulator[upstreamIndex]["args"] += argDelta
												emitClaudeEvent("content_block_delta", map[string]any{
													"type":  "content_block_delta",
													"index": toolBlockIndices[upstreamIndex],
													"delta": map[string]any{
														"type":         "input_json_delta",
														"partial_json": argDelta,
													},
												})
											}
										}
									}

									if finishReason == "stop" || finishReason == "length" || finishReason == "tool_calls" || finishReason == "function_call" || finishReason == "content_filter" {
										stats.finishReason = finishReason
										stats.sawFinish = true
										finished = true
										finalizeContentBlocks()

										stopReason = "end_turn"
										switch finishReason {
										case "length":
											stopReason = "max_tokens"
										case "tool_calls", "function_call":
											stopReason = "tool_use"
										case "content_filter":
											stopReason = "refusal"
										}
										// Do not emit message_delta/stop yet: OpenAI-compatible upstreams often
										// send the usage-only chunk after finish_reason when include_usage=true.
									}
								}
							}
						}
					}
				}
			}

			// Now handle a pending error from the read.
			if pendingErr != nil {
				if pendingErr == io.EOF {
					if !finished {
						emitClaudeError("stream ended without finish_reason")
						return
					}
					break loop
				}
				reqLogger(ctx).Error("stream read error", "error", pendingErr)
				emitClaudeError("stream read error")
				return
			}
		}
	}

	// Reached only when finished is true (valid finish_reason seen).
	ensureMessageStart()
	emitClaudeEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": buildClaudeDeltaUsage(fullUsage),
	})
	emitClaudeEvent("message_stop", map[string]any{"type": "message_stop"})
}

func indexOfInt(slice []int, val int) int {
	for i, v := range slice {
		if v == val {
			return i
		}
	}
	return 0
}

// ======================== Anthropic 格式兼容 ========================

func isAnthropicFormat(body []byte) bool {
	var obj map[string]any
	if json.Unmarshal(body, &obj) == nil {
		if typ, _ := obj["type"].(string); typ == "message" {
			return true
		}
	}
	lines := bytes.Split(body, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		// Support "data: " prefixed SSE lines.
		if bytes.HasPrefix(line, []byte("data: ")) {
			line = bytes.TrimSpace(line[6:])
		} else if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(line[5:])
		}
		if len(line) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		typ, _ := event["type"].(string)
		switch typ {
		case "message_start", "content_block_start", "content_block_delta",
			"content_block_stop", "message_delta", "message_stop", "ping",
			"error":
			return true
		}
		return false
	}
	return false
}

// anthropicBlockState tracks per-index content block reconstruction.
type anthropicBlockState struct {
	blockType     string
	id            string
	name          string
	signature     string
	data          string
	textBuilder   strings.Builder
	thinkBuilder  strings.Builder
	jsonBuilder   strings.Builder
	initialInput  any
	sawInputDelta bool
	started       bool
	stopped       bool
}

// 0). Nested maps are recursively merged. Fields absent from src are retained.
func mergeUsageMaps(dst any, src map[string]any) map[string]any {
	if src == nil {
		if dm, ok := dst.(map[string]any); ok {
			return dm
		}
		return nil
	}
	var result map[string]any
	if dm, ok := dst.(map[string]any); ok {
		result = make(map[string]any, len(dm))
		for k, v := range dm {
			result[k] = v
		}
	} else {
		result = map[string]any{}
	}
	for k, v := range src {
		if existing, ok := result[k]; ok {
			if srcMap, ok := v.(map[string]any); ok {
				if existing != nil {
					result[k] = mergeUsageMaps(existing, srcMap)
					continue
				}
			}
		}
		result[k] = v
	}
	return result
}

// - malformed tool_use input JSON
func parseAnthropicSSE(body []byte) (map[string]any, []map[string]any, error) {
	lines := bytes.Split(body, []byte("\n"))
	var anthropicMsg map[string]any
	blocks := map[int]*anthropicBlockState{}
	sawMessageStop := false
	messageStartCount := 0

	for _, rawLine := range lines {
		line := bytes.TrimSpace(rawLine)
		if len(line) == 0 {
			continue
		}
		// Standard SSE metadata lines: "event: ...", "id: ...", comment ": ..."
		if bytes.HasPrefix(line, []byte("event:")) ||
			bytes.HasPrefix(line, []byte("id:")) ||
			bytes.HasPrefix(line, []byte(":")) {
			continue
		}
		// Support "data: " prefixed SSE lines.
		if bytes.HasPrefix(line, []byte("data: ")) {
			line = bytes.TrimSpace(line[6:])
		} else if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(line[5:])
		}
		if len(line) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, nil, fmt.Errorf("malformed SSE event JSON: %w", err)
		}
		typ, _ := event["type"].(string)

		// After message_stop, only ping events and comment/metadata lines
		// are allowed. Comment/metadata lines are already filtered above.
		// Any other event is an error.
		if sawMessageStop && typ != "ping" {
			return nil, nil, fmt.Errorf("unexpected event %q after message_stop", typ)
		}

		switch typ {
		case "message_start":
			messageStartCount++
			if messageStartCount > 1 {
				return nil, nil, fmt.Errorf("multiple message_start events in SSE stream")
			}
			m, ok := event["message"].(map[string]any)
			if !ok || m == nil {
				return nil, nil, fmt.Errorf("message_start missing non-nil message object")
			}
			anthropicMsg = m
		case "content_block_start":
			if messageStartCount == 0 {
				return nil, nil, fmt.Errorf("content_block_start before message_start")
			}
			idx, ok := extractBlockIndex(event)
			if !ok {
				return nil, nil, fmt.Errorf("content_block_start missing valid non-negative integer index")
			}
			if existing, ok := blocks[idx]; ok && existing.started {
				return nil, nil, fmt.Errorf("duplicate content_block_start for index %d", idx)
			}
			cb, _ := event["content_block"].(map[string]any)
			cbType, _ := cb["type"].(string)
			if cbType == "" {
				return nil, nil, fmt.Errorf("content_block_start missing content_block type")
			}
			if cbType != "text" && cbType != "thinking" && cbType != "redacted_thinking" && cbType != "tool_use" {
				return nil, nil, fmt.Errorf("content_block_start unsupported type %q", cbType)
			}
			st := &anthropicBlockState{blockType: cbType, started: true}
			if cb != nil {
				if id, ok := cb["id"].(string); ok {
					st.id = id
				}
				if name, ok := cb["name"].(string); ok {
					st.name = name
				}
				// tool_use must have a non-empty name.
				if cbType == "tool_use" && st.name == "" {
					return nil, nil, fmt.Errorf("tool_use content_block_start missing non-empty name")
				}
				if sig, ok := cb["signature"].(string); ok {
					st.signature = sig
				}
				if d, ok := cb["data"].(string); ok {
					st.data = d
				}
				// Preserve initial text if provided.
				if t, ok := cb["text"].(string); ok && t != "" {
					st.textBuilder.WriteString(t)
				}
				if t, ok := cb["thinking"].(string); ok && t != "" {
					st.thinkBuilder.WriteString(t)
				}
				// Preserve initial input if provided as a non-empty value.
				// In Anthropic SSE, content_block_start.input is typically {}
				// and the actual input arrives via input_json_delta partials.
				// Store separately so initial input and partial deltas are not
				// concatenated into invalid JSON.
				if input, ok := cb["input"]; ok && input != nil {
					if inputStr, ok := input.(string); ok && inputStr != "" {
						st.initialInput = inputStr
					} else if m, ok := input.(map[string]any); ok && len(m) > 0 {
						st.initialInput = input
					}
				}
			}
			blocks[idx] = st
		case "content_block_delta":
			if messageStartCount == 0 {
				return nil, nil, fmt.Errorf("content_block_delta before message_start")
			}
			idx, ok := extractBlockIndex(event)
			if !ok {
				return nil, nil, fmt.Errorf("content_block_delta missing valid non-negative integer index")
			}
			st, ok := blocks[idx]
			if !ok || !st.started {
				return nil, nil, fmt.Errorf("content_block_delta for unknown index %d", idx)
			}
			if st.stopped {
				return nil, nil, fmt.Errorf("content_block_delta for already-stopped index %d", idx)
			}
			delta, ok := event["delta"].(map[string]any)
			if !ok || delta == nil {
				return nil, nil, fmt.Errorf("content_block_delta for index %d missing delta object", idx)
			}
			dt, _ := delta["type"].(string)
			if dt == "" {
				return nil, nil, fmt.Errorf("content_block_delta for index %d missing delta type", idx)
			}
			switch dt {
			case "text_delta":
				if t, ok := delta["text"].(string); ok {
					st.textBuilder.WriteString(t)
				}
			case "thinking_delta":
				if t, ok := delta["thinking"].(string); ok {
					st.thinkBuilder.WriteString(t)
				}
			case "signature_delta":
				if sig, ok := delta["signature"].(string); ok {
					st.signature += sig
				}
			case "input_json_delta":
				if partial, ok := delta["partial_json"].(string); ok {
					st.jsonBuilder.WriteString(partial)
					st.sawInputDelta = true
				}
			default:
				// Unknown delta type: ignore.
			}
		case "content_block_stop":
			if messageStartCount == 0 {
				return nil, nil, fmt.Errorf("content_block_stop before message_start")
			}
			idx, ok := extractBlockIndex(event)
			if !ok {
				return nil, nil, fmt.Errorf("content_block_stop missing valid non-negative integer index")
			}
			st, ok := blocks[idx]
			if !ok || !st.started {
				return nil, nil, fmt.Errorf("content_block_stop for unknown index %d", idx)
			}
			if st.stopped {
				return nil, nil, fmt.Errorf("duplicate content_block_stop for index %d", idx)
			}
			st.stopped = true
		case "message_delta":
			if messageStartCount == 0 {
				return nil, nil, fmt.Errorf("message_delta before message_start")
			}
			if anthropicMsg == nil {
				anthropicMsg = map[string]any{}
			}
			if delta, ok := event["delta"].(map[string]any); ok {
				if stop, ok := delta["stop_reason"].(string); ok {
					anthropicMsg["stop_reason"] = stop
				}
			}
			// message_delta usage is at the event top level (Anthropic spec).
			if usage, ok := event["usage"].(map[string]any); ok {
				anthropicMsg["usage"] = mergeUsageMaps(anthropicMsg["usage"], usage)
			} else if delta, ok := event["delta"].(map[string]any); ok {
				if usage, ok := delta["usage"].(map[string]any); ok {
					anthropicMsg["usage"] = mergeUsageMaps(anthropicMsg["usage"], usage)
				}
			}
		case "message_stop":
			// Validate at message_stop time: must have message_start,
			// all blocks stopped, and non-empty stop_reason.
			if messageStartCount == 0 {
				return nil, nil, fmt.Errorf("message_stop before message_start")
			}
			for idx, st := range blocks {
				if st != nil && st.started && !st.stopped {
					return nil, nil, fmt.Errorf("message_stop with unclosed block at index %d", idx)
				}
			}
			stopReason, _ := anthropicMsg["stop_reason"].(string)
			if stopReason == "" {
				return nil, nil, fmt.Errorf("message_stop without stop_reason")
			}
			sawMessageStop = true
		case "error":
			errType := "api_error"
			errMsg := "upstream Anthropic error"
			if errMap, ok := event["error"].(map[string]any); ok {
				if t, ok := errMap["type"].(string); ok && t != "" {
					errType = t
				}
				if m, ok := errMap["message"].(string); ok && m != "" {
					errMsg = m
				}
			}
			return nil, nil, &anthropicProtocolError{errType: errType, message: errMsg}
		default:
			// Unknown event type: ignore.
		}
	}

	if messageStartCount == 0 {
		return nil, nil, fmt.Errorf("anthropic SSE stream missing message_start")
	}
	if !sawMessageStop {
		return nil, nil, fmt.Errorf("anthropic SSE stream ended without message_stop")
	}

	// Build ordered content blocks sorted by numeric index ascending.
	indices := make([]int, 0, len(blocks))
	for idx := range blocks {
		indices = append(indices, idx)
	}
	sort.Ints(indices)

	var contentBlocks []map[string]any
	for _, idx := range indices {
		st := blocks[idx]
		if st == nil {
			continue
		}
		switch st.blockType {
		case "text":
			contentBlocks = append(contentBlocks, map[string]any{
				"type": "text",
				"text": st.textBuilder.String(),
			})
		case "thinking":
			blk := map[string]any{
				"type":     "thinking",
				"thinking": st.thinkBuilder.String(),
			}
			if st.signature != "" {
				blk["signature"] = st.signature
			}
			contentBlocks = append(contentBlocks, blk)
		case "redacted_thinking":
			blk := map[string]any{
				"type": "redacted_thinking",
			}
			if st.data != "" {
				blk["data"] = st.data
			}
			contentBlocks = append(contentBlocks, blk)
		case "tool_use":
			var input any
			if st.sawInputDelta {
				// Parse accumulated partial JSON deltas.
				inputStr := st.jsonBuilder.String()
				if inputStr != "" {
					var parsed any
					if err := json.Unmarshal([]byte(inputStr), &parsed); err != nil {
						return nil, nil, fmt.Errorf("malformed tool_use input JSON for index %d: %w", idx, err)
					}
					input = parsed
				} else {
					input = map[string]any{}
				}
			} else if st.initialInput != nil {
				// Use initial input from content_block_start.
				if inputStr, ok := st.initialInput.(string); ok {
					var parsed any
					if err := json.Unmarshal([]byte(inputStr), &parsed); err != nil {
						return nil, nil, fmt.Errorf("malformed tool_use initial input for index %d: %w", idx, err)
					}
					input = parsed
				} else {
					input = st.initialInput
				}
			} else {
				input = map[string]any{}
			}
			blk := map[string]any{
				"type":  "tool_use",
				"input": input,
			}
			if st.id != "" {
				blk["id"] = st.id
			}
			if st.name != "" {
				blk["name"] = st.name
			}
			contentBlocks = append(contentBlocks, blk)
		default:
			// Unknown block type: skip.
		}
	}

	if anthropicMsg == nil {
		anthropicMsg = map[string]any{}
	}
	return anthropicMsg, contentBlocks, nil
}

// undefined/saturating behavior on overflow.
func extractBlockIndex(event map[string]any) (int, bool) {
	rawIdx, ok := event["index"]
	if !ok || rawIdx == nil {
		return 0, false
	}
	f, ok := rawIdx.(float64)
	if !ok {
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	if math.Trunc(f) != f || f < 0 {
		return 0, false
	}
	// Check platform int range BEFORE converting, to avoid overflow.
	// On 64-bit: maxInt = 2^63-1; on 32-bit: maxInt = 2^31-1.
	// float64 can't represent all int64 values, so also cap at 2^53
	// (where all integers are exactly representable as float64).
	maxInt := float64(1<<(strconv.IntSize-1) - 1)
	if f > maxInt {
		return 0, false
	}
	// Also reject values above the float64 exact-integer upper bound.
	if f > float64(1<<53) {
		return 0, false
	}
	idx := int(f)
	if float64(idx) != f {
		return 0, false
	}
	return idx, true
}
