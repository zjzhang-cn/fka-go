package channels

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestBoundary_接缝不import任何具体渠道 这是 Node 版 `tests/channels/boundary.test.ts`
// 的等价物。
//
// 接缝的价值全在「加渠道零改业务层」上；它一旦 import 了某个具体渠道，那条性质
// 就永久失效了——而它是**静默失效**：代码照常编译、照常跑，只是架构约束没了。
// 所以在这里守住。
func TestBoundary_接缝不import任何具体渠道(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读包目录失败：%v", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("解析 %s 失败：%v", name, err)
		}

		for _, spec := range file.Imports {
			path := strings.Trim(spec.Path.Value, `"`)
			// 渠道实现在子目录里（ilink/…），所以「同包的子路径」就是具体渠道
			if strings.HasPrefix(path, "github.com/zjzhang-cn/fka-go/internal/channels/") {
				t.Errorf("%s import 了具体渠道 %q——接缝必须只认本文件里的契约", name, path)
			}
		}
	}
}

// fakeChannel 一个可控的假渠道。
//
// **它只实现必填的三组**（身份 + 出站 + 起停）：可选能力（`AddressResolver` /
// `MediaFetcher`）各自是一个独立接口，不该逼每个假替身都写桩——那正是拆接口的理由。
type fakeChannel struct {
	id        string
	label     string
	account   string
	caps      Capabilities
	senders   Senders
	state     Status
	started   bool
	onMessage func(InboundMessage)
	mu        sync.Mutex
}

func (c *fakeChannel) ID() string                 { return c.id }
func (c *fakeChannel) Label() string              { return c.label }
func (c *fakeChannel) AccountID() string          { return c.account }
func (c *fakeChannel) Capabilities() Capabilities { return c.caps }
func (c *fakeChannel) Senders() Senders           { return c.senders }
func (c *fakeChannel) Status() Status             { c.mu.Lock(); defer c.mu.Unlock(); return c.state }

func (c *fakeChannel) Start(ctx context.Context, onMessage func(InboundMessage)) error {
	c.mu.Lock()
	c.started = true
	c.onMessage = onMessage
	c.mu.Unlock()
	return nil
}

func (c *fakeChannel) Stop(ctx context.Context) error {
	c.mu.Lock()
	c.started = false
	c.mu.Unlock()
	return nil
}

// resolvable 一个**额外**实现了可选能力 `AddressResolver` 的假渠道。
//
// 可选能力单独一个类型，是为了让两条路都有人测：实现它的（能自己推断发给谁）与
// 不实现它的（调用方必须显式给收件人）。这两件事以前挤在同一个 `ok=false` 里。
type resolvable struct {
	*fakeChannel
}

func (c *resolvable) ResolveAddress(p ResolveAddressParams) (OutboundAddress, error) {
	return OutboundAddress{ConversationID: p.To, ReplyToken: p.Token}, nil
}

// resolvableTextOnly 能收能发文本、还会自己推断收件人的渠道。
func resolvableTextOnly(id, account string) *resolvable {
	return &resolvable{fakeChannel: textOnly(id, account)}
}

// emit 假装渠道收到了一条消息。
func (c *fakeChannel) emit(msg InboundMessage) {
	c.mu.Lock()
	fn := c.onMessage
	c.mu.Unlock()
	if fn != nil {
		fn(msg)
	}
}

func (c *fakeChannel) isStarted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started
}

// textOnly 一个最小可用的渠道：能收能发文本。
//
// **只实现必填的三组**，可选能力一概不实现——这就是拆接口之后新渠道的门槛。
func textOnly(id, account string) *fakeChannel {
	return &fakeChannel{
		id: id, label: id + " 渠道", account: account,
		state: StatusOffline,
		caps:  Capabilities{Text: KindCapability{Send: true, Receive: true}},
		senders: Senders{
			Text: func(ctx context.Context, p SendTextParams) (SendResult, error) {
				return SendResult{MessageID: "sent-1"}, nil
			},
		},
	}
}

type fakeProvider struct {
	id       string
	channels []Channel
	accounts map[string]string // 选择器 → 账号 id
	err      error
}

func (p *fakeProvider) ID() string               { return p.id }
func (p *fakeProvider) Label() string            { return p.id + " provider" }
func (p *fakeProvider) Ops() Ops                 { return nil }
func (p *fakeProvider) DescribeAccounts() string { return "" }

func (p *fakeProvider) Create(ctx context.Context) ([]Channel, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.channels, nil
}

func (p *fakeProvider) ResolveAccount(selector string) (string, bool) {
	id, ok := p.accounts[selector]
	return id, ok
}

// TestSeam_重复账号拒绝启动 两个渠道共用一个 accountId 会让状态快照、会话历史与
// 游标互相覆盖——**而那不会当场报**，症状出现在很远的地方。
func TestSeam_重复账号拒绝启动(t *testing.T) {
	service := NewService()
	ctx := context.Background()

	if _, err := service.Register(ctx, &fakeProvider{id: "ilink", channels: []Channel{textOnly("ilink", "acct1")}}); err != nil {
		t.Fatalf("首次注册失败：%v", err)
	}

	// 换个渠道种类，但账号标识相同
	_, err := service.Register(ctx, &fakeProvider{id: "telegram", channels: []Channel{textOnly("telegram", "acct1")}})
	if err == nil {
		t.Fatal("重复的账号标识应当被拒绝")
	}
	if !strings.Contains(err.Error(), "acct1") {
		t.Errorf("报错要点名是哪个账号：%v", err)
	}
}

func TestSeam_同渠道同账号也拒绝(t *testing.T) {
	service := NewService()
	ctx := context.Background()

	if _, err := service.Register(ctx, &fakeProvider{id: "ilink", channels: []Channel{textOnly("ilink", "a")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Register(ctx, &fakeProvider{id: "ilink", channels: []Channel{textOnly("ilink", "a")}}); err == nil {
		t.Fatal("同一渠道的同一账号不该被注册两次")
	}
}

// TestValidateChannel_能力与发送器必须一致 这是 Node 版 defineChannel() 的编译期
// 保证在 Go 里的等价物。
func TestValidateChannel_能力与发送器必须一致(t *testing.T) {
	sendMedia := func(ctx context.Context, p SendMediaParams) (SendResult, error) {
		return SendResult{}, nil
	}

	cases := []struct {
		name    string
		mutate  func(*fakeChannel)
		wantErr string
	}{
		{
			name:    "声明能发却没实现",
			mutate:  func(c *fakeChannel) { c.caps.File.Send = true },
			wantErr: "声明能发 file",
		},
		{
			name:    "没声明却实现了",
			mutate:  func(c *fakeChannel) { c.senders.Image = sendMedia },
			wantErr: "没声明能发 image",
		},
		{
			name:    "没有账号标识",
			mutate:  func(c *fakeChannel) { c.account = "" },
			wantErr: "没有账号标识",
		},
		{
			name:    "没有可读名称",
			mutate:  func(c *fakeChannel) { c.label = "" },
			wantErr: "没有可读名称",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			channel := textOnly("ilink", "a")
			tc.mutate(channel)
			err := ValidateChannel(channel)
			if err == nil {
				t.Fatalf("应当报错")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("报错要含 %q，实际 %q", tc.wantErr, err.Error())
			}
		})
	}

	// 一致的那个不该报错
	if err := ValidateChannel(textOnly("ilink", "a")); err != nil {
		t.Errorf("一致时不该报错：%v", err)
	}
}

func TestSeam_注册时核对能力(t *testing.T) {
	service := NewService()
	broken := textOnly("ilink", "a")
	broken.caps.File.Send = true // 声明能发文件，但没有文件发送器

	_, err := service.Register(context.Background(), &fakeProvider{id: "ilink", channels: []Channel{broken}})
	if err == nil {
		t.Fatal("能力对不上的渠道不该被接受")
	}
	var channelErr *Error
	if !asError(err, &channelErr) || !strings.Contains(channelErr.Hint, "渠道实现") {
		t.Errorf("Hint 要说清是渠道实现的问题：%v", err)
	}
}

// TestSeam_入站广播给所有订阅者
func TestSeam_入站广播给所有订阅者(t *testing.T) {
	service := NewService()
	ctx := context.Background()

	channel := textOnly("ilink", "a")
	if _, err := service.Register(ctx, &fakeProvider{id: "ilink", channels: []Channel{channel}}); err != nil {
		t.Fatal(err)
	}

	first := service.Subscribe()
	second := service.Subscribe()
	defer first.Close()
	defer second.Close()

	service.StartAll(ctx)
	if !channel.isStarted() {
		t.Fatal("StartAll 之后渠道应当在收")
	}

	channel.emit(InboundMessage{ChannelID: "ilink", AccountID: "a", MessageID: "m1"})

	for i, sub := range []*Subscription{first, second} {
		select {
		case event := <-sub.C:
			if event.Message.MessageID != "m1" {
				t.Errorf("订阅 %d 收到的消息不对：%+v", i, event.Message)
			}
			if event.Channel.ID() != "ilink" {
				t.Errorf("订阅 %d 拿到的渠道不对：%s", i, event.Channel.ID())
			}
		default:
			t.Errorf("订阅 %d 没收到消息", i)
		}
	}
}

// TestSeam_退订后不再收到 退订的 goroutine 必须真的停掉，否则会一直泄漏。
func TestSeam_退订后不再收到(t *testing.T) {
	service := NewService()
	ctx := context.Background()
	channel := textOnly("ilink", "a")
	if _, err := service.Register(ctx, &fakeProvider{id: "ilink", channels: []Channel{channel}}); err != nil {
		t.Fatal(err)
	}

	sub := service.Subscribe()
	service.StartAll(ctx)
	sub.Close()

	channel.emit(InboundMessage{MessageID: "m1"})

	// 退订会**关闭事件流**（消费方的 for-range 要靠它退出），所以这里必须用
	// 双返回值：单返回值的 select 会把「流已关闭」读成一条零值消息。
	select {
	case event, ok := <-sub.C:
		if ok {
			t.Errorf("退订后不该再收到：%+v", event)
		}
	default:
	}
}

func TestSeam_解析目标(t *testing.T) {
	service := NewService()
	ctx := context.Background()

	if _, err := service.Register(ctx, &fakeProvider{
		id: "ilink", accounts: map[string]string{"2": "account_002"},
		channels: []Channel{resolvableTextOnly("ilink", "account_002")},
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("渠道加账号", func(t *testing.T) {
		got, err := service.ResolveTarget(ResolveTargetParams{Channel: "ilink", Account: "account_002", To: "wx1"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Address.ConversationID != "wx1" {
			t.Errorf("= %+v", got.Address)
		}
	})

	t.Run("槽位写法也要认", func(t *testing.T) {
		if _, err := service.ResolveTarget(ResolveTargetParams{Channel: "ilink", Account: "2", To: "wx1"}); err != nil {
			t.Errorf("槽位写法应当被 provider 解析：%v", err)
		}
	})

	t.Run("只有一个实例时可以什么都不给", func(t *testing.T) {
		if _, err := service.ResolveTarget(ResolveTargetParams{To: "wx1"}); err != nil {
			t.Errorf("= %v", err)
		}
	})

	t.Run("渠道不支持推断时要指路", func(t *testing.T) {
		// 一个**没有** ResolveAddress 的渠道：接口里没有「可以不实现」这回事，
		// 不实现就是另一个接口——这里验的是「断言失败」那条路有人走。
		plain := NewService()
		if _, err := plain.Register(ctx, &fakeProvider{
			id: "tg", accounts: map[string]string{"1": "account_001"},
			channels: []Channel{textOnly("tg", "account_001")},
		}); err != nil {
			t.Fatal(err)
		}

		_, err := plain.ResolveTarget(ResolveTargetParams{})
		if err == nil {
			t.Fatal("渠道不能自己推断时该报错，而不是编一个收件人")
		}
		if ErrorKindOf(err) != KindAddress {
			t.Errorf("应是寻址失败，实际 %v", ErrorKindOf(err))
		}
		var channelErr *Error
		if !asError(err, &channelErr) || !strings.Contains(channelErr.Hint, "--to") {
			t.Errorf("该指路「用 --to 明确指定收件人」：%v", err)
		}
	})

	t.Run("找不到要说怎么办", func(t *testing.T) {
		_, err := service.ResolveTarget(ResolveTargetParams{Channel: "telegram", Account: "x", To: "wx1"})
		if err == nil {
			t.Fatal("应当报错")
		}
		if ErrorKindOf(err) != KindAddress {
			t.Errorf("应是寻址失败，实际 %v", ErrorKindOf(err))
		}
		// **报错必须带 hint**——只报错不指路等于没帮上忙
		var channelErr *Error
		if !asError(err, &channelErr) || channelErr.Hint == "" {
			t.Errorf("寻址失败要带 hint：%v", err)
		}
	})
}

// TestSeam_跨渠道撞账号在注册时就拒 键是 channel:account，所以 ilink:1 与 tg:1
// 查不出重复——但会话历史文件名是 `<账号>_<会话>.jsonl`，两者会写进同一个文件。
// 所以**账号标识必须跨渠道全局唯一**，检查放在注册期而不是解析期。
func TestSeam_跨渠道撞账号在注册时就拒(t *testing.T) {
	service := NewService()
	ctx := context.Background()

	if _, err := service.Register(ctx, &fakeProvider{id: "ilink", channels: []Channel{textOnly("ilink", "same")}}); err != nil {
		t.Fatal(err)
	}

	_, err := service.Register(ctx, &fakeProvider{id: "tg", channels: []Channel{textOnly("tg", "same")}})
	if err == nil {
		t.Fatal("跨渠道撞账号标识应当在注册时就被拒")
	}
	// 报错要点名「是哪个渠道占了」——只说「重复了」的话，部署的人无从下手
	if !strings.Contains(err.Error(), "ilink") {
		t.Errorf("要点名是哪个渠道用了这个账号：%v", err)
	}
	var channelErr *Error
	if !asError(err, &channelErr) || channelErr.Hint == "" {
		t.Errorf("要带 hint：%v", err)
	}
}

func TestSeam_停掉后不再收(t *testing.T) {
	service := NewService()
	ctx := context.Background()
	channel := textOnly("ilink", "a")
	if _, err := service.Register(ctx, &fakeProvider{id: "ilink", channels: []Channel{channel}}); err != nil {
		t.Fatal(err)
	}
	sub := service.Subscribe()
	service.StartAll(ctx)
	service.StopAll(ctx)

	if channel.isStarted() {
		t.Error("StopAll 之后渠道不该还在收")
	}
	channel.emit(InboundMessage{MessageID: "m1"})
	select {
	case event, ok := <-sub.C:
		if ok {
			t.Errorf("停掉后不该再收到：%+v", event)
		}
	default:
	}
}

func TestSessionKeyOf(t *testing.T) {
	got := SessionKeyOf(&InboundMessage{ChannelID: "ilink", AccountID: "a", ConversationID: "wx1"})
	if got != "ilink:a:wx1" {
		t.Errorf("= %q", got)
	}
}

func TestInboundMessage_Text(t *testing.T) {
	msg := &InboundMessage{Parts: []Part{
		TextPart("你好"),
		{Kind: KindImage},
		TextPart("世界"),
	}}
	if got := msg.Text(); got != "你好世界" {
		t.Errorf("= %q", got)
	}
}

func TestSenders_For_不支持是正常路径(t *testing.T) {
	channel := textOnly("ilink", "a")
	if _, ok := channel.Senders().For(KindImage); ok {
		t.Error("只声明了文本，不该说能发图片")
	}
	if _, ok := channel.Senders().For(KindFile); ok {
		t.Error("同上")
	}
	if !channel.Senders().HasText() {
		t.Error("文本发送器在")
	}
}

// asError 是 errors.As 的薄包装，避免测试 import errors 之后与包内 Error 同名混淆。
func asError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

// TestSeam_注册失败不留半个 见 `Register`：它以前「边校验边写」，第 N 个实例撞账号时
// 前 N-1 个已经进了 instances/accounts/order，而 providers 还没 append——那些实例
// 活着、能被列出来，但再也问不到它们的 provider。注册期的唯一性检查本意是
// **拒绝启动**，部分提交把它变成了一半。
func TestSeam_注册失败不留半个(t *testing.T) {
	service := NewService()
	ctx := context.Background()

	// 先正常接一个（用带 AddressResolver 的那种，好在最后验它仍然可用）
	if _, err := service.Register(ctx, &fakeProvider{
		id: "ilink", accounts: map[string]string{"1": "acct-1"},
		channels: []Channel{resolvableTextOnly("ilink", "acct-1")},
	}); err != nil {
		t.Fatal(err)
	}

	// 再注册一个 provider，它产出两个实例：**第二个与自己撞号**
	_, err := service.Register(ctx, &fakeProvider{
		id: "tg", accounts: map[string]string{"1": "acct-2"},
		channels: []Channel{textOnly("tg", "acct-2"), textOnly("tg", "acct-2")},
	})
	if err == nil {
		t.Fatal("同一个 provider 产出两个同账号的实例，该报错")
	}

	// **第一个实例不该留下**：既不能出现在实例表里，provider 也不能进候选表
	for _, channel := range service.Instances() {
		if channel.ID() == "tg" {
			t.Errorf("注册失败却留下了 %s 的实例（账号 %s）——半接入状态",
				channel.ID(), channel.AccountID())
		}
	}
	if len(service.providers) != 1 {
		t.Errorf("失败的 provider 不该进候选表：%d 个", len(service.providers))
	}
	// 先接上的那个照常可用
	if _, err := service.ResolveTarget(ResolveTargetParams{Account: "acct-1", To: "wx1"}); err != nil {
		t.Errorf("先接上的渠道不该受影响：%v", err)
	}
}
