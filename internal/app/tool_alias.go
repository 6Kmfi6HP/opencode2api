package app

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Every caller tool is renamed before a free-tier request goes upstream: the
// gate rejects bodies whose tool vocabulary still looks like another agent's
// (Claude Code's Read/Edit/Bash/Task/...). The alias is an opaque digest; the
// response path rebuilds the same digest from the caller's offered names, so no
// request-global table is needed. The prefix makes an alias recognizable.
const (
	callerToolAliasPrefix = "clnt_"
	agentToolNameLimit    = 64
)

// callerToolAlias returns the wire name for a caller tool. The digest is fixed
// length (29 bytes), so any legal caller name fits the upstream limit.
func callerToolAlias(name string) string {
	if name == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(name))
	alias := callerToolAliasPrefix + hex.EncodeToString(sum[:12])
	if len(alias) > agentToolNameLimit {
		return ""
	}
	return alias
}

// isCallerToolAlias reports whether name already carries our prefix. Retries
// reuse the same body map, so an alias from the first attempt must not be
// hashed again.
func isCallerToolAlias(name string) bool {
	return strings.HasPrefix(name, callerToolAliasPrefix)
}

// setFreeTierToolName writes the name back into whichever tool shape the
// protocol uses: chat nests it under function, Anthropic/Responses keep it at
// the top level.
func setFreeTierToolName(tool map[string]any, name string) {
	if fn, ok := tool["function"].(map[string]any); ok {
		fn["name"] = name
		return
	}
	tool["name"] = name
}

// rewriteAliasedToolReferences keeps tool_choice and conversation history in
// sync with the renamed tool definitions. Without this, a multi-turn caller
// that selected or previously called "read" would send the old name while the
// tool list now advertises "clnt_…", and the upstream would reject or ignore
// the reference.
func rewriteAliasedToolReferences(bodyMap map[string]any, subpath string, aliases map[string]string) {
	if len(aliases) == 0 {
		return
	}
	rename := func(name string) string {
		if alias, ok := aliases[name]; ok {
			return alias
		}
		return name
	}
	switch subpath {
	case "messages":
		rewriteAnthropicToolReferences(bodyMap, rename)
	case "responses":
		rewriteResponsesToolReferences(bodyMap, rename)
	default:
		rewriteChatToolReferences(bodyMap, rename)
	}
}

func rewriteChatToolReferences(bodyMap map[string]any, rename func(string) string) {
	switch choice := bodyMap["tool_choice"].(type) {
	case string:
		bodyMap["tool_choice"] = rename(choice)
	case map[string]any:
		if fn, ok := choice["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				fn["name"] = rename(name)
			}
		}
	}
	messages, _ := bodyMap["messages"].([]any)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if choice, ok := message["function_call"].(map[string]any); ok {
			if name, _ := choice["name"].(string); name != "" {
				choice["name"] = rename(name)
			}
		}
		calls, _ := message["tool_calls"].([]any)
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				continue
			}
			if fn, ok := call["function"].(map[string]any); ok {
				if name, _ := fn["name"].(string); name != "" {
					fn["name"] = rename(name)
				}
			}
		}
	}
}

func rewriteAnthropicToolReferences(bodyMap map[string]any, rename func(string) string) {
	if choice, ok := bodyMap["tool_choice"].(map[string]any); ok {
		if name, _ := choice["name"].(string); name != "" {
			choice["name"] = rename(name)
		}
	}
	messages, _ := bodyMap["messages"].([]any)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		content, _ := message["content"].([]any)
		for _, rawBlock := range content {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				continue
			}
			if typ, _ := block["type"].(string); typ != "tool_use" && typ != "server_tool_use" {
				continue
			}
			if name, _ := block["name"].(string); name != "" {
				block["name"] = rename(name)
			}
		}
	}
}

func rewriteResponsesToolReferences(bodyMap map[string]any, rename func(string) string) {
	switch choice := bodyMap["tool_choice"].(type) {
	case string:
		bodyMap["tool_choice"] = rename(choice)
	case map[string]any:
		if name, _ := choice["name"].(string); name != "" {
			choice["name"] = rename(name)
		}
		if fn, ok := choice["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				fn["name"] = rename(name)
			}
		}
	}
	input, _ := bodyMap["input"].([]any)
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch typ, _ := item["type"].(string); typ {
		case "function_call", "custom_tool_call", "tool_call":
			if name, _ := item["name"].(string); name != "" {
				item["name"] = rename(name)
			}
		}
	}
}
