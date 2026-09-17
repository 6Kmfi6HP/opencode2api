package app

import (
	"context"
	"encoding/json"
	"github.com/6Kmfi6HP/opencode2api/internal/config"
	"github.com/6Kmfi6HP/opencode2api/internal/logging"
	statsx "github.com/6Kmfi6HP/opencode2api/internal/stats"
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
					// 只保留 summary 文本（summary[*].text）作为 ReasoningContent；
					// signature 与 encrypted_content 绑定发起方且非明文，不回放为
					// 文本（摘要为空时即丢弃该条目，不注入任何原文 JSON）。
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
					// 未知 item 类型（含 item_reference、服务端专有 item 等）静默跳过：
					// 不把原始 JSON 注入 role:user 文本污染上下文。
					continue
				}
			default:
				// 数组内裸非字符串原子（null/数字/布尔等）丢弃，不转成文本消息。
				continue
			}
		}
	default:
		b, _ := json.Marshal(v)
		messages = append(messages, Message{Role: "user", Content: string(b)})
	}
	return messages
}

// convertResponsesTools 把 Responses tools 转 Chat Completions tools。
// 服务端工具（web_search/file_search/computer_use/mcp/local_shell/custom 等）
// 无对应 function 形状，由 responsesToolFunction 返回 ok=false，此处丢弃并
// 日志计数；若 tool_choice 指向被丢弃的工具，由调用方 normalizeToolChoiceWithTools
// 兜底为不传。
// ======== tool_use/tool_result 配对归一化（Responses→Anthropic 组装前） ========

// messageTextContent 从 Chat content（纯字符串或多模态 parts 数组）提取可见文本。
// 非文本分片（image_url/file 等）在无文本时兜底为其 JSON 字符串，避免空内容消息。
func messageTextContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var texts []string
		for _, p := range v {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := pm["text"].(string); t != "" {
				texts = append(texts, t)
			}
		}
		return strings.Join(texts, "\n")
	default:
		if v == nil {
			return ""
		}
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
		return ""
	}
}

// mergeConsecutiveSameRole 把相邻同 role 的 domain.Message 合并。tool（合并到
// 前一条，保留 ToolCallID 供配对索引，后续已展开为 tool_result）与
// system/developer/user/assistant（仅无 tool_calls 时按对话回合合并）分别处理。
// Anthropic 要求 tool_use 与其 tool_result 之间无任意 user/assistant 文本，
// 本归一化与 normalizeAnthropicToolPairing 配合恢复工具配对与回合交替。
func mergeConsecutiveSameRole(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	sameContent := func(a, b any) bool {
		ab, aerr := json.Marshal(a)
		bb, berr := json.Marshal(b)
		return aerr == nil && berr == nil && string(ab) == string(bb)
	}
	mergeContent := func(dst, src any) any {
		switch d := dst.(type) {
		case string:
			if s, ok := src.(string); ok {
				if d == "" {
					return s
				}
				if s == "" {
					return d
				}
				return d + "\n\n" + s
			}
		case []any:
			if s, ok := src.([]any); ok {
				return append(append([]any{}, d...), s...)
			}
		}
		if ds := messageTextContent(dst); ds != "" {
			if ss := messageTextContent(src); ss != "" {
				return ds + "\n\n" + ss
			}
			return ds
		}
		return dst
	}
	for _, m := range msgs {
		n := len(out)
		if n == 0 {
			out = append(out, m)
			continue
		}
		last := &out[n-1]
		switch {
		case m.Role == "tool" && last.Role == "tool":
			// 连续的 tool（尚未转换的 messages 形态罕见；normalize/pairing 已由
			// chatMessagesToAnthropic 的 appendBlocks 处理为单 user 消息）。保留
			// ToolCallID 列表进 content 无意义，此处不合并 tool。
			out = append(out, m)
		case last.Role == m.Role && len(last.ToolCalls) == 0 && len(m.ToolCalls) == 0 &&
			m.Role != "tool":
			if last.Refusal == nil {
				last.Refusal = m.Refusal
			}
			if last.ReasoningContent == nil {
				last.ReasoningContent = m.ReasoningContent
			}
			if m.Content != nil {
				if last.Content == nil || (last.Content == "" && sameContent(last.Content, "")) {
					last.Content = m.Content
				} else {
					last.Content = mergeContent(last.Content, m.Content)
				}
			}
		default:
			out = append(out, m)
		}
	}
	return out
}

// mergeAdjacentToolCallAssistants 把相邻的「纯 tool_call assistant」消息合并
// 为单条，使 Responses 并行调用（多个 function_call）共享同一 assistant,
// 对应随后的 tool 结果按 call 序紧邻排列（normalizeAnthropicToolPairing
// 视为同一 assistant turn 的处理单元）。
func mergeAdjacentToolCallAssistants(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if len(out) > 0 {
			prev := &out[len(out)-1]
			// 仅当两条均为「工具调用载体」(无可见文本/refusal/reasoning) 时合并,
			// 避免把真实文本回合错位拼接到一起。
			if prev.Role == "assistant" && m.Role == "assistant" &&
				len(prev.ToolCalls) > 0 && len(m.ToolCalls) > 0 &&
				prev.Refusal == nil && m.Refusal == nil &&
				prev.ReasoningContent == nil && m.ReasoningContent == nil &&
				messageTextContent(prev.Content) == "" && messageTextContent(m.Content) == "" {
				prev.ToolCalls = append(append([]ToolCall{}, prev.ToolCalls...), m.ToolCalls...)
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

// normalizeAnthropicToolPairing 把 Chat messages 归一化成满足 Anthropic
// tool_use/tool_result 不变量的序列（详见 normalizeAnthropicToolPairing
// 上层注释）：剔除未答复的 assistant.tool_calls（连同空 assistant 消息）、
// 剔除孤儿 tool 消息，并把每个 tool 结果紧贴它的 assistant 消息后排序。
//
// 同时剔除 parseToolCallArguments 解析失败（`_raw` 兜底）的非法 arguments 调用
// 及其 output —— 防上游 400 死循环（本归一化覆盖 Worker A chat_to_anthropic 的
// `_raw` 兜底，以组装后的序列为准）。最后跑一次相邻同 role 合并恢复交替。
func normalizeAnthropicToolPairing(messages []Message) []Message {
	// 把相邻的「纯 tool_call assistant」合并为一条,使并行调用共享同一
	// assistant,其 tool 结果按 call 序紧邻排列(对应 sub2api 的并行 call 归并)。
	messages = mergeAdjacentToolCallAssistants(messages)
	// 索引所有 tool 结果消息按 ToolCallID（后出现覆盖先前同 id）。
	results := map[string]Message{}
	for _, m := range messages {
		if m.Role == "tool" && m.ToolCallID != "" {
			results[m.ToolCallID] = m
		}
	}

	droppedCalls := 0
	droppedOrphans := 0
	out := make([]Message, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case "assistant":
			if len(m.ToolCalls) == 0 {
				out = append(out, m)
				continue
			}
			kept := make([]ToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				if _, ok := results[tc.ID]; !ok {
					droppedCalls++ // 未答复的调用
					continue
				}
				// 非法 arguments（parseToolCallArguments 兜底 _raw）连同其
				// output 一并删除，防上游 400 死循环。
				parsed := parseToolCallArguments(tc.Function.Arguments)
				if _, raw := parsed["_raw"]; raw {
					droppedCalls++
					delete(results, tc.ID) // 已消费 → 不再作为孤儿再匹配
					continue
				}
				kept = append(kept, tc)
			}
			text := messageTextContent(m.Content)
			if len(kept) == 0 {
				if text == "" && m.Refusal == nil && m.ReasoningContent == nil {
					continue // 整条 assistant 消息无内容 → 删除
				}
				keptMsg := m
				keptMsg.ToolCalls = nil
				out = append(out, keptMsg)
				continue
			}
			keptMsg := m
			keptMsg.ToolCalls = kept
			out = append(out, keptMsg)
			for _, tc := range kept {
				out = append(out, results[tc.ID])
				delete(results, tc.ID) // 已消费 → 不再作为孤儿再出现
			}
		case "tool":
			droppedOrphans++ // 原位置的 tool 消息（已规范到 call 旁边或孤儿）一律剔除
		default:
			out = append(out, m)
		}
	}
	if droppedCalls > 0 || droppedOrphans > 0 {
		slog.Info("normalizeAnthropicToolPairing",
			"unanswered_or_invalid_calls_dropped", droppedCalls,
			"standalone_tool_msgs_dropped", droppedOrphans)
	}
	return mergeConsecutiveSameRole(out)
}

func convertResponsesTools(tools []ResponsesTool) []Tool {
	converted := make([]Tool, 0, len(tools))
	dropped := 0
	for _, tool := range tools {
		fn, ok := responsesToolFunction(tool)
		if !ok {
			dropped++
			continue
		}
		converted = append(converted, Tool{Type: "function", Function: fn})
	}
	if dropped > 0 {
		slog.Info("responses tools dropped (server-side tool types unsupported on chat path)",
			"dropped", dropped, "kept", len(converted))
	}
	return converted
}

// convertResponsesTextToResponseFormat translates the Responses API `text`
// parameter ({format:{type:...}, verbosity:...}) into the Chat Completions
// `response_format` shape ({type:...}) that upstream providers require.
//
// Returns nil when no representable format can be built (unknown type,
// missing required json_schema fields, or a non-object text value) so the
// caller can omit response_format instead of sending a malformed object that
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
		if fn.Strict == nil {
			fn.Strict = new(bool) // 显式 false：部分上游缺省按 strict=true 校验
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

// normalizeToolChoiceWithTools 在 convertResponsesToolChoice 结果上兜底：
// 当 tool_choice.name 指向的函数不在已保留的 Chat tools 里（例如对应的是被
// 丢弃的服务端工具 web_search/file_search/computer_use/mcp/local_shell/custom），
// 把 tool_choice 改为不传（返回 nil），避免上游因引用了不存在的工具而 400。
func normalizeToolChoiceWithTools(choice any, tools []Tool) any {
	m, ok := choice.(map[string]any)
	if !ok {
		return choice
	}
	// 两种形状：{type:"function", function:{name}}（Chat 已转换形状）与
	// {type:"<kind>", name:"<n>"}（Responses 原始形状，含 function/服务端 kind）。
	var name string
	if fn, ok := m["function"].(map[string]any); ok {
		name, _ = fn["name"].(string)
	}
	if name == "" {
		name, _ = m["name"].(string)
	}
	if name == "" {
		return choice // 非具名选择（auto/required/none 等），保留
	}
	for _, t := range tools {
		if t.Function.Name == name {
			return choice
		}
	}
	return nil
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

// toolResultOutputKind marks the output item types that carry a tool/function
// output payload. tool_result is the Anthropic-style alias accepted by the
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

// normalizeToolResultOutput is the single helper that extracts a textual
// output from a tool/function output item. It prefers the standard `output`
// field; for Anthropic-style tool_result it reads `content` when `output` is
// absent. content supports a string, a string array, or an array of
// {type:"text"|"input_text"|"output_text", text:"..."} blocks joined by
// newlines in original order. The boolean reports whether a payload was
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

// validateResponsesFileItem validates a single top-level input item or a
// content part within a message content array. File validation applies only
// to official input paths: top-level input_file items and message content
// arrays. Output/tool_result content arrays are not validated for file
// inputs — they use text shapes only (normalizeToolResultOutput supports
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
	auth, body, ok := readJSONRequestBody(w, r)
	if !ok {
		return
	}

	logging.MaybeBodySummary(r.Context(), "responses request body", body)

	var respReq ResponsesAPIRequest
	if err := json.Unmarshal(body, &respReq); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	modelIn := respReq.Model
	respReq.Model = resolveModelForAuth(auth, respReq.Model)
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
		// 续链时若未带 instructions，回填上一轮的系统指令（与 Tools/ToolChoice
		// 逻辑一致），保证跨轮系统提示不丢。
		if respReq.Instructions == "" && previousState.Instructions != "" {
			respReq.Instructions = previousState.Instructions
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

	// 协议路由：显式规则 > native-responses 运行时记忆 > 默认 chat。
	// responses 分支为既有透传；anthropic 分支延迟到 chatReq 构建完成后
	// 调用（复用 messages 转换结果）；chat 分支保持既有翻译路径。
	upstreamProto := resolveUpstreamProtocol(respReq.Model)
	if upstreamProto == upstreamProtocolResponses {
		slog.Info("responses passthrough (remembered)",
			"model_in", modelIn, "model", respReq.Model, "stream", respReq.Stream)
		if forwardNativeResponses(r.Context(), w, auth, respReq.Model, body, respReq.Stream, respReq) {
			return
		}
		// 仅传输层错误（拿不到上游响应）才会到这里，上游 4xx/5xx 已由
		// forward 原样透传状态码与错误信息。
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "upstream connection error"}})
		return
	}

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
		// normalizeToolChoiceWithTools 兜底：tool_choice 指向被丢弃的服务端
		// 工具时改为不传，避免上游因引用不存在的 function 而 400。
		chatReq.ToolChoice = normalizeToolChoiceWithTools(convertResponsesToolChoice(respReq.ToolChoice), chatReq.Tools)
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
	if !config.ForceDisableThinking() && respReq.Reasoning.Effort != "" {
		if respReq.Reasoning.Effort != "none" {
			chatReq.ReasoningEffort = respReq.Reasoning.Effort
		}
	}

	// 协议路由 anthropic 分支：chatReq（含 messages 转换/多模态/text-only 降级）
	// 已就绪，转交 chatToAnthropicBody 走上游原生 /v1/messages。
	if upstreamProto == upstreamProtocolAnthropic {
		// 与下方 keepReasoning 语义一致：fixToolCallGaps/ensureReasoningContent
		// 属于 chat 翻译路径的修补，交叉路径由 chatToAnthropicBody 自行处理。
		// 组装前先做 tool_use/tool_result 配对归一化（见
		// normalizeAnthropicToolPairing；发生在 chatMessagesToAnthropic 之前）。
		chatReq.Messages = normalizeAnthropicToolPairing(chatReq.Messages)
		// 转 Anthropic 时 service_tier 仅放行上游白名单（auto/standard_only）；
		// 其余值（priority/flex 等 OpenAI 口径）丢弃，避免上游 400。
		if respReq.ServiceTier != "" {
			switch respReq.ServiceTier {
			case "auto", "standard_only":
				if chatReq.ExtraBody == nil {
					chatReq.ExtraBody = map[string]any{}
				}
				chatReq.ExtraBody["service_tier"] = respReq.ServiceTier
			default:
				slog.Info("responses service_tier dropped for anthropic upstream",
					"model", chatReq.Model, "service_tier", respReq.ServiceTier)
			}
		}
		wantReasoningX := !config.ForceDisableThinking()
		forwardResponsesViaAnthropic(w, r, auth, &chatReq, wantReasoningX)
		return
	}

	wantReasoning := !config.ForceDisableThinking()
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
	logging.PlanRequest(r.Context(), map[string]any{
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
		"multimodal_parts":     countMultimodalParts(chatReq.Messages),
		"text_only_model":      modelIsTextOnly(chatReq.Model),
		"max_tokens":           chatReq.MaxTokens,
		"max_tokens_cap":       config.MaxTokensCapFor(chatReq.Model),
	})

	upstreamBody := buildUpstreamBody(&chatReq)

	if respReq.Stream {
		upResp, status, _, err := callOpenCodeAPIStream(r.Context(), upstreamBody, chatReq.Model, auth)
		if err != nil || status < 200 || status >= 300 {
			// 先保留翻译路径的上游错误体，再探测原生透传。
			var transErrBody []byte
			if upResp != nil {
				transErrBody, _ = io.ReadAll(upResp)
				upResp.Close()
			}
			// 翻译路径失败：探测上游原生 responses，成功则透传并记住该模型。
			// 类型化转换错误（上游有明确错误信息）不探测，原样返回。
			if shouldProbeNativeResponses(status, err) && probeNativeResponses(r.Context(), w, auth, chatReq.Model, body, true, respReq) {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if len(transErrBody) > 0 {
				w.Write(transErrBody)
				return
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
		// 翻译路径失败：探测上游原生 responses，成功则透传并记住该模型。
		// 类型化转换错误（上游有明确错误信息）不探测，原样返回。
		if shouldProbeNativeResponses(status, err) && probeNativeResponses(r.Context(), w, auth, chatReq.Model, body, false, respReq) {
			return
		}
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

	result := logging.SummarizeChatResult(respBody)
	logging.LogResult(r.Context(), result)

	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			statsx.RecordChatUsage(chatReq.Model, u)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	logging.MaybeBodySummary(r.Context(), "responses response body", responsesBody)
	w.Write(responsesBody)
}

// ======================== Responses Stream Handler ========================

func responsesInputTokensDetails(details any) map[string]any {
	if m, ok := details.(map[string]any); ok {
		if cached, ok := m["cached_tokens"]; ok && cached != nil {
			return m
		}
		m["cached_tokens"] = 0
		return m
	}
	return map[string]any{"cached_tokens": 0}
}

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
	stats := &logging.StreamStats{Start: time.Now()}

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
	// Some upstreams (e.g. muse-spark-1.2-contributor-free) terminate a stream
	// with a usage-only chunk but no finish_reason and no [DONE]. When the turn
	// produced output and we saw a terminal usage chunk, synthesize completion.
	usageTerminalSeen := false
	toolCalls := map[int]map[string]any{}
	toolOrder := []int{}
	toolKinds := responsesToolKindMap(tools)
	indexAllocator := outputIndexAllocator{}
	reasoningOutputIndex := -1
	messageIndex := -1

	reader := newStreamReader(ctx, resp.Body, 0)

	defer func() {
		stats.TextChars = len(fullText)
		stats.ReasoningChars = len(fullReasoning)
		stats.ToolCallCount = len(toolOrder)
		stats.Log(ctx, "responses")
	}()
	// Reader cleanup: signal goroutine, unblock any pending read, wait for exit.
	defer reader.Close()

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
		writeSSEEvent(w, flusher, "response.reasoning_summary_text.done", map[string]any{
			"type":            "response.reasoning_summary_text.done",
			"sequence_number": seq,
			"item_id":         reasoningID,
			"output_index":    reasoningOutputIndex,
			"summary_index":   0,
			"text":            fullReasoning,
		})
		seq++
		writeSSEEvent(w, flusher, "response.reasoning_summary_part.done", map[string]any{
			"type":            "response.reasoning_summary_part.done",
			"sequence_number": seq,
			"item_id":         reasoningID,
			"output_index":    reasoningOutputIndex,
			"summary_index":   0,
			"part":            map[string]any{"type": "summary_text", "text": fullReasoning},
		})
		seq++
		writeSSEEvent(w, flusher, "response.output_item.done", map[string]any{
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
		writeSSEEvent(w, flusher, "response.output_text.done", map[string]any{
			"type":            "response.output_text.done",
			"sequence_number": seq,
			"item_id":         msgID,
			"output_index":    idx,
			"content_index":   0,
			"text":            fullText,
			"logprobs":        []any{},
		})
		seq++
		writeSSEEvent(w, flusher, "response.content_part.done", map[string]any{
			"type":            "response.content_part.done",
			"sequence_number": seq,
			"item_id":         msgID,
			"output_index":    idx,
			"content_index":   0,
			"part":            map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": fullText},
		})
		seq++
		writeSSEEvent(w, flusher, "response.output_item.done", map[string]any{
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
		writeSSEEvent(w, flusher, "response.refusal.done", map[string]any{
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
		writeSSEEvent(w, flusher, "response.function_call_arguments.done", map[string]any{
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
		writeSSEEvent(w, flusher, "response.output_item.done", map[string]any{
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
		writeSSEEvent(w, flusher, "response.created", map[string]any{
			"type":            "response.created",
			"sequence_number": seq,
			"response":        map[string]any{"id": responseID, "object": "response", "created_at": createdAt, "status": "in_progress", "background": false, "error": nil, "output": []any{}},
		})
		seq++
		writeSSEEvent(w, flusher, "response.in_progress", map[string]any{
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
		writeSSEEvent(w, flusher, "response.failed", map[string]any{
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
		case result := <-reader.Read():
			// bufio.ReadString may return both a non-empty line and an error
			// (e.g. the last line without a trailing newline + io.EOF). Process
			// the line first, then handle the accompanying error via pendingErr.
			pendingErr := result.err

			line := result.line
			trimmed := strings.TrimSpace(line)
			if trimmed == "data: [DONE]" || trimmed == "[DONE]" {
				stats.DoneSeen = true
				if !finished {
					if usageTerminalSeen && (messageStarted || reasoningStarted || len(toolCalls) > 0) {
						stats.SawFinish = true
						stats.FinishReason = "stop"
						finished = true
						break loop
					}
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
							stats.NoteChunk()
							ensureCreated(chunk)
							choices, ok := chunk["choices"].([]any)
							if !ok || len(choices) == 0 {
								if usage, ok := chunk["usage"].(map[string]any); ok {
									totalUsage = usage
									if usageHasCompletion(usage) {
										usageTerminalSeen = true
									}
								}
							} else {
								choice, _ := choices[0].(map[string]any)
								delta, _ := choice["delta"].(map[string]any)
								finishReason, _ := choice["finish_reason"].(string)
								if finishReason != "" {
									stats.FinishReason = finishReason
									stats.SawFinish = true
								}

								if !finished {
									if rc, ok := delta["reasoning_content"]; ok && wantReasoning {
										rcStr, _ := rc.(string)
										if rcStr != "" {
											if !reasoningStarted {
												reasoningOutputIndex = indexAllocator.Allocate()
												seq++
												writeSSEEvent(w, flusher, "response.output_item.added", map[string]any{
													"type":            "response.output_item.added",
													"sequence_number": seq,
													"output_index":    reasoningOutputIndex,
													"item":            reasoningItem("in_progress"),
												})
												seq++
												writeSSEEvent(w, flusher, "response.reasoning_summary_part.added", map[string]any{
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
											writeSSEEvent(w, flusher, "response.reasoning_summary_text.delta", map[string]any{
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
												stats.PromotedReasoning = true
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
											writeSSEEvent(w, flusher, "response.output_item.added", map[string]any{
												"type":            "response.output_item.added",
												"sequence_number": seq,
												"output_index":    idx,
												"item":            map[string]any{"id": msgID, "type": "message", "status": "in_progress", "content": []any{}, "role": "assistant"},
											})
											seq++
											writeSSEEvent(w, flusher, "response.content_part.added", map[string]any{
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
										writeSSEEvent(w, flusher, "response.output_text.delta", map[string]any{
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
										writeSSEEvent(w, flusher, "response.refusal.delta", map[string]any{
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
											writeSSEEvent(w, flusher, "response.output_item.added", map[string]any{
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
											writeSSEEvent(w, flusher, "response.function_call_arguments.delta", map[string]any{
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
						if usageTerminalSeen && (messageStarted || reasoningStarted || len(toolCalls) > 0) {
							stats.SawFinish = true
							stats.FinishReason = "stop"
							finished = true
							break loop
						}
						emitResponseFailed("stream ended without finish_reason")
						return
					}
					break loop
				}
				logging.FromContext(ctx).Error("stream read error", "error", pendingErr)
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
		writeSSEEvent(w, flusher, "response.output_item.added", map[string]any{
			"type":            "response.output_item.added",
			"sequence_number": seq,
			"output_index":    idx,
			"item":            map[string]any{"id": msgID, "type": "message", "status": "in_progress", "content": []any{}, "role": "assistant"},
		})
		seq++
		writeSSEEvent(w, flusher, "response.content_part.added", map[string]any{
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
		usage["input_tokens_details"] = responsesInputTokensDetails(totalUsage["prompt_tokens_details"])
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
		statsx.RecordChatUsage(model, totalUsage)
	}

	seq++
	writeSSEEvent(w, flusher, terminalEvent, map[string]any{
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
	// 空输出补一条空 message：Responses 客户端（Codex/官方 SDK）期望非空
	// output；纯 reasoning（wantReasoning=false）+ 无内容的回合兜底空文本，
	// 保持数组形状与 status。
	if len(output) == 0 {
		output = append(output, emptyAssistantMessageItem(outputID, status))
	}
	responses["output"] = output
	if chat.Usage != nil {
		responses["usage"] = chatUsageMapToResponses(chat.Usage)
	}

	result, _ := json.Marshal(responses)
	return result
}

// emptyAssistantMessageItem 构造条 status 一致的空 output_text message。
func emptyAssistantMessageItem(outputID, status string) map[string]any {
	return map[string]any{
		"id":     outputID,
		"type":   "message",
		"status": status,
		"role":   "assistant",
		"content": []any{map[string]any{
			"type":        "output_text",
			"text":        "",
			"annotations": []any{},
			"logprobs":    []any{},
		}},
	}
}

// chatUsageMapToResponses 把 Chat Completions usage（上游 zen/go 口径）转
// Responses 口径：
//   - input_tokens = prompt_tokens + 顶层 cache_read_input_tokens +
//     cache_creation_input_tokens（anthropicUsageToChat 已把这两个顶层字段透传
//     进 chat usage，而 prompt_tokens 是缓存感知的读数，按 Responses 口径加回）。
//     无缓存字段时退化为原 prompt_tokens（保持既有行为）。
//   - input_tokens_details.cached_tokens 优先取顶层 cache_read_input_tokens
//     （未见时退回 prompt_tokens_details.cached_tokens，再兜底 0）。
//   - output_tokens_details 从 completion_tokens_details 透传
//     reasoning_tokens（thinking token）等细节。
func chatUsageMapToResponses(u map[string]any) map[string]any {
	usage := map[string]any{}
	readInt := func(v any) int64 {
		if n, ok := numberAsFloat(v); ok {
			return int64(n)
		}
		return 0
	}
	// input：prompt 分量 + 顶层缓存字段
	prompt, hasPrompt := u["prompt_tokens"]
	cacheRead, hasCacheRead := u["cache_read_input_tokens"]
	cacheCreation, hasCacheCreation := u["cache_creation_input_tokens"]
	inputTotal := readInt(prompt)
	if hasPrompt {
		if hasCacheRead {
			inputTotal += readInt(cacheRead)
		}
		if hasCacheCreation {
			inputTotal += readInt(cacheCreation)
		}
		usage["input_tokens"] = inputTotal
	}
	// cached_tokens：优先顶层 cache_read_input_tokens，退回 prompt_tokens_details。
	// 其余 prompt_tokens_details 字段（text_tokens 等）原样透传。
	var detailsOut map[string]any
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		detailsOut = make(map[string]any, len(d)+1)
		for k, v := range d {
			detailsOut[k] = v
		}
	} else {
		detailsOut = map[string]any{}
	}
	cached := int64(0)
	if hasCacheRead {
		cached = readInt(cacheRead)
	} else if c, ok := detailsOut["cached_tokens"]; ok {
		cached = readInt(c)
	}
	detailsOut["cached_tokens"] = cached
	usage["input_tokens_details"] = detailsOut
	if v, ok := u["completion_tokens"]; ok {
		usage["output_tokens"] = v
	}
	if v, ok := u["completion_tokens_details"]; ok {
		usage["output_tokens_details"] = v
	} else if reasoningTokens := reasoningTokenEstimate(u); reasoningTokens > 0 {
		usage["output_tokens_details"] = map[string]any{"reasoning_tokens": reasoningTokens}
	}
	if v, ok := u["total_tokens"]; ok {
		usage["total_tokens"] = v
	}
	if v, ok := u["input_tokens"]; ok && usage["input_tokens"] == nil {
		usage["input_tokens"] = v
	}
	if v, ok := u["output_tokens"]; ok && usage["output_tokens"] == nil {
		usage["output_tokens"] = v
	}
	return usage
}

// reasoningTokenEstimate 从 usage 中提取 thinking/reasoning token 数量的兜底
// 估算：缺 completion_tokens_details 时按已知顶层/通用键查找。
func reasoningTokenEstimate(u map[string]any) int64 {
	for _, k := range []string{"reasoning_tokens", "thinking_tokens"} {
		if v, ok := numberAsFloat(u[k]); ok && v > 0 {
			return int64(v)
		}
	}
	return 0
}
