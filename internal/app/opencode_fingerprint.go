package app

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// ======================== 免费层指纹: 工具与 stream 门禁 ========================
//
// 上游 2026-09-18 新增校验(移植自 lite opencode2api-lite.go L1468-1548):
// 免费层请求 body 的 tools 必须包含 bash / glob / grep / read 四件工具
// (顺序、描述、额外工具均无所谓,缺任一即 403 FreeTierError)。
// 客户端(cherry studio / codex / 普通聊天)通常不带全这些工具,这里准备
// 最小定义,由 ensureFreeTierTools 把缺失的补进请求。描述与参数结构可任意,
// 仅需名字命中。
var freeTierRequiredTools = []map[string]any{
	{"type": "function", "function": map[string]any{
		"name":        "bash",
		"description": "Run a shell command and return its output",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string", "description": "The shell command to execute"}},
			"required":   []string{"command"},
		},
	}},
	{"type": "function", "function": map[string]any{
		"name":        "glob",
		"description": "Find files matching a glob pattern",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"pattern": map[string]any{"type": "string", "description": "The glob pattern to match files against"}},
			"required":   []string{"pattern"},
		},
	}},
	{"type": "function", "function": map[string]any{
		"name":        "grep",
		"description": "Search file contents with a regular expression",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"pattern": map[string]any{"type": "string", "description": "The regular expression pattern to search for"}},
			"required":   []string{"pattern"},
		},
	}},
	{"type": "function", "function": map[string]any{
		"name":        "read",
		"description": "Read the contents of a file",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"file_path": map[string]any{"type": "string", "description": "The path of the file to read"}},
			"required":   []string{"file_path"},
		},
	}},
}

// freeTierRequiredToolsAnthropic 是 Anthropic Messages / OpenAI Responses
// 协议形状(tools[].name / input_schema)下的同一组四件占位工具。
var freeTierRequiredToolsAnthropic = []map[string]any{
	{"name": "bash", "description": "Run a shell command and return its output",
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string", "description": "The shell command to execute"}},
			"required":   []string{"command"},
		}},
	{"name": "glob", "description": "Find files matching a glob pattern",
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"pattern": map[string]any{"type": "string", "description": "The glob pattern to match files against"}},
			"required":   []string{"pattern"},
		}},
	{"name": "grep", "description": "Search file contents with a regular expression",
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"pattern": map[string]any{"type": "string", "description": "The regular expression pattern to search for"}},
			"required":   []string{"pattern"},
		}},
	{"name": "read", "description": "Read the contents of a file",
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"file_path": map[string]any{"type": "string", "description": "The path of the file to read"}},
			"required":   []string{"file_path"},
		}},
}

// freeTierRequiredToolsResponses 是 OpenAI Responses 协议形状下的同一组四件
// 占位工具：{"type":"function","name","description","parameters"}。Anthropic
// 的 input_schema 形状不能用于 responses 子路径（上游按 FunctionTool 校验，
// 缺 type/parameters 报 `did not match any supported type`）。
var freeTierRequiredToolsResponses = []map[string]any{
	{"type": "function", "name": "bash", "description": "Run a shell command and return its output",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string", "description": "The shell command to execute"}},
			"required":   []string{"command"},
		}},
	{"type": "function", "name": "glob", "description": "Find files matching a glob pattern",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"pattern": map[string]any{"type": "string", "description": "The glob pattern to match files against"}},
			"required":   []string{"pattern"},
		}},
	{"type": "function", "name": "grep", "description": "Search file contents with a regular expression",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"pattern": map[string]any{"type": "string", "description": "The regular expression pattern to search for"}},
			"required":   []string{"pattern"},
		}},
	{"type": "function", "name": "read", "description": "Read the contents of a file",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"file_path": map[string]any{"type": "string", "description": "The path of the file to read"}},
			"required":   []string{"file_path"},
		}},
}

// freeTierToolNameOf 同时识别三种上游协议形状里的工具名:
// OpenAI Chat(tools[].function.name)与 Anthropic Messages / OpenAI
// Responses(tools[].name)。
func freeTierToolNameOf(t map[string]any) string {
	if fn, ok := t["function"].(map[string]any); ok {
		if name, _ := fn["name"].(string); name != "" {
			return name
		}
	}
	name, _ := t["name"].(string)
	return name
}

// ensureFreeTierTools 检查上游请求体里的 tools,把 bash/glob/grep/read 四件中
// 缺失的**逐项**补上(只补缺失项、保留客户端已有工具的原始位置与形状,
// 四件按 bash,glob,grep,read 顺序追加在后),而不是"客户端带了任一工具就整包
// 跳过"——上游按"有无四件"整体判定,部分带齐与完全不带一样会被 403。
// subpath == "messages" 用 Anthropic 的裸 name+input_schema 形状；
// subpath == "responses" 用 OpenAI Responses 的 type:function+parameters 形状；
// chat/completions 沿用 OpenAI 嵌套 function 形状。tools 已补齐时不动。
// bare name 命中检查由 freeTierToolNameOf 兼容三种形状。
func ensureFreeTierTools(bodyMap map[string]any, subpath string) {
	if bodyMap == nil {
		return
	}
	rawTools, _ := bodyMap["tools"].([]any)
	existing := make(map[string]bool, len(rawTools)+len(freeTierRequiredTools))
	for _, t := range rawTools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if name := freeTierToolNameOf(tm); name != "" {
			existing[name] = true
		}
	}
	missing := make([]any, 0, len(freeTierRequiredTools))
	switch subpath {
	case "messages":
		for _, tool := range freeTierRequiredToolsAnthropic {
			if !existing[tool["name"].(string)] {
				missing = append(missing, tool)
			}
		}
	case "responses":
		for _, tool := range freeTierRequiredToolsResponses {
			if !existing[tool["name"].(string)] {
				missing = append(missing, tool)
			}
		}
	default: // chat/completions
		for _, tool := range freeTierRequiredTools {
			fn := tool["function"].(map[string]any)
			if !existing[fn["name"].(string)] {
				missing = append(missing, tool)
			}
		}
	}
	if len(missing) == 0 {
		return
	}
	bodyMap["tools"] = append(rawTools, missing...)
}

// applyFreeTierFingerprint 对"解析后的上游模型本身是免费"的请求执行免费层
// 指纹重做(issue #19,2026-09-18 实测门禁口径:按解析后的上游模型判定,
// 不按客户端 Authorization tier):
//   - tools: 缺 bash/glob/grep/read 任何一件时按上游协议形状补齐缺失项;
//   - stream: 一律强制 true(上游仅接受流式);
//   - stream_options: chat/completions 与 responses 子路径保留客户端自定义键、
//     仅补 include_usage=true;messages 子路径不写 stream_options(不属于
//     Anthropic schema)。
//
// count_tokens 及其它非三协议子路径不套该门禁。
func applyFreeTierFingerprint(bodyMap map[string]any, subpath, modelID string) {
	if bodyMap == nil {
		return
	}
	switch subpath {
	case "chat/completions", "messages", "responses":
	default:
		return
	}
	if !isFreeModel(modelID) {
		return
	}
	ensureFreeTierTools(bodyMap, subpath)
	bodyMap["stream"] = true
	if subpath == "messages" {
		return
	}
	if existing, ok := bodyMap["stream_options"].(map[string]any); ok {
		existing["include_usage"] = true
	} else {
		bodyMap["stream_options"] = map[string]any{"include_usage": true}
	}
}

// ======================== 非流聚合 ========================

// aggregateOpenAIStream 把上游强制 stream:true 返回的 OpenAI SSE 流聚合为
// 一个完整的 chat.completion JSON(移植自 lite opencode2api-lite.go
// L1947-1983)。上游免费层新校验要求免费层请求必须 stream:true;对客户端
// 声明的非流式请求,代理在上游侧改走流式并在本地还原为等价的非流式 JSON,
// 客户端感知不变。
// 主体不是 OpenAI SSE(如上游直发 JSON、空响应或流中带 error 事件)时原样返回,
// 由上层既有逻辑处理,因此已 2xx 的流式路径不会被双重聚合(调用方不会把
// 聚合结果再喂回来)。
func aggregateOpenAIStream(body []byte, modelID string) []byte {
	var id string
	var created int64
	var contentBuilder, reasoningBuilder strings.Builder
	finishReason := ""
	role := "assistant"
	var usage map[string]any
	type toolAcc struct {
		id, name string
		args     strings.Builder
	}
	tools := map[int]*toolAcc{}
	toolOrder := []int{}
	sawChunk := false

	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		data := bytes.TrimSpace(line[6:])
		if string(data) == "[DONE]" {
			break
		}
		var chunk map[string]any
		if err := json.Unmarshal(data, &chunk); err != nil {
			continue
		}
		sawChunk = true
		if _, ok := chunk["error"]; ok {
			// 流内错误事件: 交给上层原有逻辑处理,不做聚合
			return body
		}
		if v, ok := chunk["id"].(string); ok && v != "" && id == "" {
			id = v
		}
		if v, ok := chunk["created"].(float64); ok && created == 0 {
			created = int64(v)
		}
		if u, ok := chunk["usage"].(map[string]any); ok && u != nil {
			usage = u
		}
		choices, ok := chunk["choices"].([]any)
		if !ok || len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if choice == nil {
			continue
		}
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			finishReason = fr
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if r, ok := delta["role"].(string); ok && r != "" {
			role = r
		}
		if c, ok := delta["content"].(string); ok && c != "" {
			contentBuilder.WriteString(c)
		}
		if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
			reasoningBuilder.WriteString(rc)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, rtc := range tcs {
				tc, ok := rtc.(map[string]any)
				if !ok {
					continue
				}
				idxF, _ := tc["index"].(float64)
				idx := int(idxF)
				acc := tools[idx]
				if acc == nil {
					acc = &toolAcc{}
					tools[idx] = acc
					toolOrder = append(toolOrder, idx)
				}
				if v, ok := tc["id"].(string); ok && v != "" {
					acc.id = v
				}
				if fn, ok := tc["function"].(map[string]any); ok {
					if n, ok := fn["name"].(string); ok && n != "" {
						acc.name = n
					}
					if a, ok := fn["arguments"].(string); ok && a != "" {
						acc.args.WriteString(a)
					}
				}
			}
		}
	}
	if !sawChunk {
		return body
	}
	if id == "" {
		id = "chatcmpl_" + randomString(24)
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	if finishReason == "" {
		finishReason = "stop"
	}

	message := map[string]any{"role": role, "content": contentBuilder.String()}
	if reasoningBuilder.Len() > 0 {
		message["reasoning_content"] = reasoningBuilder.String()
	}
	if len(toolOrder) > 0 {
		toolCalls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			acc := tools[idx]
			toolCalls = append(toolCalls, map[string]any{
				"id":   acc.id,
				"type": "function",
				"function": map[string]any{
					"name":      acc.name,
					"arguments": acc.args.String(),
				},
			})
		}
		message["tool_calls"] = toolCalls
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   modelID,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return body
	}
	return out
}
