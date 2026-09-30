package messages_test

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/zjzhang-cn/fka-go/internal/agent"
	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/llm"
	"github.com/zjzhang-cn/fka-go/internal/messages"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// gateModel 一个「进去就卡住、直到放行才作答」的模型。
//
// ## 为什么不能用 echoModel
//
// echoModel 整个调用都持着 `m.mu`。拿它去验并行，第二个请求会**堵在锁上**，
// 于是「两个账号都进来了」看起来像是串行——**测出来的是锁，不是分片**。
// 所以这里的 `entered <- 问题` 刻意不持任何锁。
type gateModel struct {
	// entered 每进来一次发一条问题。**无缓冲**：发不出去就说明没人接，
	// 那正是「第二个账号没能在第一个完成前进来」的失败形态
	entered chan string
	// release 关掉就放行全部进来的请求
	release chan struct{}
	once    sync.Once
}

func newGateModel() *gateModel {
	return &gateModel{
		entered: make(chan string),
		release: make(chan struct{}),
	}
}

func (m *gateModel) open() { m.once.Do(func() { close(m.release) }) }

func (m *gateModel) client() llm.ChatClient {
	return func(ctx context.Context, messagesIn []llm.ChatMessage, defs []llm.ToolDef) (llm.ChatResult, error) {
		question := ""
		for _, message := range messagesIn {
			if message.Role == "user" {
				question = message.Content
				break
			}
		}
		m.entered <- question
		select {
		case <-m.release:
			return llm.ChatResult{Content: "好"}, nil
		case <-ctx.Done():
			return llm.ChatResult{}, ctx.Err()
		}
	}
}

// gatedFixture 装一份「卡在模型里的」两账号夹具。
func gatedFixture(t *testing.T) (*gateModel, *channels.Service, *messages.Handler, map[string]*fakeChannel) {
	t.Helper()

	registry := tools.NewRegistry(nil, tools.ReadToolPolicy())
	model := newGateModel()
	runner := agent.NewRunner(model.client(), registry, agent.RunnerOptions{
		MaxSteps: 2, SystemPrompt: "测试用",
	})
	if runner == nil {
		t.Fatal("runner 不该为 nil")
	}

	service := channels.NewService()
	created, err := service.Register(context.Background(),
		&fakeProvider{id: "fake", accounts: []string{"acct-1", "acct-2"}})
	if err != nil {
		t.Fatalf("注册假渠道失败：%v", err)
	}

	byAccount := map[string]*fakeChannel{}
	for _, channel := range created {
		fake := channel.(*fakeChannel)
		byAccount[fake.accountID] = fake
	}
	return model, service, messages.NewHandler(runner), byAccount
}

// messageOf 造一条指定账号的入站消息。
func messageOf(accountID, messageID, text string) channels.InboundMessage {
	message := textMessage(text)
	message.AccountID = accountID
	message.MessageID = messageID
	message.ConversationID = "conv-" + accountID
	return message
}

// Test账号之间并行处理 **本组用例要钉的那件事**：账号 1 的一轮问答卡在模型里
// 的时候，账号 2 的消息**必须已经在处理**。
//
// 串行实现下这条会红——第二个 `entered` 永远等不到，直到第一个放行。
// 这正是线上症状：一个人问完长问题，另一个人要等整整一轮才被回。
func Test账号之间并行处理(t *testing.T) {
	model, service, handler, byAccount := gatedFixture(t)

	subscription := service.Subscribe()
	stopped := make(chan struct{})
	go func() {
		messages.Dispatch(handler, subscription)()
		close(stopped)
	}()
	service.StartAll(context.Background())
	// **先放行再等退出**：worker 卡在模型里，而 Dispatch 返回前要等它们退出。
	// 顺序反了就是死锁——这正是用例本身要防的那类错误
	defer func() {
		subscription.Close()
		model.open()
		<-stopped
	}()

	// 先让账号 1 进去并卡住
	byAccount["acct-1"].onMessage(messageOf("acct-1", "m1", "第一个账号的问题"))
	select {
	case <-model.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("账号 1 的消息没进模型")
	}

	// 账号 1 还卡着，账号 2 就该已经进来了
	byAccount["acct-2"].onMessage(messageOf("acct-2", "m2", "第二个账号的问题"))
	select {
	case question := <-model.entered:
		if question != "第二个账号的问题" {
			t.Errorf("先进来的是 %q，期望是账号 2 那条", question)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("账号 1 卡着的时候账号 2 进不来——消息处理是串行的")
	}
}

// Test同一账号串行处理 分片的另一半：**同一个账号内不能并行**。
//
// 不是为了性能，是为了让历史与上下文有确定的先后——历史是单文件 append，
// 两条同账号消息并发跑会各自基于同一份旧历史作答，然后交叉落盘，那会静默
// 损坏会话历史而不报任何错。
func Test同一账号串行处理(t *testing.T) {
	model, service, handler, byAccount := gatedFixture(t)

	subscription := service.Subscribe()
	stopped := make(chan struct{})
	go func() {
		messages.Dispatch(handler, subscription)()
		close(stopped)
	}()
	service.StartAll(context.Background())
	defer func() {
		subscription.Close()
		model.open()
		<-stopped
	}()

	channel := byAccount["acct-1"]
	channel.onMessage(messageOf("acct-1", "m1", "第一条"))
	select {
	case <-model.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("第一条没进模型")
	}

	// 第一条还卡着，同账号的第二条**不该**进模型
	channel.onMessage(messageOf("acct-1", "m2", "第二条"))
	select {
	case question := <-model.entered:
		t.Fatalf("同账号不该并行，第二条 %q 在第一条完成前就进来了", question)
	case <-time.After(300 * time.Millisecond):
	}

	// 放行之后第二条才轮到它——而第一条**不会**再进一次模型（那会让「串行」
	// 变成「重复」）
	model.open()
	select {
	case question := <-model.entered:
		if question != "第二条" {
			t.Errorf("该轮到第二条，实际 %q", question)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("放行后第二条没进模型")
	}

	// 两条都答完，且不多跑
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(channel.sentTexts()) >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(channel.sentTexts()); got != 2 {
		t.Errorf("该恰好回两条，实际 %d 条", got)
	}
	select {
	case question := <-model.entered:
		t.Errorf("多跑了一轮：%q", question)
	case <-time.After(100 * time.Millisecond):
	}
}

// Test停机后不留goroutine 订阅流关闭之后 Dispatch **必须返回**，且返回时
// 那些账号 worker 都退出了。
//
// 这条不是洁癖：`app.Serve` 拿到这个返回之后就去 `Close()` 关 MCP 子进程，
// 漏着的 worker 会在一个已经关掉的子进程上调工具，症状是「停机时最后一条
// 消息报工具调用失败」。
func Test停机后不留goroutine(t *testing.T) {
	before := runtime.NumGoroutine()

	for round := 0; round < 5; round++ {
		model, service, handler, byAccount := gatedFixture(t)

		subscription := service.Subscribe()
		stopped := make(chan struct{})
		go func() {
			messages.Dispatch(handler, subscription)()
			close(stopped)
		}()
		service.StartAll(context.Background())

		// 每个账号都留一条**正在处理中**的消息：worker 卡在模型里，
		// 于是「关了订阅流之后还能不能等到它们退出」才有意义
		byAccount["acct-1"].onMessage(messageOf("acct-1", "m1", "卡住"))
		<-model.entered
		byAccount["acct-2"].onMessage(messageOf("acct-2", "m2", "卡住"))
		<-model.entered

		subscription.Close()
		model.open() // 放行，worker 跑完剩下的就该退出

		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
			t.Fatalf("第 %d 轮：订阅流关了 Dispatch 还没返回，worker 漏了", round+1)
		}
		service.StopAll(context.Background())
	}

	// 循环里造的那些 goroutine 应该都回收了。留一点余量给测试框架本身
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+5 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutine 没回收干净：%d → %d", before, runtime.NumGoroutine())
}
