//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// configureProcess 给子进程单开一个**进程组**。
//
// `bash -c` 会 fork 子进程（管道两端、`&` 后台任务）。只杀 bash 自己，那些子进程
// 会留在超时已经结束之后继续跑。进程组让「超时」能一次性收掉整棵进程树。
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcess 对整个进程组发 SIGKILL，失败再退回杀单个进程。
func killProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// 负 pid 表示「这个进程组」
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return cmd.Process.Kill()
}
