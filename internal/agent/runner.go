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
// 身份之外的东西（存储根、管理员）也由工厂注入 ToolContext：**它们是部署事实，
// 不是每条消息的事实**。
type RunnerInput struct {
	// SessionID 会话标识
	SessionID string
	// AccountID 渠道内的账号 id。**只用于会话日志的文件名**
	AccountID string
	// ViewerWxid 提问者。**MCP 服务器据此过滤**
	//
	// ⚠️ 已接受的风险，见 tools.Context.ViewerWxid 的说明：文档与记忆走 MCP 之后，
	// 这个值经由工具参数到达 server，模型可以改写它。
	ViewerWxid string
	// TurnID 这一轮接的是哪条消息
	TurnID string
	// History 该会话的历史
	History *llm.History
	// Question 本轮问题
	Question string
	// QuotedText 被引用那条的正文
	QuotedText string
	// Reply 以 Bot 的身份回话（发文件）。**逐条消息给**——「能发给谁」取决这条
	// 消息的会话与回复令牌，那是消息层的事实。nil = 这个渠道/这条消息发不了文件。
	Reply tools.Reply
}

// Runner 跑一轮问答。**现在有没有任何工具由 HasTools 回答**：消息层据此决定走
// 单次问答还是工具循环。
type Runner struct {
	// MaxSteps 覆盖配置。0 = 用 DefaultMaxSteps
	MaxSteps int
	// ContextTokens 上下文预算
	ContextTokens int
	// StorageRoot 存储根，进 ToolContext
	StorageRoot string
	// AdminWxid 管理员微信 ID，可改任何人的文档
	AdminWxid string
	// SystemPrompt 覆盖系统提示。空 = 取 AGENT 那条
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

// NewRunner 造 runner。**chat 为 nil 或工具循环被关时返回 nil**——调用方据此走
// 单次路径，不必再判一个布尔。
func NewRunner(
	chat llm.ChatClient,
	registry tools.Service,
	opts RunnerOptions,
) *Runner {
	config := ReadConfig()
	if !config.Enabled || chat == nil {
		return nil
	}

	maxSteps := config.MaxSteps
	if opts.MaxSteps > 0 {
		maxSteps = opts.MaxSteps
	}

	return &Runner{
		MaxSteps:        maxSteps,
		ContextTokens:   opts.ContextTokens,
		StorageRoot:     opts.StorageRoot,
		AdminWxid:       opts.AdminWxid,
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
	StorageRoot   string
	AdminWxid     string
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

// ErrNoRunner runner 为 nil 时 Run 返它。调用方应该先判 HasTools。
var ErrNoRunner = errors.New("工具循环未启用")

// HasTools 现在有没有任何工具。**空表 = 没有工具**。
//
// 同步、只读注册表快照，**不触发模型调用**。但要注意它会走一次源的 List——
// MCP 源在那里惰性连进程，所以这个调用可能有网络/进程开销。
func (r *Runner) HasTools(ctx context.Context) (bool, error) {
	if r == nil {
		return false, nil
	}
	listed, err := r.tools.Tools(ctx, tools.Context{})
	if err != nil {
		return false, err
	}
	return len(listed) > 0, nil
}

// Run 跑一轮问答。runner 为 nil 时返错——调用方应该先判 HasTools。
func (r *Runner) Run(ctx context.Context, input RunnerInput) (RunResult, error) {
	if r == nil {
		return RunResult{}, ErrNoRunner
	}
	return Run(ctx, RunInput{
		SessionID:     input.SessionID,
		AccountID:     input.AccountID,
		TurnID:        input.TurnID,
		History:       input.History,
		Question:      input.Question,
		QuotedText:    input.QuotedText,
		ContextTokens: r.ContextTokens,
		ToolContext: tools.Context{
			ViewerWxid:  input.ViewerWxid,
			StorageRoot: r.StorageRoot,
			AdminWxid:   r.AdminWxid,
			Reply:       input.Reply,
		},
	}, Deps{
		Chat:            r.chat,
		Tools:           r.tools,
		MaxSteps:        r.MaxSteps,
		SystemPrompt:    r.SystemPrompt,
		SessionHistory:  r.sessionHistory,
		Model:           r.Model,
		Host:            r.Host,
		TimeoutMs:       r.TimeoutMs,
		StreamTimeoutMs: r.StreamTimeoutMs,
	})
}
