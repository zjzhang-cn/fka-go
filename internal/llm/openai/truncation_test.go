package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/llm"
)

// sseChunk 一帧 chunk，finish_reason 可为 nil 或 "length" 等。
func sseChunk(delta map[string]any, finish any) []byte {
	encoded, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": "test-model",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	})
	return []byte("data: " + string(encoded) + "\n\n")
}

// Test截断_finish_reason_length 推理占满 max_tokens、正文为空时的收尾帧是
// `finish_reason=length`——必须**明确报被截断**，而不是让空回答冒成上层那句含糊的
// 「既没回答也没调用工具」（真实日志里就是这么出现的）。
func Test截断_finish_reason_length(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(sseChunk(map[string]any{"role": "assistant", "reasoning_content": "想很久很久……"}, nil))
		_, _ = w.Write(sseChunk(map[string]any{"role": "assistant"}, "length"))
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	cfg := llm.Config{
		BaseURL: server.URL, APIKey: "k", Model: "m",
		TimeoutMs: 5000, StreamTimeoutMs: 5000,
	}
	chat := Provider{}.CreateChat(cfg)

	_, err := chat(context.Background(), []llm.ChatMessage{{Role: llm.RoleUser, Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("被长度截断该返错")
	}
	if !strings.Contains(err.Error(), "截断") || !strings.Contains(err.Error(), "长度限制") {
		t.Errorf("该说清是被长度限制截断：%v", err)
	}
}

// Test截断_有正文则不报错 被截断但有正文时不返错——把已有的一半答案交出去，
// 比整轮失败强。
func Test截断_有正文则不报错(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(sseChunk(map[string]any{"role": "assistant", "content": "半句答"}, nil))
		_, _ = w.Write(sseChunk(map[string]any{"role": "assistant"}, "length"))
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	cfg := llm.Config{
		BaseURL: server.URL, APIKey: "k", Model: "m",
		TimeoutMs: 5000, StreamTimeoutMs: 5000,
	}
	chat := Provider{}.CreateChat(cfg)

	result, err := chat(context.Background(), []llm.ChatMessage{{Role: llm.RoleUser, Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("有正文时不该返错：%v", err)
	}
	if result.Content != "半句答" {
		t.Errorf("该把已有的正文交出去，实际 %q", result.Content)
	}
}
