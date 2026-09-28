package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zjzhang-cn/fka-go/internal/searchterms"
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

// DB 暴露底层连接，给只读查询（db stats / doctor）用。
func (d *DB) DB() *sql.DB { return d.db }

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

// FindMemories 关键词检索。**永不返错**——记忆的契约是「查不到就是查不到」，
// 而一个坏查询不该让 `/回忆` 崩掉。
//
// 权限过滤进 WHERE。**这是本函数最要紧的一处**：private 的记忆不进别人的检索，
// 也不作为别人问答的依据——与文档一样严，不是「看不见」而是「不参与」。
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

	out := make([]Memory, 0, limit)
	for rows.Next() {
		var m Memory
		if err := rows.Scan(&m.ID, &m.Type, &m.Content, &m.OwnerWxid, &m.Visibility, &m.CreatedAt); err != nil {
			return FindMemoriesResult{}, err
		}
		out = append(out, m)
	}
	return FindMemoriesResult{Hits: out, Total: total}, rows.Err()
}

// ── 文档 ────────────────────────────────────────────────

// Document 文档的一行。列名是 Go 侧的 camelCase。
type Document struct {
	ID       string
	Filename string
	// Filepath **相对存储根**的原始文件路径
	Filepath  string
	Size      *int64
	OwnerWxid string
	// Visibility 默认 private（上传即私有，家人要能问到需显式标 public）
	Visibility  string
	ContentHash string
	Status      string
	Error       *string
	Attempts    int
	CreatedAt   int64
	UpdatedAt   int64
	// Annotations 用户批注。读时解析，没有就是空
	Annotations []Annotation
	// ContentBytes 解析结果的字节数。**从磁盘现算**，库里没有这一列
	ContentBytes int64
}

const documentColumns = `id, filename, filepath, size, owner_wxid, visibility, content_hash,
	status, error, attempts, annotations, created_at, updated_at`

// ListDocumentsInput 列文档的条件。
type ListDocumentsInput struct {
	Limit      int
	Status     string
	OwnerWxid  string
	Visibility string
	// ViewerWxid 「这个人的视角」：public 的加上他自己的。**权限过滤**，
	// 不是精确匹配某人。空 = 不过滤（本机运维视角，CLI 用）
	ViewerWxid string
}

// ListDocumentsResult 列文档的结果。
type ListDocumentsResult struct {
	Rows []Document
	// Total 满足条件的总数（不受 Limit 影响）
	Total int
	// Truncated 是否因为 Limit 被截断
	Truncated bool
}

// ListDocuments 列文档。**筛选写进 SQL**，不是取回来再过滤。
func (d *DB) ListDocuments(ctx context.Context, in ListDocumentsInput) (ListDocumentsResult, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}

	where := make([]string, 0, 4)
	args := make([]any, 0, 4)
	if in.Status != "" {
		where = append(where, "status = ?")
		args = append(args, in.Status)
	}
	if in.OwnerWxid != "" {
		where = append(where, "owner_wxid = ?")
		args = append(args, in.OwnerWxid)
	}
	if in.Visibility != "" {
		where = append(where, "visibility = ?")
		args = append(args, in.Visibility)
	}
	if in.ViewerWxid != "" {
		// 权限过滤进 WHERE。**漏一次就漏数据**
		where = append(where, "(visibility = ? OR owner_wxid = ?)")
		args = append(args, "public", in.ViewerWxid)
	}

	clause := ""
	if len(where) > 0 {
		clause = "WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM documents "+clause, args...).
		Scan(&total); err != nil {
		return ListDocumentsResult{}, fmt.Errorf("数文档失败：%w", err)
	}

	rows, err := d.db.QueryContext(ctx,
		"SELECT "+documentColumns+" FROM documents "+clause+" ORDER BY created_at DESC LIMIT ?",
		append(args, limit)...)
	if err != nil {
		return ListDocumentsResult{}, fmt.Errorf("查文档失败：%w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Document, 0, limit)
	for rows.Next() {
		doc, err := scanDocument(rows)
		if err != nil {
			return ListDocumentsResult{}, err
		}
		out = append(out, doc)
	}
	return ListDocumentsResult{Rows: out, Total: total, Truncated: total > len(out)}, rows.Err()
}

// FindDocument 按 id 或 id 前缀找一份。
//
// 支持前缀是因为列表只显示 8 位 id，让人去别处抠完整 id 很别扭。**有歧义时返错**，
// 不随便挑一个——那等于让用户看错文档却不知道。
func (d *DB) FindDocument(ctx context.Context, idOrPrefix string) (Document, error) {
	row := d.db.QueryRowContext(ctx,
		"SELECT "+documentColumns+" FROM documents WHERE id = ?", idOrPrefix)
	if doc, err := scanDocument(row); err == nil {
		return doc, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Document{}, err
	}

	// 前缀匹配。`%` 与 `_` 要转义，否则用户输入里带上它们会扩大匹配范围
	rows, err := d.db.QueryContext(ctx,
		"SELECT "+documentColumns+" FROM documents WHERE id LIKE ? ESCAPE '\\' || '%' ORDER BY created_at DESC LIMIT 10",
		searchterms.EscapeLike(idOrPrefix)+"%")
	if err != nil {
		return Document{}, fmt.Errorf("查文档失败：%w", err)
	}
	defer func() { _ = rows.Close() }()

	matches := make([]Document, 0, 2)
	for rows.Next() {
		doc, err := scanDocument(rows)
		if err != nil {
			return Document{}, err
		}
		matches = append(matches, doc)
	}
	if err := rows.Err(); err != nil {
		return Document{}, err
	}

	switch len(matches) {
	case 0:
		return Document{}, ErrNotFound
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, 0, len(matches))
		for _, doc := range matches {
			ids = append(ids, doc.ID)
		}
		return Document{}, fmt.Errorf("前缀 %s 匹配到 %d 份文档（%s），请给更完整的 id",
			idOrPrefix, len(matches), strings.Join(ids, "、"))
	}
}

// scanner 收窄到「扫一行」——database/sql 的 Scan 签名统一。
type scanner interface {
	Scan(dest ...any) error
}

func scanDocument(row scanner) (Document, error) {
	var (
		doc         Document
		rawError    sql.NullString
		rawAnnotate any
	)
	err := row.Scan(&doc.ID, &doc.Filename, &doc.Filepath, &doc.Size, &doc.OwnerWxid,
		&doc.Visibility, &doc.ContentHash, &doc.Status, &rawError, &doc.Attempts,
		&rawAnnotate, &doc.CreatedAt, &doc.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	if err != nil {
		return Document{}, fmt.Errorf("读文档行失败：%w", err)
	}

	if rawError.Valid {
		value := rawError.String
		doc.Error = &value
	}
	doc.Annotations = ParseAnnotations(rawAnnotate)
	return doc, nil
}
