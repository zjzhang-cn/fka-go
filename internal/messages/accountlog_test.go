package messages_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/messages"
)

// readLogs 读日志文件里**这一轮**写下的那些行。
//
// ## 为什么读文件，而不是换掉 os.Stdout 去抓控制台
//
// 换 `os.Stdout` 那个做法更省事，但它是**有竞态的**——`Logger.write` 每次写入时
// 现读 `os.Stdout`，而恢复函数在写它，`-race` 会当场报出来。日志文件那条路
// 没有这个问题：写入全在 `Logger.mu` 之下，而本测试包不并行。
//
// ## 为什么不需要管级别
//
// `Logger.write` 的注释写明「文件输出：始终全量」——控制台按级别过滤，文件不过。
// 所以这里**不用**去改 `LOG_LEVEL`，也就不会在测试之间互相影响。
//
// ## 怎么只取这一轮
//
// 文件是包级 logger 写的，**同包所有用例共用一份**，而且按天累积——上一次跑留下的行
// 也都在里面。所以按本轮特有的消息号过滤：`logOwnerMessage` 里的消息号是这条用例独占的。
//
// **过滤串要带上字段的写法**（`messageId=…` 而不是光一个消息号）：只拿消息号的话，
// 升级前那批 JSON 格式的老行也会被捞进来，逐行验归属时全红——而红的原因跟归属无关。
// 写成 `键=值` 之后，老格式的行天然不匹配（它们的字段是 `"messageId":"…"`）。
func readLogs(t *testing.T, since string) string {
	t.Helper()

	path := config.LogFilePath(time.Now().UTC())
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到日志文件 %s：%v", path, err)
	}

	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if since == "" || strings.Contains(line, since) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// logOwnerMessage 这条用例独占的消息号。**别的用例不许用**——
// readLogs 靠它把这一轮的行从同包其他用例的行里挑出来。
const logOwnerMessage = "logscope-owner"

// Test每一跳的日志都带账号 这是整件事的验收：**从收到消息到工具返回，每一条
// 日志都要能看出是哪个账号触发的**。
//
// ## 为什么要抓真实输出，而不是查代码
//
// 「字段会自动合并」是 `config.Fields` 自己的性质，用例在 config 包里。
// 真正会坏的是**链路**——ctx 到底有没有从消息层传到工具层。而那种坏法在代码
// 上完全看不出来：`config.Log().Debug(config.TypeTOOL, "工具调用", config.Context{...})` 编译得过、
// 包内测试全绿，只是日志里没有归属。所以必须真跑一遍、看真的输出。
func Test每一跳的日志都带账号(t *testing.T) {
	f := newFixture(t, "答完了")
	// 默认行为会调一次工具再作答——**工具那一跳才是这条用例的关键**
	message := textMessage("我们去年三亚玩得怎么样")
	message.MessageID = logOwnerMessage
	f.deliver(t, message)
	logged := readLogs(t, "messageId="+logOwnerMessage)

	if strings.TrimSpace(logged) == "" {
		t.Fatal("一条日志都没抓到，测不到任何东西")
	}

	// 这一轮该出现的节点，**分属不同层**：
	// 收发在消息层、提示词在工具循环、调用与结果在注册表
	//
	// **刻意不含模型客户端那两条**（`提交模型请求` / `模型返回` / `模型的推理`）——
	// 这个夹具用的是**假 ChatClient**，`internal/llm/openai` 那一层压根没经过。
	// 把它们写进来，这条用例会永远红，而红的原因跟归属无关。
	// 那两条由 `internal/llm/openai/logscope_test.go` 单独钉。
	wantMessages := []string{
		"收到消息",   // 消息层
		"提示词已拼接", // 工具循环
		"工具调用",   // 注册表
		"工具返回",   // 注册表
		"已作答",    // 消息层
		"答复已发出",  // 消息层
	}
	for _, want := range wantMessages {
		if !strings.Contains(logged, want) {
			t.Errorf("该有「%s」这条日志，实际输出：\n%s", want, logged)
		}
	}

	// **逐行验**，且只在带归属字段的那几类上验。
	// 「出现过一次 account 就算数」太松：日志里哪怕只有一行带账号也能过，
	// 而漏掉的恰恰是工具层那几行——所以这里数的是**每一行**
	checked := 0
	for _, line := range strings.Split(logged, "\n") {
		if !carriesOwnership(line) {
			continue
		}
		checked++
		if !strings.Contains(line, "account=acct-1") {
			t.Errorf("这条日志该带 acct-1 的归属，实际：\n%s", line)
		}
		if !strings.Contains(line, "messageId="+logOwnerMessage) {
			t.Errorf("这条日志该带消息号（同账号的多条消息要分得开），实际：\n%s", line)
		}
	}
	if checked < 4 {
		t.Errorf("逐行检查的样本太少（%d 行），这条用例可能没真跑到工具那一跳", checked)
	}
}

// Test两个账号的日志不会串 **并行之后才有的风险**：一旦归属是靠某个进程级状态
// 传递的，两个账号的日志就会互相盖。而单账号的用例永远测不出来。
func Test两个账号的日志不会串(t *testing.T) {
	f := newFixtureWithAccounts(t, "好", "acct-1", "acct-2")
	f.model.noToolCall = true

	subscription := f.service.Subscribe()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		messages.Dispatch(f.handler, subscription)()
	}()
	f.service.StartAll(context.Background())
	defer func() {
		subscription.Close()
		<-stopped
	}()

	f.channelOf("acct-1").onMessage(messageOf("acct-1", "msg-A", "第一个"))
	f.channelOf("acct-2").onMessage(messageOf("acct-2", "msg-B", "第二个"))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.channelOf("acct-1").sentTexts()) > 0 && len(f.channelOf("acct-2").sentTexts()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	logged := readLogs(t, "messageId=msg-")

	// 每条「答复已发出」都要同时对上账号**与它自己的消息号**：
	// 消息号不同才说明两条真的各自走完了自己的链路，而不是互相抄了归属
	pairs := map[string]string{"acct-1": "msg-A", "acct-2": "msg-B"}
	matched := map[string]bool{}
	for _, line := range strings.Split(logged, "\n") {
		if !strings.Contains(line, "答复已发出") {
			continue
		}
		for account, messageID := range pairs {
			if !strings.Contains(line, "account="+account) {
				continue
			}
			matched[account] = true
			if !strings.Contains(line, "messageId="+messageID) {
				t.Errorf("%s 的答复带错了消息号（串号了）：\n%s", account, line)
			}
		}
	}
	for _, account := range []string{"acct-1", "acct-2"} {
		if !matched[account] {
			t.Errorf("没看到 %s 的答复日志，实际输出：\n%s", account, logged)
		}
	}
}

// carriesOwnership 这行日志**是不是该带归属**。
//
// 只挑「每一轮都会打」的那几类，且**刻意不包含模型客户端那两条**——
// 单元测试跑的是假模型，`llm` 那一层在这个夹具里压根没经过，
// 把它们算进样本只会让这条用例永远失败。
func carriesOwnership(line string) bool {
	return strings.Contains(line, "收到消息") ||
		strings.Contains(line, "已作答") ||
		strings.Contains(line, "答复已发出") ||
		strings.Contains(line, "提示词已拼接") ||
		strings.Contains(line, "工具调用") ||
		strings.Contains(line, "工具返回")
}
