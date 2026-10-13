package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/channels"
)

// Test用量_答案之后打一行 用量经 `Notify` 缓冲，等 `say` 打答复时补在答案之后——
// 呈现顺序是「● 助手 → 答案 → 用量」，答案仍只进 Out。
func Test用量_答案之后打一行(t *testing.T) {
	c, out, ui := newChat("")
	c.Notify(channels.UsageNotice{Usage: channels.TokenUsage{
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
	}})

	if _, err := c.Senders().Text(context.Background(), channels.SendTextParams{Text: "答案"}); err != nil {
		t.Fatal(err)
	}

	shown := ui.String()
	label := strings.Index(shown, "● 助手")
	usage := strings.Index(shown, "· 用量：入 10 / 出 5（共 15）")
	if label < 0 || usage < 0 {
		t.Fatalf("stderr 缺标签或用量行：%q", shown)
	}
	if label > usage {
		t.Errorf("用量该排在答复之后：%q", shown)
	}
	if out.String() != "答案\n" {
		t.Errorf("stdout 该只有答案，实际 %q", out.String())
	}
	if strings.Contains(out.String(), "用量") {
		t.Errorf("用量混进了 stdout：%q", out.String())
	}
}

// Test用量_ask方括号行 Ask 模式的用量行是 `[用量] …`（方括号、不上色）。
func Test用量_ask方括号行(t *testing.T) {
	out := &strings.Builder{}
	ui := &strings.Builder{}
	c := New(Options{
		Mode: ModeAsk, Session: "cli-x", Principal: "tester", Question: "问",
		Out: out, UI: ui, Palette: NewPalette(false),
	})
	c.Notify(channels.UsageNotice{Usage: channels.TokenUsage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10}})

	c.Senders().Text(context.Background(), channels.SendTextParams{Text: "答"})

	if !strings.Contains(ui.String(), "[用量] 入 8 / 出 2（共 10）") {
		t.Errorf("stderr 缺方括号用量行：%q", ui.String())
	}
}

// Test用量_全零不打 服务端没给用量时（零值）不打那一行——不拿估算冒充实测。
func Test用量_全零不打(t *testing.T) {
	c, _, ui := newChat("")
	c.Notify(channels.UsageNotice{}) // 零值

	c.Senders().Text(context.Background(), channels.SendTextParams{Text: "答"})

	if strings.Contains(ui.String(), "用量") {
		t.Errorf("零值不该打用量行：%q", ui.String())
	}
}

// Test用量行_渲染与缺项 usageLine 是纯函数：都给了、带缓存命中、没给总数、全零。
func Test用量行_渲染与缺项(t *testing.T) {
	for name, tc := range map[string]struct {
		in, out, total, cached int
		want                   string
	}{
		"都给了":   {10, 5, 15, 0, "入 10 / 出 5（共 15）"},
		"带缓存命中": {10, 5, 15, 8, "入 10（缓存命中 8） / 出 5（共 15）"},
		"没给总数":  {10, 5, 0, 0, "入 10 / 出 5"},
		"全零不打":  {0, 0, 0, 0, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := usageLine(tc.in, tc.out, tc.total, tc.cached); got != tc.want {
				t.Errorf("usageLine(%d, %d, %d, %d) = %q，想要 %q", tc.in, tc.out, tc.total, tc.cached, got, tc.want)
			}
		})
	}
}
