//go:build cgo

package main

// cgoEnabled 这个二进制是**带 cgo** 编的。
//
// 本项目的硬约束是零 CGO（见 docs/decisions.md），所以 `fka version` 会打出
// 「← 不该是 on」提示。
//
// 判定只能靠 build tag：`CGO_ENABLED=0` 时 cgo 包根本不会被编译，
// `cgo` 这个 tag 也不会被设置——所以**没有别的办法从运行时问出这件事**。
const cgoEnabled = true
