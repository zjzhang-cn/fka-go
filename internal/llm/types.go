// Package llm 是语言模型的**契约**：业务层与「怎么跟模型服务说话」之间的那条线。
//
// 这里只有类型与 provider 形状，没有实现。OpenAI 兼容的实现（请求体、响应解析、
// /chat/completions）在 llm/openai；拼提示词与压缩历史这些与供应商无关的纯函数在
// history.go。
//
// ## 为什么 composer 与 chat 都要由 provider 产出
//
// 对外同时给两样东西：单发的 Compose（问题 + 片段 → 答案）与带工具的 Chat
// （消息列表进、消息列表出）。它们的**请求与响应形状是供应商相关的**（单发允许空
// 答案、带工具要求 content 或 tool_calls 二选一），所以两者都由 provider 造，
// 而不是共用一个「通用补全」再在上层拆。
package llm

import "context"

// Passage 片段的最小形状。**刻意不依赖 qa 包的类型**——这一层在它下面。
type Passage struct {
	Filename string
	// Text 正文片段。可能为空（关键词只命中了文件名）
	Text string
}

// Config 一条能用得上的配置。缺任何必填项时对应 provider 的 ReadConfig 返回 false。
type Config struct {
	// BaseURL 含版本段，如 https://api.openai.com/v1。调用时拼 /chat/completions
	BaseURL string
	APIKey  string
	Model   string

	// TimeoutMs 单次回复的**整体**上限：从发请求到读完响应的总时长，默认 120s。
	//
	// 与 StreamTimeoutMs 配合：流式接收时只要还有数据就不算断流，但整轮仍受这个总上限约束。
	TimeoutMs int

	// StreamTimeoutMs 流式接收的**断流**上限：多久没收到新数据算卡死，默认 5s。
	//
	// 每收到一块（含推理内容）就重置，所以模型长思考不会因为它被砍；只有真正断流才中止。
	StreamTimeoutMs int

	// ContextTokens 送进模型的上下文预算（估算 token）。**0 = 不设上限**。
	//
	// 各家上下文窗口差别很大、也探测不到，所以给一个保守的总预算：历史装不下时
	// 按 CompressHistory 从最老整条丢。
	ContextTokens int
}

// Role 一条消息的角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall 模型要求调用的一次工具。
//
// **Arguments 是原始 JSON 字符串，不是已解析的对象**——这一条是会话历史能逐字重放的
// 前提：反序列化再序列化会改变字节序（Go 按字段声明序，Node 按插入序），而 provider
// 的 KV 缓存认的是「从第一条起逐字不变的前缀」。
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatMessage 一次请求里的一条消息。比 HistoryMessage（历史）宽：assistant 可以
// 请求工具，还要能把工具结果回填成 RoleTool。
//
// **字段顺序即序列化顺序**，且刻意只用结构体、不用 map：Go 的 encoding/json 对 map
// 会按 key 排序、对结构体按声明序输出。用 map 会让同一段逻辑两次运行产生不同字节，
// 前缀缓存随之失效。
type ChatMessage struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
	// ToolCalls 在 RoleAssistant 时：这一轮要求调用的工具
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID 在 RoleTool 时：回答的是哪次调用
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// HistoryMessage 历史里的一条（只有文本与角色，没有工具）。
type HistoryMessage struct {
	Role Role
	Text string
}

// History 这一段历史是怎么选出来的。落进 transcript，解释「为什么带了这些」。
type History struct {
	Messages []HistoryMessage
	// Mode 轮次边界模式：quote / time / all。见 messages/history 的策略
	Mode string
	// GapMinutes 仅 time 模式有意义
	GapMinutes *int
	// ChatMessages 从会话文件读回的**完整消息**（含工具调用与工具结果），逐字原样。
	//
	// 有它时优先于 Messages：后者是从数据库重建的有损版本，而前者保留了模型当时
	// 看到的一切。两条路共用 LoadHistoryPrefix，哪个优先由那里的规则决定。
	ChatMessages []ChatMessage
}

// ChatResult 一次带工具的调用结果：要么说话，要么要工具，要么两者都有。
type ChatResult struct {
	Content   string
	ToolCalls []ToolCall
}

// ToolDef 给模型看的工具声明。Parameters 是 JSON Schema 对象。
type ToolDef struct {
	Name        string
	Description string
	// Parameters 保留成原始 JSON 而不是强类型结构：**工具的 schema 是数据，
	// 不是本项目的类型**。MCP 服务器各自定义自己的 schema，Go 侧不该逐个翻译。
	Parameters map[string]any
}

// ChatClient 一次带工具的调用。tools 为空时就是普通的聊天补全。
type ChatClient func(ctx context.Context, messages []ChatMessage, tools []ToolDef) (ChatResult, error)

// Composer 一个单发函数：问题 + 片段 → 一段答案。
type Composer func(ctx context.Context, question string, passages []Passage, callCtx *CallContext) (string, error)

// CallContext 一次调用属于哪段会话/哪一轮。
//
// 由**调用方**（消息层）提供——会话与轮次是消息层的知识，这一层不认识 messages 表。
// 缺席时按「一次调用一段新会话」记，日志仍写得出来。
type CallContext struct {
	// SessionID 会话标识：**对端**（私聊即对方在渠道里的 id）。
	//
	// 刻意**不用** messages.conversation_id——那个是引用推出来的「这一串接在哪句
	// 后面」，每开一段新引用就换一个。用它命名会让同一个人的日志散成许多文件。
	SessionID string
	// AccountID 渠道内的账号 id。**只用于会话日志的文件名**
	AccountID string
	// TurnID 这一轮接的是哪条消息
	TurnID string
	// History 该会话的历史。nil = 单发
	History *History
	// QuotedText 被引用那条的正文；**只在历史里找不到它时才传**
	QuotedText string
	// HistoryStore 会话历史存储。由**组装根**注入——
	//
	// 刻意不在 provider 里自己造：存储的目录与开关是部署事实（config 组装根知道），
	// 造在 provider 里会让「测试里不落盘」这种最常见的需求无法表达。nil = 不持久化。
	HistoryStore SessionHistoryStore
}

// CompressionResult 收拢历史到预算内的结果。
type CompressionResult struct {
	Messages []ChatMessage
	// Dropped 被整条丢掉的条数（只从最老的丢，留下的一条都不改写）
	Dropped int
	// EstimatedTokens 估算 token 数
	EstimatedTokens int
}

// SessionHistoryStore 会话历史的持久化：**每个 session 一个文件，原样存模型看到的完整消息**。
//
// Load 给出可逐字重放的前缀（KV 缓存的命中前提），Append 追加本轮新产生的消息。
//
// **两个方向都永不返错**——调用点在长驻的消息处理链里，返错只会逼每个调用点写一遍
// 「记日志然后忽略」。失败在这里就地记下并降级（读失败当空、追加失败丢弃）。
type SessionHistoryStore interface {
	Load(sessionID string, accountID string) []ChatMessage
	Append(sessionID string, accountID string, messages []ChatMessage)
}

// Probe 启动期探测结果。
type Probe struct {
	Available bool
	// Hint 不可用时的原因与下一步
	Hint string
	// LoadMs 加载/探测耗时，成功时才有
	LoadMs int64
}

// Provider 一种模型服务实现。Factory 形状：ReadConfig 说配没配，CreateChat/
// CreateComposer 造出两样对外能力。
//
// ReadConfig 返回 false 表示**本实现未配置**（缺 key/model，或没被 LLM_PROVIDER
// 选中），组装根据此不启用。
type Provider interface {
	// ID 种类标识，如 openai。LLM_PROVIDER 的取值
	ID() string
	// Label 人类可读名称，用于日志与错误提示
	Label() string
	// IsDefault 没显式选用时是否作为默认
	IsDefault() bool
	// ReadConfig 读本实现的配置。纯函数，便于测试与 doctor 复用
	ReadConfig() (Config, bool)
	// CreateChat 造一个带工具能力的调用函数
	CreateChat(cfg Config) ChatClient
	// CreateComposer 造单发 composer
	CreateComposer(cfg Config) Composer
	// Probe 启动探测（可选）。**永不返错**
	Probe(ctx context.Context, cfg Config) Probe
}
