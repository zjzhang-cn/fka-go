package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zjzhang-cn/fka-go/internal/llm"
)

// flakySSEServer 一个前 failFirst 次**断流**、之后正常的假模型服务。
//
// 「断流」在测试里的形状是：接了连接却（连响应头都）不吐，等客户端自己超时把请求
// 掐掉。这正好命中 idle 那条路——不用真的掐 TCP，测起来稳定。计数器记服务端收到
// 几次请求，用来断言「重试了几次」。
func flakySSEServer(t *testing.T, failFirst int, content string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if int(n) <= failFirst {
			// 断流：不发任何数据。客户端取消请求时 r.Context() 会 Done，用它
			// 及时退出；再加一道 300ms 兜底，免得挂了测试卡死。
			select {
			case <-r.Context().Done():
			case <-time.After(300 * time.Millisecond):
			}
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		encodeAndFlush(w, map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": "test-model",
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"role": "assistant", "content": content},
				"finish_reason": nil,
			}},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

// Test断流后重试_再试几次能成功 钉住新行为：流被掐断不是立刻报错，而是按
// StreamRetries 再试几次——第三次接到正常流就应当拿到答案。
func Test断流后重试_再试几次能成功(t *testing.T) {
	server, calls := flakySSEServer(t, 2, "接上了")
	notice := &strings.Builder{}
	chat := Provider{Retry: notice}.CreateChat(llm.Config{
		BaseURL: server.URL + "/v1", APIKey: "k", Model: "test-model",
		TimeoutMs: 5000, StreamTimeoutMs: 60, StreamRetries: 3,
	})

	result, err := chat(context.Background(), []llm.ChatMessage{
		{Role: llm.RoleUser, Content: "在吗"},
	}, nil)
	if err != nil {
		t.Fatalf("前两次断流、第三次成功，不该报错：%v", err)
	}
	if result.Content != "接上了" {
		t.Errorf("回答 = %q，想要「接上了」", result.Content)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Errorf("服务端收到 %d 次请求，想要 3（两次断流 + 一次成功）", got)
	}
	// 重试要有提示，不静默：两次重试各一行
	if got := strings.Count(notice.String(), "[断流]"); got != 2 {
		t.Errorf("重试提示行数 = %d，想要 2：%q", got, notice.String())
	}
}

// Test断流重试用尽_如实失败：一直断流时，试满次数就报错，且错误说清是断流。
func Test断流重试用尽_如实失败(t *testing.T) {
	server, calls := flakySSEServer(t, 100, "永远到不了")
	notice := &strings.Builder{}
	chat := Provider{Retry: notice}.CreateChat(llm.Config{
		BaseURL: server.URL + "/v1", APIKey: "k", Model: "test-model",
		TimeoutMs: 5000, StreamTimeoutMs: 60, StreamRetries: 2,
	})

	_, err := chat(context.Background(), []llm.ChatMessage{
		{Role: llm.RoleUser, Content: "在吗"},
	}, nil)
	if err == nil {
		t.Fatal("一直断流应当在重试用尽后报错")
	}
	if !strings.Contains(err.Error(), "断流") {
		t.Errorf("错误该说清是断流：%v", err)
	}
	// 1 次初始 + 2 次重试
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Errorf("服务端收到 %d 次请求，想要 3（1 次初始 + 2 次重试）", got)
	}
	// 重试用尽：提示里两次重试都要有，且带上总数
	if got := strings.Count(notice.String(), "[断流]"); got != 2 {
		t.Errorf("重试提示行数 = %d，想要 2：%q", got, notice.String())
	}
	if !strings.Contains(notice.String(), "2/3") {
		t.Errorf("提示该带「第 2/3 次」，实际：%q", notice.String())
	}
}

// Test断流重试次数为0_不重试：0 是合法的——只试一次，与原行为一致。
func Test断流重试次数为0_不重试(t *testing.T) {
	server, calls := flakySSEServer(t, 1, "不该走到这")
	chat := Provider{}.CreateChat(llm.Config{
		BaseURL: server.URL + "/v1", APIKey: "k", Model: "test-model",
		TimeoutMs: 5000, StreamTimeoutMs: 60, StreamRetries: 0,
	})

	if _, err := chat(context.Background(), []llm.ChatMessage{
		{Role: llm.RoleUser, Content: "在吗"},
	}, nil); err == nil {
		t.Fatal("0 次重试时断流应当直接报错")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("服务端收到 %d 次请求，想要 1（0 次重试 = 不重试）", got)
	}
}

// Test非断流失败不重试 钉住重试的边界：HTTP 400 这类错**不是断流**，重试解决不了，
// 不该白白多打几次接口。
func Test非断流失败不重试(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)

	chat := Provider{}.CreateChat(llm.Config{
		BaseURL: server.URL + "/v1", APIKey: "k", Model: "test-model",
		TimeoutMs: 5000, StreamTimeoutMs: 60, StreamRetries: 3,
	})

	if _, err := chat(context.Background(), []llm.ChatMessage{
		{Role: llm.RoleUser, Content: "在吗"},
	}, nil); err == nil {
		t.Fatal("400 应当报错")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("400 不该重试：服务端收到 %d 次请求，想要 1", got)
	}
}

// Test断流重试次数从配置读：默认 3、可配、0 表示不重试。
func Test断流重试次数从配置读(t *testing.T) {
	t.Setenv("LLM_API_KEY", "k")
	t.Setenv("LLM_MODEL", "m")
	t.Setenv(StreamRetriesEnv, "")

	if cfg, ok := ReadConfig(); !ok || cfg.StreamRetries != DefaultStreamRetries {
		t.Errorf("默认重试次数 = %d，想要 %d", cfg.StreamRetries, DefaultStreamRetries)
	}

	t.Setenv(StreamRetriesEnv, "0")
	if cfg, _ := ReadConfig(); cfg.StreamRetries != 0 {
		t.Errorf("0 应当表示不重试，实际 %d", cfg.StreamRetries)
	}

	t.Setenv(StreamRetriesEnv, "5")
	if cfg, _ := ReadConfig(); cfg.StreamRetries != 5 {
		t.Errorf("自定义重试次数没生效，实际 %d", cfg.StreamRetries)
	}

	// 非法值退回默认，而不是让服务起不来
	t.Setenv(StreamRetriesEnv, "abc")
	if cfg, _ := ReadConfig(); cfg.StreamRetries != DefaultStreamRetries {
		t.Errorf("非法值应退回默认 %d，实际 %d", DefaultStreamRetries, cfg.StreamRetries)
	}
}
