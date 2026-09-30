// Package ilink 是 iLink（微信）渠道的完整实现：**协议细节与渠道接口之间的那层膜**。
//
// 两件事：
//
//  1. ToInboundMessage —— 纯函数，把协议消息归一化成渠道无关的入站消息
//  2. NewChannel —— 实现 channels.Channel：轮询起停、发送、取媒体
//
// 协议实现仍在 `bot/`（client / parse / crypto / cdn / upload / send / poller / login）——
// 那是**协议该在的地方**，本包只把它们接到渠道接口上。业务层看不到这两边。
package ilink

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/channels/ilink/bot"
)

// 渠道标识与名称。**ID 会写进 InboundMessage.ChannelID 与会话键**，所以它一旦
// 定下就不能改——改了等于所有会话历史文件都换了前缀。
const (
	ID    = "ilink"
	Label = "微信（iLink）"
)

// mediaHandle 媒体句柄里装的是什么：**下载所需的全部参数**。
//
// MediaRef.ID 对调用方不透明，但必须**自足**——消息可能在收到后很久才被处理，
// 甚至跨进程重启，那时没有别的地方能再查到这条 CDN 参数。所以把 CDNMedia
// （以及图片特有的平铺 aeskey）序列化进去。
//
// 值本身仍是 iLink 的 CDN 参数——**句柄的含义由渠道解释**，这正是「不透明」的意思。
type mediaHandle struct {
	Media bot.CDNMedia `json:"media"`
	// Aeskey 图片项的密钥是平铺的 `image_item.aeskey`；文件项只能从 `media.aes_key` 取
	Aeskey string `json:"aeskey,omitempty"`
}

// mediaRefOf 把 CDNMedia 包成渠道无关的媒体句柄。
func mediaRefOf(media bot.CDNMedia, extra channels.MediaRef, aeskey string) channels.MediaRef {
	raw, err := json.Marshal(mediaHandle{Media: media, Aeskey: aeskey})
	if err != nil {
		// mediaHandle 全是值类型，json.Marshal 不会失败。真失败了说明有字段类型不对，
		// 那时给一个**空的**句柄——后续 FetchMedia 会明确报错，
		// 而不是在入站阶段就炸掉整条消息
		raw = []byte("{}")
	}
	extra.ID = string(raw)
	return extra
}

// decodeHandle 解析句柄。取不回来时返错——那说明这个 ref 不是本渠道产生的。
func decodeHandle(ref channels.MediaRef) (mediaHandle, error) {
	var handle mediaHandle
	if err := json.Unmarshal([]byte(ref.ID), &handle); err != nil {
		return handle, fmt.Errorf("不是 iLink 渠道的媒体句柄：%s", snippet(ref.ID))
	}
	// 一个都没有说明反序列化出来是空的，而不是真的拿到了参数
	if handle.Media.EncryptQueryParam == "" && handle.Media.FullURL == "" {
		return handle, fmt.Errorf("媒体句柄里没有 CDN 参数：%s", snippet(ref.ID))
	}
	return handle, nil
}

func snippet(text string) string {
	const limit = 80
	if len(text) > limit {
		return text[:limit] + "…"
	}
	return text
}

// sizeOf 数字字符串转数字；转不出来就不填——**不编造**。
func sizeOf(length string) int64 {
	if length == "" {
		return 0
	}
	value, err := strconv.ParseInt(length, 10, 64)
	if err != nil {
		return 0
	}
	return value
}

// toPart 一个 item → 一个 part。取不到内容的（怪消息）返回 false，由调用方丢掉。
func toPart(item bot.MessageItem) (channels.Part, bool) {
	switch item.Type {
	case bot.ItemTypeText:
		if item.Text == "" {
			return channels.Part{}, false
		}
		return channels.Part{Kind: channels.KindText, Text: item.Text}, true

	case bot.ItemTypeImage:
		if item.Image == nil || item.Image.Media.EncryptQueryParam == "" {
			return channels.Part{}, false
		}
		return channels.Part{
			Kind:  channels.KindImage,
			Media: mediaRefOf(item.Image.Media, channels.MediaRef{}, item.Image.Aeskey),
		}, true

	case bot.ItemTypeVoice:
		if item.Voice == nil || item.Voice.Media.EncryptQueryParam == "" {
			return channels.Part{}, false
		}
		// 语音转文字的正文挂在 `Transcript` 上——那往往是这条消息**唯一**可读的内容。
		//
		// 这里曾经有一段 `if item.Voice.Text != "" { ref.Checksum = "" }`：给一个零值
		// 字段赋零值，注释却写着「有了就带上」。**空操作**，删掉。
		return channels.Part{
			Kind:       channels.KindVoice,
			Media:      mediaRefOf(item.Voice.Media, channels.MediaRef{}, item.Voice.Aeskey),
			DurationMs: item.Voice.Duration,
			Transcript: item.Voice.Text,
		}, true

	case bot.ItemTypeFile:
		if item.File == nil || item.File.Media.EncryptQueryParam == "" {
			return channels.Part{}, false
		}
		ref := channels.MediaRef{
			FileName: item.File.FileName,
			MimeType: "",
			Checksum: item.File.MD5,
		}
		ref.Size = sizeOf(item.File.Len)
		return channels.Part{
			Kind:  channels.KindFile,
			Media: mediaRefOf(item.File.Media, ref, ""),
		}, true

	case bot.ItemTypeVideo:
		if item.Video == nil || item.Video.Media.EncryptQueryParam == "" {
			return channels.Part{}, false
		}
		ref := channels.MediaRef{}
		if item.Video.Size > 0 {
			ref.Size = item.Video.Size
		}
		return channels.Part{
			Kind:       channels.KindVideo,
			Media:      mediaRefOf(item.Video.Media, ref, item.Video.Aeskey),
			DurationMs: item.Video.Duration,
		}, true
	}
	return channels.Part{}, false
}

// ToInboundMessage iLink 的消息 → 渠道无关的入站消息。
//
// **纯函数、无 IO**：所以它可以用真实 fixture 离线测——那些「字段名写错就静默
// 读到零值」的坑，都靠这一层被测到。
func ToInboundMessage(raw bot.WeixinMessage, accountID string) channels.InboundMessage {
	parts := make([]channels.Part, 0, len(raw.Items))
	for _, item := range raw.Items {
		if part, ok := toPart(item); ok {
			parts = append(parts, part)
		}
	}

	message := channels.InboundMessage{
		ChannelID:   ID,
		AccountID:   accountID,
		MessageID:   raw.MessageID,
		SenderID:    raw.FromUserID,
		PrincipalID: raw.FromUserID,
		RecipientID: raw.ToUserID,
		// 私聊里「会话」就是对方
		ConversationID: raw.FromUserID,
		ReplyToken:     raw.ContextToken,
		Parts:          parts,
		Timestamp:      raw.CreateTimeMs,
		Raw:            raw,
	}

	if raw.Quote != nil {
		quoted := &channels.QuotedMessage{ID: raw.Quote.ID, Body: raw.Quote.Body}
		if raw.Quote.PartialText != nil {
			quoted.PartialText = &channels.PartialText{
				Start:    raw.Quote.PartialText.Start,
				End:      raw.Quote.PartialText.End,
				StartIdx: raw.Quote.PartialText.StartIdx,
				EndIdx:   raw.Quote.PartialText.EndIdx,
				QuoteMD5: raw.Quote.PartialText.QuoteMD5,
			}
		}
		message.Quoted = quoted
	}
	return message
}

// ── 渠道实例 ────────────────────────────────────────────

// sessionState 记「最近一次入站」的行话。**渠道自己的状态**，不让业务层写。
//
// 为什么需要它：iLink 协议**没有「主动开启会话」的接口**，回复必须回传收到消息
// 时的 `context_token`。所以「发给谁」的解析只能靠「上次那个人发过什么」。
type sessionState struct {
	mu     sync.RWMutex
	byAcct map[string]map[string]sessionEntry
}

type sessionEntry struct {
	conversation string
	replyToken   string
	at           int64
}

func newSessionState() *sessionState {
	return &sessionState{byAcct: map[string]map[string]sessionEntry{}}
}

// maxSessionsPerAccount 每个账号最多记多少条「最近一次入站」。
//
// 会话上下文是**只增不减**的：每个跟 Bot 说过话的人都会留下一条。家庭规模下它长不到
// 哪去，但「只增不减」的结构迟早要有个上限——而策略很清楚：**留最近的 N 条**，
// 因为 `lastOf` 要的就是最近的那个（同 `at` 时与会话名定序，与 lastOf 同一套规则）。
const maxSessionsPerAccount = 64

// recordInbound 记下某账号与某人的最近一次入站。
func (s *sessionState) recordInbound(accountID, conversationID, replyToken string, at int64) {
	if replyToken == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	conversations, ok := s.byAcct[accountID]
	if !ok {
		conversations = map[string]sessionEntry{}
		s.byAcct[accountID] = conversations
	}
	conversations[conversationID] = sessionEntry{
		conversation: conversationID, replyToken: replyToken, at: at,
	}

	if len(conversations) <= maxSessionsPerAccount {
		return
	}

	// 超了就把最老的挤掉（按 `at`，同 at 用会话名定序）
	oldestID := ""
	var oldest sessionEntry
	for id, entry := range conversations {
		if oldestID == "" || entry.at < oldest.at ||
			(entry.at == oldest.at && id < oldestID) {
			oldestID, oldest = id, entry
		}
	}
	delete(conversations, oldestID)
}

// lastOf 某账号最近一次入站。没有就是零值。
func (s *sessionState) lastOf(accountID string) (sessionEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var best sessionEntry
	found := false
	for _, entry := range s.byAcct[accountID] {
		// 谁的 at 大谁最新。**at 相同时按 conversation 名字定序**——
		// map 遍历顺序随机，不定序的话同一时刻的结果会飘
		if !found || entry.at > best.at ||
			(entry.at == best.at && entry.conversation > best.conversation) {
			best, found = entry, true
		}
	}
	return best, found
}

// of 某账号与某个具体会话最近一次入站。
func (s *sessionState) of(accountID, conversationID string) (sessionEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.byAcct[accountID][conversationID]
	return entry, ok
}

// accountTable 账号表。**渠道实例通过它读「当下」的账号**——
// 重新登录会整只替换表里的对象（见 Provider.Login）。
type accountTable struct {
	mu       sync.RWMutex
	accounts map[string]bot.WeixinAccount
}

func newAccountTable(accounts []bot.WeixinAccount) *accountTable {
	table := &accountTable{accounts: make(map[string]bot.WeixinAccount, len(accounts))}
	for _, account := range accounts {
		table.accounts[account.ID] = account
	}
	return table
}

func (t *accountTable) get(id string) (bot.WeixinAccount, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	account, ok := t.accounts[id]
	return account, ok
}

func (t *accountTable) replace(account bot.WeixinAccount) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.accounts[account.ID] = account
}

func (t *accountTable) setStatus(id, status string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	account, ok := t.accounts[id]
	if !ok {
		return
	}
	account.Status = status
	t.accounts[id] = account
}

func (t *accountTable) list() []bot.WeixinAccount {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]bot.WeixinAccount, 0, len(t.accounts))
	for _, account := range t.accounts {
		out = append(out, account)
	}
	return out
}

// pollerFor 造一个账号的轮询器。**可注入**——测试要能不联网就把整条链路摆布开。
//
// 回调在**构造时**给，不在事后挂：轮询器一启动就可能来消息，事后再挂会漏掉
// 启动瞬间到挂上之间那几条。
type pollerFor func(account bot.WeixinAccount, cursors *bot.CursorStore,
	onMessage func(bot.WeixinMessage), onExpired func()) poller

// poller 一个账号的「会跑的东西」的最小形状。
//
// **只要三件事**：能起、能停、能报告 session 过期。回调由 NewPoller 装上，
// 所以这里不用重复声明 onMessage。
type poller interface {
	Start(ctx context.Context)
	Stop()
}

// stopTimeout 停机等待的上限。
const stopTimeout = 5 * time.Second

// Channel 一个 iLink 账号 = 一个渠道实例。
type Channel struct {
	accountID string
	table     *accountTable
	state     *sessionState
	cursors   *bot.CursorStore
	newPoller pollerFor

	mu        sync.Mutex
	running   bool
	onMessage func(channels.InboundMessage)
	p         poller
	cancel    context.CancelFunc
}

// NewChannel 造一个账号的渠道实例。
func NewChannel(account bot.WeixinAccount, table *accountTable, state *sessionState,
	cursors *bot.CursorStore, newPoller pollerFor) *Channel {
	return &Channel{
		accountID: account.ID,
		table:     table,
		state:     state,
		cursors:   cursors,
		newPoller: newPoller,
	}
}

// ID 渠道类型标识。
func (c *Channel) ID() string { return ID }

// Label 人类可读名称。
func (c *Channel) Label() string { return Label }

// AccountID 本实例的账号标识。
func (c *Channel) AccountID() string { return c.accountID }

// StorageID 本账号在**存储层**用的稳定标识。
//
// 用 USER_ID（`ILINK_ACCOUNT_<N>_USER_ID`）而不是槽位推出来的 `account_001`：
// 前者更稳定也更可读，而按账号落盘的目录希望用它。登录前拿不到 USER_ID 时退回账号 id。
//
// **每次都重读账号表**——重新登录会整只替换表里的对象，而这里若缓存了创建时的
// 副本，重登后就会一直用旧值。
func (c *Channel) StorageID() string {
	account, ok := c.table.get(c.accountID)
	if !ok {
		return c.accountID
	}
	if account.ILinkUserID != "" {
		return account.ILinkUserID
	}
	return account.ID
}

// Status 运行状态。同样每次重读——状态只有账号表那一份真相。
func (c *Channel) Status() channels.Status {
	account, ok := c.table.get(c.accountID)
	if !ok {
		return channels.StatusOffline
	}
	switch account.Status {
	case bot.AccountOnline:
		return channels.StatusOnline
	case bot.AccountExpired:
		return channels.StatusExpired
	default:
		return channels.StatusOffline
	}
}

// Capabilities 能力声明。**收发对称**：
//
//   - 文本 / 文件 / 图片：能收也能发
//   - 语音 / 视频：**能收不能发**——解析它们，但发送首期不做
//   - ProactivePush 假——协议没有「主动开启会话」的接口
//
// `Send: false` 让 ValidateChannel 知道这里不该有发送器；`Receive: true` 由
// toPart 与一致性测试对齐。
func (c *Channel) Capabilities() channels.Capabilities {
	return channels.Capabilities{
		Text:          channels.KindCapability{Send: true, Receive: true},
		File:          channels.KindCapability{Send: true, Receive: true},
		Image:         channels.KindCapability{Send: true, Receive: true},
		Voice:         channels.KindCapability{Send: false, Receive: true},
		Video:         channels.KindCapability{Send: false, Receive: true},
		ProactivePush: false,
	}
}

// Senders 出站处理器。与 Capabilities 一一对应。
//
// **每次发送都重新读一次账号表**：重新登录后凭证已变，而这里若握着旧对象，
// 每一条发送都会带着失效的 token——症状是「轮询正常、发送全报 -14」。
func (c *Channel) Senders() channels.Senders {
	return channels.Senders{
		Text:  c.sendText,
		File:  c.sendFile,
		Image: c.sendImage,
		// 语音 / 视频刻意为 nil：Capabilities 声明了不能发，
		// 提供发送器会被 ValidateChannel 拒下
	}
}

// account 读「当下」的账号。**读不到就返错**——拿一个空 token 去发只会得到
// 一个不指向根因的 -14。
func (c *Channel) account() (bot.WeixinAccount, error) {
	account, ok := c.table.get(c.accountID)
	if !ok || account.BotToken == "" {
		return bot.WeixinAccount{}, fmt.Errorf("账号 %s 不在账号表里，或还没有凭证", c.accountID)
	}
	return account, nil
}

// target 解析成出站目标：会话 + 回复令牌。
//
// **回复令牌是必需的**：协议没有主动开启会话的接口，而令牌只在收到消息时产生。
// 主动发送时用 ResolveAddress 从「最近一次入站」里取。
func (c *Channel) target(target channels.SendTarget) (string, string, error) {
	conversation := target.ConversationID
	token := target.ReplyToken

	if token == "" {
		if conversation == "" {
			entry, ok := c.state.lastOf(c.accountID)
			if !ok {
				return "", "", fmt.Errorf("账号 %s 还没收到过任何消息，"+
					"而 iLink 协议要求回传 context_token 才能回复", c.accountID)
			}
			return entry.conversation, entry.replyToken, nil
		}
		entry, ok := c.state.of(c.accountID, conversation)
		if !ok {
			return "", "", fmt.Errorf("没有 %s 与 %s 的会话上下文："+
				"协议要求回传收到消息时的 context_token", c.accountID, conversation)
		}
		return entry.conversation, entry.replyToken, nil
	}
	if conversation == "" {
		return "", "", fmt.Errorf("给了回复令牌却没给会话：说不清发给谁")
	}
	return conversation, token, nil
}

func (c *Channel) sendText(ctx context.Context, params channels.SendTextParams) (channels.SendResult, error) {
	account, err := c.account()
	if err != nil {
		return channels.SendResult{}, err
	}
	conversation, token, err := c.target(params.Target)
	if err != nil {
		return channels.SendResult{}, err
	}

	result, err := bot.NewSender(account, nil).SendText(ctx, conversation, token, params.Text)
	if err != nil {
		return channels.SendResult{}, err
	}
	return channels.SendResult{MessageID: result.MessageID, Raw: result.Raw}, nil
}

func (c *Channel) sendFile(ctx context.Context, params channels.SendMediaParams) (channels.SendResult, error) {
	account, err := c.account()
	if err != nil {
		return channels.SendResult{}, err
	}
	conversation, token, err := c.target(params.Target)
	if err != nil {
		return channels.SendResult{}, err
	}

	result, err := bot.NewSender(account, nil).SendFile(ctx, conversation, token, params.Path, params.FileName)
	if err != nil {
		return channels.SendResult{}, err
	}
	return channels.SendResult{MessageID: result.MessageID, Raw: result.Raw}, nil
}

func (c *Channel) sendImage(ctx context.Context, params channels.SendMediaParams) (channels.SendResult, error) {
	account, err := c.account()
	if err != nil {
		return channels.SendResult{}, err
	}
	conversation, token, err := c.target(params.Target)
	if err != nil {
		return channels.SendResult{}, err
	}

	result, err := bot.NewSender(account, nil).SendImage(ctx, conversation, token, params.Path, params.FileName)
	if err != nil {
		return channels.SendResult{}, err
	}
	return channels.SendResult{MessageID: result.MessageID, Raw: result.Raw}, nil
}

// Start 开始接收消息。**幂等**：已经在跑时什么也不做。
func (c *Channel) Start(ctx context.Context, onMessage func(channels.InboundMessage)) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.running {
		return nil
	}

	// **先校验再改状态**：以前这两句在查询之后，于是「账号不在账号表里」之后这个
	// 实例永远停在 `running=true`——后续 Start 走幂等分支直接返 nil，而它一个轮询
	// 都没起（`Service.StartAll` 那边只记一条 Warn，界面上看不出区别）。
	account, ok := c.table.get(c.accountID)
	if !ok {
		return fmt.Errorf("账号 %s 不在账号表里", c.accountID)
	}

	c.running = true
	c.onMessage = onMessage

	// 回调闭在**渠道实例**上而不是 provider 上：一个 poller 管一个账号，
	// 而入站消息要带上这一账号的会话上下文
	p := c.newPoller(account, c.cursors, func(message bot.WeixinMessage) {
		inbound := ToInboundMessage(message, c.accountID)
		// **先记上下文，再上抛**：业务层收到消息后可能立刻回复，
		// 而那时它需要的就是这份令牌
		c.state.recordInbound(c.accountID, inbound.ConversationID, inbound.ReplyToken, inbound.Timestamp)

		c.mu.Lock()
		listener := c.onMessage
		c.mu.Unlock()
		if listener != nil {
			listener(inbound)
		}
	}, func() {
		// session 过期要**标为 expired 而不是 offline**：
		// 前者表示凭证失效需重新扫码，后者只是没在跑。运维要靠这个区分该做什么
		c.table.setStatus(c.accountID, bot.AccountExpired)

		// 继续拉只会反复拿到 -14，所以停掉这个账号。
		//
		// ## 为什么必须换一个 goroutine 停，而不是就地在回调里停
		//
		// 这个回调是**轮询器在自己的 goroutine 上叫的**（`poller.go` 的 `loop` 里
		// 那句 `p.OnSessionExpired()`），而 `Stop` 要等那个 goroutine 退出
		// （`p.Wait()`）——就地停就是「等自己」，**永久阻塞**。症状不是报错，而是
		// 每过期一次泄漏一个 goroutine，账号停在 `running=false` 的假停机态。
		// 原来那句 `context.WithoutCancel` 只解决了 ctx 取消，解决不了「等的对象
		// 是自己」。
		//
		// 新 goroutine 自带超时，且**不复用触发它的那个 ctx**：那时它可能已经取消，
		// 而它的 `defer cancel` 会让刚起的停机立刻被取消掉。
		go func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
			defer cancel()
			_ = c.Stop(stopCtx)
		}()
	})

	runCtx, cancel := context.WithCancel(ctx)
	c.p = p
	c.cancel = cancel
	p.Start(runCtx)
	return nil
}

// Stop 停止接收。**幂等**，且会等轮询真的退出。
//
// ## 为什么要等，又为什么不能无限等
//
// 等：调用方紧接着可能关掉 HTTP 传输层，而长轮询还挂在一次读上——直接返回会产生
// 一堆看不懂的连接错误。
//
// 不无限等：真卡住时停机不该跟着卡住（`stopTimeout` 就是那个上限）。放弃等待不是
// 放弃停机——`cancel` 已经调过，那个 goroutine 会随着 ctx 取消自己结束；这里把
// 超时**如实返错**，由调用方记一条警告，而不是安静地装作停好了。
func (c *Channel) Stop(ctx context.Context) error {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return nil
	}
	c.running = false
	c.onMessage = nil
	p, cancel := c.p, c.cancel
	c.p, c.cancel = nil, nil
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	stopped := true
	if p != nil {
		waited := make(chan struct{})
		go func() { p.Stop(); close(waited) }()

		select {
		case <-waited:
		case <-ctx.Done():
			stopped = false
		}
	}

	// **别把 expired 覆盖成 offline。** session 过期的路径是「先标 expired
	// （需重新扫码）、再停机」，而停机若无条件写 offline，那个更要紧的信号
	// 就被抹掉了——运维看到「没在跑」只会反复重启，而真正要做的是重新扫码。
	if account, ok := c.table.get(c.accountID); ok && account.Status != bot.AccountExpired {
		c.table.setStatus(c.accountID, bot.AccountOffline)
	}

	if !stopped {
		return fmt.Errorf("账号 %s 的轮询没在停机时限内退出（%s）", c.accountID, stopTimeout)
	}
	return nil
}

// FetchMedia 取回媒体字节。这是入站媒体唯一的落地方式。
func (c *Channel) FetchMedia(ctx context.Context, ref channels.MediaRef) ([]byte, error) {
	handle, err := decodeHandle(ref)
	if err != nil {
		return nil, err
	}

	// 图片项的密钥是平铺的；文件项只能从 media.aes_key 取
	aeskey := handle.Aeskey
	if aeskey == "" {
		aeskey = handle.Media.AesKey
	}
	if aeskey == "" {
		return nil, fmt.Errorf("媒体句柄里没有解密密钥，无法取回")
	}

	account, err := c.account()
	if err != nil {
		return nil, err
	}
	return bot.DownloadMedia(ctx, bot.NewClient(account, nil), handle.Media, aeskey)
}

// ResolveAddress 把「发给谁」解析成出站地址。
//
// iLink 是**有状态**的渠道：要拿到 context_token 就必须读过「最近一次入站」，
// 而那是渠道自己的状态（sessionState），不是入参能带的信息。
func (c *Channel) ResolveAddress(params channels.ResolveAddressParams) (channels.OutboundAddress, bool, error) {
	conversation := strings.TrimSpace(params.To)
	token := strings.TrimSpace(params.Token)

	if conversation == "" {
		entry, ok := c.state.lastOf(c.accountID)
		if !ok {
			return channels.OutboundAddress{}, false, nil
		}
		conversation, token = entry.conversation, entry.replyToken
	} else if token == "" {
		entry, ok := c.state.of(c.accountID, conversation)
		if !ok {
			return channels.OutboundAddress{}, false, nil
		}
		token = entry.replyToken
	}

	return channels.OutboundAddress{ConversationID: conversation, ReplyToken: token}, true, nil
}
