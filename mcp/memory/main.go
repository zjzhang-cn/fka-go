// Command fka-memory 是**家庭记忆的 MCP server**（stdio）。
//
// ## 它是什么
//
// 一个**独立的可执行程序**。主程序（`cmd/fka`）从 `mcp.json` 把它当子进程拉起，
// 之后两者之间只说 JSON-RPC：
//
//	{
//	  "mcpServers": {
//	    "memory": { "command": "/path/to/fka-memory", "args": ["--db", "/path/to/db.sqlite"] }
//	  }
//	}
//
// 主程序**不再直接碰 memories 表**——存储整个沉到本进程里。
//
// ## 为什么不写成主程序的一个子命令
//
// 每个 server 一个目录、一个可执行程序：它可以单独构建、单独部署、单独重启，
// 崩了不影响主程序，也不需要主程序为了拉起它而多一条命令。
//
// ## stdout 一个字都不能有
//
// stdout 是 JSON-RPC 通道。**任何** fmt.Println 都会插进协议流里把 server 打挂。
// 诊断信息一律走 config.Log()（stderr + 按天轮转的日志文件）。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/mark3labs/mcp-go/server"

	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/mcpboot"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	// 信号只用来让 defer 跑完（关库、关日志）。**不要**在这里打任何东西到 stdout
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	path, err := mcpboot.ResolveDBPath(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "解析数据库路径失败："+err.Error())
		return 1
	}

	// **先迁移再开服务**：server 是写方，它面对的库必须已跟上代码。迁移失败就拒绝
	// 启动——让主程序明确看到「server 没起来」，而不是让它调一个查不到表的 server
	db, err := mcpboot.OpenAndMigrate(ctx, path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "记忆 server 起不来："+err.Error())
		return 1
	}
	defer func() { _ = db.Close() }()

	config.Log().Info("记忆 MCP server 就绪", config.Context{"path": path})

	mcpServer := server.NewMCPServer("fka-memory", "0.1.0",
		server.WithToolCapabilities(true))
	register(mcpServer, db)

	// ServeStdio 独占 stdin/stdout。**不要**在它前后往 stdout 打任何东西
	if err := server.ServeStdio(mcpServer); err != nil {
		fmt.Fprintln(os.Stderr, "记忆 server 退出："+err.Error())
		return 1
	}
	return 0
}
