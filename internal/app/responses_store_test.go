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
			// tool_call 上下文(messages 长度断言在下方)。tools 字段如果
			// 非零,只能是免费层门禁(上游 2026-09-18,按解析后的模型判定,
			// 缺 bash/glob/grep/read 任一即 403)对免费模型请求兜底的四件
			// 占位——这不构成 store 泄漏;客户端第二轮并未声明任何工具,
			// 代理也未从 storedResponses 取回上一轮工具(state 中根本无
			// 记录,见 storeResponseState 的 store 早退)。"primary-model"
			// 在测试目录中未被标记为免费模型时 tools 应该完全为空。
			tools, _ := payload["tools"].([]any)
			if len(tools) != 0 && len(tools) != 4 {
				t.Fatalf("tools leaked from store:false response: %#v", payload["tools"])
			}
			for _, rt := range tools {
				tm, ok := rt.(map[string]any)
				if !ok {
					t.Fatalf("injected tool %v of unexpected shape", rt)
				}
				var name string
				if fn, ok := tm["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				} else {
					name, _ = tm["name"].(string)
				}
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
