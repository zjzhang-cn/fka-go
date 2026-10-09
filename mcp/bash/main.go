// Command fka-bash 是**沙盒 bash 执行的 MCP server**。
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
// ## 传输方式由参数决定
//
// 默认是 `stdio`（被主程序当子进程拉起）。`--transport` 可以在**同一份工具实现**上
// 换传输，不改 server.go 一行：
//
//	--transport stdio   本地管道（默认，主程序拉子进程用这种）
//	--transport sse     老式 HTTP+SSE，监听 --addr（默认 127.0.0.1:8080），路径 /sse
//	--transport http    streamable HTTP，监听 --addr，路径 /mcp
//
// 换成 HTTP 后，主程序那种「拉子进程」的配置就换成一端 `url`：
//
//	{
//	  "mcpServers": {
//	    "bash": { "url": "http://127.0.0.1:8080/sse", "transport": "sse" }
//	  }
//	}
//
// 内容块是协议层的，不绑定传输：`read` 的文本 / 图片 / 音频块换到 HTTP 后面照旧。
//
//	--addr ADDR             监听地址（SSE / http 用；默认 127.0.0.1:8080）
//	BASH_MCP_TRANSPORT      等价于 --transport
//	BASH_MCP_ADDR           等价于 --addr
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
// ## stdout 一个字都不能有（stdio 档）
//
// stdio 档下 stdout 是 JSON-RPC 通道。**任何** fmt.Println 都会插进协议流里把
// server 打挂。诊断信息一律走 log.Log()（stderr + 按天轮转的日志文件）。
// 换到 HTTP 档后 stdout 不再是通道，但这条纪律仍然保留——两种档共用一份源码。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/zjzhang-cn/fka-go/mcp/bash/internal/log"
)

// 传输方式。**取值与 mcp.json 里 `transport` 一字不差**——写错一个字符的表现是
// 「配置读进来了但没人认」。
const (
	transportStdio = "stdio"
	transportSSE   = "sse"
	transportHTTP  = "http"
)

// defaultAddr 监听地址默认**只绑本地回环**：这是个能在沙盒里跑命令的 server，
// 默认暴露到 0.0.0.0 等于把命令执行权敞开给同网段。要对外必须显式写 --addr。
const defaultAddr = "127.0.0.1:8080"

// shutdownGrace 收到停机信号后等 HTTP 连接关掉的宽限。
const shutdownGrace = 5 * time.Second

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	// **不要**在这个进程里往 stdout 打任何东西。stdio 档的信号由 SDK 自己接
	// （ServeStdio 收到 SIGINT / SIGTERM 会返回 context.Canceled）；HTTP 档没这个
	// 待遇，信号在 serveHTTP 里单独接。两档都刻意把干净停机返成 context.Canceled。
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

	transport := resolveTransport(args)
	if transport == "" {
		fmt.Fprintln(os.Stderr, "bash server：--transport 只能是 stdio / sse / http，收到 "+firstNonEmpty(flagValue(args, "--transport"), os.Getenv("BASH_MCP_TRANSPORT")))
		return 2
	}

	// 换传输只换「谁把 JSON-RPC 送进来」，工具实现一行不改：stdio 独占 stdin/stdout，
	// HTTP 档另起一个监听。**不要在它前后往 stdout 打任何东西**
	var serveErr error
	switch transport {
	case transportStdio:
		serveErr = server.ServeStdio(mcpServer)
	case transportSSE, transportHTTP:
		serveErr = serveHTTP(transport, resolveAddr(args), mcpServer)
	}
	if code := exitCodeFor(serveErr); code != 0 {
		fmt.Fprintln(os.Stderr, "bash server 退出："+serveErr.Error())
		return code
	}
	if serveErr != nil {
		log.Log().Info("收到停机信号，已退出", log.Context{"transport": transport})
	}
	return 0
}

// httpServer 两种 HTTP 传输都满足的最小接口。用一个接口而不是各自分支，是因为
// Start / Shutdown / 信号收尾三件事对 sse 与 http 完全一样，只有构造不同。
type httpServer interface {
	Start(addr string) error
	Shutdown(ctx context.Context) error
}

// serveHTTP 起一个监听 addr 的 HTTP MCP server，直到收到停机信号。
//
// ## 为什么信号要自己接，stdio 档却不用
//
// stdio 档由 SDK 的 ServeStdio 注册 SIGINT / SIGTERM 并返回 context.Canceled。
// HTTP 档 SDK 不管信号，`Start` 是阻塞的 ListenAndServe——不自己接信号、不调
// Shutdown，进程就会带着半开的连接被直接杀掉（干净停机被记成崩溃，见 exitCodeFor）。
func serveHTTP(transport, addr string, mcpServer *server.MCPServer) error {
	var srv httpServer
	if transport == transportSSE {
		srv = server.NewSSEServer(mcpServer)
	} else {
		srv = server.NewStreamableHTTPServer(mcpServer)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(addr) }()

	log.Log().Info("bash MCP server 开始监听", log.Context{
		"transport": transport, "addr": addr,
	})

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		// 返 context.Canceled：exitCodeFor 认它是干净停机的 0
		return ctx.Err()
	case err := <-errCh:
		// Start 自己的错（端口被占等）**原样上抛**，别吞掉：那才是真正起不来
		return err
	}
}

// resolveTransport 定出传输方式：--transport > BASH_MCP_TRANSPORT > 默认 stdio。
//
// 认不出的取值返回空串，由调用方按用法错退出（退出码 2）——静默退回 stdio 会让
// 「我明明配了 sse」变成一句查不出的假象。
func resolveTransport(args []string) string {
	value := strings.ToLower(strings.TrimSpace(
		firstNonEmpty(flagValue(args, "--transport"), os.Getenv("BASH_MCP_TRANSPORT"))))
	switch value {
	case "":
		return transportStdio
	case transportStdio, transportSSE, transportHTTP:
		return value
	}
	return ""
}

// resolveAddr 定出 HTTP 监听地址：--addr > BASH_MCP_ADDR > 127.0.0.1:8080。
func resolveAddr(args []string) string {
	return firstNonEmpty(flagValue(args, "--addr"), strings.TrimSpace(os.Getenv("BASH_MCP_ADDR")), defaultAddr)
}

// exitCodeFor 把 `ServeStdio` / `serveHTTP` 的返回翻成退出码。
//
// 退出码是契约（0 成功 / 1 预期内的失败）。SDK 自己也注册了 SIGINT / SIGTERM，
// 收到信号时 `ServeStdio` 返回 `context.Canceled`——照直报成 1 的话，**每一次干净的
// 停机都会被记成一次崩溃**，systemd 的 Restart 策略跟着走。HTTP 档的 serveHTTP
// 也刻意返回 context.Canceled，走同一条换算。
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
