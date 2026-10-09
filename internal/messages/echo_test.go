// 渠道可插拔回显的用例：实现了 `channels.EmitterProvider` 就用渠道自己的，否则回落到
// 默认的 loggingEmitter——**消息层一行不用改**，这正是「加渠道只实现接口」那条。
package messages

import (
	"context"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/agent"
	"github.com/zjzhang-cn/fka-go/internal/channels"
)

// recordingEcho 记录收到的回显事件。
type recordingEcho struct {
	reasoning []string
	tools     [][3]string
	answers   []string
}

func (r *recordingEcho) Reasoning(text string) { r.reasoning = append(r.reasoning, text) }
func (r *recordingEcho) Tool(name, arguments, result string) {
	r.tools = append(r.tools, [3]string{name, arguments, result})
}
func (r *recordingEcho) Answer(text string) { r.answers = append(r.answers, text) }

// echoProviderChannel 嵌入 Channel 接口（其余方法本用例用不到），只补 EmitterProvider。
// 嵌入 nil 接口 + 只覆盖关心的方法，是 Go 里造最小假替身的常用写法。
type echoProviderChannel struct {
	channels.Channel
	echo channels.Emitter
}

func (c echoProviderChannel) Emitter(context.Context) channels.Emitter { return c.echo }

// plainChannel 不支持 EmitterProvider 的渠道。
type plainChannel struct{ channels.Channel }

// Test渠道回显_渠道可提供自己的Emitter 渠道实现 EmitterProvider 时，工具循环推来的
// 事件要送到**它**的实现上（经 channelEcho 适配）。
func Test渠道回显_渠道可提供自己的Emitter(t *testing.T) {
	rec := &recordingEcho{}
	emitter := emitterFor(context.Background(), echoProviderChannel{echo: rec})

	emitter.Reasoning("想")
	emitter.Tool(agent.ToolEvent{Name: "x", Arguments: "{}", Result: "r"})
	emitter.Answer("答")

	if len(rec.reasoning) != 1 || rec.reasoning[0] != "想" {
		t.Errorf("推理没送到渠道的 emitter：%v", rec.reasoning)
	}
	if len(rec.tools) != 1 || rec.tools[0] != [3]string{"x", "{}", "r"} {
		t.Errorf("工具事件没送到渠道的 emitter：%v", rec.tools)
	}
	if len(rec.answers) != 1 || rec.answers[0] != "答" {
		t.Errorf("答案没送到渠道的 emitter：%v", rec.answers)
	}
}

// Test渠道回显_没提供就用默认且不炸 普通渠道回落 loggingEmitter：推理/答案不回显、
// 工具只记日志，且各方法照调不 panic。
func Test渠道回显_没提供就用默认且不炸(t *testing.T) {
	emitter := emitterFor(context.Background(), plainChannel{})

	emitter.Reasoning("想")
	emitter.Tool(agent.ToolEvent{Name: "x", Arguments: "{}", Result: "r"})
	emitter.Answer("答")
}
