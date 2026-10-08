package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/zjzhang-cn/fka-go/mcp/memory/internal/domain"
	"github.com/zjzhang-cn/fka-go/mcp/memory/internal/searchterms"
)

// DB 是存储层对外的全部能力。
//
// ## 权限过滤写在这里，不留给实现自觉
//
// FindMemories 的 ViewerWxid 是**必填**的：private 只对属主可见。漏掉它不是
// 「显示多了」，是**别人的私有数据进了结果**。过滤必须发生在 SQL 的 WHERE 里，
// 不是查完之后——后者要记得在每一处都加一遍，漏一处就泄漏。
type DB struct {
	db *sql.DB
}

// New 拿一个已打开的连接造 DB。**不跑迁移**——见 Migrate。
//
// 这里曾经还接一个 `path`（配一个 `Path()` 读取口），用来给调用方报「库在哪」——
// 而零个调用方用它：路径在 `main.go` 里本来就有（`resolveDBPath` 的返回值），
// 存储层再存一份只是让两处可能不一致。
func New(db *sql.DB) *DB { return &DB{db: db} }

// Close 关连接。
func (d *DB) Close() error { return d.db.Close() }

// ── 家庭记忆 ─────────────────────────────────────────────

// Memory 记忆的一行。
type Memory struct {
	ID         string
	Type       string
	Content    string
	OwnerWxid  string
	Visibility string
	CreatedAt  int64
}

// DefaultMemoryLimit 记忆列表默认返回多少条。
const DefaultMemoryLimit = 10

// capacityHint 预分配时的容量提示**上限**。
//
// 调用方可以传一个很大的 limit 来表达「都要」。而 `make([]T, 0, limit)` 会**按
// limit 预分配**——传 1<<30 就是当场申请几十 GB 然后被 OOM kill。
//
// 库函数不能因为调用方传了个大数就炸，所以容量提示单独封顶：**真正 append 时 Go
// 会按需扩容**，多几次拷贝而已。
const capacityHint = 64

// capacityFor 预分配多少个元素。**上限就是 capacityHint**——理由见上，
// 三个分支写出来与这个 min 是同一件事。
func capacityFor(limit int) int {
	if limit <= 0 {
		return capacityHint
	}
	return min(limit, capacityHint)
}

// InsertMemory 记一条记忆。
func (d *DB) InsertMemory(ctx context.Context, m Memory) error {
	// **默认值取自领域词汇，不写字符串字面量**：`domain.VisPublic` 是那条规则的
	// 一处出处（包注释写着「schema 反过来 import 它」——以前并没有，字面量散在
	// 这里和 `server.go` 两处，改一级可见性只会改到一处）
	visibility := m.Visibility
	if visibility == "" {
		visibility = string(domain.VisPublic)
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO memories (id, type, content, owner_wxid, visibility, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		m.ID, m.Type, m.Content, m.OwnerWxid, visibility, m.CreatedAt)
	if err != nil {
		return fmt.Errorf("记记忆失败：%w", err)
	}
	return nil
}

// FindMemoriesInput 检索记忆的输入。
type FindMemoriesInput struct {
	// Query 空格分隔的关键词，词之间是「且」
	Query string
	// ViewerWxid 提问者。**必填**——权限过滤靠它
	ViewerWxid string
	// Limit 为 0 时用 DefaultMemoryLimit
	Limit int
}

// FindMemoriesResult 检索结果。
type FindMemoriesResult struct {
	Hits []Memory
	// Total 满足条件的总数（不受 Limit 影响）
	Total int
}

// scopeWhere 权限过滤谓词与参数的**唯一出处**。
//
// `(visibility = ? OR owner_wxid = ?)` 是本 server 唯一的数据防线：散在多处的话，
// 漏掉的那一处不会报错、只会泄漏。所以谓词与参数绑在一起返回——任何新的读查询
// 都从这里取，不许手写。公开那一档取自 domain.VisPublic：它同时是「写进去的值」
// 与「人人能查到的值」，两处必须一致。
//
// （用例里仍写字符串字面量：那钉的是库里实际存了什么；拿常量去测就是自证。）
func scopeWhere(viewerWxid string) (string, []any) {
	return `(visibility = ? OR owner_wxid = ?)`, []any{string(domain.VisPublic), viewerWxid}
}

// FindMemories 关键词检索。
//
// 权限过滤进 WHERE。**这是本函数最要紧的一处**：private 的记忆不进别人的检索，
// 也不作为别人问答的依据——不是「看不见」而是「不参与」。
func (d *DB) FindMemories(ctx context.Context, in FindMemoriesInput) (FindMemoriesResult, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = DefaultMemoryLimit
	}

	// **权限过滤在 WHERE 里，每次查询都带**。谓词与参数从 scopeWhere 取——
	// 手写一份就多一处「可能忘了加」的地方
	scope, args := scopeWhere(in.ViewerWxid)
	where := []string{scope}

	// 词之间是「且」：每个词都要出现在内容里
	for _, term := range searchterms.SplitTerms(in.Query) {
		where = append(where, `content LIKE ? ESCAPE '\'`)
		args = append(args, "%"+searchterms.EscapeLike(term)+"%")
	}
	clause := "WHERE " + strings.Join(where, " AND ")

	var total int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM memories "+clause, args...).
		Scan(&total); err != nil {
		return FindMemoriesResult{}, fmt.Errorf("数记忆失败：%w", err)
	}

	// rowid DESC 兜底：同毫秒的两条只按 created_at 排会是不确定顺序
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, type, content, owner_wxid, visibility, created_at
		 FROM memories `+clause+`
		 ORDER BY created_at DESC, rowid DESC
		 LIMIT ?`, append(args, limit)...)
	if err != nil {
		return FindMemoriesResult{}, fmt.Errorf("查记忆失败：%w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Memory, 0, capacityFor(limit))
	for rows.Next() {
		var m Memory
		if err := rows.Scan(&m.ID, &m.Type, &m.Content, &m.OwnerWxid, &m.Visibility, &m.CreatedAt); err != nil {
			return FindMemoriesResult{}, err
		}
		out = append(out, m)
	}
	return FindMemoriesResult{Hits: out, Total: total}, rows.Err()
}
