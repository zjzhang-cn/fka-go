// Command fka 是家庭知识管家（Go 版）的入口。
//
// ## 子命令
//
//	ask    无头跑一轮工具循环问答（不经过微信）。**现在唯一能端到端验证 agent 的入口**
//	tools  列出模型现在能看到的工具与五类放行情况
//
// 之后的子命令（serve / mcp-docs / mcp-memory / doctor / db / search …）按层补。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/zjzhang-cn/fka-go/internal/app"
	"github.com/zjzhang-cn/fka-go/internal/config"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// exitCode 退出码：0 成功、1 预期内的失败、2 用法错。
// 与原实现的约定一致——脚本靠它区分「环境没配好」与「你敲错了命令」。
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

func run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return exitUsage
	}

	// Ctrl-C 与 SIGTERM 都要走同一套清理：先断 MCP 子进程，再关日志。
	// **没有它的话 stdio 服务器会留在后台**
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// CLI 的 stdout 是给人和脚本消费的结果输出，不该混入内部日志
	// （那些仍然完整写进日志文件）
	config.Log().SetConsoleLevel(config.LevelWarn)

	switch args[0] {
	case "ask":
		return runAsk(ctx, args[1:])
	case "tools":
		return runTools(ctx, args[1:])
	case "help", "-h", "--help":
		printUsage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "不认识的命令：%s\n\n", args[0])
		printUsage()
		return exitUsage
	}
}

// build 装配一份 app。**每个子命令自己装**——CLI 不常驻，装一次就扔。
func build() *app.App {
	storageRoot, source := app.StorageRootFromEnv()
	if source == "fallback" {
		// 回退必须显式：静默回退会让「生产忘了挂 NAS」表现为「文件存在但找不到」
		config.Log().Warn("NAS 挂载点不存在，原始文件将落到安装根下（这不是生产配置）",
			config.Context{"root": storageRoot})
	}
	return app.Build(app.Options{StorageRoot: storageRoot, AdminWxid: os.Getenv("ADMIN_WXID")})
}

func printUsage() {
	fmt.Fprint(os.Stderr, `fka —— 家庭知识管家（Go 版）

用法：
  fka ask <问题>        无头跑一轮工具循环问答
  fka tools [--json]    列出模型现在能看到的工具与五类放行情况
  fka help              本帮助

环境变量见 .env.example。必填的只有 LLM_API_KEY 与 LLM_MODEL；
LLM_TOOL_EFFECTS 默认只放行 read，要用记忆/文档工具需显式加 external。
`)
}
