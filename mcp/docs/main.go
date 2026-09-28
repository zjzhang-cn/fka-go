// Command fka-docs 是**文档管理的 MCP server**（stdio）。
//
// ## 它是什么
//
// 一个**独立的可执行程序**。主程序（`cmd/fka`）从 `mcp.json` 把它当子进程拉起，
// 之后两者之间只说 JSON-RPC：
//
//	{
//	  "mcpServers": {
//	    "docs": {
//	      "command": "/path/to/fka-docs",
//	      "args": ["--db", "/path/to/db.sqlite", "--storage", "/path/to/nas"]
//	    }
//	  }
//	}
//
// 主程序**不再直接碰 documents 表**，也**不再直接读 NAS 上的解析结果**——
// 存储整个沉到本进程里。
//
// ## 眼下只有只读的三件
//
// `search_documents` / `list_documents` / `get_document`。`send_document` 要渠道的
// 发送器、`delete_document` 要连向量索引一起删、归档流水线要文档转换——**都还没接上**，
// 而**声明一个跑不了的工具比不声明更糟**：模型会调它、收到失败、再换个方式试，
// 白烧一轮。所以这里只声明真正能用的。
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
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mark3labs/mcp-go/server"

	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/mcpboot"
	"github.com/zjzhang-cn/fka-go/internal/nas"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	// 信号只用来让 defer 跑完（关库、关日志）。**不要**在这里往 stdout 打任何东西
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbPath, err := mcpboot.ResolveDBPath(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "解析数据库路径失败："+err.Error())
		return 1
	}

	storageRoot, err := resolveStorageRoot(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "解析存储根失败："+err.Error())
		return 1
	}

	// 存储根回退必须显式：**静默回退会让「生产忘了挂 NAS」表现为「文件存在但
	// 找不到」**，而排查时完全不知道从哪下手。
	if strings.TrimSpace(os.Getenv("FILE_STORE_PATH")) == "" {
		root := nas.ResolveRoot()
		if root.Source == nas.SourceFallback {
			config.Log().Warn("NAS 挂载点不存在，原始文件与解析结果走安装根下的回退目录",
				config.Context{"root": root.Path, "detail": root.Detail})
		}
	}

	// **先迁移再开服务**：server 是写方，它面对的库必须已跟上代码
	db, err := mcpboot.OpenAndMigrate(ctx, dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "文档 server 起不来："+err.Error())
		return 1
	}
	defer func() { _ = db.Close() }()

	config.Log().Info("文档 MCP server 就绪", config.Context{
		"db": dbPath, "storage": storageRoot,
	})

	mcpServer := server.NewMCPServer("fka-docs", "0.1.0",
		server.WithToolCapabilities(true))
	register(mcpServer, db, storageRoot)

	// ServeStdio 独占 stdin/stdout。**不要**在它前后往 stdout 打任何东西
	if err := server.ServeStdio(mcpServer); err != nil {
		fmt.Fprintln(os.Stderr, "文档 server 退出："+err.Error())
		return 1
	}
	return 0
}

// resolveStorageRoot 定出存储根：--storage 参数 > FILE_STORE_PATH > NAS 挂载 > 回退。
//
// 顺序与 `internal/nas.ResolveRoot` 一致，**多出的只有 --storage**（便于在开发机上
// 指一份别的 NAS）。**必须变成绝对路径**：相对路径按服务进程的 cwd 解析，而那取决
// 于主程序是从哪个目录拉起它的——同一份 mcp.json 在不同机器上会指向不同的目录，
// 而且不报错。
func resolveStorageRoot(args []string) (string, error) {
	root := ""

	for i, arg := range args {
		if arg == "--storage" && i+1 < len(args) {
			root = args[i+1]
			break
		}
		if value, ok := strings.CutPrefix(arg, "--storage="); ok {
			root = value
			break
		}
	}
	if root == "" {
		root = nas.ResolveRoot().Path
	}

	return filepath.Abs(root)
}
