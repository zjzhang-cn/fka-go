package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/agent"
	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/channels/cli"
	"github.com/zjzhang-cn/fka-go/internal/messages"
)

// stubRunner 假的工具循环：只记下每次 Run 的输入，回一句能被断言的话。
// REPL 的行为在 cli 包验；这里只钉 cmd 侧那条接线——`@引用` 展开进模型问题。
type stubRunner struct {
	mu     sync.Mutex
	inputs []agent.RunnerInput
}

func (s *stubRunner) Run(_ context.Context, input agent.RunnerInput) (agent.RunResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inputs = append(s.inputs, input)
	return agent.RunResult{
		Text: "答：" + input.Question, Steps: 1, StoppedBy: agent.StoppedByAnswered,
	}, nil
}

func (s *stubRunner) seen() []agent.RunnerInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.RunnerInput(nil), s.inputs...)
}

// Test聊天回合_引用文件进模型问题：`@路径` 的正文要进到模型看到的问题里，
// 并在 stderr 上提示引了哪些文件，答案仍只写 stdout。
func Test聊天回合_引用文件进模型问题(t *testing.T) {
	path := filepath.Join(t.TempDir(), "笔记.txt")
	if err := os.WriteFile(path, []byte("三亚的行程"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &stubRunner{}
	handler := messages.NewHandler(runner)
	out := &strings.Builder{}
	ui := &strings.Builder{}
	channel := cli.New(cli.Options{
		Mode: cli.ModeChat, Session: "cli-x", Principal: "tester",
		Out: out, UI: ui, Palette: cli.NewPalette(false),
	})

	in := channels.InboundMessage{
		ChannelID: "cli", AccountID: "local", MessageID: "m1",
		PrincipalID: "tester", ConversationID: "cli-x",
		Parts: []channels.Part{channels.TextPart("看下 @" + path)},
	}
	chatTurn(context.Background(), handler, channel, in, ui, cli.NewPalette(false), false)

	inputs := runner.seen()
	if len(inputs) != 1 {
		t.Fatalf("跑了 %d 轮，想要 1 轮", len(inputs))
	}
	if !strings.Contains(inputs[0].Question, "三亚的行程") {
		t.Errorf("文件正文没进问题：%q", inputs[0].Question)
	}
	if !strings.Contains(ui.String(), "引用文件") {
		t.Errorf("stderr 没提示引用：%q", ui.String())
	}
	if !strings.Contains(out.String(), "答：") {
		t.Errorf("答案没写到 stdout：%q", out.String())
	}
	if !handlerSeenSession(inputs, "cli-x") {
		t.Errorf("会话键没传下去：%+v", inputs)
	}
}

func handlerSeenSession(inputs []agent.RunnerInput, want string) bool {
	for _, in := range inputs {
		if in.SessionID == want {
			return true
		}
	}
	return false
}
