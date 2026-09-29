// 本文件只管一件事：**没给 `--session` 时，那个会话 id 从哪来**。
//
// ## 为什么不跟记忆 server 共享一份 newID
//
// `mcp/memory` 里有一份同形状的 UUID 生成器，而那个 server 被边界测试钉死成
// **不许 import 树外的任何包**（它要能单独构建、部署、换掉）。所以那份是刻意的副本。
// 这一侧则相反：仓库里**没有**共享的 id 包（`internal/ids` 已随存储那半边搬走），
// 为一个 12 行的 stdlib 包装新建一个包不划算，于是就地写着。
//
// 真正的原因是**形状要一样**：`cli-<uuid>` 与 `memory-<uuid>` 并排出现在日志与
// 文件名里时，一眼能看出是同一类标识，而不是两段来路不明的随机十六进制。
package main

import (
	"crypto/rand"
	"fmt"
	"time"
)

// cliSessionPrefix 命令行会话的前缀。**留着它是为了认得出来**——
// 日志与 `data/history/` 下的文件名里出现 `cli-…` 就知道是命令行来的
// （渠道那边是对方的 wxid）。
//
// ## `cli-` + UUID 正好 40 字符，这不是巧合
//
// `llm.safeSegment` 把会话段截到 40 字符。40 刚好用满：一个字符都不浪费，
// 也**不会**被截。而截断是静默的——两个只在末尾不同的会话会落进同一个文件，
// 于是「上一条 CLI 问的什么」又回来了，只是换了个更隐蔽的方式。
// 改前缀长度前先看一眼 `safeSegment`。
const cliSessionPrefix = "cli-"

// newCliSessionID 命令行那一轮用的会话 id：**每次问都是一个新会话**。
//
// ## 为什么不能是固定的一个名字
//
// 会话历史落在 `<安装根>/data/history/<会话>.jsonl`。兜底曾是常量 `"cli"`，
// 于是「上一条 CLI 问的什么」会跟着**下一条毫不相关的命令**进上下文——
// 症状是模型忽然提起你半小时前随口问过的那件事，而命令行里**什么都没变**。
// 每轮一个新 id 之后，`ask` 就是名副其实的一次性问答；想接着聊就显式给
// `--session`（或 `FKA_SESSION`），那仍然是连续会话。
func newCliSessionID() string {
	return cliSessionPrefix + newUUIDv4()
}

// newUUIDv4 一个 v4 形状的 UUID（8-4-4-4-12）。
//
// **crypto/rand 而不是 math/rand**：这个 id 唯一的作用是「每次都不一样」，
// 而可预测的随机数在多开几个终端时恰好最容易撞。撞了不会立刻出事——
// 它表现为两个不相关的会话共享历史，**排查起来非常费劲**。
//
// 读不到随机源是系统级异常，但**不该因此让一次问答跑不起来**：退化成时间戳。
// 那时确实可能撞（同一纳秒），但撞的表现是历史串了，**不是静默丢东西**。
func newUUIDv4() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	// 版本 4 与变体位：形状上就是 UUID，日志与文件名里认得出
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}
