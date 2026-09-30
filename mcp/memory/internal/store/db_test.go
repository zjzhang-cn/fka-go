// 本文件是 `memories` 表**读写**的用例。
//
// ## 为什么它此前是零用例，而这里必须是第一优先
//
// `migrate_test.go` 的 10 个用例全是迁移——建库、认领、幂等，一条都不碰 `FindMemories`。
// 而 `(visibility = ? OR owner_wxid = ?)` 是本 server **唯一的数据防线**（`db.go` 的
// `FindMemories` 注释）：它必须留在 SQL 的 `WHERE` 里、每次查询都带。漏一次不是
// 「显示多了」，是**别人的私有数据进了别人的上下文**。
//
// 这条防线此前只有 code review 守着，而 `docs/port-plan.md` 的不变量表声称
// 「每个查询」都有测试。
package store

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

// newStoreDB 造一个建到最新、隔离在临时目录里的库。
func newStoreDB(t *testing.T) *DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), dbName)
	if _, err := Migrate(context.Background(), path); err != nil {
		t.Fatalf("迁移失败：%v", err)
	}

	conn, err := Open(path)
	if err != nil {
		t.Fatalf("打开库失败：%v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return New(conn)
}

// remember 记一条记忆。createdAt 由调用方给，好在排序用例里造出确定的时间差。
func remember(t *testing.T, db *DB, id, content, owner, visibility string, createdAt int64) {
	t.Helper()

	if err := db.InsertMemory(context.Background(), Memory{
		ID: id, Type: "knowledge", Content: content,
		OwnerWxid: owner, Visibility: visibility, CreatedAt: createdAt,
	}); err != nil {
		t.Fatalf("写记忆失败：%v", err)
	}
}

// contents 把结果里的正文抽出来，便于断言集合。
func contents(hits []Memory) []string {
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		out = append(out, hit.Content)
	}
	return out
}

// TestFindMemories_别人的private不进结果 见 `db.go` 的 FindMemories：
// 过滤发生在 SQL 的 WHERE 里，不是查完再筛——后者要每处都记得加一遍，漏一处就泄漏。
func TestFindMemories_别人的private不进结果(t *testing.T) {
	db := newStoreDB(t)
	remember(t, db, "m1", "自己公开的", "wx-owner", "public", 1000)
	remember(t, db, "m2", "自己私密的", "wx-owner", "private", 2000)
	remember(t, db, "m3", "别人公开的", "wx-other", "public", 3000)
	remember(t, db, "m4", "别人私密的", "wx-other", "private", 4000)

	ctx := context.Background()

	// 属主：自己两条（公开 + 私密）都能看到，别人只看到 public
	owner, err := db.FindMemories(ctx, FindMemoriesInput{ViewerWxid: "wx-owner"})
	if err != nil {
		t.Fatal(err)
	}
	if owner.Total != 3 {
		t.Errorf("属主该看到 3 条（自己 2 + 别人的 public），实际 %d 条：%v", owner.Total, contents(owner.Hits))
	}
	for _, hit := range owner.Hits {
		if hit.Content == "别人私密的" {
			t.Fatal("别人的 private 进了结果——这是数据泄漏，不是「显示多了」")
		}
	}

	// 没有身份的人（空 viewer）：只能看到 public
	// （工具层会先拒掉空 viewer，这里是数据层自己的兜底）
	anonymous, err := db.FindMemories(ctx, FindMemoriesInput{ViewerWxid: ""})
	if err != nil {
		t.Fatal(err)
	}
	if anonymous.Total != 2 {
		t.Errorf("空身份只该看到 2 条 public，实际 %d 条：%v", anonymous.Total, contents(anonymous.Hits))
	}

	// 换一个人：私密的那条只对属主可见，换谁都一样
	other, err := db.FindMemories(ctx, FindMemoriesInput{ViewerWxid: "wx-third"})
	if err != nil {
		t.Fatal(err)
	}
	if other.Total != 2 {
		t.Errorf("第三个人只该看到 2 条 public，实际 %d 条：%v", other.Total, contents(other.Hits))
	}
}

// TestFindMemories_关键词之间是且 多给一个词只会更严，不会更宽。
func TestFindMemories_关键词之间是且(t *testing.T) {
	db := newStoreDB(t)
	remember(t, db, "m1", "三亚 全家 旅行", "wx", "public", 1000)
	remember(t, db, "m2", "三亚 出差", "wx", "public", 2000)
	remember(t, db, "m3", "北京 全家", "wx", "public", 3000)

	ctx := context.Background()
	cases := []struct {
		query string
		want  []string
	}{
		{"三亚", []string{"三亚 出差", "三亚 全家 旅行"}},
		{"三亚 全家", []string{"三亚 全家 旅行"}},
		// 词的顺序不影响结果（SQL 里是若干 AND，不是按顺序匹配）
		{"全家 三亚", []string{"三亚 全家 旅行"}},
		{"全家", []string{"北京 全家", "三亚 全家 旅行"}},
		{"三亚 不存在的词", nil},
		// 空白与多余分隔符不该造出空词（空词会变成 LIKE '%%'，把所有东西都捞回来）
		{"  三亚   全家  ", []string{"三亚 全家 旅行"}},
	}

	for _, c := range cases {
		got, err := db.FindMemories(ctx, FindMemoriesInput{Query: c.query, ViewerWxid: "wx"})
		if err != nil {
			t.Fatalf("%q 检索失败：%v", c.query, err)
		}
		// 结果按 created_at 倒序，所以期望值也按「新的在前」写
		if !slices.Equal(contents(got.Hits), c.want) {
			t.Errorf("%q 的结果 = %v，期望 %v", c.query, contents(got.Hits), c.want)
		}
	}
}

// TestFindMemories_新的在前_同毫秒也确定 同毫秒的两条只按 created_at 排是不确定
// 顺序（排序不稳定 + 存储顺序），所以查询里还有 `rowid DESC` 兜底。
func TestFindMemories_新的在前_同毫秒也确定(t *testing.T) {
	db := newStoreDB(t)
	remember(t, db, "m1", "较早", "wx", "public", 1000)
	remember(t, db, "m2", "较晚", "wx", "public", 2000)
	remember(t, db, "m3", "同毫秒 A", "wx", "public", 2000)
	remember(t, db, "m4", "同毫秒 B", "wx", "public", 2000)

	ctx := context.Background()
	in := FindMemoriesInput{ViewerWxid: "wx"}

	first, err := db.FindMemories(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.FindMemories(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(contents(first.Hits), contents(second.Hits)) {
		t.Errorf("两次查询顺序不一致：%v vs %v", contents(first.Hits), contents(second.Hits))
	}

	want := []string{"同毫秒 B", "同毫秒 A", "较晚", "较早"}
	if !slices.Equal(contents(first.Hits), want) {
		t.Errorf("顺序 = %v，期望 %v（新的在前，同毫秒按 rowid 倒序）", contents(first.Hits), want)
	}
}

// TestFindMemories_limit不改变total limit 管「返回几条」，total 管「一共几条符合」。
func TestFindMemories_limit不改变total(t *testing.T) {
	db := newStoreDB(t)
	for i := 1; i <= 5; i++ {
		remember(t, db, fmt.Sprintf("m%d", i), fmt.Sprintf("记忆 %d", i), "wx", "public", int64(i))
	}

	ctx := context.Background()
	got, err := db.FindMemories(ctx, FindMemoriesInput{ViewerWxid: "wx", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Hits) != 2 {
		t.Errorf("limit=2 该只返回 2 条，实际 %d 条", len(got.Hits))
	}
	if got.Total != 5 {
		t.Errorf("total 不受 limit 影响，该是 5，实际 %d", got.Total)
	}

	// limit=0 = 用默认值；5 条还不到默认的 10，所以全给
	def, err := db.FindMemories(ctx, FindMemoriesInput{ViewerWxid: "wx"})
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Hits) != 5 {
		t.Errorf("不给 limit 该用默认 %d 条（这里只有 5 条），实际 %d 条",
			DefaultMemoryLimit, len(def.Hits))
	}
}

// TestInsertMemory_不给visibility默认public 与文档相反：记忆默认 public
// （家庭记忆本就该被家人问到），而文档默认 private。落成空串的话谁都查不到。
func TestInsertMemory_不给visibility默认public(t *testing.T) {
	db := newStoreDB(t)

	if err := db.InsertMemory(context.Background(), Memory{
		ID: "m1", Type: "knowledge", Content: "没写可见性",
		OwnerWxid: "wx-owner", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// 别人看得到 → 落库的是 public 而不是空串
	got, err := db.FindMemories(context.Background(), FindMemoriesInput{ViewerWxid: "wx-other"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(contents(got.Hits), []string{"没写可见性"}) {
		t.Errorf("缺 visibility 该落成 public（家人能问到），实际结果：%v", contents(got.Hits))
	}
}

// TestFindMemories_通配符要转义 不转义的话，用户敲的 `%` 会变成「匹配任意内容」
// ——搜 `100%` 把整库捞出来，而用户以为自己在搜一个百分号。
func TestFindMemories_通配符要转义(t *testing.T) {
	db := newStoreDB(t)
	remember(t, db, "m1", "进度 100% 完成", "wx", "public", 1000)
	remember(t, db, "m2", "无关的另一条", "wx", "public", 2000)

	ctx := context.Background()

	percent, err := db.FindMemories(ctx, FindMemoriesInput{Query: "100%", ViewerWxid: "wx"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(contents(percent.Hits), []string{"进度 100% 完成"}) {
		t.Errorf("`%%` 该按字面匹配，实际：%v", contents(percent.Hits))
	}

	// `_` 匹配任意单字符：不转义时「无关_另一条」会命中「无关的另一条」
	underscore, err := db.FindMemories(ctx, FindMemoriesInput{Query: "无关_另一条", ViewerWxid: "wx"})
	if err != nil {
		t.Fatal(err)
	}
	if len(underscore.Hits) != 0 {
		t.Errorf("`_` 该按字面匹配（转义掉），实际命中了：%v", contents(underscore.Hits))
	}
}
