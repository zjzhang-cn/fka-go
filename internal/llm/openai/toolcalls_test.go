// 本文件钉**流式 tool_calls 分片的归并**。
//
// ## 为什么单独立出来
//
// `sseServer`（logscope_test.go）只吐 content / reasoning，归并这段此前零覆盖——
// 而它是工具循环的主路径：参数拼不齐时，模型要么收到半截 JSON，要么整条调用丢失。
//
// ## 「缺 index」不是假想
//
// 部分 OpenAI 兼容实现会在分片里省略 `index`。缺 index 的续片没有 ID/Name，
// 若被当成新条目，会在收尾的 `call.ID != "" && call.Name != ""` 处被**整体丢掉**，
// 只剩第一段参数——表现为「工具调用参数被静默截断」。
package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/llm"
)

// toolCallSSEServer 按给定顺序吐 tool_calls 分片的假模型服务。
//
// 分片**逐字给出**：有没有 index、ID/Name 在不在，都由调用点写死——
// 那正是这些用例要钉的东西，不让 helper 替它们做主。
func toolCallSSEServer(t *testing.T, deltas []map[string]any) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		for _, delta := range deltas {
			encodeAndFlush(w, map[string]any{
				"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": "test-model",
				"choices": []any{map[string]any{
					"index": 0, "delta": delta, "finish_reason": nil,
				}},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// streamToolCalls 调一轮假模型，把归并出来的 tool_calls 拿回来。
func streamToolCalls(t *testing.T, server *httptest.Server) []llm.ToolCall {
	t.Helper()

	chat := Provider{}.CreateChat(llm.Config{
		BaseURL: server.URL + "/v1", APIKey: "k", Model: "test-model",
		TimeoutMs: 5000, StreamTimeoutMs: 5000,
	})
	result, err := chat(context.Background(), []llm.ChatMessage{
		{Role: llm.RoleUser, Content: "查一下"},
	}, nil)
	if err != nil {
		t.Fatalf("调假模型失败：%v", err)
	}
	return result.ToolCalls
}

// TestStreamToolCalls_缺index的续片接在同一条上 续片只有 arguments、没有 ID/Name：
// 各起一条的话会在收尾被整体丢掉，第一段（通常不完整的）参数成了全部。
func TestStreamToolCalls_缺index的续片接在同一条上(t *testing.T) {
	server := toolCallSSEServer(t, []map[string]any{
		{"role": "assistant", "tool_calls": []any{map[string]any{
			"id": "call_1", "type": "function",
			"function": map[string]any{"name": "search", "arguments": `{"q":`},
		}}},
		{"tool_calls": []any{map[string]any{
			"function": map[string]any{"arguments": `"三亚"}`},
		}}},
	})

	calls := streamToolCalls(t, server)
	if len(calls) != 1 {
		t.Fatalf("该归并成 1 条，实际 %d 条：%+v", len(calls), calls)
	}
	if calls[0].ID != "call_1" || calls[0].Name != "search" {
		t.Errorf("ID/Name 不对：%+v", calls[0])
	}
	// **逐字拼完**：少一段就是非法 JSON，而它还要进会话历史逐字重放
	if calls[0].Arguments != `{"q":"三亚"}` {
		t.Errorf("arguments 被截断：%q", calls[0].Arguments)
	}
}

// TestStreamToolCalls_带index的各归各的 规范路径不回归：两个调用交错分片，
// 参数各自完整、顺序按首次出现的 index。
func TestStreamToolCalls_带index的各归各的(t *testing.T) {
	server := toolCallSSEServer(t, []map[string]any{
		{"tool_calls": []any{
			map[string]any{"index": 0, "id": "call_a", "type": "function",
				"function": map[string]any{"name": "a", "arguments": `{"x":`}},
			map[string]any{"index": 1, "id": "call_b", "type": "function",
				"function": map[string]any{"name": "b", "arguments": `{"y":`}},
		}},
		{"tool_calls": []any{
			map[string]any{"index": 1, "function": map[string]any{"arguments": `2}`}},
			map[string]any{"index": 0, "function": map[string]any{"arguments": `1}`}},
		}},
	})

	calls := streamToolCalls(t, server)
	if len(calls) != 2 {
		t.Fatalf("该有 2 条，实际 %d 条：%+v", len(calls), calls)
	}
	if calls[0].Name != "a" || calls[0].Arguments != `{"x":1}` {
		t.Errorf("第一条不对：%+v", calls[0])
	}
	if calls[1].Name != "b" || calls[1].Arguments != `{"y":2}` {
		t.Errorf("第二条不对：%+v", calls[1])
	}
}
