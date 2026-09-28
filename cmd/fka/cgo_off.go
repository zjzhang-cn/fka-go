//go:build !cgo

package main

// cgoEnabled 这个二进制是**不带 cgo** 编的，符合本项目的硬约束。
const cgoEnabled = false
