package messages

import (
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/agent"
	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/llm"
)

// recording notifier，两个落点各一份：渠道自己、与默认回显。
//
// **嵌入接口、只实现 Notify**：`channelEcho.Notify` 只对这两个目标做类型断言、
// 不调它们的其它方法，所以嵌入一个 nil 接口当占位就够了。
type notifierChannel struct {
	channels.Channel
	got []channels.TokenUsage
}

func (n *notifierChannel) Notify(notice channels.Notice) {
	if usage, ok := notice.(channels.UsageNotice); ok {
		n.got = append(n.got, usage.Usage)
	}
}

type notifierEmitter struct {
	channels.Emitter
	got []channels.TokenUsage
}

func (n *notifierEmitter) Notify(notice channels.Notice) {
	if usage, ok := notice.(channels.UsageNotice); ok {
		n.got = append(n.got, usage.Usage)
	}
}

// Test通知_优先发给渠道本身 用量通知落到**渠道**上（CLI 走这条，渠道缓冲到答复后打）。
func Test通知_优先发给渠道本身(t *testing.T) {
	channel := &notifierChannel{}
	echo := channelEcho{channel: channel, inner: &notifierEmitter{}}

	echo.Notify(agent.UsageNotice{Usage: llm.TokenUsage{
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
	}})

	if len(channel.got) != 1 {
		t.Fatalf("渠道该收到一条通知，实际 %d", len(channel.got))
	}
	want := channels.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	if channel.got[0] != want {
		t.Errorf("用量搬运错了：%+v，想要 %+v", channel.got[0], want)
	}
}

// Test通知_退回默认回显 渠道本身不收通知时，退回默认回显（常驻渠道据此记日志）。
func Test通知_退回默认回显(t *testing.T) {
	fallback := &notifierEmitter{}
	echo := channelEcho{channel: nil, inner: fallback}

	echo.Notify(agent.UsageNotice{Usage: llm.TokenUsage{PromptTokens: 3, TotalTokens: 3}})

	if len(fallback.got) != 1 {
		t.Fatalf("默认回显该收到一条通知，实际 %d", len(fallback.got))
	}
}

// plainEmitter 只实现 channels.Emitter、不实现 Notifier。
type plainEmitter struct{ channels.Emitter }

// Test通知_两边都不收就静默 通知是尽力而为：没人收不报错、不 panic。
func Test通知_两边都不收就静默(t *testing.T) {
	echo := channelEcho{channel: nil, inner: plainEmitter{}}

	echo.Notify(agent.UsageNotice{Usage: llm.TokenUsage{PromptTokens: 1, TotalTokens: 1}})
}
