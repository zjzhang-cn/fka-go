// Package bot 是 iLink 协议的内部实现：**「iLink 这个协议怎么说话」**。
//
// 它只服务 iLink 这一种渠道，所以是渠道实现的内部——`internal/channels` 的接缝
// 不认识它，`internal/messages` 也不认识它。
//
// ## 线格式字段名一律 snake_case
//
// 这是从 Node 版踩坑踩出来的：服务端返回的是 `encrypt_query_param` / `aes_key`
// / `file_item.len`，按 camelCase 读**不会报错，只会静默读到 undefined**。
// 那是「长轮询正常、游标推进、却什么都收不到」的根因。所以这里用
// `Raw*` 前缀的结构体逐字照抄线上形状，`map[string]any` 只留给原文保真。
//
// ## uint64 一律按字符串处理
//
// 线上 `message_id` 是 uint64。JS 只能精确到 2^53，官方实现同样按字符串处理
// （"uint64 on the wire; parsed losslessly as a string"）。Go 里最容易犯的错是
// 直接反序列化成 `int64`/`float64`——后者在 id 超过 2^53 时**静默丢精度**，
// 而症状是「消息偶尔对不上号」。所以走 `json.RawMessage` 自己转，见 stringID。
package bot

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// ── 常量 ────────────────────────────────────────────────

// 消息项类型。
const (
	ItemTypeText  = 1
	ItemTypeImage = 2
	ItemTypeVoice = 3
	ItemTypeFile  = 4
	ItemTypeVideo = 5
)

// 消息归属：1=用户发出，2=Bot 发出。**出站必须是 2**。
const (
	MessageTypeUser = 1
	MessageTypeBot  = 2
)

// 消息状态：0=新建 1=生成中 2=已完成。**出站用 2**。
const (
	MessageStateNew        = 0
	MessageStateGenerating = 1
	MessageStateFinish     = 2
)

// ChannelVersion 所有 POST 请求体都要带的 base_info.channel_version。
const ChannelVersion = "1.0.0"

// RetSessionExpired session 过期。收到它需要重新扫码登录。
const RetSessionExpired = -14

// RetOK 成功。
const RetOK = 0

// IsSuccessRet 归一化 ret 字段。
//
// ## 为什么不能写 `ret == RetOK`
//
// **实测：服务端在成功时根本不返回 `ret` 字段**（响应体只有
// `msgs` / `sync_buf` / `get_updates_buf`），所以它是「缺席」而不是 0。
//
// 而 `nil` 缺席若被当成「不是 0」，会把**每一次成功响应都判成失败**——
// 于是长轮询正常、游标推进，却一条消息都收不到。
func IsSuccessRet(present bool, ret int) bool {
	return !present || ret == RetOK
}

// ── uint64 无损 ─────────────────────────────────────────

// stringID 把线上可能是数字、也可能已被引号化成字符串的 id 统一成字符串。
//
// 三种输入都要认：
//
//   - `"12345"`（带引号）—— 长 id 被服务端引号化；
//   - `12345`（裸数字）  —— 短 id；
//   - `""`               —— 缺失或空，**原样返回空串**，不编造。
//
// **不走 `json.Unmarshal` 到 int64**：那正是丢精度的地方；而 float64 更糟——
// 它连「丢了」都不告诉你。
func stringID(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	// `null` 也当缺席：它不是「id 字面量是 null」，而是**服务端没给这个字段**。
	// 照字面量返回 "null" 会造出一个看着像真 id 的假 id，而它会被拿去当主键。
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return ""
		}
		return text
	}
	// 裸数字：直接按字面量返回，**不经过 float64**
	return string(trimmed)
}

// optionalStringID 缺席时给「有没有」——因为「空的 id」与「没有 id」在这套协议里
// 是两回事（后者意味着这条 item 没带自己的 id）。
func optionalStringID(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", false
	}
	return stringID(raw), true
}

// stringOrInt64 读一个「协议里是字符串、但可能是数字」的字段（`file_item.len` 就是）。
//
// **空串一律当缺席**——协议里 `len: ""` 与「没给 len」对我们是同一件事：不知道多大。
func stringOrInt64(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", false
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return "", false
		}
		if text == "" {
			return "", false
		}
		return text, true
	}
	if _, err := strconv.ParseInt(string(trimmed), 10, 64); err != nil {
		return "", false
	}
	return string(trimmed), true
}

// ── 线格式（snake_case，逐字照抄）──────────────────────

// CDNMedia 媒体引用。
//
// **字段名来自真实消息实测**（2026-09-14，真实图片消息），不是推定。
type CDNMedia struct {
	// EncryptQueryParam 加密查询参数，拼进 CDN 下载 URL
	EncryptQueryParam string `json:"encrypt_query_param"`
	// AesKey 密钥的 base64 编码。解码后是 32 个十六进制字符，
	// **再按 hex 解码**才是 16 字节的 AES 密钥。见 crypto.go 的 ParseAesKey
	AesKey string `json:"aes_key,omitempty"`
	// EncryptType 加密类型。出站固定为 1
	EncryptType int `json:"encrypt_type,omitempty"`
	// FullURL 可直接 GET 的完整下载地址（实测入站消息提供，省去手工拼 URL）
	FullURL string `json:"full_url,omitempty"`
}

// ImageItem 图片项。实测 `aeskey` 与尺寸写在 image_item 层，media 里也有一份 aes_key。
type ImageItem struct {
	// Aeskey 32 个十六进制字符，按 hex 解码得 16 字节密钥
	Aeskey string   `json:"aeskey"`
	Media  CDNMedia `json:"media"`

	MidSize     int64 `json:"mid_size,omitempty"`
	ThumbSize   int64 `json:"thumb_size,omitempty"`
	ThumbHeight int64 `json:"thumb_height,omitempty"`
	ThumbWidth  int64 `json:"thumb_width,omitempty"`
	HDSize      int64 `json:"hd_size,omitempty"`
}

// VoiceItem 语音项。
type VoiceItem struct {
	Aeskey   string   `json:"aeskey,omitempty"`
	Media    CDNMedia `json:"media"`
	Duration int64    `json:"duration,omitempty"`
	// Text 语音转文字
	Text string `json:"text,omitempty"`
}

// FileItem 文件项。
//
// **与图片项的关键差异：文件项没有平铺的 `aeskey`**，密钥只能从 `media.aes_key`
// 取。大小字段是 `len`（**协议里是字符串**）而非 `size`。
type FileItem struct {
	Media    CDNMedia `json:"media"`
	FileName string   `json:"file_name,omitempty"`
	MD5      string   `json:"md5,omitempty"`
	// Len 明文字节数。**协议里是字符串**，实测入站消息也是字符串
	Len string `json:"len,omitempty"`
}

// VideoItem 视频项。
type VideoItem struct {
	Aeskey     string    `json:"aeskey,omitempty"`
	Media      CDNMedia  `json:"media"`
	ThumbMedia *CDNMedia `json:"thumb_media,omitempty"`
	Duration   int64     `json:"duration,omitempty"`
	Size       int64     `json:"size,omitempty"`
}

// PartialText 引用的是选中一段时，用来在原文里定位。
type PartialText struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	StartIdx int    `json:"startindex"`
	EndIdx   int    `json:"endindex"`
	QuoteMD5 string `json:"quotemd5"`
}

// RefMessage 被引用的那条消息。
//
// **三个字段是三种情况，不是三个都要有**：
//
//	| 字段          | 什么时候有                                  |
//	|---------------|---------------------------------------------|
//	| MessageItem   | 老客户端把被引用消息**整条带过来**            |
//	| Title         | 客户端给的摘要                               |
//	| SvrID         | **新版客户端只给这个**——正文得自己缓存过才还原 |
//	| PartialText   | 引用的是**选中一段**，靠 start/end 在原文里定位 |
//
// 所以只有 `svr_id` 时引用是**还原不出来**的，见 quote.go。
type RefMessage struct {
	MessageItem *MessageItem `json:"message_item,omitempty"`
	Title       string       `json:"title,omitempty"`
	SvrID       string       `json:"svr_id,omitempty"`
	PartialText *PartialText `json:"partial_text,omitempty"`
}

// MessageItem 消息里的一项。
type MessageItem struct {
	// Type 1=TEXT 2=IMAGE 3=VOICE 4=FILE 5=VIDEO
	Type int `json:"type"`
	// MsgID 这一项自己的 id。**字符串**，理由同 WeixinMessage.MessageID
	MsgID string `json:"msg_id,omitempty"`
	// RefMsg 这一项引用了别的消息时出现
	RefMsg *RefMessage `json:"ref_msg,omitempty"`
	Text   string      `json:"text,omitempty"`

	Image *ImageItem `json:"image_item,omitempty"`
	Voice *VoiceItem `json:"voice_item,omitempty"`
	File  *FileItem  `json:"file_item,omitempty"`
	Video *VideoItem `json:"video_item,omitempty"`
}

// RawItem 线上的 item 形状。
//
// **只列我们建模的字段**；其余（`is_completed`、`button_item_list`、`root_id`…）
// 不进类型，靠消息级原文保真。
type RawItem struct {
	Type int `json:"type"`
	// MsgID 这一项自己的 id。**必须走 stringID**，所以自己实现反序列化——
	// 直接声明成 string 的话，裸数字会先被解成 float64 而悄悄丢精度
	MsgID string `json:"msg_id,omitempty"`
	Text  *struct {
		Text string `json:"text,omitempty"`
	} `json:"text_item,omitempty"`
	Image *ImageItem `json:"image_item,omitempty"`
	Voice *VoiceItem `json:"voice_item,omitempty"`
	File  *FileItem  `json:"file_item,omitempty"`
	Video *VideoItem `json:"video_item,omitempty"`

	// RefMsg 走 RawMessage：`svr_id` 可能是数字也可能是字符串，在这一层还
	// 分不清，交给 toRefMessage 处理
	RefMsg json.RawMessage `json:"ref_msg,omitempty"`
}

// UnmarshalJSON 只为把 `msg_id` 交给 stringID。
func (i *RawItem) UnmarshalJSON(data []byte) error {
	type alias RawItem
	var shadow struct {
		*alias
		MsgID json.RawMessage `json:"msg_id"`
	}
	shadow.alias = (*alias)(i)

	if err := json.Unmarshal(data, &shadow); err != nil {
		return err
	}
	i.MsgID, _ = optionalStringID(shadow.MsgID)
	return nil
}

// RawMessage 一条消息的线上形状。
type RawMessage struct {
	Seq int64 `json:"seq"`
	// MessageID 线上是 uint64。声明成 string 并由 UnmarshalJSON 填，是为了
	// **无损**——直接解成数字会静默丢精度
	MessageID    string `json:"message_id"`
	FromUserID   string `json:"from_user_id"`
	ToUserID     string `json:"to_user_id"`
	SessionID    string `json:"session_id"`
	MessageType  int    `json:"message_type"`
	MessageState int    `json:"message_state"`
	// ContextToken 回复时必须回传
	ContextToken string `json:"context_token"`
	CreateTimeMs int64  `json:"create_time_ms"`

	ClientID     string `json:"client_id,omitempty"`
	UpdateTimeMs int64  `json:"update_time_ms,omitempty"`
	DeleteTimeMs int64  `json:"delete_time_ms,omitempty"`
	GroupID      string `json:"group_id,omitempty"`
	RunID        string `json:"run_id,omitempty"`

	// Items 对应线上的 `item_list`。走自定义反序列化，见 UnmarshalJSON
	Items []RawItem `json:"item_list,omitempty"`

	// OriginalJSON 收到时的原始字节。
	//
	// **刻意保留原文而不是重新序列化**：重新序列化会把没建模的字段全丢掉
	// （`is_completed`、`button_item_list`、`root_id`…），而保留它们正是存原文
	// 的目的——没建模的字段应该只是「我们暂时没读」，而不是「丢了」。
	OriginalJSON string `json:"-"`
}

// UnmarshalJSON 让 `message_id` 走 stringID。
//
// 单独写一个方法就是为了这件事：`message_id` 是 uint64，声明成 `int64` 或
// `float64` 都会在 id 超过 2^53 时**静默丢精度**——症状是「消息偶尔对不上号」，
// 而不是报错。`RawItem` 出于同样的理由也自己实现了反序列化。
func (m *RawMessage) UnmarshalJSON(data []byte) error {
	// 指向自身类型但去掉 UnmarshalJSON，避免无限递归
	type alias RawMessage
	var shadow struct {
		*alias
		MessageID json.RawMessage `json:"message_id"`
	}
	shadow.alias = (*alias)(m)

	if err := json.Unmarshal(data, &shadow); err != nil {
		return err
	}
	m.MessageID = stringID(shadow.MessageID)
	m.OriginalJSON = string(bytes.TrimSpace(data))
	return nil
}

// ── 消息模型 ────────────────────────────────────────────

// WeixinMessage 解析后的消息模型。
type WeixinMessage struct {
	Seq int64
	// MessageID 消息 ID。**字符串**，理由见包头
	MessageID string
	// FromUserID 发送者 ID
	FromUserID string
	// ToUserID 接收者 ID
	ToUserID string
	// SessionID 会话 ID
	SessionID string
	// MessageType 1=USER 2=BOT
	MessageType int
	// MessageState 0=NEW 1=GENERATING 2=FINISH
	MessageState int
	// ContextToken 上下文令牌（回复时必须回传）
	ContextToken string
	Items        []MessageItem
	// Quote 这条消息引用了别的消息时的上下文（parse 时就解析好，不在别处算）
	Quote *QuoteContext
	// CreateTimeMs 创建时间（毫秒）
	CreateTimeMs int64
	// AccountID 所属账号 ID
	AccountID string

	ClientID     string
	UpdateTimeMs int64
	DeleteTimeMs int64
	GroupID      string
	RunID        string

	// Raw 原始报文（JSON 文本）。**只在 worker 里填**，用于原样入库。
	//
	// 协议还在长——`ref_msg`、`partial_text`、`group_id` 都是后来才有的字段，
	// 而每加一个都得先改上面的映射才读得到。有了原文，**没建模的字段就只是
	// 我们暂时没读，而不是丢了**。
	Raw string
}

// Text 拼起所有文本 item 的正文。
//
// 归一化成 InboundMessage 时用它——**一条消息可能有多个 item**（图 + 文、
// 文件 + 说明），而提问只关心文字那部分。
func (m WeixinMessage) Text() string {
	var out string
	for _, item := range m.Items {
		if item.Text != "" {
			out += item.Text
		}
	}
	return out
}

// MediaItems 挑出所有媒体 item。**逐个返回而不是合并**——不同种类的媒体要发给
// 不同的 sender（图片走图片发送器、文件走文件发送器），合并就丢了这个信息。
func (m WeixinMessage) MediaItems() []MessageItem {
	var out []MessageItem
	for _, item := range m.Items {
		if item.Image != nil || item.Voice != nil || item.File != nil || item.Video != nil {
			out = append(out, item)
		}
	}
	return out
}
