// 本文件是**取消/停止**的方向：业务层把「取消某会话进行中的一轮」的入口交给渠道，
// 于是网页那类带按钮的前端能中途喊停（CLI 的 Ctrl-C 早就走的是 ctx 取消，不走这里）。
//
// ## 为什么是一个注入进来的可选接口
//
// 「哪一轮在跑」是**消息层的知识**（它调度 worker、持有 turn ctx），渠道看不见；
// 而「用户点了停止」是**渠道的知识**。两者要接上，只能在装配根把消息层的入口递给
// 渠道——照 `AddressResolver` / `MediaFetcher` 的样子做成可选能力：渠道实现
// `TurnControllable` 才拿得到，不实现就只是没有「停止」这个功能。
package channels

// TurnController 取消某个会话正在跑的那一轮。**由业务层实现**（消息层知道哪一轮
// 在跑），经 `TurnControllable` 注入给渠道。
//
// 会话键是 `渠道:账号:会话`（见 `SessionKeyOf`）——渠道按同一条规则算出自己要停的
// 是哪一场，不必认识消息层的分片结构。
type TurnController interface {
	// CancelTurn 取消这个会话进行中的一轮。返回是否真的取消了一个
	// （false = 当时没有在跑，调用方据此回一句「没有进行中的一轮」而不是假装成功）。
	CancelTurn(sessionKey string) bool
}

// TurnControllable **可选**能力：渠道接受一个取消入口。装配根在启动前把它递给
// 每个实现了它的渠道实例。
type TurnControllable interface {
	SetTurnController(controller TurnController)
}
