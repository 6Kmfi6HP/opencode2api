package app

import (
	_ "embed"
	"encoding/json"
	"strings"
)

// ======================== 免费层指纹: harness preamble + 工具 manifest ========================
//
// 上游免费层门禁按"真实 OpenCode CLI 请求"校验请求体（zen-proxy 对线上网关的
// 实测口径，2026-11）：
//   - system 文本必须是 CLI 自己的 harness preamble（agent-preamble.txt，
//     截断到 ~1.8 KB 或换成通用提示词都按 429 FreeUsageLimitError 拒绝）；
//   - tools 必须携带 CLI 自己的工具 manifest（agent-tools.json 里能映射回
//     调用方工具的 6 件：read/edit/glob/grep/shell/write），排在调用方工具之前。
//
// 旧实现只补 <env> 自证块 + bash/glob/grep/read 四件小写 stub，已被该门禁拒绝。
// 本文件按 zen-proxy 的逻辑重做：preamble 前置（不替换调用方原文）、manifest
// 前置（与 manifest 同名的调用方工具丢弃）、无工具调用方强制 tool_choice=none。
// 回复侧把 manifest 调用映射回调用方工具名并丢弃调用方无法执行的调用，见
// agent_tool_reply.go。

//go:embed fingerprint/agent-preamble.txt
var agentPreambleText string

//go:embed fingerprint/agent-tools.json
var agentToolsJSON []byte

// agentPreambleMarker 是 harness preamble 的首行。system 文本已以此开头时
// 视为 OpenCode 自己（或串联的代理）的请求，不再重复前置。
const agentPreambleMarker = "You are an AI agent running in OpenCode"

// agentToolSpec 是 manifest 工具名到调用方工具的映射（对齐 zen-proxy
// AGENT_TOOL_MAP）：Name 是 Anthropic 官方名（Claude Code 注册的 PascalCase），
// Args 是 manifest 参数名 → 调用方参数名，值为 nil 的参数在映射时丢弃
// （bash/shell 的 workdir 在调用方 Bash 工具上没有对应物）。
type agentToolSpec struct {
	Name string
	Args map[string]*string
}

var agentToolMap = map[string]agentToolSpec{
	"read":  {Name: "Read", Args: map[string]*string{"filePath": new("file_path")}},
	"edit":  {Name: "Edit", Args: map[string]*string{"filePath": new("file_path"), "oldString": new("old_string"), "newString": new("new_string"), "replaceAll": new("replace_all")}},
	"write": {Name: "Write", Args: map[string]*string{"filePath": new("file_path")}},
	// nil 表示该参数在调用方工具上没有对应物，映射时丢弃。
	"bash":  {Name: "Bash", Args: map[string]*string{"workdir": nil}},
	"shell": {Name: "Bash", Args: map[string]*string{"workdir": nil}},
	"grep":  {Name: "Grep", Args: map[string]*string{}},
	"glob":  {Name: "Glob", Args: map[string]*string{}},
	"list":  {Name: "LS", Args: map[string]*string{}},
}

// agentManifestChat 是 manifest 的 chat 形状（{"type":"function","function":{...}}），
// 与嵌入文件逐字节一致，init 时一次性解析。
var agentManifestChat []map[string]any

func init() {
	var parsed struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(agentToolsJSON, &parsed); err != nil {
		return
	}
	for _, t := range parsed.Tools {
		fn, _ := t["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if _, ok := agentToolMap[name]; ok {
			agentManifestChat = append(agentManifestChat, t)
		}
	}
}

// agentManifestTools 返回指定上游协议形状的 manifest 副本（每次新建 map，
// 调用方可安全改写）：
//   - "messages"：Anthropic 裸 name + input_schema；
//   - "responses"：OpenAI Responses 顶层 type/name/parameters；
//   - 其余（chat/completions）：嵌入文件的嵌套 function 形状。
func agentManifestTools(subpath string) []map[string]any {
	out := make([]map[string]any, 0, len(agentManifestChat))
	for _, t := range agentManifestChat {
		fn, _ := t["function"].(map[string]any)
		switch subpath {
		case "messages":
			tool := map[string]any{
				"name":         fn["name"],
				"input_schema": fn["parameters"],
			}
			if d, _ := fn["description"].(string); d != "" {
				tool["description"] = d
			}
			out = append(out, tool)
		case "responses":
			tool := map[string]any{
				"type":       "function",
				"name":       fn["name"],
				"parameters": fn["parameters"],
			}
			if d, _ := fn["description"].(string); d != "" {
				tool["description"] = d
			}
			out = append(out, tool)
		default:
			out = append(out, cloneToolMap(t))
		}
	}
	return out
}

// cloneToolMap 深拷贝一层工具定义（manifest 只含 map/切片/标量，json 往返
// 最省事且与嵌入文件语义一致）。
func cloneToolMap(t map[string]any) map[string]any {
	b, err := json.Marshal(t)
	if err != nil {
		return t
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return t
	}
	return out
}

// manifestToolNames 返回 manifest 工具名集合（小写，与协议形状无关）。
func manifestToolNames() map[string]bool {
	names := make(map[string]bool, len(agentManifestChat))
	for _, t := range agentManifestChat {
		if fn, ok := t["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				names[name] = true
			}
		}
	}
	return names
}

// ======================== 请求侧：preamble 前置 ========================

// combinePreamble 把 harness preamble 前置到调用方 system 文本。已以 marker
// 开头的文本（OpenCode 自己或串联代理）原样返回，避免双重 preamble。
func combinePreamble(systemText string) string {
	preamble := strings.TrimRight(agentPreambleText, "\n")
	if preamble == "" {
		return systemText
	}
	if strings.HasPrefix(strings.TrimLeft(systemText, " \t\r\n"), agentPreambleMarker) {
		return systemText
	}
	if strings.TrimSpace(systemText) == "" {
		return preamble
	}
	return preamble + "\n\n" + systemText
}

// messageTextContentOf 提取一条 chat 消息的纯文本（string 或 text parts）。
func messageTextContentOf(m map[string]any) string {
	switch c := m["content"].(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			t, _ := pm["text"].(string)
			if t == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(t)
		}
		return b.String()
	}
	return ""
}

// injectPreambleChat 把 preamble 前置到首条 system 消息（与调用方原文合并为
// 单条，门禁按单条 system 文本校验）；没有 system 消息时在头部插入一条。
func injectPreambleChat(bodyMap map[string]any) {
	msgs, _ := bodyMap["messages"].([]any)
	for i, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok || m["role"] != "system" {
			continue
		}
		combined := combinePreamble(messageTextContentOf(m))
		if combined == messageTextContentOf(m) {
			return
		}
		m["content"] = combined
		msgs[i] = m
		return
	}
	preamble := strings.TrimRight(agentPreambleText, "\n")
	if preamble == "" {
		return
	}
	sys := map[string]any{"role": "system", "content": preamble}
	bodyMap["messages"] = append([]any{sys}, msgs...)
}

// injectPreambleMessages 把 preamble 前置到 Anthropic system（string 或 text
// parts 数组都折叠为单条字符串：门禁按单条 system 文本校验）。
func injectPreambleMessages(bodyMap map[string]any) {
	bodyMap["system"] = combinePreamble(anthropicSystemTextOf(bodyMap["system"]))
}

// anthropicSystemTextOf 把 Anthropic system 字段（string 或 text parts）折叠
// 为纯文本。
func anthropicSystemTextOf(system any) string {
	switch s := system.(type) {
	case string:
		return s
	case []any:
		var b strings.Builder
		for _, p := range s {
			switch v := p.(type) {
			case string:
				if v == "" {
					continue
				}
				if b.Len() > 0 {
					b.WriteString("\n\n")
				}
				b.WriteString(v)
			case map[string]any:
				t, _ := v["text"].(string)
				if t == "" {
					continue
				}
				if b.Len() > 0 {
					b.WriteString("\n\n")
				}
				b.WriteString(t)
			}
		}
		return b.String()
	}
	return ""
}

// injectPreambleResponses 把 preamble 前置到 Responses instructions。
func injectPreambleResponses(bodyMap map[string]any) {
	instr, _ := bodyMap["instructions"].(string)
	bodyMap["instructions"] = combinePreamble(instr)
}

// ======================== 请求侧：manifest 前置 ========================

// applyAgentManifest 把 manifest 前置到 tools，并丢弃与 manifest 同名的调用方
// 工具（门禁要的是 CLI 自己的 schema；回复侧按映射把 manifest 调用改回调用方
// 工具名）。返回调用方是否自带工具：无工具的请求（标题生成、普通问答）被强制
// tool_choice=none——manifest 只为过门禁，答案必须是文本。
//
// 名字按协议形状读取（chat 的 function.name、messages/responses 的裸 name），
// 冲突判定大小写敏感（Read 与 read 不冲突，对齐 zen-proxy）。
func applyAgentManifest(bodyMap map[string]any, subpath string) (callerHasTools bool) {
	manifest := agentManifestTools(subpath)
	if len(manifest) == 0 {
		_, callerHasTools = bodyMap["tools"].([]any)
		return callerHasTools && len(bodyMap["tools"].([]any)) > 0
	}
	manifestNames := manifestToolNames()
	rawTools, _ := bodyMap["tools"].([]any)
	caller := make([]any, 0, len(rawTools))
	for _, t := range rawTools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		name := freeTierToolNameOf(tm)
		if name != "" && manifestNames[name] {
			continue
		}
		caller = append(caller, t)
		callerHasTools = true
	}
	merged := make([]any, 0, len(manifest)+len(caller))
	for _, t := range manifest {
		merged = append(merged, t)
	}
	merged = append(merged, caller...)
	bodyMap["tools"] = merged
	if !callerHasTools {
		// 无工具调用方：manifest 只为过门禁，答案必须是文本。
		// /responses 只接受 "auto"，故删除；messages 用 Anthropic 对象形。
		switch subpath {
		case "responses":
			delete(bodyMap, "tool_choice")
		case "messages":
			bodyMap["tool_choice"] = map[string]any{"type": "none"}
		default:
			bodyMap["tool_choice"] = "none"
		}
	}
	return callerHasTools
}

// applyFreeTierBodyFingerprint 对免费模型请求执行请求体指纹重做：
//   - tools：manifest 前置，同名调用方工具丢弃，无工具调用方强制 tool_choice；
//   - system：harness preamble 前置到首条 system/instructions（已是 preamble
//     开头的文本不动）；
//   - stream：一律强制 true（上游免费层只接受流式）；
//   - stream_options：chat/responses 保留调用方自定义键、补 include_usage；
//     messages 子路径不写（不属于 Anthropic schema）。
//
// count_tokens / systemone 等非三协议子路径不套该门禁。
func applyFreeTierBodyFingerprint(bodyMap map[string]any, subpath, modelID string) {
	if bodyMap == nil || !isFreeModel(modelID) {
		return
	}
	switch subpath {
	case "chat/completions", "messages", "responses":
	default:
		return
	}
	applyAgentManifest(bodyMap, subpath)
	switch subpath {
	case "messages":
		injectPreambleMessages(bodyMap)
	case "responses":
		injectPreambleResponses(bodyMap)
	default:
		injectPreambleChat(bodyMap)
	}
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
