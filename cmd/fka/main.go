// Command fka 是这个 agent 的入口。
//
// ## 版本信息由构建时注入
//
// 下面三个变量由 `make build` 通过 -ldflags -X 写进来。**给它们默认值是刻意的**：
// `go build ./...`（不��� Makefile）也能编，编出来的版本是 dev——
// 而「编出来了但版本说不清」比「编不出来」更难查。
//
// ## 子命令
//
//	ask    无头跑一轮工具循环问答（不经过渠道）
//	tools  列出模型现在能看到的工具与五类放行情况
//	serve  常驻：接渠道、收消息、跑问答
//	login  扫码登录渠道账号
//	version  版本、提交、构建时间
//
// 之后的子命令（doctor / db / channels …）按层补。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/zjzhang-cn/fka-go/internal/app"
	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/config"
)

// 由 `make build` 通过 -ldflags -X 注入。默认值给 `go build` 直编的人用。
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
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

	// 控制台日志级别：**先于任何子命令**定下来。
	//
	// 定成「先设默认、再让 --log-level 覆盖」，而不是让 applyLogLevel 自己
	// 设——这样不给参数时的行为**一字未变**，而 `--log-level` 只是压过它。
	//
	// 必须早于 switch：首个入站消息可能在任何子命令的代码跑起来之前就记日志。
	config.Log().SetConsoleLevel(defaultConsoleLevel)
	if err := applyLogLevel(args); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		printUsage()
		return exitUsage
	}

	switch args[0] {
	case "ask":
		return runAsk(ctx, args[1:])
	case "tools":
		return runTools(ctx, args[1:])
	case "serve":
		return runServe(ctx, args[1:])
	case "login":
		return runLogin(ctx, args[1:])
	case "version":
		return runVersion()
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
  fka version           版本信息
  fka help              本帮助

fka 自己不带任何能力：本事全靠 mcp.json 里的 MCP server 与 <安装根>/skills 下的技能。
必填的环境变量只有 LLM_API_KEY 与 LLM_MODEL。
LLM_TOOL_EFFECTS 默认只放行 read；MCP 工具一律是 external 类，要用得显式加上。

  fka ask --principal <身份> --session <会话> "…"
`)

	// 日志那行印在正文之外：**它是每个子命令都认的**，不属于任何一个
	printLogLevelUsage(os.Stderr)
}

// runVersion 打版本信息。
//
// **第一行是纯版本号、后面才是提交与日期**——那是为了让人能直接
// `fka version | head -1` 拿去比对，或在 CI 里 grep。
func runVersion() int {
	fmt.Println(version)
	fmt.Println("commit:    " + commit)
	fmt.Println("built:     " + buildDate)
	fmt.Println("channel:   " + runtime.GOOS + "/" + runtime.GOARCH)
	fmt.Println("cgo:       " + cgoFlag())
	return exitOK
}

// cgoFlag 这个二进制是不是带 cgo 编的。
//
// **零 CGO 是硬约束**，而它平时不会以任何形式冒出来——直到某天在别的机器上
// 跑不起来。所以让它能被问出来。实际取值由 build tag 决定，见 cgo_on/off.go。
func cgoFlag() string {
	if cgoEnabled {
		return "on  ← 不该是 on"
	}
	return "off"
}
