package main

// responseOutcome is shared by streaming and non-streaming builders so their
// top-level and item statuses cannot drift apart.
type responseOutcome struct {
	Status            string
	Event             string
	IncompleteDetails any
}

func responsesOutcome(finishReason string) responseOutcome {
	if finishReason == "length" {
		return responseOutcome{Status: "incomplete", Event: "response.incomplete", IncompleteDetails: map[string]any{"reason": "max_output_tokens"}}
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
	if req.Reasoning.Effort != "" {
		response["reasoning"] = map[string]any{"effort": req.Reasoning.Effort}
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
}
