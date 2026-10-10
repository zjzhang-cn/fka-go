package messages_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zjzhang-cn/fka-go/internal/agent"
	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/llm"
	"github.com/zjzhang-cn/fka-go/internal/messages"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// ── 假渠道 ──────────────────────────────────────────────
//
// 判据是「业务层不认识任何渠道实现」，所以这里**手写**一个渠道，而不是复用某个真
// 渠道的测试夹具——复用自己的就等于在测「接缝能不能认出自己家的东西」。

type sentText struct {
	conversation string
	token        string
	text         string
}

type sentMedia struct {
	conversation string
	token        string
	kind         channels.MediaKind
	path         string
}

type fakeChannel struct {
	accountID string
	onMessage func(channels.InboundMessage)

	mu     sync.Mutex
	texts  []sentText
	medias []sentMedia

	// sendTextErr 让发送失败，用来验「发不出去只记日志、不再发第二条解释」
	sendTextErr error
}

// 消息层只认「身份 + 出站 + 起停」——所以这个假渠道**只实现必填的三组**。
// 可选能力（能不能自己推断收件人、能不能取媒体）不该逼它写桩，那正是拆接口的理由。
func (c *fakeChannel) ID() string        { return "fake" }
func (c *fakeChannel) Label() string     { return "假渠道" }
func (c *fakeChannel) AccountID() string { return c.accountID }

func (c *fakeChannel) Capabilities() channels.Capabilities {
	return channels.Capabilities{
		Text:  channels.KindCapability{Send: true, Receive: true},
		File:  channels.KindCapability{Send: true, Receive: false},
		Image: channels.KindCapability{Send: false, Receive: false},
	}
}

func (c *fakeChannel) Senders() channels.Senders {
	return channels.Senders{
		Text: func(ctx context.Context, p channels.SendTextParams) (channels.SendResult, error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.sendTextErr != nil {
				return channels.SendResult{}, c.sendTextErr
			}
			c.texts = append(c.texts, sentText{p.Target.ConversationID, p.Target.ReplyToken, p.Text})
			return channels.SendResult{MessageID: "sent-1"}, nil
		},
		File: func(ctx context.Context, p channels.SendMediaParams) (channels.SendResult, error) {
			// **尊重调用方的 ctx**（真的发送器也这样）：否则「实现里偷偷换成
			// context.Background()」这件事在用例里完全看不出来
			if err := ctx.Err(); err != nil {
				return channels.SendResult{}, err
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			c.medias = append(c.medias, sentMedia{
				p.Target.ConversationID, p.Target.ReplyToken, channels.KindFile, p.Path,
			})
			return channels.SendResult{MessageID: "sent-file"}, nil
		},
	}
}

func (c *fakeChannel) Status() channels.Status { return channels.StatusOnline }

func (c *fakeChannel) Start(ctx context.Context, onMessage func(channels.InboundMessage)) error {
	c.onMessage = onMessage
	return nil
}
func (c *fakeChannel) Stop(ctx context.Context) error { c.onMessage = nil; return nil }

func (c *fakeChannel) sentTexts() []sentText {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]sentText(nil), c.texts...)
}

func (c *fakeChannel) sentMedias() []sentMedia {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]sentMedia(nil), c.medias...)
}

// fakeProvider 一种渠道，可以产出多个账号。
type fakeProvider struct {
	id       string
	accounts []string
}

func (p *fakeProvider) ID() string    { return p.id }
func (p *fakeProvider) Label() string { return "假渠道" }
func (p *fakeProvider) Create(ctx context.Context) ([]channels.Channel, error) {
	out := make([]channels.Channel, 0, len(p.accounts))
	for _, account := range p.accounts {
		out = append(out, &fakeChannel{accountID: account})
	}
	return out, nil
}
func (p *fakeProvider) ResolveAccount(selector string) (string, bool) { return selector, true }
func (p *fakeProvider) DescribeAccounts() string                      { return "任意账号" }
func (p *fakeProvider) Ops() channels.Ops                             { return nil }

// ── 假模型 ──────────────────────────────────────────────

// echoModel 假装一个「先调工具、再作答」的模型。
//
// ## 为什么要真的调一次工具
//
// 只回一句话的话，**工具那一条链路一次都没被走过**：身份有没有传下去、Reply 有没有
// 串进 tools.Context，全都不受检验。所以默认行为是先发一个 tool_call，下一轮再给答案。
type echoModel struct {
	mu        sync.Mutex
	seenTools [][]string
	answer    string
	failWith  error
	calls     int
	// noToolCall 直接给答案，不调工具
	noToolCall bool
}

func (m *echoModel) client() llm.ChatClient {
	return func(ctx context.Context, messagesIn []llm.ChatMessage, toolDefs []llm.ToolDef) (llm.ChatResult, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.calls++
		if m.failWith != nil {
			return llm.ChatResult{}, m.failWith
		}
		names := make([]string, 0, len(toolDefs))
		for _, def := range toolDefs {
			names = append(names, def.Name)
		}
		m.seenTools = append(m.seenTools, names)

		// 只在**还没调过工具**时调一次。工具结果回填进来之后就该收尾作答——
		// 不然每轮都调，循环会一直转到步数上限。
		if !m.noToolCall && len(toolDefs) > 0 && !hasToolResult(messagesIn) {
			return llm.ChatResult{ToolCalls: []llm.ToolCall{{
				ID: "call-1", Name: toolDefs[0].Name, Arguments: "{}",
			}}}, nil
		}
		return llm.ChatResult{Content: m.answer}, nil
	}
}

func (m *echoModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// hasToolResult 这轮之前有没有已经回填的工具结果。
func hasToolResult(messagesIn []llm.ChatMessage) bool {
	for _, message := range messagesIn {
		if message.Role == "tool" {
			return true
		}
	}
	return false
}

// ── 夹具 ────────────────────────────────────────────────

// echoTool 一个只读工具：记录它看到的身份，并在被调时做 sendFile 交代的事。
//
// 用 tools.SourceFuncs 而不是自己写一整个 Source——**多数源用不上 PromptSection
// 与 Close**，为它们写空方法只会让夹具变长。
type echoTool struct {
	lastPrincipal string
	// sendFile 是它被调时要做的事。nil = 什么都不做。
	// **带上调用方的 ctx**：这样用例能验「工具那一步的取消信号真的传到了 Reply」
	sendFile func(ctx context.Context, reply tools.Reply) error
}

func (t *echoTool) source() tools.Source {
	return tools.SourceFuncs{
		SourceID:    "echo",
		SourceLabel: "回声",
		ListFunc: func(ctx context.Context, tc tools.Context) ([]tools.Spec, error) {
			return []tools.Spec{{
				Name:        "say",
				Description: "原样说一遍",
				Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
				Effect:      tools.EffectRead,
			}}, nil
		},
		CallFunc: func(ctx context.Context, name string, args map[string]any, tc tools.Context) (tools.Result, error) {
			t.lastPrincipal = tc.PrincipalID
			if t.sendFile != nil {
				if err := t.sendFile(ctx, tc.Reply); err != nil {
					return tools.FailResult("发文件失败：%s", err.Error()), nil
				}
			}
			return tools.OKResult("说了"), nil
		},
	}
}

type fixture struct {
	service  *channels.Service
	handler  *messages.Handler
	channel  *fakeChannel
	channels []*fakeChannel
	model    *echoModel
	tool     *echoTool
}

// channelOf 某个账号的渠道实例。分片用例要按账号分别投递。
func (f *fixture) channelOf(accountID string) *fakeChannel {
	for _, channel := range f.channels {
		if channel.accountID == accountID {
			return channel
		}
	}
	return nil
}

// newFixture 装一份「假渠道 + 假模型 + 真接缝 + 真消息层」。
//
// **刻意不 mock 接缝**：这条竖切要验的正是「订阅 → 分发 → 回话」这条链，
// mock 掉接缝就只剩各零件自测了。
func newFixture(t *testing.T, answer string) *fixture {
	t.Helper()
	return newFixtureWithAccounts(t, answer, "acct-1")
}

// newFixtureWithAccounts 同 newFixture，但一次接上多个账号——
// 按账号分片的行为只有在两个账号同时来消息时才看得出来。
func newFixtureWithAccounts(t *testing.T, answer string, accounts ...string) *fixture {
	t.Helper()

	registry := tools.NewRegistry(nil, tools.ReadToolPolicy())
	tool := &echoTool{}
	registry.Use(tool.source())

	model := &echoModel{answer: answer}
	runner := agent.NewRunner(model.client(), registry, agent.RunnerOptions{
		MaxSteps: 3, SystemPrompt: "测试用",
	})
	if runner == nil {
		t.Fatal("runner 不该为 nil")
	}

	service := channels.NewService()
	created, err := service.Register(context.Background(), &fakeProvider{id: "fake", accounts: accounts})
	if err != nil {
		t.Fatalf("注册假渠道失败：%v", err)
	}

	channelsOut := make([]*fakeChannel, 0, len(created))
	for _, channel := range created {
		channelsOut = append(channelsOut, channel.(*fakeChannel))
	}
	if len(channelsOut) == 0 {
		t.Fatal("至少该造出一个渠道实例")
	}

	return &fixture{
		service:  service,
		handler:  messages.NewHandler(runner),
		channel:  channelsOut[0],
		channels: channelsOut,
		model:    model,
		tool:     tool,
	}
}

// deliver 模拟渠道来了一条消息，并等答复落地。
//
// **顺序与生产一致**：先 Subscribe，再 StartAll。反过来会有一个丢消息的窗口——
// 渠道一开收就可能来消息，而那时还没有订阅者。
//
// 消费循环也在**另一个 goroutine**里跑，和 app.Serve 一样。这不是形式：接缝的投递
// 是非阻塞的，没人消费的话事件就烂在缓冲区里，答复永远不会出现。
func (f *fixture) deliver(t *testing.T, message channels.InboundMessage) {
	t.Helper()
	subscription := f.service.Subscribe()
	defer subscription.Close()

	// **手写消费循环而不是用 HandleFunc**，为了能确定性地知道「处理完了」：
	// HandleFunc 要等订阅流关闭才返回，而那要等 StopAll——每个用例白等几秒，
	// 失败时还看不出是慢还是卡。
	//
	// 仍然走**真的接缝广播 + 真的消息层**。HandleFunc 本身由下面一个用例单独钉。
	processed := make(chan struct{})
	go func() {
		event := <-subscription.C
		f.handler.Handle(context.Background(), event)
		close(processed)
	}()

	f.service.StartAll(context.Background())
	f.channel.onMessage(message)

	// **同步点就在这里**：处理完就会 close，没有答复落地与否的问题。
	// 之前这里还跟了一段「轮询等答复」，而它对「发不出去」那个用例会空转满
	// 超时——测试**变慢而不是失败**，是最坏的一种坏法。
	select {
	case <-processed:
	case <-time.After(2 * time.Second):
		t.Fatal("消息层两秒内没处理完")
	}
}

func textMessage(text string) channels.InboundMessage {
	return channels.InboundMessage{
		ChannelID: "fake", AccountID: "acct-1", MessageID: "m1",
		SenderID: "s1", PrincipalID: "wx_zhang", ConversationID: "c1",
		ReplyToken: "tok-1", Parts: []channels.Part{channels.TextPart(text)},
		Timestamp: 1700000000000,
	}
}

// ── 用例 ────────────────────────────────────────────────

// Test入站问题得到答复 **主链路**：渠道来一条文字 → 答复回到同一会话、带上同一个
// 回复令牌。回复令牌是渠道要求的东西，漏了它就发不出去。
func Test入站问题得到答复(t *testing.T) {
	f := newFixture(t, "三亚好玩吗？")
	// 这条只验「答复原路回去」，不验工具——跳过工具那一轮，模型正好调一次
	f.model.noToolCall = true

	f.deliver(t, textMessage("我们去年三亚玩得怎么样"))

	texts := f.channel.sentTexts()
	if len(texts) != 1 {
		t.Fatalf("该恰好回一条，实际 %d 条：%+v", len(texts), texts)
	}
	if texts[0].text != "三亚好玩吗？" {
		t.Errorf("答复内容 = %q", texts[0].text)
	}
	if texts[0].conversation != "c1" {
		t.Errorf("答复发到了会话 %q，该是 c1", texts[0].conversation)
	}
	if texts[0].token != "tok-1" {
		t.Errorf("答复带的是令牌 %q，该带入站那条的 tok-1", texts[0].token)
	}
	if f.model.callCount() != 1 {
		t.Errorf("模型被调了 %d 次，该 1 次", f.model.callCount())
	}
}

// Test身份来自消息层 不是模型说了算。
//
// 工具认的身份是**模型填的参数**（已接受的风险），但**至少消息层给的那个值要真的
// 传下去**。这里验的是链路没在中间丢掉它。
func Test身份来自消息层(t *testing.T) {
	f := newFixture(t, "好")
	f.deliver(t, textMessage("记一条"))

	if f.tool.lastPrincipal != "wx_zhang" {
		t.Errorf("工具看到的身份 = %q，该是 wx_zhang", f.tool.lastPrincipal)
	}
	// 「调工具 → 拿到结果 → 作答」两轮，不是一轮就完
	if f.model.callCount() != 2 {
		t.Errorf("该走「调工具 + 作答」两轮模型调用，实际 %d 轮", f.model.callCount())
	}
}

// Test身份缺失就拒答 **宁可回一句，也不让它猜。**
//
// 缺了 PrincipalID，模型就得自己编一个填进工具参数——那等于没有权限过滤。
func Test身份缺失就拒答(t *testing.T) {
	f := newFixture(t, "不该被调用")

	message := textMessage("记一条：孩子过敏")
	message.PrincipalID = ""
	f.deliver(t, message)

	texts := f.channel.sentTexts()
	if len(texts) != 1 {
		t.Fatalf("该回一句说明，实际 %d 条", len(texts))
	}
	if !strings.Contains(texts[0].text, "认不出你是谁") {
		t.Errorf("该说清为什么拒答：%q", texts[0].text)
	}
	if f.model.callCount() != 0 {
		t.Errorf("拒答不该调模型，实际调了 %d 次", f.model.callCount())
	}
}

// Test只有图片时如实说不能答 文件/图片那条路随文档能力一起没了。
//
// 与其静默丢弃，回一句「我只处理文字问题」——用户至少知道该发什么。
func Test只有图片时如实说不能答(t *testing.T) {
	f := newFixture(t, "不该被调用")

	message := textMessage("")
	message.Parts = []channels.Part{{
		Kind:  channels.KindImage,
		Media: channels.MediaRef{ID: "media-1", FileName: "photo.jpg"},
	}}
	f.deliver(t, message)

	texts := f.channel.sentTexts()
	if len(texts) != 1 {
		t.Fatalf("该回一句说明，实际 %d 条", len(texts))
	}
	if !strings.Contains(texts[0].text, "只处理文字问题") {
		t.Errorf("该说清只处理文字：%q", texts[0].text)
	}
	if f.model.callCount() != 0 {
		t.Errorf("没有可答内容不该调模型，实际调了 %d 次", f.model.callCount())
	}
}

// Test模型出错如实说不静默降级 「稍后再试」会把「接口没配好」伪装成「模型偶尔不回」。
func Test模型出错如实说(t *testing.T) {
	f := newFixture(t, "")
	f.model.failWith = errors.New("401 unauthorized")

	f.deliver(t, textMessage("你好"))

	texts := f.channel.sentTexts()
	if len(texts) != 1 {
		t.Fatalf("该回一句失败说明，实际 %d 条", len(texts))
	}
	if !strings.Contains(texts[0].text, "401 unauthorized") {
		t.Errorf("该把真实原因说出来：%q", texts[0].text)
	}
}

// Test空答复不当成成功 模型给空串时回一句「没想出该怎么回」，
// 而不是发一条空消息出去。
func Test空答复不当成成功(t *testing.T) {
	f := newFixture(t, "   \n  ")
	f.deliver(t, textMessage("你好"))

	texts := f.channel.sentTexts()
	if len(texts) != 1 {
		t.Fatalf("该回一句说明，实际 %d 条", len(texts))
	}
	if strings.TrimSpace(texts[0].text) == "" {
		t.Error("不该发空消息")
	}
}

// Test发不出去只记日志 已经答完了，回话失败**不该再发第二条**去解释
// 「刚才那条没发出去」——那会在用户的会话里凭空多出一条。
func Test发不出去只记日志(t *testing.T) {
	f := newFixture(t, "答复")
	f.model.noToolCall = true
	f.channel.sendTextErr = errors.New("连接断了")

	f.deliver(t, textMessage("你好"))

	if got := len(f.channel.sentTexts()); got != 0 {
		t.Errorf("发送失败不该记成已发出：%d 条", got)
	}
	// 关键：模型确实被调过了——失败发生在**回话**这一步，不是没跑
	// 关键：模型确实被调过了——失败发生在**回话**这一步，不是压根没跑
	if f.model.callCount() != 1 {
		t.Errorf("模型该被调过一次，实际 %d 次", f.model.callCount())
	}
}

// Test工具能往当前会话发文件 Reply 是**逐条消息**造的：「能发给谁」取决于这条消息的
// 会话与回复令牌，那是消息层的事实，工具层不该知道。
//
// 这条链路要穿过 agent.Runner ——它把 RunnerInput.Reply 装进 tools.Context——
// 所以它同时是「Reply 没在中间丢掉」的证明。
func Test工具能往当前会话发文件(t *testing.T) {
	f := newFixture(t, "文件发你了")

	f.tool.sendFile = func(ctx context.Context, reply tools.Reply) error {
		if reply == nil {
			return errors.New("这一轮没有回话能力")
		}
		// **把工具的 ctx 传下去**：发文件是一次网络上传，停机时该能被打断
		return reply.File(ctx, "/tmp/a.pdf", "a.pdf")
	}

	f.deliver(t, textMessage("把那份 PDF 发我"))

	medias := f.channel.sentMedias()
	if len(medias) != 1 {
		t.Fatalf("该发出一个文件，实际 %d 个", len(medias))
	}
	if medias[0].path != "/tmp/a.pdf" {
		t.Errorf("发的是 %q", medias[0].path)
	}
	if medias[0].conversation != "c1" {
		t.Errorf("文件发到了会话 %q，该是 c1", medias[0].conversation)
	}
	// ReplyToken 也要带上——渠道靠它知道这是回哪条
	if medias[0].token != "tok-1" {
		t.Errorf("文件带的是令牌 %q，该是 tok-1", medias[0].token)
	}
}

// Test工具能往当前会话发文字 与发文件同一条链路，但走渠道的文本发送器——
// 模型因此能先发一段说明、再发文件，而不是只有最终答案那一条出口。
func Test工具能往当前会话发文字(t *testing.T) {
	f := newFixture(t, "答案在这")

	f.tool.sendFile = func(ctx context.Context, reply tools.Reply) error {
		if reply == nil {
			return errors.New("这一轮没有回话能力")
		}
		return reply.Text(ctx, "先发这句")
	}

	f.deliver(t, textMessage("说话"))

	var got []string
	for _, item := range f.channel.sentTexts() {
		got = append(got, item.text)
	}
	joined := strings.Join(got, "|")
	// 工具那句 + 最终答案那句，两条都在
	if !strings.Contains(joined, "先发这句") || !strings.Contains(joined, "答案在这") {
		t.Fatalf("工具文字与最终答案该都发出去，实际 %v", got)
	}
}

// Test渠道发不了图片就退成文件 **发成文件比发不出去强**——假渠道声明了
// Image.Send=false，所以只该有文件、没有图片。
func Test渠道发不了图片就退成文件(t *testing.T) {
	f := newFixture(t, "当文件发了")

	f.tool.sendFile = func(ctx context.Context, reply tools.Reply) error {
		if reply == nil {
			return errors.New("这一轮没有回话能力")
		}
		return reply.Image(ctx, "/tmp/p.png", "p.png")
	}

	f.deliver(t, textMessage("发张图"))

	medias := f.channel.sentMedias()
	if len(medias) != 1 {
		t.Fatalf("该发出一个文件，实际 %d 个", len(medias))
	}
	if medias[0].kind != channels.KindFile {
		t.Errorf("渠道发不了图片时该退成文件，实际 %s", medias[0].kind)
	}
}

// Test常驻循环会一直处理 上面那些用例手写了消费循环来换取确定性；这条钉
// **生产真正用的那个**（messages.Dispatch）：它要能连着处理多条，而不是处理一条就卡住。
func Test常驻循环会一直处理(t *testing.T) {
	f := newFixture(t, "好")
	f.model.noToolCall = true

	subscription := f.service.Subscribe()
	stopped := make(chan struct{})
	go func() {
		messages.Dispatch(f.handler, subscription)()
		close(stopped)
	}()

	f.service.StartAll(context.Background())
	for i, text := range []string{"第一句", "第二句", "第三句"} {
		message := textMessage(text)
		message.MessageID = string(rune('a' + i))
		f.channel.onMessage(message)
	}

	// 等三条都落地
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(f.channel.sentTexts()) < 3 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(f.channel.sentTexts()); got != 3 {
		t.Fatalf("该处理三条，实际回了 %d 条", got)
	}

	// 停机后循环要退出，否则 Close 之后还有 goroutine 在读一个没人写的流
	f.service.StopAll(context.Background())
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Error("停机后消费循环没退出")
	}
}

// Test回话带的是工具那一步的ctx 发文件是一次网络上传（要上传到 CDN 再引用），
// 所以**「谁在发」这一轮的取消信号必须传到发送器上**。实现里写死
// `context.Background()` 的话，这里传入的 ctx 被取消也拦不住它——而这个用例
// 就是那么红的（假发送器尊重 ctx，见夹具）。
func Test回话带的是工具那一步的ctx(t *testing.T) {
	f := newFixture(t, "文件发你了")

	sendErr := make(chan error, 1)
	f.tool.sendFile = func(toolCtx context.Context, reply tools.Reply) error {
		// 从工具真正拿到的那个 ctx 派生一个并立刻取消——模拟「停机」，
		// 然后照常试着发
		ctx, cancel := context.WithCancel(toolCtx)
		cancel()

		err := reply.File(ctx, "/tmp/a.pdf", "a.pdf")
		sendErr <- err
		return err
	}

	f.deliver(t, textMessage("把那份 PDF 发我"))

	select {
	case err := <-sendErr:
		if err == nil {
			t.Error("ctx 已取消，发送却不该成功——成功说明实现用的是别的 ctx" +
				"（`context.Background()` 就长这样）")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("工具没被调到")
	}
}
