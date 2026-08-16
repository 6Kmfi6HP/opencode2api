package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesStoreFalseDoesNotMakeStateAvailableToPreviousResponseID(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			responseID := fmt.Sprintf("resp_not_stored_%t", stream)
			firstBody := fmt.Sprintf(`{"id":%q,"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"weather","arguments":"{}"}}]},"finish_reason":"stop"}]}
data: [DONE]
`, responseID)
			if stream {
				firstBody = "data: " + firstBody
			} else {
				firstBody = fmt.Sprintf(`{"id":%q,"choices":[{"message":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, responseID)
			}
			transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
				{status: http.StatusOK, body: firstBody},
				{status: http.StatusOK, body: `{"id":"resp_next","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`},
			})

			firstReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{
				"model":"primary-model",
				"input":"call a tool",
				"stream":%t,
				"store":false,
				"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}]
			}`, stream)))
			responsesHandler(httptest.NewRecorder(), firstReq)

			secondReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{
				"model":"primary-model",
				"previous_response_id":%q,
				"input":"continue"
			}`, responseID)))
			secondRec := httptest.NewRecorder()
			responsesHandler(secondRec, secondReq)
			if secondRec.Code != http.StatusOK {
				t.Fatalf("second status = %d, body=%s", secondRec.Code, secondRec.Body.String())
			}

			payload := transport.requestPayloads[1]
			if _, ok := payload["tools"]; ok {
				t.Fatalf("tools leaked from store:false response: %#v", payload["tools"])
			}
			messages, _ := payload["messages"].([]any)
			if len(messages) != 1 {
				encoded, _ := json.Marshal(messages)
				t.Fatalf("previous output replayed after store:false: %s", encoded)
			}
		})
	}
}
