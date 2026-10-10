// Package channels 是渠道抽象层：**业务层与「消息怎么通信」之间的那条线**。
//
// 判据是：接入第二个渠道（Telegram、飞书、企业微信…）时，**业务层改动 0 行**。
// 业务代码只认这里的 InboundMessage 与 Channel，不认识任何具体渠道。
//
// ## 刻意不做的事
//
// 入站防抖、@提及、访问控制、送达回执、流式草稿——家用/单聊场景用不上，
// 契约的复杂度应该匹配使用方需要的复杂度。
//
// ## Go 与 Node 版的一处不同：一致性检查从编译期挪到注册时
//
// Node 版有 `defineChannel()`：能力声明与发送器的一致性由**类型系统**保证
// （`SendersFor<C>` 按 `capabilities` 算出哪些发送器必填，缺一个编译不过）。
//
// Go 做不到这一点——**接口没法按另一个字段的值推必填项**。所以这里改成
// ValidateChannel：注册渠道时逐项核对，对不上就**拒绝启动**并指名缺哪个。
// 这不是「防御性编程」，它是那个编译期保证在 Go 里的等价物：两者都在「跑起来
// 之前」把不一致的渠道拦下来，区别只在拦住它的是编译器还是这个函数。
package channels

import (
	"context"
	"errors"
	"fmt"
)

// ErrorKind 渠道错误的分类。
//
// **三类合并成一个类型 + 一个 kind**，而不是三个结构体：它们除了 kind 之外完全
// 一样，而分开写就要写三遍 `Error()`/`Hint` 的取用逻辑。调用方 switch kind 即可。
type ErrorKind string

const (
	// KindGeneric 渠道接缝的基错误
	KindGeneric ErrorKind = "channel"
	// KindAddress 寻址失败。**都是用户可解决的**（去发条消息、指定收件人、换个渠道），
	// 所以要带 Hint 说**怎么办**
	KindAddress ErrorKind = "address"
	// KindLogin 交互式登录的用户可解决失败：二维码过期、账号槽位满了……
	//
	// **接缝不认识控制面（IPC）**：渠道抛它，控制面收到后翻成传输层的失败。
	// 渠道只描述「怎么了 + 怎么办」，怎么送过传输层是控制面的事。
	KindLogin ErrorKind = "login"
)

// Error 渠道错误。Hint 说**怎么办**——只报错不指路等于没帮上忙。
type Error struct {
	Kind    ErrorKind
	Message string
	Hint    string
}

func (e *Error) Error() string { return e.Message }

// Errorf 造一个渠道错误。
func Errorf(kind ErrorKind, hint, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...), Hint: hint}
}

// AddressError 寻址失败。
func AddressError(hint, format string, args ...any) *Error {
	return Errorf(KindAddress, hint, format, args...)
}

// LoginError 交互式登录失败。
func LoginError(hint, format string, args ...any) *Error {
	return Errorf(KindLogin, hint, format, args...)
}

// ErrorKindOf 取错误的分类。不是渠道错误时返回空串。
func ErrorKindOf(err error) ErrorKind {
	var channelErr *Error
	if errors.As(err, &channelErr) {
		return channelErr.Kind
	}
	return ""
}

// MediaKind 消息种类。入站的 Part 与出站的 Senders 共用这套键，
// 能力表也按它逐项声明——**收发两侧用的是同一组名字**。
type MediaKind string

const (
	KindText  MediaKind = "text"
	KindFile  MediaKind = "file"
	KindImage MediaKind = "image"
	KindVoice MediaKind = "voice"
	KindVideo MediaKind = "video"
)

// KindCapability 一种消息种类的收发能力。
//
// Send 为真时**必须**提供对应发送器（ValidateChannel 核对）；Receive 为真时渠道
// **声明**能产出这种入站 part——接缝看不到 provider 的解析实现，所以这条只能由
// 一致性测试守。
type KindCapability struct {
	Send    bool
	Receive bool
}

// Capabilities 渠道能力声明。**收发对称**。
//
// 声明为 false 的方向，调用方必须降级（SenderFor 返回零值 + ok=false）。
type Capabilities struct {
	Text  KindCapability
	File  KindCapability
	Image KindCapability
	Voice KindCapability
	Video KindCapability
	// ProactivePush 主动推送（无需入站消息即可发起会话）。
	// 它不是一种消息，故与上面并列。
	ProactivePush bool
}

// For 取某一种类的能力。
func (c Capabilities) For(kind MediaKind) KindCapability {
	switch kind {
	case KindText:
		return c.Text
	case KindFile:
		return c.File
	case KindImage:
		return c.Image
	case KindVoice:
		return c.Voice
	case KindVideo:
		return c.Video
	}
	return KindCapability{}
}

// MediaRef 入站媒体句柄。**不含字节**——如何取回是渠道的责任。
//
// 不带字节是有意的：消息可能在收到后很久才被处理（排队解析），若带着整份字节，
// 内存会被大文件占满。系统决定何时取、是否取；取回的入口是 `MediaFetcher`。
type MediaRef struct {
	// ID 渠道内的媒体标识。对调用方不透明，只能交回 Channel.FetchMedia
	ID string
	// FileName 文件名（渠道提供时）
	FileName string
	// MimeType MIME 类型（渠道提供时）
	MimeType string
	// Size 字节数（渠道提供时）
	Size int64
	// Checksum 渠道提供的完整性校验值
	Checksum string
}

// PartialText 引用的是原文里的一段。要配合 QuotedMessage.Body 才能定位。
type PartialText struct {
	Start    string
	End      string
	StartIdx int
	EndIdx   int
	QuoteMD5 string
}

// QuotedMessage 被引用的那条消息。正文可能拿不到——渠道通常只在收到时缓存，
// 而引用可能发生在很久之后。**缺正文必须暴露成「未解析」而不是编一个空串**。
type QuotedMessage struct {
	// ID 被引用消息的 id。**引用还原全靠它**
	ID string
	// Body 被引用消息的正文（渠道内联了内容时才有）
	Body string
	// PartialText 引用的是原文里的一段
	PartialText *PartialText
}

// Part 入站消息的一部分。
//
// **用结构体 + Kind 而不是接口**：一份入站消息通常只有一两个 part，用接口就得
// 为每种类型一个包装类型，扫起来比看一眼 Kind 费劲。要的字段是零值即「没有」。
type Part struct {
	Kind  MediaKind
	Text  string
	Media MediaRef
	// DurationMs 语音/视频时长
	DurationMs int64
	// Transcript 语音转文字（渠道提供时）
	Transcript string
}

// TextPart 造一段文本 part。
func TextPart(text string) Part { return Part{Kind: KindText, Text: text} }

// InboundMessage 归一化后的入站消息。所有渠道都产出这个形状。
type InboundMessage struct {
	// ChannelID 渠道标识，如 ilink
	ChannelID string
	// AccountID 渠道内的账号标识（一个渠道可有多个账号）
	AccountID string
	// MessageID 渠道内的消息 ID
	MessageID string
	// SenderID 发送者标识（**渠道内**的稳定 ID）
	SenderID string
	// PrincipalID 发送者的**全局唯一身份**。业务层认这个，不认 SenderID。
	//
	// 存在的理由：SenderID 只在渠道内唯一。它被当成三件事用——文件属主、检索时的
	// 提问者、管理员判据——而这三件都要求**全局唯一**。接第二个渠道时 `12345` 撞上
	// 某个身份标识就是权限穿透。
	//
	// 约定：**不透明**（业务层只比较不解析）、**必须全局唯一**（跨渠道由渠道保证）、
	// 需要命名空间时用 `telegram:12345` 这种形式。
	PrincipalID string
	// RecipientID 接收者标识（渠道内的稳定 ID，即 Bot 自己在这个渠道里的身份）
	RecipientID string
	// SenderName 发送者显示名。**仅用于展示，不可用于鉴权**
	SenderName string
	// ConversationID 归属会话（私聊即对方的 ID）
	ConversationID string
	// ReplyToken 回复令牌。回复必须是回复，因此这个值随入站消息而来。
	// 若渠道支持主动推送，主动发送时不用它。
	ReplyToken string
	Parts      []Part
	// Quoted 这条消息引用了别的消息时的上下文
	Quoted *QuotedMessage
	// Timestamp 毫秒时间戳
	Timestamp int64
	// Raw 渠道原始消息，仅用于调试与排障
	Raw any
}

// Text 拼起所有文本 part。便捷方法。
func (m *InboundMessage) Text() string {
	out := ""
	for _, part := range m.Parts {
		if part.Kind == KindText {
			out += part.Text
		}
	}
	return out
}

// SessionKeyOf 一条入站消息的**全局会话键**：`渠道:账号:会话`。
//
// 三个字段缺一不可：ConversationID 只在渠道内唯一，而账号标识在渠道之间也可能撞上。
func SessionKeyOf(message *InboundMessage) string {
	return message.ChannelID + ":" + message.AccountID + ":" + message.ConversationID
}

// SendTarget 出站目标。
type SendTarget struct {
	// ConversationID 发给哪个会话（私聊即对方的 ID）
	ConversationID string
	// ReplyToken 回复入站消息时必填；主动推送时留空，
	// 且要求渠道声明 Capabilities.ProactivePush
	ReplyToken string
}

// SendTextParams 发文本。
type SendTextParams struct {
	Target SendTarget
	Text   string
}

// SendMediaParams 发媒体。
//
// **两种给字节的方式**：本地路径（Path）或直接字节（Data）。二者都给时以 Data 为准。
//
// Data 是为**远端沙盒**留的：SSE 访问的 MCP server 在别的机器上，文件没有本地路径，
// 只能以字节穿过 MCP 到达 agent，再交给这里。stdio 的本机文件仍走 Path，省一次读。
type SendMediaParams struct {
	Target SendTarget
	// Path 本地路径。**Data 为空时**按它读文件
	Path string
	// Data 直接给出的字节。**非 nil 优先**——给了它就不再读 Path
	Data []byte
	// FileName 收件人看到的文件名
	FileName string
	// MimeType 类型。Path 方式可为空（渠道自行判断）
	MimeType string
}

// SendResult 一次发送的结果。
type SendResult struct {
	// MessageID 渠道返回的消息 ID
	MessageID string
	// Raw 渠道原始报文。出站方向的「报文真相」，日志与排障用
	Raw any
}

// Senders 出站处理器。字段存在即可用；与 Capabilities 里的同名布尔值必须一致
// （ValidateChannel 核对）。
type Senders struct {
	Text  func(ctx context.Context, p SendTextParams) (SendResult, error)
	File  func(ctx context.Context, p SendMediaParams) (SendResult, error)
	Image func(ctx context.Context, p SendMediaParams) (SendResult, error)
	Voice func(ctx context.Context, p SendMediaParams) (SendResult, error)
	Video func(ctx context.Context, p SendMediaParams) (SendResult, error)
}

// For 取某一种类的发送器。第二个返回值 false 表示**不支持**——
// 那是正常路径而不是错误：调用方换成「把图片当文件发」或回一句说明，
// 都比抛异常合适。
func (s Senders) For(kind MediaKind) (func(context.Context, SendMediaParams) (SendResult, error), bool) {
	switch kind {
	case KindFile:
		return s.File, s.File != nil
	case KindImage:
		return s.Image, s.Image != nil
	case KindVoice:
		return s.Voice, s.Voice != nil
	case KindVideo:
		return s.Video, s.Video != nil
	}
	return nil, false
}

// HasText 文本发送器。**单独给一个方法**，因为它的参数类型不一样
func (s Senders) HasText() bool { return s.Text != nil }

// Status 渠道运行状态。
type Status string

const (
	StatusOffline Status = "offline"
	StatusOnline  Status = "online"
	StatusExpired Status = "expired"
)

// ResolveAddressParams 用户在命令行给出的寻址选择器。渠道按自己的状态解释它们。
type ResolveAddressParams struct {
	// To 收件人。不给就用「最近一次入站」推断（渠道支持的话）
	To string
	// Token 显式指定的令牌，覆盖自动取到的
	Token string
}

// OutboundAddress 出站地址：发给哪个会话、带什么令牌。
//
// ReplyToken 是可选的——**只有需要它的渠道才有**（iLink 的 context_token：
// 协议没有「主动开启会话」的接口，token 只在收到消息时产生）。
type OutboundAddress struct {
	ConversationID string
	ReplyToken     string
}

// ── 渠道的角色接口 ──────────────────────────────────────
//
// ## 为什么拆开，而不是一个大接口
//
// 一条判据：**每个方法都要能回答「谁在调它」**。以前那个 11 方法的 `Channel` 把
// 五件事揉在一起，于是有两类问题：
//
//   - 真实消费方各只用一个子集（消息层只要身份与发送器、接缝的起停只要
//     Start/Stop），而接口宽度是**替换的成本**——测试假替身被迫为用不到的方法写桩，
//     新渠道也是；
//   - **可选能力混在必填里**时没有别的表达方式，只能让实现方返回一套「不支持」的
//     零值语义。而「接口方法不存在『可以不实现』」——那个 `ok=false` 就是这条矛盾
//     的产物。
//
// 所以：必填的三组用嵌入拼成 `Channel`；可选的两件事各自是一个接口，调用方用
// **类型断言**问「你支不支持」。

// Identity 渠道实例的身份：日志、错误提示、账号唯一性都靠它。
type Identity interface {
	// ID 渠道类型标识
	ID() string
	// Label 人类可读名称，用于日志
	Label() string
	// AccountID 本实例的账号标识
	AccountID() string
	// Status 渠道运行状态
	Status() Status
}

// Outbound 出站能力：能力声明与发送器**必须成对**（ValidateChannel 核对）。
type Outbound interface {
	// Capabilities 能力声明
	Capabilities() Capabilities
	// Senders 出站处理器，与 Capabilities 一一对应
	Senders() Senders
}

// Lifecycle 收发的起停。实现方负责自身的重连与游标管理。
type Lifecycle interface {
	// Start 开始接收消息
	Start(ctx context.Context, onMessage func(InboundMessage)) error
	// Stop 停止接收，释放资源。调用后不得再触发 onMessage
	Stop(ctx context.Context) error
}

// Channel 一个渠道实例**必须**做到的事。
type Channel interface {
	Identity
	Outbound
	Lifecycle
}

// AddressResolver **可选**能力：能自己推断「发给谁」。
//
// 有状态的渠道实现它（iLink：回复必须回传收到消息时的 `context_token`）；无状态的
// 渠道不实现——调用方用类型断言区分两件事：
//
//   - 「这个渠道不支持推断」= 断言失败，调用方要求显式给收件人；
//   - 「支持推断，但这次推不出来」= 返回 error（例如还没收到过任何消息）。
//
// 这两件事以前挤在同一个 `ok=false` 里。
type AddressResolver interface {
	ResolveAddress(params ResolveAddressParams) (OutboundAddress, error)
}

// MediaFetcher **可选**能力：能取回入站媒体的字节。
//
// ⚠️ **目前没有生产调用方**：消息层收到媒体消息会如实拒答（这个 agent 不拥有存储，
// 收到文件也没地方归档，见 `internal/messages` 包头）。留着这个接口，是因为
// 「入站媒体唯一的落地方式」必须有个明确的位置——要读图时接的就是这里，
// 而不是让每个渠道都写一个返回错误的 `FetchMedia` 桩。
type MediaFetcher interface {
	FetchMedia(ctx context.Context, ref MediaRef) ([]byte, error)
}

// Emitter 渠道的「回显」接收者（**可选能力**）。
//
// 消息层把工具循环推来的过程事件转给它：推理增量、每次工具调用、最终答案。渠道
// 自己决定显不显示、怎么显示——iLink 只记日志、不做终端回显；将来的渠道可以发一条
// 「正在查…」的草稿消息，或什么都不做。
//
// **用基本类型而不是 `agent.ToolEvent`**：接缝不认识 agent（否则加渠道就得依赖
// 工具循环那一层）；加一个渠道只依赖本包。
type Emitter interface {
	// Reasoning 推理增量（模型按块吐，一次一小段）
	Reasoning(text string)
	// Tool 一次工具调用完成：名字、原样参数、结果
	Tool(name, arguments, result string)
	// Answer 最终答案全文
	Answer(text string)
}

// EmitterProvider **可选**能力：渠道为**一条消息**提供它的回显实现。
//
// ctx 里带着账号/消息号等归属（消息层已绑好），渠道据此写出带归属的日志。
// 返回 nil = 用消息层的默认（只记工具日志、不做回显）。
//
// 与 AddressResolver / MediaFetcher 同一条规矩：调用方用**类型断言**探测支持与否。
type EmitterProvider interface {
	Emitter(ctx context.Context) Emitter
}

// LoginParams 交互式登录的参数。
type LoginParams struct {
	// Account 账号选择器（槽位号或账号 id）。怎么读是渠道自己的事
	Account string
	// Ctx 中断信号：客户端断开时用来停掉登录流程
	Ctx context.Context
	// Emit 流式事件出口（二维码、状态），由控制面转发给客户端
	Emit func(event string, data any)
}

// LoginResult 交互式登录成功后的结果：登录到了哪个账号、它现在的状态。
//
// 以前 Login 返回 `any`，调用方得靠类型断言到 `map[string]any` 再取键名，键名拼错
// 只会在运行期静默变成空值。现在字段在编译期就是确定的。
type LoginResult struct {
	AccountID string `json:"accountId"`
	Status    string `json:"status"`
}

// Ops 渠道**运维端口**（可选）：渠道自己提供、CLI 直接调用的操作。
//
// 不支持某项能力就不实现对应方法——调用方据此报「不支持」，而不是拿到 nil 去调。
// 目前只有登录；状态与上下文报告是渠道的具体方法（见 ilink.Provider），
// 还没有通过这个接口被调用的需求，所以不在接口里。
type Ops interface {
	// Login 交互式登录（扫码等）。不支持登录的渠道不实现
	Login(params LoginParams) (LoginResult, error)
}

// Provider 渠道**种类**。一个 provider 可以产出多个渠道实例（一个账号一个）。
//
// 加一个渠道 = 实现一个 provider + 在装配里加一项——业务层一行不动。
type Provider interface {
	// ID 种类标识，如 ilink。会写进 InboundMessage.ChannelID
	ID() string
	// Label 人类可读名称，用于日志与错误提示
	Label() string
	// Create 产出这个种类当前的渠道实例。服务启动时调一次
	Create(ctx context.Context) ([]Channel, error)
	// ResolveAccount 把用户给的账号选择器（`2` / `account_002`）解析成账号 id（可选）。
	//
	// 选择器怎么读是**渠道自己的事**（iLink 读 .env 的 ILINK_ACCOUNT_N_ID），
	// 所以这一步留在 provider 里，而不是写进 seam。
	ResolveAccount(selector string) (string, bool)
	// DescribeAccounts 把可选账号列成一句话，用在「你选的账号不存在」的提示里（可选）
	DescribeAccounts() string
	// Ops 运维端口（状态 / 上下文 / 登录）。nil = 这个渠道没有这些操作
	Ops() Ops
}

// ValidateChannel 核对能力声明与发送器是否一致。
//
// **这是 Node 版 `defineChannel()` 的编译期保证在 Go 里的等价物**（见包头）：
// 声明能发的就必须有发送器，声明不能发的就不许有。
func ValidateChannel(channel Channel) error {
	caps := channel.Capabilities()
	senders := channel.Senders()

	if caps.Text.Send && senders.Text == nil {
		return fmt.Errorf("渠道 %s 声明能发文本，但没有提供文本发送器", channel.ID())
	}
	if !caps.Text.Send && senders.Text != nil {
		return fmt.Errorf("渠道 %s 没声明能发文本，却提供了文本发送器", channel.ID())
	}

	for _, kind := range []MediaKind{KindFile, KindImage, KindVoice, KindVideo} {
		capability := caps.For(kind)
		sender, hasSender := senders.For(kind)
		if capability.Send && !hasSender {
			return fmt.Errorf("渠道 %s 声明能发 %s，但没有提供对应的发送器", channel.ID(), kind)
		}
		if !capability.Send && hasSender {
			return fmt.Errorf("渠道 %s 没声明能发 %s，却提供了对应的发送器", channel.ID(), kind)
		}
		_ = sender
	}

	if channel.AccountID() == "" {
		return fmt.Errorf("渠道 %s 没有账号标识", channel.ID())
	}
	if channel.Label() == "" {
		return fmt.Errorf("渠道 %s 没有可读名称", channel.ID())
	}
	return nil
}
