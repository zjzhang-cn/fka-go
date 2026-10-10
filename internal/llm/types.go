// Package llm 是语言模型的**契约**：业务层与「怎么跟模型服务说话」之间的那条线。
//
// 这里只有类型与 provider 形状，没有实现。OpenAI 兼容的实现（请求体、响应解析、
// /chat/completions）在 llm/openai；拼提示词与压缩历史这些纯函数在 history.go。
//
// ## 「中立」这个词只对一半成立，务必读这一段
//
// ChatMessage / ToolCall 的 json tag **逐字就是 OpenAI /chat/completions 的消息
// 形状**，而且 `internal/llm/session.go` 直接把这个结构体序列化进
// `data/history/*.jsonl`。也就是说：**磁盘上的历史就是某一家协议的线格式**，
// 而它必须逐字重放（AGENTS.md：会话历史逐字重放，provider 的前缀缓存认「从第一条
// 起逐字不变的前缀」）。
//
// 所以本包的形状**不是**「所有 provider 的交集」，而是「OpenAI 兼容协议」。
// 加一个形状不同的 provider（内容块式的 /v1/messages 之类）时，正确做法是**在那个
// provider 包内翻译**，不要动这里的类型——动它就是改线格式，而线格式已经落盘。
//
// `internal/llm/boundary_test.go` 用 AST 扫 import 守住同一条：接缝不许伸手到实现。
package llm

import "context"

// MaxAnswerTokens 回答长度的**预算**：`internal/agent` 算上下文时给回答预留的位置。
//
// **它不再作为请求的 `max_tokens` 发出去**——那个额度会把推理一起限死，长推理会在
// 吐出正文前被截断（见 openai 的 baseRequest）。这里只用于「上下文里给回答留多少」
// 的估算。放在 `llm` 而不是 `llm/openai`，是因为那一层不认识任何 provider。
const MaxAnswerTokens = 800

// Config 一条能用得上的配置。缺任何必填项时对应 provider 的 ReadConfig 返回 false。
type Config struct {
	// BaseURL 含版本段，如 https://api.openai.com/v1。
	//
	// 调用时在它后面拼端点。**端点名字属于实现**（本仓库的实现拼 /chat/completions），
	// 所以这里不要写死路径。
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

	// StreamRetries 断流后再试几次，默认 3（共最多 4 次尝试）。**0 = 不重试**。
	//
	// 「断流」指流中途被掐断：断流超时命中，或连接在读数途中出错。**整体超时**
	// （TimeoutMs）与调用方主动取消（Ctrl-C）不重试——前者预算已尽，后者是用户要走。
	StreamRetries int

	// ContextTokens 送进模型的上下文预算（估算 token）。**0 = 不设上限**。
	//
	// 各家上下文窗口差别很大、也探测不到，所以给一个保守的总预算：历史装不下时
	// 按 CompressHistory 从最老整条丢。
	ContextTokens int

	// ExtraBody 请求体**外层**额外并入的字段（`map` 的键即 JSON 的键）。
	//
	// ## 为什么它在契约上，而不是写死在某个 provider 里
	//
	// 有些厂商在 `/chat/completions` 上加了扩展字段（例如推理开关
	// `enable_thinking`），而它们**不是 OpenAI 规范的一部分**。写死在实现里意味着
	// 「所有 OpenAI 兼容端点都被塞上这一家的开关」；而 `ReadConfig` 是纯函数、
	// 只读环境变量，所以这个口子必须由配置带进来。
	//
	// **nil / 空 map = 什么都不并**。默认值（不是 nil）由各 provider 的 ReadConfig
	// 决定——那是它的产品决定，见 `openai.ReadConfig` 的说明。
	ExtraBody map[string]any
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
// ## 字段顺序即序列化顺序，且它就是落盘格式
//
// 刻意只用结构体、不用 map：Go 的 encoding/json 对 map 会按 key 排序、对结构体按
// declaration 序输出。用 map 会让同一段逻辑两次运行产生不同字节，前缀缓存随之失效。
//
// 这些 json tag 同时是**磁盘上 `data/history/*.jsonl` 的格式**（见 session.go），而
// 那个文件要逐字重放。**改 tag = 改产品。**
type ChatMessage struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
	// ToolCalls 在 RoleAssistant 时：这一轮要求调用的工具
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID 在 RoleTool 时：回答的是哪次调用
	ToolCallID string `json:"tool_call_id,omitempty"`

	// ImageAttachments 本轮随这条 user 消息发给模型的图片。
	//
	// **json:"-"，刻意不落盘**：图片是 base64 的 data URI，写进
	// `data/history/*.jsonl` 会把历史撑爆，而且每轮都要重放一遍。它只活在**当前
	// 这一轮请求**里——「历史上有的逐字重放」这条不变量管的是盘上那几个字段，
	// 而这里从不出现在盘上。代价是下一轮追问「那张图」时图已经不在上下文里。
	ImageAttachments []ImageAttachment `json:"-"`

	// Transient 这条消息**只为当前这一轮请求存在**，不写进会话历史。
	//
	// 目前只有一处用它：工具结果里的图片装不进 role=tool 的消息，被搬到一条额外的
	// user 消息上（见 internal/agent）。那条消息的正文只是「附了图」的说明，落盘后
	// 会留下一句指向「已经不存在的图」的话——与图片本身一样，它不该进历史。
	// 持久化时整条丢弃（internal/agent 的 persistable）。
	Transient bool `json:"-"`
}

// ImageAttachment 一张随 user 消息发送的图片。
type ImageAttachment struct {
	// Name 给人看的名字（提示与日志用），不进请求体
	Name string
	// DataURI 形如 data:image/png;base64,...；也接受 http(s) 链接
	DataURI string
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

// CompressionResult 收拢历史到预算内的结果。
type CompressionResult struct {
	Messages []ChatMessage
	// Dropped 被整条丢掉的条数（只从最老的丢，留下的一条都不改写）
	Dropped int
}

// SessionHistoryStore 会话历史的持久化：**每个 session 一个文件，原样存模型看到的完整消息**。
//
// Load 给出可逐字重放的前缀（KV 缓存的命中前提），Append 追加本轮新产生的消息。
//
// **两个方向都永不返错**——调用点在长驻的消息处理链里，返错只会逼每个调用点写一遍
// 「记日志然后忽略」。失败在这里就地记下并降级（读失败当空、追加失败丢弃）。
type SessionHistoryStore interface {
	Load(sessionID string) []ChatMessage
	Append(sessionID string, messages []ChatMessage)
}

// Provider 一种模型服务实现。
//
// ## 三个方法，每一个都有活着的调用方
//
// 这条契约以前是七个：`IsDefault` / `Probe` / `Label` / `CreateComposer` 全部零调用，
// 而 `CreateComposer` 造出的值连存放处都没人读。留着它们的意思是「每个新 provider 都得
// 实现一遍没人调的方法」——那不是可替换，是可妨碍。
//
// 想加一个 provider：在 llm/<名字>/ 写一份实现，**在 `internal/app` 的默认列表里加一行**。
// 别的包一个字都不用动（`internal/llm/boundary_test.go` 会守住这条）。
type Provider interface {
	// ID 种类标识，如 openai。装配根用它写「接上的是哪一个」那条日志
	ID() string
	// ReadConfig 读本实现的配置。纯函数，便于测试与 doctor 复用。
	// 返回 false 表示**本实现未配置**（缺 key 或 model），装配根据此换一个或整块缺席
	ReadConfig() (Config, bool)
	// CreateChat 造一个带工具能力的调用函数
	CreateChat(cfg Config) ChatClient
}
