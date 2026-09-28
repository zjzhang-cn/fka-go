package ilink

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/channels/ilink/bot"
)

// 真实报文形状的 fixture。字段名与类型照 Node 版记的**真机实测**结论。
const (
	fixtureText = `{
      "seq": 1, "message_id": "7391827364518293647382",
      "from_user_id": "o9cq80_zhang", "to_user_id": "o9cq80_bot",
      "session_id": "s1", "message_type": 1, "message_state": 2,
      "context_token": "ctx-token-1", "create_time_ms": 1758000000000,
      "item_list": [{ "type": 1, "msg_id": "9007199254740993",
                      "text_item": { "text": "去年三亚好玩吗" } }] }`

	fixtureImage = `{
      "seq": 2, "message_id": 12345678901234,
      "from_user_id": "o9cq80_zhang", "to_user_id": "o9cq80_bot",
      "session_id": "s1", "message_type": 1, "message_state": 2,
      "context_token": "ctx-2", "create_time_ms": 1758000001000,
      "item_list": [{ "type": 2, "msg_id": 55555,
        "image_item": { "aeskey": "0123456789abcdef0123456789abcdef", "mid_size": 288784,
          "media": { "encrypt_query_param": "param-xyz",
                     "aes_key": "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=",
                     "full_url": "https://cdn.example/media/abc" } } }] }`

	fixtureFile = `{
      "seq": 3, "message_id": 777,
      "from_user_id": "o9cq80_zhang", "to_user_id": "o9cq80_bot",
      "session_id": "s1", "message_type": 1, "message_state": 2,
      "context_token": "ctx-3", "create_time_ms": 1758000002000,
      "item_list": [{ "type": 4,
        "file_item": { "file_name": "房产证.docx", "md5": "d41d8cd98f00b204e9800998ecf8427e",
                       "len": "204800",
          "media": { "encrypt_query_param": "param-file",
                     "aes_key": "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" } } }] }`

	fixtureQuote = `{
      "seq": 4, "message_id": 888,
      "from_user_id": "o9cq80_zhang", "to_user_id": "o9cq80_bot",
      "session_id": "s1", "message_type": 1, "message_state": 2,
      "context_token": "ctx-4", "create_time_ms": 1758000003000,
      "item_list": [{ "type": 1, "text_item": { "text": "这个怎么办" },
        "ref_msg": { "svr_id": "889900112233", "title": "冰箱保修还有30天" } }] }`
)

func parseFixture(t *testing.T, raw string) bot.WeixinMessage {
	t.Helper()
	var message bot.RawMessage
	if err := json.Unmarshal([]byte(raw), &message); err != nil {
		t.Fatalf("解析 fixture 失败：%v", err)
	}
	return bot.ParseMessage(message, "account_001")
}

// ── 归一化 ──────────────────────────────────────────────

// Test归一化基本字段
func Test归一化基本字段(t *testing.T) {
	inbound := ToInboundMessage(parseFixture(t, fixtureText), "account_001")

	if inbound.ChannelID != ID {
		t.Errorf("ChannelID = %q", inbound.ChannelID)
	}
	if inbound.AccountID != "account_001" {
		t.Errorf("AccountID = %q", inbound.AccountID)
	}
	// uint64 不能丢精度
	if inbound.MessageID != "7391827364518293647382" {
		t.Errorf("MessageID 丢了精度：%q", inbound.MessageID)
	}
	if inbound.SenderID != "o9cq80_zhang" {
		t.Errorf("SenderID = %q", inbound.SenderID)
	}
	// **PrincipalID 就是 wxid 原样**：数据（属主、可见性）与检索过滤都按它存的
	if inbound.PrincipalID != "o9cq80_zhang" {
		t.Errorf("PrincipalID = %q", inbound.PrincipalID)
	}
	if inbound.RecipientID != "o9cq80_bot" {
		t.Errorf("RecipientID = %q", inbound.RecipientID)
	}
	// 私聊里「会话」就是对方
	if inbound.ConversationID != "o9cq80_zhang" {
		t.Errorf("ConversationID = %q", inbound.ConversationID)
	}
	// **回复令牌必须带出来**：协议没有主动开启会话的接口，
	// 而业务层收到消息后可能立刻回复
	if inbound.ReplyToken != "ctx-token-1" {
		t.Errorf("ReplyToken = %q", inbound.ReplyToken)
	}
	if inbound.Timestamp != 1758000000000 {
		t.Errorf("Timestamp = %d", inbound.Timestamp)
	}
	if inbound.Text() != "去年三亚好玩吗" {
		t.Errorf("Text() = %q", inbound.Text())
	}
}

// Test图片归一化且句柄自足 **句柄必须自足**：消息可能在收到后很久才被处理，
// 甚至跨进程重启，那时没有别的地方能再查到这条 CDN 参数。
func Test图片归一化且句柄自足(t *testing.T) {
	inbound := ToInboundMessage(parseFixture(t, fixtureImage), "account_001")

	if len(inbound.Parts) != 1 || inbound.Parts[0].Kind != channels.KindImage {
		t.Fatalf("该有一个图片 part：%+v", inbound.Parts)
	}

	ref := inbound.Parts[0].Media
	// 句柄里该装下解密所需的全部参数
	var handle mediaHandle
	if err := json.Unmarshal([]byte(ref.ID), &handle); err != nil {
		t.Fatalf("句柄不是合法 JSON：%v", err)
	}
	if handle.Media.EncryptQueryParam != "param-xyz" {
		t.Errorf("句柄里该有 encrypt_query_param：%+v", handle)
	}
	// 图片项的密钥是**平铺**的
	if handle.Aeskey != "0123456789abcdef0123456789abcdef" {
		t.Errorf("句柄里该有平铺的 aeskey：%q", handle.Aeskey)
	}
	if handle.Media.FullURL != "https://cdn.example/media/abc" {
		t.Errorf("句柄里该有 full_url：%q", handle.Media.FullURL)
	}
}

// Test文件归一化 Len是协议里的字符串，要转成数字填进句柄
func Test文件归一化(t *testing.T) {
	inbound := ToInboundMessage(parseFixture(t, fixtureFile), "account_001")

	if len(inbound.Parts) != 1 || inbound.Parts[0].Kind != channels.KindFile {
		t.Fatalf("该有一个文件 part：%+v", inbound.Parts)
	}
	part := inbound.Parts[0]
	if part.Media.FileName != "房产证.docx" {
		t.Errorf("FileName = %q", part.Media.FileName)
	}
	if part.Media.Size != 204800 {
		t.Errorf("Size = %d，期望 204800（协议里 len 是字符串）", part.Media.Size)
	}
	if part.Media.Checksum != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Errorf("Checksum = %q", part.Media.Checksum)
	}
	// 文件项**没有**平铺的 aeskey——密钥只能从 media.aes_key 取
	var handle mediaHandle
	if err := json.Unmarshal([]byte(part.Media.ID), &handle); err != nil {
		t.Fatal(err)
	}
	if handle.Aeskey != "" {
		t.Errorf("文件项不该有平铺 aeskey，实际 %q", handle.Aeskey)
	}
	if handle.Media.AesKey == "" {
		t.Error("文件项的密钥只能来自 media.aes_key，句柄里该有它")
	}
}

func Test引用归一化(t *testing.T) {
	inbound := ToInboundMessage(parseFixture(t, fixtureQuote), "account_001")

	if inbound.Quoted == nil {
		t.Fatal("该带出引用")
	}
	if inbound.Quoted.ID != "889900112233" {
		t.Errorf("引用 id = %q", inbound.Quoted.ID)
	}
	if inbound.Quoted.Body != "冰箱保修还有30天" {
		t.Errorf("引用正文 = %q", inbound.Quoted.Body)
	}
}

func Test没有引用时是Nil(t *testing.T) {
	inbound := ToInboundMessage(parseFixture(t, fixtureText), "account_001")
	if inbound.Quoted != nil {
		t.Errorf("没有引用时不该是 nil 外壳：%+v", inbound.Quoted)
	}
}

// Test一个消息多个part 一条消息可以有「图 + 文」，两个都要在。
func Test一个消息多个Part(t *testing.T) {
	raw := `{"seq":5,"message_id":1,"from_user_id":"a","to_user_id":"b",
      "session_id":"s","message_type":1,"message_state":2,"context_token":"c",
      "create_time_ms":1,
      "item_list":[
        {"type":2,"image_item":{"aeskey":"0123456789abcdef0123456789abcdef",
          "media":{"encrypt_query_param":"p"}}},
        {"type":1,"text_item":{"text":"看这张"}}]}`

	inbound := ToInboundMessage(parseFixture(t, raw), "account_001")
	if len(inbound.Parts) != 2 {
		t.Fatalf("该有两个 part：%+v", inbound.Parts)
	}
	if inbound.Parts[0].Kind != channels.KindImage || inbound.Parts[1].Kind != channels.KindText {
		t.Errorf("part 顺序或种类不对：%+v", inbound.Parts)
	}
}

// Test取不到内容的item被丢掉 **宁可少一个 part，也不要一个空壳**——
// 空壳会让业务层以为「有个空图片」，而它其实压根不存在。
func Test取不到内容的Item被丢掉(t *testing.T) {
	raw := `{"seq":6,"message_id":1,"from_user_id":"a","to_user_id":"b",
      "session_id":"s","message_type":1,"message_state":2,"context_token":"c",
      "create_time_ms":1,
      "item_list":[
        {"type":1},
        {"type":2},
        {"type":1,"text_item":{"text":"真话"}}]}`

	inbound := ToInboundMessage(parseFixture(t, raw), "account_001")
	if len(inbound.Parts) != 1 || inbound.Parts[0].Text != "真话" {
		t.Errorf("该只留有内容的那个：%+v", inbound.Parts)
	}
}

func Test句柄不是本渠道的就报错(t *testing.T) {
	_, err := decodeHandle(channels.MediaRef{ID: "这不是句柄"})
	if err == nil {
		t.Fatal("该报错")
	}
	// 合法 JSON 但没有 CDN 参数，同样不是合法句柄
	if _, err := decodeHandle(channels.MediaRef{ID: "{}"}); err == nil {
		t.Error("没有 CDN 参数的句柄该报错")
	}
}

// ── 能力声明 ────────────────────────────────────────────

// Test能力声明与发送器一致 这条由接缝的 ValidateChannel 核对，
// 而**这里自己先跑一遍**：声明与发送器对不上会在接缝那里被拒，
// 错误信息说「渠道 ilink 声明能发语音，但没有提供对应的发送器」——
// 不如在自己的测试里就说清。
func Test能力声明与发送器一致(t *testing.T) {
	channel := newTestChannel(t, "account_001")

	if err := channels.ValidateChannel(channel); err != nil {
		t.Fatalf("能力声明与发送器对不上：%v", err)
	}

	caps := channel.Capabilities()
	// 语音 / 视频**能收不能发**
	if !caps.Voice.Receive || caps.Voice.Send {
		t.Errorf("语音该能收不能发：%+v", caps.Voice)
	}
	if !caps.Video.Receive || caps.Video.Send {
		t.Errorf("视频该能收不能发：%+v", caps.Video)
	}
	// 协议没有主动开启会话的接口
	if caps.ProactivePush {
		t.Error("iLink 没有主动推送，不该声明支持")
	}
	// 但**确实能收**语音与视频——声明 Receive 才不会让上层以为收不到
	if _, ok := channel.Senders().For(channels.KindVoice); ok {
		t.Error("不该提供语音发送器")
	}
}

// TestStorageID优先用UserId 槽位推出来的 `account_001` 不如 USER_ID 稳定可读，
// 而按账号落盘的目录希望用它。
func TestStorageID优先用UserId(t *testing.T) {
	channel := newTestChannel(t, "account_001")
	if got := channel.StorageID(); got != "o9cq80_zhang" {
		t.Errorf("StorageID = %q，期望 USER_ID", got)
	}

	// 登录前拿不到 USER_ID 时退回账号 id
	provider := NewProvider()
	provider.Accounts = []bot.WeixinAccount{{ID: "account_001", BotToken: "t"}}
	created, err := provider.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := created[0].StorageID(); got != "account_001" {
		t.Errorf("没有 USER_ID 时该退回账号 id，实际 %q", got)
	}
}

// Test每次读当下而不是缓存 重新登录会整只替换账号表里的对象（凭证变了）。
// 这里若缓存了创建时的副本，重登后就会一直用旧值——
// 症状是「轮询正常、发送全报 -14」。
func Test每次读当下而不是缓存(t *testing.T) {
	provider := NewProvider()
	provider.Accounts = []bot.WeixinAccount{{ID: "account_001", BotToken: "old-token"}}
	created, err := provider.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	channel := created[0]

	if got := channel.StorageID(); got != "account_001" {
		t.Errorf("初始 StorageID = %q", got)
	}

	// 整只替换——就像重新登录做的那样
	provider.table.replace(bot.WeixinAccount{
		ID: "account_001", BotToken: "new-token", ILinkUserID: "o9cq80_new",
		Status: bot.AccountOnline,
	})

	if got := channel.StorageID(); got != "o9cq80_new" {
		t.Errorf("替换后该读到新值，实际 %q —— 说明它缓存了创建时的副本", got)
	}
	if got := channel.Status(); got != channels.StatusOnline {
		t.Errorf("状态该跟着变，实际 %q", got)
	}
}

// Test状态区分Expired与Offline 前者表示凭证失效需重新扫码，后者只是没在跑。
// 运维要靠这个区分该做什么。
func Test状态区分Expired与Offline(t *testing.T) {
	provider := NewProvider()
	provider.Accounts = []bot.WeixinAccount{{ID: "account_001", BotToken: "t"}}
	created, _ := provider.Create(context.Background())
	channel := created[0]

	for _, c := range []struct {
		status string
		want   channels.Status
	}{
		{bot.AccountOnline, channels.StatusOnline},
		{bot.AccountOffline, channels.StatusOffline},
		{bot.AccountExpired, channels.StatusExpired},
		{"别的值", channels.StatusOffline},
	} {
		provider.table.setStatus("account_001", c.status)
		if got := channel.Status(); got != c.want {
			t.Errorf("状态 %q → %q，期望 %q", c.status, got, c.want)
		}
	}
}

// ── 寻址 ────────────────────────────────────────────────

// Test没收到过就不能解析地址 协议要求回传 context_token，而它只在收到消息时产生。
// 解析不出就说解析不出，**不要编一个**。
func Test没收到过就不能解析地址(t *testing.T) {
	channel := newTestChannel(t, "account_001")

	_, ok, err := channel.ResolveAddress(channels.ResolveAddressParams{})
	if err != nil {
		t.Fatalf("不该报错，该说「解析不出」：%v", err)
	}
	if ok {
		t.Error("没收到过任何消息时不该解析成功")
	}
}

func Test按最近一次入站解析(t *testing.T) {
	channel := newTestChannel(t, "account_001")

	channel.state.recordInbound("account_001", "o9cq80_zhang", "ctx-1", 1000)
	channel.state.recordInbound("account_001", "o9cq80_li", "ctx-2", 2000)

	// 不给 To：用最近一次
	address, ok, err := channel.ResolveAddress(channels.ResolveAddressParams{})
	if err != nil || !ok {
		t.Fatalf("该解析成功：ok=%v err=%v", ok, err)
	}
	if address.ConversationID != "o9cq80_li" || address.ReplyToken != "ctx-2" {
		t.Errorf("该用最近一次（时间最大的那个）：%+v", address)
	}

	// 给了 To：按会话取
	address, ok, err = channel.ResolveAddress(channels.ResolveAddressParams{To: "o9cq80_zhang"})
	if err != nil || !ok {
		t.Fatalf("该解析成功：ok=%v err=%v", ok, err)
	}
	if address.ReplyToken != "ctx-1" {
		t.Errorf("ReplyToken = %q，期望 ctx-1", address.ReplyToken)
	}

	// 给了没见过的会话
	if _, ok, _ := channel.ResolveAddress(channels.ResolveAddressParams{To: "陌生人"}); ok {
		t.Error("没见过的人不该解析成功")
	}
}

// Test同时刻的会话要有确定顺序 map 遍历顺序随机，不定序的话
// 「最近一次」在两条同一毫秒时会飘。
func Test同时刻的会话要有确定顺序(t *testing.T) {
	channel := newTestChannel(t, "account_001")
	channel.state.recordInbound("account_001", "aaa", "ctx-a", 1000)
	channel.state.recordInbound("account_001", "zzz", "ctx-z", 1000)

	first, _, _ := channel.ResolveAddress(channels.ResolveAddressParams{})
	for i := 0; i < 20; i++ {
		again, _, _ := channel.ResolveAddress(channels.ResolveAddressParams{})
		if again != first {
			t.Fatalf("同一时刻的结果在飘：%+v vs %+v", again, first)
		}
	}
}

// Test空令牌不记 没拿到令牌的那次入站不该把「上次说过话」记成有上下文
func Test空令牌不记(t *testing.T) {
	state := newSessionState()
	state.recordInbound("account_001", "someone", "", 1000)

	if _, ok := state.lastOf("account_001"); ok {
		t.Error("空令牌不该被记下来")
	}
}

// ── 起停 ────────────────────────────────────────────────

// fakePoller 一个不联网的轮询器：能记下回调、能手动触发。
type fakePoller struct {
	mu        sync.Mutex
	started   bool
	stopped   bool
	onMessage func(bot.WeixinMessage)
	onExpired func()
	calls     int
}

func (p *fakePoller) Start(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = true
}

func (p *fakePoller) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
}

func (p *fakePoller) emit(message bot.WeixinMessage) {
	p.mu.Lock()
	onMessage := p.onMessage
	p.mu.Unlock()
	if onMessage != nil {
		onMessage(message)
	}
}

func (p *fakePoller) expire() {
	p.mu.Lock()
	onExpired := p.onExpired
	p.mu.Unlock()
	if onExpired != nil {
		onExpired()
	}
}

func (p *fakePoller) isStarted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started
}

func (p *fakePoller) isStopped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopped
}

// newTestChannel 造一个带假轮询器的渠道实例。
func newTestChannel(t *testing.T, accountID string) *Channel {
	t.Helper()

	provider := NewProvider()
	provider.Accounts = []bot.WeixinAccount{{
		ID: accountID, BotToken: "token", ILinkUserID: "o9cq80_zhang",
		BaseURL: "https://example.invalid", Status: bot.AccountOnline,
	}}
	if _, err := provider.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	return provider.channels[0]
}

// Test起停幂等 重复 Start 不该起第二个轮询——两个轮询会各自推进游标、互相覆盖。
func Test起停幂等(t *testing.T) {
	provider := NewProvider()
	provider.Accounts = []bot.WeixinAccount{{ID: "account_001", BotToken: "t"}}

	var fake *fakePoller
	provider.NewPoller = func(account bot.WeixinAccount, cursors *bot.CursorStore,
		onMessage func(bot.WeixinMessage), onExpired func()) poller {
		fake = &fakePoller{onMessage: onMessage, onExpired: onExpired}
		return fake
	}

	created, err := provider.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	channel := created[0]

	if err := channel.Start(context.Background(), func(channels.InboundMessage) {}); err != nil {
		t.Fatal(err)
	}
	if err := channel.Start(context.Background(), func(channels.InboundMessage) {}); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 0 {
		t.Errorf("重复 Start 该是空操作")
	}
	if !fake.isStarted() {
		t.Error("该起来了")
	}

	if err := channel.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := channel.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !fake.isStopped() {
		t.Error("该停了")
	}
}

// Test入站先记上下文再上抛 **业务层收到消息后可能立刻回复**，而那时它需要
// 的就是这份令牌——先上抛的话那一刻还没有。
func Test入站先记上下文再上抛(t *testing.T) {
	provider := NewProvider()
	provider.Accounts = []bot.WeixinAccount{{ID: "account_001", BotToken: "t"}}

	var fake *fakePoller
	provider.NewPoller = func(account bot.WeixinAccount, cursors *bot.CursorStore,
		onMessage func(bot.WeixinMessage), onExpired func()) poller {
		fake = &fakePoller{onMessage: onMessage, onExpired: onExpired}
		return fake
	}

	created, _ := provider.Create(context.Background())
	channel := created[0]

	var resolvedAtCallback channels.OutboundAddress
	var resolved bool
	_ = channel.Start(context.Background(), func(inbound channels.InboundMessage) {
		// 回调里立刻试着「发给刚刚说话的那个人」——令牌必须已经在
		address, ok, _ := channel.ResolveAddress(channels.ResolveAddressParams{
			To: inbound.ConversationID,
		})
		resolvedAtCallback, resolved = address, ok
	})

	fake.emit(parseFixture(t, fixtureText))

	if !resolved {
		t.Fatal("回调里不该解析不出——说明上下文是在上抛之后才记的")
	}
	if resolvedAtCallback.ReplyToken != "ctx-token-1" {
		t.Errorf("回调里拿到的令牌 = %q", resolvedAtCallback.ReplyToken)
	}
}

// TestSession过期要停掉并标Expired 继续拉只会反复拿到 -14。
func TestSession过期要停掉并标Expired(t *testing.T) {
	provider := NewProvider()
	provider.Accounts = []bot.WeixinAccount{{ID: "account_001", BotToken: "t"}}

	var fake *fakePoller
	provider.NewPoller = func(account bot.WeixinAccount, cursors *bot.CursorStore,
		onMessage func(bot.WeixinMessage), onExpired func()) poller {
		fake = &fakePoller{onMessage: onMessage, onExpired: onExpired}
		return fake
	}

	created, _ := provider.Create(context.Background())
	channel := created[0]
	_ = channel.Start(context.Background(), func(channels.InboundMessage) {})

	fake.expire()

	if !fake.isStopped() {
		t.Error("过期后该停掉这个账号")
	}
	if got := channel.Status(); got != channels.StatusExpired {
		t.Errorf("状态 = %q，期望 expired（它表示需重新扫码，而 offline 只表示没在跑）", got)
	}
}

// ── provider ────────────────────────────────────────────

func Test账号从环境读(t *testing.T) {
	t.Setenv("ILINK_ACCOUNT_1_ID", "account_001")
	t.Setenv("ILINK_ACCOUNT_1_BOT_TOKEN", "tok-1")
	t.Setenv("ILINK_ACCOUNT_1_BASE_URL", "https://one")
	t.Setenv("ILINK_ACCOUNT_1_USER_ID", "u1")
	// **中间有空洞是允许的**：删掉一个账号不必重排剩下的
	t.Setenv("ILINK_ACCOUNT_3_ID", "account_003")
	t.Setenv("ILINK_ACCOUNT_3_BOT_TOKEN", "tok-3")

	accounts := AccountsFromEnv(t.TempDir())
	if len(accounts) != 2 {
		t.Fatalf("该读出 2 个账号，实际 %d：%+v", len(accounts), accounts)
	}
	if accounts[0].ID != "account_001" || accounts[1].ID != "account_003" {
		t.Errorf("账号读错了：%s %s", accounts[0].ID, accounts[1].ID)
	}
	if accounts[0].ILinkUserID != "u1" || accounts[0].BaseURL != "https://one" {
		t.Errorf("字段没读对：%+v", accounts[0])
	}
}

// Test没有Token的槽位不算账号 那多半是 `.env.example` 复制过来时留下的空壳，
// 而接进来只会得到一个一路报错的渠道实例。
func Test没有Token的槽位不算账号(t *testing.T) {
	t.Setenv("ILINK_ACCOUNT_1_ID", "account_001")
	t.Setenv("ILINK_ACCOUNT_1_BOT_TOKEN", "")
	t.Setenv("ILINK_ACCOUNT_2_ID", "account_002")
	t.Setenv("ILINK_ACCOUNT_2_BOT_TOKEN", "tok")

	accounts := AccountsFromEnv(t.TempDir())
	if len(accounts) != 1 || accounts[0].ID != "account_002" {
		t.Errorf("该只留下有 token 的那个：%+v", accounts)
	}
}

func Test缺BaseURL时补默认值(t *testing.T) {
	t.Setenv("ILINK_ACCOUNT_1_ID", "account_001")
	t.Setenv("ILINK_ACCOUNT_1_BOT_TOKEN", "tok")

	accounts := AccountsFromEnv(t.TempDir())
	if len(accounts) != 1 {
		t.Fatal("该读出一个账号")
	}
	if accounts[0].BaseURL != bot.DefaultBaseURL {
		t.Errorf("BaseURL = %q，期望补成默认", accounts[0].BaseURL)
	}
}

func Test槽位选择器(t *testing.T) {
	provider := NewProvider()
	provider.Accounts = []bot.WeixinAccount{
		{ID: "account_001", BotToken: "t1"},
		{ID: "account_002", BotToken: "t2"},
	}
	if _, err := provider.Create(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 直接给 id
	if got, ok := provider.ResolveAccount("account_002"); !ok || got != "account_002" {
		t.Errorf("按 id 解析失败：%q %v", got, ok)
	}
	// 给槽位号
	if got, ok := provider.ResolveAccount("1"); !ok || got != "account_001" {
		t.Errorf("按槽位号解析失败：%q %v", got, ok)
	}
	// 不认的
	if _, ok := provider.ResolveAccount("9"); ok {
		t.Error("不存在的槽位不该解析成功")
	}
}

// Test按ID排序注册 槽位是从环境扫出来的，map 遍历是随机的——
// 不排序的话每次启动账号顺序都不同，日志与 `fka tools` 的输出就没法比对。
func Test按ID排序注册(t *testing.T) {
	provider := NewProvider()
	provider.Accounts = []bot.WeixinAccount{
		{ID: "account_003", BotToken: "t"},
		{ID: "account_001", BotToken: "t"},
		{ID: "account_002", BotToken: "t"},
	}
	created, err := provider.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, channel := range created {
		got = append(got, channel.AccountID())
	}
	want := []string{"account_001", "account_002", "account_003"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个 = %q，期望 %q（顺序该稳定）", i, got[i], want[i])
		}
	}
}

// Test零个账号不返错 登录是经由本服务做的，没账号就返错的话全新环境会陷入死锁——
// 没账号起不了服务，没服务又登不了账号。
func Test零个账号不返错(t *testing.T) {
	provider := NewProvider()
	created, err := provider.Create(context.Background())
	if err != nil {
		t.Fatalf("不该返错：%v", err)
	}
	if len(created) != 0 {
		t.Errorf("该返回空切片，实际 %d 个", len(created))
	}
	if !strings.Contains(provider.DescribeAccounts(), "登录") {
		t.Errorf("该告诉人怎么登录：%q", provider.DescribeAccounts())
	}
}

// TestContext不泄露令牌 状态快照会落盘，而令牌等同于发消息的资格。
func TestContext不泄露令牌(t *testing.T) {
	provider := NewProvider()
	provider.Accounts = []bot.WeixinAccount{{ID: "account_001", BotToken: "t"}}
	_, _ = provider.Create(context.Background())
	provider.state.recordInbound("account_001", "o9cq80_zhang", "ctx-secret", 1000)

	encoded, err := json.Marshal(provider.Context())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "ctx-secret") {
		t.Errorf("令牌不该出现在状态里：%s", encoded)
	}
	if !strings.Contains(string(encoded), "hasToken") {
		t.Errorf("该说明「有没有令牌」而不是给令牌本身：%s", encoded)
	}
}

// Test槽位越界要拒 变量名里带个 1 的一路扫到 1000000 个槽位不是我们想要的
func Test槽位越界要拒(t *testing.T) {
	provider := NewProvider()
	provider.Accounts = []bot.WeixinAccount{{ID: "account_001", BotToken: "t"}}
	_, _ = provider.Create(context.Background())

	if _, err := provider.pickSlot("0"); err == nil {
		t.Error("槽位 0 该被拒")
	}
	if _, err := provider.pickSlot("abc"); err == nil {
		t.Error("非数字该被拒")
	}
	if _, err := provider.pickSlot("999"); err == nil {
		t.Error("越界该被拒")
	}
	// 下一个空槽：跳过有 token 的
	index, err := provider.pickSlot("next")
	if err != nil {
		t.Fatal(err)
	}
	if index != 2 {
		t.Errorf("下一个空槽 = %d，期望 2", index)
	}
}

// Test登录前先能算空槽 没有 Create 过的 provider 不该能登录
func Test登录流程走通(t *testing.T) {
	provider := NewProvider()
	provider.Accounts = []bot.WeixinAccount{{ID: "account_001", BotToken: "old"}}
	_, _ = provider.Create(context.Background())

	// 假登录服务端
	server := fakeLoginServer(t)
	provider.BaseURL = server.URL
	provider.HTTPClient = server.Client()

	// 凭证要写进 .env——用临时 HOME 免得动到真的
	home := t.TempDir()
	t.Setenv("FKA_HOME", home)

	var events []string
	result, err := provider.Login(channels.LoginParams{
		Ctx:  context.Background(),
		Emit: func(event string, data any) { events = append(events, event) },
	})
	if err != nil {
		t.Fatalf("登录失败：%v", err)
	}

	// 事件序列：取码 → 出码 → 确认
	if len(events) < 3 {
		t.Errorf("事件太少：%v", events)
	}
	if events[0] != "qrcode:fetching" || events[1] != "qrcode:ready" {
		t.Errorf("事件顺序不对：%v", events)
	}
	if events[len(events)-1] != "login:done" {
		t.Errorf("该有 login:done：%v", events)
	}

	report, _ := result.(map[string]any)
	if report["accountId"] != "account_002" {
		t.Errorf("该登录到下一个空槽：%v", report)
	}

	// 凭证落盘了
	envData, err := readFileString(home + "/.env")
	if err != nil {
		t.Fatalf("凭证该写进 .env：%v", err)
	}
	if !strings.Contains(envData, "ILINK_ACCOUNT_2_BOT_TOKEN=fresh-token") {
		t.Errorf(".env 里没有新凭证：\n%s", envData)
	}

	// **账号表换成了新凭证**——而渠道实例是每次重读的，所以它立刻生效
	account, ok := provider.table.get("account_002")
	if !ok || account.BotToken != "fresh-token" {
		t.Errorf("账号表该换成新凭证：%+v", account)
	}
}

// Test登录要清游标 留着旧游标的话，重新登录后服务端会以为客户端已消费到那一段，
// 于是那段时间的消息**永久收不到**。
func Test登录要清游标(t *testing.T) {
	// 槽位 1 被占了，所以登录会落到槽位 2——**游标要清的是那个槽位的**
	provider := NewProvider()
	provider.Accounts = []bot.WeixinAccount{{ID: "account_001", BotToken: "t"}}
	_, _ = provider.Create(context.Background())
	provider.cursors.Set("account_002", "stale-buf")

	server := fakeLoginServer(t)
	provider.BaseURL = server.URL
	provider.HTTPClient = server.Client()
	t.Setenv("FKA_HOME", t.TempDir())

	if _, err := provider.Login(channels.LoginParams{Ctx: context.Background()}); err != nil {
		t.Fatalf("登录失败：%v", err)
	}
	if got := provider.cursors.Get("account_002"); got != "" {
		t.Errorf("登录后游标该清掉，实际还留着 %q", got)
	}
}

func Test取媒体缺密钥要拒(t *testing.T) {
	channel := newTestChannel(t, "account_001")

	// 句柄里没有平铺 aeskey，media 里也没有 aes_key
	handle, _ := json.Marshal(mediaHandle{Media: bot.CDNMedia{EncryptQueryParam: "p"}})
	_, err := channel.FetchMedia(context.Background(), channels.MediaRef{ID: string(handle)})
	if err == nil {
		t.Fatal("没密钥该报错")
	}
	if !strings.Contains(err.Error(), "密钥") {
		t.Errorf("该点名是密钥的事：%v", err)
	}
}

func Test停机后可再起(t *testing.T) {
	provider := NewProvider()
	provider.Accounts = []bot.WeixinAccount{{ID: "account_001", BotToken: "t"}}
	created, _ := provider.Create(context.Background())
	channel := created[0]

	if err := channel.Start(context.Background(), func(channels.InboundMessage) {}); err != nil {
		t.Fatal(err)
	}
	if err := channel.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 停过之后还能再起（session 过期后重新登录要走这条路）
	if err := channel.Start(context.Background(), func(channels.InboundMessage) {}); err != nil {
		t.Errorf("停机后该能再起：%v", err)
	}
	_ = channel.Stop(context.Background())
}

func Test停机要等轮询真的退出(t *testing.T) {
	channel := newTestChannel(t, "account_001")
	_ = channel.Start(context.Background(), func(channels.InboundMessage) {})

	done := make(chan struct{})
	go func() {
		_ = channel.Stop(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 三秒内没返回")
	}
}

// ── 夹具 ────────────────────────────────────────────────

// readFileString 读文件成字符串。
func readFileString(path string) (string, error) {
	data, err := os.ReadFile(path)
	return string(data), err
}

// fakeLoginServer 一个假的登录服务端：取码 → 立刻 confirmed。
func fakeLoginServer(t *testing.T) *httptest.Server {
	t.Helper()

	const token = "3f2a91c4d8e7b6051a2f3c4d5e6f7081"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			_, _ = w.Write([]byte(`{"qrcode":"` + token + `",
			  "qrcode_img_content":"https://liteapp.weixin.qq.com/q/7GiQu1?qrcode=` + token + `&bot_type=3"}`))
		case "/ilink/bot/get_qrcode_status":
			_, _ = w.Write([]byte(`{"status":"confirmed","bot_token":"fresh-token",
			  "ilink_bot_id":"bot-2","ilink_user_id":"user-2",
			  "baseurl":"` + server.URL + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}
