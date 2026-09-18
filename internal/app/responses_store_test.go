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
			// store:false 语义红线: 重放的 messages 不得包含上一轮的
			// tool_call 上下文(messages 长度断言在下方)。tools 字段因
			// 上游 2026-09-18 免费层新门禁(必须带 bash/glob/grep/read
			// 四件,缺即 403)由 buildOCRequestWithSubpath 兜底注入——这不
			// 是 store 泄漏,客户端并未声明任何工具、代理也未从
			// storedResponses 取回上一轮工具(state 中根本无记录,
			// 见 storeResponseState 的 store 早退)。断言 tools 恰为四
			// 件免费层占位工具,证明无上一轮工具穿透。
			tools, _ := payload["tools"].([]any)
			if len(tools) != 4 {
				t.Fatalf("tools leaked from store:false response: %#v", payload["tools"])
			}
			for _, rt := range tools {
				fn, ok := rt.(map[string]any)["function"].(map[string]any)
				if !ok {
					t.Fatalf("injected tool %v of unexpected shape", rt)
				}
				name, _ := fn["name"].(string)
				if name != "bash" && name != "glob" && name != "grep" && name != "read" {
					t.Fatalf("non-free-tier tool %q leaked into store:false replay", name)
				}
			}
			messages, _ := payload["messages"].([]any)
			if len(messages) != 1 {
				encoded, _ := json.Marshal(messages)
				t.Fatalf("previous output replayed after store:false: %s", encoded)
			}
		})
	}
}
