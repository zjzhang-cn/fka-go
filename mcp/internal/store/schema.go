package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// v1 的建库语句。**逐字兼容 Node 版用 drizzle-kit 生成的 DDL**——这是「今天的数据能被
// 直接认领」的前提：现有 data/db.sqlite 里已有 330 条消息与 5 份文档，Go 版必须能
// 打开它、改它、继续往上加列，而不是重建。
//
// 改这一份**不会**让已建好的库自动变形——那是迁移的事，见 migrate.go。

var v1Statements = []string{
	`CREATE TABLE ` + "`documents`" + ` (
	` + "`id`" + ` text PRIMARY KEY NOT NULL,
	` + "`filename`" + ` text NOT NULL,
	` + "`filepath`" + ` text NOT NULL,
	` + "`size`" + ` integer,
	` + "`owner_wxid`" + ` text NOT NULL,
	` + "`visibility`" + ` text DEFAULT 'public' NOT NULL,
	` + "`content_hash`" + ` text NOT NULL,
	` + "`status`" + ` text DEFAULT 'pending' NOT NULL,
	` + "`error`" + ` text,
	` + "`attempts`" + ` integer DEFAULT 0 NOT NULL,
	` + "`created_at`" + ` integer NOT NULL,
	` + "`updated_at`" + ` integer NOT NULL,
	` + "`annotations`" + ` text
)`,
	`CREATE INDEX ` + "`idx_documents_content_hash`" + ` ON ` + "`documents`" + ` (` + "`content_hash`" + `)`,
	`CREATE INDEX ` + "`idx_documents_owner`" + ` ON ` + "`documents`" + ` (` + "`owner_wxid`" + `)`,
	`CREATE INDEX ` + "`idx_documents_status`" + ` ON ` + "`documents`" + ` (` + "`status`" + `)`,

	`CREATE TABLE ` + "`memories`" + ` (
	` + "`id`" + ` text PRIMARY KEY NOT NULL,
	` + "`type`" + ` text NOT NULL,
	` + "`content`" + ` text NOT NULL,
	` + "`owner_wxid`" + ` text NOT NULL,
	` + "`visibility`" + ` text DEFAULT 'public' NOT NULL,
	` + "`created_at`" + ` integer NOT NULL
)`,
	`CREATE INDEX ` + "`idx_memories_owner`" + ` ON ` + "`memories`" + ` (` + "`owner_wxid`" + `)`,

	`CREATE TABLE ` + "`messages`" + ` (
	` + "`account_id`" + ` text NOT NULL,
	` + "`message_id`" + ` text,
	` + "`quoted_id`" + ` text,
	` + "`conversation_id`" + ` text NOT NULL,
	` + "`sender_id`" + ` text NOT NULL,
	` + "`recipient_id`" + ` text NOT NULL,
	` + "`create_time_ms`" + ` integer NOT NULL,
	` + "`direction`" + ` integer NOT NULL,
	` + "`parts`" + ` text NOT NULL,
	` + "`raw`" + ` text,
	` + "`document_id`" + ` text
)`,
	// 唯一键是**四列**，不是单独的 message_id。只用 message_id 的话，服务端在两段会话里
	// 给出同一个 id 时第二条会被挡掉，而写入那边是 onConflictDoNothing —— 于是**静默丢掉**
	// 一条。四列里有可空的（message_id），而 SQLite 的 UNIQUE 索引里 NULL 互不相同，
	// 渠道没给 id 的那种记录因此不会被互相挡掉。
	`CREATE UNIQUE INDEX ` + "`idx_messages_identity`" + ` ON ` + "`messages`" + ` (` + "`account_id`" + `,` + "`message_id`" + `,` + "`sender_id`" + `,` + "`direction`" + `)`,
	// 按会话翻历史。权限过滤也走它——一段会话里只有两个参与者
	`CREATE INDEX ` + "`idx_messages_conversation`" + ` ON ` + "`messages`" + ` (` + "`account_id`" + `,` + "`conversation_id`" + `,` + "`create_time_ms`" + `)`,
	// 顺着会话**往下**走：这条消息后来被谁接了。往上走（我接在谁后面）拿 message_id
	// 去查，idx_messages_identity 的头两列正好是它，不必再建一个
	`CREATE INDEX ` + "`idx_messages_quoted`" + ` ON ` + "`messages`" + ` (` + "`account_id`" + `,` + "`quoted_id`" + `)`,
}

// v1Tables 期望的表与列。**认领已有库时逐项核对**——见 migrate.go 的 adopt 路径。
//
// 用它而不是反过来（从库里读出结构再比指纹），是因为「我们期望什么」是唯一能
// 表达「这份库是不是我们认识的那一份」的东西。
var v1Tables = map[string][]string{
	"documents": {"id", "filename", "filepath", "size", "owner_wxid", "visibility",
		"content_hash", "status", "error", "attempts", "created_at", "updated_at", "annotations"},
	"memories": {"id", "type", "content", "owner_wxid", "visibility", "created_at"},
	"messages": {"account_id", "message_id", "quoted_id", "conversation_id", "sender_id",
		"recipient_id", "create_time_ms", "direction", "parts", "raw", "document_id"},
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
// 「今天那份 schema」定为 **v1**，于是现有库可以被**认领**而不是重建——330 条消息与
// 几年积累的记忆不该因为换语言而丢。
type migration struct {
	// Version 目标 user_version。**必须连续**：1, 2, 3 …
	Version int
	// Note 一句话说明这条改了什么。报错与 doctor 都会显示它
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
	{Version: 1, Note: "documents / memories / messages 三张表与索引（与 Node 版逐字一致）", Apply: applyV1},
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

// describePending 把待迁移列表渲染成人话，给报错与 doctor 用。
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
