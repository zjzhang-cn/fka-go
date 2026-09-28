// Package agent 是工具调用循环：模型说「我要查 X」→ 执行 → 把结果给它 → 直到它给出回答。
//
// ## 为什么要有循环，而不是把资料一次性塞进去
//
// 单发那条路是「先本地检索、再让模型写」。它对「用哪个词搜」这个问题没有发言权
// ——词是用户打的。模型自己搜的时候可以**先看一眼有什么、再决定搜什么**：第一次搜
// 「学费」没结果，它会换成「缴费」再试。这是把「找资料」的判断权交还给更擅长它的那
// 一方。
//
// ## 上限是硬约束，不是配置装饰
//
// 每一轮工具调用都要一次真实请求。没有上限时，一个「帮我整理家里所有资料」式的
// 问题能把额度烧完。所以 maxSteps 到了就**不再给工具**，只让它基于已有的结果写答案
// ——用户拿到的是「不完美但有用」的回答，而不是又一句「稍后再试」。
//
// ## 模型/接口的错往上抛，工具的错变成一句话
//
// 模型接口失败、响应形状不对：这一层**返错**，由调用方决定降级。工具坏了（源返错、
// 参数不合法）则不然——那是「这一轮没查到」，转成给模型的一句话，它下一轮能换个
// 方式再试。
//
// 与 llm/openai 的分工：那边管「怎么跟接口说话」，这边管「说几轮、每轮做什么」。
package agent

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/llm"
	"github.com/zjzhang-cn/fka-go/internal/llm/openai"
	"github.com/zjzhang-cn/fka-go/internal/prompts"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

const (
	// DefaultMaxSteps 默认最多几轮。5 轮够「搜 → 不对 → 换词再搜 → 取全文 → 回答」
	// 这条最常见的路径。
	DefaultMaxSteps = 5
	// MaxMaxSteps 上限。再高就不是「多查两次」，而是拿额度赌一个大概率答不出来的
	// 问题。
	MaxMaxSteps = 999
)

// MaxToolResultChars 单条工具结果进模型的字符上限。
//
// ⚠️ **当前不生效**：2026-09-28 那次「移除工具结果字符限制」把截断去掉了，因为
// 截断会把 `get_document` 取回的正文砍掉一半，模型据此答错。改成靠工具自己返回
// 摘要。保留这个常量是为了让「要不要重新加截断」有据可查，而不是靠记忆。
const MaxToolResultChars = 1200

// MaxStepsAnswer 工具用完仍没收拢时的兜底话术。**如实说**，不假装是答案。
const MaxStepsAnswer = "（查到的资料有点多，我没能收拢成一个答案。你可以把问题问得再具体一点，或者补一个限定词。）"

// Config 工具循环的开关与上限。
type Config struct {
	// Enabled 关了就退回单发那条路（LLM_TOOLS=off）
	Enabled  bool
	MaxSteps int
}

// ReadConfig 读配置。**默认开**：工具是问答的主要方式，关掉是个显式的选择。
//
// LLM_TOOLS=off|0|false|no 关；LLM_MAX_STEPS 超范围时夹到 MaxMaxSteps 而不是
// 返错——启动期的一个配置笔误不该让服务起不来。
func ReadConfig() Config {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("LLM_TOOLS")))
	enabled := true
	if raw != "" {
		switch raw {
		case "0", "false", "off", "no":
			enabled = false
		}
	}

	steps, err := strconv.Atoi(strings.TrimSpace(os.Getenv("LLM_MAX_STEPS")))
	maxSteps := DefaultMaxSteps
	if err == nil && steps > 0 {
		maxSteps = min(steps, MaxMaxSteps)
	}

	return Config{Enabled: enabled, MaxSteps: maxSteps}
}

// RunInput 跑一轮的输入。
type RunInput struct {
	// SessionID 会话标识
	SessionID string
	// AccountID 渠道内的账号 id。**只用于会话日志的文件名**
	AccountID string
	// TurnID 这一轮接的是哪条消息
	TurnID string
	// History 该会话的历史。与单发路径共用同一份选法
	History *llm.History
	// Question 本轮问题
	Question string
	// QuotedText 被引用那条的正文；**只在历史里找不到它时才传**
	QuotedText string
	// ContextTokens 上下文预算。0 = 不压缩
	ContextTokens int
	// ToolContext 这次调用能看到的全部外界
	ToolContext tools.Context
}

// RunResult 跑一轮的结果。
type RunResult struct {
	Text string
	// UsedTools 这轮实际调过的工具名（含重复），供日志与测试断言
	UsedTools []string
	// Steps 用了几次模型调用
	Steps int
	// StoppedBy 为什么停：answered = 模型自己给出答案；max-steps = 到上限收尾
	StoppedBy string
}

// StoppedBy 的两个取值。
const (
	StoppedByAnswered = "answered"
	StoppedByMaxSteps = "max-steps"
)

// Deps 跑一轮要绑给循环的东西。
type Deps struct {
	Chat  llm.ChatClient
	Tools tools.Service
	// MaxSteps 覆盖配置。0 = 用 DefaultMaxSteps
	MaxSteps int
	// SystemPrompt 由组装根注入；空 = 取 AGENT 那条
	SystemPrompt string
	// Model 与 Host **仅用于日志与 transcript**。错误信息里只出现 Host，永不含 key
	Model string
	Host  string
	// TimeoutMs / StreamTimeoutMs 同样仅记录进 transcript
	TimeoutMs       int
	StreamTimeoutMs int
	// SessionHistory 会话历史持久化。nil = 不持久化，历史只来自 input.History
	SessionHistory llm.SessionHistoryStore
}

// Run 跑一轮。**返错留给调用方决定降级**（见包头）。
func Run(ctx context.Context, input RunInput, deps Deps) (RunResult, error) {
	maxSteps := deps.MaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultMaxSteps
	}

	toolDefs, err := deps.Tools.ToToolDefs(ctx, input.ToolContext)
	if err != nil {
		return RunResult{}, err
	}
	system := prompts.Compose(pickSystemPrompt(deps.SystemPrompt), deps.Tools.PromptSections(input.ToolContext))

	// 历史前缀：优先会话文件里逐字原样那份（含工具调用），否则退回数据库重建的那份
	prior := llm.LoadHistoryPrefix(deps.SessionHistory, llm.HistoryKey{
		SessionID: input.SessionID,
		AccountID: input.AccountID,
		History:   input.History,
	})

	// 预算里先扣掉**固定开销**：system、本轮问题、给回答留的位置，以及工具声明本身
	// （工具一多，声明也能占掉不少）。剩下的才是历史可用的部分
	encodedTools, _ := json.Marshal(toolDefs)
	fixed := llm.EstimateTokens(system) + llm.EstimateTokens(input.Question) +
		openai.MaxAnswerTokens + llm.EstimateTokens(string(encodedTools))
	budget := 0
	if input.ContextTokens > 0 {
		budget = max(0, input.ContextTokens-fixed)
	}
	kept := llm.CompressHistory(prior, budget)

	// 整个循环只有这一份消息列表在长。工具结果追加在后面，历史在前面
	messages := make([]llm.ChatMessage, 0, len(kept.Messages)+8)
	messages = append(messages, llm.ChatMessage{Role: llm.RoleSystem, Content: system})
	messages = append(messages, kept.Messages...)
	messages = append(messages, llm.ChatMessage{Role: llm.RoleUser, Content: userContent(input)})

	// 本回合新产生的消息从本轮 user 起。收尾时原样落进会话文件——**含 assistant 的
	// toolCalls 与工具结果**，下一轮才能逐字重放这一整段前缀
	turnStart := len(messages) - 1
	persistTurn := func() {
		if deps.SessionHistory != nil {
			deps.SessionHistory.Append(input.SessionID, input.AccountID, messages[turnStart:])
		}
	}

	usedTools := make([]string, 0, 8)
	steps := 0

	for index := 1; index <= maxSteps; index++ {
		steps = index

		result, err := deps.Chat(ctx, messages, toolDefs)
		if err != nil {
			// 模型/接口的错**往上抛**：降级的措辞是调用方的知识
			return RunResult{}, err
		}

		// 没有工具调用 = 它觉得可以答了。这就是最终答案
		if len(result.ToolCalls) == 0 {
			config.Log().Debug("工具循环结束：模型给出回答", config.Context{
				"model": deps.Model, "steps": index,
				"tools": len(usedTools), "chars": len([]rune(result.Content)),
			})

			messages = append(messages, llm.ChatMessage{
				Role: llm.RoleAssistant, Content: result.Content,
			})
			persistTurn()

			return RunResult{
				Text: result.Content, UsedTools: usedTools,
				Steps: index, StoppedBy: StoppedByAnswered,
			}, nil
		}

		// 有工具调用：先把 assistant 这条推进去（OpenAI 要求 tool 消息必须紧跟对应的
		// tool_calls），再逐个执行并追加结果
		messages = append(messages, llm.ChatMessage{
			Role:      llm.RoleAssistant,
			Content:   result.Content,
			ToolCalls: result.ToolCalls,
		})

		for _, call := range result.ToolCalls {
			usedTools = append(usedTools, call.Name)

			// 失败也照样喂回去：模型看到「参数不合法」会自己改，这正是循环的意义
			outcome := runToolCall(ctx, call, deps, input.ToolContext)
			messages = append(messages, llm.ChatMessage{
				Role:       llm.RoleTool,
				ToolCallID: call.ID,
				Content:    outcome,
			})
		}
	}

	// 步数用尽：**不再给工具**，只让它基于已有结果写答案
	return forcedAnswer(ctx, messages, deps, input, usedTools, steps, persistTurn), nil
}

// forcedAnswer 步数用尽后的收尾。
//
// 不带工具再问一次，而不是直接回一句「查不动了」——此时手上已经有若干工具结果，
// 让模型把它们收拢成一段话，用户至少能拿到答案的一部分。这一路**故意兜错**：
// 收尾失败也不该把整轮问答带走，回一句实话即可。
func forcedAnswer(
	ctx context.Context,
	messages []llm.ChatMessage,
	deps Deps,
	input RunInput,
	usedTools []string,
	steps int,
	persistTurn func(),
) RunResult {
	result, err := deps.Chat(ctx, messages, nil)
	if err != nil {
		config.Log().Warn("工具循环到达步数上限，收尾也失败了", config.Context{
			"model": deps.Model, "steps": steps, "error": err.Error(),
		})
		return RunResult{
			Text: MaxStepsAnswer, UsedTools: usedTools,
			Steps: steps, StoppedBy: StoppedByMaxSteps,
		}
	}

	config.Log().Warn("工具循环到达步数上限，已用无工具收尾", config.Context{
		"model": deps.Model, "steps": steps, "tools": len(usedTools),
	})

	messages = append(messages, llm.ChatMessage{Role: llm.RoleAssistant, Content: result.Content})
	persistTurn()

	text := result.Content
	if text == "" {
		text = MaxStepsAnswer
	}
	return RunResult{Text: text, UsedTools: usedTools, Steps: steps, StoppedBy: StoppedByMaxSteps}
}

// runToolCall 执行一次工具调用，返回**要给模型看的那句话**。
func runToolCall(ctx context.Context, call llm.ToolCall, deps Deps, tc tools.Context) string {
	args, ok := ParseToolArguments(call.Arguments)
	if !ok {
		// 模型把 JSON 写坏了。**不返错**——把这件事告诉它，下一轮它会写对
		return "参数不是合法的 JSON 对象：" + llm.Clamp(call.Arguments, 200)
	}

	result := deps.Tools.Call(ctx, call.Name, args, tc)
	return result.Content
}

// ParseToolArguments 解析模型给的参数。空串按「没有参数」处理——不带参数的工具很
// 常见，有些实现会老老实实给 "" 或 {}。
func ParseToolArguments(raw string) (map[string]any, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return map[string]any{}, true
	}

	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return nil, false
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return nil, false
	}
	return object, true
}

// userContent 本轮 user 消息。引用的正文**只在历史里找不到它时才贴**——与单发
// 路径同一条规则，否则同一条消息会说两遍。
func userContent(input RunInput) string {
	quoted := strings.TrimSpace(input.QuotedText)
	if quoted == "" {
		return input.Question
	}
	return input.Question + "\n\n（用户引用了这条消息，作为背景）" + llm.Clamp(quoted, 400)
}

func pickSystemPrompt(injected string) string {
	if injected != "" {
		return injected
	}
	return prompts.Agent
}
