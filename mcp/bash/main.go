// Command fka-bash 是**沙盒 bash 执行的 MCP server**（stdio）。
//
// ## 它是什么
//
// 一个**独立的可执行程序**，也是一个**自给自足的项目**：沙盒根归它解析、目录归它建、
// 策略归它定、日志归它写。它不 import agent 的任何包，也不与别的 server 共享一行
// 代码——可以单独构建、单独部署、单独重启，崩了不影响主程序。
//
// 主程序（`cmd/fka`）从 `mcp.json` 把它当子进程拉起，之后两者之间只说 JSON-RPC：
//
//	{
//	  "mcpServers": {
//	    "bash": { "command": "/path/to/fka-bash", "args": ["--root", "/path/to/sandbox"] }
//	  }
//	}
//
// 主程序要放行 `external`（**MCP 工具一律是 external**）：
//
//	LLM_TOOL_EFFECTS=read,external
//
// ## 默认沙盒根是**自己的**目录
//
// 默认 `<安装根>/sandbox`。命令的 cwd 固定在这棵树里，HOME 与 TMPDIR 也指到这里，
// 于是 `~`、临时文件都落在沙盒内。
//
// ## 默认用 Bubblewrap 做真实隔离
//
// 默认模式是 `bwrap`：用 Linux 命名空间起一个沙盒，**文件系统只有沙盒根可写**
// （`/` 只读绑定），网络/PID/IPC/UTS/user 全部另起。找不到 `bwrap` 就**拒绝启动**，
// 不会静默退化成不隔离的直接执行。
//
// 配置项：
//
//	--mode bwrap|direct     沙盒引擎（默认 bwrap；direct 是纯 Go 退化档，不隔离）
//	--bwrap PATH            Bubblewrap 路径（默认从 PATH 找）
//	--share-net             保留宿主网络（默认起独立网络命名空间）
//	BASH_BWRAP_ARGS        追加给 bwrap 的参数（空白分隔）
//
// `direct` 模式只固定 cwd，**不是安全边界**——`cd /` 照样能离开工作目录。细节见
// sandbox.go 文件头。
//
// ## stdout 一个字都不能有
//
// stdout 是 JSON-RPC 通道。**任何** fmt.Println 都会插进协议流里把 server 打挂。
// 诊断信息一律走 log.Log()（stderr + 按天轮转的日志文件）。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/server"

	"github.com/zjzhang-cn/fka-go/mcp/bash/internal/log"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	// **不要**在这个进程里往 stdout 打任何东西。信号由 SDK 自己接（ServeStdio
	// 收到 SIGINT / SIGTERM 会返回 context.Canceled），这里没有需要 defer 清理的
	// 资源，所以不必再自建 NotifyContext——留着那个 ctx 反而是个没人用的变量。
	sandbox, err := NewSandbox(Options{
		Root:         resolveRoot(args),
		Allow:        sandboxAllow(args),
		Deny:         sandboxDeny(args),
		Mode:         firstNonEmpty(flagValue(args, "--mode"), os.Getenv("BASH_SANDBOX_MODE")),
		Bwrap:        firstNonEmpty(flagValue(args, "--bwrap"), os.Getenv("BASH_BWRAP")),
		BwrapArgs:    strings.Fields(os.Getenv("BASH_BWRAP_ARGS")),
		ShareNetwork: shareNetwork(args),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "bash 沙盒起不来："+err.Error())
		return 1
	}

	log.Log().Info("bash MCP server 就绪", log.Context{
		"root":  sandbox.Root,
		"mode":  sandbox.Mode,
		"allow": len(sandbox.Allow),
		"deny":  len(sandbox.Deny),
	})

	mcpServer := server.NewMCPServer("fka-bash", "0.1.0",
		server.WithToolCapabilities(true))
	register(mcpServer, sandbox)

	// ServeStdio 独占 stdin/stdout。**不要**在它前后往 stdout 打任何东西
	err = server.ServeStdio(mcpServer)
	if code := exitCodeFor(err); code != 0 {
		fmt.Fprintln(os.Stderr, "bash server 退出："+err.Error())
		return code
	}
	if err != nil {
		log.Log().Info("收到停机信号，已退出", log.Context{})
	}
	return 0
}

// exitCodeFor 把 `ServeStdio` 的返回翻成退出码。
//
// 退出码是契约（0 成功 / 1 预期内的失败）。SDK 自己也注册了 SIGINT / SIGTERM，
// 收到信号时 `ServeStdio` 返回 `context.Canceled`——照直报成 1 的话，**每一次干净的
// 停机都会被记成一次崩溃**，systemd 的 Restart 策略跟着走。
func exitCodeFor(err error) int {
	if err == nil || errors.Is(err, context.Canceled) {
		return 0
	}
	return 1
}

// resolveRoot 定出沙盒根：--root 参数 > BASH_SANDBOX_ROOT 环境变量 > <安装根>/sandbox。
//
// **必须变成绝对路径**：相对路径会按服务进程的 cwd 解析，而那取决于主程序从哪个
// 目录拉起它——同一份 mcp.json 在不同机器上会指向不同的目录，而且不报错。
func resolveRoot(args []string) string {
	root := flagValue(args, "--root")
	if root == "" {
		root = strings.TrimSpace(os.Getenv("BASH_SANDBOX_ROOT"))
	}
	if root == "" {
		root = filepath.Join(home(), "sandbox")
	}
	return root
}

func sandboxAllow(args []string) []string {
	return append(splitList(flagValue(args, "--allow")), splitList(os.Getenv("BASH_ALLOW"))...)
}

func sandboxDeny(args []string) []string {
	return append(splitList(flagValue(args, "--deny")), splitList(os.Getenv("BASH_DENY"))...)
}

// shareNetwork 是否保留宿主网络。默认**不保留**（bwrap 起独立网络命名空间）。
func shareNetwork(args []string) bool {
	for _, arg := range args {
		if arg == "--share-net" {
			return true
		}
	}
	return truthy(os.Getenv("BASH_SHARE_NET"))
}

func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// flagValue 取 `--name value` 或 `--name=value`。
func flagValue(args []string, name string) string {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1]
		}
		if value, ok := strings.CutPrefix(arg, name+"="); ok {
			return value
		}
	}
	return ""
}

// splitList 把逗号分隔的名单拆开，忽略空白项。
func splitList(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// home 解析安装根：FKA_HOME > 可执行文件所在目录 > cwd。
//
// 与主程序同一套锚点——静态二进制的天然锚点，跨机器拷贝后仍然自洽。
func home() string {
	if dir := strings.TrimSpace(os.Getenv("FKA_HOME")); dir != "" {
		return dir
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}
