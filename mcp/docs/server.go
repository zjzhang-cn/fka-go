package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/zjzhang-cn/fka-go/mcp/internal/log"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/zjzhang-cn/fka-go/mcp/internal/nas"
	"github.com/zjzhang-cn/fka-go/mcp/internal/store"
)

// 工具名。**刻意不带前缀**——前缀由上层的工具注册表加（`docs__`）。
const (
	ToolSearch = "search_documents"
	ToolList   = "list_documents"
	ToolGet    = "get_document"
)

const (
	// defaultSearchLimit 一次最多返回几份。给模型看的清单，不是给翻页用的
	defaultSearchLimit = 5
	// maxResultLimit 工具允许的返回上限。**模型说 100 也不给**——那会挤掉后续
	// 轮次的预算
	maxResultLimit = 20
	// maxDocumentChars get_document 返回的正文上限。超了截断并说明——
	// 全文该走 `send_document` 取文件
	maxDocumentChars = 4000
)

// impl server 的实现。
type impl struct {
	db          *store.DB
	storageRoot string
}

// register 把只读工具挂到一个 MCP server 上。
//
// ## 为什么只读这三件
//
// 它们是「用家里的资料回答问题」的最小闭环：不知道有什么就先 `list`，知道大概讲
// 什么就 `search`，拿到 id 就 `get` 看全文。
//
// `send_document` 要渠道的发送器、`delete_document` 要连向量索引一起删——**都做不了**
// ，而**声明一个跑不了的工具比不声明更糟**：模型会调它、收到失败、再换个方式试，
// 白烧一轮。所以这里只声明真正能用的。
//
// ## 结果一律是 JSON 文本
//
// 不是中文列表：**模型读结构化数据比读排版过的文字更稳**，也不会因为「第 3 条排在
// 哪一行」产生歧义。空结果也返回合法 JSON（`found: 0`），让模型能区分「没找到」
// 与「工具坏了」——后者是 `isError`。
func register(mcpServer *server.MCPServer, db *store.DB, storageRoot string) {
	s := &impl{db: db, storageRoot: storageRoot}

	mcpServer.AddTool(mcp.NewTool(ToolSearch,
		mcp.WithDescription(
			"在家庭知识库里搜文档，返回文件名、id 和命中片段。用户问「有没有关于 X 的资料」时用它。"+
				"多个词之间是「且」。viewer_wxid 必须照实填调用方告诉你的那个值。"),
		mcp.WithString("query",
			mcp.Description("检索词。多个词用空格分开，词之间是「且」的关系。"),
			mcp.Required(),
		),
		mcp.WithString("mode",
			mcp.Description("keyword 按字面匹配。vector（按意思相近）还没建好，"+
				"填了会明确告诉你不可用，不会静默退化成 keyword。"),
		),
		mcp.WithNumber("limit",
			mcp.Description("返回几份（默认 5，上限 20）。"),
		),
		mcp.WithString("viewer_wxid",
			mcp.Description("提问者标识。**必须照实填**，用于权限过滤。"),
			mcp.Required(),
		),
	), s.handleSearch)

	mcpServer.AddTool(mcp.NewTool(ToolList,
		mcp.WithDescription(
			"按归档时间倒序列出最近的文档，看「库里都有些什么」。不知道搜什么词时用它。"),
		mcp.WithNumber("limit", mcp.Description("返回几份（默认 10，上限 20）。")),
		mcp.WithString("viewer_wxid",
			mcp.Description("提问者标识。**必须照实填**，用于权限过滤。"),
			mcp.Required(),
		),
	), s.handleList)

	mcpServer.AddTool(mcp.NewTool(ToolGet,
		mcp.WithDescription(
			"按 id 取一份文档的正文。id 可以只写前面几位（会做前缀匹配，不唯一时会给出候选）。"),
		mcp.WithString("id", mcp.Description("文档 id 或它的前几位。"), mcp.Required()),
		mcp.WithString("viewer_wxid",
			mcp.Description("提问者标识。**必须照实填**，用于权限过滤。"),
			mcp.Required(),
		),
	), s.handleGet)
}

func (s *impl) handleSearch(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	viewer, blocked := requireViewer(request)
	if blocked != nil {
		return blocked, nil
	}

	query := strings.TrimSpace(request.GetString("query", ""))
	if query == "" {
		return fail("检索词不能为空。"), nil
	}

	mode := strings.TrimSpace(request.GetString("mode", ""))
	if mode == "" {
		mode = "keyword"
	}
	if mode != "keyword" {
		// **如实说不可用**，不要静默退化成 keyword——用户（与模型）会以为语义搜过了
		return fail(fmt.Sprintf("mode=%s 还没有建好（向量索引与云端嵌入尚未接上），"+
			"可以改用 mode=keyword。", mode)), nil
	}

	result, err := Search(ctx, s.db.ListDocuments, SearchOptions{
		StorageRoot: s.storageRoot,
		Query:       query,
		ViewerWxid:  viewer,
		Limit:       readLimit(request, "limit", defaultSearchLimit),
	})
	if err != nil {
		log.Log().Warn("文档检索失败", log.Context{"error": err.Error()})
		return fail("文档检索失败：" + err.Error()), nil
	}

	// 与 Node 版同形：给模型一个「这份有多少条、命中在哪」的清单
	summary := map[string]any{
		"query":     query,
		"mode":      "keyword",
		"found":     len(result.Hits),
		"total":     result.Total,
		"truncated": result.Truncated,
		"terms":     result.Terms,
		"documents": result.Hits,
	}
	return jsonResult(summary), nil
}

func (s *impl) handleList(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	viewer, blocked := requireViewer(request)
	if blocked != nil {
		return blocked, nil
	}

	result, err := s.db.ListDocuments(ctx, store.ListDocumentsInput{
		ViewerWxid: viewer,
		Limit:      readLimit(request, "limit", 10),
	})
	if err != nil {
		log.Log().Warn("列文档失败", log.Context{"error": err.Error()})
		return fail("列文档失败：" + err.Error()), nil
	}

	type row struct {
		ID         string `json:"id"`
		Filename   string `json:"filename"`
		Status     string `json:"status"`
		Visibility string `json:"visibility"`
		Bytes      *int64 `json:"bytes"`
		CreatedAt  string `json:"createdAt"`
	}
	rows := make([]row, 0, len(result.Rows))
	for _, doc := range result.Rows {
		rows = append(rows, row{
			ID: doc.ID, Filename: doc.Filename, Status: doc.Status,
			Visibility: doc.Visibility, Bytes: doc.Size,
			CreatedAt: time.UnixMilli(doc.CreatedAt).UTC().Format(time.RFC3339),
		})
	}

	return jsonResult(map[string]any{
		"total": result.Total, "truncated": result.Truncated, "documents": rows,
	}), nil
}

func (s *impl) handleGet(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	viewer, blocked := requireViewer(request)
	if blocked != nil {
		return blocked, nil
	}

	id := strings.TrimSpace(request.GetString("id", ""))
	if id == "" {
		return fail("要给一个 id。"), nil
	}

	doc, err := s.db.FindDocument(ctx, id)
	if err != nil {
		// 前缀歧义：FindDocument 已经在错误里列出了候选与各自的短 id
		if strings.Contains(err.Error(), "请给更完整的 id") {
			return fail(err.Error()), nil
		}
		if strings.Contains(err.Error(), "没有找到") {
			return fail(fmt.Sprintf("没有找到 id 以 %s 开头的文档。可以用 search_documents 找找。", id)), nil
		}
		log.Log().Warn("取文档失败", log.Context{"id": id, "error": err.Error()})
		return fail("取文档失败：" + err.Error()), nil
	}

	// 按 id 精确命中之后才判断能不能看。**private 只对属主可见**
	//
	// ⚠️ viewer 由模型填，server 按决定不做进程级绑定。**这一句是本 server 唯一的
	// 数据防线**。收紧的做法见 docs/permissions.md。
	if doc.Visibility != "public" && doc.OwnerWxid != viewer {
		return fail("这份文档是别人私有（private）的，不能给你看。"), nil
	}

	// 正文从磁盘读（数据库不存正文）
	content := nas.ReadExtracted(s.storageRoot, nas.Location{
		ID: doc.ID, OwnerWxid: doc.OwnerWxid, Filename: doc.Filename, Filepath: doc.Filepath,
	})
	truncated := false
	if runes := []rune(content); len(runes) > maxDocumentChars {
		content = string(runes[:maxDocumentChars]) + "…（已截断）"
		truncated = true
	}

	return jsonResult(map[string]any{
		"id": doc.ID, "filename": doc.Filename, "filepath": doc.Filepath,
		"status": doc.Status, "visibility": doc.Visibility,
		"createdAt":        time.UnixMilli(doc.CreatedAt).UTC().Format(time.RFC3339),
		"content":          content,
		"contentTruncated": truncated,
		"note": func() string {
			if truncated {
				return "正文太长已截断。要完整内容请用 send_document 把文件发给用户。"
			}
			if content == "" {
				return "这份文档在盘上没有解析结果（可能是图片或扫描件，还没提文字）。"
			}
			return ""
		}(),
	}), nil
}

// requireViewer 取提问者。**没有它就没法做权限过滤**——所以这里返回一句话让模型补上，
// 而不是当作匿名放行。
//
// 第二个返回值非 nil 表示「这句不能执行」，内容就是要喂回模型的话。
func requireViewer(request mcp.CallToolRequest) (string, *mcp.CallToolResult) {
	viewer := strings.TrimSpace(request.GetString("viewer_wxid", ""))
	if viewer == "" {
		return "", fail("viewer_wxid 是必填的：不知道提问者就没法做权限过滤。" +
			"请照实填调用方告诉你的那个微信 ID。")
	}
	return viewer, nil
}

// readLimit 读 limit 参数。**模型说 100 也不给**——那会挤掉后续轮次的预算。
func readLimit(request mcp.CallToolRequest, key string, fallback int) int {
	raw := request.GetString(key, "")
	if raw == "" {
		return fallback
	}
	value := 0
	for _, r := range raw {
		if r < '0' || r > '9' {
			return fallback
		}
		value = value*10 + int(r-'0')
		if value > maxResultLimit {
			return maxResultLimit
		}
	}
	if value <= 0 {
		return fallback
	}
	return value
}

func jsonResult(payload map[string]any) *mcp.CallToolResult {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fail("结果序列化失败：" + err.Error())
	}
	return &mcp.CallToolResult{Content: []mcp.Content{mcp.NewTextContent(string(encoded))}}
}

// fail 造一条**给模型看**的失败。isError 让模型知道这次没成功、它下一轮能改；
// 抛异常只会让它看到「工具坏了」。
func fail(body string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{mcp.NewTextContent(body)}}
}
