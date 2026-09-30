// 本文件是**工具层**的用例：参数怎么读、结果怎么说给人听。
//
// ## 为什么这里必须有用例
//
// `store` 那 10 个用例全是迁移，「参数读对了没有」不在它的射程里。而这一层最容易
// 出的错**不报错**：声明与读取方式不一致时（`mcp.WithNumber` 声明、`GetString` 读），
// 参数会静默失效——照样返回结果，只是条数不由调用方定。真机上看不出来，只能靠这里。
package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zjzhang-cn/fka-go/mcp/memory/internal/store"
)

// newTestDB 造一个已迁移、隔离在临时目录里的库。用完关掉。
func newTestDB(t *testing.T) *store.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "memory.sqlite")
	if _, err := store.Migrate(context.Background(), path); err != nil {
		t.Fatalf("迁移失败：%v", err)
	}

	conn, err := store.Open(path)
	if err != nil {
		t.Fatalf("打开库失败：%v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return store.New(conn, path)
}

// insertMemory 记一条 public 记忆。
func insertMemory(t *testing.T, db *store.DB, content string) {
	t.Helper()

	if err := db.InsertMemory(context.Background(), store.Memory{
		ID: newID(), Type: "knowledge", Content: content,
		OwnerWxid: "wx-owner", Visibility: "public", CreatedAt: nowMs(),
	}); err != nil {
		t.Fatalf("写记忆失败：%v", err)
	}
}

// callRequest 造一个工具调用请求。
//
// **参数值如实用 `float64`**：JSON 解出来的数字就是这个类型，而「声明是 number、
// 却按 string 读」正是要防的那件事——用 `"1"` 去测会把 bug 测没。
func callRequest(args map[string]any) mcp.CallToolRequest {
	var request mcp.CallToolRequest
	request.Params.Arguments = args
	return request
}

// resultText 取结果里的那段文字。
func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()

	if len(result.Content) == 0 {
		t.Fatal("结果里没有内容")
	}
	return mcp.GetTextFromContent(result.Content[0])
}

// Test搜索_limit按声明按数字读 声明是 `mcp.WithNumber`，读取就必须用 `GetInt`：
// `GetString` 只在值是 Go `string` 时返回，而 JSON 数字是 `float64`——于是它永远
// 拿到空串，limit 永远是默认 10，调用方指定的条数静默失效。
func Test搜索_limit按声明按数字读(t *testing.T) {
	db := newTestDB(t)
	for i := 1; i <= 3; i++ {
		insertMemory(t, db, fmt.Sprintf("第 %d 条记忆", i))
	}

	impl := &memoryServer{db: db}
	result, err := impl.handleSearch(context.Background(), callRequest(map[string]any{
		"viewer_wxid": "wx-viewer",
		"limit":       float64(1),
	}))
	if err != nil {
		t.Fatal(err)
	}

	body := resultText(t, result)
	if !strings.Contains(body, "找到 1 条记忆") {
		t.Errorf("limit=1 该只返回 1 条，实际：%s", body)
	}
	// 总数不受 limit 影响——它回答的是「一共符合条件几条」
	if !strings.Contains(body, "共 3 条") {
		t.Errorf("总数该是 3（limit 只影响返回条数）：%s", body)
	}
}

// Test搜索_不给limit用默认10 默认值是 `store.DefaultMemoryLimit`，不是「全给」。
func Test搜索_不给limit用默认10(t *testing.T) {
	db := newTestDB(t)
	for i := 1; i <= 12; i++ {
		insertMemory(t, db, fmt.Sprintf("第 %d 条记忆", i))
	}

	impl := &memoryServer{db: db}
	result, err := impl.handleSearch(context.Background(), callRequest(map[string]any{
		"viewer_wxid": "wx-viewer",
	}))
	if err != nil {
		t.Fatal(err)
	}

	body := resultText(t, result)
	if !strings.Contains(body, "找到 10 条记忆") {
		t.Errorf("不给 limit 该用默认 10 条，实际：%s", body)
	}
	if !strings.Contains(body, "共 12 条") {
		t.Errorf("总数该是 12：%s", body)
	}
}

// Test搜索_别人的private不进结果 这是本 server **唯一的数据防线**
// （`(visibility = ? OR owner_wxid = ?)` 在 SQL 的 WHERE 里，见 store/db.go）。
// 它以前零用例——而 `docs/port-plan.md` 的不变量表声称「每个查询」都有测试守。
func Test搜索_别人的private不进结果(t *testing.T) {
	db := newTestDB(t)

	if err := db.InsertMemory(context.Background(), store.Memory{
		ID: newID(), Type: "knowledge", Content: "只有属主能问到的秘密",
		OwnerWxid: "wx-owner", Visibility: "private", CreatedAt: nowMs(),
	}); err != nil {
		t.Fatalf("写记忆失败：%v", err)
	}
	insertMemory(t, db, "家人都能问到的常识")

	impl := &memoryServer{db: db}

	// 别人来问：private 那条**不参与**（不是「看不见」，是不进结果集）
	other, err := impl.handleSearch(context.Background(), callRequest(map[string]any{
		"viewer_wxid": "wx-someone-else",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if body := resultText(t, other); strings.Contains(body, "秘密") {
		t.Errorf("别人的 private 记忆进了结果——这是数据泄漏：%s", body)
	}

	// 属主自己问：看得到
	owner, err := impl.handleSearch(context.Background(), callRequest(map[string]any{
		"viewer_wxid": "wx-owner",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if body := resultText(t, owner); !strings.Contains(body, "秘密") {
		t.Errorf("属主该看得到自己的 private 记忆：%s", body)
	}
}

// Test搜索_viewer缺失要拒 没有提问者就没法做权限过滤——宁可回一句也不猜一个身份。
func Test搜索_viewer缺失要拒(t *testing.T) {
	impl := &memoryServer{db: newTestDB(t)}

	result, err := impl.handleSearch(context.Background(), callRequest(map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Error("缺 viewer_wxid 该回一条 IsError 的结果（给模型看的一句话，不是异常）")
	}
	if body := resultText(t, result); !strings.Contains(body, "viewer_wxid") {
		t.Errorf("该说清缺的是哪个参数：%s", body)
	}
}

// Test停机信号不算失败 见 `exitCodeFor`：退出码是契约（0 成功 / 1 预期内的失败），
// 而 SDK 收到 SIGINT / SIGTERM 时让 `ServeStdio` 返回 `context.Canceled`。
//
// 实测（fifo 喂住 stdin、1.5 秒后 `kill -TERM`）：修之前退出码是 **1**，日志最后一行
// 是「记忆 server 退出：context canceled」——每一次干净停机都被记成一次崩溃，
// systemd 的 Restart 策略跟着走。
func Test停机信号不算失败(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"正常结束", nil, 0},
		{"收到停机信号", context.Canceled, 0},
		{"真的失败", errors.New("stdio 断了"), 1},
		{"包装过的停机信号", fmt.Errorf("ServeStdio 返回：%w", context.Canceled), 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := exitCodeFor(c.err); got != c.want {
				t.Errorf("exitCodeFor(%v) = %d，期望 %d", c.err, got, c.want)
			}
		})
	}
}
