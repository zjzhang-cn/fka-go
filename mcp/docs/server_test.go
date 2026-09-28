package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zjzhang-cn/fka-go/internal/nas"
	"github.com/zjzhang-cn/fka-go/internal/store"
)

const (
	alice = "o9cq80_wW26MUpS_bTllNSzseA5k@im.wechat"
	bob   = "o9cq80xMCchA6gPj7HO1s_MPeJn4@im.wechat"
	// stranger 一个谁都不是的人。**下面几条测试的全部意义就在这里**
	stranger = "o9cq80_never_seen_before@im.wechat"
)

// fixture 一座临时库 + 一个临时存储根，仿真实数据：两个属主、文档默认 private。
type fixture struct {
	server *impl
	dbPath string
	root   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db.sqlite")
	root := filepath.Join(dir, "nas")

	if _, err := store.Migrate(context.Background(), dbPath); err != nil {
		t.Fatalf("建库失败：%v", err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("开库失败：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// 仿真实的两份文档：alice 一份「居住证」，bob 一份「课前测」
	seed(t, store.New(db, dbPath), root, store.Document{
		ID:       "aaaa1111-2222-4333-8444-555555555555",
		Filename: "居住证(卡)申领情况记录表.pdf", Filepath: "files/" + bob + "/居住证.pdf",
		OwnerWxid: bob, Visibility: "private", ContentHash: "h1", Status: "ready",
		CreatedAt: 1, UpdatedAt: 1,
	}, "居住证 编号 12345 有效期限 2027年")

	seed(t, store.New(db, dbPath), root, store.Document{
		ID:       "bbbb2222-3333-4444-8555-666666666666",
		Filename: "G3-24 课前测.docx", Filepath: "files/" + alice + "/课前测.docx",
		OwnerWxid: alice, Visibility: "private", ContentHash: "h2", Status: "ready",
		CreatedAt: 2, UpdatedAt: 2,
	}, "课前测 姓名 张三 分数 88")

	return &fixture{server: &impl{db: store.New(db, dbPath), storageRoot: root}, dbPath: dbPath, root: root}
}

func seed(t *testing.T, db *store.DB, root string, doc store.Document, body string) { //nolint:unused
	t.Helper()
	ctx := context.Background()

	if _, err := db.DB().ExecContext(ctx,
		`INSERT INTO documents (id, filename, filepath, size, owner_wxid, visibility, content_hash, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		doc.ID, doc.Filename, doc.Filepath, int64(len(body)), doc.OwnerWxid,
		doc.Visibility, doc.ContentHash, doc.Status, doc.CreatedAt, doc.UpdatedAt); err != nil {
		t.Fatalf("铺文档失败：%v", err)
	}

	if _, err := nas.SaveExtracted(root, nas.Location{
		ID: doc.ID, OwnerWxid: doc.OwnerWxid, Filename: doc.Filename, Filepath: doc.Filepath,
	}, body, nil); err != nil {
		t.Fatalf("铺解析结果失败：%v", err)
	}
}

// handler server 里那几个 handler 的统一签名。
type handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)

// call 直接调 handler 并解出 JSON 负载。**绕开 agent**——权限行为必须是确定的，
// 不能靠模型「恰好填对了 viewer」。
//
// handler 以参数传入而不是 `call(t, f.server.handleX(...))`：Go 不允许把多返回值
// 展开进一个带额外参数的函数。
func (f *fixture) call(t *testing.T, h handler, ctx context.Context, r mcp.CallToolRequest) (map[string]any, bool) {
	t.Helper()
	result, err := h(ctx, r)
	if err != nil {
		t.Fatalf("handler 返错：%v", err)
	}
	if len(result.Content) == 0 {
		t.Fatalf("没有内容：%+v", result)
	}
	text, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("内容不是文本：%T", result.Content[0])
	}
	if result.IsError {
		return map[string]any{"__error": text.Text}, false
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text.Text), &payload); err != nil {
		t.Fatalf("结果不是 JSON（%v）：%s", err, text.Text)
	}
	return payload, true
}

func request(pairs ...any) mcp.CallToolRequest {
	args := map[string]any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		args[pairs[i].(string)] = pairs[i+1]
	}
	return mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "x", Arguments: args}}
}

// TestSearch_按viewer过滤 这是本 server 最重要的一条。
//
// viewer 由**模型填**，server 按决定不做进程级绑定——所以这句 WHERE 是唯一防线。
// **别人的私有文档绝不能出现在结果里。**
func TestSearch_按viewer过滤(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// 查询是 AND 语义，所以每人只查**自己那份**的词
	cases := []struct {
		name      string
		viewer    string
		query     string
		wantTotal int
	}{
		{"属主看得到自己的", alice, "课前测", 1},
		{"另一个属主看得到自己的", bob, "居住证", 1},
		// ⚠️ 这一条是重点：陌生人对两份 private 都查不到
		{"陌生人查 alice 的词", stranger, "课前测", 0},
		{"陌生人查 bob 的词", stranger, "居住证", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, ok := f.call(t, f.server.handleSearch, ctx, request(
				"query", tc.query, "viewer_wxid", tc.viewer))
			if !ok {
				t.Fatalf("不该失败：%+v", payload)
			}
			if got := int(payload["found"].(float64)); got != tc.wantTotal {
				t.Errorf("found = %d，期望 %d", got, tc.wantTotal)
			}
			// 确认**没看到别人的**那一份，而不只是数量对
			if tc.wantTotal == 0 {
				blob, _ := json.Marshal(payload)
				if !strings.Contains(string(blob), `"found":0`) {
					t.Errorf("陌生人的结果里出现了别人的私有文档：%s", blob)
				}
			}
		})
	}
}

// TestGet_私有文档按属主 这条路径与 search 不同：它是**按 id 精确命中之后**才判权限，
// 所以必须单独验——漏了它，模型只要猜对 id 就能读到别人的文档。
func TestGet_私有文档按属主(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const id = "aaaa1111" // 居住证，属主是 bob

	// 属主本人：拿到正文
	payload, ok := f.call(t, f.server.handleGet, ctx, request("id", id, "viewer_wxid", bob))
	if !ok {
		t.Fatalf("属主本人应当能取到：%+v", payload)
	}
	if content, _ := payload["content"].(string); !strings.Contains(content, "居住证 编号 12345") {
		t.Errorf("正文不对：%v", payload["content"])
	}

	// 别人与陌生人：一律拒绝
	for _, viewer := range []string{alice, stranger} {
		payload, ok := f.call(t, f.server.handleGet, ctx, request("id", id, "viewer_wxid", viewer))
		if ok {
			t.Errorf("viewer=%s 不该取到别人的私有文档：%+v", viewer, payload)
			continue
		}
		if !strings.Contains(payload["__error"].(string), "private") {
			t.Errorf("该说清是权限问题：%v", payload["__error"])
		}
	}
}

// TestGet_前缀匹配与歧义 id 可以只写前几位，不唯一时给候选而不是随便挑一个
func TestGet_前缀匹配与歧义(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// 唯一前缀
	payload, ok := f.call(t, f.server.handleGet, ctx, request("id", "bbbb", "viewer_wxid", alice))
	if !ok {
		t.Fatalf("唯一前缀应当命中：%+v", payload)
	}
	if payload["id"] != "bbbb2222-3333-4444-8555-666666666666" {
		t.Errorf("取错了：%v", payload["id"])
	}

	// 再插一份**同前缀**的，`bbbb2222` 就歧义了
	if _, err := f.server.db.DB().ExecContext(ctx,
		`INSERT INTO documents (id, filename, filepath, owner_wxid, visibility, content_hash, status, created_at, updated_at)
		 VALUES ('bbbb2222-9999-4888-8999-aaaaaaaaaaaa', '同名另一份.pdf', 'files/x/y.pdf', ?, 'private', 'h3', 'ready', 3, 3)`,
		alice); err != nil {
		t.Fatal(err)
	}

	// 歧义前缀：给候选，**不能随便挑一个**——那等于让用户看错文档却不知道
	payload, ok = f.call(t, f.server.handleGet, ctx, request("id", "bbbb2222", "viewer_wxid", alice))
	if ok {
		t.Errorf("歧义前缀不该直接给一份：%+v", payload)
	} else if !strings.Contains(payload["__error"].(string), "请给更完整的 id") {
		t.Errorf("该说清要更完整的 id：%v", payload["__error"])
	}
}

func TestGet_找不到时给出下一步(t *testing.T) {
	f := newFixture(t)
	payload, ok := f.call(t, f.server.handleGet, context.Background(), request("id", "zzzz", "viewer_wxid", alice))
	if ok {
		t.Fatalf("不该成功：%+v", payload)
	}
	if !strings.Contains(payload["__error"].(string), "search_documents") {
		t.Errorf("该告诉模型下一步用什么：%v", payload["__error"])
	}
}

func TestList_按viewer过滤(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	payload, ok := f.call(t, f.server.handleList, ctx, request("viewer_wxid", stranger))
	if !ok {
		t.Fatalf("不该失败：%+v", payload)
	}
	if got := int(payload["total"].(float64)); got != 0 {
		t.Errorf("陌生人看到 %d 份，期望 0", got)
	}

	payload, _ = f.call(t, f.server.handleList, ctx, request("viewer_wxid", alice))
	if got := int(payload["total"].(float64)); got != 1 {
		t.Errorf("alice 应看到 1 份，实际 %d", got)
	}
}

// Test缺viewer一律拒绝 不知道提问者就没法做权限过滤。**绝不能当作匿名放行**。
func Test缺viewer一律拒绝(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for name, h := range map[string]func(mcp.CallToolRequest) (*mcp.CallToolResult, error){
		"search": func(r mcp.CallToolRequest) (*mcp.CallToolResult, error) { return f.server.handleSearch(ctx, r) },
		"list":   func(r mcp.CallToolRequest) (*mcp.CallToolResult, error) { return f.server.handleList(ctx, r) },
		"get":    func(r mcp.CallToolRequest) (*mcp.CallToolResult, error) { return f.server.handleGet(ctx, r) },
	} {
		t.Run(name, func(t *testing.T) {
			// 刻意**不传** viewer_wxid
			result, err := h(request("query", "课前测", "id", "aaaa"))
			if err != nil {
				t.Fatalf("返错：%v", err)
			}
			if !result.IsError {
				t.Fatalf("缺 viewer 时必须拒绝，实际成功：%+v", result)
			}
			text := result.Content[0].(mcp.TextContent).Text
			if !strings.Contains(text, "viewer_wxid") {
				t.Errorf("要说清缺什么：%s", text)
			}
		})
	}
}

// TestSearch_词之间是且
func TestSearch_词之间是且(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// 两个词都在同一份文档里
	payload, _ := f.call(t, f.server.handleSearch, ctx,
		request("query", "课前测 姓名", "viewer_wxid", alice))
	if got := int(payload["found"].(float64)); got != 1 {
		t.Errorf("两个词都在时应命中：%+v", payload)
	}

	// 第二个词不在
	payload, _ = f.call(t, f.server.handleSearch, ctx,
		request("query", "课前测 不存在的词", "viewer_wxid", alice))
	if got := int(payload["found"].(float64)); got != 0 {
		t.Errorf("有一个词不在就不该命中，found=%d：%+v", got, payload)
	}
}

func TestSearch_空检索词拒绝(t *testing.T) {
	f := newFixture(t)
	result, err := f.server.handleSearch(context.Background(), request("query", "   ", "viewer_wxid", alice))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Error("空检索词不该返回全库")
	}
}

// TestSearch_语义模式如实说不可用 **不要静默退化成 keyword**——用户会以为语义搜过了
func TestSearch_语义模式如实说不可用(t *testing.T) {
	f := newFixture(t)
	result, err := f.server.handleSearch(context.Background(),
		request("query", "课前测", "mode", "vector", "viewer_wxid", alice))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("mode=vector 还没建好，该明确拒绝")
	}
	if !strings.Contains(result.Content[0].(mcp.TextContent).Text, "keyword") {
		t.Errorf("该告诉它可以改用什么：%s", result.Content[0].(mcp.TextContent).Text)
	}
}

// TestGet_正文超长截断并说明
func TestGet_正文超长截断并说明(t *testing.T) {
	f := newFixture(t)
	long := strings.Repeat("很长的正文内容。", 2000)
	seed(t, f.server.db, f.root, store.Document{
		ID: "dddd4444-5555-4666-8777-888888888888", Filename: "长文档.pdf",
		Filepath: "files/" + alice + "/长文档.pdf", OwnerWxid: alice, Visibility: "private",
		ContentHash: "h4", Status: "ready", CreatedAt: 4, UpdatedAt: 4,
	}, long)

	payload, ok := f.call(t, f.server.handleGet, context.Background(), request("id", "dddd", "viewer_wxid", alice))
	if !ok {
		t.Fatalf("%+v", payload)
	}
	content, _ := payload["content"].(string)
	if len([]rune(content)) > maxDocumentChars+10 {
		t.Errorf("截断了但还是太长：%d 字符", len([]rune(content)))
	}
	if !strings.Contains(content, "已截断") {
		t.Errorf("该说明截断了：%q", content[len(content)-20:])
	}
	if payload["contentTruncated"] != true {
		t.Error("该明确标出 contentTruncated")
	}
}

// TestReadLimit_模型说100也不给 那会挤掉后续轮次的预算
func TestReadLimit_模型说100也不给(t *testing.T) {
	for raw, want := range map[string]int{
		"":     defaultSearchLimit,
		"3":    3,
		"100":  maxResultLimit,
		"0":    defaultSearchLimit,
		"abc":  defaultSearchLimit,
		"-5":   defaultSearchLimit,
		"9999": maxResultLimit,
	} {
		if got := readLimit(request("limit", raw), "limit", defaultSearchLimit); got != want {
			t.Errorf("readLimit(%q) = %d，期望 %d", raw, got, want)
		}
	}
}

// TestExtractSnippet 片段必须**压成一行**：正文是 Markdown，片段里若带换行，
// 塞进列表之后「一个元素其实是两行」，按行做断言的测试就永远测不到想测的东西。
func TestExtractSnippet(t *testing.T) {
	t.Run("压成一行", func(t *testing.T) {
		got := ExtractSnippet("# 标题\n\n正文里有\n换行的 关键词。", []string{"关键词"}, 40)
		if got == nil {
			t.Fatal("没命中")
		}
		if strings.ContainsAny(*got, "\n\r") {
			t.Errorf("片段里有换行：%q", *got)
		}
	})

	t.Run("最靠前的词做中心", func(t *testing.T) {
		content := "开头有甲。中间有乙。后面有丙。"
		got := ExtractSnippet(content, []string{"丙", "甲"}, 5)
		if got == nil {
			t.Fatal("没命中")
		}
		// 中心是「甲」（更靠前），所以片段里不该出现「丙」
		if strings.Contains(*got, "丙") {
			t.Errorf("应从最靠前的命中处取：%q", *got)
		}
	})

	t.Run("首尾补省略号", func(t *testing.T) {
		got := ExtractSnippet("前面还有很多字，后面也有很多字，中间是关键词，再后面还有字", []string{"关键词"}, 3)
		if got == nil {
			t.Fatal("没命中")
		}
		if !strings.HasPrefix(*got, "…") || !strings.HasSuffix(*got, "…") {
			t.Errorf("两端都该有省略号：%q", *got)
		}
	})

	t.Run("没命中返回 nil", func(t *testing.T) {
		if got := ExtractSnippet("正文", []string{"关键词"}, 40); got != nil {
			t.Errorf("= %q", *got)
		}
	})

	t.Run("空正文返回 nil", func(t *testing.T) {
		if got := ExtractSnippet("", []string{"关键词"}, 40); got != nil {
			t.Errorf("= %q", *got)
		}
	})
}

// TestSearch_正文命中才给片段 命中在文件名时没有片段可给
func TestSearch_正文命中才给片段(t *testing.T) {
	f := newFixture(t)
	payload, ok := f.call(t, f.server.handleSearch, context.Background(), request("query", "居住证", "viewer_wxid", bob))
	if !ok {
		t.Fatalf("%+v", payload)
	}
	docs := payload["documents"].([]any)
	if len(docs) != 1 {
		t.Fatalf("%+v", payload)
	}
	hit := docs[0].(map[string]any)
	// 「居住证」既在文件名也在正文里 → both
	if hit["matched"] != string(MatchedBoth) {
		t.Errorf("matched = %v，期望 both", hit["matched"])
	}
	if hit["snippet"] == nil {
		t.Error("正文命中就该给片段")
	}
}

// TestMain_存储根不存在也不崩 还没挂 NAS 时要给一句人话，而不是让 server 起不来
func TestMain_存储根不存在也不崩(t *testing.T) {
	dir := t.TempDir()
	root, err := resolveStorageRoot([]string{"--storage", filepath.Join(dir, "没有这个")})
	if err != nil {
		t.Fatalf("解析存储根失败：%v", err)
	}
	if !filepath.IsAbs(root) {
		t.Errorf("存储根必须是绝对路径（相对路径按服务进程的 cwd 解析）：%q", root)
	}
	if _, err := os.Stat(root); err == nil {
		t.Errorf("解析不该顺手建目录")
	}
}
