package agent

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
