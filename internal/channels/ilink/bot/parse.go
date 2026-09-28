package bot

import (
	"encoding/json"
	"fmt"
	"strings"
)

// iLink 报文的解析：**纯函数、无 IO**。
//
// ## 为什么单独一个模块
//
// 这段逻辑原先内联在长轮询循环里，于是它**没法离线测试**——唯一验证手段是真机
// 发消息。而「线格式字段名写错」的症状恰恰是**静默读到零值**（服务端是
// snake_case，按 camelCase 读不会报错，只会什么都读不到）：这是最需要测试的一类，
// 却最不适合靠真机碰运气。项目里已经踩过两次（`CDNMedia` 的字段名、
// `msg_id` / `ref_msg` 整个没被映射）。
//
// 抽出来之后：真实 fixture 直接喂给 ParseGetUpdates / ParseMessage 即可断言，
// 轮询循环只剩 goroutine 生命周期与事件上报。
//
// ## 字段名全部来自实测
//
// 这里的 `Raw*` 类型不是猜的：`file_item.len` 是**字符串**、`image_item.aeskey`
// 是平铺的 32 位 hex、`media.aes_key` 是 base64——都对应真机报文。

// GetUpdatesBatch 一轮 getupdates 的解析结果。
type GetUpdatesBatch struct {
	// Messages 解析后的消息，保持服务端给的顺序
	Messages []WeixinMessage
	// Cursor 新一轮的游标。服务端没给或类型不对时为空
	Cursor string
	// SessionExpired 服务端说这个账号的 session 过期了（`ret == -14`）
	SessionExpired bool
}

// ParseGetUpdates 解析一轮 getupdates 响应。
//
// ## 非零 ret 抛错，因为它是协议层面的异常
//
// 它不是「这一轮没消息」，调用方要么上报要么退避——**静默忽略正是过去
// 「出问题完全看不见」的原因**。`ret == -14` 是唯一不抛的失败：那是
// 「该重新扫码了」，是个正常状态。
func ParseGetUpdates(data []byte, accountID string) (GetUpdatesBatch, error) {
	var payload struct {
		Ret           *int            `json:"ret"`
		Msgs          []RawMessage    `json:"msgs"`
		GetUpdatesBuf json.RawMessage `json:"get_updates_buf"`
	}
	// 空响应体不该炸：服务端偶尔回空，而「空」等价于「这一轮没消息」
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &payload); err != nil {
			return GetUpdatesBatch{}, fmt.Errorf("getupdates 响应不是合法 JSON：%w", err)
		}
	}

	ret := 0
	if payload.Ret != nil {
		ret = *payload.Ret
	}

	// -14 单独处理：不是错误，是「凭证失效，去重新登录」
	if ret == RetSessionExpired {
		return GetUpdatesBatch{SessionExpired: true}, nil
	}

	// 用 IsSuccessRet 而非 `ret == RetOK`：成功时服务端**根本不返回 ret 字段**
	if !IsSuccessRet(payload.Ret != nil, ret) {
		return GetUpdatesBatch{}, fmt.Errorf("getupdates 返回 ret=%d：%s",
			ret, snippet(data))
	}

	batch := GetUpdatesBatch{SessionExpired: false}
	for _, raw := range payload.Msgs {
		batch.Messages = append(batch.Messages, ParseMessage(raw, accountID))
	}
	batch.Cursor = parseCursor(payload.GetUpdatesBuf)
	return batch, nil
}

// parseCursor 游标只在**类型确实是字符串**时才算数。
//
// 服务端没给、或给了一个数字时都当没有——游标错了会重放或漏消息，而那两种都
// 不该由「猜一个」来修。
func parseCursor(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed[0] != '"' {
		return ""
	}
	var cursor string
	if err := json.Unmarshal(raw, &cursor); err != nil {
		return ""
	}
	return cursor
}

// ParseMessage 一条线上消息 → 消息模型。
//
// 引用**在这里就解析掉**（ResolveQuote）：它是纯派生，挂在消息上随处可用，
// 免得每个消费者各自记得去 Items 里翻 RefMsg。
//
// ## 缺失的字段不编造
//
// 真缺了就让它缺着。填 0 或空串会让「这条消息不完整」变成查不出来的事——
// 而排查时你根本不知道该怀疑什么。
func ParseMessage(raw RawMessage, accountID string) WeixinMessage {
	message := WeixinMessage{
		Seq:         raw.Seq,
		MessageID:   raw.MessageID,
		FromUserID:  raw.FromUserID,
		ToUserID:    raw.ToUserID,
		SessionID:   raw.SessionID,
		MessageType: raw.MessageType,
		// 状态 2=FINISH 才是「一条完整消息」。中间态（1=生成中）要照收不误：
		// 收件人可能只发了半句就撤了，丢掉它就丢了用户真正说过的话
		MessageState: raw.MessageState,
		ContextToken: raw.ContextToken,
		CreateTimeMs: raw.CreateTimeMs,
		AccountID:    accountID,
		ClientID:     raw.ClientID,
		UpdateTimeMs: raw.UpdateTimeMs,
		DeleteTimeMs: raw.DeleteTimeMs,
		GroupID:      raw.GroupID,
		RunID:        raw.RunID,
	}

	for _, item := range raw.Items {
		message.Items = append(message.Items, toMessageItem(item))
	}

	// **原文照存**：协议每加一个字段都得先改上面的映射才读得到，而没建模的
	// 字段不该就这么丢掉。落库时进 raw 列
	message.Raw = marshalRaw(raw)

	message.Quote = ResolveQuote(message)
	return message
}

// toMessageItem 原始 item → 消息模型。
func toMessageItem(item RawItem) MessageItem {
	out := MessageItem{Type: item.Type, MsgID: item.MsgID}
	if item.Text != nil {
		out.Text = strings.TrimSpace(item.Text.Text)
	}
	out.Image = item.Image
	out.Voice = item.Voice
	out.File = item.File
	out.Video = item.Video

	if len(strings.TrimSpace(string(item.RefMsg))) > 0 {
		out.RefMsg = toRefMessage(item.RefMsg)
	}
	return out
}

// toRefMessage 被引用的那条消息。**只有客户端内联了内容时才有正文**。
func toRefMessage(raw json.RawMessage) *RefMessage {
	var shadow struct {
		Title       string          `json:"title"`
		SvrID       json.RawMessage `json:"svr_id"`
		MessageItem json.RawMessage `json:"message_item"`
		PartialText *PartialText    `json:"partial_text"`
	}
	if err := json.Unmarshal(raw, &shadow); err != nil {
		// 引用读不出来就当没有——**不让一条坏引用炸掉整条消息**，
		// 那样连正文一起丢了
		return nil
	}

	out := &RefMessage{
		Title:       shadow.Title,
		PartialText: shadow.PartialText,
	}
	if len(strings.TrimSpace(string(shadow.SvrID))) > 0 {
		out.SvrID = stringID(shadow.SvrID)
	}
	if len(strings.TrimSpace(string(shadow.MessageItem))) > 0 {
		var item RawItem
		if err := json.Unmarshal(shadow.MessageItem, &item); err == nil {
			out.MessageItem = &MessageItem{}
			*out.MessageItem = toMessageItem(item)
		}
	}
	return out
}

// marshalRaw 把原始报文照存成 JSON 文本。
//
// 直接重新序列化 `RawMessage` 是不行的——**那会把没建模的字段全丢掉**，
// 而保留它们正是存原文的目的。所以这里保留原始字节，只在必要时去掉 BOM。
func marshalRaw(raw RawMessage) string {
	return raw.OriginalJSON
}

// snippet 报错时带一段响应文本。**只截前 300 字节**，免得把整段响应灌进日志。
func snippet(data []byte) string {
	const limit = 300
	text := strings.TrimSpace(string(data))
	if len(text) > limit {
		return text[:limit] + "…"
	}
	return text
}
