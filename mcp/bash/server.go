// bash MCP server 的工具实现。
//
// ## 这个 server 提供什么
//
// 一个 **`run` 工具**：在沙盒里执行一条 shell 命令，返回退出码与 stdout/stderr。
// 沙盒的边界（工作目录、超时、输出上限、命令白名单/黑名单）由 sandbox.go 强制，
// 这里只负责把工具参数读出来、把结果说成模型看得懂的一段话。
//
// ## 为什么失败也返 `IsError` 的普通结果，而不是抛异常
//
// 与记忆 server 同一条：抛异常会让模型只看到「工具坏了」，不知道下一步该改什么
// （cwd 越界？命令被禁？超时？）。返回一段点名原因的文字，它下一轮就能自己修。
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/zjzhang-cn/fka-go/mcp/bash/internal/log"
)

// ToolRun 工具名。**刻意不带前缀**——前缀由上层注册表加（`mcp__bash__`）。
const ToolRun = "run"

// bashServer 工具实现。**只有本目录内的 main 与测试用得到**。
type bashServer struct {
	sandbox *Sandbox
}

// register 把工具挂到 MCP server 上。sandbox 已按配置建好。
func register(mcpServer *server.MCPServer, sandbox *Sandbox) {
	s := &bashServer{sandbox: sandbox}

	mcpServer.AddTool(mcp.NewTool(ToolRun,
		mcp.WithDescription(
			"在一个受限沙盒里执行一条 shell 命令，返回退出码与输出。"+
				"命令的工作目录固定在沙盒根或其相对子目录内；有超时与输出上限；"+
				"危险命令可能被策略拒绝。这是**护栏不是越狱墙**——不要用它去碰系统。"),
		mcp.WithString("command",
			mcp.Description("要执行的 shell 命令（会交给 bash -c）。"),
			mcp.Required(),
		),
		mcp.WithString("cwd",
			mcp.Description("沙盒根下的相对工作目录，默认就是沙盒根。不允许绝对路径或 .. 越界。"),
		),
		mcp.WithNumber("timeout_sec",
			mcp.Description("超时秒数。不填用默认值，超过上限会被夹到上限。"),
		),
	), s.handleRun)
}

func (s *bashServer) handleRun(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	command := strings.TrimSpace(request.GetString("command", ""))
	if command == "" {
		return fail("command 是必填的：要执行哪条 shell 命令。"), nil
	}

	// **声明是 number（`mcp.WithNumber`），就必须按数字读。**
	// `GetString` 只在值是 Go `string` 时返回，而 JSON 数字解出来是 `float64`——
	// 于是超时参数会永远取默认值，而界面上看不出任何异常（见 mcp/memory 的同款坑）。
	timeoutSec := request.GetInt("timeout_sec", 0)

	result, err := s.sandbox.Run(ctx, RunRequest{
		Command: command,
		Cwd:     request.GetString("cwd", ""),
		Timeout: time.Duration(timeoutSec) * time.Second,
	})
	if err != nil {
		// 策略拒绝 / cwd 越界 / 启动失败：这是**没能跑起来**，不是命令跑完的退出码
		log.Log().Warn("命令被拒或没能启动", log.Context{"error": err.Error()})
		return fail(err.Error()), nil
	}

	body := formatResult(result)
	if result.TimedOut {
		return fail(body), nil
	}
	return text(body), nil
}

// formatResult 把一条命令的结果拼成给模型看的一段话。
//
// **退出码非 0 不算工具失败**：命令跑了、返回了，那是一个正常结果；模型要的正是
// 退出码与 stderr。只有「没能跑起来」和「超时被杀」才走 IsError（在 handler 里判）。
func formatResult(r Result) string {
	var b strings.Builder
	switch {
	case r.TimedOut:
		fmt.Fprintf(&b, "命令超时（跑了 %s）已被终止。\n", r.Duration.Round(time.Millisecond))
	case r.ExitCode != 0:
		fmt.Fprintf(&b, "命令以退出码 %d 结束。\n", r.ExitCode)
	default:
		b.WriteString("命令执行成功（退出码 0）。\n")
	}

	writeStream(&b, "stdout", r.Stdout)
	writeStream(&b, "stderr", r.Stderr)
	if r.Stdout == "" && r.Stderr == "" {
		b.WriteString("（没有输出）\n")
	}
	if r.Truncated {
		b.WriteString("（输出超过上限，已截断）\n")
	}
	return b.String()
}

func writeStream(b *strings.Builder, name, body string) {
	if body == "" {
		return
	}
	b.WriteString(name)
	b.WriteString(":\n")
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteByte('\n')
	}
}

// text 造一条成功结果。
func text(body string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{mcp.NewTextContent(body)}}
}

// fail 造一条**给模型看**的失败。isError=true 让它知道这次没成功。
func fail(body string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{mcp.NewTextContent(body)}}
}
