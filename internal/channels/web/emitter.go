package web

// sseEmitter 把工具循环推来的过程事件转成 SSE 帧。
//
// ## 为什么 Answer 是空的
//
// 最终答案由消息层经渠道的 `Senders.Text` 送出（`channelReply`/`handler.reply`
// → `Senders.Text`），而工具循环同时也会调 `Emitter.Answer`。两者是**同一份文本**，
// 这里若也推一帧，浏览器就会看到答案两次。所以答案只走 `Senders.Text`（推
// `event: message`），本实现只负责**过程事件**：推理增量与工具调用。
type sseEmitter struct {
	hub          *hub
	conversation string
}

func (e *sseEmitter) Reasoning(text string) {
	e.hub.broadcast(e.conversation, "reasoning", map[string]any{"text": text})
}

func (e *sseEmitter) Tool(name, arguments, result string) {
	e.hub.broadcast(e.conversation, "tool", map[string]any{
		"name": name, "arguments": arguments, "result": result,
	})
}

// Answer 刻意空实现——见类型注释。
func (e *sseEmitter) Answer(string) {}
