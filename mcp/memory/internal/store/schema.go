package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// v1 的建库语句。**`memories` 一张表，逐字与 Node 版 drizzle-kit 生成的 DDL 一致**——
// 这是「今天的数据能被直接认领」的前提：现有库里已有的记忆，Go 版必须能打开它、
// 改它、继续往上加列，而不是重建。
//
// 改这一份**不会**让已建好的库自动变形——那是迁移的事，见 migrate.go。
var v1Statements = []string{
	`CREATE TABLE ` + "`memories`" + ` (
	` + "`id`" + ` text PRIMARY KEY NOT NULL,
	` + "`type`" + ` text NOT NULL,
	` + "`content`" + ` text NOT NULL,
	` + "`owner_wxid`" + ` text NOT NULL,
	` + "`visibility`" + ` text DEFAULT 'public' NOT NULL,
	` + "`created_at`" + ` integer NOT NULL
)`,
	`CREATE INDEX ` + "`idx_memories_owner`" + ` ON ` + "`memories`" + ` (` + "`owner_wxid`" + `)`,
}

// v1Tables 期望的表与列。**认领已有库时逐项核对**——见 migrate.go 的 adopt 路径。
//
// 只有 `memories`。这个 server **不拥有别的表**，因此也**不核对别的表**：
// 与别人共用一个库文件时，多出来的 `documents` / `messages` 与它无关，
// 别人的 schema 变动不该让它拒绝启动。
var v1Tables = map[string][]string{
	"memories": {"id", "type", "content", "owner_wxid", "visibility", "created_at"},
}

// expectedTables 按名字排好序。**报错信息里要稳定**，不能靠 map 遍历顺序。
func expectedTables() []string {
	names := make([]string, 0, len(v1Tables))
	for name := range v1Tables {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// migration 一条版本化迁移。
//
// ## 为什么从 1 开始而不是从 0
//
// Node 版**没有迁移**，schema 直接从代码生成，指纹不匹配就整库重建。Go 版把
// 「今天那份 schema」定为 **v1**，于是现有库可以被**认领**而不是重建——
// 几年积累的记忆不该因为换语言而丢。
type migration struct {
	// Version 目标 user_version。**必须连续**：1, 2, 3 …
	Version int
	// Note 一句话说明这条改了什么。报错会显示它
	Note string
	// Apply 在一个事务里跑。**只写增量**：建表或 ALTER，绝不重建
	Apply func(exec execer) error
}

// execer 只要能执行语句就够——迁移里不读数据。
type execer interface {
	Exec(query string, args ...any) (sqlResult, error)
}

// sqlResult 是 database/sql.Result 的最小别名。**刻意不直接写 database/sql.Result**：
// 那样 `execer` 就必须 import database/sql，而这个接口的全部意义是「迁移不关心
// 插入了几行」。抽象到只暴露「不报错」这一点。
type sqlResult interface{}

// execAdapter 把 *sql.Tx 的返回值丢掉，只留下「有没有错」。
type execAdapter struct{ tx *sql.Tx }

func (a execAdapter) Exec(query string, args ...any) (sqlResult, error) {
	_, err := a.tx.Exec(query, args...)
	return nil, err
}

var migrations = []migration{
	{Version: 1, Note: "memories 表与索引（与 Node 版逐字一致）", Apply: applyV1},
}

// LatestVersion 当前代码期望的 user_version。
func LatestVersion() int {
	latest := 0
	for _, m := range migrations {
		if m.Version > latest {
			latest = m.Version
		}
	}
	return latest
}

// pending 返回从 from 之后到最新版本之间的迁移。
func pending(from int) []migration {
	out := make([]migration, 0, 4)
	for _, m := range migrations {
		if m.Version > from {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

func applyV1(exec execer) error {
	for _, statement := range v1Statements {
		if _, err := exec.Exec(statement); err != nil {
			return fmt.Errorf("建表语句失败（%s）：%w", firstLine(statement), err)
		}
	}
	return nil
}

// describePending 把待迁移列表渲染成人话，给报错用。
func describePending(from int) string {
	list := pending(from)
	if len(list) == 0 {
		return ""
	}
	parts := make([]string, 0, len(list))
	for _, m := range list {
		parts = append(parts, fmt.Sprintf("v%d %s", m.Version, m.Note))
	}
	return strings.Join(parts, "；")
}

func firstLine(s string) string {
	if index := strings.IndexByte(s, '\n'); index >= 0 {
		return strings.TrimSpace(s[:index])
	}
	return s
}
