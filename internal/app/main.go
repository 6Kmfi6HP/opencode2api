package app

import (
	"github.com/6Kmfi6HP/opencode2api/internal/domain"
)

// Protocol DTOs live in internal/domain. Aliases keep the root package main
// surface stable while the implementation is split into packages.
type OpenAIRequest = domain.OpenAIRequest
type Message = domain.Message
type ToolCall = domain.ToolCall
type FunctionCall = domain.FunctionCall
type Tool = domain.Tool
type ToolFunction = domain.ToolFunction
type AppConfig = domain.AppConfig
type KeywordMatchType = domain.KeywordMatchType
type ModelKeywordRule = domain.ModelKeywordRule

const MatchContains = domain.MatchContains
const MatchPrefix = domain.MatchPrefix
const MatchExact = domain.MatchExact
const MatchRegex = domain.MatchRegex

type ModelAliasList = domain.ModelAliasList

type Socks5Proxy = domain.Socks5Proxy
type ClaudeRequest = domain.ClaudeRequest
type ClaudeMessage = domain.ClaudeMessage
type ClaudeContent = domain.ClaudeContent
type ClaudeTool = domain.ClaudeTool
type ClaudeResponse = domain.ClaudeResponse
type ClaudeUsage = domain.ClaudeUsage
type ResponsesAPIRequest = domain.ResponsesAPIRequest
type ResponsesTool = domain.ResponsesTool
type ReasonEffort = domain.ReasonEffort
type StoredResponseState = domain.StoredResponseState
