package agent

import "github.com/zjzhang-cn/fka-go/internal/llm"

// Emitter 是一轮问答里「回显」的接收者。
//
// ## 为什么要有这一层
//
// 三个前端要看的**是同一批事件**——推理增量、每次工具调用、最终答案——但显示方式
// 完全不同：`chat` 画带色的一行行与转圈，`ask` 打 `[推理]/[工具]/[助手]`，渠道只记
// 日志、只把答复发回去。把「事件是什么」收在这里，「显不显示、怎么显示」交给各自的
// 实现，加一个前端时工具循环一行不改。
//
// ## 为什么是方法不是 `Emit(Event)`
//
// 三类事件的载荷不同（推理是一小段文字、工具是三样、答案是全文），各自一个方法比
// 塞进一个带 `Kind` 的联合体更好读，实现方也不会漏掉某种事件。
//
// **nil = 不回显**。
type Emitter interface {
	// Reasoning 推理增量（模型按块吐，一次一小段）。
	Reasoning(text string)
	// Tool 一次工具调用已完成：名字、原样 JSON 参数、结果。
	Tool(event ToolEvent)
	// Answer 最终答案全文。
	Answer(text string)
}

// emitterWriter 把「推理增量」转成 Emitter 事件：模型层按块 `Write`，这里逐块上抛。
// 它由工具循环在调模型前经 `llm.WithReasoningWriter` 绑进 ctx。
type emitterWriter struct{ emitter Emitter }

func (w emitterWriter) Write(p []byte) (int, error) {
	w.emitter.Reasoning(string(p))
	return len(p), nil
}

// Notice 一条发给前端的**非过程通知**——一轮的账目（目前只有用量），不是过程事件。
//
// ## 为什么与 `Emitter` 分开，且是可选接口
//
// `Emitter` 是一轮内的过程事件流（增量、可能很多次）；用量是轮末才有定论的账目。
// 做进 `Emitter` 会让每个实现都被迫补一个多数用不上的方法，而**接口的宽度就是替换
// 的成本**。所以通知走**可选接口** `Notifier`，前端用类型断言探测。
//
// ## 为什么这里也有一份（而不是复用 channels.Notice）
//
// 工具循环不认识渠道接缝——`channels` 那一侧是同一个形状的**另一份**，由消息层的
// 适配器（`channelEcho`）搬运。这与 `ToolEvent` 之于 `channels.Emitter.Tool` 是同
// 一条规矩：接缝与 agent 互不认识。
//
// **密封接口**：只有本包能实现，`Notify` 的实现方可以放心断言、未知类型静默忽略。
type Notice interface {
	notice()
}

// UsageNotice 一轮结束的 token 用量（服务端报的整轮之和）。零值不发。
type UsageNotice struct{ Usage llm.TokenUsage }

func (UsageNotice) notice() {}

// Notifier **可选**能力：前端想收非过程通知就实现它。用类型断言探测，不实现就不发。
type Notifier interface {
	Notify(notice Notice)
}

// emitUsage 把一轮用量通知给前端（若它实现了 `Notifier`）。**零值不发**——
// 服务端没给就没有这条通知。
func emitUsage(emitter Emitter, usage llm.TokenUsage) {
	if emitter == nil || usage.IsZero() {
		return
	}
	if notifier, ok := emitter.(Notifier); ok {
		notifier.Notify(UsageNotice{Usage: usage})
	}
}
