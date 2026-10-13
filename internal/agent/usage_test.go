package agent

import (
	"context"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/llm"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// TestRun_用量是各步之和 多步工具轮的用量是**整轮之和**：第一步（调工具）与
// 第二步（给答案）各自的服务端用量要累加，而不是只留最后一步那次。
func TestRun_用量是各步之和(t *testing.T) {
	source := &fakeSource{specs: []tools.Spec{searchSpec()}, replies: map[string]string{"search": "查到"}}
	registry := tools.NewRegistry([]tools.Source{source}, tools.DefaultPolicy())

	calls := 0
	chat := func(ctx context.Context, messages []llm.ChatMessage, defs []llm.ToolDef) (llm.ChatResult, error) {
		calls++
		if calls == 1 {
			return llm.ChatResult{
				ToolCalls: []llm.ToolCall{{ID: "c1", Name: "search", Arguments: `{"q":"x"}`}},
				Usage:     llm.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
			}, nil
		}
		return llm.ChatResult{
			Content: "答案",
			Usage:   llm.TokenUsage{PromptTokens: 20, CompletionTokens: 7, TotalTokens: 27},
		}, nil
	}

	runner := newTestRunner(chat, registry)
	result, err := runner.Run(context.Background(), RunnerInput{
		SessionID: "s", PrincipalID: "p", Question: "问",
	})
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}

	want := llm.TokenUsage{PromptTokens: 30, CompletionTokens: 12, TotalTokens: 42}
	if result.Usage != want {
		t.Errorf("用量 = %+v，想要各步之和 %+v", result.Usage, want)
	}
}
