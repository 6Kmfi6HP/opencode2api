package app

// responseOutcome is shared by streaming and non-streaming builders so their
// top-level and item statuses cannot drift apart. Status "" = 无 Responses
// 等价物,status 置空不发(对齐 Bifrost *string omitempty 的 unset 语义)。
type responseOutcome struct {
	Status            string
	Event             string
	IncompleteDetails any
}

// responsesOutcome maps a chat-style finish_reason to the Responses status /
// terminal event / incomplete_details(对齐 Bifrost
// responsesStatusFromChatFinishReason 与 anthropicResponsesStatus):
//   - 截断类:finish_reason=length 与 Anthropic 的
//     model_context_window_exceeded 都报 incomplete/max_output_tokens;
//   - content_filter 报 incomplete/content_filter;
//   - pause_turn/compaction 表示未完结且无 Responses 等价物,status 置空
//     不报完成态。
func responsesOutcome(finishReason string) responseOutcome {
	switch finishReason {
	case "length", "model_context_window_exceeded":
		return responseOutcome{Status: "incomplete", Event: "response.incomplete", IncompleteDetails: map[string]any{"reason": "max_output_tokens"}}
	case "content_filter":
		return responseOutcome{Status: "incomplete", Event: "response.incomplete", IncompleteDetails: map[string]any{"reason": "content_filter"}}
	case "pause_turn", "compaction":
		return responseOutcome{}
	}
	return responseOutcome{Status: "completed", Event: "response.completed"}
}

// outputIndexAllocator assigns indices by first appearance. It deliberately
// does not derive one item's index from whether another item happened to exist.
type outputIndexAllocator struct{ next int }

func (a *outputIndexAllocator) Allocate() int {
	index := a.next
	a.next++
	return index
}

func (a *outputIndexAllocator) Len() int { return a.next }

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
