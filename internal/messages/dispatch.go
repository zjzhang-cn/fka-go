// 本文件是**常驻消息循环的形状**：一条 dispatcher 读订阅流，按「账号 + 身份」
// 把事件分给各自的 worker。
//
// ## 它为什么是这一层的事
//
// 「同一路会话要按顺序处理、不同路之间不用互相等」是**消息层的性质**：
// 接缝不认这个（它只管投递），渠道层不认这个（一个渠道实例只看自己那一个账号），
// 工具循环也不认。所以分片这件事整个落在 `internal/messages`。
//
// ## 为什么按「账号 + 身份」，而不是只按账号、也不是每条消息一个 goroutine
//
// **同一个「账号 + 身份」必须串行**。两件事都要求它：
//
//   - 历史是**单文件 append**（`llm/session.go`），并发写会交叉；
//   - 模型上下文有前后依赖——同一路两条消息并发跑，会各自基于**同一份旧历史**
//     作答，答完再交叉落进文件。那不会报错，历史会静静地错掉。
//
// 而不同「账号 + 身份」之间没有任何共享状态：`agent.Runner` 无状态（每次调用把
// 依赖按值传走）、`tools.Registry` 有锁、MCP 的 stdio 传输按 request id 解复用、
// 会话历史早就备好了 per-path 锁。所以并行是白捡的。
//
// **身份为什么也必须进分片键**：会话历史的文件名是 `渠道:账号:会话`，而**会话
// 这一格早就按身份分了命名空间**——web 的会话键显式前缀 PrincipalID（见
// `channels/web` 的 `sessionConversation`），ilink 的会话就是发信人的私聊。
// 两个身份天然写两份历史，却共用一个 `AccountID`（web 是**所有 token 共用一个
// 账号**），只按账号分片就把它们塞进同一个 worker 白等——这正是「两个 token
// 也并行不起来」的原因。身份为空时退回 `channel:account`，与旧行为一致。
//
// **按会话更细，但不要**：同一路里连续说话本来就该按顺序答——按会话分片会让
// 同一个人的两条消息并发作答，与直觉相悖。
//
// ## 并发上限为什么不用配
//
// worker 数量 = 见到过消息的「账号 × 身份」数，**不随消息量增长**。所以并发
// 天然有界，不需要额外的信号量或环境变量。想再压一层得先有第二个理由。
package messages

import (
	"context"
	"sync"

	"github.com/zjzhang-cn/fka-go/internal/channels"
)

// Dispatch 消息循环的形状：**一条 dispatcher，按账号分给各自的 worker**。
//
// 返回的函数**跑完订阅流并等所有 worker 退出才返回**——这是「停机时不漏
// goroutine」的唯一保证，调用方要等它就该等这个信号。
func Dispatch(handler *Handler, subscription *channels.Subscription) func() {
	return func() {
		queues := map[string]chan channels.Event{}
		var workers sync.WaitGroup

		// 订阅流关了之后**先关各账号的队列再等 worker**：不关的话那些
		// `for range` 永不结束，goroutine 就漏在进程里
		defer func() {
			for _, queue := range queues {
				close(queue)
			}
			workers.Wait()
		}()

		for event := range subscription.C {
			key := shardKeyOf(event)
			queue, known := queues[key]
			if !known {
				// **无缓冲**是有意的：账号的 worker 正忙时，dispatcher 就停在这儿
				// 等，而不是把消息堆起来把「处理不过来」藏起来。压力接着堵住订阅
				// 流，堵到接缝的 256 满了，接缝自己会记 Warn 并丢弃——
				// 沿用 seam.go 早就定下的语义，不在这里另发明一套
				queue = make(chan channels.Event)
				queues[key] = queue
				workers.Add(1)
				go runAccountWorker(handler, queue, &workers)
			}
			queue <- event
		}
	}
}

// runAccountWorker 一个账号的 worker。**这个账号的消息逐条串行**。
//
// `Handle` 拿的是 `context.Background()` 而不是停机信号：正在跑的那一轮已经
// 开始回答了，砍掉它等于让用户等来一句没有下文的话。宁可让进程带着这一轮退出。
func runAccountWorker(handler *Handler, queue <-chan channels.Event, workers *sync.WaitGroup) {
	defer workers.Done()
	for event := range queue {
		handler.Handle(context.Background(), event)
	}
}

// shardKeyOf 分片键。**带渠道种类与身份**：
//
//   - 带渠道：AccountID 只在渠道内唯一（渠道种类之间也可能撞号），接缝自己的
//     查找键也是 `channel:account`——见 seam.go 的 `key()`；
//   - 带身份：同一账号下不同身份各写自己的会话历史，并行不冲突（web 的多 token
//     就靠这一格拆开）。身份为空时退回 `channel:account`，与旧行为逐字一致。
func shardKeyOf(event channels.Event) string {
	key := event.Message.ChannelID + ":" + event.Message.AccountID
	if principal := event.Message.PrincipalID; principal != "" {
		key += ":" + principal
	}
	return key
}
