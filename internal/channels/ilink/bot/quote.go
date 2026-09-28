package bot

import "strings"

// 引用消息的解析。
//
// ## 引用有三种形态，能还原出来的只有前两种
//
//	| 客户端给了什么              | 能拿到什么                        |
//	|---------------------------|-----------------------------------|
//	| `message_item`（老客户端）  | **完整正文**                      |
//	| `title`（摘要）             | 摘要                              |
//	| **只有 `svr_id`**（新版）   | **只有 id，正文拿不到**            |
//	| `partial_text`（选中一段）  | 需要原文才能按 start/end 定位      |
//
// 所以**「解析引用」不是万能的**：新版微信只给服务端消息 ID，正文得在收到原消息
// 时就缓存过才还原得出来。那是另一套东西（旁路消息存储），本模块不做。
//
// 本模块只做**同步、无 IO** 的那一半：从 `ref_msg` 里能直接读到的就填上，
// 读不到就**如实地说读不到**，而不是编一个空的正文让上层以为引用是空的。
//
// > 字段语义与官方实现 `Tencent/openclaw-weixin` 的
// > `src/messaging/inbound.ts`（`findReferenceItem` / `inlineQuoteBody`）保持一致。
// > 协议细节以官方实现为准。

// QuoteContext 从一条消息里解析出来的引用上下文。
type QuoteContext struct {
	// ID 被引用消息的 id。优先服务端 id，退回 item 自己的 id
	ID string
	// IDKnown 这个 id 到底有没有拿到。「有 id 但没正文」与「什么都没有」
	// 是两种情况，上层要能分开说
	IDKnown bool
	// Body 被引用消息的正文。**只有客户端内联了内容时才有**
	Body string
	// BodyKnown 正文是不是真拿到了
	BodyKnown bool
	// PartialText 引用的是原文里的一段。要配合 Body 才能定位
	PartialText *PartialText
}

// FindReferenceItem 找出携带 `RefMsg` 的那个 item。
//
// 注意 `ref_msg` 挂在 **item** 上而不是消息外层——一条消息可以有多个 item，
// 引用只可能出现在其中一个上。
func FindReferenceItem(message WeixinMessage) *MessageItem {
	for i := range message.Items {
		if message.Items[i].RefMsg != nil {
			return &message.Items[i]
		}
	}
	return nil
}

// BodyOf 从 item 里挤出可读的正文。
//
// 按 item 类型取对应的文本：文本项取文本，文件项取文件名，以此类推——
// **引用一条文件消息时，用户看到的是文件名**，我们要能说出「你引用的是那份文件」。
func BodyOf(item MessageItem) (string, bool) {
	if trimmed := strings.TrimSpace(item.Text); trimmed != "" {
		return trimmed, true
	}
	if item.File != nil {
		if trimmed := strings.TrimSpace(item.File.FileName); trimmed != "" {
			return "[文件] " + trimmed, true
		}
	}
	if item.Image != nil {
		return "[图片]", true
	}
	if item.Voice != nil {
		return "[语音]", true
	}
	if item.Video != nil {
		return "[视频]", true
	}
	return "", false
}

// InlineQuoteBody 客户端内联的引用正文：**摘要 + 被引用消息本体**，两样都可能缺。
//
// 用 ` | ` 连接是照官方实现的做法：两者是同一件事的两个来源（摘要是服务端给的，
// 本体是客户端给的），拼起来信息最全，而分开看任一个都可能残缺。
func InlineQuoteBody(item MessageItem) (string, bool) {
	if item.RefMsg == nil {
		return "", false
	}

	var parts []string
	if trimmed := strings.TrimSpace(item.RefMsg.Title); trimmed != "" {
		parts = append(parts, trimmed)
	}
	if item.RefMsg.MessageItem != nil {
		if body, ok := BodyOf(*item.RefMsg.MessageItem); ok {
			parts = append(parts, body)
		}
	}

	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, " | "), true
}

// ResolveQuote 解析一条消息的引用上下文。没有引用时返回 nil。
//
// **正文拿不到时不编造**：返回的对象里 `BodyKnown` 是 false，调用方据此可以说
// 「你引用的那条我这边看不到内容」——那比回一句空白的引用诚实得多。
func ResolveQuote(message WeixinMessage) *QuoteContext {
	item := FindReferenceItem(message)
	if item == nil || item.RefMsg == nil {
		return nil
	}
	ref := item.RefMsg

	quote := &QuoteContext{}

	// 服务端 id 优先：新版客户端只给这个，而它是唯一跨消息稳定的标识
	id := strings.TrimSpace(ref.SvrID)
	if id == "" && ref.MessageItem != nil {
		id = strings.TrimSpace(ref.MessageItem.MsgID)
	}
	quote.ID = id
	quote.IDKnown = id != ""

	if body, ok := InlineQuoteBody(*item); ok {
		quote.Body = body
		quote.BodyKnown = true
	}

	quote.PartialText = ref.PartialText
	return quote
}

// IsQuoteResolved 引用内容能不能还原出来。
//
// 只有 id、没有正文时是**还原不出来**的——那需要本地消息缓存（本模块不做）。
// 上层据此决定措辞：说得清「看不到内容」比让用户以为 Bot 眼瞎好。
func IsQuoteResolved(quote *QuoteContext) bool {
	if quote == nil {
		return false
	}
	return quote.BodyKnown || !quote.IDKnown
}
