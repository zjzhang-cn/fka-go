// 家庭记忆的 MCP server 工具实现。
//
// ## 为什么记忆要搬出主进程
//
// 决定：内部记忆与文档管理都用 MCP 实现。好处是主程序**不再直接碰
// memories 表**——存储整个沉到 server 里去，agent 只经 JSON-RPC 调工具。
//
// ## ⚠️ 权限边界：这里有一条**已接受的风险**
//
// viewer 由**模型填的工具参数**传进来，server 按约定**不做进程级绑定**。
// 详见 go/README.md 的「权限边界」一节，以及 tools.Context.ViewerWxid。
//
// 因此 FindMemories 里那句 `(visibility = ? OR owner_wxid = ?)` **是本 server
// 唯一的数据防线**。它必须留在 WHERE 里、每次查询都要带——漏一次就是别人的
// private 记忆进了别人的上下文。
//
// 收紧的做法（未做，改动很小）：主程序拉起本 server 时注入身份，
// server 忽略工具参数里的 viewer。
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/zjzhang-cn/fka-go/mcp/memory/internal/log"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/zjzhang-cn/fka-go/mcp/memory/internal/domain"
	"github.com/zjzhang-cn/fka-go/mcp/memory/internal/store"
)

// Tools 的名字。**刻意不带前缀**——前缀由上层的工具注册表加（`memory__`）。
const (
	ToolSearch   = "search_memories"
	ToolRemember = "remember_memory"
)

// server 记忆 server 的实现。**只有本目录内的 main 与测试用得到**。
type memoryServer struct {
	db *store.DB
}

// register 把工具挂到一个 MCP server 上。db 已打开且已迁移到最新。
func register(mcpServer *server.MCPServer, db *store.DB) {
	s := &memoryServer{db: db}

	mcpServer.AddTool(mcp.NewTool(ToolSearch,
		mcp.WithDescription(
			"查家庭记忆。记忆是家人记下的一句话事实（事件 / 待办 / 经验 / 常识）。"+
				"关键词之间是「且」。viewer_wxid 必须照实填调用方告诉你的那个值。"),
		mcp.WithString("query",
			mcp.Description("空格分隔的关键词。留空则列出最近的记忆。"),
		),
		mcp.WithString("viewer_wxid",
			mcp.Description("提问者标识。**必须照实填**，用于权限过滤。"),
			mcp.Required(),
		),
		mcp.WithNumber("limit",
			mcp.Description("最多返回几条。默认 10。"),
		),
	), s.handleSearch)

	mcpServer.AddTool(mcp.NewTool(ToolRemember,
		mcp.WithDescription(
			"记一条家庭记忆。只增，不改不删——写错了再记一条更正的就是。"),
		mcp.WithString("content",
			mcp.Description("记忆内容，就是用户说的那句话本身。"),
			mcp.Required(),
		),
		mcp.WithString("type",
			mcp.Description("四类之一：event（已发生的事）/ reminder（要关注的时间点）/ "+
				"experience（经验教训）/ knowledge（家庭常识，默认）。"),
		),
		mcp.WithString("visibility",
			mcp.Description("public（默认，家人能问到）或 private（只有记录者能问到）。"),
		),
		mcp.WithString("viewer_wxid",
			mcp.Description("记录者标识。**必须照实填**。"),
			mcp.Required(),
		),
	), s.handleRemember)
}

func (s *memoryServer) handleSearch(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	viewer := strings.TrimSpace(request.GetString("viewer_wxid", ""))
	if viewer == "" {
		// **不返异常**：返回一句给模型看的话，它下一轮会自己补上。
		// 抛异常会让模型只看到「工具出错」，不知道该改什么
		return fail("viewer_wxid 是必填的：不知道提问者就没法做权限过滤。" +
			"请照实填调用方告诉你的那个微信 ID。"), nil
	}

	// **声明是 number（`mcp.WithNumber`），就必须按数字读。**
	//
	// `GetString` 只在值是 Go `string` 时返回，而 JSON 数字解出来是 `float64`——
	// 于是它永远返回空串，limit 永远是默认的 10：「最多返回几条」这条参数整个失效，
	// 而界面上看不出任何异常（照样返回结果，只是条数不由你定）。
	// `GetInt` 认 float64 / int / 数字字符串三种。
	limit := request.GetInt("limit", 0)

	result, err := s.db.FindMemories(ctx, store.FindMemoriesInput{
		Query:      request.GetString("query", ""),
		ViewerWxid: viewer,
		Limit:      limit,
	})
	if err != nil {
		log.Log().Warn("记忆检索失败", log.Context{"error": err.Error()})
		return fail("记忆检索失败：" + err.Error()), nil
	}

	if len(result.Hits) == 0 {
		return text("没有找到相关记忆。"), nil
	}

	var out strings.Builder
	fmt.Fprintf(&out, "找到 %d 条记忆（共 %d 条符合条件）：\n", len(result.Hits), result.Total)
	for i, hit := range result.Hits {
		fmt.Fprintf(&out, "%d. [%s] %s\n", i+1, memoryTypeLabel(hit.Type), hit.Content)
	}
	return text(out.String()), nil
}

func (s *memoryServer) handleRemember(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	viewer := strings.TrimSpace(request.GetString("viewer_wxid", ""))
	if viewer == "" {
		return fail("viewer_wxid 是必填的：没有记录者就不知道这条记忆归谁。"), nil
	}

	content := strings.TrimSpace(request.GetString("content", ""))
	if content == "" {
		return fail("content 是必填的：记忆内容不能为空。"), nil
	}

	memoryType := strings.TrimSpace(request.GetString("type", ""))
	if memoryType == "" {
		memoryType = string(domain.MemKnowledge)
	}
	// 未知的类型**原样回显给模型**而不是映射到 knowledge——映射会让模型以为自己
	// 记下了一类东西，而 /回忆 里显示的是另一类
	if !domain.ValidMemoryType(memoryType) {
		return fail(fmt.Sprintf("type %q 不认识。四类只有：event、reminder、experience、knowledge。",
			memoryType)), nil
	}

	visibility := strings.TrimSpace(request.GetString("visibility", ""))
	if visibility == "" {
		// 记忆默认 public：家庭记忆本就该被家人问到（与文档相反——文档默认 private）
		visibility = string(domain.VisPublic)
	}
	if visibility != string(domain.VisPublic) && visibility != string(domain.VisPrivate) {
		return fail(fmt.Sprintf("visibility %q 不认识，只有 public 与 private。", visibility)), nil
	}

	if err := s.db.InsertMemory(ctx, store.Memory{
		ID:         newID(),
		Type:       memoryType,
		Content:    content,
		OwnerWxid:  viewer,
		Visibility: visibility,
		CreatedAt:  nowMs(),
	}); err != nil {
		log.Log().Warn("记忆写入失败", log.Context{"error": err.Error()})
		return fail("记忆写入失败：" + err.Error()), nil
	}

	return text(fmt.Sprintf("已记住（%s，%s）：%s", memoryType, visibilityLabel(visibility), content)), nil
}

// memoryTypeLabel 类型的展示名。**未知类型原样回显**，不落到兜底标签。
func memoryTypeLabel(memoryType string) string {
	switch domain.MemoryType(memoryType) {
	case domain.MemEvent:
		return "已发生的事"
	case domain.MemReminder:
		return "要关注的时间点"
	case domain.MemExperience:
		return "经验教训"
	case domain.MemKnowledge:
		return "家庭常识"
	}
	return memoryType
}

func visibilityLabel(visibility string) string {
	if visibility == string(domain.VisPrivate) {
		return "只有你自己能问到"
	}
	return "家人能问到"
}

// text 造一条成功结果。
func text(body string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{mcp.NewTextContent(body)}}
}

// fail 造一条**给模型看**的失败。isError=true 让模型知道这次没成功，
// 它下一轮能改；而抛异常只会让它看到「工具坏了」。
func fail(body string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{mcp.NewTextContent(body)}}
}

// IsUnavailable 判断一个连接错误是不是「没建库」。主程序据此提示。
func IsUnavailable(err error) bool {
	return errors.Is(err, sql.ErrConnDone) || strings.Contains(err.Error(), "no such table")
}
