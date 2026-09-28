package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zjzhang-cn/fka-go/mcp/memory/internal/searchterms"
)

// ErrNotFound 找不到。**调用方据此区分「没有」与「坏了」**。
var ErrNotFound = errors.New("没有找到")

// DB 是存储层对外的全部能力。
//
// ## 权限过滤写在这里，不留给实现自觉
//
// FindMemories 的 ViewerWxid 是**必填**的：private 只对属主可见。漏掉它不是
// 「显示多了」，是**别人的私有数据进了结果**。过滤必须发生在 SQL 的 WHERE 里，
// 不是查完之后——后者要记得在每一处都加一遍，漏一处就泄漏。
type DB struct {
	db   *sql.DB
	path string
}

// New 拿一个已打开的连接造 DB。**不跑迁移**——见 Migrate。
func New(db *sql.DB, path string) *DB { return &DB{db: db, path: path} }

// Path 库路径。
func (d *DB) Path() string { return d.path }

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

func capacityFor(limit int) int {
	if limit <= 0 {
		return capacityHint
	}
	if limit < capacityHint {
		return limit
	}
	return capacityHint
}

// InsertMemory 记一条记忆。
func (d *DB) InsertMemory(ctx context.Context, m Memory) error {
	visibility := m.Visibility
	if visibility == "" {
		visibility = "public"
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

// FindMemories 关键词检索。
//
// 权限过滤进 WHERE。**这是本函数最要紧的一处**：private 的记忆不进别人的检索，
// 也不作为别人问答的依据——不是「看不见」而是「不参与」。
func (d *DB) FindMemories(ctx context.Context, in FindMemoriesInput) (FindMemoriesResult, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = DefaultMemoryLimit
	}

	where := []string{`(visibility = ? OR owner_wxid = ?)`}
	args := []any{"public", in.ViewerWxid}

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
