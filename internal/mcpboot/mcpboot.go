// Package mcpboot 是**各个 MCP server 共用的一点点启动逻辑**。
//
// ## 为什么只放这一点点
//
// 每个 server 必须是**独立目录 + 独立可执行程序**（能单独构建、单独部署、单独
// 重启，崩了不影响主程序）。所以这里**不**放工具实现、不放业务逻辑，只放每个
// server 都要重复一遍的那几行：解析库路径、迁移、开连接。
//
// 共享的部分一旦开始长，就会把「独立」这件事吃掉。所以这个包的体积是刻意压着的——
// 超过三四个函数就该重新考虑是不是该复制。
package mcpboot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/store"
)

// ResolveDBPath 定出库路径：--db 参数 > DB_PATH 环境变量 > <安装根>/data/db.sqlite。
//
// ## 为什么必须变成绝对路径
//
// 相对路径会按**服务进程的 cwd** 解析，而那取决于主程序是从哪个目录拉起它的
// ——同一份 mcp.json 在不同机器上会指向不同的库，而且不报错。
func ResolveDBPath(args []string) (string, error) {
	path := flagValue(args, "--db")
	if path == "" {
		path = strings.TrimSpace(os.Getenv("DB_PATH"))
	}
	if path == "" {
		path = config.DataPath("db.sqlite")
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

// OpenAndMigrate 打开库并**确保它已跟上代码**。
//
// server 是写方，它面对的库必须已迁移——所以这里用 Migrate（会升级）而不是
// AssertCurrent（只检查）。迁移失败就返错，让主程序明确看到「server 没起来」，
// 而不是让它调一个查不到表的 server 然后收到一堆看不懂的错。
func OpenAndMigrate(ctx context.Context, path string) (*store.DB, error) {
	state, err := store.Migrate(ctx, path)
	if err != nil {
		return nil, err
	}
	config.Log().Info("数据库已就绪", config.Context{
		"path": path, "version": state.Version, "adopted": state.Adoptable,
	})

	connection, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	return store.New(connection, path), nil
}

// Describe 渲染一行给日志用。
func Describe(name string, path string) string {
	return fmt.Sprintf("%s server: %s", name, path)
}
