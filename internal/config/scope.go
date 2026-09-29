// 本文件解决一个问题：**一条日志怎么知道它属于哪个账号**。
//
// ## 为什么不能靠「每处手写」
//
// 一轮问答要穿过消息层 → 工具循环 → 模型客户端 → 工具源，四层各有各的日志点。
// 账号只在最上面那层（`messages`）是现成的，往下就断了——而让人在每个日志点
// 手动补一个 `"account": …`，等于把「记得写」这件事交给每一次将来新增的代码。
// 漏一处，那条日志就成了排查时的假线索：**看起来完整，实际缺了归属**。
//
// ## 为什么不能用全局变量
//
// 消息处理是**按账号并行的**（见 `internal/messages/dispatch.go`）。
// 进程级的「当前账号」在两个 goroutine 之间来回写，出来的日志必然串号——
// 而串号的日志比没有日志更坏，它会把注意力引到错误的账号上。
//
// ## 所以绑在 ctx 上
//
// `context.Context` 是唯一能穿过 `llm.ChatClient` 与工具调用而不改它们签名的载体。
// 消息层绑一次，往下每一层派生出来的 ctx 都带着它，之后打日志时由 `Fields`
// 自动合并——**不用记，也就不会漏**。
//
// 代价是 `context.Context` 与 `config.Context` 这两个名字会出现在同一行里。
// 认这两个的区别：**`context.Context` 传「这一轮是谁」，`config.Context` 是
// 「这一条日志额外说明什么」**。
package config

import "context"

// scopeKey 绑字段用的 ctx 键。**用私有空结构体当键**——用字符串当键的话，
// 别的包也能误写进来，而那时合并进来的东西没人负责。
type scopeKey struct{}

// Bind 把一组字段绑到 ctx 上。**派生出来的 ctx 都带着它**。
//
// 空的 fields 直接原样返回——绑一个空集合只会白白多一层 ctx。
func Bind(ctx context.Context, fields Context) context.Context {
	if len(fields) == 0 {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}

	merged := make(Context, len(FieldsOf(ctx))+len(fields))
	for key, value := range FieldsOf(ctx) {
		merged[key] = value
	}
	for key, value := range fields {
		merged[key] = value
	}
	return context.WithValue(ctx, scopeKey{}, merged)
}

// FieldsOf 取回 ctx 上绑着的那组字段。**没绑过返回 nil**，
// 所以它可以直接 range，也能直接判 `== nil`。
func FieldsOf(ctx context.Context) Context {
	if ctx == nil {
		return nil
	}
	fields, _ := ctx.Value(scopeKey{}).(Context)
	return fields
}

// Fields 把 ctx 上绑着的字段与 extra 合并，返回打日志要用的那一组。
//
// **extra 覆盖绑定的那些**：同一轮里更具体的信息（这一步的工具名、这一次的步号）
// 该盖过通用的（账号、会话）。而调用方仍然**不必**把 account / messageId
// 再抄一遍——那正是这里要消掉的东西。
//
// 两者都没有时返回 nil，让 `Logger.write` 走「context 为空就不写字段」那条路。
func Fields(ctx context.Context, extra Context) Context {
	bound := FieldsOf(ctx)
	if len(bound) == 0 && len(extra) == 0 {
		return nil
	}

	merged := make(Context, len(bound)+len(extra))
	for key, value := range bound {
		merged[key] = value
	}
	for key, value := range extra {
		merged[key] = value
	}
	return merged
}
