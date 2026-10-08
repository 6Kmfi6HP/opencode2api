package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/6Kmfi6HP/opencode2api/internal/config"
	"github.com/6Kmfi6HP/opencode2api/internal/logging"
	statsx "github.com/6Kmfi6HP/opencode2api/internal/stats"
)

// ======================== /v1/chat/completions → Anthropic 上游 ========================

// defaultAnthropicMaxTokens 是 Chat 入站缺省 max_tokens 时的兜底值。
// Anthropic Messages 上游把 max_tokens 视为必填。
const defaultAnthropicMaxTokens = 8192

// effortToThinkingBudget 把 reasoning_effort 映射为 Anthropic thinking 预算。
func effortToThinkingBudget(effort string) int {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal", "low":
		return 1024
	case "medium":
		return 4096
	case "high":
		return 10240
	case "xhigh", "max":
		return 32768
	default:
		return 0
	}
}

// chatToolChoiceToAnthropic 把 Chat tool_choice 转为 Anthropic 形状。
// 返回 nil 时调用方不应写入 tool_choice 键。allowed_tools 形状不在这里转换
// ——它需要按声明收窄工具列表，由 chatToAnthropicBodyWithRaw 经
// chatAllowedToolsChoice + filterToolsByAllowedNames 处理。
func chatToolChoiceToAnthropic(choice any) any {
	switch v := choice.(type) {
	case string:
		switch v {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required":
			return map[string]any{"type": "any"}
		case "none":
			return map[string]any{"type": "none"}
		}
	case map[string]any:
		// 兼容省略 type 的 {function:{name}} 形状（保持既有宽松解析）。
		if fn, ok := v["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		}
		switch t, _ := v["type"].(string); t {
		case "auto", "any":
			// any 为 Anthropic 原生形状，直接透传。
			return map[string]any{"type": t}
		case "required":
			return map[string]any{"type": "any"}
		case "none":
			return map[string]any{"type": "none"}
		case "custom":
			// Anthropic 无 custom 形状；auto 最接近（对齐 Bifrost）。
			return map[string]any{"type": "auto"}
		}
	}
	return nil
}

// chatAllowedToolsChoice 解析 allowed_tools 形状的 tool_choice：
// {"type":"allowed_tools","mode":"auto|any|required","tools":[{"type":"function",
// "function":{"name":...}}]}。返回允许的工具名集合与 mode；第三值报告该
// choice 是否为 allowed_tools 形状（其余形状由 chatToolChoiceToAnthropic 处理）。
// 解析不出任何工具名的 allowed_tools 视为空交集——调用方收窄为 none，
// 绝不静默放行全量工具（对齐 Bifrost filterToolsByAllowed 的失败语义）。
func chatAllowedToolsChoice(choice any) (permitted map[string]struct{}, mode string, ok bool) {
	m, isMap := choice.(map[string]any)
	if !isMap {
		return nil, "", false
	}
	if t, _ := m["type"].(string); t != "allowed_tools" {
		return nil, "", false
	}
	permitted = make(map[string]struct{})
	if list, isList := m["tools"].([]any); isList {
		for _, item := range list {
			tm, isEntry := item.(map[string]any)
			if !isEntry {
				continue
			}
			name := ""
			if fn, isFn := tm["function"].(map[string]any); isFn {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				// 宽松兼容把名字直接放在条目上的形状。
				name, _ = tm["name"].(string)
			}
			if name != "" {
				permitted[name] = struct{}{}
			}
		}
	}
	mode, _ = m["mode"].(string)
	return permitted, mode, true
}

// filterToolsByAllowedNames 按允许名单收窄已声明的 function 工具列表（对齐
// Bifrost filterToolsByAllowed）：Anthropic 没有「从子集中选」的 tool_choice
// 形状，限制只能靠移除声明来表达——模型只能调用它见到的工具。空交集时返回
// 空列表，调用方以 tool_choice none 表达「无工具可调」，绝不静默放行全量。
func filterToolsByAllowedNames(tools []Tool, permitted map[string]struct{}) []Tool {
	if len(tools) == 0 || len(permitted) == 0 {
		return nil
	}
	kept := make([]Tool, 0, len(tools))
	for _, t := range tools {
		if _, ok := permitted[t.Function.Name]; ok {
			kept = append(kept, t)
		}
	}
	return kept
}

// applyChatParallelToolUse 把 Chat parallel_tool_calls:false 携带为 Anthropic
// tool_choice 的 disable_parallel_tool_use:true（对齐 Bifrost
// applyParallelToolUse——同一指令在 Anthropic 上布尔语义反转，且只在确有
// 工具可能被调用时才有载体）：
//   - parallel_tool_calls 缺省或 true：Anthropic 默认即并行，不发；
//   - 未声明工具：无工具可调，跳过；
//   - tool_choice 为 none：跳过；
//   - 无 tool_choice 且已声明工具：补 auto 载体（auto 是 Anthropic 默认行为，
//     只补开关不改可调用集合）；
//   - 其余 tool_choice：在既有载体上加 disable_parallel_tool_use:true。
func applyChatParallelToolUse(body map[string]any, declaredTools int, req *OpenAIRequest) {
	parallel := false
	switch v := extraBodyValue(req, "parallel_tool_calls").(type) {
	case bool:
		parallel = v
	case *bool:
		if v == nil {
			return
		}
		parallel = *v
	default:
		return
	}
	if parallel || declaredTools == 0 {
		return
	}
	tc, _ := body["tool_choice"].(map[string]any)
	if tc == nil {
		body["tool_choice"] = map[string]any{
			"type": "auto", "disable_parallel_tool_use": true,
		}
		return
	}
	if t, _ := tc["type"].(string); t == "none" {
		return
	}
	tc["disable_parallel_tool_use"] = true
}

// chatTextToAnthropicContent 把字符串或 Chat 多模态 content 数组转为
// Anthropic content block 数组。图片按 data URI / URL 归类为 base64/url
// source（URL 经 scheme 白名单放行）；file/document part 映射为 document 块
// （对齐 Bifrost ConvertToAnthropicDocumentBlock）；part 上客户端逐块携带的
// cache_control 断点透传（Anthropic 接受块级 breakpoint）。无法表达的 part
// 丢弃（记 debug 日志）：
//   - 图片全部无法转换时回填 [image attached] 文本；
//   - 消息含 document 块但无可用 text 块时补 "." 占位（Anthropic 要求
//     document 消息必须携带 text 块,对齐 Bifrost documentPlaceholderText）。
//
// 保证多模态消息永远有内容（Anthropic 不接受空 content 数组）。
func chatTextToAnthropicContent(content any) ([]map[string]any, bool) {
	switch v := content.(type) {
	case string:
		if v == "" {
			return nil, false
		}
		return []map[string]any{{"type": "text", "text": v}}, true
	case []any:
		var blocks []map[string]any
		droppedImage := false
		droppedDocument := false
		for _, part := range v {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch pm["type"] {
			case "text":
				if t, _ := pm["text"].(string); t != "" {
					blocks = append(blocks, chatTextBlockWithCacheControl(t, pm))
				}
			case "image_url":
				url, _ := pm["url"].(string)
				if url == "" {
					if iu, ok := pm["image_url"].(map[string]any); ok {
						url, _ = iu["url"].(string)
					}
				}
				if url == "" {
					slog.Debug("chat→anthropic: image_url part without url skipped")
					droppedImage = true
					continue
				}
				if block := imageURLToAnthropicBlock(url); block != nil {
					if cc := cacheControlFromPart(pm); cc != nil {
						block["cache_control"] = cc
					}
					blocks = append(blocks, block)
				} else {
					droppedImage = true
				}
			case "file", "document":
				if block := chatFilePartToAnthropicDocumentBlock(pm); block != nil {
					if cc := cacheControlFromPart(pm); cc != nil {
						block["cache_control"] = cc
					}
					blocks = append(blocks, block)
				} else {
					slog.Debug("chat→anthropic: file/document part without representable source dropped")
					droppedDocument = true
				}
			default:
				slog.Debug("chat→anthropic: unsupported content part skipped", "part_type", pm["type"])
			}
		}
		// Anthropic 要求 document 消息必须携带非空白 text 块（"A text block
		// must be included when using documents"）：缺 text 时补 "." 占位
		// （对齐 Bifrost documentPlaceholderText,防止整条消息被上游拒绝）。
		if hasAnthropicDocumentBlock(blocks) && !hasUsableTextBlock(blocks) {
			blocks = append([]map[string]any{{"type": "text", "text": documentPlaceholderText}}, blocks...)
		}
		if len(blocks) == 0 && (droppedImage || droppedDocument) {
			// 多模态消息的图片/文档全部无法转换时保留一个占位文本,避免用户
			// 消息内容整体消失（对齐 text-only 降级的 multimodalAttachedLabel）。
			if droppedDocument && !droppedImage {
				blocks = append(blocks, map[string]any{"type": "text", "text": documentPlaceholderText})
			} else {
				blocks = append(blocks, map[string]any{"type": "text", "text": multimodalAttachedLabel})
			}
		}
		return blocks, len(blocks) > 0
	default:
		return nil, false
	}
}

// documentPlaceholderText 前置给只含 document 块但无 text 块的消息（对齐
// Bifrost documentPlaceholderText）。Anthropic 要求 document 消息必须携带
// text 块且拒绝空白 text 块,占位符必须非空白。
const documentPlaceholderText = "."

// systemReminderEnvelope 把对话中间出现的 system 文本包进 <system-reminder>
// 信封（对齐 Bifrost inlineMidConversationSystem 的 wrap 形状）。
func systemReminderEnvelope(text string) string {
	return "<system-reminder>\n" + text + "\n</system-reminder>\n"
}

// hasAnthropicDocumentBlock 报告块序列里是否含 document 块（对齐 Bifrost
// hasAnthropicDocumentBlock——该消息需要伴随 text 块）。
func hasAnthropicDocumentBlock(blocks []map[string]any) bool {
	for _, b := range blocks {
		if t, _ := b["type"].(string); t == "document" {
			return true
		}
	}
	return false
}

// hasUsableTextBlock 报告块序列里是否含非空白 text 块（空白 text 上游单独
// 拒绝："text content blocks must contain non-whitespace text"）。
func hasUsableTextBlock(blocks []map[string]any) bool {
	for _, b := range blocks {
		if t, _ := b["type"].(string); t == "text" {
			if text, _ := b["text"].(string); strings.TrimSpace(text) != "" {
				return true
			}
		}
	}
	return false
}

// chatTextBlockWithCacheControl 构造 text block 并透传客户端逐块携带的
// cache_control 断点（Anthropic 风格客户端扩展；无 cache_control 时保持
// 既有两键形状）。
func chatTextBlockWithCacheControl(text string, part map[string]any) map[string]any {
	block := map[string]any{"type": "text", "text": text}
	if cc := cacheControlFromPart(part); cc != nil {
		block["cache_control"] = cc
	}
	return block
}

// cacheControlFromPart 提取内容 part 上的 cache_control 断点（map 形状原样
// 透传；无/畸形返回 nil）。
func cacheControlFromPart(part map[string]any) map[string]any {
	cc, ok := part["cache_control"].(map[string]any)
	if !ok || len(cc) == 0 {
		return nil
	}
	return cc
}

// chatFilePartToAnthropicDocumentBlock 把 Chat file/document part 映射为
// Anthropic document 块（对齐 Bifrost ConvertToAnthropicDocumentBlock）。
// 接受两种入站形状（Bifrost rewriteDocumentBlock 归一化的两端）：
//   - {"type":"file","file":{file_id|file_url|file_data,file_type,filename}}
//     （OpenAI chat wire,Bifrost ChatInputFile 同名键）
//   - {"type":"document","source":{type:base64|url|text|file,...},"title":...}
//     （Anthropic 风格）
//
// citations / title 透传；无法解析出任何 source 载荷时返回 nil（调用方丢弃
// 并计数,消息级 "." 占位兜底,防止 doc-only 消息整条消失）。
func chatFilePartToAnthropicDocumentBlock(part map[string]any) map[string]any {
	file, _ := part["file"].(map[string]any)
	source, hasSource := part["source"].(map[string]any)
	if file == nil && !hasSource {
		return nil
	}
	str := func(m map[string]any, key string) string {
		if m == nil {
			return ""
		}
		s, _ := m[key].(string)
		return strings.TrimSpace(s)
	}
	doc := map[string]any{"type": "document"}
	if title := str(file, "filename"); title != "" {
		doc["title"] = title
	} else if title := str(part, "title"); title != "" {
		doc["title"] = title
	}
	if citations, ok := part["citations"]; ok && citations != nil {
		doc["citations"] = citations
	}
	src := map[string]any{}
	switch {
	case str(file, "file_id") != "":
		// 上传文件引用（Anthropic files API 的 file_id）。
		src["type"] = "file"
		src["file_id"] = str(file, "file_id")
	case str(file, "file_url") != "":
		src["type"] = "url"
		src["url"] = str(file, "file_url")
	case str(file, "file_data") != "":
		s := documentSourceFromFileData(str(file, "file_data"), str(file, "file_type"))
		if s == nil {
			return nil
		}
		src = s
	case hasSource:
		switch t, _ := source["type"].(string); t {
		case "file":
			if id := str(source, "file_id"); id != "" {
				src["type"] = "file"
				src["file_id"] = id
			} else {
				return nil
			}
		case "url":
			if u := str(source, "url"); u != "" {
				src["type"] = "url"
				src["url"] = u
			} else {
				return nil
			}
		case "text":
			if data := str(source, "data"); data != "" {
				src["type"] = "text"
				src["data"] = data
				if mt := str(source, "media_type"); mt != "" {
					src["media_type"] = mt
				} else {
					src["media_type"] = "text/plain"
				}
			} else {
				return nil
			}
		case "base64":
			if data := str(source, "data"); data != "" {
				src["type"] = "base64"
				src["data"] = data
				if mt := str(source, "media_type"); mt != "" {
					src["media_type"] = mt
				} else {
					src["media_type"] = "application/pdf"
				}
			} else {
				return nil
			}
		default:
			// source.type=content（Anthropic 内联内容数组）等无法表达。
			return nil
		}
	default:
		return nil
	}
	doc["source"] = src
	return doc
}

// documentSourceFromFileData 把 file_data 解析为 document source（对齐 Bifrost
// ConvertToAnthropicDocumentBlock 的 file_data 分支）：
//   - 显式编码的 text 文档 data URL（data:text/*;base64,...）→ text source
//   - 纯文本（非 data: 且 file_type 为 text/plain|txt）→ text source
//   - 其余 data: URL → base64 source（media_type 取 data URL 元或 file_type）
//   - 二进制 → base64 source（media_type 取 file_type,缺省 application/pdf）
//
// 无法表达（畸形 data URI）时返回 nil。
func documentSourceFromFileData(data, fileType string) map[string]any {
	if source := inlineTextDocumentSource(data); source != nil {
		return source
	}
	if !strings.HasPrefix(data, "data:") {
		if fileType == "text/plain" || fileType == "txt" {
			return map[string]any{"type": "text", "media_type": "text/plain", "data": data}
		}
		return map[string]any{
			"type": "base64", "data": data, "media_type": defaultDocumentMediaType(fileType),
		}
	}
	meta, payload, ok := strings.Cut(strings.TrimPrefix(data, "data:"), ";base64,")
	if !ok {
		return nil
	}
	mediaType := meta
	if mediaType == "" {
		mediaType = defaultDocumentMediaType(fileType)
	}
	return map[string]any{"type": "base64", "data": payload, "media_type": mediaType}
}

// defaultDocumentMediaType document base64 source 的 media type 兜底（对齐
// Bifrost：file_type 缺省 application/pdf）。
func defaultDocumentMediaType(fileType string) string {
	if fileType == "" {
		return "application/pdf"
	}
	return fileType
}

// inlineTextDocumentSource 解码显式编码的 text 文档 data URL（对齐 Bifrost
// inlineTextDataURL）：data:<text/*|application/json>[;…];base64,<b64> →
// {type:"text", media_type:"text/plain", data:<decoded>}。其余形状返回 nil。
func inlineTextDocumentSource(data string) map[string]any {
	header, encoded, ok := strings.Cut(data, ",")
	if !ok || !strings.HasPrefix(header, "data:") || !strings.HasSuffix(header, ";base64") {
		return nil
	}
	media := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")))
	if i := strings.IndexByte(media, ';'); i >= 0 {
		media = media[:i]
	}
	if !isTextDocumentMediaType(media) {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || !utf8.Valid(decoded) {
		return nil
	}
	return map[string]any{"type": "text", "media_type": "text/plain", "data": string(decoded)}
}

// isTextDocumentMediaType 识别 Anthropic 接受为 text source 的文本文档格式
// （对齐 Bifrost isTextDocumentMediaType）。
func isTextDocumentMediaType(media string) bool {
	return strings.HasPrefix(media, "text/") || media == "application/json" ||
		strings.HasPrefix(media, "application/") && strings.HasSuffix(media, "+json")
}

// imageURLToAnthropicBlock 把 data URI 或白名单 scheme URL 转为 Anthropic
// image block（清洗经 sanitizeImageURLForUpstream）。base64 data URI 归
// base64 source；非 base64 data URI（如 svg 明文载荷）与 http(s) URL 归
// url source 透传（对齐 Bifrost ExtractURLTypeInfo 的 URL 归类——非 base64
// data URI 此前被直接丢弃）。畸形 data URI / 白名单外 scheme 丢弃该 part
// （返回 nil）并日志。钝 media 类型（无 "/"）回退 image/png。
func imageURLToAnthropicBlock(url string) map[string]any {
	sanitized, ok := sanitizeImageURLForUpstream(url)
	if !ok {
		trimmed := strings.TrimSpace(url)
		if len(trimmed) > 24 {
			trimmed = trimmed[:24]
		}
		slog.Debug("chat→anthropic: image url rejected by sanitize", "url_prefix", trimmed)
		return nil
	}
	if after, ok := strings.CutPrefix(sanitized, "data:"); ok {
		if meta, b64, ok := strings.Cut(after, ";base64,"); ok {
			mediaType := meta
			media := strings.TrimPrefix(meta, "image/")
			if mediaType == "" || media == "" || media == mediaType {
				// 钝 media 类型（如 data:png;base64 或空）视为 png。
				media = "png"
				mediaType = "image/png"
			}
			return map[string]any{
				"type": "image",
				"source": map[string]any{
					"type": "base64", "media_type": mediaType, "data": b64,
				},
			}
		}
	}
	// 非 base64 data URI 与常规 URL 都按 url source 透传。
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type": "url", "url": sanitized,
		},
	}
}

// allowedImageURLSchemes 是图片 URL 的白名单 scheme（对齐 Bifrost
// SanitizeImageURL defaultImageURLSchemes）。白名单外的 scheme（file:// 等）
// 上游无法取用,丢弃。
var allowedImageURLSchemes = map[string]struct{}{"http": {}, "https": {}}

// sanitizeImageURLForUpstream 清洗图片 URL（对齐 Bifrost SanitizeImageURL /
// ExtractURLTypeInfo）：
//   - data URI：base64 编码或带 media type 的非 base64 载荷均放行（非 base64
//     data URI 此前被直接丢弃,现归 url source 透传）；无 media type 的畸形
//     data URI 拒绝；
//   - 裸 base64 图片数据：包成 data URI（media type 按签名嗅探）；
//   - 其余 URL：scheme 必须命中白名单（http/https,大小写不敏感）且有 host。
//
// 第二返回值报告是否可用；不可用时调用方丢弃该 part。
func sanitizeImageURLForUpstream(rawURL string) (string, bool) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", false
	}
	if strings.HasPrefix(trimmed, "data:") {
		meta, payload, ok := strings.Cut(strings.TrimPrefix(trimmed, "data:"), ",")
		if !ok || payload == "" || strings.TrimSpace(strings.Split(meta, ";")[0]) == "" {
			return "", false
		}
		return trimmed, true
	}
	if isLikelyBase64Image(trimmed) {
		clean := stripBase64Whitespace(trimmed)
		return "data:" + detectImageTypeFromBase64(clean) + ";base64," + clean, true
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", false
	}
	if _, ok := allowedImageURLSchemes[strings.ToLower(parsed.Scheme)]; !ok {
		return "", false
	}
	return parsed.String(), true
}

// detectImageTypeFromBase64 从 base64 头部识别图片类型（对齐 Bifrost
// detectImageTypeFromBase64 的签名表,未知回退 jpeg）。
func detectImageTypeFromBase64(b64 string) string {
	clean := stripBase64Whitespace(b64)
	switch {
	case strings.HasPrefix(clean, "/9j/") || strings.HasPrefix(clean, "/9k/"):
		return "image/jpeg"
	case strings.HasPrefix(clean, "iVBORw0KGgo"):
		return "image/png"
	case strings.HasPrefix(clean, "R0lGOD"):
		return "image/gif"
	case strings.HasPrefix(clean, "Qk"):
		return "image/bmp"
	case strings.HasPrefix(clean, "UklGR") && len(clean) >= 16 && clean[12:16] == "V0VC":
		return "image/webp"
	case strings.HasPrefix(clean, "PHN2Zy") || strings.HasPrefix(clean, "PD94bW"):
		return "image/svg+xml"
	default:
		return "image/jpeg"
	}
}

// stripBase64Whitespace 去掉 base64 数据里的空白与换行（上游 base64 解码
// 拒绝内嵌换行）。
func stripBase64Whitespace(s string) string {
	if !strings.ContainsAny(s, " \t\r\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t', '\r', '\n':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isLikelyBase64Image 粗判裸 base64 图片数据（对齐 Bifrost isLikelyBase64,
// 加最小长度护栏避免把普通单词误判成 base64）。
func isLikelyBase64Image(s string) bool {
	clean := stripBase64Whitespace(s)
	if len(clean) < 32 {
		return false
	}
	for _, r := range clean {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '+', r == '/', r == '=':
		default:
			return false
		}
	}
	return true
}

// chatMessagesToAnthropic 把 Chat messages 转为 Anthropic system + messages。
// 首条 user/assistant 轮之前的 system/developer 提取为顶层 system（多条
// "\n\n" 连接）；对话中间出现的 system/developer 原地转为 <system-reminder>
// 包裹的 user 轮（对齐 Bifrost inlineMidConversationSystem——顶层 system 在
// 每条消息之前渲染,中途增长会使其后的缓存前缀整体失效,锚点必须留在
// messages 里）；role=tool 转为 tool_result user 消息；assistant tool_calls
// 内联为 tool_use block。全部输入只有 system/developer 时降级为单个 user
// 消息（对齐 Bifrost chat.go:985-992,Anthropic 不接受 messages:[]）。
// 返回的 messages 已做连续同角色合并（Anthropic 要求交替）。
func chatMessagesToAnthropic(messages []Message, restoreCase bool) (string, []map[string]any) {
	var systemParts []string
	var out []map[string]any
	// seenConversation 跟踪是否已处理过 user/assistant 轮：其后的
	// system/developer 属于对话中段,原地内联而非再提升到顶层 system。
	seenConversation := false
	appendBlocks := func(role string, blocks []map[string]any) {
		if len(blocks) == 0 {
			return
		}
		// 连续同角色合并:Anthropic 不允许相邻同 role,且合并 tool_use 是
		// 合法的(同 turn 可含 text+tool_use 序列)。
		// 注:真正的非法情况是 assistant[tool_use] 之后无 user[tool_result]
		// 直接又出现 assistant —— 那种序列本身在 chat 输入里就不规范,
		// 上游会以 tool_use_without_result 拒绝,这里不做单独修复(超出
		// merge 的职责)。
		if n := len(out); n > 0 {
			if prevRole, _ := out[n-1]["role"].(string); prevRole == role {
				prev, _ := out[n-1]["content"].([]map[string]any)
				// tool_result 前插:紧跟 assistant tool_use 的 user 消息必须以
				// tool_result 块开头（Anthropic 校验 "Did not find N
				// `tool_result` block(s) at the beginning of this message"）。
				// 前插保持 [tool_result..., 先前文本] 形状——中段 system 内联
				// 的 <system-reminder> 文本不被合并提到 tool_result 之前。
				if role == "user" && hasToolResultBlock(blocks) {
					merged := make([]map[string]any, 0, len(prev)+len(blocks))
					merged = append(merged, blocks...)
					merged = append(merged, prev...)
					out[n-1]["content"] = merged
					return
				}
				out[n-1]["content"] = append(prev, blocks...)
				return
			}
		}
		out = append(out, map[string]any{"role": role, "content": blocks})
	}
	for _, msg := range messages {
		switch msg.Role {
		case "system", "developer":
			// parts 数组只提取 text 分片进 system（保持现状;system 文本无需
			// 携带多模态内容,图片分片有意丢弃）。
			var texts []string
			var lastCC map[string]any
			if blocks, ok := chatTextToAnthropicContent(msg.Content); ok {
				for _, b := range blocks {
					if t, _ := b["text"].(string); t != "" {
						texts = append(texts, t)
						if cc, _ := b["cache_control"].(map[string]any); cc != nil {
							lastCC = cc
						}
					}
				}
			}
			if len(texts) == 0 {
				continue
			}
			if seenConversation {
				// 对话中段 system：原地转 <system-reminder> 包裹的 user 轮,
				// 缓存锚点留在 messages 里（对齐 Bifrost
				// inlineMidConversationSystem——块内 breakpoint 折叠到最后一块,
				// 不烧多余的缓存检查点）。
				var blocks []map[string]any
				for _, t := range texts {
					blocks = append(blocks, map[string]any{"type": "text", "text": systemReminderEnvelope(t)})
				}
				if lastCC != nil {
					blocks[len(blocks)-1]["cache_control"] = lastCC
				}
				appendBlocks("user", blocks)
			} else {
				systemParts = append(systemParts, texts...)
			}
		case "assistant":
			seenConversation = true
			var blocks []map[string]any
			// 先重放推理块（对齐 Bifrost chat.go:1073-1092 与
			// leadingAnthropicReasoningBlockCount:34-44）：Anthropic 要求
			// thinking-enabled 的 assistant 轮以其 thinking/redacted_thinking
			// 块开头，块必须先于 text/tool_use。reasoning.encrypted 的 data
			// 是 Anthropic redacted_thinking 密文，原样回放供上游解密；签名
			// 缺失的 text 详情不发——unsigned thinking 块上游必拒。
			for _, d := range msg.ReasoningDetails {
				switch d.Type {
				case "reasoning.encrypted":
					if d.Data != "" {
						blocks = append(blocks, map[string]any{"type": "redacted_thinking", "data": d.Data})
					}
				case "reasoning.text":
					if d.Text != "" && d.Signature != "" {
						blocks = append(blocks, map[string]any{
							"type": "thinking", "thinking": d.Text, "signature": d.Signature,
						})
					}
				}
			}
			if bs, ok := chatTextToAnthropicContent(msg.Content); ok {
				blocks = append(blocks, bs...)
			}
			for _, tc := range msg.ToolCalls {
				if tc.ID == "" {
					// 空 id 的 tool call 无法与任何 tool_result 配对（Anthropic
					// 要求 tool_use id 非空），照发必 400——跳过该调用。
					slog.Debug("chat→anthropic: tool call without id skipped", "name", tc.Function.Name)
					continue
				}
				input := parseToolCallArguments(tc.Function.Arguments)
				toolName := tc.Function.Name
				if restoreCase {
					toolName = restoreToolNameCase(toolName)
				}
				blocks = append(blocks, map[string]any{
					// 清洗 id（同一 ID 在 tool_result 侧清洗到相同值才能配对）。
					"type": "tool_use", "id": sanitizeToolUseID(tc.ID), "name": toolName, "input": input,
				})
			}
			appendBlocks("assistant", blocks)
		case "tool":
			seenConversation = true
			if msg.ToolCallID == "" {
				// 孤儿 tool_result：空 tool_use_id 照发必 400。降级为普通
				// user 文本/图片 block 保留上下文（对齐 claude_responses.go
				// 的缺 ID 降级与 Bifrost Responses 路径）。
				appendBlocks("user", chatToolResultFallbackBlocks(msg))
				continue
			}
			appendBlocks("user", []map[string]any{chatToolResultBlock(msg)})
		case "user", "":
			seenConversation = true
			if blocks, ok := chatTextToAnthropicContent(msg.Content); ok {
				appendBlocks("user", blocks)
			}
		}
	}
	if len(out) == 0 && len(systemParts) > 0 {
		// 全-system（无任何 user/assistant 轮）：降级为单个 user 消息,
		// 不编出 messages:[]（对齐 Bifrost chat.go:985-992）。
		out = append(out, map[string]any{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": strings.Join(systemParts, "\n\n")},
			},
		})
		systemParts = nil
	}
	return strings.Join(systemParts, "\n\n"), out
}

// hasToolResultBlock 报告 blocks 里是否含 tool_result 块（tool_result 前插
// 合并的判定条件）。
func hasToolResultBlock(blocks []map[string]any) bool {
	for _, b := range blocks {
		if t, _ := b["type"].(string); t == "tool_result" {
			return true
		}
	}
	return false
}

// chatToolResultBlock 构造 role=tool 消息的 tool_result block。字符串 content
// 保持现状写为单个 text block；[]any parts 逐 part 映射进 content blocks
// （text→text、image_url→image，未知 part 跳过并记日志）；part 级
// cache_control 断点提升到 tool_result 块本身（首个命中提升,其余折叠）——
// Anthropic 拒绝嵌在 tool_result.content 里的块级断点（"cache_control may
// not be specified within `tool_result.content`. Instead, place it directly
// on `tool_result`",对齐 Bifrost chat.go:1021-1029）,嵌在 content 内照发必
// 400 invalid_request_error;空内容写 "(empty)"
// （Anthropic 不接受空 tool_result content）。
func chatToolResultBlock(msg Message) map[string]any {
	content := []map[string]any{}
	// part 级断点提升位:首个命中写到 tool_result 块本身,嵌套副本不落。
	var hoistedCC map[string]any
	switch c := msg.Content.(type) {
	case string:
		if c == "" {
			c = "(empty)"
		}
		content = append(content, map[string]any{"type": "text", "text": c})
	case []any:
		for _, part := range c {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch pm["type"] {
			case "text":
				block := map[string]any{"type": "text", "text": toString(pm["text"])}
				if cc := cacheControlFromPart(pm); cc != nil {
					if hoistedCC == nil {
						// 首个断点提升到 tool_result 块本身,嵌套块不落
						// （Anthropic 拒绝 tool_result.content 内嵌断点）。
						hoistedCC = cc
					}
				}
				content = append(content, block)
			case "image_url":
				url, _ := pm["url"].(string)
				if url == "" {
					if iu, ok := pm["image_url"].(map[string]any); ok {
						url, _ = iu["url"].(string)
					}
				}
				if url == "" {
					slog.Debug("chat→anthropic tool_result: image part without url skipped", "tool_use_id", msg.ToolCallID)
					continue
				}
				if block := imageURLToAnthropicBlock(url); block != nil {
					if cc := cacheControlFromPart(pm); cc != nil && hoistedCC == nil {
						// 同 text:image 块上的断点提升,嵌套副本清掉。
						hoistedCC = cc
					}
					content = append(content, block)
				}
			default:
				slog.Debug("chat→anthropic tool_result: unsupported part skipped", "part_type", pm["type"], "tool_use_id", msg.ToolCallID)
			}
		}
		if len(content) == 0 {
			// parts 全部无法映射（或本来就是空数组）：回填占位文本。
			content = append(content, map[string]any{"type": "text", "text": "(empty)"})
		}
	default:
		// 数值/对象等非标准 content：序列化为文本，保持现状（不丢数据）。
		if msg.Content != nil {
			text := ""
			if b, err := json.Marshal(msg.Content); err == nil {
				text = string(b)
			}
			content = append(content, map[string]any{"type": "text", "text": text})
		} else {
			content = append(content, map[string]any{"type": "text", "text": "(empty)"})
		}
	}
	blocks := map[string]any{
		// 清洗 id（与 assistant.tool_calls 侧同一确定性函数，保证配对）。
		"type": "tool_result", "tool_use_id": sanitizeToolUseID(msg.ToolCallID),
		"content": content,
	}
	if hoistedCC != nil {
		// 提升的 part 级断点落在 tool_result 块上（客户端仍拿到一个断点,
		// 位置改为上游接受的那层）。
		blocks["cache_control"] = hoistedCC
	}
	if msg.IsError != nil && *msg.IsError {
		// 错误语义用 is_error:true 表达（对齐 Bifrost chat.go 的 tool_result
		// 映射），不再把 "Error: " 前缀烤进内容文本。
		blocks["is_error"] = true
	}
	return blocks
}

// chatToolResultFallbackBlocks 把孤儿（空 tool_use_id）role=tool 消息的内容
// 降级为普通 user text/image block——保留上下文，不作为 tool_result 照发
// （Anthropic 对空 tool_use_id 必 400）。part 级 cache_control 断点保留:
// 普通块接受块级断点,折叠到最后一块（对齐 inlineMidConversationSystem 的
// 折叠规则,不烧多余检查点）。
func chatToolResultFallbackBlocks(msg Message) []map[string]any {
	tr := chatToolResultBlock(msg)
	inner, ok := tr["content"].([]map[string]any)
	if !ok || len(inner) == 0 {
		return []map[string]any{{"type": "text", "text": "(empty)"}}
	}
	if cc, _ := tr["cache_control"].(map[string]any); cc != nil {
		inner[len(inner)-1]["cache_control"] = cc
	}
	return inner
}

// parseToolCallArguments 把 Chat 工具调用 arguments JSON 解析为 Anthropic
// tool_use input。合法 JSON 对象以紧凑 json.RawMessage 透传——保留原始键序
// 与数字形态，避免 map[string]any 往返重排键序破坏上游前缀缓存（对齐
// Bifrost parseJSONInput / compactJSONBytes）。空补 {}。坏 JSON 或非对象形态
// 无法构成 Anthropic 要求的 input 对象，兜底 {"_raw": <原文>} 保留数据
// （透传原始字节会让整个请求体序列化失败，比包装更糟）。
func parseToolCallArguments(args string) any {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return json.RawMessage("{}")
	}
	if strings.HasPrefix(trimmed, "{") {
		var buf bytes.Buffer
		if err := json.Compact(&buf, []byte(trimmed)); err == nil {
			return json.RawMessage(buf.Bytes())
		}
	}
	return map[string]any{"_raw": args}
}

// resolveMaxTokens 统一 Chat→Anthropic / Chat→Responses 两个方向的 max tokens
// 口径：max_completion_tokens 优先于 max_tokens，两个键都可以从 typed
// OpenAIRequest（MaxTokens 字段）顶层或 ExtraBody / 原始请求体 map 顶层拿到
// （OpenAIRequest 没有 max_completion_tokens 字段，调用方须先 json.Unmarshal
// 入站 body 传入 rawBody）。均未设置（或 <=0）时回退 MaxTokensCapFor(modelID)，
// 仍无 cap 则兜底 8192。最终结果钳制到 [128, cap]（cap>0 且 <128 时以 cap 为准，
// 不再强制下限 —— 配置者显式限满时须尊重）。
func resolveMaxTokens(rawBody map[string]any, req *OpenAIRequest, modelID string) int {
	value := 0
	if rawBody != nil {
		if v, ok := intFromAny(rawBody["max_completion_tokens"]); ok && v > 0 {
			value = v
		}
	}
	if value <= 0 && req != nil {
		if v, ok := intFromAny(extraBodyValue(req, "max_completion_tokens")); ok && v > 0 {
			value = v
		}
	}
	if value <= 0 && rawBody != nil {
		if v, ok := intFromAny(rawBody["max_tokens"]); ok && v > 0 {
			value = v
		}
	}
	if value <= 0 && req != nil && req.MaxTokens != nil && *req.MaxTokens > 0 {
		value = *req.MaxTokens
	}
	tokenCap := 0
	if modelID != "" {
		tokenCap = config.MaxTokensCapFor(modelID)
	}
	if value <= 0 {
		if tokenCap > 0 {
			value = tokenCap
		} else {
			value = defaultAnthropicMaxTokens
		}
	}
	if tokenCap > 0 && value > tokenCap {
		value = tokenCap
	}
	if value < 128 && !(tokenCap > 0 && tokenCap < 128) {
		value = 128
	}
	return value
}

// intFromAny 宽松地把 JSON 数字 / int 值转为 int。
func intFromAny(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case int32:
		return int(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
		return 0, false
	default:
		return 0, false
	}
}

// chatToAnthropicBody 把 Chat Completions 请求转为 Anthropic Messages 请求体。
// rawBody 可选：传入调用方的原始请求体 map,resolveMaxTokens 用它读
// max_completion_tokens（类型化 OpenAIRequest 装不下的顶层字段）。
func chatToAnthropicBody(req *OpenAIRequest, modelID string, restoreCase bool) []byte {
	return chatToAnthropicBodyWithRaw(req, modelID, nil, restoreCase)
}

func chatToAnthropicBodyWithRaw(req *OpenAIRequest, modelID string, rawBody map[string]any, restoreCase bool) []byte {
	system, messages := chatMessagesToAnthropic(req.Messages, restoreCase)
	body := map[string]any{
		"model":    modelID,
		"messages": messages,
		"stream":   req.Stream,
	}
	if system != "" {
		body["system"] = system
	}
	maxTokens := resolveMaxTokens(rawBody, req, modelID)
	body["max_tokens"] = maxTokens
	// thinking 与采样参数互斥(对齐 sub2api 与本项目 convertClaudeRequest
	// anthropic_protocol.go:119-144):thinking 生效(将写入 thinking 键)时剥离
	// temperature/top_p,避免上游 Anthropic 400。判定先行,写入分支据此跳过。
	// thinking 对象按机型构造(对齐 Bifrost chat.go:771-873):恒发
	// enabled+budget 会被 adaptive-only 机型 400——见 anthropicThinkingForUpstream。
	thinkingObj, thinkingEffortOut := anthropicThinkingForUpstream(modelID, req)
	// 采样参数两道门(对齐 Bifrost chat.go:424-435):
	//   - thinking 生效时剥离(Anthropic thinking 请求拒绝 sampling);
	//   - adaptive-only 机型(Opus 4.7+/Sonnet 5+/Fable 系)无条件剥离——
	//     这些机型拒绝 temperature/top_p/top_k(400),与 thinking 决策无关:
	//     无 reasoning effort 请求下 thinkingObj 落 nil,门必须仍挂机型,
	//     否则温度原样透传整条请求被拒。
	// 非受限机型的写入分支:temperature/top_p 同时携带收成单参(Anthropic
	// 不允许两者同发,优先 temperature)。
	if thinkingObj == nil && !adaptiveOnlyThinkingModel(modelID) {
		if req.Temperature != nil {
			body["temperature"] = *req.Temperature
		} else if req.TopP != nil {
			body["top_p"] = *req.TopP
		}
	}
	if stop := extraBodyValue(req, "stop"); stop != nil {
		if arr, ok := stop.([]any); ok {
			var strs []string
			for _, v := range arr {
				if s, ok := v.(string); ok {
					strs = append(strs, s)
				}
			}
			if len(strs) > 0 {
				body["stop_sequences"] = strs
			}
		} else if s, ok := stop.(string); ok && s != "" {
			body["stop_sequences"] = []string{s}
		}
	}
	// allowed_tools 形状的 tool_choice：按声明收窄工具列表（对齐 Bifrost
	// filterToolsByAllowed），空交集时收窄为「无工具可调」，绝不静默放行
	// 全量工具。
	permitted, allowedMode, isAllowedTools := chatAllowedToolsChoice(req.ToolChoice)
	declaredTools := 0
	if len(req.Tools) > 0 {
		declared := req.Tools
		if isAllowedTools {
			declared = filterToolsByAllowedNames(req.Tools, permitted)
		}
		if len(declared) > 0 {
			tools := make([]map[string]any, 0, len(declared))
			for _, t := range declared {
				schema := t.Function.Parameters
				if schema == nil {
					schema = map[string]any{"type": "object", "properties": map[string]any{}}
				}
				tool := map[string]any{
					"name":         t.Function.Name,
					"input_schema": schema,
				}
				if t.Function.Description != "" {
					tool["description"] = t.Function.Description
				}
				tools = append(tools, tool)
			}
			body["tools"] = tools
			declaredTools = len(tools)
		}
	}
	if isAllowedTools {
		// Anthropic 没有「从子集中选」的 tool_choice 形状：限制以收窄后的
		// 声明列表表达（对齐 Bifrost）。空交集时 "any"/"auto" 挂在空列表上
		// 是上游必拒的请求，按字面语义落 none；mode=required → any；否则 auto。
		switch {
		case declaredTools == 0:
			body["tool_choice"] = map[string]any{"type": "none"}
		case allowedMode == "required":
			body["tool_choice"] = map[string]any{"type": "any"}
		default:
			body["tool_choice"] = map[string]any{"type": "auto"}
		}
	} else if req.ToolChoice != nil {
		if choice := chatToolChoiceToAnthropic(req.ToolChoice); choice != nil {
			body["tool_choice"] = choice
		}
	}
	applyChatParallelToolUse(body, declaredTools, req)
	// thinking 写入:对象已在上方按机型构造(thinkingObj),与采样参数剥离
	// 用同一判定,避免两处推导不一致。effort 落 output_config.effort
	// (adaptive-only 机型上 budget_tokens 不可用,effort 是唯一预算旋钮;
	// 对齐 Bifrost setEffortOnOutputConfig)。
	if thinkingObj != nil {
		body["thinking"] = thinkingObj
	}
	if thinkingEffortOut != "" {
		body["output_config"] = map[string]any{"effort": thinkingEffortOut}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return []byte(fmt.Sprintf(`{"model":%q,"messages":[],"max_tokens":%d,"stream":%t}`, modelID, maxTokens, req.Stream))
	}
	return b
}

// extraBodyValue 取 ExtraBody 中的顶层字段。
func extraBodyValue(req *OpenAIRequest, key string) any {
	if req.ExtraBody == nil {
		return nil
	}
	return req.ExtraBody[key]
}

// rawRequestBodyMap 从已解析的 OpenAIRequest 重建入站 body 的顶层键视图，
// 供 resolveMaxTokens 读取 max_completion_tokens 等类型化结构装不下的字段。
// 注：理想形态是解析调用方持有的原始 body 字节（chat.go 的
// readJSONRequestBody 读后未把字节留存在请求上下文里,改它超出本文件边界）,
// 这里用 ExtraBody+标准字段重建等价视图 —— ExtraBody 内的顶层键天然就位,
// req.MaxTokens 回填为 max_tokens。
func rawRequestBodyMap(req *OpenAIRequest) map[string]any {
	raw := map[string]any{}
	if req == nil {
		return raw
	}
	if req.MaxTokens != nil {
		raw["max_tokens"] = *req.MaxTokens
	}
	for k, v := range req.ExtraBody {
		raw[k] = v
	}
	return raw
}

// forwardChatViaAnthropic 处理规则命中 anthropic 的 Chat 入站请求：请求转为
// Anthropic Messages，响应按客户端流式偏好转换回 Chat 形状。上游 4xx/5xx
// 经 parseAnthropicErrorBody 转为 chat 错误形状写回。
func forwardChatViaAnthropic(w http.ResponseWriter, r *http.Request, auth UpstreamAuth, req *OpenAIRequest, keepReasoning bool) {
	ctx := r.Context()
	// 请求侧归一化前置：补全 assistant.tool_calls 的 tool 响应、text-only 模型
	// 降级多模态 content 为 "[image attached]" 文本。fixToolCallGaps /
	// modelIsTextOnly / downgradeMultimodalContent 定义在 chat.go（Worker B
	// 所有），这里只调用不修改。
	req.Messages = fixToolCallGaps(req.Messages)
	textOnly := modelIsTextOnly(req.Model)
	for i := range req.Messages {
		if parts, ok := req.Messages[i].Content.([]any); ok {
			req.Messages[i].Content = downgradeMultimodalContent(parts, textOnly)
		}
	}
	rawBody := rawRequestBodyMap(req)
	// 工具名兼容：转换器输出的超长/非法字符 name 在上游边界统一缩短（转换器
	// 零改动），映射注入 ctx 供响应呈现面还原。
	restoreCase := isClaudeCodeClient(r.Header.Get("User-Agent"))
	upstreamBody, rewrites := sanitizeAnthropicUpstreamBody(chatToAnthropicBodyWithRaw(req, req.Model, rawBody, restoreCase), restoreCase)
	ctx = withAnthropicNameRewrites(ctx, rewrites)
	log := logging.FromContext(ctx)
	log.Info("chat via anthropic upstream",
		"model", req.Model, "stream", req.Stream, "keep_reasoning", keepReasoning)

	if req.Stream {
		// DriveStreamWithRetry 内部首轮调 callOnce 建立流；peek 失败（空流
		// EOF/读错/首字节看门狗）时按 StreamEmptyRetryMax 换 key 重试。
		// 用 UpstreamErrorCapture 暂存非 2xx 响应,这样 retry 全失败后能
		// 把上游真实 status+body 透回去给客户端,而不是吞掉换成通用 502。
		upstreamCap := &UpstreamErrorCapture{}
		callOnce := upstreamCap.WrapCallOnce(func(callCtx context.Context) (io.ReadCloser, int, error) {
			rc, status, _, err := callOpenCodeAnthropicEndpoint(callCtx, upstreamBody, req.Model, auth)
			return rc, status, err
		})
		runOnce := func(runCtx context.Context, rw http.ResponseWriter, rc io.Reader, peeked []streamReadResult, rd *streamReader) (bool, error) {
			return anthropicSSEToChatStream(runCtx, rw, rc, req.Model, keepReasoning, true, peeked, rd)
		}
		committed, streamErr := DriveStreamWithRetry(ctx, w, AnthropicProtocolHooks, callOnce, runOnce)
		if committed {
			return
		}
		// 全部 attempt 都未 commit：尚未向客户端写过任何字节,落 502,
		// 不伪装半截流。ctx 取消由调用方按客户端断开处理,原样透传。
		if errors.Is(streamErr, context.Canceled) || errors.Is(streamErr, context.DeadlineExceeded) {
			return
		}
		log.Warn("chat via anthropic stream empty after retries", "model", req.Model, "err", streamErr)
		// 优先回写上游真实错误(4xx/5xx 的 status+body),比通用 502 更利于调试。
		if upstreamCap.WriteUpstreamErrorTo(w) {
			return
		}
		writeUpstreamError(w, http.StatusBadGateway, fmt.Errorf("upstream stream empty after retries"), "chat")
		return
	}

	rc, status, _, err := callOpenCodeAnthropicEndpoint(ctx, upstreamBody, req.Model, auth)
	if err != nil || status < 200 || status >= 300 {
		var errBody []byte
		if rc != nil {
			errBody, _ = io.ReadAll(io.LimitReader(rc, 64*1024))
			rc.Close()
		}
		if status < 100 || status >= 600 {
			status = http.StatusBadGateway
		}
		if len(errBody) > 0 {
			if ape, ok := parseAnthropicErrorBody(errBody); ok {
				writeUpstreamError(w, status, ape, "chat")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			w.Write(errBody)
			return
		}
		writeUpstreamError(w, status, fmt.Errorf("upstream error"), "chat")
		return
	}
	defer rc.Close()

	respBody, readErr := io.ReadAll(io.LimitReader(rc, 32*1024*1024))
	if readErr != nil {
		writeUpstreamError(w, http.StatusBadGateway, fmt.Errorf("upstream read error"), "chat")
		return
	}
	// Anthropic message（或 SSE 缓冲）→ Chat，复用既有回归转换器。
	// 先还原缩短名（含 SSE 兜底），转换器天然携带客户端原始名。有意行为
	// 变化：Claude 系 UA 客户端在本路径非流式响应中也获得免费层占位名
	// 大小写还原（bash/glob/grep/read → PascalCase），与流式路径及
	// chat 上游聚合路径的既有还原契约对齐。
	respBody = restoreAnthropicResponseNames(respBody, rewrites)
	outBody, convErr := convertAnthropicToOpenAI(respBody, req.Model)
	if convErr != nil {
		writeUpstreamError(w, http.StatusBadGateway, convErr, "chat")
		return
	}
	if cleaned, err := convertResponse(outBody, keepReasoning); err == nil {
		outBody = cleaned
	}
	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			statsx.RecordChatUsage(req.Model, anthropicUsageToChat(u))
		}
	}
	result := logging.SummarizeChatResult(outBody)
	logging.LogResult(ctx, result)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(outBody)
}

// ======================== Anthropic SSE → Chat SSE 状态机 ========================

type anthropicToChatState struct {
	w             http.ResponseWriter
	flusher       http.Flusher
	stats         *logging.StreamStats
	id            string
	model         string
	keepReasoning bool
	includeUsage  bool
	sentRole      bool
	blocks        map[int]string // anthropic block index → "text"|"thinking"|"tool_use"
	toolIndices   map[int]int    // anthropic block index → chat tool_calls index
	// 每个打开中的 tool_use block 的元数据。initialInput 缓存 start 块附带的
	// 初始 input;sawInputDelta 记录是否已有 input_json_delta —— 两者互斥,
	// stop 时若 sawInputDelta=false 且 initialInput 非空,需要兜底 emit 一次,
	// 否则该 tool call 的 input 会整体丢失(上游偶发场景)。
	toolStates map[int]*anthropicToolState
	toolCount  int
	stopReason string
	// unmappableStop 标记上游发来无 chat 等价物的 stop_reason(pause_turn/
	// compaction):normalizeFinishReason 置空后 finalize 不再缺省 "stop",
	// 终块 finish_reason 编 null,不报完成态(对齐 Bifrost)。
	unmappableStop bool
	fullUsage      map[string]any
	// restoreCase 为 true(Claude 系客户端)时把免费层小写占位工具名还原
	// 为 PascalCase;其余客户端保持小写透传。
	restoreCase bool
	// rewrites 携带本请求的缩短名还原映射（anthropic 上游边界注入 ctx）；
	// nil 安全（restore 原样返回）。
	rewrites *responsesNameRewrites
	// reasoningTokens 由 thinking_delta 的字符数粗计(tokens≈字符/4)，仅当上游
	// Anthropic usage 未提供 output_tokens_details.thinking_tokens 时兜底填
	// completion_tokens_details.reasoning_tokens（上游精确值覆盖本近似）。
	reasoningTokens int
	// reasoningDetailIdxByBlock 把 anthropic block index 映射到稳定的
	// reasoning_details 序号（对齐 Bifrost reasoningDetailIndex：首个使用时
	// 分配下一个），thinking/signature/redacted 共享一个序号空间。
	reasoningDetailIdxByBlock map[int]int
	reasoningDetailCount      int
	skippedSig                int
	skippedRedacted           int
	finalized                 bool // finalize 幂等开关:message_stop 与 EOF 路径共用
}

type anthropicToolState struct {
	sawInputDelta bool
	initialInput  any // string 或 map[string]any;空/nil 表示没有
}

// 保留提供给 chat 端 arguments 的初值;在 content_block_stop 消费。
func (t *anthropicToolState) initialArguments() string {
	switch v := t.initialInput.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
		return ""
	}
}

// anthropicSSEToChatStream 把上游 Anthropic Messages SSE 翻译为 Chat SSE。
//
// 返回值的约定（与 PeekFirstFrame 配合，实现「首 token 前可重试」）：
//   - (true, nil)：已向客户端写过至少一个字节（含 finish 后的 [DONE]），上游流
//     正常翻译完毕或按已有规则合成收尾。这是唯一「已 commit」的返回。
//   - (false, err)：未向客户端写过任何字节——peek 窗口里上游给了空流/
//     错误事件/EOF/超时或 message_start 之前 EOF。调用方可以安全地关闭当前
//     rc、换 key 重发请求。
//
// peeked 非空时直接进入主循环（此调用已是某次 peek-commit 之后的干跑），
// 不再二次 peek、不再做首字节看门狗（窗口已在第一次调用里耗尽）。rd 是
// peek 主循环里续用的 reader；为 nil 时函数自己在 rc 上新建。
func anthropicSSEToChatStream(ctx context.Context, w http.ResponseWriter, rc io.Reader, model string, keepReasoning bool, includeUsage bool, peeked []streamReadResult, rd *streamReader) (bool, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	st := &anthropicToChatState{
		w:                         w,
		flusher:                   flusher,
		stats:                     &logging.StreamStats{Start: time.Now()},
		id:                        "chatcmpl-" + randomHex(12),
		model:                     model,
		keepReasoning:             keepReasoning,
		includeUsage:              includeUsage,
		blocks:                    map[int]string{},
		toolIndices:               map[int]int{},
		toolStates:                map[int]*anthropicToolState{},
		reasoningDetailIdxByBlock: map[int]int{},
		fullUsage:                 map[string]any{},
		restoreCase:               shouldRestoreToolCase(ctx),
		rewrites:                  nameRewritesFor(ctx),
	}
	defer func() {
		st.stats.ToolCallCount = st.toolCount
		if len(st.fullUsage) > 0 {
			statsx.RecordChatUsage(model, anthropicUsageToChat(st.fullUsage))
		}
		st.stats.Log(ctx, "chat")
	}()

	// WriteHeader 延迟到首字节真正写出（避免 peek 阶段占用了 200，让那次
	// 空流的调用方可安全重试而不污染客户端）。
	wroteHeader := false
	writeHeaderOnce := func() {
		if wroteHeader {
			return
		}
		w.WriteHeader(http.StatusOK)
		wroteHeader = true
	}

	var reader *streamReader
	if len(peeked) == 0 {
		peek := PeekFirstFrame(ctx, rc, time.Duration(config.StreamFirstByteTimeoutMs())*time.Millisecond, AnthropicProtocolHooks)
		if peek.Err != nil {
			return false, peek.Err
		}
		peeked = peek.Consumed
		reader = peek.Reader
		if reader == nil {
			// 上游 EOF 但已见完整帧（极少见：单帧流）。续读的 reader 直接
			// 落在已 EOF 的 rc 上,主循环立即收 EOF 并走 finalize。
			reader = newStreamReader(ctx, rc, 15*time.Second)
		} else {
			// 复用 peek 的 reader(它的 bufio 可能已预读后续行);顺手开
			// keepalive(此前为 0,看门狗由 timeout 承担)。
			reader.enableKeepalive(15 * time.Second)
		}
	} else {
		// 已有 peeked：此调用即某次 peek-commit 之后的干跑；调用方透传
		// 了自己的 reader（rd）就用，否则在 rc 上新建。
		if rd != nil {
			reader = rd
		} else {
			reader = newStreamReader(ctx, rc, 15*time.Second)
		}
	}
	defer reader.Close()

	// flushPending 按序回放 peek 阶段攒下的行（peek 阶段已按 SSE 行结构
	// 验证过帧完整性），随后转为 nil 直读上游。
	pending := append([]streamReadResult(nil), peeked...)
	consumePending := func() []streamReadResult {
		out := pending
		pending = nil
		return out
	}

	// processResult 处理一行上游 SSE（行可能为空字符串收尾一帧,err 非空
	// 表示当前行已是最后一行）。committed=true 表示已经向客户端写出过至
	// 少一字节。
	processResult := func(result streamReadResult) (committed bool, done bool, retErr error) {
		if result.line != "" {
			st.stats.NoteChunk()
			writeHeaderOnce()
			st.handleLine(result.line)
		}
		if result.err != nil {
			// EOF / 读错误兜底。
			// 关键不变量:一旦 writeHeaderOnce 触发(任何行非空就调),HTTP
			// 头已发出,**绝不能返回 (false, ...)** 否则 DriveStreamWithRetry
			// 会用同一个 ResponseWriter 二次 WriteHeader 并 retry,流被污染
			// (I4/I7)。message_start 未到但 header 已写的「伪 commit」场景:
			// 让 st.finalize() 合成 finish 终止,对客户端是干净流末尾。
			if !wroteHeader {
				// 真未 commit(message_start 都未达且一字未写)= 让 Drive retry。
				// 此分支只在 peeked 全空且首行就是 EOF 时进入。
				return false, true, errStreamIncompleteNoCommit
			}
			// 按错误类型分流(对照 claude_responses P3 / chat.go / responses.go
			// 的截断标记):干净 EOF(io.EOF)才是流自然终止,finalize 合成
			// stop+[DONE];RST/unexpected EOF/墙钟超时等传输中断不得伪造
			// clean stop——补发 in-band error 帧 + [DONE],客户端得以感知
			// 失败并重试该回合,而不是把半截 tool_use 当完整回合记入历史。
			if !errors.Is(result.err, io.EOF) {
				st.stats.SawFinish = false
				slog.Warn("chat via anthropic stream interrupted mid-stream",
					"model", model, "err", result.err)
				st.emitErrorFrame("upstream stream interrupted before completion")
				return true, true, nil
			}
			// 已写过任意字节(无论是否到 message_start) —— 收尾 finalize。
			// 已有 role 输出后再 EOF:按finalize合成 stop chunk+[DONE],
			// 保证 OpenAI SDK 不挂起（幂等）。
			st.finalize()
			if st.skippedSig > 0 || st.skippedRedacted > 0 {
				slog.Debug("chat stream: dropped unrepresentable anthropic deltas",
					"model", model, "signature_delta", st.skippedSig, "redacted_thinking", st.skippedRedacted)
			}
			return true, true, nil
		}
		return wroteHeader, false, nil
	}

	// 先回放 peeked，再进入主循环。
	for _, res := range consumePending() {
		_, done, err := processResult(res)
		if done {
			if err != nil {
				return false, err
			}
			return true, nil
		}
	}

	for {
		select {
		case <-ctx.Done():
			return wroteHeader, ctx.Err()
		case result := <-reader.Read():
			_, done, err := processResult(result)
			if done {
				if err != nil {
					return false, err
				}
				return true, nil
			}
		}
	}
}

// finalize 幂等地结束 chat 流：补发 finish chunk（stopReason 缺省 "stop"）、
// includeUsage 时的 usage 终块与 [DONE]。message_stop 正常路径与 EOF 兜底
// 路径共用。
func (st *anthropicToChatState) finalize() {
	if st.finalized {
		return
	}
	st.finalized = true
	if st.stopReason == "" && !st.unmappableStop {
		st.stopReason = "stop"
	}
	st.emitChunk(map[string]any{}, st.stopReason, nil)
	if st.includeUsage && len(st.fullUsage) > 0 {
		st.emitChunk(map[string]any{}, "", st.chatUsage())
	}
	st.w.Write([]byte("data: [DONE]\n\n"))
	if st.flusher != nil {
		st.flusher.Flush()
	}
	st.stats.DoneSeen = true
	st.stats.SawFinish = true
	st.stats.FinishReason = st.stopReason
}

// emitErrorFrame 在已 commit 的流上补发 in-band 错误帧 + [DONE],形状对照
// chat.go emitError 的 upstream_truncated(带 finish_reason=error 的 choices)。
// 传输中断时使用,取代伪造 clean stop 的 finalize——客户端拿到完整(带错的)
// 终止序列,得以感知失败并重试该回合。幂等:置 finalized 防止后续再 finalize。
func (st *anthropicToChatState) emitErrorFrame(msg string) {
	if st.finalized {
		return
	}
	st.finalized = true
	payload := map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "upstream_truncated",
		},
		"choices": []any{map[string]any{"index": 0, "finish_reason": "error"}},
	}
	data, _ := json.Marshal(payload)
	st.w.Write([]byte("data: " + string(data) + "\n\n"))
	st.w.Write([]byte("data: [DONE]\n\n"))
	if st.flusher != nil {
		st.flusher.Flush()
	}
	st.stats.DoneSeen = false
}

// chatUsage 返回发给 chat 客户端的 usage：Anthropic 侧未携带
// output_tokens_details.thinking_tokens 时,用 thinking_delta 字符计数估算
// reasoning_tokens 兜底（粗略 tokens≈chars/4,仅无精确值时启用）。
func (st *anthropicToChatState) chatUsage() map[string]any {
	usage := anthropicUsageToChat(st.fullUsage)
	if usage == nil {
		return nil
	}
	if st.reasoningTokens > 0 {
		details, _ := usage["completion_tokens_details"].(map[string]any)
		if details == nil {
			details = map[string]any{}
		}
		if v, ok := numberAsFloat(details["reasoning_tokens"]); !ok || v <= 0 {
			est := st.reasoningTokens / 4
			if est < 1 {
				est = 1
			}
			details["reasoning_tokens"] = est
		}
		if len(details) > 0 {
			usage["completion_tokens_details"] = details
		}
	}
	return usage
}

// emitReasoningDetail 写出一个 reasoning_details delta（typed 推理槽位,
// OpenRouter 形状,对齐 Bifrost anthropic→chat 流式转换）。index 按 anthropic
// block 稳定分配（对齐 Bifrost reasoningDetailIndex：首个使用时分配下一个）,
// 同一 thinking 块的文本/签名/redacted 命中同一序号,客户端按 index 聚合。
// keepReasoning=false 时抑制（与 reasoning_content 同一契约）,返回 false 供
// 调用方计数 skip。
func (st *anthropicToChatState) emitReasoningDetail(blockIdx int, detail map[string]any) bool {
	if !st.keepReasoning {
		return false
	}
	n, ok := st.reasoningDetailIdxByBlock[blockIdx]
	if !ok {
		n = st.reasoningDetailCount
		st.reasoningDetailIdxByBlock[blockIdx] = n
		st.reasoningDetailCount++
	}
	detail["index"] = n
	st.emitChunk(map[string]any{"reasoning_details": []any{detail}}, "", nil)
	return true
}

// emitChunk 写出一个 Chat SSE chunk。delta 为 nil 时用 {}；finishReason 非空时
// 附带 finish_reason；usage 非空时附带 usage（仅终块）。
func (st *anthropicToChatState) emitChunk(delta map[string]any, finishReason string, usage map[string]any) {
	chunk := map[string]any{
		"id":      st.id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   st.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReasonOr(finishReason),
		}},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	st.w.Write([]byte("data: " + string(b) + "\n\n"))
	if st.flusher != nil {
		st.flusher.Flush()
	}
}

func finishReasonOr(fr string) any {
	if fr == "" {
		return nil
	}
	return fr
}

// handleLine 处理一行上游 Anthropic SSE。
func (st *anthropicToChatState) handleLine(line string) {
	payload, ok := strings.CutPrefix(line, "data: ")
	if !ok {
		return
	}
	var evt map[string]any
	if json.Unmarshal([]byte(payload), &evt) != nil {
		return
	}
	switch typ, _ := evt["type"].(string); typ {
	case "message_start":
		if msg, ok := evt["message"].(map[string]any); ok {
			if u, ok := msg["usage"].(map[string]any); ok {
				mergeUsage(st.fullUsage, u)
			}
			if m, _ := msg["model"].(string); m != "" {
				st.model = m
			}
		}
		if !st.sentRole {
			st.sentRole = true
			st.emitChunk(map[string]any{"role": "assistant", "content": ""}, "", nil)
		}
	case "content_block_start":
		idx := numberToInt(evt["index"])
		cb, _ := evt["content_block"].(map[string]any)
		bt, _ := cb["type"].(string)
		st.blocks[idx] = bt
		if bt == "text" {
			// start 事件直接携带初始非空 text 时预 emit 一次 text_delta,
			// 否则这段起始内容会丢失（上游通常随后再发 text_delta,但
			// 携带完整文本的 content_block_start 是合法的）。
			if t, _ := cb["text"].(string); t != "" {
				st.emitChunk(map[string]any{"content": t}, "", nil)
			}
		}
		if bt == "redacted_thinking" {
			// redacted_thinking 块在 content_block_start 就完整到达（无后续
			// delta）。把密文负载作为 reasoning.encrypted detail 流出（对齐
			// Bifrost chat.go:1741-1760）：客户端下一轮回传后 chat→anthropic
			// 出站原样重放——Anthropic 拒绝 tool-use 回合丢块的最新 assistant
			// 消息,只计 skip 会让该块不可回放。
			if data, _ := cb["data"].(string); data != "" {
				if !st.emitReasoningDetail(idx, map[string]any{
					"type": "reasoning.encrypted", "data": data,
				}) {
					st.skippedRedacted++
				}
			}
		}
		if bt == "tool_use" {
			toolIdx := st.toolCount
			st.toolCount++
			st.toolIndices[idx] = toolIdx
			tool := &anthropicToolState{}
			st.toolStates[idx] = tool
			// 上游 tool_use name 还原为客户端原始名（缩短名经 rewrites
			// 映射；免费层小写占位名仅对 Claude 系客户端还原为规范大小写
			// (bash/glob/grep/read -> Bash/Glob/Grep/Read),其余保持透传。
			rawName, _ := cb["name"].(string)
			name := st.rewrites.restore(rawName)
			id, _ := cb["id"].(string)
			// 缓存 start 块的 initial input(常见 {});不要立刻 emit 给 chat 端
			// —— OpenAI 客户端会 concat 所有 arguments 片段,若 start 下发了
			// initial,后续 partial_json 会拼出非法 JSON。stop 时兜底 emit:
			// !sawInputDelta 时优先 initial,initial 为空则补 "{}"。
			if raw, ok := cb["input"]; ok && raw != nil {
				if s, ok := raw.(string); ok && s != "" && s != "{}" {
					tool.initialInput = s
				} else if m, ok := raw.(map[string]any); ok && len(m) > 0 {
					tool.initialInput = m
				}
			}
			st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": toolIdx, "id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": ""},
			}}}, "", nil)
		}
	case "content_block_delta":
		idx := numberToInt(evt["index"])
		d, _ := evt["delta"].(map[string]any)
		dt, _ := d["type"].(string)
		switch dt {
		case "text_delta":
			if t, _ := d["text"].(string); t != "" {
				st.emitChunk(map[string]any{"content": t}, "", nil)
			}
		case "thinking_delta":
			if t, _ := d["thinking"].(string); t != "" {
				// 粗略计数 reasoning tokens(字符/4),仅在上游 usage 缺
				// thinking_tokens 时作 completion_tokens_details 兜底。
				st.reasoningTokens += len(t)
				if st.keepReasoning {
					st.emitChunk(map[string]any{"reasoning_content": t}, "", nil)
					// 思考文本同时经 reasoning_details typed 通道流出（对齐
					// Bifrost chat.go:1850-1866,thinking_delta 携带
					// ReasoningDetails{text}:客户端按 index 聚合后,下一轮
					// assistant 历史才能重放 thinking+signature 配对块
					// （thinking-head 要求）,thinking-enabled 的 tool-use 回合
					// 不因丢块被上游 400 拒绝。
					st.emitReasoningDetail(idx, map[string]any{
						"type": "reasoning.text", "text": t,
					})
				}
			}
		case "input_json_delta":
			if toolIdx, ok := st.toolIndices[idx]; ok {
				if tool, ok := st.toolStates[idx]; ok {
					tool.sawInputDelta = true
				}
				if pj, _ := d["partial_json"].(string); pj != "" {
					st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
						"index": toolIdx, "id": nil, "type": "function",
						"function": map[string]any{"name": "", "arguments": pj},
					}}}, "", nil)
				}
			}
		case "signature_delta":
			// 签名只在同协议 roundtrip 有意义,不拼进 reasoning_content；作为
			// reasoning.text detail 的 signature 流出（对齐 Bifrost chat.go
			// 1868-1890）,客户端回传历史后 chat→anthropic 出站据此重放
			// thinking+signature 块,满足 thinking-head 要求。
			if sig, _ := d["signature"].(string); sig != "" {
				if !st.emitReasoningDetail(idx, map[string]any{
					"type": "reasoning.text", "signature": sig,
				}) {
					st.skippedSig++
				}
			}
		case "redacted_thinking":
			if data, _ := d["data"].(string); data != "" {
				if !st.emitReasoningDetail(idx, map[string]any{
					"type": "reasoning.encrypted", "data": data,
				}) {
					st.skippedRedacted++
				}
			} else {
				st.skippedRedacted++
			}
		}
	case "content_block_stop":
		idx := numberToInt(evt["index"])
		// 若 tool_use 全程没收到 input_json_delta,在 stop 时兜底 emit 一次:
		// 有 initial input 用 initial,否则补 "{}"。OpenAI 客户端 concat 各
		// chunk 的 arguments 后须得到合法 JSON —— 空串会让 json.Unmarshal
		// 失败;"{}" 对齐 buildOpenAIResponse 对 input nil/{} 的非流式语义。
		if tool, ok := st.toolStates[idx]; ok && !tool.sawInputDelta {
			if toolIdx, ok := st.toolIndices[idx]; ok {
				s := tool.initialArguments()
				if s == "" {
					s = "{}"
				}
				st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
					"index": toolIdx, "id": nil, "type": "function",
					"function": map[string]any{"name": "", "arguments": s},
				}}}, "", nil)
			}
		}
		delete(st.blocks, idx)
		delete(st.toolIndices, idx)
		delete(st.toolStates, idx)
	case "message_delta":
		if delta, ok := evt["delta"].(map[string]any); ok {
			if sr, _ := delta["stop_reason"].(string); sr != "" {
				st.stopReason = normalizeFinishReason(sr)
				if st.stopReason == "" {
					// pause_turn/compaction 无 chat 等价物:置空不报完成态。
					st.unmappableStop = true
				}
			}
		}
		if u, ok := evt["usage"].(map[string]any); ok {
			mergeUsage(st.fullUsage, u)
		}
	case "message_stop":
		// 正常终止路径与 EOF 兜底共用 finalize（幂等）。
		st.finalize()
	case "error":
		em, _ := evt["error"].(map[string]any)
		msg := "upstream error"
		if m, _ := em["message"].(string); m != "" {
			msg = m
		}
		st.w.Write([]byte("data: " + `{"error":{"message":` + jsonString(msg) + `}}` + "\n\n"))
		if st.flusher != nil {
			st.flusher.Flush()
		}
	}
}

// numberToInt 宽松地把 any 数字转为 int（SSE JSON 解码后多为 float64）。
func numberToInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}

// jsonString 由 count_tokens.go 提供（Worker D 所有），这里复用。
