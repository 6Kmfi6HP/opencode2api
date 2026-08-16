package app

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// ======================== Responses API ========================

func responsesInputToMessages(input any, instructions string) []Message {
	var messages []Message
	if instructions != "" {
		messages = append(messages, Message{Role: "system", Content: instructions})
	}
	switch v := input.(type) {
	case string:
		messages = append(messages, Message{Role: "user", Content: v})
	case []any:
		functionOutputs := collectFunctionOutputs(v)
		// Pre-collect call IDs present in this input array so output items
		// whose matching call is also present are not independently appended
		// (the call branch emits the paired tool message). Standalone outputs
		// (e.g. previous-response-id replay) have no matching call and still
		// append independently. Uses the same call-ID extraction rule as the
		// call branch: call_id → id → nested tool_use.id.
		callIDsPresent := map[string]bool{}
		for _, item := range v {
			elem, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch elem["type"] {
			case "function_call", "tool_call", "apply_patch_call", "shell_call":
				cid, _ := elem["call_id"].(string)
				if cid == "" {
					cid, _ = elem["id"].(string)
				}
				if cid == "" {
					if tu, ok := elem["tool_use"].(map[string]any); ok {
						cid, _ = tu["id"].(string)
					}
				}
				if cid != "" {
					callIDsPresent[cid] = true
				}
			}
		}
		for _, item := range v {
			switch elem := item.(type) {
			case string:
				messages = append(messages, Message{Role: "user", Content: elem})
			case map[string]any:
				itemType, _ := elem["type"].(string)
				switch itemType {
				case "function_call", "tool_call", "apply_patch_call", "shell_call":
					callID, _ := elem["call_id"].(string)
					if callID == "" {
						callID, _ = elem["id"].(string)
					}
					name, _ := elem["name"].(string)
					if name == "" {
						switch itemType {
						case "apply_patch_call":
							name = "apply_patch"
						case "shell_call":
							name = "shell"
						}
					}
					args, _ := elem["arguments"].(string)
					if name == "" {
						if tu, ok := elem["tool_use"].(map[string]any); ok {
							name, _ = tu["name"].(string)
							callID, _ = tu["id"].(string)
							if a, ok := tu["arguments"].(string); ok {
								args = a
							} else if inp, ok := tu["input"]; ok {
								b, _ := json.Marshal(inp)
								args = string(b)
							}
						}
					}
					if args == "" {
						args = buildBuiltInToolCallArguments(itemType, elem)
					}
					if args == "" {
						args = "{}"
					}
					messages = append(messages, Message{
						Role:    "assistant",
						Content: "",
						ToolCalls: []ToolCall{{
							ID:   callID,
							Type: "function",
							Function: FunctionCall{
								Name:      name,
								Arguments: args,
							},
						}},
					})
					if callID != "" {
						// Map presence (not value=="") decides whether a payload
						// was provided: an empty string is a legitimate output.
						output, hasOutput := functionOutputs[callID]
						if !hasOutput {
							output = "[tool output missing]"
						}
						messages = append(messages, Message{Role: "tool", ToolCallID: callID, Content: output})
					}
				case "function_call_output", "tool_result", "apply_patch_call_output", "shell_call_output":
					callID, _ := elem["call_id"].(string)
					if callID == "" {
						callID, _ = elem["tool_use_id"].(string)
					}
					if callID != "" {
						// If the matching call item is also present in this
						// input array, skip independent emission — the call
						// branch will emit the paired assistant+tool messages,
						// preventing a leading duplicate tool message when
						// output precedes call. Standalone outputs (no matching
						// call, e.g. previous-response-id replay) still append
						// independently.
						if callIDsPresent[callID] {
							continue
						}
						// Map presence (not value=="") decides whether a payload
						// was provided: an empty string is a legitimate output.
						output, hasOutput := functionOutputs[callID]
						if !hasOutput {
							// Fallback for items not collected (e.g. output
							// field absent on a standard *_call_output). Use
							// the single normalizer so Anthropic-style content
							// is honored and the raw tool_result wrapper JSON
							// is never emitted.
							text, present := normalizeToolResultOutput(elem)
							if present {
								output = text
								hasOutput = true
							}
						}
						if !hasOutput {
							output = "[tool output missing]"
						}
						messages = append(messages, Message{Role: "tool", ToolCallID: callID, Content: output})
					}
					continue
				case "reasoning":
					if text := extractTextFromContentParts(elem["summary"]); text != "" {
						messages = append(messages, Message{Role: "assistant", Content: "", ReasoningContent: &text})
					}
					continue
				case "message", "":
					role := "user"
					if r, ok := elem["role"].(string); ok && r != "" {
						role = r
					}
					if role == "developer" {
						role = "system"
					}
					content := responsesContentToMessageContent(elem["content"])
					messages = append(messages, Message{Role: role, Content: content})
				case "input_file":
					// Top-level input_file item (file upload). Map to a user
					// message carrying a structured file part. Malformed items
					// (no payload) are rejected earlier by the handler, so a
					// failure here is dropped rather than serialized as text.
					if file, ok := responsesInputFileToFile(elem); ok {
						messages = append(messages, Message{
							Role:    "user",
							Content: []any{map[string]any{"type": "file", "file": file}},
						})
					}
					continue
				default:
					role := "user"
					if r, ok := elem["role"].(string); ok && r != "" {
						role = r
					}
					content := responsesContentToMessageContent(elem["content"])
					emptyContent := false
					switch v := content.(type) {
					case nil:
						emptyContent = true
					case string:
						emptyContent = v == ""
					case []any:
						emptyContent = len(v) == 0
					}
					if emptyContent {
						b, err := json.Marshal(elem)
						if err != nil {
							continue
						}
						content = string(b)
					}
					messages = append(messages, Message{Role: role, Content: content})
				}
			default:
				b, _ := json.Marshal(elem)
				messages = append(messages, Message{Role: "user", Content: string(b)})
			}
		}
	default:
		b, _ := json.Marshal(v)
		messages = append(messages, Message{Role: "user", Content: string(b)})
	}
	return messages
}

func convertResponsesTools(tools []ResponsesTool) []Tool {
	converted := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		fn, ok := responsesToolFunction(tool)
		if !ok {
			continue
		}
		converted = append(converted, Tool{Type: "function", Function: fn})
	}
	return converted
}

// upstream would reject with a 400.
func convertResponsesTextToResponseFormat(text any) any {
	obj, ok := text.(map[string]any)
	if !ok {
		return nil
	}
	format, ok := obj["format"].(map[string]any)
	if !ok {
		// Only verbosity was provided (no format) — nothing to map.
		return nil
	}
	typ, _ := format["type"].(string)
	switch typ {
	case "text", "json_object":
		return map[string]any{"type": typ}
	case "json_schema":
		jsonSchema := map[string]any{}
		if name, ok := format["name"].(string); ok && name != "" {
			jsonSchema["name"] = name
		}
		if desc, ok := format["description"].(string); ok {
			jsonSchema["description"] = desc
		}
		if schema, ok := format["schema"]; ok {
			jsonSchema["schema"] = schema
		}
		if strict, ok := format["strict"]; ok {
			jsonSchema["strict"] = strict
		}
		// name and schema are required by both APIs; without them upstream
		// would reject the object, so drop the format entirely.
		if _, hasName := jsonSchema["name"]; !hasName {
			return nil
		}
		if _, hasSchema := jsonSchema["schema"]; !hasSchema {
			return nil
		}
		return map[string]any{"type": "json_schema", "json_schema": jsonSchema}
	default:
		return nil
	}
}

func responsesToolFunction(tool ResponsesTool) (ToolFunction, bool) {
	switch tool.Type {
	case "function":
		fn := ToolFunction{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  tool.Parameters,
		}
		if tool.Function != nil {
			fn = *tool.Function
		}
		if fn.Parameters == nil {
			fn.Parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		return fn, true
	case "apply_patch":
		return ToolFunction{
			Name:        "apply_patch",
			Description: "Create, update, or delete files using a structured patch operation or unified diff.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"input": map[string]any{
						"type":        "string",
						"description": "Patch diff or patch instructions to apply.",
					},
					"operation": map[string]any{
						"type":        "object",
						"description": "Structured patch operation, including file action and diff payload.",
					},
				},
			},
		}, true
	case "shell":
		return ToolFunction{
			Name:        "shell",
			Description: "Run a shell command in the local workspace and return stdout, stderr, and exit details.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "Shell command to execute.",
					},
					"timeout_ms": map[string]any{
						"type":        "integer",
						"description": "Optional timeout in milliseconds.",
					},
					"working_directory": map[string]any{
						"type":        "string",
						"description": "Optional working directory for the command.",
					},
					"max_output_tokens": map[string]any{
						"type":        "integer",
						"description": "Optional output budget hint.",
					},
				},
				"required": []string{"command"},
			},
		}, true
	default:
		return ToolFunction{}, false
	}
}

func responsesToolName(tool ResponsesTool) string {
	switch tool.Type {
	case "function":
		if tool.Function != nil && tool.Function.Name != "" {
			return tool.Function.Name
		}
		return tool.Name
	case "apply_patch":
		return "apply_patch"
	case "shell":
		return "shell"
	default:
		return ""
	}
}

func responsesToolKindMap(tools []ResponsesTool) map[string]string {
	kinds := make(map[string]string, len(tools))
	for _, tool := range tools {
		name := responsesToolName(tool)
		if name == "" {
			continue
		}
		kinds[name] = tool.Type
	}
	return kinds
}

// includeHas reports whether the include array contains the given key.
func includeHas(include []string, key string) bool {
	for _, v := range include {
		if v == key {
			return true
		}
	}
	return false
}

func toolCallOutputType(name string, kinds map[string]string) string {
	switch kinds[name] {
	case "apply_patch":
		return "apply_patch_call"
	case "shell":
		return "shell_call"
	default:
		return "function_call"
	}
}

func convertResponsesToolChoice(choice any) any {
	if choice == nil {
		return nil
	}
	choiceMap, ok := choice.(map[string]any)
	if !ok {
		return choice
	}
	if choiceMap["type"] == "function" {
		if name, ok := choiceMap["name"].(string); ok && name != "" {
			return map[string]any{
				"type":     "function",
				"function": map[string]any{"name": name},
			}
		}
	}
	if choiceType, ok := choiceMap["type"].(string); ok {
		switch choiceType {
		case "apply_patch", "shell":
			return map[string]any{
				"type":     "function",
				"function": map[string]any{"name": choiceType},
			}
		}
	}
	return choice
}

// Responses entrypoint in addition to the standard *_call_output types.
var toolResultOutputKind = map[string]struct{}{
	"function_call_output":    {},
	"apply_patch_call_output": {},
	"shell_call_output":       {},
	"tool_result":             {},
}

func collectFunctionOutputs(items []any) map[string]string {
	outputs := map[string]string{}
	for _, item := range items {
		elem, ok := item.(map[string]any)
		if !ok {
			continue
		}
		itemType, _ := elem["type"].(string)
		if _, ok := toolResultOutputKind[itemType]; !ok {
			continue
		}
		// Standard Responses items use call_id; Anthropic-style tool_result
		// uses tool_use_id when call_id is absent.
		callID, _ := elem["call_id"].(string)
		if callID == "" {
			callID, _ = elem["tool_use_id"].(string)
		}
		if callID == "" {
			continue
		}
		text, present := normalizeToolResultOutput(elem)
		if present {
			outputs[callID] = text
		}
		// When no payload is present, the key is left absent so the caller
		// surfaces "[tool output missing]" — the raw wrapper JSON is never
		// stored as the output.
	}
	return outputs
}

// present (an empty string is a legitimate provided output).
func normalizeToolResultOutput(elem map[string]any) (string, bool) {
	var text string
	present := false
	// Standard `output` field takes priority.
	if v, ok := elem["output"]; ok && v != nil {
		switch s := v.(type) {
		case string:
			text = s
		default:
			b, _ := json.Marshal(v)
			text = string(b)
		}
		present = true
	} else if c, ok := elem["content"]; ok && c != nil {
		// Anthropic-style tool_result uses `content`.
		text = joinToolResultContent(c)
		present = true
	}
	if !present {
		return "", false
	}
	// Apply is_error prefix here so the collected map already carries error
	// semantics, independent of call/output ordering in the array.
	if isError, _ := elem["is_error"].(bool); isError {
		text = applyErrorPrefix(text)
	}
	return text, true
}

// joinToolResultContent renders an Anthropic tool_result content value to text.
func joinToolResultContent(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			pb, ok := p.(map[string]any)
			if !ok {
				if s, ok := p.(string); ok {
					parts = append(parts, s)
				}
				continue
			}
			switch pb["type"] {
			case "text", "input_text", "output_text":
				if t, ok := pb["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		if c != nil {
			b, _ := json.Marshal(c)
			return string(b)
		}
		return ""
	}
}

func parseJSONString(input string) any {
	var parsed any
	if input == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(input), &parsed); err != nil {
		return nil
	}
	return parsed
}

func buildBuiltInToolCallArguments(itemType string, elem map[string]any) string {
	if arguments, ok := elem["arguments"].(string); ok && arguments != "" {
		return arguments
	}

	payload := map[string]any{}
	switch itemType {
	case "apply_patch_call":
		if input, ok := elem["input"].(string); ok && input != "" {
			payload["input"] = input
		}
		if operation, ok := elem["operation"]; ok && operation != nil {
			payload["operation"] = operation
		}
	case "shell_call":
		for _, key := range []string{"command", "timeout_ms", "working_directory", "max_output_tokens"} {
			if value, ok := elem[key]; ok && value != nil {
				payload[key] = value
			}
		}
	}
	if len(payload) == 0 {
		payload = elem
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func buildResponseToolCallItem(tc ToolCall, outputType string) map[string]any {
	switch outputType {
	case "apply_patch_call":
		item := map[string]any{
			"id":      "apc_" + tc.ID,
			"type":    outputType,
			"status":  "completed",
			"call_id": tc.ID,
		}
		if parsed, ok := parseJSONString(tc.Function.Arguments).(map[string]any); ok {
			for key, value := range parsed {
				item[key] = value
			}
		} else if tc.Function.Arguments != "" {
			item["arguments"] = tc.Function.Arguments
		}
		return item
	case "shell_call":
		item := map[string]any{
			"id":      "shc_" + tc.ID,
			"type":    outputType,
			"status":  "completed",
			"call_id": tc.ID,
		}
		if parsed, ok := parseJSONString(tc.Function.Arguments).(map[string]any); ok {
			for key, value := range parsed {
				item[key] = value
			}
		} else if tc.Function.Arguments != "" {
			item["arguments"] = tc.Function.Arguments
		}
		return item
	default:
		return map[string]any{
			"id":        "fc_" + tc.ID,
			"type":      "function_call",
			"status":    "completed",
			"arguments": tc.Function.Arguments,
			"call_id":   tc.ID,
			"name":      tc.Function.Name,
		}
	}
}

func cloneJSONValue[T any](value T) T {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var cloned T
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		return value
	}
	return cloned
}

func storeResponseState(response map[string]any, req ResponsesAPIRequest) {
	if req.Store != nil && !*req.Store {
		return
	}
	responseID, _ := response["id"].(string)
	if responseID == "" {
		return
	}
	output, _ := response["output"].([]any)
	storedResponsesMu.Lock()
	storedResponses[responseID] = StoredResponseState{
		Model:        req.Model,
		Instructions: req.Instructions,
		Tools:        cloneJSONValue(req.Tools),
		ToolChoice:   cloneJSONValue(req.ToolChoice),
		Output:       cloneJSONValue(output),
	}
	storedResponsesMu.Unlock()
}

func loadResponseState(responseID string) (StoredResponseState, bool) {
	storedResponsesMu.RLock()
	defer storedResponsesMu.RUnlock()
	state, ok := storedResponses[responseID]
	if !ok {
		return StoredResponseState{}, false
	}
	return cloneJSONValue(state), true
}

func extractTextFromContentParts(content any) string {
	parts, ok := content.([]any)
	if !ok {
		if s, ok := content.(string); ok {
			return s
		}
		return ""
	}
	var texts []string
	for _, p := range parts {
		if part, ok := p.(map[string]any); ok {
			if part["type"] == "input_text" || part["type"] == "output_text" {
				if t, ok := part["text"].(string); ok {
					texts = append(texts, t)
				}
			}
		}
	}
	return strings.Join(texts, "\n")
}

func convertResponsesContentPart(part map[string]any) (map[string]any, bool) {
	partType, _ := part["type"].(string)
	switch partType {
	case "input_text", "output_text", "text":
		text, _ := part["text"].(string)
		if text == "" {
			return nil, false
		}
		return map[string]any{
			"type": "text",
			"text": text,
		}, true
	case "input_image":
		imageURL, _ := part["image_url"].(string)
		if imageURL == "" {
			return nil, false
		}
		imageURLValue := map[string]any{
			"url": imageURL,
		}
		if detail, ok := part["detail"].(string); ok && detail != "" {
			imageURLValue["detail"] = detail
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": imageURLValue,
		}, true
	case "input_file":
		file, ok := responsesInputFileToFile(part)
		if !ok {
			return nil, false
		}
		return map[string]any{"type": "file", "file": file}, true
	default:
		return nil, false
	}
}

// into tool_use input, document source, schemas, or arbitrary domain data.
func validateClaudeDocumentBlocks(msgs []ClaudeMessage) string {
	for _, msg := range msgs {
		if m := validateClaudeDocumentBlocksContent(msg.Content); m != "" {
			return m
		}
	}
	return ""
}

// tool_use input or other arbitrary map values.
func validateClaudeDocumentBlocksContent(content any) string {
	blocks, ok := content.([]any)
	if !ok {
		return ""
	}
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		bt, _ := block["type"].(string)
		if bt == "document" {
			if _, ok := claudeDocumentBlockToOpenAI(block); !ok {
				return "document is missing a usable source payload"
			}
		}
		if bt == "tool_result" {
			// tool_result content is itself a content array that may contain
			// document blocks. Recurse into it, but not into any other fields.
			if m := validateClaudeDocumentBlocksContent(block["content"]); m != "" {
				return m
			}
		}
	}
	return ""
}

// when a malformed file item is found.
func validateResponsesFileItems(input any) string {
	switch v := input.(type) {
	case []any:
		for _, item := range v {
			if msg := validateResponsesFileItem(item); msg != "" {
				return msg
			}
		}
	}
	return ""
}

// strings and text/input_text/output_text blocks).
func validateResponsesFileItem(item any) string {
	elem, ok := item.(map[string]any)
	if !ok {
		return ""
	}
	itemType, _ := elem["type"].(string)
	// Top-level input_file item or input_file content part.
	if itemType == "input_file" {
		if _, ok := responsesInputFileToFile(elem); !ok {
			return "input_file is missing file_data, file_id, and file_url"
		}
		return ""
	}
	// For message items, recurse into the content array (content parts).
	if itemType == "message" || itemType == "" {
		if content, ok := elem["content"].([]any); ok {
			for _, part := range content {
				if msg := validateResponsesFileItem(part); msg != "" {
					return msg
				}
			}
		}
		return ""
	}
	// All other item types (function_call, tool_call, tool_result,
	// apply_patch_call, shell_call, reasoning, *_call_output, etc.) are not
	// inspected — their arguments/input/content fields are not file inputs.
	return ""
}

// Returns (file, true) when a usable payload exists; (nil, false) otherwise.
func responsesInputFileToFile(part map[string]any) (map[string]any, bool) {
	file := map[string]any{}

	// Helper: read a non-empty string from a map by key.
	nonEmptyStr := func(m map[string]any, key string) (string, bool) {
		if v, ok := m[key].(string); ok && v != "" {
			return v, true
		}
		return "", false
	}

	// Nested input_file object: {"type":"input_file","input_file":{...}}.
	// Only select known fields; do not wholesale-copy.
	if nested, ok := part["input_file"].(map[string]any); ok {
		if v, ok := nonEmptyStr(nested, "file_data"); ok {
			file["file_data"] = v
		}
		if v, ok := nonEmptyStr(nested, "file_id"); ok {
			file["file_id"] = v
		}
		// nested file_url maps to file.file_data (best-effort).
		if v, ok := nonEmptyStr(nested, "file_url"); ok {
			file["file_data"] = v
		}
		if v, ok := nonEmptyStr(nested, "filename"); ok {
			file["filename"] = v
		}
	}

	// Flat fields take priority over nested values.
	if v, ok := nonEmptyStr(part, "file_data"); ok {
		file["file_data"] = v
	}
	if v, ok := nonEmptyStr(part, "file_id"); ok {
		file["file_id"] = v
	}
	// Flat file_url maps to file.file_data (best-effort).
	if v, ok := nonEmptyStr(part, "file_url"); ok {
		file["file_data"] = v
	}
	if v, ok := nonEmptyStr(part, "filename"); ok {
		file["filename"] = v
	}

	// A usable payload requires at least one of file_data / file_id.
	if _, hasData := file["file_data"]; !hasData {
		if _, hasID := file["file_id"]; !hasID {
			return nil, false
		}
	}
	return file, true
}

func responsesContentToMessageContent(content any) any {
	if content == nil {
		return nil
	}
	if s, ok := content.(string); ok {
		return s
	}

	parts, ok := content.([]any)
	if !ok {
		b, err := json.Marshal(content)
		if err != nil {
			return nil
		}
		return string(b)
	}

	convertedParts := make([]any, 0, len(parts))
	texts := make([]string, 0, len(parts))
	onlyTextParts := true

	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		convertedPart, ok := convertResponsesContentPart(part)
		if !ok {
			text := extractTextFromContentParts([]any{part})
			if text == "" {
				b, err := json.Marshal(part)
				if err != nil {
					continue
				}
				text = string(b)
			}
			convertedParts = append(convertedParts, map[string]any{
				"type": "text",
				"text": text,
			})
			texts = append(texts, text)
			continue
		}

		if convertedPart["type"] != "text" {
			onlyTextParts = false
		}
		if text, ok := convertedPart["text"].(string); ok && text != "" {
			texts = append(texts, text)
		}
		convertedParts = append(convertedParts, convertedPart)
	}

	if len(convertedParts) == 0 {
		return ""
	}
	if onlyTextParts {
		return strings.Join(texts, "\n")
	}
	return convertedParts
}

func chatContentToResponsesContent(content any) ([]any, string) {
	switch v := content.(type) {
	case nil:
		return nil, ""
	case string:
		if v == "" {
			return nil, ""
		}
		return []any{map[string]any{
			"type":        "output_text",
			"text":        v,
			"annotations": []any{},
			"logprobs":    []any{},
		}}, v
	case []any:
		parts := make([]any, 0, len(v))
		texts := make([]string, 0, len(v))
		for _, rawPart := range v {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			partType, _ := part["type"].(string)
			switch partType {
			case "text", "input_text", "output_text":
				text, _ := part["text"].(string)
				if text == "" {
					continue
				}
				annotations, ok := part["annotations"]
				if !ok {
					annotations = []any{}
				}
				logprobs, ok := part["logprobs"]
				if !ok {
					logprobs = []any{}
				}
				texts = append(texts, text)
				parts = append(parts, map[string]any{
					"type":        "output_text",
					"text":        text,
					"annotations": annotations,
					"logprobs":    logprobs,
				})
			}
		}
		return parts, strings.Join(texts, "\n")
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, ""
		}
		text := string(b)
		return []any{map[string]any{
			"type":        "output_text",
			"text":        text,
			"annotations": []any{},
			"logprobs":    []any{},
		}}, text
	}
}

func responsesHandler(w http.ResponseWriter, r *http.Request) {
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
	maybeLogBodySummary(r.Context(), "responses request body", body)
	_ = cnt

	var respReq ResponsesAPIRequest
	if err := json.Unmarshal(body, &respReq); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	modelIn := respReq.Model
	respReq.Model = resolveModel(respReq.Model)
	if !validateRequestTemperature(w, respReq.Temperature, "responses", 0, 2) {
		return
	}
	if msg := validateResponsesFileItems(respReq.Input); msg != "" {
		writeProtocolValidation400(w, "responses", "input_file", msg)
		return
	}
	// Note: respReq.Messages (nonstandard compatibility field) is forwarded
	// as-is using Chat content shapes; Responses-style input_file parts are
	// not validated or converted there. Use the official `input` field for
	// input_file support.
	previousState, hasPreviousState := StoredResponseState{}, false
	if respReq.PreviousResponseID != "" {
		previousState, hasPreviousState = loadResponseState(respReq.PreviousResponseID)
		if respReq.Model == "" && previousState.Model != "" {
			respReq.Model = previousState.Model
		}
		if len(respReq.Tools) == 0 && len(previousState.Tools) > 0 {
			respReq.Tools = previousState.Tools
		}
		if respReq.ToolChoice == nil && previousState.ToolChoice != nil {
			respReq.ToolChoice = previousState.ToolChoice
		}
	}
	if respReq.Model == "" {
		modelIDs := getModelIDs()
		if len(modelIDs) > 0 {
			respReq.Model = modelIDs[0]
		} else {
			respReq.Model = "deepseek-v4-flash-free"
		}
	}
	respReq.Model = mapPublicToFreeModel(auth, respReq.Model)

	// 多模态路由

	messages := respReq.Messages
	if len(messages) == 0 {
		if hasPreviousState && len(previousState.Output) > 0 {
			messages = append(messages, responsesInputToMessages(previousState.Output, "")...)
		}
		messages = append(messages, responsesInputToMessages(respReq.Input, respReq.Instructions)...)
	} else if respReq.Instructions != "" {
		messages = append([]Message{{Role: "system", Content: respReq.Instructions}}, messages...)
	}

	chatReq := OpenAIRequest{
		Model:    respReq.Model,
		Messages: messages,
		Stream:   respReq.Stream,
	}
	if respReq.Stream {
		chatReq.ExtraBody = map[string]any{
			"stream_options": map[string]any{"include_usage": true},
		}
	}
	if respReq.Temperature != nil {
		chatReq.Temperature = respReq.Temperature
	}
	if respReq.MaxTokens != nil {
		chatReq.MaxTokens = respReq.MaxTokens
	}
	if respReq.TopP != nil {
		chatReq.TopP = respReq.TopP
	}
	if len(respReq.Tools) > 0 {
		chatReq.Tools = convertResponsesTools(respReq.Tools)
	}
	if respReq.ToolChoice != nil {
		chatReq.ToolChoice = convertResponsesToolChoice(respReq.ToolChoice)
	}
	if respReq.ParallelToolCalls != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["parallel_tool_calls"] = *respReq.ParallelToolCalls
	}
	if respReq.Stop != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["stop"] = respReq.Stop
	}
	if respReq.FrequencyPenalty != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["frequency_penalty"] = *respReq.FrequencyPenalty
	}
	if respReq.PresencePenalty != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["presence_penalty"] = *respReq.PresencePenalty
	}
	if respReq.User != "" {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["user"] = respReq.User
	}
	if respReq.Text != nil {
		// OpenAI Responses API `text` is {format:{type:...}, verbosity:...};
		// upstream expects Chat Completions `response_format` with a top-level
		// `type`. Translate, and drop the field entirely when it cannot be
		// represented (never send a malformed response_format upstream, which
		// would surface as a 400).
		if rf := convertResponsesTextToResponseFormat(respReq.Text); rf != nil {
			if chatReq.ExtraBody == nil {
				chatReq.ExtraBody = map[string]any{}
			}
			chatReq.ExtraBody["response_format"] = rf
		}
	}
	if respReq.Truncation != "" {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["truncation"] = respReq.Truncation
	}
	if respReq.ServiceTier != "" {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["service_tier"] = respReq.ServiceTier
	}
	if respReq.PromptCacheKey != "" {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["prompt_cache_key"] = respReq.PromptCacheKey
	}
	if respReq.SafetyIdentifier != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["safety_identifier"] = respReq.SafetyIdentifier
	}
	if respReq.TopLogprobs != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		chatReq.ExtraBody["top_logprobs"] = *respReq.TopLogprobs
	}
	if respReq.StreamOptions != nil {
		if chatReq.ExtraBody == nil {
			chatReq.ExtraBody = map[string]any{}
		}
		streamOptions, ok := respReq.StreamOptions.(map[string]any)
		if !ok {
			streamOptions = map[string]any{}
		}
		if _, exists := streamOptions["include_usage"]; !exists && respReq.Stream {
			streamOptions["include_usage"] = true
		}
		chatReq.ExtraBody["stream_options"] = streamOptions
	}
	// 将 Responses API reasoning.effort 映射到 Chat Completions
	if !getForceDisableThinking() && respReq.Reasoning.Effort != "" {
		if respReq.Reasoning.Effort != "none" {
			chatReq.ReasoningEffort = respReq.Reasoning.Effort
		}
	}

	wantReasoning := !getForceDisableThinking()
	chatReq.Messages = fixToolCallGaps(chatReq.Messages)
	keepReasoning := wantsReasoning(&chatReq)
	chatReq.Messages = ensureReasoningContent(chatReq.Messages, keepReasoning)

	effortIn := chatReq.ReasoningEffort
	if effortIn == "" {
		effortIn = respReq.Reasoning.Effort
	}
	upstreamSurface := "zen"
	if auth.shouldUseGoEndpoint(chatReq.Model) {
		upstreamSurface = "go"
	}
	logRequestPlan(r.Context(), map[string]any{
		"protocol":             "responses",
		"model_in":             modelIn,
		"model_resolved":       chatReq.Model,
		"auth_mode":            authModeString(auth.Mode),
		"auth_source":          auth.Source,
		"has_key":              auth.Token != "",
		"upstream_surface":     upstreamSurface,
		"stream":               respReq.Stream,
		"keep_reasoning":       keepReasoning,
		"thinking":             thinkingState(nil),
		"reasoning_effort_in":  effortIn,
		"reasoning_effort_out": mappedReasoningEffort(effortIn),
		"tools_count":          len(respReq.Tools),
		"messages_count":       len(chatReq.Messages),
		"max_tokens":           chatReq.MaxTokens,
		"max_tokens_cap":       getMaxTokensCapForModel(chatReq.Model),
	})

	upstreamBody := buildUpstreamBody(&chatReq)

	if respReq.Stream {
		upResp, status, _, err := callOpenCodeAPIStream(r.Context(), upstreamBody, chatReq.Model, auth)
		if err != nil || status < 200 || status >= 300 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if upResp != nil {
				errBody, _ := io.ReadAll(upResp)
				if len(errBody) > 0 {
					w.Write(errBody)
					return
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error"}})
			return
		}
		defer upResp.Close()

		resp := &http.Response{
			StatusCode: status,
			Body:       upResp,
			Header:     make(http.Header),
		}
		responsesStreamHandler(w, r, resp, chatReq.Model, chatReq.Model, wantReasoning, respReq.Tools, respReq.ToolChoice, respReq)
		return
	}

	respBody, status, _, err := callOpenCodeAPI(r.Context(), upstreamBody, chatReq.Model, auth)
	if err != nil || status < 200 || status >= 300 {
		if err != nil {
			writeUpstreamError(w, status, err, "responses")
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if len(respBody) > 0 {
				w.Write(respBody)
			} else {
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream error"}})
			}
		}
		return
	}

	responsesBody := convertChatToResponses(respBody, chatReq.Model, wantReasoning, respReq.Tools, respReq.ToolChoice, respReq.Include)
	var responseMap map[string]any
	if json.Unmarshal(responsesBody, &responseMap) == nil {
		applyResponsesRequestEcho(responseMap, respReq)
		if enriched, marshalErr := json.Marshal(responseMap); marshalErr == nil {
			responsesBody = enriched
		}
		storeResponseState(responseMap, respReq)
	}

	result := summarizeChatResult(respBody)
	logRequestResult(r.Context(), result)

	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			pt, _ := u["prompt_tokens"].(float64)
			ct, _ := u["completion_tokens"].(float64)
			tt, _ := u["total_tokens"].(float64)
			if tt > 0 {
				recordTokenUsage(chatReq.Model, int64(pt), int64(ct), int64(tt))
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	maybeLogBodySummary(r.Context(), "responses response body", responsesBody)
	w.Write(responsesBody)
}

// ======================== Responses Stream Handler ========================

func responsesStreamHandler(w http.ResponseWriter, r *http.Request, resp *http.Response, model string, _ string, wantReasoning bool, tools []ResponsesTool, toolChoice any, originalReq ResponsesAPIRequest) {
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(resp.Body)
	stats := &streamResultStats{start: time.Now()}

	responseID := "resp_" + time.Now().Format("20060102150405") + "_" + randomString(8)
	reasoningID := "rs_" + responseID
	msgID := "msg_" + responseID + "_0"
	createdAt := time.Now().Unix()
	seq := 0

	reasoningStarted := false
	reasoningDone := false
	messageStarted := false
	messageDone := false
	fullReasoning := ""
	fullText := ""
	fullRefusal := ""
	refusalStarted := false
	totalUsage := map[string]any{}
	createdSent := false
	terminalStatus := "completed"
	terminalEvent := "response.completed"
	itemStatus := "completed"
	finished := false
	toolCalls := map[int]map[string]any{}
	toolOrder := []int{}
	toolKinds := responsesToolKindMap(tools)
	indexAllocator := outputIndexAllocator{}
	reasoningOutputIndex := -1
	messageIndex := -1

	// --- Reader goroutine -> channel so the main loop can select on
	// read/context without blocking, and context cancellation unblocks the
	// reader via Close. ---
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

	defer func() {
		stats.textChars = len(fullText)
		stats.reasoningChars = len(fullReasoning)
		stats.toolCallCount = len(toolOrder)
		stats.log(ctx, "responses")
	}()
	// Reader cleanup: signal goroutine, unblock any pending read, wait for exit.
	defer func() {
		close(readerDone)
		resp.Body.Close()
		<-readerExited
	}()

	messageOutputIndex := func() int {
		if messageIndex < 0 {
			messageIndex = indexAllocator.Allocate()
		}
		return messageIndex
	}

	reasoningItem := func(status string) map[string]any {
		item := map[string]any{
			"id":      reasoningID,
			"type":    "reasoning",
			"summary": []any{},
		}
		if status != "" {
			item["status"] = status
		}
		if status == "completed" && includeHas(originalReq.Include, "reasoning.encrypted_content") {
			item["encrypted_content"] = ""
		}
		if fullReasoning != "" {
			item["summary"] = []any{map[string]any{"type": "summary_text", "text": fullReasoning}}
		}
		return item
	}

	messageItem := func(status string) map[string]any {
		content := []any{}
		if fullRefusal != "" {
			content = append(content, map[string]any{
				"type":    "refusal",
				"refusal": fullRefusal,
			})
		}
		content = append(content, map[string]any{
			"type":        "output_text",
			"annotations": []any{},
			"logprobs":    []any{},
			"text":        fullText,
		})
		return map[string]any{
			"id":      msgID,
			"type":    "message",
			"status":  status,
			"content": content,
			"role":    "assistant",
		}
	}

	emitReasoningDone := func() {
		if !reasoningStarted || reasoningDone {
			return
		}
		seq++
		emitSSEEvent(w, flusher, "response.reasoning_summary_text.done", map[string]any{
			"type":            "response.reasoning_summary_text.done",
			"sequence_number": seq,
			"item_id":         reasoningID,
			"output_index":    reasoningOutputIndex,
			"summary_index":   0,
			"text":            fullReasoning,
		})
		seq++
		emitSSEEvent(w, flusher, "response.reasoning_summary_part.done", map[string]any{
			"type":            "response.reasoning_summary_part.done",
			"sequence_number": seq,
			"item_id":         reasoningID,
			"output_index":    reasoningOutputIndex,
			"summary_index":   0,
			"part":            map[string]any{"type": "summary_text", "text": fullReasoning},
		})
		seq++
		emitSSEEvent(w, flusher, "response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
			"sequence_number": seq,
			"output_index":    reasoningOutputIndex,
			"item":            reasoningItem(itemStatus),
		})
		reasoningDone = true
	}

	emitMessageDone := func() {
		if !messageStarted || messageDone {
			return
		}
		idx := messageOutputIndex()
		seq++
		emitSSEEvent(w, flusher, "response.output_text.done", map[string]any{
			"type":            "response.output_text.done",
			"sequence_number": seq,
			"item_id":         msgID,
			"output_index":    idx,
			"content_index":   0,
			"text":            fullText,
			"logprobs":        []any{},
		})
		seq++
		emitSSEEvent(w, flusher, "response.content_part.done", map[string]any{
			"type":            "response.content_part.done",
			"sequence_number": seq,
			"item_id":         msgID,
			"output_index":    idx,
			"content_index":   0,
			"part":            map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": fullText},
		})
		seq++
		emitSSEEvent(w, flusher, "response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
			"sequence_number": seq,
			"output_index":    idx,
			"item":            messageItem(itemStatus),
		})
		messageDone = true
	}

	emitRefusalDone := func() {
		if !refusalStarted {
			return
		}
		idx := messageOutputIndex()
		seq++
		emitSSEEvent(w, flusher, "response.refusal.done", map[string]any{
			"type":            "response.refusal.done",
			"sequence_number": seq,
			"item_id":         msgID,
			"output_index":    idx,
			"content_index":   0,
			"refusal":         fullRefusal,
		})
	}

	emitToolCallDone := func(idx int, call map[string]any) {
		if done, _ := call["done"].(bool); done {
			return
		}
		call["done"] = true
		itemID, _ := call["item_id"].(string)
		callID, _ := call["call_id"].(string)
		name, _ := call["name"].(string)
		args, _ := call["arguments"].(string)
		seq++
		emitSSEEvent(w, flusher, "response.function_call_arguments.done", map[string]any{
			"type":            "response.function_call_arguments.done",
			"sequence_number": seq,
			"item_id":         itemID,
			"output_index":    idx,
			"name":            name,
			"arguments":       args,
		})
		seq++
		itemType, _ := call["item_type"].(string)
		if itemType == "" {
			itemType = "function_call"
		}
		item := buildResponseToolCallItem(ToolCall{ID: callID, Function: FunctionCall{Name: name, Arguments: args}}, itemType)
		item["status"] = itemStatus
		emitSSEEvent(w, flusher, "response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
			"sequence_number": seq,
			"output_index":    idx,
			"item":            item,
		})
	}

	ensureCreated := func(chunk map[string]any) {
		if createdSent {
			return
		}
		if chunk != nil {
			if id, ok := chunk["id"].(string); ok && id != "" {
				responseID = normalizeResponsesID(id)
				reasoningID = "rs_" + responseID + "_0"
				msgID = "msg_" + responseID + "_0"
			}
			if created, ok := chunk["created"].(float64); ok {
				createdAt = int64(created)
			}
		}
		seq++
		emitSSEEvent(w, flusher, "response.created", map[string]any{
			"type":            "response.created",
			"sequence_number": seq,
			"response":        map[string]any{"id": responseID, "object": "response", "created_at": createdAt, "status": "in_progress", "background": false, "error": nil, "output": []any{}},
		})
		seq++
		emitSSEEvent(w, flusher, "response.in_progress", map[string]any{
			"type":            "response.in_progress",
			"sequence_number": seq,
			"response":        map[string]any{"id": responseID, "object": "response", "created_at": createdAt, "status": "in_progress"},
		})
		createdSent = true
	}

	emitResponseFailed := func(msg string) {
		ensureCreated(nil)
		failedResponse := map[string]any{
			"id":         responseID,
			"object":     "response",
			"created_at": createdAt,
			"status":     "failed",
			"background": false,
			"error": map[string]any{
				"code":    "server_error",
				"message": msg,
			},
			"incomplete_details": nil,
			"model":              model,
			"output":             []any{},
		}
		applyResponsesRequestEcho(failedResponse, originalReq)
		seq++
		emitSSEEvent(w, flusher, "response.failed", map[string]any{
			"type":            "response.failed",
			"sequence_number": seq,
			"response":        failedResponse,
		})
		if flusher != nil {
			flusher.Flush()
		}
	}

loop:
	for {
		select {
		case <-ctx.Done():
			// Client cancelled: quiet exit, no error writes.
			return
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
					emitResponseFailed("stream ended with [DONE] but no finish_reason")
					return
				}
				break loop
			}
			if strings.HasPrefix(line, "data: ") {
				payload := line[6:]
				if strings.TrimSpace(payload) != "" {
					var chunk map[string]any
					if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
						emitResponseFailed("stream received malformed JSON data")
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
							emitResponseFailed(errMsg)
							return
						} else {
							stats.noteChunk()
							ensureCreated(chunk)
							choices, ok := chunk["choices"].([]any)
							if !ok || len(choices) == 0 {
								if usage, ok := chunk["usage"].(map[string]any); ok {
									totalUsage = usage
								}
							} else {
								choice, _ := choices[0].(map[string]any)
								delta, _ := choice["delta"].(map[string]any)
								finishReason, _ := choice["finish_reason"].(string)
								if finishReason != "" {
									stats.finishReason = finishReason
									stats.sawFinish = true
								}

								if !finished {
									if rc, ok := delta["reasoning_content"]; ok && wantReasoning {
										rcStr, _ := rc.(string)
										if rcStr != "" {
											if !reasoningStarted {
												reasoningOutputIndex = indexAllocator.Allocate()
												seq++
												emitSSEEvent(w, flusher, "response.output_item.added", map[string]any{
													"type":            "response.output_item.added",
													"sequence_number": seq,
													"output_index":    reasoningOutputIndex,
													"item":            reasoningItem("in_progress"),
												})
												seq++
												emitSSEEvent(w, flusher, "response.reasoning_summary_part.added", map[string]any{
													"type":            "response.reasoning_summary_part.added",
													"sequence_number": seq,
													"item_id":         reasoningID,
													"output_index":    reasoningOutputIndex,
													"summary_index":   0,
													"part":            map[string]any{"type": "summary_text", "text": ""},
												})
												reasoningStarted = true
											}
											fullReasoning += rcStr
											seq++
											emitSSEEvent(w, flusher, "response.reasoning_summary_text.delta", map[string]any{
												"type":            "response.reasoning_summary_text.delta",
												"sequence_number": seq,
												"item_id":         reasoningID,
												"output_index":    reasoningOutputIndex,
												"summary_index":   0,
												"delta":           rcStr,
											})
										}
									}

									contentStr := ""
									if c, ok := delta["content"]; ok && c != nil {
										contentStr, _ = c.(string)
									}
									// #37635: when thinking is not kept, promote misplaced reasoning to visible text.
									if contentStr == "" && !wantReasoning {
										if rc, ok := delta["reasoning_content"].(string); ok {
											if rc != "" {
												stats.promotedReasoning = true
											}
											contentStr = rc
										}
									}
									if contentStr != "" {
										// The terminal finish reason determines the item's final status. Keep the
										// reasoning item open until that reason is known so a truncation cannot
										// first announce it as completed.
										if !messageStarted {
											idx := messageOutputIndex()
											seq++
											emitSSEEvent(w, flusher, "response.output_item.added", map[string]any{
												"type":            "response.output_item.added",
												"sequence_number": seq,
												"output_index":    idx,
												"item":            map[string]any{"id": msgID, "type": "message", "status": "in_progress", "content": []any{}, "role": "assistant"},
											})
											seq++
											emitSSEEvent(w, flusher, "response.content_part.added", map[string]any{
												"type":            "response.content_part.added",
												"sequence_number": seq,
												"item_id":         msgID,
												"output_index":    idx,
												"content_index":   0,
												"part":            map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": ""},
											})
											messageStarted = true
										}
										fullText += contentStr
										seq++
										emitSSEEvent(w, flusher, "response.output_text.delta", map[string]any{
											"type":            "response.output_text.delta",
											"sequence_number": seq,
											"item_id":         msgID,
											"output_index":    messageOutputIndex(),
											"content_index":   0,
											"delta":           contentStr,
											"logprobs":        []any{},
										})
									}

									if refusalStr, ok := delta["refusal"].(string); ok && refusalStr != "" {
										if !refusalStarted {
											refusalStarted = true
										}
										fullRefusal += refusalStr
										seq++
										emitSSEEvent(w, flusher, "response.refusal.delta", map[string]any{
											"type":            "response.refusal.delta",
											"sequence_number": seq,
											"item_id":         msgID,
											"output_index":    messageOutputIndex(),
											"content_index":   0,
											"delta":           refusalStr,
										})
									}

									rawToolCalls, _ := delta["tool_calls"].([]any)
									for _, rawToolCall := range rawToolCalls {
										tc, ok := rawToolCall.(map[string]any)
										if !ok {
											continue
										}
										idxFloat, _ := tc["index"].(float64)
										upstreamIndex := int(idxFloat)
										call, exists := toolCalls[upstreamIndex]
										if !exists {
											outputIndex := indexAllocator.Allocate()
											callID, _ := tc["id"].(string)
											if callID == "" {
												callID = "call_" + randomString(12)
											}
											fn, _ := tc["function"].(map[string]any)
											name, _ := fn["name"].(string)
											itemType := toolCallOutputType(name, toolKinds)
											call = map[string]any{
												"output_index": outputIndex,
												"item_id":      "fc_" + callID,
												"call_id":      callID,
												"name":         name,
												"arguments":    "",
												"done":         false,
												"item_type":    itemType,
											}
											toolCalls[upstreamIndex] = call
											toolOrder = append(toolOrder, upstreamIndex)
											seq++
											emitSSEEvent(w, flusher, "response.output_item.added", map[string]any{
												"type":            "response.output_item.added",
												"sequence_number": seq,
												"output_index":    outputIndex,
												"item": map[string]any{
													"id":        call["item_id"],
													"type":      itemType,
													"status":    "in_progress",
													"arguments": "",
													"call_id":   callID,
													"name":      name,
												},
											})
										}
										fn, _ := tc["function"].(map[string]any)
										if name, _ := fn["name"].(string); name != "" {
											call["name"] = name
											if call["item_type"] == "function_call" {
												call["item_type"] = toolCallOutputType(name, toolKinds)
											}
										}
										if argDelta, _ := fn["arguments"].(string); argDelta != "" {
											call["arguments"] = call["arguments"].(string) + argDelta
											seq++
											emitSSEEvent(w, flusher, "response.function_call_arguments.delta", map[string]any{
												"type":            "response.function_call_arguments.delta",
												"sequence_number": seq,
												"item_id":         call["item_id"],
												"output_index":    call["output_index"],
												"delta":           argDelta,
											})
										}
									}

									if usage, ok := chunk["usage"].(map[string]any); ok {
										totalUsage = usage
									}
									if finishReason == "stop" || finishReason == "length" || finishReason == "tool_calls" || finishReason == "function_call" || finishReason == "content_filter" {
										finished = true
										if finishReason == "length" {
											terminalStatus = "incomplete"
											terminalEvent = "response.incomplete"
											itemStatus = "incomplete"
										}
										// Do not emit done events yet: a trailing error
										// must still produce response.failed without any
										// status=completed item.done. Done events are
										// emitted only after the loop exits cleanly.
									}
								} else {
									// After finish_reason, only look for usage-only trailing chunks.
									if usage, ok := chunk["usage"].(map[string]any); ok {
										totalUsage = usage
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
						emitResponseFailed("stream ended without finish_reason")
						return
					}
					break loop
				}
				reqLogger(ctx).Error("stream read error", "error", pendingErr)
				emitResponseFailed("stream read error")
				return
			}
		}
	}

	// Reached only when finished is true.
	emitReasoningDone()
	emitRefusalDone()
	if !messageStarted && len(toolCalls) == 0 {
		idx := messageOutputIndex()
		seq++
		emitSSEEvent(w, flusher, "response.output_item.added", map[string]any{
			"type":            "response.output_item.added",
			"sequence_number": seq,
			"output_index":    idx,
			"item":            map[string]any{"id": msgID, "type": "message", "status": "in_progress", "content": []any{}, "role": "assistant"},
		})
		seq++
		emitSSEEvent(w, flusher, "response.content_part.added", map[string]any{
			"type":            "response.content_part.added",
			"sequence_number": seq,
			"item_id":         msgID,
			"output_index":    idx,
			"content_index":   0,
			"part":            map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": ""},
		})
		messageStarted = true
	}
	emitMessageDone()
	for _, idx := range toolOrder {
		emitToolCallDone(toolCalls[idx]["output_index"].(int), toolCalls[idx])
	}

	output := make([]any, indexAllocator.Len())
	if reasoningStarted {
		output[reasoningOutputIndex] = reasoningItem(itemStatus)
	}
	if messageStarted {
		output[messageIndex] = messageItem(itemStatus)
	}
	for _, idx := range toolOrder {
		call := toolCalls[idx]
		itemType, _ := call["item_type"].(string)
		if itemType == "" {
			itemType = "function_call"
		}
		item := buildResponseToolCallItem(ToolCall{
			ID: call["call_id"].(string),
			Function: FunctionCall{
				Name:      call["name"].(string),
				Arguments: call["arguments"].(string),
			},
		}, itemType)
		item["status"] = itemStatus
		output[call["output_index"].(int)] = item
	}

	completedResponse := map[string]any{
		"id":                 responseID,
		"object":             "response",
		"created_at":         createdAt,
		"status":             terminalStatus,
		"background":         false,
		"error":              nil,
		"incomplete_details": nil,
		"model":              model,
		"output":             output,
	}
	if terminalStatus == "incomplete" {
		completedResponse["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	applyResponsesRequestEcho(completedResponse, originalReq)
	if len(tools) > 0 {
		completedResponse["tools"] = tools
	}
	if toolChoice != nil {
		completedResponse["tool_choice"] = toolChoice
	}

	if len(totalUsage) > 0 {
		usage := map[string]any{}
		if v, ok := totalUsage["prompt_tokens"]; ok {
			usage["input_tokens"] = v
		}
		if v, ok := totalUsage["prompt_tokens_details"]; ok {
			usage["input_tokens_details"] = v
		} else {
			usage["input_tokens_details"] = map[string]any{"cached_tokens": 0}
		}
		if v, ok := totalUsage["completion_tokens"]; ok {
			usage["output_tokens"] = v
		}
		if v, ok := totalUsage["completion_tokens_details"]; ok {
			usage["output_tokens_details"] = v
		}
		if v, ok := totalUsage["total_tokens"]; ok {
			usage["total_tokens"] = v
		}
		if v, ok := totalUsage["input_tokens"]; ok && usage["input_tokens"] == nil {
			usage["input_tokens"] = v
		}
		if v, ok := totalUsage["output_tokens"]; ok && usage["output_tokens"] == nil {
			usage["output_tokens"] = v
		}
		completedResponse["usage"] = usage
	}

	if totalUsage != nil {
		pt, _ := totalUsage["prompt_tokens"].(float64)
		ct, _ := totalUsage["completion_tokens"].(float64)
		tt, _ := totalUsage["total_tokens"].(float64)
		if tt > 0 {
			recordTokenUsage(model, int64(pt), int64(ct), int64(tt))
		}
	}

	seq++
	emitSSEEvent(w, flusher, terminalEvent, map[string]any{
		"type":            terminalEvent,
		"sequence_number": seq,
		"response":        completedResponse,
	})

	if flusher != nil {
		flusher.Flush()
	}
	storeResponseState(completedResponse, originalReq)
}

func convertChatToResponses(chatBody []byte, model string, wantReasoning bool, tools []ResponsesTool, toolChoice any, include []string) []byte {
	var chat struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          any        `json:"content"`
				Refusal          string     `json:"refusal"`
				ReasoningContent string     `json:"reasoning_content"`
				ToolCalls        []ToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		slog.Warn("convertChatToResponses unmarshal failed", "error", err)
	}

	reasoning := ""
	finishReason := ""
	var toolCalls []ToolCall
	messageContent := []any(nil)
	toolKinds := responsesToolKindMap(tools)
	if len(chat.Choices) > 0 {
		messageContent, _ = chatContentToResponsesContent(chat.Choices[0].Message.Content)
		if refusal := chat.Choices[0].Message.Refusal; refusal != "" {
			messageContent = []any{map[string]any{"type": "refusal", "refusal": refusal}}
		}
		rc := chat.Choices[0].Message.ReasoningContent
		if wantReasoning {
			reasoning = rc
		}
		toolCalls = chat.Choices[0].Message.ToolCalls
		finishReason = chat.Choices[0].FinishReason
		if len(messageContent) == 0 && rc != "" && len(toolCalls) == 0 {
			messageContent, _ = chatContentToResponsesContent(rc)
		}
	}

	outcome := responsesOutcome(finishReason)
	status := outcome.Status
	normalizedID := normalizeResponsesID(chat.ID)
	responses := map[string]any{
		"id":                 normalizedID,
		"object":             "response",
		"status":             status,
		"background":         false,
		"error":              nil,
		"incomplete_details": outcome.IncompleteDetails,
		"model":              model,
		"created_at":         chat.Created,
	}
	if len(tools) > 0 {
		responses["tools"] = tools
	}
	if toolChoice != nil {
		responses["tool_choice"] = toolChoice
	}
	outputID := "msg_" + normalizedID + "_0"
	output := []any{}
	if reasoning != "" {
		reasoningItem := map[string]any{
			"id":      "rs_" + normalizedID,
			"type":    "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": reasoning}},
		}
		if includeHas(include, "reasoning.encrypted_content") {
			reasoningItem["encrypted_content"] = ""
		}
		output = append(output, reasoningItem)
	}
	if len(messageContent) > 0 {
		output = append(output, map[string]any{
			"id":      outputID,
			"type":    "message",
			"status":  status,
			"role":    "assistant",
			"content": messageContent,
		})
	}
	for _, tc := range toolCalls {
		item := buildResponseToolCallItem(tc, toolCallOutputType(tc.Function.Name, toolKinds))
		item["status"] = status
		output = append(output, item)
	}
	responses["output"] = output
	if chat.Usage != nil {
		usage := map[string]any{}
		if v, ok := chat.Usage["prompt_tokens"]; ok {
			usage["input_tokens"] = v
		}
		if v, ok := chat.Usage["prompt_tokens_details"]; ok {
			usage["input_tokens_details"] = v
		} else {
			usage["input_tokens_details"] = map[string]any{"cached_tokens": 0}
		}
		if v, ok := chat.Usage["completion_tokens"]; ok {
			usage["output_tokens"] = v
		}
		if v, ok := chat.Usage["completion_tokens_details"]; ok {
			usage["output_tokens_details"] = v
		}
		if v, ok := chat.Usage["total_tokens"]; ok {
			usage["total_tokens"] = v
		}
		if v, ok := chat.Usage["input_tokens"]; ok && usage["input_tokens"] == nil {
			usage["input_tokens"] = v
		}
		if v, ok := chat.Usage["output_tokens"]; ok && usage["output_tokens"] == nil {
			usage["output_tokens"] = v
		}
		responses["usage"] = usage
	}

	result, _ := json.Marshal(responses)
	return result
}

func emitSSEEvent(w http.ResponseWriter, flusher http.Flusher, event string, data map[string]any) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		slog.Error("marshal SSE event failed", "error", err)
		return
	}
	w.Write([]byte("event: " + event + "\n"))
	w.Write([]byte("data: " + string(jsonData) + "\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}
