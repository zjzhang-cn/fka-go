package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zjzhang-cn/fka-go/internal/llm"
)

// usageSSEServer 一个会发**收尾用量帧**（`choices` 为空、只带 usage）的假模型服务。
// 返回收到的请求体，供断言 `stream_options.include_usage`。
func usageSSEServer(t *testing.T, content string, usage map[string]any) (*httptest.Server, *map[string]any) {
	t.Helper()

	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		requestBody = parsed

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		encodeAndFlush(w, map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": "test-model",
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"role": "assistant", "content": content},
				"finish_reason": nil,
			}},
		})
		if usage != nil {
			// **收尾帧**：choices 为空、只带 usage。它正是被 `len(chunk.Choices)==0`
			// 跳过的那种形状——采集必须在跳过之前。
			encodeAndFlush(w, map[string]any{
				"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": "test-model",
				"choices": []any{},
				"usage":   usage,
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(server.Close)
	return server, &requestBody
}

func usageChat(t *testing.T, server *httptest.Server) llm.ChatClient {
	t.Helper()
	return Provider{}.CreateChat(llm.Config{
		BaseURL: server.URL + "/v1", APIKey: "k", Model: "test-model",
		TimeoutMs: 5000, StreamTimeoutMs: 200, StreamRetries: 0,
	})
}

// Test用量_带usage帧时收进结果 服务端发收尾用量帧时，三个数要如实落到
// `ChatResult.Usage`；请求体也必须带 `stream_options.include_usage`——不带它
// 服务端根本不会发这一帧。
func Test用量_带usage帧时收进结果(t *testing.T) {
	server, requestBody := usageSSEServer(t, "答案", map[string]any{
		"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15,
	})

	result, err := usageChat(t, server)(context.Background(), []llm.ChatMessage{
		{Role: llm.RoleUser, Content: "在吗"},
	}, nil)
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}

	want := llm.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	if result.Usage != want {
		t.Errorf("用量 = %+v，想要 %+v", result.Usage, want)
	}

	options, _ := (*requestBody)["stream_options"].(map[string]any)
	if options == nil || options["include_usage"] != true {
		t.Errorf("请求体该带 stream_options.include_usage=true，实际 %v", (*requestBody)["stream_options"])
	}
}

// Test用量_没有usage帧时保持零值 服务端不发用量帧时，**一个数都不许猜**——
// 零值表示「没给」，展示层据此不打那一行。
func Test用量_没有usage帧时保持零值(t *testing.T) {
	server, _ := usageSSEServer(t, "答案", nil)

	result, err := usageChat(t, server)(context.Background(), []llm.ChatMessage{
		{Role: llm.RoleUser, Content: "在吗"},
	}, nil)
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if !result.Usage.IsZero() {
		t.Errorf("服务端没给用量，结果该是零值，实际 %+v", result.Usage)
	}
}

// Test用量_空choices的收尾帧不打断正文与工具拼接 收尾帧的 choices 是空的，
// 采集它**不能**把正文或工具调用的拼接带跑偏。
func Test用量_空choices的收尾帧不打断正文与工具拼接(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// 先来一段正文与一个工具调用
		encodeAndFlush(w, map[string]any{
			"id": "x", "object": "chat.completion.chunk", "model": "m",
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"role": "assistant", "content": "先答一半"},
				"finish_reason": nil,
			}},
		})
		encodeAndFlush(w, map[string]any{
			"id": "x", "object": "chat.completion.chunk", "model": "m",
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": "call-1", "type": "function",
					"function": map[string]any{"name": "search", "arguments": `{"q":"生日"}`},
				}}},
				"finish_reason": nil,
			}},
		})
		// 收尾：空 choices + 用量
		encodeAndFlush(w, map[string]any{
			"id": "x", "object": "chat.completion.chunk", "model": "m",
			"choices": []any{},
			"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(server.Close)

	result, err := usageChat(t, server)(context.Background(), []llm.ChatMessage{
		{Role: llm.RoleUser, Content: "在吗"},
	}, []llm.ToolDef{{Name: "search"}})
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}

	if result.Content != "先答一半" {
		t.Errorf("正文拼接被收尾帧带跑了：%q", result.Content)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Name != "search" ||
		result.ToolCalls[0].Arguments != `{"q":"生日"}` {
		t.Errorf("工具调用拼接被收尾帧带跑了：%+v", result.ToolCalls)
	}
	if result.Usage.TotalTokens != 7 {
		t.Errorf("用量没采到：%+v", result.Usage)
	}
}

// Test用量_断流重试后取成功那次 第一次断流、第二次成功：用量只有成功那次有，
// **不许把重试也算进去**（断流的那次压根没有用量帧）。
func Test用量_断流重试后取成功那次(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			// 断流：接了连接什么也不吐，等客户端超时
			select {
			case <-r.Context().Done():
			case <-time.After(300 * time.Millisecond):
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		encodeAndFlush(w, map[string]any{
			"id": "x", "object": "chat.completion.chunk", "model": "m",
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"role": "assistant", "content": "接上了"},
				"finish_reason": nil,
			}},
		})
		encodeAndFlush(w, map[string]any{
			"id": "x", "object": "chat.completion.chunk", "model": "m",
			"choices": []any{},
			"usage":   map[string]any{"prompt_tokens": 8, "completion_tokens": 2, "total_tokens": 10},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(server.Close)

	chat := Provider{}.CreateChat(llm.Config{
		BaseURL: server.URL + "/v1", APIKey: "k", Model: "test-model",
		TimeoutMs: 5000, StreamTimeoutMs: 60, StreamRetries: 3,
	})
	result, err := chat(context.Background(), []llm.ChatMessage{
		{Role: llm.RoleUser, Content: "在吗"},
	}, nil)
	if err != nil {
		t.Fatalf("重试后成功，不该报错：%v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("服务端收到 %d 次请求，想要 2（一次断流 + 一次成功）", got)
	}
	want := llm.TokenUsage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10}
	if result.Usage != want {
		t.Errorf("用量 = %+v，想要成功那次的 %+v（不该把重试也算进去）", result.Usage, want)
	}
}
