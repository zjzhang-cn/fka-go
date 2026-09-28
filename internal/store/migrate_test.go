package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// counts 三个表的行数 + 关键列，用来证明「认领后数据没动」。
func counts(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"documents", "memories", "messages"} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatalf("数 %s 失败：%v", table, err)
		}
		out[table] = n
	}
	return out
}

// TestMigrate_全新库建到最新 全新路径上跑 v1，user_version 落到最新，三张表齐。
func TestMigrate_全新库建到最新(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")

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

	for _, table := range expectedTables() {
		if _, err := tableColumns(context.Background(), db, table); err != nil {
			t.Errorf("%s 不可读：%v", table, err)
		}
	}
}

// TestMigrate_认领Node版的库 这是**整个迁移框架存在的理由**：Node 版的库
// user_version=0（Node 从不设它）但结构是今天的结构。必须认领而不是重建——
// 重建会丢 330 条消息与几年积累的记忆。
func TestMigrate_认领Node版的库(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")

	// 造一份「Node 版建的库」：结构与 v1 逐字一致，但 user_version 保持 0，
	// 而且**有数据**
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range v1Statements {
		if _, err := seed.Exec(statement); err != nil {
			t.Fatalf("铺库失败：%v", err)
		}
	}
	if _, err := seed.Exec(
		`INSERT INTO documents (id, filename, filepath, owner_wxid, content_hash, status, created_at, updated_at)
		 VALUES ('doc-1', '房产证.pdf', 'files/wx/a.pdf', 'wx1', 'hash1', 'ready', 100, 100)`); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(
		`INSERT INTO memories (id, type, content, owner_wxid, created_at) VALUES ('m1','knowledge','孩子鸡蛋过敏','wx1',1)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := seed.Exec(fmt.Sprintf(
			`INSERT INTO messages (account_id, message_id, quoted_id, conversation_id, sender_id, recipient_id, create_time_ms, direction, parts)
			 VALUES ('a1','msg%d',NULL,'c1','wx1','bot',100,1,'[]')`, i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := seed.Exec(`CREATE TABLE __schema (fingerprint TEXT NOT NULL, built_at INTEGER NOT NULL);`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	before := func() map[string]int {
		db, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		return counts(t, db)
	}()
	if before["documents"] != 1 || before["memories"] != 1 || before["messages"] != 3 {
		t.Fatalf("铺的数据不对：%+v", before)
	}

	state, err := Migrate(context.Background(), path)
	if err != nil {
		t.Fatalf("认领失败：%v", err)
	}
	if state.Version != LatestVersion() {
		t.Errorf("认领后 Version = %d，期望 %d", state.Version, LatestVersion())
	}
	if !state.Adoptable {
		t.Error("应当被判为可认领")
	}

	// 认领后重新开连接数一遍
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	got := counts(t, db)
	for table, want := range map[string]int{"documents": 1, "memories": 1, "messages": 3} {
		if got[table] != want {
			t.Errorf("%s 行数 %d → %d，**认领不该动数据**", table, want, got[table])
		}
	}

	// 认领必须幂等：再跑一次什么都不该做
	if _, err := Migrate(context.Background(), path); err != nil {
		t.Fatalf("二次 Migrate 返错：%v", err)
	}
}

// TestMigrate_缺列就拒绝认领 那份库来自更早的 Node 结构。**硬认领会让写入在约束上
// 炸、读取少一列**，症状出现在很远的地方——宁可现在说清缺哪一列。
func TestMigrate_缺列就拒绝认领(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")

	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// 一个**没有 annotations 列**的 documents（那是后来才加的）
	if _, err := seed.Exec(`CREATE TABLE documents (
		id text PRIMARY KEY NOT NULL, filename text NOT NULL, filepath text NOT NULL,
		size integer, owner_wxid text NOT NULL, visibility text DEFAULT 'public' NOT NULL,
		content_hash text NOT NULL, status text DEFAULT 'pending' NOT NULL, error text,
		attempts integer DEFAULT 0 NOT NULL, created_at integer NOT NULL, updated_at integer NOT NULL
	); CREATE TABLE memories (
		id text PRIMARY KEY NOT NULL, type text NOT NULL, content text NOT NULL,
		owner_wxid text NOT NULL, visibility text DEFAULT 'public' NOT NULL, created_at integer NOT NULL
	); CREATE TABLE messages (
		account_id text NOT NULL, message_id text, quoted_id text, conversation_id text NOT NULL,
		sender_id text NOT NULL, recipient_id text NOT NULL, create_time_ms integer NOT NULL,
		direction integer NOT NULL, parts text NOT NULL, raw text, document_id text
	);`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Migrate(context.Background(), path)
	if err == nil {
		t.Fatal("缺列的库不该被认领")
	}
	mismatch, ok := IsShapeMismatch(err)
	if !ok {
		t.Fatalf("应是 *ErrShapeMismatch，实际 %T：%v", err, err)
	}
	// 报错必须**指名缺哪一列**，否则用户无从下手
	if !contains(mismatch.Reason, "annotations") {
		t.Errorf("报错要点名缺的列：%q", mismatch.Reason)
	}
}

// TestMigrate_缺表也拒绝认领
func TestMigrate_缺表也拒绝认领(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	seed, _ := sql.Open("sqlite", path)
	if _, err := seed.Exec(`CREATE TABLE documents (id text)`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Migrate(context.Background(), path); err == nil {
		t.Fatal("缺表的库不该被认领")
	}
}

// TestMigrate_缺一列的库不认领_不重建 认领失败时**绝不能顺手重建**——那正是 Node 版
// 让人丢数据的动作。
func TestMigrate_认领失败不重建(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	seed, _ := sql.Open("sqlite", path)
	if _, err := seed.Exec(`CREATE TABLE documents (id text PRIMARY KEY); CREATE TABLE unrelated (x text);`); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`INSERT INTO unrelated VALUES ('keep me')`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Migrate(context.Background(), path); err == nil {
		t.Fatal("应当失败")
	}

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var value string
	if err := db.QueryRow("SELECT x FROM unrelated").Scan(&value); err != nil {
		t.Fatalf("原库被动了：%v", err)
	}
	if value != "keep me" {
		t.Errorf("原库数据被改：%q", value)
	}
}

func TestAssertCurrent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db.sqlite")

	// 还没建库
	if err := AssertCurrent(ctx, path); err == nil {
		t.Error("还没建库时应当拒绝启动")
	}

	if _, err := Migrate(ctx, path); err != nil {
		t.Fatal(err)
	}
	if err := AssertCurrent(ctx, path); err != nil {
		t.Errorf("已跟上代码时不该报错：%v", err)
	}
}

func TestAssertCurrent_待迁移时拒绝启动(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db.sqlite")

	// 造一份 user_version=0 的库，且结构完整
	seed, _ := sql.Open("sqlite", path)
	for _, statement := range v1Statements {
		if _, err := seed.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	// 手动把版本压到「有更新的迁移待跑」的状态：最新是 1，那就没有更高的了——
	// 所以这里只验证「user_version=0 但结构对得上」会被认领而不是拒绝
	if err := AssertCurrent(ctx, path); err != nil {
		t.Errorf("user_version=0 但结构对得上时应当顺手认领：%v", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	version, _ := readUserVersion(ctx, db)
	if version != 1 {
		t.Errorf("user_version = %d，期望 1（被认领）", version)
	}
}

func TestState_只读且不建库(t *testing.T) {
	path := filepath.Join(t.TempDir(), "没建.db.sqlite")
	state := State(context.Background(), path)
	if state.Exists {
		t.Error("不存在的库 Exists 应为 false")
	}
	if state.Error != "" {
		t.Errorf("不存在的库不该报错：%q", state.Error)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("State 不该顺手建库")
	}
}

// TestMigrate_真库认领 用仓库里那份**真实的** data/db.sqlite 跑一遍。
//
// 这是决定 5（保留今天的数据 + 版本化迁移）唯一有意义的验收：不是「我造了一份
// 看起来像的库」，而是「打开用户真正在用的那份，一行不丢」。先复制再跑，
// **绝不碰原库**。
func TestMigrate_真库认领(t *testing.T) {
	real := filepath.Join("..", "..", "..", "data", "db.sqlite")
	if _, err := os.Stat(real); err != nil {
		t.Skip("真库不存在（这台机器还没建过库），跳过")
	}

	dir := t.TempDir()
	copyOf := filepath.Join(dir, "db.sqlite")
	if err := copyFile(real, copyOf); err != nil {
		t.Skipf("复制真库失败，跳过：%v", err)
	}

	before, err := Open(copyOf)
	if err != nil {
		t.Skipf("打开真库副本失败，跳过：%v", err)
	}
	want := counts(t, before)
	_ = before.Close()

	state, err := Migrate(context.Background(), copyOf)
	if err != nil {
		t.Fatalf("认领真库失败：%v", err)
	}
	if !state.Adoptable {
		t.Errorf("真库应可认领。tables=%v", state.Tables)
	}

	after, err := Open(copyOf)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = after.Close() }()
	got := counts(t, after)
	for table, n := range want {
		if got[table] != n {
			t.Errorf("%s 行数 %d → %d", table, n, got[table])
		}
	}
	t.Logf("真库认领成功：v0 → v%d，documents=%d memories=%d messages=%d",
		state.Version, got["documents"], got["memories"], got["messages"])
}

func copyFile(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, data, 0o644)
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
