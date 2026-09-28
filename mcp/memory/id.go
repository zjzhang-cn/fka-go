package main

import (
	"crypto/rand"
	"fmt"
	"time"
)

// newID 记忆的 id。**UUID 形状**（8-4-4-4-12）而不是随便一段随机十六进制：
// 日志、文件路径、以及将来可能的引用里，形状一致的东西更好认。
//
// 用 crypto/rand 而不是 math/rand：id 撞了会**静默丢掉一条记忆**
// （唯一键冲突 + onConflictDoNothing），而那种错极难查。
func newID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// 读不到随机源是系统级的异常，但**不能因此让记记忆失败**——
		// 退化成时间戳：会撞的概率在这里可以接受（且撞了会报错，不是静默丢）
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	// 版本 4 与变体位，符合 UUID 的形状约定
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

func nowMs() int64 { return time.Now().UnixMilli() }
