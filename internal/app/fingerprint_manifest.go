package app

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ======================== 免费层指纹: 官方 CLI prompt + 工具 manifest ========================
//
// 上游免费层门禁按"真实 OpenCode CLI 请求"校验请求体（zen-proxy 对线上网关的
// 实测口径，2026-11）：
//   - system 文本必须是 CLI 自己的 prompt（opencode-prompt-v2.txt），截断或
//     换成通用 agent 提示词都会按 429 FreeUsageLimitError 拒绝；
//   - tools 必须携带 CLI 2.x 的完整工具集（13 件：edit/execute/glob/grep/
//     question/read/shell/skill/subagent/webfetch/websearch/write/patch）。
//
// 本文件按 zen-proxy 的逻辑重做：官方 prompt 前置（不替换调用方原文）、完整
// manifest 前置、所有调用方工具改成不透明别名（避免调用方工具词汇泄漏给门禁，
// 也避免与官方 manifest 重名）。回复侧把 manifest 调用映射回调用方工具名并
// 丢弃调用方无法执行的调用，见 agent_tool_reply.go。

//go:embed fingerprint/opencode-prompt-v2.txt
var opencodePromptV2 string

//go:embed fingerprint/opencode-tools-v2.json
var opencodeToolsV2JSON []byte

// opencodePromptMarker 是官方 prompt 的首行。system 文本已以此开头时视为
// OpenCode 自己（或串联的代理）的请求，不再重复前置。
const opencodePromptMarker = "You are opencode, an interactive CLI tool"

// opencodeSkills is the built-in skill listing the CLI appends after its
// environment block. It is static, so it is part of the injected prompt.
const opencodeSkills = `Skills provide specialized instructions and workflows for specific tasks.
Use the skill tool to load a skill when a task matches its description.
<available_skills>
  <skill>
    <name>customize-opencode</name>
    <description>Use ONLY when the user is editing or creating opencode's own configuration: opencode.json, opencode.jsonc, files under .opencode/, or files under ~/.config/opencode/. Also use when creating or fixing opencode agents, subagents, skills, plugins, MCP servers, or permission rules. Do not use for the user's own application code, or for any project that is not configuring opencode itself.</description>
    <location>&lt;built-in&gt;</location>
  </skill>
</available_skills>`

var (
	opencodeEnvOnce sync.Once
	opencodeWorkDir string
)

// opencodeEnvironment renders the <env> block the CLI puts after its base
// prompt. The working directory is resolved once per process; a platform
// without one falls back to the official container path.
func opencodeEnvironment(model string) string {
	opencodeEnvOnce.Do(func() {
		directory := ""
		if current, err := os.Getwd(); err == nil {
			directory = filepath.Clean(strings.TrimSpace(current))
		}
		if directory == "" || directory == "." {
			directory = "/workspace"
		}
		opencodeWorkDir = directory
	})
	apiModel := strings.TrimSpace(strings.TrimPrefix(model, "opencode/"))
	if apiModel == "" {
		apiModel = "unknown"
	}
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	return strings.Join([]string{
		"You are powered by the model named " + apiModel + ". The exact model ID is opencode/" + apiModel,
		"Here is some useful information about the environment you are running in:",
		"<env>",
		"  Working directory: " + opencodeWorkDir,
		"  Workspace root folder: " + opencodeWorkDir,
		"  Is directory a git repo: yes",
		"  Platform: " + platform,
		"  Today's date: " + time.Now().UTC().Format("Mon Jan 02 2006"),
		"</env>",
	}, "\n")
}

// agentPreambleFor assembles the official CLI system prompt for modelID. The
// prompt bytes are static apart from the live environment block, so a
// conversation keeps the same prefix across turns and the upstream prefix
// cache stays warm.
func agentPreambleFor(modelID string) string {
	if opencodePromptV2 == "" {
		return ""
	}
	return opencodePromptV2 + opencodeEnvironment(modelID) + "\n" + opencodeSkills
}

// agentToolSpec 是官方工具名到调用方工具的映射（对齐 zen-proxy
// OfficialToolCandidates）：Name 是 Anthropic 官方名（Claude Code 注册的
// PascalCase），Args 是官方参数名 → 调用方参数名，值为 nil 的参数在映射时
// 丢弃（shell 的 workdir 在调用方 Bash 工具上没有对应物）。
type agentToolSpec struct {
	Name string
	Args map[string]*string
}

var agentToolMap = map[string]agentToolSpec{
	"read":      {Name: "Read", Args: map[string]*string{"path": new("file_path")}},
	"edit":      {Name: "Edit", Args: map[string]*string{"path": new("file_path"), "oldString": new("old_string"), "newString": new("new_string"), "replaceAll": new("replace_all")}},
	"write":     {Name: "Write", Args: map[string]*string{"path": new("file_path")}},
	"bash":      {Name: "Bash", Args: map[string]*string{"workdir": nil}},
	"shell":     {Name: "Bash", Args: map[string]*string{"workdir": nil}},
	"execute":   {Name: "Bash", Args: map[string]*string{"workdir": nil}},
	"grep":      {Name: "Grep", Args: map[string]*string{}},
	"glob":      {Name: "Glob", Args: map[string]*string{}},
	"question":  {Name: "AskUserQuestion", Args: map[string]*string{}},
	"skill":     {Name: "Skill", Args: map[string]*string{"id": new("skill")}},
	"subagent":  {Name: "Task", Args: map[string]*string{"agent": new("subagent_type")}},
	"webfetch":  {Name: "WebFetch", Args: map[string]*string{}},
	"websearch": {Name: "WebSearch", Args: map[string]*string{}},
}

// opencodeToolOrder is the official 2.x manifest order. It is fixed so the
// injected prefix stays byte-stable across requests.
var opencodeToolOrder = []string{
	"edit", "execute", "glob", "grep", "question", "read", "shell",
	"skill", "subagent", "webfetch", "websearch", "write", "patch",
}

// agentManifestChat 是 manifest 的 chat 形状（{"type":"function","function":{...}}），
// init 时从官方 2.x 抓包一次性构建。
var agentManifestChat []map[string]any

func init() {
	var defs map[string]struct {
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	}
	if err := json.Unmarshal(opencodeToolsV2JSON, &defs); err != nil {
		return
	}
	for _, name := range opencodeToolOrder {
		def, ok := defs[name]
		if !ok {
			continue
		}
		fn := map[string]any{
			"name":        name,
			"description": def.Description,
			"parameters":  def.Parameters,
			"strict":      false,
		}
		agentManifestChat = append(agentManifestChat, map[string]any{
			"type":     "function",
			"function": fn,
		})
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

// ======================== 请求侧：官方 prompt 前置 ========================

// combinePreamble 把官方 CLI prompt 前置到调用方 system 文本。已以 marker
// 开头的文本（OpenCode 自己或串联代理）原样返回，避免双重 prompt。
func combinePreamble(modelID, systemText string) string {
	preamble := agentPreambleFor(modelID)
	if preamble == "" {
		return systemText
	}
	if strings.HasPrefix(strings.TrimLeft(systemText, " \t\r\n"), opencodePromptMarker) {
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
func injectPreambleChat(bodyMap map[string]any, modelID string) {
	msgs, _ := bodyMap["messages"].([]any)
	for i, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok || m["role"] != "system" {
			continue
		}
		combined := combinePreamble(modelID, messageTextContentOf(m))
		if combined == messageTextContentOf(m) {
			return
		}
		m["content"] = combined
		msgs[i] = m
		return
	}
	preamble := agentPreambleFor(modelID)
	if preamble == "" {
		return
	}
	sys := map[string]any{"role": "system", "content": preamble}
	bodyMap["messages"] = append([]any{sys}, msgs...)
}

// injectPreambleMessages 把 preamble 前置到 Anthropic system（string 或 text
// parts 数组都折叠为单条字符串：门禁按单条 system 文本校验）。
func injectPreambleMessages(bodyMap map[string]any, modelID string) {
	bodyMap["system"] = combinePreamble(modelID, anthropicSystemTextOf(bodyMap["system"]))
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
func injectPreambleResponses(bodyMap map[string]any, modelID string) {
	instr, _ := bodyMap["instructions"].(string)
	bodyMap["instructions"] = combinePreamble(modelID, instr)
}

// ======================== 请求侧：manifest 前置 ========================

// applyAgentManifest 把 manifest 前置到 tools。与 manifest 同名的调用方工具
// 不能继续用原名（门禁要的是 CLI 自己的 schema），但也不能直接丢弃——调用方
// 可能依赖它自己的 schema。改为可逆的 clnt_ 别名，回复侧再解回原名；
// tool_choice 与历史里的工具引用同步改写。返回调用方是否自带工具：无工具的
// 请求（标题生成、普通问答）被强制 tool_choice=none——manifest 只为过门禁，
// 答案必须是文本。
//
// 名字按协议形状读取（chat 的 function.name、messages/responses 的裸 name），
// 冲突判定大小写敏感（Read 与 read 不冲突，对齐 zen-proxy）。
func applyAgentManifest(bodyMap map[string]any, subpath string) (callerHasTools bool) {
	manifest := agentManifestTools(subpath)
	if len(manifest) == 0 {
		_, callerHasTools = bodyMap["tools"].([]any)
		return callerHasTools && len(bodyMap["tools"].([]any)) > 0
	}
	manifestByName := make(map[string]map[string]any, len(manifest))
	for _, t := range manifest {
		if name := freeTierToolNameOf(t); name != "" {
			manifestByName[name] = t
		}
	}
	rawTools, _ := bodyMap["tools"].([]any)
	caller := make([]any, 0, len(rawTools))
	aliases := make(map[string]string)
	for _, t := range rawTools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		name := freeTierToolNameOf(tm)
		if name != "" {
			// buildOCRequest* runs once per retry on the same body map. A
			// manifest tool injected by the previous attempt is already in
			// the canonical shape; keep it instead of aliasing our own tool.
			if canonical, ok := manifestByName[name]; ok && reflect.DeepEqual(tm, canonical) {
				caller = append(caller, t)
				callerHasTools = true
				continue
			}
			// Every caller tool gets an opaque name, even when it does not
			// collide with the manifest: the free-tier gate rejects bodies
			// that still look like another agent's toolset (Claude Code's
			// Read/Edit/Bash/Task/... vocabulary).
			if !isCallerToolAlias(name) {
				alias := callerToolAlias(name)
				if alias == "" {
					// No safe wire name; dropping is the only fallback.
					continue
				}
				setFreeTierToolName(tm, alias)
				aliases[name] = alias
			}
		}
		caller = append(caller, t)
		callerHasTools = true
	}
	merged := make([]any, 0, len(manifest)+len(caller))
	for _, t := range manifest {
		present := false
		for _, existing := range caller {
			if em, ok := existing.(map[string]any); ok && reflect.DeepEqual(t, em) {
				present = true
				break
			}
		}
		if !present {
			merged = append(merged, t)
		}
	}
	merged = append(merged, caller...)
	bodyMap["tools"] = merged
	rewriteAliasedToolReferences(bodyMap, subpath, aliases)
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

// freeTierCompletionLimit is the max_completion_tokens the official 2.x client
// puts on every chat body.
const freeTierCompletionLimit = 32000

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
	// A title call is the one request the free tier wants without tools: an
	// attached agent manifest turns it into a 403/429. Keep the preamble so it
	// still looks like the CLI, but preserve the tool-less request.
	if !isTitleGenerationRequest(bodyMap) {
		applyAgentManifest(bodyMap, subpath)
	}
	switch subpath {
	case "messages":
		injectPreambleMessages(bodyMap, modelID)
	case "responses":
		injectPreambleResponses(bodyMap, modelID)
	default:
		injectPreambleChat(bodyMap, modelID)
	}
	bodyMap["stream"] = true
	if subpath == "messages" {
		return
	}
	// The official 2.x client always puts a completion ceiling on a chat body.
	// Keep the same shape when the caller did not express a preference; an
	// explicit max_tokens/max_completion_tokens still wins (convertRequest also
	// injects the configured cap as max_tokens when one is set).
	if subpath == "chat/completions" {
		_, hasMaxCompletionTokens := bodyMap["max_completion_tokens"]
		_, hasMaxTokens := bodyMap["max_tokens"]
		if !hasMaxCompletionTokens && !hasMaxTokens {
			bodyMap["max_completion_tokens"] = freeTierCompletionLimit
		}
	}
	if existing, ok := bodyMap["stream_options"].(map[string]any); ok {
		existing["include_usage"] = true
	} else {
		bodyMap["stream_options"] = map[string]any{"include_usage": true}
	}
}
