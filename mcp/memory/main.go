// Command fka-memory 是**家庭记忆的 MCP server**（stdio）。
//
// ## 它是什么
//
// 一个**独立的可执行程序**，也是一个**自给自足的项目**：库归它建、schema 归它定、
// 迁移归它跑、日志归它写。它不 import agent 的任何包，也不与别的 server 共享
// 一行代码——所以它可以单独构建、单独部署、单独重启，崩了不影响主程序。
//
// 主程序（`cmd/fka`）从 `mcp.json` 把它当子进程拉起，之后两者之间只说 JSON-RPC：
//
//	{
//	  "mcpServers": {
//	    "memory": { "command": "/path/to/fka-memory", "args": ["--db", "/path/to/memory.sqlite"] }
//	  }
//	}
//
// ## 默认库是**自己的**文件
//
// 默认 `<安装根>/data/memory.sqlite`，**不是** `<安装根>/data/db.sqlite`。
// 「自己管理自己」从文件名开始：它不该与谁共用一个库，也就不必在别人的 schema
// 变动时猜「那张表是不是我的」。
//
// 指到旧的那个共享库**照样能用**——认领路径只核对 `memories` 一张表，别的表一概
// 忽略（见 internal/store 的 verifyShape）。
//
// ## 为什么不写成主程序的一个子命令
//
// 每个 server 一个目录、一个可执行程序：它可以单独构建、单独部署、单独重启，
// 崩了不影响主程序，也不需要主程序为了拉起它而多一条命令。
//
// ## stdout 一个字都不能有
//
// stdout 是 JSON-RPC 通道。**任何** fmt.Println 都会插进协议流里把 server 打挂。
// 诊断信息一律走 log.Log()（stderr + 按天轮转的日志文件）。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mark3labs/mcp-go/server"

	"github.com/zjzhang-cn/fka-go/mcp/memory/internal/log"
	"github.com/zjzhang-cn/fka-go/mcp/memory/internal/store"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	// 信号只用来让 defer 跑完（关库、关日志）。**不要**在这里打任何东西到 stdout
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	path, err := resolveDBPath(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "解析数据库路径失败："+err.Error())
		return 1
	}

	// **先迁移再开服务**：server 是写方，它面对的库必须已跟上代码。迁移失败就拒绝
	// 启动——让主程序明确看到「server 没起来」，而不是让它调一个查不到表的 server
	db, err := openAndMigrate(ctx, path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "记忆 server 起不来："+err.Error())
		return 1
	}
	defer func() { _ = db.Close() }()

	log.Log().Info("记忆 MCP server 就绪", log.Context{"path": path})

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

// dbFileName **自己的**库文件名。见文件头「默认库是自己的文件」。
const dbFileName = "memory.sqlite"

// resolveDBPath 定出库路径：--db 参数 > DB_PATH 环境变量 > <安装根>/data/memory.sqlite。
//
// ## 为什么必须变成绝对路径
//
// 相对路径会按**服务进程的 cwd** 解析，而那取决于主程序是从哪个目录拉起它的
// ——同一份 mcp.json 在不同机器上会指向不同的库，而且不报错。
func resolveDBPath(args []string) (string, error) {
	path := flagValue(args, "--db")
	if path == "" {
		path = strings.TrimSpace(os.Getenv("DB_PATH"))
	}
	if path == "" {
		path = filepath.Join(home(), "data", dbFileName)
	}
	return filepath.Abs(path)
}

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

// openAndMigrate 打开库并**确保它已跟上代码**。
//
// server 是写方，所以这里用 Migrate（会升级）而不是只检查。迁移失败就返错，
// 让主程序明确看到「server 没起来」。
func openAndMigrate(ctx context.Context, path string) (*store.DB, error) {
	state, err := store.Migrate(ctx, path)
	if err != nil {
		return nil, err
	}
	log.Log().Info("数据库已就绪", log.Context{
		"path": path, "version": state.Version, "adopted": state.Adoptable,
	})

	connection, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	return store.New(connection, path), nil
}
