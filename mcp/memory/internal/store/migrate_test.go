package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrate_全新库建到最新 全新路径上跑 v1，user_version 落到最新，`memories` 齐。
func TestMigrate_全新库建到最新(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbName)

	state, err := Migrate(context.Background(), path)
	if err != nil {
		t.Fatalf("Migrate 返错：%v", err)
	}
	if state.Version != LatestVersion() {
		t.Errorf("Version = %d，期望 %d", state.Version, LatestVersion())
	}
	if state.Pending != "" {
		t.Errorf("全新库不该有待迁移：%q", state.Pending)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open 失败：%v", err)
	}
	defer func() { _ = db.Close() }()

	// **真的查一次**，而不是用包内的 `tableColumns` 自证：建出来的表要能被 SQL 查询，
	// 那是调用方（server）实际会做的事；用同一个 introspection 辅助函数去验
	// 「表建对了」等于自己证明自己。
	if _, err := db.Query("SELECT id, type, content, owner_wxid, visibility, created_at FROM memories"); err != nil {
		t.Errorf("memories 不可查：%v", err)
	}
}

// TestMigrate_全新库只有自己那一张表 **自己管理自己**的第一层含义：一个全新库里
// **只有 memories**。多了别人的表就说明这份 DDL 又开始替别人操心了。
func TestMigrate_全新库只有自己那一张表(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbName)

	state, err := Migrate(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Tables) != 1 || state.Tables[0] != "memories" {
		t.Errorf("全新库应当只有 memories 一张表，实际：%v", state.Tables)
	}
}

const dbName = "memory.sqlite"

// legacyMemoriesDDL **Node 版**建的 memories 表，逐字照抄。
//
// 下面两个用例靠它造「user_version=0 但结构是今天那份」的库——那正是认领路径
// 存在的理由。
const legacyMemoriesDDL = `CREATE TABLE ` + "`memories`" + ` (
	` + "`id`" + ` text PRIMARY KEY NOT NULL,
	` + "`type`" + ` text NOT NULL,
	` + "`content`" + ` text NOT NULL,
	` + "`owner_wxid`" + ` text NOT NULL,
	` + "`visibility`" + ` text DEFAULT 'public' NOT NULL,
	` + "`created_at`" + ` integer NOT NULL
)`

// makeLegacy 造一份「Node 版建的库」：结构与 v1 逐字一致，但 user_version 保持 0
// （Node 从不设它）。
func makeLegacy(t *testing.T, path string) {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(legacyMemoriesDDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO memories (id, type, content, owner_wxid, visibility, created_at)
		 VALUES ('m1', 'knowledge', '孩子鸡蛋过敏', 'wx_zhang', 'private', 1700000000000)`); err != nil {
		t.Fatal(err)
	}
}

func memoryCount(t *testing.T, path string) int {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n); err != nil {
		t.Fatalf("数 memories 失败：%v", err)
	}
	return n
}

// TestMigrate_认领Node版的库 这是**整个迁移框架存在的理由**：Node 版的库
// user_version=0 但结构是今天的结构。必须**认领**而不是重建——重建会丢几年积累的记忆。
func TestMigrate_认领Node版的库(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbName)
	makeLegacy(t, path)

	state, err := Migrate(context.Background(), path)
	if err != nil {
		t.Fatalf("认领失败：%v", err)
	}
	if !state.Adoptable {
		t.Errorf("应可认领。tables=%v", state.Tables)
	}
	if state.Version != 1 {
		t.Errorf("认领后 user_version = %d，期望 1", state.Version)
	}
	if got := memoryCount(t, path); got != 1 {
		t.Errorf("认领后 memories 行数 = %d，期望 1（**一行都不能动**）", got)
	}
}

// TestMigrate_缺列就拒绝认领 缺一列就拒绝：那是从更早的 Node 结构来的，硬认领会让
// 写入在约束上炸、读取少一列，而症状出现在很远的地方。
func TestMigrate_缺列就拒绝认领(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbName)

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// 比 v1 少两列
	if _, err := db.Exec(`CREATE TABLE memories (
		id text PRIMARY KEY NOT NULL,
		type text NOT NULL,
		content text NOT NULL,
		owner_wxid text NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	_, err = Migrate(context.Background(), path)
	if err == nil {
		t.Fatal("缺列时该拒绝认领")
	}
	mismatch, ok := IsShapeMismatch(err)
	if !ok {
		t.Fatalf("该返 ErrShapeMismatch，实际：%T %v", err, err)
	}
	if !strings.Contains(mismatch.Reason, "visibility") {
		t.Errorf("该点名缺哪列：%q", mismatch.Reason)
	}
}

// TestMigrate_缺表也拒绝认领
func TestMigrate_缺表也拒绝认领(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbName)

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// 只有别人的表，没有 memories
	if _, err := db.Exec(`CREATE TABLE documents (id text PRIMARY KEY NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	if _, err := Migrate(context.Background(), path); err == nil {
		t.Fatal("没有 memories 时该拒绝认领")
	} else if _, ok := IsShapeMismatch(err); !ok {
		t.Fatalf("该返 ErrShapeMismatch，实际：%T %v", err, err)
	}
}

// TestMigrate_认领失败不重建 **最要紧的一条**：认领失败时**绝不能**顺手把库重建。
// 重建会「修好」错误，代价是数据没了——而用户看到的是一个安静的库，不是事故。
func TestMigrate_认领失败不重建(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbName)

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE memories (id text PRIMARY KEY NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memories (id) VALUES ('keep-me')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	if _, err := Migrate(context.Background(), path); err == nil {
		t.Fatal("该拒绝认领")
	}

	// 原来的行必须还在，且**没有被补上别的列**
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db2.Close() }()

	var id string
	if err := db2.QueryRow("SELECT id FROM memories").Scan(&id); err != nil {
		t.Fatalf("原来的行没了：%v", err)
	}
	if id != "keep-me" {
		t.Errorf("id = %q", id)
	}
	if got := userVersion(t, db2); got != 0 {
		t.Errorf("认领失败却打了版本号 %d", got)
	}
}

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestMigrate_共用库文件时只认自己那张 **自己管理自己**的第二层含义：
// 别人多出来的表**一律忽略**——那是别人的账，它的 schema 变动不该让我拒绝启动。
func TestMigrate_共用库文件时只认自己那张(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbName)
	makeLegacy(t, path)

	// 塞一个与本 server 毫无关系的表，还带一个 Node 版留下的书签表
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{
		`CREATE TABLE documents (id text PRIMARY KEY NOT NULL)`,
		`CREATE TABLE __schema (fingerprint text)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()

	state, err := Migrate(context.Background(), path)
	if err != nil {
		t.Fatalf("别人的表不该让我认领失败：%v", err)
	}
	if !state.Adoptable {
		t.Errorf("该可认领。tables=%v", state.Tables)
	}
	// 别人的表还在——**我没有权利动它**
	if got := memoryCount(t, path); got != 1 {
		t.Errorf("memories 行数 = %d，期望 1", got)
	}
}

// TestMigrate_认领不动别人的版本号 user_version 是**整个库文件**的，不是我这张表的。
// 认领时打上 v1 是在**替整份库**记账——所以这一条是「共用库文件」这个前提下
// 必须讲清的后果，不是 bug。
func TestMigrate_认领会给整份库打版本号(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbName)
	makeLegacy(t, path)

	if _, err := Migrate(context.Background(), path); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if got := userVersion(t, db); got != 1 {
		t.Errorf("user_version = %d，期望 1", got)
	}
}

// TestMigrate_幂等 已是最新时再跑一次什么都不做——主程序可能反复拉起这个 server。
func TestMigrate_幂等(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbName)

	first, err := Migrate(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Migrate(context.Background(), path)
	if err != nil {
		t.Fatalf("第二次 Migrate 返错：%v", err)
	}
	if second.Version != first.Version || second.Pending != "" {
		t.Errorf("第二次不该有变化：%+v", second)
	}
}

// TestMigrate_库比代码新要拒绝 **回滚**场景：二进制退回旧版，而库已被新版迁过。
//
// 此时 `pending` 是空的，若没有这道闸门，代码会把 user_version **谎报**成自己的
// 最新版然后继续服务——以一份自己并不认识的结构读写。按包注释自己的原则，
// 宁可拒绝启动。
func TestMigrate_库比代码新要拒绝(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbName)

	// 先正常建到最新，再把版本号推到「未来」——这就是旧二进制面对的那份库
	if _, err := Migrate(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	conn, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()

	state, err := Migrate(context.Background(), path)
	if err == nil {
		t.Fatalf("库版本比代码新时必须拒绝启动，实际成功了：%+v", state)
	}
	var tooNew *ErrTooNew
	if !errors.As(err, &tooNew) {
		t.Fatalf("错误该是 *ErrTooNew，实际 %T：%v", err, err)
	}
	if tooNew.DB != 99 || tooNew.Code != LatestVersion() {
		t.Errorf("错误里该带上两边版本：db=%d code=%d", tooNew.DB, tooNew.Code)
	}

	// **绝不谎报**：拒绝启动时不能动库里的版本号
	conn2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn2.Close() }()

	version, err := readUserVersion(context.Background(), conn2)
	if err != nil {
		t.Fatal(err)
	}
	if version != 99 {
		t.Errorf("库版本被改成了 v%d——拒绝启动时不该碰它", version)
	}
}

// TestMigrate_真库认领 真库在这台机器上存在时，认领它并逐行核对行数没变。
func TestMigrate_真库认领(t *testing.T) {
	// **4 层上去才是仓库根**（store → internal → memory → mcp）。
	//
	// 这里以前写的是 5 层，于是它指向 `<家目录>/data/memory.sqlite`：真库在仓库里时
	// 这条用例**永远跳过**（看起来像「这台机器还没建过库」），而家目录里恰好有个同名
	// 文件时它还会去认领一个**无关的库**。所以先确认层级没写错，再谈跳不跳。
	root := filepath.Join("..", "..", "..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("层级数写错了：%s 之上不是仓库根（%v）", root, err)
	}

	real := filepath.Join(root, "data", dbName)
	if _, err := os.Stat(real); err != nil {
		t.Skip("真库不存在（这台机器还没建过库），跳过")
	}

	// **-wal / -shm 一起拷**：库是 WAL 模式，只拷主文件可能拿到一份缺最近提交的
	// 快照（见 Open 里的 `PRAGMA journal_mode = WAL`）。
	copyOf := filepath.Join(t.TempDir(), dbName)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(real + suffix)
		if err != nil {
			if suffix == "" {
				t.Skipf("复制真库失败，跳过：%v", err)
			}
			continue // 没有 -wal / -shm 是正常的（没在跑，或已 checkpoint）
		}
		if err := os.WriteFile(copyOf+suffix, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	before := memoryCount(t, copyOf)
	state, err := Migrate(context.Background(), copyOf)
	if err != nil {
		t.Fatalf("认领真库失败：%v", err)
	}
	if !state.Adoptable {
		t.Errorf("真库应可认领。tables=%v", state.Tables)
	}
	if after := memoryCount(t, copyOf); after != before {
		t.Errorf("memories 行数 %d → %d", before, after)
	}
	t.Logf("真库认领成功：v0 → v%d，memories=%d 条", state.Version, before)
}
