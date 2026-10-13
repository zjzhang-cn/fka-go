package messages_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/agent"
	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/llm"
	"github.com/zjzhang-cn/fka-go/internal/messages"
)

// stubRunner 假的工具循环：只记下每次 Run 的输入，结果由用例定。
// 与 handler_test 里那套真模型不同——`Ask` 的归类逻辑不需要一次真的模型调用。
type stubRunner struct {
	mu     sync.Mutex
	inputs []agent.RunnerInput
	result agent.RunResult
	err    error
}

func (s *stubRunner) Run(_ context.Context, input agent.RunnerInput) (agent.RunResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inputs = append(s.inputs, input)
	return s.result, s.err
}

func (s *stubRunner) seen() []agent.RunnerInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.RunnerInput(nil), s.inputs...)
}

// TestAsk_答出来时不带说明也不发送 一次成功的问答只回结果：Explain 与 Cause 都空，
// 而且**Ask 自己不发送**——呈现是调用方（Handle 或 CLI）的事。
func TestAsk_答出来时不带说明也不发送(t *testing.T) {
	runner := &stubRunner{result: agent.RunResult{
		Text: "三亚挺好", Steps: 1, StoppedBy: agent.StoppedByAnswered,
	}}
	channel := &fakeChannel{accountID: "acct-1"}

	outcome := messages.NewHandler(runner).Ask(context.Background(), messages.AskInput{
		Channel: channel, Message: textMessage("去年三亚怎么样"),
	})

	if outcome.Explain != "" {
		t.Errorf("答出来了不该带说明：%q", outcome.Explain)
	}
	if outcome.Cause != nil {
		t.Errorf("答出来了不该带原因：%v", outcome.Cause)
	}
	if outcome.Result.Text != "三亚挺好" {
		t.Errorf("结果没透传：%q", outcome.Result.Text)
	}
	if got := len(channel.sentTexts()); got != 0 {
		t.Errorf("Ask 不该发送，实际发了 %d 条", got)
	}
}

// TestAsk_模型出错时把原因带回来 模型的错**如实报**，而且要把错误本身带出来
// （调用方靠它决定退出码），不能只留一句给人看的话。
func TestAsk_模型出错时把原因带回来(t *testing.T) {
	failure := errors.New("401 unauthorized")
	runner := &stubRunner{err: failure}

	outcome := messages.NewHandler(runner).Ask(context.Background(), messages.AskInput{
		Channel: &fakeChannel{accountID: "acct-1"}, Message: textMessage("你好"),
	})

	if outcome.Cause == nil {
		t.Fatal("模型出错该把原因带回来")
	}
	if !errors.Is(outcome.Cause, failure) {
		t.Errorf("原因该是那次失败，实际 %v", outcome.Cause)
	}
	if !strings.Contains(outcome.Explain, "401 unauthorized") {
		t.Errorf("说明该含真实原因：%q", outcome.Explain)
	}
}

// TestAsk_身份缺失只给拒答说明 缺 PrincipalID 时**连模型都不该调**，只回一句拒答。
func TestAsk_身份缺失只给拒答说明(t *testing.T) {
	runner := &stubRunner{}
	message := textMessage("记一条：孩子过敏")
	message.PrincipalID = ""

	outcome := messages.NewHandler(runner).Ask(context.Background(), messages.AskInput{
		Channel: &fakeChannel{accountID: "acct-1"}, Message: message,
	})

	if !strings.Contains(outcome.Explain, "认不出你是谁") {
		t.Errorf("该说清为什么拒答：%q", outcome.Explain)
	}
	if outcome.Cause != nil {
		t.Errorf("拒答不是失败，不该带原因：%v", outcome.Cause)
	}
	if len(runner.seen()) != 0 {
		t.Errorf("拒答不该调模型，实际调了 %d 次", len(runner.seen()))
	}
}

// TestAsk_不给会话键时按渠道协议算 渠道不传 SessionID 时，会话键是
// `渠道:账号:会话`——历史文件名就按它取。
func TestAsk_不给会话键时按渠道协议算(t *testing.T) {
	runner := &stubRunner{result: agent.RunResult{Text: "好"}}
	message := textMessage("问")

	messages.NewHandler(runner).Ask(context.Background(), messages.AskInput{
		Channel: &fakeChannel{accountID: "acct-1"}, Message: message,
	})

	seen := runner.seen()
	if len(seen) != 1 {
		t.Fatalf("该跑一轮，实际 %d 轮", len(seen))
	}
	if want := channels.SessionKeyOf(&message); seen[0].SessionID != want {
		t.Errorf("会话键 = %q，想要 %q", seen[0].SessionID, want)
	}
}

// TestAsk_本地输入三样直达循环 SessionID / Ephemeral / Images 这三样从 CLI 一路
// 传到工具循环，中间不许被丢掉——丢了的表现是「--session 不生效」或「@图片 没发出去」。
func TestAsk_本地输入三样直达循环(t *testing.T) {
	runner := &stubRunner{result: agent.RunResult{Text: "看图了"}}
	images := []llm.ImageAttachment{{Name: "图.png", DataURI: "data:image/png;base64,AAAA"}}

	messages.NewHandler(runner).Ask(context.Background(), messages.AskInput{
		Channel:   &fakeChannel{accountID: "acct-1"},
		Message:   textMessage("这是什么"),
		SessionID: "cli-abc",
		Ephemeral: true,
		Images:    images,
	})

	seen := runner.seen()
	if len(seen) != 1 {
		t.Fatalf("该跑一轮，实际 %d 轮", len(seen))
	}
	if seen[0].SessionID != "cli-abc" {
		t.Errorf("SessionID = %q，想要 cli-abc", seen[0].SessionID)
	}
	if !seen[0].Ephemeral {
		t.Error("Ephemeral 没传下去（一次性问答会落盘）")
	}
	if len(seen[0].Images) != 1 || seen[0].Images[0].Name != "图.png" {
		t.Errorf("Images 没传下去：%+v", seen[0].Images)
	}
}

// TestAsk_空答复给诚实句 模型给了空正文时，这一轮该回的是**诚实句**而不是一段
// 空串——`Text()` 是发送方唯一该用的入口。交互式前端与渠道从此一致。
func TestAsk_空答复给诚实句(t *testing.T) {
	runner := &stubRunner{result: agent.RunResult{Text: "   \n  "}}

	outcome := messages.NewHandler(runner).Ask(context.Background(), messages.AskInput{
		Channel: &fakeChannel{accountID: "acct-1"}, Message: textMessage("你好"),
	})

	if outcome.Cause != nil {
		t.Fatalf("空答复不是调用失败：%v", outcome.Cause)
	}
	if strings.TrimSpace(outcome.Text()) == "" {
		t.Error("该回一句诚实说明，不该发空串")
	}
	if !strings.Contains(outcome.Text(), "换个说法") {
		t.Errorf("诚实句不对：%q", outcome.Text())
	}
}

// TestAsk_结果如实透传 步数、停止原因、工具记录都要原样带出来——CLI 的用量行与
// debug 行都读它。
func TestAsk_结果如实透传(t *testing.T) {
	want := agent.RunResult{
		Text:      "查到了",
		UsedTools: []string{"mcp__memory__search_memories"},
		ToolEvents: []agent.ToolEvent{{
			Name: "mcp__memory__search_memories", Arguments: `{"query":"生日"}`, Result: "1 条",
		}},
		Steps: 2, StoppedBy: agent.StoppedByAnswered,
	}
	runner := &stubRunner{result: want}

	outcome := messages.NewHandler(runner).Ask(context.Background(), messages.AskInput{
		Channel: &fakeChannel{accountID: "acct-1"}, Message: textMessage("我生日"),
	})

	if outcome.Result.Steps != want.Steps || outcome.Result.StoppedBy != want.StoppedBy {
		t.Errorf("步数/停止原因没透传：%+v", outcome.Result)
	}
	if len(outcome.Result.UsedTools) != 1 || outcome.Result.UsedTools[0] != want.UsedTools[0] {
		t.Errorf("工具记录没透传：%+v", outcome.Result.UsedTools)
	}
	if len(outcome.Result.ToolEvents) != 1 || outcome.Result.ToolEvents[0].Arguments != `{"query":"生日"}` {
		t.Errorf("工具事件没透传：%+v", outcome.Result.ToolEvents)
	}
}
