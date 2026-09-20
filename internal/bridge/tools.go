package bridge

import (
	"encoding/json"
	"strings"
)

// This file holds the pure Responses<->Chat tool conversion helpers moved out
// of internal/app. None of them touch I/O, logging, metrics, or config; any
// externally-derived input arrives as a value parameter. The app layer keeps
// same-name lowercase forwarding shells so existing *_test.go files compile
// unchanged.

// convertResponsesTools 把 Responses tools 转 Chat Completions tools。
// 服务端工具（web_search/file_search/computer_use/mcp/local_shell/custom 等）
// 无对应 function 形状，由 responsesToolFunction 返回 ok=false，此处丢弃并
// 返回丢弃计数（供调用方打日志）；若 tool_choice 指向被丢弃的工具，由调用方
// normalizeToolChoiceWithTools 兜底为不传。
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

// MessageTextContent 从 Chat content（纯字符串或多模态 parts 数组）提取可见文本。
// 非文本分片（image_url/file 等）在无文本时兜底为其 JSON 字符串，避免空内容消息。
// (Moved from app/responses.go messageTextContent.)
func MessageTextContent(content any) string { return messageTextContent(content) }

// mergeConsecutiveSameRole 把相邻同 role 的 Message 合并。tool（合并到
// 前一条，保留 ToolCallID 供配对索引，后续已展开为 tool_result）与
// system/developer/user/assistant（仅无 tool_calls 时按对话回合合并）分别处理。
// Anthropic 要求 tool_use 与其 tool_result 之间无任意 user/assistant 文本，
// 本归一化与 NormalizeAnthropicToolPairing 配合恢复工具配对与回合交替。
func MergeConsecutiveSameRole(msgs []Message) []Message {
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

// MergeAdjacentToolCallAssistants 把相邻的「纯 tool_call assistant」消息合并
// 为单条，使 Responses 并行调用（多个 function_call）共享同一 assistant。
// (Moved from app/responses.go.)
func MergeAdjacentToolCallAssistants(msgs []Message) []Message {
	return mergeAdjacentToolCallAssistants(msgs)
}

// NormalizeAnthropicToolPairing 把 Chat messages 归一化成满足 Anthropic
// tool_use/tool_result 不变量的序列：剔除未答复的 assistant.tool_calls（连同空
// assistant 消息）、剔除孤儿 tool 消息，并把每个 tool 结果紧贴它的 assistant
// 消息后排序。
//
// 同时剔除 parseToolCallArguments 解析失败（`_raw` 兜底）的非法 arguments 调用
// 及其 output —— 防上游 400 死循环。最后跑一次相邻同 role 合并恢复交替。
//
// droppedCalls / droppedOrphans 计数返回给调用方打日志（bridge 不打日志）。
// (Moved from app/responses.go normalizeAnthropicToolPairing, dropped counts
// now returned instead of logged.)
func NormalizeAnthropicToolPairing(messages []Message) (out []Message, droppedCalls, droppedOrphans int) {
	// 把相邻的「纯 tool_call assistant」合并为一条,使并行调用共享同一
	// assistant,其 tool 结果按 call 序紧邻排列(对应 sub2api 的并行 call 归并)。
	messages = MergeAdjacentToolCallAssistants(messages)
	// 索引所有 tool 结果消息按 ToolCallID（后出现覆盖先前同 id）。
	results := map[string]Message{}
	for _, m := range messages {
		if m.Role == "tool" && m.ToolCallID != "" {
			results[m.ToolCallID] = m
		}
	}

	out = make([]Message, 0, len(messages))
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
	return MergeConsecutiveSameRole(out), droppedCalls, droppedOrphans
}

// ConvertResponsesTools 把 Responses tools 转 Chat Completions tools。
// 服务端工具（web_search/file_search/computer_use/mcp/local_shell/custom 等）
// 无对应 function 形状，由 responsesToolFunction 返回 ok=false，此处丢弃并
// 返回丢弃计数（供调用方打日志）；若 tool_choice 指向被丢弃的工具，由调用方
// NormalizeToolChoiceWithTools 兜底为不传。
// (Moved from app/responses.go convertResponsesTools, dropped count returned.)
func ConvertResponsesTools(tools []ResponsesTool) (converted []Tool, dropped int) {
	converted = make([]Tool, 0, len(tools))
	for _, tool := range tools {
		fn, ok := ResponsesToolFunction(tool)
		if !ok {
			dropped++
			continue
		}
		converted = append(converted, Tool{Type: "function", Function: fn})
	}
	return converted, dropped
}

// ResponsesToolFunction 提取一个 Responses tool 的 Chat function 形状。
// (Moved from app/responses.go.)
func ResponsesToolFunction(tool ResponsesTool) (ToolFunction, bool) {
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

// ResponsesToolName 取一个 Responses tool 的规范名。
// (Moved from app/responses.go.)
func ResponsesToolName(tool ResponsesTool) string {
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

// ResponsesToolKindMap 构造 name -> Responses tool type 的映射。
// (Moved from app/responses.go.)
func ResponsesToolKindMap(tools []ResponsesTool) map[string]string {
	kinds := make(map[string]string, len(tools))
	for _, tool := range tools {
		name := ResponsesToolName(tool)
		if name == "" {
			continue
		}
		kinds[name] = tool.Type
	}
	return kinds
}

// IncludeHas reports whether the include array contains the given key.
// (Moved from app/responses.go.)
func IncludeHas(include []string, key string) bool {
	for _, v := range include {
		if v == key {
			return true
		}
	}
	return false
}

// ToolCallOutputType 把 tool name 映射到 Responses output item type。
// (Moved from app/responses.go.)
func ToolCallOutputType(name string, kinds map[string]string) string {
	switch kinds[name] {
	case "apply_patch":
		return "apply_patch_call"
	case "shell":
		return "shell_call"
	default:
		return "function_call"
	}
}

// NormalizeToolChoiceWithTools 在 ConvertResponsesToolChoice 结果上兜底：
// 当 tool_choice.name 指向的函数不在已保留的 Chat tools 里（例如对应的是被
// 丢弃的服务端工具 web_search/file_search/computer_use/mcp/local_shell/custom），
// 把 tool_choice 改为不传（返回 nil），避免上游因引用了不存在的工具而 400。
// (Moved from app/responses.go.)
func NormalizeToolChoiceWithTools(choice any, tools []Tool) any {
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

// ConvertResponsesToolChoice 把 Responses tool_choice 转 Chat tool_choice。
// (Moved from app/responses.go.)
func ConvertResponsesToolChoice(choice any) any {
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

// ConvertResponsesTextToResponseFormat translates the Responses API `text`
// parameter ({format:{type:...}, verbosity:...}) into the Chat Completions
// `response_format` shape ({type:...}) that upstream providers require.
//
// Returns nil when no representable format can be built (unknown type,
// missing required json_schema fields, or a non-object text value) so the
// caller can omit response_format instead of sending a malformed object that
// upstream would reject with a 400.
// (Moved from app/responses.go.)
func ConvertResponsesTextToResponseFormat(text any) any {
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

// parseToolCallArguments 解析 tool_call arguments（非法时以 _raw 兜底）。
// (Moved from app/chat_to_anthropic.go.)
func parseToolCallArguments(args string) map[string]any {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return map[string]any{}
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(trimmed), &input); err == nil {
		return input
	}
	return map[string]any{"_raw": args}
}

// ParseToolCallArguments 解析 tool_call arguments（非法时以 _raw 兜底）。
// (Moved from app/chat_to_anthropic.go.)
func ParseToolCallArguments(args string) map[string]any { return parseToolCallArguments(args) }

// applyErrorPrefix prepends the stable "Error: " marker used by both the
// Anthropic Messages and Responses request paths when a tool_result carries
// is_error:true. It avoids producing a duplicate prefix when the output text
// already starts with "Error:" (e.g. an upstream that echoes the error).
// (Moved from app/chat_protocol.go.)
func ApplyErrorPrefix(text string) string {
	if strings.HasPrefix(text, "Error:") {
		return text
	}
	return "Error: " + text
}
