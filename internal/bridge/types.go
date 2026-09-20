package bridge

import (
	"github.com/6Kmfi6HP/opencode2api/internal/domain"
)

// This file aggregates the wire DTO definitions used by the bridge's pure
// conversion functions as type aliases. internal/domain remains the physical
// owner of every type; aliasing here lets bridge signatures read naturally and
// keeps a single source of truth for the wire shape shared with internal/app.

type OpenAIRequest = domain.OpenAIRequest

type Message = domain.Message

type ToolCall = domain.ToolCall

type FunctionCall = domain.FunctionCall

type Tool = domain.Tool

type ToolFunction = domain.ToolFunction

type ClaudeRequest = domain.ClaudeRequest

type ClaudeMessage = domain.ClaudeMessage

type ClaudeContent = domain.ClaudeContent

type ClaudeTool = domain.ClaudeTool

type ClaudeResponse = domain.ClaudeResponse

type ResponsesAPIRequest = domain.ResponsesAPIRequest

type ResponsesTool = domain.ResponsesTool

type ReasonEffort = domain.ReasonEffort

type StoredResponseState = domain.StoredResponseState
