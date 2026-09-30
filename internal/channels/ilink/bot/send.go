package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// 消息发送。
//
// 协议约束：
//   - POST {baseUrl}/ilink/bot/sendmessage
//   - **必须回传入站消息的 context_token**——回复是「回复」，不是新会话
//   - 文本消息 type = 1
//
// ## 为什么出站要单独一个 client
//
// 发送要的东西与收不同：**带 client_id、带 content_token、判 ret**。
// 混进长轮询那个 client 会让两边都带上一半用不上的东西。
type sender struct {
	client *client
}

// NewSender 造发送器。**导出是因为渠道适配层要用**——它是「协议能发消息」
// 与「渠道有一个文本发送器」之间唯一的桥。
//
// httpClient 为 nil 时用默认实现。
func NewSender(account WeixinAccount, httpClient *http.Client) *sender {
	return &sender{client: NewClient(account, httpClient)}
}

// GenerateClientID 生成出站消息的 client_id。
//
// 协议要求每条出站消息带一个客户端生成的唯一 ID。**实测缺失它会得到
// `ret=-2 invalid arguments`**。格式沿用协议文档的
// `pinix-weixin:<毫秒时间戳>-<随机十六进制>`。
//
// 它也是**我们在服务端认下这条消息之前唯一的身份**——排查「那条到底发出去没有」
// 时，日志里的 client_id 与库里的对得上，中间那一段就接起来了。
func GenerateClientID() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// 读不到随机源时退回时间戳低位。**不失败**：client_id 只是个标识，
		// 为它让整条消息发不出去不值得
		return fmt.Sprintf("pinix-weixin:%d-00000000", time.Now().UnixMilli())
	}
	return fmt.Sprintf("pinix-weixin:%d-%s",
		time.Now().UnixMilli(), hex.EncodeToString(buf[:]))
}

// wireItem 线上 item 的形状。
//
// 与 MessageItem 的唯一差别：内容挂在 `*_item` 子对象上（`{type, text_item}`
// 对 `{type, text}`）。**这个差别是协议定的，不是我们选的**。
type wireItem struct {
	Type      int        `json:"type"`
	TextItem  *textItem  `json:"text_item,omitempty"`
	ImageItem *imageWire `json:"image_item,omitempty"`
	FileItem  *fileWire  `json:"file_item,omitempty"`
}

type textItem struct {
	Text string `json:"text"`
}

// imageWire 出站图片项。
//
// **图片项与文件项的关键差异：密钥要平铺在 `aeskey` 上**（hex，32 字符）。
// 文件项没有这个字段，密钥只能藏在 `media.aes_key` 里。实测入站图片就是这样。
type imageWire struct {
	Aeskey string   `json:"aeskey"`
	Media  CDNMedia `json:"media"`
	// MidSize 明文字节数。**只报 mid_size**：上传时固定 no_need_thumb
	// （只传原图、不做缩略图），所以没有 thumb_* / hd_size
	MidSize int64 `json:"mid_size"`
}

type fileWire struct {
	Media    CDNMedia `json:"media"`
	FileName string   `json:"file_name,omitempty"`
	MD5      string   `json:"md5,omitempty"`
	// Len 协议要求是**十进制字符串**
	Len string `json:"len,omitempty"`
}

// ToWireItem 解析后的 item → 线上 item。
//
// 内容一个字不改，只换挂载点。有了它，**落库那份（MessageItem）与发出去那份
// （item_list）就是同一个来源**，不会各写一遍然后慢慢走样。
func ToWireItem(item MessageItem) wireItem {
	out := wireItem{Type: item.Type}
	if item.Text != "" {
		out.TextItem = &textItem{Text: item.Text}
	}
	if item.Image != nil {
		out.ImageItem = &imageWire{
			Aeskey: item.Image.Aeskey,
			// media 里也留一份 base64 的，与入站形状一致
			Media: CDNMedia{
				EncryptQueryParam: item.Image.Media.EncryptQueryParam,
				AesKey:            item.Image.Media.AesKey,
				EncryptType:       1,
			},
			MidSize: item.Image.MidSize,
		}
	}
	if item.File != nil {
		out.FileItem = &fileWire{
			Media:    item.File.Media,
			FileName: item.File.FileName,
			MD5:      item.File.MD5,
			Len:      item.File.Len,
		}
	}
	return out
}

// OutboundMsg 出站报文的 `msg` 那一层——**发到线上的就是它**。
//
// 单独建模而不是就地拼字面量，是因为**落库要存的正是这个对象**，而且离线可测。
type OutboundMsg struct {
	// FromUserID 出站为空串，协议规定
	FromUserID string `json:"from_user_id"`
	ToUserID   string `json:"to_user_id"`
	ClientID   string `json:"client_id"`
	// MessageType 出站必须是 2（Bot 发出）
	MessageType int `json:"message_type"`
	// MessageState 出站用 2（FINISH）
	MessageState int `json:"message_state"`
	// ContextToken 回复时必须原样回传
	ContextToken string     `json:"context_token"`
	ItemList     []wireItem `json:"item_list"`
	RunID        string     `json:"run_id,omitempty"`
}

// baseInfo 所有 POST 请求体都要带的那一层。
type baseInfo struct {
	ChannelVersion string `json:"channel_version"`
}

// BuildOutboundMsgParams 构造出站报文的参数。
type BuildOutboundMsgParams struct {
	ToUserID     string
	ContextToken string
	ClientID     string
	Items        []MessageItem
	RunID        string
}

// BuildOutboundMsg 构造一条出站报文。**纯函数**，可以脱网测试。
//
// 所有消息类型共用它：图片、语音、视频将来只是传不同的 items 进来，
// 信封字段只有这一处会写，不会各写一份然后漏掉某个字段
// （漏字段的症状是 `ret=-2 invalid arguments`，不指向具体缺了哪一个）。
func BuildOutboundMsg(params BuildOutboundMsgParams) OutboundMsg {
	msg := OutboundMsg{
		FromUserID:   "",
		ToUserID:     params.ToUserID,
		ClientID:     params.ClientID,
		MessageType:  MessageTypeBot,
		MessageState: MessageStateFinish,
		ContextToken: params.ContextToken,
		RunID:        params.RunID,
	}
	for _, item := range params.Items {
		msg.ItemList = append(msg.ItemList, ToWireItem(item))
	}
	return msg
}

// SendResult 一次发送的结果。
type SendResult struct {
	// MessageID 服务端返回的消息 ID
	MessageID string
	// Raw 原始响应。**出站方向的「报文真相」**，排障用
	Raw json.RawMessage
}

// 发送超时。分三档是因为它们的风险差一个量级：文本是秒级交互，
// 媒体要走一次上传，60s 起步。
const (
	textTimeout      = 15 * time.Second
	referenceTimeout = 20 * time.Second
	mediaTimeout     = 60 * time.Second
)

// PostMessage 把一条已经构造好的报文发出去。
//
// 只管发：鉴权、判 `ret`。**失败一律返错**——静默失败会让上层以为消息已送达。
func (s *sender) PostMessage(ctx context.Context, msg OutboundMsg, what string,
	timeout time.Duration) (SendResult, error) {

	data, err := s.client.postJSON(ctx, "/ilink/bot/sendmessage", struct {
		Msg      OutboundMsg `json:"msg"`
		BaseInfo baseInfo    `json:"base_info"`
	}{msg, baseInfo{ChannelVersion}}, timeout)
	if err != nil {
		return SendResult{}, fmt.Errorf("发送%s失败：%w", what, err)
	}

	// **失败也要带应答**：失败时的 ret / errmsg 正是排查要用的东西，
	// 而它若只随异常抛出去、异常又被 catch 掉，就什么都不剩了
	if err := checkRet(data, "发送"+what); err != nil {
		return SendResult{Raw: data}, err
	}

	// message_id 走 stringID：它是 uint64，解成数字会丢精度
	var payload struct {
		MessageID json.RawMessage `json:"message_id"`
		MsgID     json.RawMessage `json:"msg_id"`
	}
	// 解析失败**必须报错**：以前这里是 `_ = json.Unmarshal(...)`，解不出来就当作
	// 没有 id，于是「发送成功」的空 SendResult 顺着往上走
	if err := json.Unmarshal(data, &payload); err != nil {
		return SendResult{Raw: data}, fmt.Errorf("发送%s失败：解析应答失败：%w", what, err)
	}

	id := stringID(payload.MessageID)
	if id == "" {
		id = stringID(payload.MsgID)
	}
	// **成功必须带回一个 id**：拿不到就说明这不是一次成功的发送（协议变了，或者
	// 应答根本不是我们的服务端）。返回空 id + nil error 会让上层以为消息已送达
	// ——那正是本文件开头声明绝不允许的事。
	if id == "" {
		return SendResult{Raw: data}, fmt.Errorf("发送%s失败：应答里没有 message_id / msg_id：%s",
			what, snippet(data))
	}
	return SendResult{MessageID: id, Raw: data}, nil
}

// check 三个必填前置条件。**在发请求之前拒绝**，而不是发一个空 token 的请求
// 让服务端回 `ret=-2` ——那样只得到一个不指向根因的错误。
func checkSendPreconditions(toUserID, contextToken, what string) error {
	if contextToken == "" {
		return fmt.Errorf("contextToken 缺失：回复必须回传入站消息的 context_token")
	}
	if toUserID == "" {
		return fmt.Errorf("toUserId 缺失：无法确定%s的回复目标", what)
	}
	return nil
}

// SendText 发送文本消息。
func (s *sender) SendText(ctx context.Context, toUserID, contextToken, text string) (SendResult, error) {
	if err := checkSendPreconditions(toUserID, contextToken, "文本"); err != nil {
		return SendResult{}, err
	}
	if text == "" {
		return SendResult{}, fmt.Errorf("text 为空：拒绝发送空消息")
	}

	msg := BuildOutboundMsg(BuildOutboundMsgParams{
		ToUserID:     toUserID,
		ContextToken: contextToken,
		ClientID:     GenerateClientID(),
		Items:        []MessageItem{{Type: ItemTypeText, Text: text}},
	})
	return s.PostMessage(ctx, msg, "文本", textTimeout)
}

// SendFileReference 通过**引用已有的 CDN 资源**发送文件。
//
// 与 SendText 的区别：不重新上传，把一个已知的 CDNMedia 塞进 file_item。
//
// ⚠️ **该路径的可行性尚在验证中。** 2026-09-14 实测：服务端接受请求并返回
// `message_id`，但**不投递**。官方协议文档也只描述「先上传再发送」这一条路。
// 所以正式发文件走 SendFile（先上传），这里保留是因为它是那条路的一环。
func (s *sender) SendFileReference(ctx context.Context, toUserID, contextToken string,
	media CDNMedia, fileName, md5 string, length int) (SendResult, error) {

	if err := checkSendPreconditions(toUserID, contextToken, "文件引用"); err != nil {
		return SendResult{}, err
	}
	if media.EncryptQueryParam == "" {
		return SendResult{}, fmt.Errorf("media 缺少 encrypt_query_param，无法构造文件引用")
	}

	msg := BuildOutboundMsg(BuildOutboundMsgParams{
		ToUserID:     toUserID,
		ContextToken: contextToken,
		ClientID:     GenerateClientID(),
		Items: []MessageItem{{
			Type: ItemTypeFile,
			File: &FileItem{
				Media:    media,
				FileName: fileName,
				MD5:      md5,
				// 协议要求 len 是十进制字符串
				Len: strconv.Itoa(length),
			},
		}},
	})
	return s.PostMessage(ctx, msg, "文件引用", referenceTimeout)
}

// maxSendBytes 一次发送的文件大小上限。
//
// ## 为什么必须有
//
// 整个文件要先读进内存、再加密（又是一份），**路径还是模型填的工具参数**——
// 一句「把那个 4GB 的视频发我」就能把进程打爆，而失败形态是 OOM kill：连一条
// 日志都不会留下（那正是本仓库最讨厌的那种失败）。
//
// **是 var 而不是 const**：用例要把它调小，否则验一次超限得造 64MB 的文件
// ——那比这条用例要防的问题还费资源。
var maxSendBytes int64 = 64 << 20

// readCapped 读一个本地文件，**超过上限就直接拒**。
//
// 先看大小再读，而不是「读完再看长度」：目的是**不让那份内存被分配出去**。
// stat 之后文件被追加的情况由 LimitReader 兜住。
func readCapped(path string, max int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("是个目录，不是文件：%s", path)
	}
	if info.Size() > max {
		return nil, fmt.Errorf("文件 %d 字节，超过上限 %d 字节", info.Size(), max)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("文件在读取过程中变大，超过上限 %d 字节", max)
	}
	return data, nil
}

// SendFile 发送本地文件。完整路径：**先上传到 CDN，再引用返回的下载参数**。
//
// 为什么不直接引用入站消息里的 CDN 资源：**实测不可行**（服务端收下但不投递）。
func (s *sender) SendFile(ctx context.Context, toUserID, contextToken, localPath, fileName string) (SendResult, error) {
	if err := checkSendPreconditions(toUserID, contextToken, "文件"); err != nil {
		return SendResult{}, err
	}

	data, err := readCapped(localPath, maxSendBytes)
	if err != nil {
		return SendResult{}, fmt.Errorf("读取待发送文件失败（%s）：%w", localPath, err)
	}
	if fileName == "" {
		fileName = filepath.Base(localPath)
	}

	// 官方：file_item.len 是十进制字符串。md5 **不在 file_item 里**，
	// 它只用于 getuploadurl 的 rawfilemd5
	uploaded, err := UploadMedia(ctx, s.client, toUserID, data, UploadMediaFile, mediaTimeout)
	if err != nil {
		return SendResult{}, fmt.Errorf("上传待发送文件失败（%s）：%w", fileName, err)
	}

	return s.SendFileReference(ctx, toUserID, contextToken,
		uploaded.Media, fileName, "", len(data))
}

// SendImage 发送本地图片。
//
// 与 SendFile 同一条路（上传 → 引用），只有两处不同：
//
//  1. 上传时声明 media_type = 1（图片）而不是 3；
//  2. item 用 image_item，且**密钥要平铺在 aeskey 上**（hex，32 字符）。
//
// ⚠️ **图片这条路尚未经真机验证**：文件那条已实测往返成功；图片只做到
// 「形状与入站一致 + 离线覆盖请求体」。
func (s *sender) SendImage(ctx context.Context, toUserID, contextToken, localPath, fileName string) (SendResult, error) {
	if err := checkSendPreconditions(toUserID, contextToken, "图片"); err != nil {
		return SendResult{}, err
	}

	data, err := readCapped(localPath, maxSendBytes)
	if err != nil {
		return SendResult{}, fmt.Errorf("读取待发送图片失败（%s）：%w", localPath, err)
	}
	if fileName == "" {
		fileName = filepath.Base(localPath)
	}

	uploaded, err := UploadMedia(ctx, s.client, toUserID, data, UploadMediaImage, mediaTimeout)
	if err != nil {
		return SendResult{}, fmt.Errorf("上传待发送图片失败（%s）：%w", fileName, err)
	}

	msg := BuildOutboundMsg(BuildOutboundMsgParams{
		ToUserID:     toUserID,
		ContextToken: contextToken,
		ClientID:     GenerateClientID(),
		Items: []MessageItem{{
			Type: ItemTypeImage,
			Image: &ImageItem{
				// 平铺的 hex 密钥：图片项独有
				Aeskey:  uploaded.Aeskey,
				Media:   uploaded.Media,
				MidSize: int64(len(data)),
			},
		}},
	})
	return s.PostMessage(ctx, msg, "图片", mediaTimeout)
}
