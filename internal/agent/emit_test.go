package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/llm"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// recordingEmitter 记录收到的回显事件。
type recordingEmitter struct {
	reasoning strings.Builder
	tools     []ToolEvent
	answers   []string
}

func (e *recordingEmitter) Reasoning(text string) { e.reasoning.WriteString(text) }
func (e *recordingEmitter) Tool(event ToolEvent)  { e.tools = append(e.tools, event) }
func (e *recordingEmitter) Answer(text string)    { e.answers = append(e.answers, text) }

// TestRun_Emitter收到推理工具与答案 钉住统一回显这条路：工具循环把三类事件推给
// Emitter——推理经 ctx 绑定的 writer、工具调用逐次、最终答案一次。前端据此各自排版。
func TestRun_Emitter收到推理工具与答案(t *testing.T) {
	source := &fakeSource{specs: []tools.Spec{searchSpec()}, replies: map[string]string{"search": "查到了"}}
	registry := tools.NewRegistry([]tools.Source{source}, tools.DefaultPolicy())

	calls := 0
	chat := func(ctx context.Context, _ []llm.ChatMessage, _ []llm.ToolDef) (llm.ChatResult, error) {
		calls++
		// 模拟 provider：推理写进 ctx 绑定的 writer（由工具循环按 Emitter 绑好）
		if w, ok := llm.ReasoningWriter(ctx); ok {
			fmt.Fprint(w, "想一想")
		}
		if calls == 1 {
			return llm.ChatResult{ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "fake__search", Arguments: `{"q":"x"}`},
			}}, nil
		}
		return llm.ChatResult{Content: "答案"}, nil
	}

	emitter := &recordingEmitter{}
	if _, err := newTestRunner(chat, registry).Run(context.Background(), RunnerInput{
		SessionID: "s1", Question: "问", PrincipalID: "wx1", Emitter: emitter,
	}); err != nil {
		t.Fatalf("Run 返错：%v", err)
	}

	// 两次模型调用各写一次推理
	if got := emitter.reasoning.String(); got != "想一想想一想" {
		t.Errorf("推理没收到：%q", got)
	}
	if len(emitter.tools) != 1 {
		t.Fatalf("工具事件数 = %d，想要 1", len(emitter.tools))
	}
	if emitter.tools[0].Name != "fake__search" || emitter.tools[0].Arguments != `{"q":"x"}` {
		t.Errorf("工具事件不对：%+v", emitter.tools[0])
	}
	if len(emitter.answers) != 1 || emitter.answers[0] != "答案" {
		t.Errorf("答案事件不对：%v", emitter.answers)
	}
}

// TestRun_没有Emitter就不经ctx写推理 nil Emitter 时工具循环不该往 ctx 绑推理 writer，
// 让 provider 走自己的落点（渠道可以完全不回显）。
func TestRun_没有Emitter就不经ctx写推理(t *testing.T) {
	source := &fakeSource{specs: []tools.Spec{searchSpec()}, replies: map[string]string{"search": "x"}}
	registry := tools.NewRegistry([]tools.Source{source}, tools.DefaultPolicy())

	sawWriter := false
	chat := func(ctx context.Context, _ []llm.ChatMessage, _ []llm.ToolDef) (llm.ChatResult, error) {
		if _, ok := llm.ReasoningWriter(ctx); ok {
			sawWriter = true
		}
		return llm.ChatResult{Content: "答"}, nil
	}

	if _, err := newTestRunner(chat, registry).Run(context.Background(), RunnerInput{
		SessionID: "s1", Question: "q", PrincipalID: "wx1",
	}); err != nil {
		t.Fatalf("Run 返错：%v", err)
	}
	if sawWriter {
		t.Error("没有 Emitter 时不该往 ctx 绑推理 writer")
	}
}
