// Package tools 是工具的**契约**：一个源就是一撮工具，注册表把它们合成一张给
// 模型看的表。
//
// ## 为什么要有「源」这一层
//
// 技能、MCP、以及将来的内置工具，三者的**加载方式完全不同**（读一个目录 / 连一个
// 进程 / 代码里写死），但对模型来说都只是「一批有名字的工具」。把这个差异关在
// ToolSource 里，循环与注册表就只认识工具，不认识它从哪来——加一个新来源时循环
// 一行不改。
//
// ## 只描述，不执行
//
// 这里只有形状与错误类型，没有实现。Effect 是**声明**，由调用方按 effect 决定
// 要不要放行（见 policy.go）。
package tools

import (
	"context"
	"fmt"

	"github.com/zjzhang-cn/fka-go/internal/llm"
)

// Effect 工具对这个世界做了什么。**每一类都能单独放行**，按后果从轻到重排。
//
//	| 效果      | 做什么                          | 错了的代价        |
//	|-----------|---------------------------------|-------------------|
//	| read      | 查资料（搜文档 / 列文档 / 取正文 / 查记忆） | 一条不准的答案     |
//	| memory    | 记一条记忆（只增，不改不删）          | 记错一句，/回忆里多一条 |
//	| send      | 以 Bot 的身份发给用户（文件、图片）      | 用户立刻看到，再发一次就好 |
//	| delete    | 删文档（文件 + 索引 + 数据库行）        | 删了就没了          |
//	| external  | 出网或拉起别的进程（MCP）              | 数据出境，无法回收    |
//
// ## 为什么不合成「写」一类
//
// 曾经只有 write 一个词，结果是：想让它能记一条记忆，就得同时允许删文档。
// **把差别很大的操作捆在一个开关上，实际效果是这个开关没人敢开**——最后等于什么都
// 没开。所以按「错了会怎样」拆开，而不是按「是不是修改」拆：
//
//   - memory 与 delete 的差别是**可撤销性**：记忆只增，删错就没了；
//   - send 与它们的差别是**可见性**：发出去的文件用户当场看得见。
//
// 名字都直接说「动的是什么」，不叫 write 那种上义词——上义词会让 read,write 看起来
// 像把删除也包进来了。
type Effect string

const (
	// EffectRead 查资料
	EffectRead Effect = "read"
	// EffectMemory 记一条记忆
	EffectMemory Effect = "memory"
	// EffectSend 以 Bot 的身份发文件 / 图片
	EffectSend Effect = "send"
	// EffectDelete 删文档
	EffectDelete Effect = "delete"
	// EffectExternal 出网或拉起别的进程（MCP）
	EffectExternal Effect = "external"
)

// Spec 给模型看的工具声明。Parameters 是 JSON Schema 对象。
type Spec struct {
	// Name **源内的**名字，如 search_documents。注册表会加上源前缀
	Name        string
	Description string
	// Parameters 保留成 map 而不是强类型结构：**工具的 schema 是数据**
	// （MCP 服务器各自定义），Go 侧不该逐个翻译——翻译一遍就多一处会漂移的地方。
	Parameters map[string]any
	Effect     Effect
}

// Result 一次工具调用的结果。
//
// OK == false **不是异常**，是要给模型看的一句话（「参数不合法」「没找到」）。
// 异常留给「源自己坏了」——那种情况返错，由循环决定是否降级。
//
// ⚠️ `OK` 在**生产路径上只被 mcp/source.go 写、被一条日志读**，循环不读它
// （`internal/agent` 的 `runToolCall` 只取 Content）。它**如实搬过 MCP 边界**，
// 因为 SDK 的 `IsError` 得有个落点，但「成不成功」对模型没有意义——模型只有一段文字
// 可读。刻意丢弃这件事由 `internal/agent` 的 `TestRun_成不成功都只有一条通道` 钉住。
type Result struct {
	OK      bool
	Content string
	// Images 结果里附带的图片（MCP 的 image 内容块）。
	//
	// **与 Content 并列，不是它的替代**：Content 是给模型看的文字，Images 是**图片
	// 字节**。循环把它们搬到一条额外的 user 消息上发给模型——因为 role=tool 的消息
	// 只能装字符串，装不下图片（见 internal/agent 的 runToolCall 与装配点）。
	// 空 = 这条结果没有图片。
	Images []llm.ImageAttachment
}

// OKResult 造一条成功结果。
func OKResult(content string) Result { return Result{OK: true, Content: content} }

// FailResult 造一条失败结果。**给模型看的一句话**，不是异常。
func FailResult(format string, args ...any) Result {
	return Result{OK: false, Content: fmt.Sprintf(format, args...)}
}

// Context 一次调用能看到的全部外界。
//
// 刻意**不含会话历史与用户问题**：工具要的是「以谁的身份查什么」，把对话塞进来会
// 让工具开始依赖上下文，那就不再是能被单独测试的东西了。
type Context struct {
	// PrincipalID **这次调用是谁发起的**。渠道决定它是什么——iLink 是微信 id，
	// 别的渠道可能是别的标识，所以刻意不叫 wxid、不叫 user。
	//
	// 这个 agent 自己不拥有数据，但**它选的 MCP server 可能有**。server 据此做
	// 权限过滤，所以**漏了就是数据泄漏**。
	//
	// ⚠️ **已接受的风险**：下游 server 认的身份参数是**模型填的工具参数**，
	// 不是这里由代码塞进去的。收紧的做法（进程级身份 / 签名参数）见 docs/permissions.md。
	PrincipalID string

	// Reply 以自己的身份回话。**逐条消息提供**，nil = 当前渠道回不了话
	Reply Reply

	// Extra 由组装根注入的窄端口。**刻意不给 ORM、不给文件路径、不给存储根**——
	// 这个 agent 不拥有任何数据，「数据存在哪」是 MCP server 的事，不是它的事。
	Extra map[string]any
}

// Reply 以 Bot 的身份回话的能力（目前只有发文件）。
//
// 由消息层逐条提供（从渠道的发送器里取），因为「能发给谁」取决于当前这条消息的
// 会话与回复令牌——工具拿不到、也不该猜到这些。
//
// **两个方法都收 ctx**：发文件要走网络（上传），而「谁在发」这一轮的取消信号在
// 工具的 ctx 上。实现里换成 `context.Background()` 的话，停机时这一发就断不掉
// （而且从签名上完全看不出来）。
type Reply interface {
	// File 把一个本地文件发给当前会话。FileName 缺省时用路径里的文件名
	File(ctx context.Context, path string, fileName string) error
	// Image 把一张图片发给当前会话。渠道发不了图片时**调用方退回 File**——
	// 发成文件比发不出去强
	Image(ctx context.Context, path string, fileName string) error
}

// Source 一撮工具。实现只需管自己那批，前缀与合并由注册表做。
type Source interface {
	// ID 前缀来源，会出现在工具名里（skills__load）。**只用小写字母与下划线**
	ID() string
	// Label 日志里怎么称呼它
	Label() string
	// List 这个源现在有哪些工具。**允许为空**（目录里没有技能时）——空不等于错
	//
	// 带一个 context 是因为列举可能要连进程（MCP）：不传 ctx 就只能
	// context.Background()，而那会让「服务器卡住」变成一次永久挂起。
	List(ctx context.Context, tc Context) ([]Spec, error)
	// Call 执行一次调用。args 已经过注册表的参数校验
	Call(ctx context.Context, name string, args map[string]any, tc Context) (Result, error)
	// PromptSection 要写进 system prompt 的额外说明（技能目录用它列出有哪些技能）。
	// 返回空串表示这段没有内容，循环就不加。
	PromptSection(ctx Context) (string, error)
	// Close 进程退出时收尾（MCP 要断连接、终止子进程）。**没有可关的就返回 nil**
	Close() error
}

// SourceFuncs 把一份函数拼成 Source。**测试与 CLI 的便利**——让假源不用写一整个
// 类型，也省掉 List/PromptSection/Close 这些多数源用不上的方法。
type SourceFuncs struct {
	SourceID    string
	SourceLabel string
	ListFunc    func(ctx context.Context, tc Context) ([]Spec, error)
	CallFunc    func(ctx context.Context, name string, args map[string]any, tc Context) (Result, error)
	PromptFunc  func(ctx Context) (string, error)
	CloseFunc   func() error
}

func (s SourceFuncs) ID() string    { return s.SourceID }
func (s SourceFuncs) Label() string { return s.SourceLabel }

func (s SourceFuncs) List(ctx context.Context, tc Context) ([]Spec, error) {
	if s.ListFunc == nil {
		return nil, nil
	}
	return s.ListFunc(ctx, tc)
}

func (s SourceFuncs) Call(ctx context.Context, name string, args map[string]any, tc Context) (Result, error) {
	if s.CallFunc == nil {
		return FailResult("工具源 %s 没有实现调用", s.SourceID), nil
	}
	return s.CallFunc(ctx, name, args, tc)
}

func (s SourceFuncs) PromptSection(ctx Context) (string, error) {
	if s.PromptFunc == nil {
		return "", nil
	}
	return s.PromptFunc(ctx)
}

func (s SourceFuncs) Close() error {
	if s.CloseFunc == nil {
		return nil
	}
	return s.CloseFunc()
}

// RegisteredTool 合并后的一件工具。FullName 才是模型看到的名字（带源前缀）。
type RegisteredTool struct {
	// FullName 模型看到的名字，如 mcp__sqlite__query_album
	FullName string
	SourceID string
	Label    string
	Spec     Spec
}

// Service 工具接缝。业务层只认它。
//
// Use 收下源，Sources 列已注册的源，Tools 列合并后的工具。一次问答开始时快照一次
// Tools，整轮用同一张表（见 registry.go）。
type Service interface {
	// Use 注册一个工具源
	Use(source Source)
	// Sources 已注册的源，按注册顺序
	Sources() []Source
	// Tools 合并、过滤放行后的工具表快照
	Tools(ctx context.Context, tc Context) ([]RegisteredTool, error)
	// ToToolDefs 给模型看的工具声明
	ToToolDefs(ctx context.Context, tc Context) ([]llm.ToolDef, error)
	// Call 执行一次调用。未放行 / 名字错 / 参数非法都转成 OK=false 的一句话
	Call(ctx context.Context, fullName string, args map[string]any, tc Context) Result
	// PromptSections 各源要额外写进 system prompt 的段落，按源顺序。段落原样返回，
	// 去空白与去空段由 prompts.Compose 统一做
	PromptSections(ctx Context) []string
	// Close 关闭所有源。由组装根在自己的清理路径里调
	Close() error
}
