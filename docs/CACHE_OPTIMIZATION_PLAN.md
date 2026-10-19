# 免费层指纹缓存优化方案

## 问题诊断

当前 `ensureFreeTierTools` 采用"逐项补齐缺失工具"策略，导致不同客户端发给上游的 `tools` 数组内容各异：

- 客户端 A 带 `[Read]` → 代理补成 `[Read, bash, glob, grep, read]`（6项）
- 客户端 B 带 `[Bash, Read, custom_tool]` → 代理补成 `[Bash, Read, custom_tool, bash, glob, grep, read]`（7项）
- Cherry Studio 不带工具 → 代理补成 `[bash, glob, grep, read]`（4项）

上游 prompt caching 基于请求体精确匹配，tools 数组任何差异都产生新缓存键。

## 可选方案

### 方案 A：完全统一追加（无条件）

**逻辑**：无视客户端是否已有四件工具，一律追加小写 bash/glob/grep/read。

**优点**：所有免费层请求的 tools 数组尾部完全一致。

**缺点**：可能导致工具名重复（客户端带 `Bash`，代理又追加 `bash`，虽然大小写不同但语义重复）。

### 方案 B：标准化 + 补齐

**逻辑**：
1. 检测客户端 tools 中是否有四件工具的任何大小写变体
2. 有 → 就地改写为小写版本（替换 name，保留 description/parameters）
3. 无 → 追加标准小写版本

**优点**：避免重复，保留客户端自定义描述。

**缺点**：
- 改写客户端原有工具顺序和定义
- 客户端带部分工具时，tools 数组长度仍不统一（如只带 Read 的补 3 个，带 Bash+Read 的补 2 个）

### 方案 C：移除 + 统一追加（推荐）

**逻辑**：
1. 从客户端 tools 中**移除**所有四件工具的任何大小写变体（bash/Bash/BASH, glob/Glob, grep/Grep, read/Read）
2. 保留客户端的自定义工具（如 `custom_tool`, `weather` 等）
3. 统一在**尾部**追加标准的小写四件：`[bash, glob, grep, read]`

**最终形态**：
- 客户端 A 带 `[Read]` → `[bash, glob, grep, read]`
- 客户端 B 带 `[Bash, Read, custom_tool]` → `[custom_tool, bash, glob, grep, read]`
- Cherry Studio 不带工具 → `[bash, glob, grep, read]`

**优点**：
- 最彻底的统一：所有免费层请求的 tools 数组**要么**是纯四件，**要么**是 `[客户端自定义工具...] + [bash, glob, grep, read]`
- 缓存分叉减少到"有无自定义工具"二分类，而非"缺哪几件"的组合爆炸
- 符合上游门禁口径（L17-18 注释："描述与参数结构可任意，仅需名字命中"）

**缺点**：
- 客户端精心定义的四件工具描述会被丢弃（但根据实测，上游只看名字）
- 需验证模型是否依赖客户端自定义的参数 schema（理论上不应该，因为 tool_use 只看 name）

### 方案 D：完全标准化（激进）

**逻辑**：移除客户端**所有**工具，统一追加标准四件。

**优点**：缓存命中率 100%（所有免费层请求完全一致）。

**缺点**：客户端自定义工具（如 `weather`, `browser` 等业务工具）也被移除，**破坏性过大，不可行**。

## 推荐方案：方案 C

**理由**：
1. 平衡缓存收益与兼容性：保留客户端业务工具，只统一四件占位工具
2. 符合上游实测口径：门禁只看工具名，不看描述和参数定义
3. 实现简单：在现有 `ensureFreeTierTools` 基础上增加一个"预清理"步骤

## 实现细节

### 代码改动（opencode_fingerprint.go）

```go
// normalizeFourToolsForCache 从 tools 数组中移除四件占位工具的所有大小写变体，
// 保留客户端自定义工具，然后统一在尾部追加标准小写四件。最大化缓存命中率。
func normalizeFourToolsForCache(tools []any, subpath string) []any {
    fourToolNames := map[string]bool{
        "bash": true, "glob": true, "grep": true, "read": true,
        "Bash": true, "Glob": true, "Grep": true, "Read": true,
    }
    
    // 第一步：移除四件工具的所有变体，保留自定义工具
    filtered := make([]any, 0, len(tools))
    for _, t := range tools {
        tm, ok := t.(map[string]any)
        if !ok {
            continue
        }
        name := freeTierToolNameOf(tm)
        if !fourToolNames[name] {
            filtered = append(filtered, t)
        }
    }
    
    // 第二步：统一追加标准小写四件
    var standardFour []map[string]any
    switch subpath {
    case "messages":
        standardFour = freeTierRequiredToolsAnthropic
    case "responses":
        standardFour = freeTierRequiredToolsResponses
    default: // chat/completions
        standardFour = freeTierRequiredTools
    }
    
    for _, tool := range standardFour {
        filtered = append(filtered, tool)
    }
    
    return filtered
}

// ensureFreeTierTools 修改为调用 normalizeFourToolsForCache
func ensureFreeTierTools(bodyMap map[string]any, subpath string) {
    if bodyMap == nil {
        return
    }
    rawTools, _ := bodyMap["tools"].([]any)
    bodyMap["tools"] = normalizeFourToolsForCache(rawTools, subpath)
}
```

### 行为变化对比

| 场景 | 旧行为 | 新行为 |
|------|--------|--------|
| Claude Code 带 `[Bash, Read]` | `[Bash, Read, bash, glob, grep, read]` | `[bash, glob, grep, read]` |
| Cherry Studio 不带工具 | `[bash, glob, grep, read]` | `[bash, glob, grep, read]` |
| 客户端带 `[custom_tool]` | `[custom_tool, bash, glob, grep, read]` | `[custom_tool, bash, glob, grep, read]` |
| 客户端带 `[Bash, custom_tool]` | `[Bash, custom_tool, bash, glob, grep, read]` | `[custom_tool, bash, glob, grep, read]` |

**关键统一点**：
- 所有"只用四件工具"的客户端 → 完全相同的 tools 数组 → **同一缓存键**
- 所有"四件 + 同一组自定义工具"的客户端 → 完全相同的 tools 数组 → **同一缓存键**

## 风险评估

### 高风险场景

**客户端依赖自定义的工具参数 schema**：如果某客户端为 `Bash` 工具定义了扩展参数（如 `timeout`, `env`），移除后模型可能无法正确发起包含这些参数的 tool_call。

**缓解措施**：
1. 根据 L17-18 注释，上游门禁"描述与参数结构可任意，仅需名字命中"
2. 实测表明模型发起 tool_call 时只看工具名，不依赖参数 schema（否则现有"逐项补齐"策略下，客户端带 PascalCase `Bash` 但代理追加小写 `bash` 的场景早已报错）
3. 保留大小写还原逻辑（`shouldRestoreToolCase` + `restoreToolNameCase`），确保 Claude Code 客户端看到 PascalCase 响应

### 中风险场景

**客户端为四件工具提供自定义描述**：如中文客户端定义 `{name: "read", description: "读取文件内容", ...}`，移除后会丢失本地化描述。

**影响评估**：
- 上游模型看不到这些描述（被替换为英文标准描述）
- 但模型理解工具语义主要靠 name（`read` 本身就是自解释的），description 是辅助
- **实际影响较小**

### 低风险场景

**缓存预热期短暂下降**：改动后第一批请求的缓存会 miss（因为 tools 数组结构变了），但很快会建立新缓存。

## 测试计划

### 单元测试（free_tier_fingerprint_test.go）

新增测试用例：

```go
func TestNormalizeFourToolsForCache(t *testing.T) {
    tests := []struct {
        name     string
        input    []any
        subpath  string
        wantLen  int
        wantLast string // 最后一个工具名应为 "read"
    }{
        {
            name: "Claude Code 带 PascalCase 四件",
            input: []any{
                map[string]any{"type": "function", "function": map[string]any{"name": "Bash"}},
                map[string]any{"type": "function", "function": map[string]any{"name": "Read"}},
            },
            subpath: "chat/completions",
            wantLen: 4, // 移除 2 个，追加 4 个标准
            wantLast: "read",
        },
        {
            name: "Cherry Studio 不带工具",
            input: []any{},
            subpath: "chat/completions",
            wantLen: 4,
            wantLast: "read",
        },
        {
            name: "客户端带自定义工具 + 部分四件",
            input: []any{
                map[string]any{"type": "function", "function": map[string]any{"name": "weather"}},
                map[string]any{"type": "function", "function": map[string]any{"name": "Bash"}},
            },
            subpath: "chat/completions",
            wantLen: 5, // weather + 标准四件
            wantLast: "read",
        },
    }
    
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got := normalizeFourToolsForCache(tt.input, tt.subpath)
            if len(got) != tt.wantLen {
                t.Errorf("len = %d, want %d", len(got), tt.wantLen)
            }
            lastName := freeTierToolNameOf(got[len(got)-1].(map[string]any))
            if lastName != tt.wantLast {
                t.Errorf("last tool name = %s, want %s", lastName, tt.wantLast)
            }
        })
    }
}
```

### 集成测试

**E2E 场景**：
1. 用 Cherry Studio（不带工具）发起对话 → 验证 tools 为标准四件
2. 用 Claude Code（带 PascalCase 四件）发起对话 → 验证 tools 被替换为小写四件
3. 用带自定义工具的客户端发起对话 → 验证自定义工具保留 + 标准四件追加

**缓存验证**：
- 同一场景连续发起 3 次请求，观察上游日志中的 `cache_read_input_tokens` / `cache_creation_input_tokens`
- 预期：第 2-3 次请求缓存命中率显著提升

### 回归测试

运行现有全部测试：

```bash
go test ./internal/app -v
```

确保：
- `free_tier_fingerprint_test.go` 通过
- `e2e_launch_claude_test.go` 通过（Claude Code 端到端）
- `protocol_regression_test.go` 通过（协议兼容性）

## 回滚策略

### 快速回滚

1. 保留旧版 `ensureFreeTierTools` 为 `ensureFreeTierToolsLegacy`
2. 新增环境变量开关：

```go
if os.Getenv("OPENCODE2API_LEGACY_TOOL_FINGERPRINT") == "true" {
    ensureFreeTierToolsLegacy(bodyMap, subpath)
} else {
    ensureFreeTierTools(bodyMap, subpath) // 新逻辑
}
```

3. 若线上出现工具调用异常，立即设置环境变量回滚

### 灰度发布

1. 对 10% 流量启用新逻辑（通过随机哈希 session ID）
2. 监控指标：
   - 缓存命中率（目标：从 81-84% 提升到 95%+）
   - 工具调用成功率（目标：无下降）
   - 错误率（目标：无异常峰值）
3. 若指标正常，逐步扩大到 50% → 100%

## 预期收益

### 缓存命中率提升

**当前**：
- 总命中率 81-84%
- 稳态 96-100%（相同客户端重复对话）
- 冷启动低（不同客户端首次对话，tools 数组各异）

**改动后**：
- 总命中率预计提升到 **92-95%**
- 稳态保持 96-100%
- 冷启动显著提升（所有"只用四件工具"的客户端共享缓存）

### Token 成本节约

假设日均 10,000 次免费层请求，平均上下文 5,000 tokens：
- 缓存命中率从 84% 提升到 94% → 额外节约 10% 的 input tokens
- 月节约：10,000 × 30 × 5,000 × 0.10 = **15M tokens**
- 按 prompt cache 折扣（10% 原价），约节约 **13.5M tokens 完整计费**

## 实施步骤

1. **代码实现**：编写 `normalizeFourToolsForCache` + 修改 `ensureFreeTierTools`
2. **单元测试**：新增 3+ 测试用例覆盖主要场景
3. **本地验证**：启动网关，用不同客户端（Claude Code, Cherry Studio, curl）发起请求，抓包对比 tools 数组
4. **回归测试**：`make test` 全过
5. **灰度发布**：10% → 50% → 100%，每阶段观察 24h
6. **监控告警**：设置缓存命中率低于 90% 的告警阈值

## 开放问题

1. **是否需要白名单机制**？某些客户端可能依赖特定的工具定义不被改写（如企业内部客户端）。可考虑通过请求头（如 `X-OpenCode-Preserve-Tools: true`）豁免。

2. **自定义工具的去重**？若客户端带 `[weather, bash, weather]`（重复的 weather），当前逻辑会保留两个 weather。是否需要对自定义工具也做去重？

3. **messages 子路径的 cache_control**？Anthropic Messages API 支持 `cache_control` 块显式标记缓存边界。是否可以在 tools 数组末尾添加 `cache_control: {type: "ephemeral"}` 进一步优化？
