package app

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"
)

// ======================== 免费层指纹: 回复侧大小写还原 ========================
//
// 请求体指纹（harness preamble + CLI 工具 manifest）在 fingerprint_manifest.go。
// applyFreeTierFingerprint 只是它的入口。本文件保留 Claude 系客户端的占位名
// 大小写还原，以及非流 SSE 聚合。

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

// freeTierStubCanonicalCase 是四件免费层占位工具小写名到 Claude Code 风格
// 规范大小写(PascalCase)的固定映射。仅对 Claude 系客户端生效,门控见
// shouldRestoreToolCase;非 Claude 客户端保持小写透传。
var freeTierStubCanonicalCase = map[string]string{
	"bash": "Bash",
	"glob": "Glob",
	"grep": "Grep",
	"read": "Read",
}

// restoreToolNameCase 把免费层占位工具的小写 tool_use 名(bash/glob/grep/read)
// 还原为 Claude Code 注册的规范 PascalCase(Bash/Glob/Grep/Read)。只在
// shouldRestoreToolCase 判定为 Claude 系客户端时由调用方启用;其余名字
// (包括客户端本来就声明为小写的普通工具如 "weather",以及已按规范大小写的
// 名字)一律原样返回——映射只命中四个精确小写串,幂等且无歧义。
func restoreToolNameCase(name string) string {
	if canonical, ok := freeTierStubCanonicalCase[name]; ok {
		return canonical
	}
	return name
}

// isClaudeCodeClient 判定 UA 是否属于 Claude 系客户端。UA 包含子串
// "claude"(大小写不敏感)即视为 Claude 系。只用于工具名大小写还原门控。
func isClaudeCodeClient(ua string) bool {
	return strings.Contains(strings.ToLower(ua), "claude")
}

// shouldRestoreToolCase 决定本次请求是否在响应路径上还原免费层小写占位
// 工具名。依据为下游客户端 UA(中间件 withClientHeadersFromRequest 已快照
// 到 ctx)。拿不到 UA 时返回 false(默认不还原,保持小写透传)。
func shouldRestoreToolCase(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	return isClaudeCodeClient(upstreamHeadersFromContext(ctx).Get("User-Agent"))
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
// 上游门禁按小写名精确匹配(大小写敏感),因此追加的 stub 一律小写:Claude
// Code 的 PascalCase(Bash/Glob/Grep/Read)在建 existing 键时统一小写做命中
// 检查,但命中不代表门禁通过——仍需追加小写 stub(2026-10-07 public 实测:
// 只带 PascalCase 四件被 403 FreeTierError 整档拒)。
// subpath == "messages" 用 Anthropic 的裸 name+input_schema 形状；
// subpath == "responses" 用 OpenAI Responses 的 type:function+parameters 形状；
// chat/completions 沿用 OpenAI 嵌套 function 形状。tools 已补齐时不动。
// bare name 命中检查由 freeTierToolNameOf 兼容三种形状。
func ensureFreeTierTools(bodyMap map[string]any, subpath string) {
	if bodyMap == nil {
		return
	}
	rawTools, _ := bodyMap["tools"].([]any)
	// existing 只记录精确小写名:上游门禁按小写名精确匹配(大小写敏感),
	// PascalCase(Bash 等)命中不代表门禁通过,仍需追加小写 stub。
	existing := make(map[string]bool, len(rawTools)+len(freeTierRequiredTools))
	for _, t := range rawTools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if name := freeTierToolNameOf(tm); name != "" && name == strings.ToLower(name) {
			existing[name] = true
		}
	}
	missing := make([]any, 0, len(freeTierRequiredTools))
	switch subpath {
	case "messages":
		for _, tool := range freeTierRequiredToolsAnthropic {
			if !existing[strings.ToLower(tool["name"].(string))] {
				fixed := map[string]any{
					"name":         strings.ToLower(tool["name"].(string)),
					"description":  tool["description"],
					"input_schema": tool["input_schema"],
				}
				missing = append(missing, fixed)
			}
		}
	case "responses":
		for _, tool := range freeTierRequiredToolsResponses {
			if !existing[strings.ToLower(tool["name"].(string))] {
				fixed := map[string]any{
					"type":        tool["type"],
					"name":        strings.ToLower(tool["name"].(string)),
					"description": tool["description"],
					"parameters":  tool["parameters"],
				}
				missing = append(missing, fixed)
			}
		}
	default: // chat/completions
		for _, tool := range freeTierRequiredTools {
			fn := tool["function"].(map[string]any)
			if !existing[strings.ToLower(fn["name"].(string))] {
				fixed := map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        strings.ToLower(fn["name"].(string)),
						"description": fn["description"],
						"parameters":  fn["parameters"],
					},
				}
				missing = append(missing, fixed)
			}
		}
	}
	if len(missing) == 0 {
		return
	}
	bodyMap["tools"] = append(rawTools, missing...)
}

// ======================== 免费层指纹: system prompt <env> 门禁 ========================
//
// 上游免费层第 6 层门禁（实测见 rea-opencode）：
// 服务端要求 system prompt 必须包含合规的 <env> 块：同时出现 "<env>"、
// "Working directory:"、"</env>"，区分大小写，且必须顺序闭合。
// 缺失时服务端刻意返回与配额耗尽相同的 429 FreeUsageLimitError 响应体和
// retry-after 头。网关在指纹阶段前置注入，对齐 CLI 行为且保持跨轮前缀缓存稳定。

const freeTierDefaultEnvBlock = "<env>\n  Working directory: /workspace\n</env>"

// hasFreeTierEnvBlock 校验字符串是否已包含合规的免费层 <env> 块。
func hasFreeTierEnvBlock(s string) bool {
	envStart := strings.Index(s, "<env>")
	if envStart == -1 {
		return false
	}
	workDirRel := strings.Index(s[envStart:], "Working directory:")
	if workDirRel == -1 {
		return false
	}
	envEndRel := strings.Index(s[envStart+workDirRel:], "</env>")
	return envEndRel != -1
}

func ensureFreeTierSystemEnvChat(bodyMap map[string]any) {
	rawMsgs, ok := bodyMap["messages"].([]any)
	if !ok || len(rawMsgs) == 0 {
		bodyMap["messages"] = []any{
			map[string]any{
				"role":    "system",
				"content": freeTierDefaultEnvBlock,
			},
		}
		return
	}
	for _, m := range rawMsgs {
		msg, ok := m.(map[string]any)
		if !ok || msg["role"] != "system" {
			continue
		}
		switch c := msg["content"].(type) {
		case string:
			if !hasFreeTierEnvBlock(c) {
				if strings.TrimSpace(c) == "" {
					msg["content"] = freeTierDefaultEnvBlock
				} else {
					msg["content"] = freeTierDefaultEnvBlock + "\n\n" + c
				}
			}
			return
		case []any:
			for _, part := range c {
				if pm, ok := part.(map[string]any); ok {
					if text, _ := pm["text"].(string); hasFreeTierEnvBlock(text) {
						return
					}
				}
			}
			envPart := map[string]any{"type": "text", "text": freeTierDefaultEnvBlock}
			msg["content"] = append([]any{envPart}, c...)
			return
		}
	}
	sysMsg := map[string]any{
		"role":    "system",
		"content": freeTierDefaultEnvBlock,
	}
	bodyMap["messages"] = append([]any{sysMsg}, rawMsgs...)
}

func ensureFreeTierSystemEnvMessages(bodyMap map[string]any) {
	sysVal, ok := bodyMap["system"]
	if !ok || sysVal == nil {
		bodyMap["system"] = freeTierDefaultEnvBlock
		return
	}
	switch s := sysVal.(type) {
	case string:
		if !hasFreeTierEnvBlock(s) {
			if strings.TrimSpace(s) == "" {
				bodyMap["system"] = freeTierDefaultEnvBlock
			} else {
				bodyMap["system"] = freeTierDefaultEnvBlock + "\n\n" + s
			}
		}
	case []any:
		for _, part := range s {
			if pm, ok := part.(map[string]any); ok {
				if text, _ := pm["text"].(string); hasFreeTierEnvBlock(text) {
					return
				}
			}
		}
		envPart := map[string]any{"type": "text", "text": freeTierDefaultEnvBlock}
		bodyMap["system"] = append([]any{envPart}, s...)
	default:
		bodyMap["system"] = freeTierDefaultEnvBlock
	}
}

func ensureFreeTierSystemEnvResponses(bodyMap map[string]any) {
	instrVal, ok := bodyMap["instructions"]
	if !ok || instrVal == nil {
		bodyMap["instructions"] = freeTierDefaultEnvBlock
		return
	}
	switch s := instrVal.(type) {
	case string:
		if !hasFreeTierEnvBlock(s) {
			if strings.TrimSpace(s) == "" {
				bodyMap["instructions"] = freeTierDefaultEnvBlock
			} else {
				bodyMap["instructions"] = freeTierDefaultEnvBlock + "\n\n" + s
			}
		}
	default:
		bodyMap["instructions"] = freeTierDefaultEnvBlock
	}
}

// ensureFreeTierSystemEnv 检查并补齐免费层上游所需的 system prompt <env> 自证块。
func ensureFreeTierSystemEnv(bodyMap map[string]any, subpath string) {
	if bodyMap == nil {
		return
	}
	switch subpath {
	case "messages":
		ensureFreeTierSystemEnvMessages(bodyMap)
	case "responses":
		ensureFreeTierSystemEnvResponses(bodyMap)
	default: // chat/completions
		ensureFreeTierSystemEnvChat(bodyMap)
	}
}

// applyFreeTierFingerprint 对解析后的上游模型本身免费的请求执行免费层指纹
// 重做（按模型判定，不按客户端 Authorization tier）。tools / system 对齐
// zen-proxy：前置 CLI harness preamble 与工具 manifest。stream 强制 true；
// chat/responses 补 include_usage。count_tokens / systemone 不套该门禁。
func applyFreeTierFingerprint(bodyMap map[string]any, subpath, modelID string) {
	applyFreeTierBodyFingerprint(bodyMap, subpath, modelID)
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
func aggregateOpenAIStream(body []byte, modelID string, restoreCase bool) []byte {
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
		// issue #34:choice 级 tool_calls 归位进 delta 后再按既有逻辑聚合。
		hoistChoiceSiblingToolCalls(choice)
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
						// 免费层模型看到注入的小写占位工具后可能以
						// "bash"/"read" 等小写名发起 tool_call;仅对 Claude
						// 系客户端还原为规范大小写(Claude Code 大小写敏感
						// 拒收小写名),其余客户端保持小写透传。
						if restoreCase {
							acc.name = restoreToolNameCase(n)
						} else {
							acc.name = n
						}
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
