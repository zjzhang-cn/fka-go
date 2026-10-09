//go:build windows

package main

import "os/exec"

// configureProcess 在 Windows 上什么都不做。
//
// 真正的「杀整棵进程树」在 Windows 上要靠 Job Object 或 `taskkill /T`，纯 Go
// 标准库给不了。这里只保证能编出来、能跑；超时杀不掉子孙进程是**已知的平台差异**，
// 不是待办——本 server 的主要目标是 Unix 家族。
func configureProcess(cmd *exec.Cmd) {}

// killProcess 只杀 shell 自己。
func killProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
