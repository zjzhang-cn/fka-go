package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/llm"
)

// sseServer 一个只会说 SSE 的假模型服务。
//
// **要流式**：这个 provider 只走 `CreateChatCompletionStream`，非流式的分支
// 根本不会被执行到。所以测试它就得真的吐 `data: ` 帧。
func sseServer(t *testing.T, reasoning string, content string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		chunk := func(delta map[string]any) {
			encodeAndFlush(w, map[string]any{
				"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": "test-model",
				"choices": []any{map[string]any{
					"index": 0, "delta": delta, "finish_reason": nil,
				}},
			})
		}

		if reasoning != "" {
			chunk(map[string]any{"role": "assistant", "reasoning_content": reasoning})
		}
		if content != "" {
			chunk(map[string]any{"role": "assistant", "content": content})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func encodeAndFlush(w http.ResponseWriter, payload map[string]any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", encoded)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// readLogs 读日志文件里**这一次调用**写下的行。
//
// ## 为什么读文件，而不是换掉 os.Stdout 去抓控制台
//
// 换 `os.Stdout` 那个做法更省事，但它是**有竞态的**——`Logger.write` 每次写入时
// 现读 `os.Stdout`，而恢复函数在写它，`-race` 会当场报出来。文件那条路没有
// 这个问题：写入全在 `Logger.mu` 之下，且文件**始终全量**（控制台才按级别过滤），
// 所以连 `LOG_LEVEL` 都不用动。
//
// ## `since` 是必须的，不是可选的优化
//
// 日志文件是**包级 logger 写的，按天累积**：上一次跑留下的行、同一包里别的用例
// 写的行、`-count=2` 跑第二遍留下的行，全都在里面。所以每条用例都要用**自己
// 独有的标记**去筛（下面的 m-7 / m-9 / 那段 250 字的推理）。
// 不筛的话这条用例会自己把自己跑挂——那正是它第一次失败时的样子。
func readLogs(t *testing.T, since string) string {
	t.Helper()

	path := config.LogFilePath(time.Now().UTC())
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到日志文件 %s：%v", path, err)
	}

	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, since) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// Test模型请求与返回的日志也带账号 模型客户端这一层**看不见渠道**——账号是消息层
// 绑在 ctx 上、一路传下来的。所以这条用例单独钉它：漏了绑定，提交与返回这两条
// 就是「谁调的模型都查不出来」的日志。
func Test模型请求与返回的日志也带账号(t *testing.T) {
	server := sseServer(t, "先想想怎么查", "查到了，去年的事。")
	// 推理默认往控制台打 `[推理] xxx`，那不是日志——关掉，免得干扰判断
	chat := Provider{}.CreateChat(llm.Config{
		BaseURL: server.URL + "/v1", APIKey: "test-key", Model: "test-model",
		TimeoutMs: 5000, StreamTimeoutMs: 5000,
	})

	// **模拟消息层绑过**：这就是整条链传下来的那个 ctx
	ctx := config.Bind(context.Background(), config.Context{
		"channel": "ilink", "account": "account_002", "messageId": "m-7",
		"conversation": "wx_zhang",
	})

	result, err := chat(ctx, []llm.ChatMessage{
		{Role: llm.RoleUser, Content: "去年三亚怎么样"},
	}, nil)
	if err != nil {
		t.Fatalf("调假模型失败：%v", err)
	}
	if result.Content != "查到了，去年的事。" {
		t.Fatalf("该拿到回答，实际 %q", result.Content)
	}
	logged := readLogs(t, "messageId=m-7")

	for _, want := range []string{"提交模型请求", "模型返回"} {
		if !strings.Contains(logged, want) {
			t.Errorf("该有「%s」这条日志，实际输出：\n%s", want, logged)
		}
	}

	// **逐行**验：提交、返回、推理三条都要带归属
	checked := 0
	for _, line := range strings.Split(logged, "\n") {
		if !strings.Contains(line, "提交模型请求") &&
			!strings.Contains(line, "模型返回") &&
			!strings.Contains(line, "模型的推理") {
			continue
		}
		checked++
		if !strings.Contains(line, "account=account_002") {
			t.Errorf("该带账号归属，实际：\n%s", line)
		}
		if !strings.Contains(line, "messageId=m-7") {
			t.Errorf("该带消息号，实际：\n%s", line)
		}
	}
	if checked < 3 {
		t.Errorf("样本太少（%d 行），这条用例没真跑到那三跳", checked)
	}

	// 提交那一条要能回答「打到了哪个接口」——这是排查换过 baseURL 的部署时
	// 第一件要确认的事，光看答案猜不出来
	for _, want := range []string{"host=", "model=test-model", "stream=true"} {
		if !strings.Contains(logged, want) {
			t.Errorf("提交日志该带 %s，实际输出：\n%s", want, logged)
		}
	}

	// **绝不能把 key 写进日志**
	if strings.Contains(logged, "test-key") {
		t.Errorf("日志里出现了 API key：\n%s", logged)
	}
}

// Test推理会被记下来 以前推理只进控制台——重定向到文件或 LLM_SHOW_REASONING=0
// 之后就彻底没了，「模型为什么这么答」只能猜
func Test推理会被记下来(t *testing.T) {
	server := sseServer(t, "先查三亚的行程单再回答", "查到了。")
	// 推理默认往控制台打 `[推理] xxx`，那不是日志——关掉，免得干扰判断
	chat := Provider{}.CreateChat(llm.Config{
		BaseURL: server.URL + "/v1", APIKey: "k", Model: "test-model",
		TimeoutMs: 5000, StreamTimeoutMs: 5000,
	})
	ctx := config.Bind(context.Background(), config.Context{
		"account": "account_003", "messageId": "m-9",
	})

	if _, err := chat(ctx, []llm.ChatMessage{
		{Role: llm.RoleUser, Content: "三亚"},
	}, nil); err != nil {
		t.Fatalf("调假模型失败：%v", err)
	}
	logged := readLogs(t, "messageId=m-9")

	if !strings.Contains(logged, "模型的推理") {
		t.Fatalf("该记下推理，实际输出：\n%s", logged)
	}
	if !strings.Contains(logged, "先查三亚的行程单再回答") {
		t.Errorf("该记下推理的原文，实际输出：\n%s", logged)
	}
	if !strings.Contains(logged, "account=account_003") {
		t.Errorf("推理日志也要带账号，实际输出：\n%s", logged)
	}
	// 字数与截断后的开头都要在
	if !strings.Contains(logged, "chars=11") {
		t.Errorf("该记推理的字数，实际输出：\n%s", logged)
	}
}

// Test推理太长会被截断 推理能有几千字，全量落盘会把日志撑爆；
// 而只看得到开头会让人以为那就是全部——所以截断处要标出被截掉了多少
func Test推理太长会被截断(t *testing.T) {
	long := strings.Repeat("甲", reasoningLogChars+50)
	server := sseServer(t, long, "答完了。")
	// 推理默认往控制台打 `[推理] xxx`，那不是日志——关掉，免得干扰判断
	chat := Provider{}.CreateChat(llm.Config{
		BaseURL: server.URL + "/v1", APIKey: "k", Model: "test-model",
		TimeoutMs: 5000, StreamTimeoutMs: 5000,
	})
	if _, err := chat(context.Background(), []llm.ChatMessage{
		{Role: llm.RoleUser, Content: "问题"},
	}, nil); err != nil {
		t.Fatalf("调假模型失败：%v", err)
	}
	logged := readLogs(t, "共 250 字")

	// **完整推理不该出现在日志里**——那正是要避免的体积
	if strings.Contains(logged, long) {
		t.Error("超长推理该被截断，实际整段落进了日志")
	}
	if !strings.Contains(logged, fmt.Sprintf("共 %d 字", len([]rune(long)))) {
		t.Errorf("截断处该标出总字数，实际输出：\n%s", logged)
	}
	if !strings.Contains(logged, fmt.Sprintf("chars=%d", len([]rune(long)))) {
		t.Errorf("该如实记下完整字数（截断只影响记多少，不影响记多少字），实际输出：\n%s", logged)
	}
}
