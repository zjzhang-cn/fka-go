// Command fka 是这个 agent 的入口。
//
// ## 子命令
//
//	ask    无头跑一轮工具循环问答（不经过渠道）
//	tools  列出模型现在能看到的工具与五类放行情况
//	serve  常驻：接渠道、收消息、跑问答
//	login  扫码登录渠道账号
//
// 之后的子命令（doctor / db / channels …）按层补。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/zjzhang-cn/fka-go/internal/app"
	"github.com/zjzhang-cn/fka-go/internal/channels"
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
	case "serve":
		return runServe(ctx, args[1:])
	case "login":
		return runLogin(ctx, args[1:])
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
func build(providers ...channels.Provider) *app.App {
	return app.Build(app.Options{ChannelProviders: providers})
}

func printUsage() {
	fmt.Fprint(os.Stderr, `fka —— 通用 agent

用法：
  fka ask <问题>        无头跑一轮工具循环问答
  fka tools [--json]    列出模型现在能看到的工具与五类放行情况
  fka serve             常驻：接渠道、收消息、跑问答
  fka login [--account N]  扫码登录（不给就用第一个空槽位）
  fka help              本帮助

fka 自己不带任何能力：本事全靠 mcp.json 里的 MCP server 与 <安装根>/skills 下的技能。
必填的环境变量只有 LLM_API_KEY 与 LLM_MODEL。
LLM_TOOL_EFFECTS 默认只放行 read；MCP 工具一律是 external 类，要用得显式加上。

  fka ask --principal <身份> --session <会话> "…"
`)
}
