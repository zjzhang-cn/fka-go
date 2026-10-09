// 本文件是**工具循环这一块的类型与构造**：跑一轮需要什么（`Runner`）、怎么装
// （`NewRunner` / `RunnerOptions`）、现在有没有工具可问（`HasTools`）。
//
// ## 为什么「跑一轮」在 loop.go 而不在这里
//
// 循环的输入是 `RunnerInput`（**谁问、问什么**——那是消息层的事实），依赖与部署事实
// （工具表、模型、会话历史、上下文预算）都在 `Runner` 上。**两个来源各管一半，
// 互不重叠**，所以 `Run` 是 `(*Runner).Run`，而不是一个把 `Runner` 的字段再抄一遍
// 的自由函数。
//
// ## 「没有 agent」是一个实现，不再是 nil
//
// LLM_TOOLS=off 或没配模型时 `NewRunner` 返回的是一个**真的** `*Runner`：它的
// `enabled` 为 false，于是 `HasTools` 回 false、`Run` 回 `ErrNoRunner`。
//
// 以前这里返回 nil。改成实现之后「缺席」从**指针的缺省**变成**一个带答案的值**：
// 「没有 agent」与「为什么没有」两件事都由这个对象说。nil 说不出原因，所以
// `cmd/fka` 那时只能自己再判一次，才知道该印「LLM_TOOLS=off」——那第二个布尔正是
// 上一版注释里说「不另设」的东西，只是它没能真的不要。
//
// 代价是两个方法里各留一行 `r == nil` 判。**那不是哨兵**（构造函数永不返回 nil），
// 而是防 `(*Runner)(nil)` 被装进 `messages.Runner` 接口后调用炸掉——接口引入的
// 这个陷阱得有人接。钉它的是 `TestNewRunner_关掉时给出的是一个能自答的实现`。
//
// （包头在 loop.go：那是唯一一份 `Package agent` 文档。这里刻意空一行，
// 免得 godoc 把两段都当成包文档。）

package agent

import (
	"context"
	"errors"

	"github.com/zjzhang-cn/fka-go/internal/llm"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// RunnerInput 一次问答要绑给工具循环的东西。
//
// **由调用方给身份与历史**（那是消息层的知识），**由工厂给配置与工具**（那是启动期
// 的知识）。消息层因此不认识 LLM_*，组装根也不认识 messages 表——与单发路径的
// compose 是同一种分工。
//
// 身份之外的东西（存储根、管理员）也由工厂注入 tools.Context：**它们是部署事实，
// 不是每条消息的事实**。
type RunnerInput struct {
	// SessionID 会话标识
	SessionID string
	// AccountID 渠道内的账号 id。**只用于会话日志的文件名**
	AccountID string
	// PrincipalID 提问者。**MCP 服务器据此过滤**
	//
	// ⚠️ 已接受的风险，见 tools.Context.PrincipalID 的说明：文档与记忆走 MCP 之后，
	// 这个值经由工具参数到达 server，模型可以改写它。
	PrincipalID string
	// TurnID 这一轮接的是哪条消息
	TurnID string
	// Question 本轮问题
	Question string
	// QuotedText 被引用那条的正文
	QuotedText string
	// Images 本轮问题附带的图片（CLI 的 `@图片` 引用）。**只在这一轮发给模型**，
	// 不落进会话历史——见 llm.ChatMessage.ImageAttachments。
	Images []llm.ImageAttachment
	// Emitter 过程事件（推理 / 工具调用 / 答案）的接收者，供各前端「回显」。
	// **nil = 不回显**。chat/ask 画给人看，渠道记日志并只把答复发回去。
	Emitter Emitter
	// Reply 以 Bot 的身份回话（发文件）。**逐条消息给**——「能发给谁」取决这条
	// 消息的会话与回复令牌，那是消息层的事实。工具层不该知道。
	Reply tools.Reply
}

// Runner 跑一轮问答。**现在有没有任何工具由 HasTools 回答**：消息层据此决定走
// 单次问答还是工具循环。
//
// 导出字段是**启动期的部署事实**（模型名、超时、上下文预算），私有的三个是依赖。
type Runner struct {
	// MaxSteps 覆盖配置。0 = 用 DefaultMaxSteps
	MaxSteps int
	// ContextTokens 上下文预算
	ContextTokens int
	// SystemPrompt 覆盖系统提示。空 = 取工具循环那条
	SystemPrompt string
	// Model / Host / TimeoutMs / StreamTimeoutMs **仅用于日志与 transcript**
	//（Host 也只进错误信息，绝不含 key）
	Model           string
	Host            string
	TimeoutMs       int
	StreamTimeoutMs int

	chat           llm.ChatClient
	tools          tools.Service
	sessionHistory llm.SessionHistoryStore
	// enabled LLM_TOOLS=off 时整块缺席——那时没有 agent，直接走单次问答
	enabled bool
}

// NewRunner 造 runner。**永远返回非 nil**：工具循环被关或 chat 为 nil 时返回一个
// 「能自答缺席」的零值 runner（见包头）。
func NewRunner(
	chat llm.ChatClient,
	registry tools.Service,
	opts RunnerOptions,
) *Runner {
	config := ReadConfig()
	if !config.Enabled || chat == nil {
		return &Runner{}
	}

	maxSteps := config.MaxSteps
	if opts.MaxSteps > 0 {
		maxSteps = opts.MaxSteps
	}

	return &Runner{
		MaxSteps:        maxSteps,
		ContextTokens:   opts.ContextTokens,
		SystemPrompt:    opts.SystemPrompt,
		Model:           opts.Model,
		Host:            opts.Host,
		TimeoutMs:       opts.TimeoutMs,
		StreamTimeoutMs: opts.StreamTimeoutMs,
		chat:            chat,
		tools:           registry,
		sessionHistory:  opts.SessionHistory,
		enabled:         true,
	}
}

// RunnerOptions 启动期的配置与依赖。
type RunnerOptions struct {
	MaxSteps      int
	ContextTokens int
	SystemPrompt  string
	// Model / Host / TimeoutMs / StreamTimeoutMs **仅用于日志与 transcript**
	//（Host 也只进错误信息，绝不含 key）
	Model           string
	Host            string
	TimeoutMs       int
	StreamTimeoutMs int
	// SessionHistory 会话历史存储。nil = 不持久化
	SessionHistory llm.SessionHistoryStore
}

// ErrNoRunner 工具循环缺席（LLM_TOOLS=off，或压根没接上模型）时 Run 返它。
//
// 它**同时是原因**：缺席的 runner 仍然是一个实现了这个错误的对象，所以调用点能用
// `errors.Is` 判它，而不必自己再去读一遍配置。
var ErrNoRunner = errors.New("工具循环未启用")

// Enabled 这个 runner 是不是真的接上了。**它现在是「缺席」的唯一编码**——
// 以前那个 nil 只能表示「没有」，说不出「为什么没有」。
func (r *Runner) Enabled() bool { return r != nil && r.enabled }

// HasTools 现在有没有任何工具。**空表 = 没有工具**。
//
// 同步、只读注册表快照，**不触发模型调用**。但要注意它会走一次源的 List——
// MCP 源在那里惰性连进程，所以这个调用可能有网络/进程开销。
func (r *Runner) HasTools(ctx context.Context) (bool, error) {
	if !r.Enabled() {
		return false, nil
	}
	listed, err := r.tools.Tools(ctx, tools.Context{})
	if err != nil {
		return false, err
	}
	return len(listed) > 0, nil
}
