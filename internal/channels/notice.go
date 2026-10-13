// 本文件是渠道可收的**非过程通知**：一轮的账目（目前只有 token 用量）经它告诉渠道，
// 而不是塞进 `Emitter` 的三个过程事件里。
//
// ## 为什么单开一个可选接口，而不是给 `Emitter` 加方法
//
// `Emitter` 是一轮内的**过程事件流**（推理按块、每个工具一次、答案一次）——每个前端
// 都要看。用量是**一轮的账目**（轮末才有定论），多数前端用不上。把它做成 `Emitter`
// 的第四个方法，等于四个已有实现全部被迫补一个多数用不上的方法——**接口的宽度就是
// 替换的成本**。
//
// 所以按本包一贯的做法（`AddressResolver` / `MediaFetcher` / `EmitterProvider`）：
// **可选能力单独一个接口**，调用方用类型断言探测；不实现也照跑。
//
// ## 为什么通知是接口，不是「结构体 + Kind」
//
// 加一种通知 = 加一个实现（如 `UsageNotice`），**方法集不动、已有实现不用改**。
// 结构体 + Kind 的做法每加一种通知都要动那个结构体，且字段会越长越杂。
package channels

// Notice 一条发给渠道的非过程通知。**密封接口**：只有本包能实现，
// 于是 `Notify` 的实现方可以放心地对已知类型做断言、对未知类型静默忽略。
type Notice interface {
	// notice 密封标记。名字小写即「本包以外实现不了」。
	notice()
}

// TokenUsage 一轮的真实 token 用量。**用基本类型**——接缝不 import `llm`，
// 与 `Emitter` 同一条规矩；搬运由消息层那次适配完成。
//
// 三个都是**服务端报的事实**：全零表示服务端没给，不用估算冒充。
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// UsageNotice 一轮结束的用量通知。
type UsageNotice struct {
	Usage TokenUsage
}

func (UsageNotice) notice() {}

// Notifier **可选**能力：渠道想收非过程通知（用量等）就实现它。
//
// 与 `AddressResolver` / `EmitterProvider` 同一条规矩：调用方用**类型断言**探测，
// 不实现就不发——通知是尽力而为，发不出去不报错。
//
// **落在渠道对象上**（不是回显对象）：用量是「这个会话的账」，渠道自己决定显不显示、
// 怎么显示（CLI 缓冲到答复之后打一行；日志回显记一条）。回显与通知是两条独立的可选
// 能力，实现其一不影响另一条。
type Notifier interface {
	Notify(notice Notice)
}
