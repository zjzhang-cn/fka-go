// Package store 是 SQLite 存储层：建库、迁移、三张表的读写。
//
// ## 为什么有迁移（Node 版没有）
//
// Node 版的做法是「schema 直建 + 结构指纹 + 不匹配就整库重建（旧库改名 .bak）」。
// 那是**开发阶段**的合理选择——数据随时可以丢。但对已经跑起来的库，它意味着：
// **每加一个字段，用户的数据就重建一次**。`documents.annotations` 就是这么加上去的。
//
// Go 版把「今天那份 schema」定为 **v1**，用 SQLite 自带的 `PRAGMA user_version` 记账：
//
//   - 加一列 = 加一条 v2 迁移（`ALTER TABLE ... ADD COLUMN`），**不丢数据**；
//   - 现有库（Node 版建的、user_version=0）走**认领**路径：逐列核对结构，对得上就
//     打上 v1，对不上就**明确报错**而不是猜；
//   - 服务启动时若有待迁移就**拒绝启动**，把话说清楚，而不是凑合跑。
//
// ## 为什么用 user_version 而不是自建 __migrations 表
//
// user_version 是 SQLite 自己的字段，**在事务里一起提交或一起回滚**，不需要额外
// 保证「台账和数据同生共死」。而自建台账表在「台账写成功但数据没改」时会永久不一致。
//
// **注意**：`__schema`（Node 版留的结构指纹表）不再被使用。指纹是「结构是否一致」的
// 代理指标，而 version 是**事实本身**——它不会因为 DDL 文本改写而误报。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/zjzhang-cn/fka-go/mcp/internal/log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite" // 纯 Go 驱动，零 CGO
)

// ErrNeedsMigration 库落后于当前代码。Hint 说明该做什么。
type ErrNeedsMigration struct {
	Path    string
	From    int
	To      int
	Pending string
}

func (e *ErrNeedsMigration) Error() string {
	return fmt.Sprintf("数据库结构落后：现在是 v%d，代码要 v%d（%s）。跑 `fka doctor --fix` 升级，旧数据保留。",
		e.From, e.To, e.Pending)
}

// ErrShapeMismatch 认领失败：这份库不是我们认识的那一份。
type ErrShapeMismatch struct {
	Path   string
	Reason string
}

func (e *ErrShapeMismatch) Error() string {
	return fmt.Sprintf("这份数据库不是当前代码认识的版本：%s。它可能来自更早的 Node 版结构，"+
		"而 Go 版的第一条迁移就以今天的结构为 v1。请先用 Node 版确认结构，或手工处理 %s。",
		e.Reason, e.Path)
}

// Open 打开（必要时创建）库。**不跑迁移**——建库与迁移是分开的一步，
// 让「只读命令在还没建库时按空处理」成为可能（见 ReadState）。
func Open(path string) (*sql.DB, error) {
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("建库目录失败：%w", err)
		}
	}

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败（%s）：%w", path, err)
	}

	// **连接池限制为 1**：SQLite 写是全库串行的，而 WAL 允许多读一写。多连接会让
	// 「开始一个事务」和「用另一条连接写」撞上，表现为 SQLITE_BUSY 而不是逻辑错。
	// 家用规模（几百份文档）下串行完全够，而稳定性值这个价。
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("数据库打不开（%s）：%w", path, err)
	}

	// WAL 让「服务在写、CLI 在读」互不阻塞。:memory: 与 file: URI 不支持
	if !strings.HasPrefix(path, ":memory:") && !strings.HasPrefix(path, "file:") {
		for _, pragma := range []string{"PRAGMA journal_mode = WAL", "PRAGMA foreign_keys = ON", "PRAGMA busy_timeout = 5000"} {
			if _, err := db.Exec(pragma); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("设置 %s 失败：%w", pragma, err)
			}
		}
	}

	return db, nil
}

func dsn(path string) string {
	if strings.HasPrefix(path, "file:") || path == ":memory:" {
		return path
	}
	// _txlock=immediate 让 BeginTx 立刻拿写锁而不是先读后升级——否则两个事务
	// 同时「读-改-写」会在升级那一步死锁，而报错是 SQLITE_BUSY，排查方向完全错位
	return path + "?_txlock=immediate"
}

// ReadState 读库与代码的差距。**永不返错**——库打不开正是「要升级/重建」的适用场景，
// 抛出去只会让体检崩掉，而那时用户最需要的就是那张体检表。
type ReadState struct {
	Path string
	// Exists 库文件在不在
	Exists bool
	// Version 库里记的 user_version
	Version int
	// Latest 当前代码期望的
	Latest int
	// Pending 待迁移的说明。为空 = 已跟上
	Pending string
	// Error 库打不开时的原因
	Error string
	// Tables 实际有哪些业务表
	Tables []string
	// Adoptable 这份库能否被认领为 v1（结构逐列对得上）
	Adoptable bool
	// Fresh 一个表都没有 —— 全新库
	Fresh bool
}

// Current 这份库是否已跟上代码。
func (s ReadState) Current() bool { return s.Error == "" && s.Pending == "" && s.Exists }

// Migrate 把库升到最新版本。**幂等**：已是最新时什么都不做。
//
// 三条路径：
//   - 全新库（没有一张业务表）→ 跑 v1 起全部迁移；
//   - user_version=0 但有表（Node 版的库）→ **认领**：逐列核对，对得上就打 v1，
//     对不上返 ErrShapeMismatch（不猜、不重建）；
//   - 已有 version → 逐条跑高于它的迁移，每条一个事务。
func Migrate(ctx context.Context, path string) (ReadState, error) {
	state := ReadState{Path: path, Latest: LatestVersion()}

	db, err := Open(path)
	if err != nil {
		state.Error = err.Error()
		return state, err
	}
	defer func() { _ = db.Close() }()

	version, err := readUserVersion(ctx, db)
	if err != nil {
		state.Error = err.Error()
		return state, err
	}
	tables, err := businessTables(ctx, db)
	if err != nil {
		state.Error = err.Error()
		return state, err
	}

	state.Exists = true
	state.Version = version
	state.Tables = tables
	state.Fresh = len(tables) == 0

	if version == 0 && !state.Fresh {
		// 认领路径
		if reason := verifyShape(ctx, db); reason != "" {
			state.Adoptable = false
			return state, &ErrShapeMismatch{Path: path, Reason: reason}
		}
		state.Adoptable = true
		if err := setUserVersion(ctx, db, 1); err != nil {
			return state, err
		}
		state.Version = 1
		version = 1
		log.Log().Info("已认领现有数据库为 v1", log.Context{
			"path": path, "tables": strings.Join(tables, "、"),
		})
	}

	for _, m := range pending(version) {
		if err := applyMigration(ctx, db, m); err != nil {
			state.Pending = describePending(version)
			return state, err
		}
	}

	state.Version = LatestVersion()
	state.Pending = ""
	return state, nil
}

// applyMigration 在**一个事务**里跑一条迁移。
//
// 事务是必须的：一条迁移里有两条语句时（加列 + 建索引），中途失败会留下半截结构，
// 而 version 已经推进——那是最难查的一类不一致。
func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始迁移 v%d 的事务失败：%w", m.Version, err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := m.Apply(execAdapter{tx: tx}); err != nil {
		return fmt.Errorf("迁移 v%d（%s）失败：%w", m.Version, m.Note, err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.Version)); err != nil {
		return fmt.Errorf("迁移 v%d 写版本号失败：%w", m.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("迁移 v%d 提交失败：%w", m.Version, err)
	}

	log.Log().Info("已应用数据库迁移", log.Context{"version": m.Version, "note": m.Note})
	return nil
}

// AssertCurrent 服务启动时的校验：库必须存在、且已跟上代码。
//
// **不一致就拒绝启动**，而不是凑合跑：新代码配旧结构是这个项目里最不容易看见的
// 一类故障（写入被约束挡下、读取少一列），症状往往出现在很远的地方。启动时一句话
// 说清，比事后从日志里猜强得多。
func AssertCurrent(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("还没建库。先跑 `fka doctor --fix` 建库")
		}
		return fmt.Errorf("数据库打不开（%s）：%w", path, err)
	}

	db, err := Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	version, err := readUserVersion(ctx, db)
	if err != nil {
		return err
	}
	if version == 0 {
		// 还没认领。服务是写方，**顺手认领掉**比让用户先跑 doctor 合理：
		// 结构对得上就只是打一个版本号，对不上会在下面报出来
		if reason := verifyShape(ctx, db); reason != "" {
			return &ErrShapeMismatch{Path: path, Reason: reason}
		}
		if err := setUserVersion(ctx, db, 1); err != nil {
			return err
		}
		version = 1
	}

	if list := pending(version); len(list) > 0 {
		return &ErrNeedsMigration{Path: path, From: version, To: LatestVersion(), Pending: describePending(version)}
	}
	return nil
}

// State 只读地看库的状态。**供 doctor / db 命令用**。
func State(ctx context.Context, path string) ReadState {
	state := ReadState{Path: path, Latest: LatestVersion()}

	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			state.Error = err.Error()
		}
		return state
	}

	db, err := Open(path)
	if err != nil {
		state.Error = err.Error()
		return state
	}
	defer func() { _ = db.Close() }()

	version, err := readUserVersion(ctx, db)
	if err != nil {
		state.Error = err.Error()
		return state
	}
	tables, err := businessTables(ctx, db)
	if err != nil {
		state.Error = err.Error()
		return state
	}

	state.Exists = true
	state.Version = version
	state.Tables = tables
	state.Fresh = len(tables) == 0
	if version == 0 && !state.Fresh {
		state.Adoptable = verifyShape(ctx, db) == ""
	}
	state.Pending = describePending(version)
	return state
}

func readUserVersion(ctx context.Context, db *sql.DB) (int, error) {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("读数据库版本号失败：%w", err)
	}
	return version, nil
}

func setUserVersion(ctx context.Context, db *sql.DB, version int) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("写数据库版本号失败：%w", err)
	}
	return nil
}

// businessTables 库里的业务表。**排除 SQLite 自带的与 Node 版留下的 `__schema`**——
// 那张表是旧实现的书签，Go 版不再使用，留着无害但不该算进「这份库有哪些表」。
func businessTables(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		return nil, fmt.Errorf("读表清单失败：%w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if strings.HasPrefix(name, "__") {
			continue
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// verifyShape 逐表逐列核对。**返回空串表示对得上**，否则是一句人话。
//
// 缺一列就拒绝认领：那份库来自更早的 Node 结构，**硬认领会让写入在约束上炸、
// 读取少一列**，症状出现在很远的地方。宁可在这里说清「缺 documents.annotations」。
func verifyShape(ctx context.Context, db *sql.DB) string {
	problems := make([]string, 0, 4)

	for _, table := range expectedTables() {
		actual, err := tableColumns(ctx, db, table)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s：%s", table, err.Error()))
			continue
		}
		if actual == nil {
			problems = append(problems, "缺表 "+table)
			continue
		}

		have := make(map[string]bool, len(actual))
		for _, column := range actual {
			have[column] = true
		}
		missing := make([]string, 0, 4)
		for _, column := range v1Tables[table] {
			if !have[column] {
				missing = append(missing, column)
			}
		}
		if len(missing) > 0 {
			problems = append(problems, fmt.Sprintf("%s 缺列 %s", table, strings.Join(missing, "、")))
		}
	}

	return strings.Join(problems, "；")
}

func tableColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	// PRAGMA 不接受占位符，表名只能内插——**而 table 来自包内的 v1Tables 常量**，
	// 不是用户输入，所以这里没有注入面
	rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// IsShapeMismatch 判断错误是不是「结构不认识」。
func IsShapeMismatch(err error) (*ErrShapeMismatch, bool) {
	var target *ErrShapeMismatch
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}
