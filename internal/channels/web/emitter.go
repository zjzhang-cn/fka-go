package web

import "github.com/zjzhang-cn/fka-go/internal/channels"

// sseEmitter 把工具循环推来的过程事件与轮末通知转成 SSE 帧。
//
// ## 为什么 Answer 是空的
//
// 最终答案由消息层经渠道的 `Senders.Text` 送出（`channelReply`/`handler.reply`
// → `Senders.Text`），而工具循环同时也会调 `Emitter.Answer`。两者是**同一份文本**，
// 这里若也推一帧，浏览器就会看到答案两次。所以答案只走 `Senders.Text`（推
// `event: message`），本实现只负责**过程事件**（推理增量与工具调用）与**用量通知**。
//
// ## 为什么通知（Notifier）长在这个逐条消息的回显对象上，而不是渠道上
//
// 用量属于**某一条会话**，而渠道实例是多会话共享的、没有「当前会话」可指；
// `sseEmitter` 是逐条消息造的（见 `Channel.Emitter`），它知道该发给哪个房间。
// 接缝的适配器（`messages.channelEcho`）先问渠道、再问这里的回显对象。
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

// Notify 收通知并推一帧给该会话：用量推 `usage`（键名完整）、执行中状态推 `turn`
// （`running` 布尔）。服务端给的数原样转发，不做本地估算。
func (e *sseEmitter) Notify(notice channels.Notice) {
	switch n := notice.(type) {
	case channels.UsageNotice:
		e.hub.broadcast(e.conversation, "usage", map[string]any{
			"promptTokens":     n.Usage.PromptTokens,
			"completionTokens": n.Usage.CompletionTokens,
			"totalTokens":      n.Usage.TotalTokens,
			"cachedTokens":     n.Usage.CachedTokens,
		})
	case channels.TurnStateNotice:
		e.hub.broadcast(e.conversation, "turn", map[string]any{"running": n.Running})
	}
}

var _ channels.Notifier = (*sseEmitter)(nil)
