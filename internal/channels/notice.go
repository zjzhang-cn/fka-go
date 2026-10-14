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
// 四个都是**服务端报的事实**：全零表示服务端没给，不用估算冒充。
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CachedTokens 命中上下文缓存的输入 token（`prompt_tokens_details.cached_tokens`），
	// **是 PromptTokens 的子集**。不是所有端都报；没有就是 0。
	CachedTokens int
}

// UsageNotice 一轮结束的用量通知。
type UsageNotice struct {
	Usage TokenUsage
}

func (UsageNotice) notice() {}

// TurnStateNotice 会话**执行中状态**的变化：true = 一轮开始、false = 一轮结束。
//
// 前端据此把「发送」切成「停止」（执行中）并在结束时切回来。**由消息层发**——
// 它才知道「一条入站消息正在处理」（含空问题/缺身份这类不走工具循环的拒答）。
type TurnStateNotice struct {
	Running bool
}

func (TurnStateNotice) notice() {}

// MediaStoredNotice 入站文件已落沙盒的通知（`FKA_MEDIA_MODE=both/sandbox`）。
//
// **只带结构化事实**——落进沙盒的相对路径（自带文件名）。「怎么讲给用户听」由各前端
// 自己拼：web 收 SSE 帧后自行拼成一行系统提示、CLI 自己拼成一行。服务端**不预先拼好
// 一整句话**——这与 `UsageNotice` 只给数字、由前端拼「用量：…」是同一条规矩。
//
// 为什么单开一个类型：它是「系统做了一件事」的账目，不是模型说的话——与 UsageNotice 同类，
// 但不该混进用量；也**不该走 Senders().Text**（那会变成一条聊天气泡）。收得到通知的前端
// （web 的 SSE 回显、CLI）用它；收不到的（iLink）由消息层退回文本，至少让人看见。
type MediaStoredNotice struct {
	// Paths 已落沙盒的相对路径（如 `inbox/a.png`）。落点工具没回可解析的行时为空。
	Paths []string
}

func (MediaStoredNotice) notice() {}

// Notifier **可选**能力：渠道想收非过程通知（用量等）就实现它。
//
// 与 `AddressResolver` / `EmitterProvider` 同一条规矩：调用方用**类型断言**探测，
// 不实现就不发——通知是尽力而为，发不出去不报错。
//
// **谁能收到**由渠道自己决定：单会话渠道（CLI）实现 `Channel` 自己（它自己就是
// 「那一场会话」）；多会话渠道把 `Notify` 长在**逐条消息的回显对象**上——用量属于
// 某条会话，而渠道实例是多会话共享的、没有「当前会话」可指，逐条消息造的回显对象
// 才知道发给哪个房间。接缝的适配器（`messages.channelEcho`）先问渠道、再问回显对象。
type Notifier interface {
	Notify(notice Notice)
}
