package app

import (
	"github.com/6Kmfi6HP/opencode2api/internal/bridge"
)

// responseOutcome is shared by streaming and non-streaming builders so their
// top-level and item statuses cannot drift apart. Thin alias forwarding to bridge.
type responseOutcome = bridge.ResponseOutcome

// responsesOutcome maps a finish reason onto the Responses terminal outcome.
// Thin shell forwarding to bridge.
func responsesOutcome(finishReason string) responseOutcome {
	return bridge.ResponsesOutcome(finishReason)
}

// outputIndexAllocator assigns indices by first appearance. It deliberately
// does not derive one item's index from whether another item happened to exist.
// Thin alias forwarding to bridge.
type outputIndexAllocator = bridge.OutputIndexAllocator

func applyResponsesRequestEcho(response map[string]any, req ResponsesAPIRequest) {
	if req.Metadata != nil {
		response["metadata"] = cloneJSONValue(req.Metadata)
	}
	// Build reasoning echo from all present sub-fields.
	reasoningEcho := map[string]any{}
	if req.Reasoning.Effort != "" {
		reasoningEcho["effort"] = req.Reasoning.Effort
	}
	if req.Reasoning.Summary != "" {
		reasoningEcho["summary"] = req.Reasoning.Summary
	}
	if req.Reasoning.Mode != "" {
		reasoningEcho["mode"] = req.Reasoning.Mode
	}
	if len(reasoningEcho) > 0 {
		response["reasoning"] = reasoningEcho
	}
	if req.ParallelToolCalls != nil {
		response["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if req.Temperature != nil {
		response["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		response["top_p"] = *req.TopP
	}
	if req.MaxTokens != nil {
		response["max_output_tokens"] = *req.MaxTokens
	}
	if req.Store != nil {
		response["store"] = *req.Store
	}
	if req.Instructions != "" {
		response["instructions"] = req.Instructions
	}
	if req.User != "" {
		response["user"] = req.User
	}
	if req.PreviousResponseID != "" {
		response["previous_response_id"] = req.PreviousResponseID
	}
	if req.Stop != nil {
		response["stop"] = req.Stop
	}
	if req.FrequencyPenalty != nil {
		response["frequency_penalty"] = *req.FrequencyPenalty
	}
	if req.PresencePenalty != nil {
		response["presence_penalty"] = *req.PresencePenalty
	}
	if req.Text != nil {
		response["text"] = req.Text
	}
	if req.Truncation != "" {
		response["truncation"] = req.Truncation
	}
	if req.ServiceTier != "" {
		response["service_tier"] = req.ServiceTier
	}
	if req.PromptCacheKey != "" {
		response["prompt_cache_key"] = req.PromptCacheKey
	}
	if req.SafetyIdentifier != nil {
		response["safety_identifier"] = req.SafetyIdentifier
	}
	if req.TopLogprobs != nil {
		response["top_logprobs"] = *req.TopLogprobs
	}
}
