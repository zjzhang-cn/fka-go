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
// ## 这个文件只有一个入口
//
// `(*Runner).Run`（见下）是跑一轮的全部。**问什么、谁问**是 `RunnerInput`；
// **依赖与部署事实**（工具表、模型、会话历史、上下文预算）都在 `Runner` 上。
// 两者不重叠，也不再有第二份把它们各抄一遍的输入结构。
//
// 与 llm/openai 的分工：那边管「怎么跟接口说话」，这边管「说几轮、每轮做什么」。
// **这个文件不 import llm/openai**——那条边曾只为 MaxAnswerTokens 一个常量存在，现已搬进
// llm；`internal/llm/boundary_test.go` 用 AST 扫 import 守住它不会再长回来。
package agent

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/llm"
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

// RunResult 跑一轮的结果。
type RunResult struct {
	Text string
	// UsedTools 这轮实际调过的工具名（含重复），供日志与测试断言
	UsedTools []string
	// ToolEvents 这轮每次工具调用的**可见记录**（名字 + 原样参数 + 结果），
	// 按调用顺序。给 CLI/TUI 显示用：`chat` 据此逐条渲染「调了什么、回了什么」。
	//
	// 结果**不截断**（截断是显示层的事，见 `cmd/fka/chat.go`）；`Arguments` 是
	// `ToolCall.Arguments` 那份原始 JSON 字符串，**不再解析**——见 llm.ToolCall。
	ToolEvents []ToolEvent
	// Steps 用了几次模型调用
	Steps int
	// StoppedBy 为什么停：answered = 模型自己给出答案；max-steps = 到上限收尾
	StoppedBy string
}

// ToolEvent 一次工具调用的可见记录。**只给显示/日志看**，不参与喂回模型的通道
// （那条通道是 role=tool 消息，见 runToolCall）。
type ToolEvent struct {
	Name      string
	Arguments string
	Result    string
}

// StoppedBy 的两个取值。
const (
	StoppedByAnswered = "answered"
	StoppedByMaxSteps = "max-steps"
)

// Run 跑一轮。**返错留给调用方决定降级**（见包头）。
//
// 缺席时返 ErrNoRunner——**不返 nil 结果**，所以调用点永远不会拿到一个空 RunResult
// 然后把它当成「模型没说话」。
func (r *Runner) Run(ctx context.Context, input RunnerInput) (RunResult, error) {
	if !r.Enabled() {
		return RunResult{}, ErrNoRunner
	}

	maxSteps := r.MaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultMaxSteps
	}

	// 工具看到的那份外界。**由输入里的身份与回话能力拼出来**，而不是让消息层
	// 自己拼一个 tools.Context 递进来——那样同一个「外界」就有两个入口，
	// 而漏掉一处身份的后果是权限过滤失效（见 tools.Context.PrincipalID）。
	tc := tools.Context{PrincipalID: input.PrincipalID, Reply: input.Reply}

	toolDefs, err := r.tools.ToToolDefs(ctx, tc)
	if err != nil {
		return RunResult{}, err
	}
	system := prompts.Compose(pickSystemPrompt(r.SystemPrompt), r.tools.PromptSections(tc))

	// 历史前缀：会话文件里那份，逐字原样（含工具调用与工具结果）。
	// 存储关掉时（SESSION_HISTORY=0）就没有历史，这一轮从零开始。
	prior := llm.LoadHistoryPrefix(r.sessionHistory, input.SessionID, input.AccountID)

	// 本轮 user 消息**只算一次**：预算与实际发出的必须是同一份文本。
	// 预算按 input.Question 算、发出时再走一遍 userContent（拼上引用正文）的话，
	// 引用那截字（≤400）就从预算里漏掉了——漏的不多，但「预算与实发口径不同」
	// 这类错只能靠人记得，而人总会忘
	question := userContent(input)

	// 预算里先扣掉**固定开销**：system、本轮问题、给回答留的位置，以及工具声明本身
	// （工具一多，声明也能占掉不少）。剩下的才是历史可用的部分
	encodedTools, _ := json.Marshal(toolDefs)
	fixed := llm.EstimateTokens(system) + llm.EstimateTokens(question) +
		llm.MaxAnswerTokens + llm.EstimateTokens(string(encodedTools))
	budget := 0
	if r.ContextTokens > 0 {
		budget = max(0, r.ContextTokens-fixed)
	}
	kept := llm.CompressHistory(prior, budget)

	// 整个循环只有这一份消息列表在长。工具结果追加在后面，历史在前面
	messages := make([]llm.ChatMessage, 0, len(kept.Messages)+8)
	messages = append(messages, llm.ChatMessage{Role: llm.RoleSystem, Content: system})
	messages = append(messages, kept.Messages...)
	messages = append(messages, llm.ChatMessage{
		Role: llm.RoleUser, Content: question, ImageAttachments: input.Images,
	})

	// 本回合新产生的消息从本轮 user 起。收尾时原样落进会话文件——**含 assistant 的
	// toolCalls 与工具结果**，下一轮才能逐字重放这一整段前缀
	turnStart := len(messages) - 1
	persistTurn := func() {
		if r.sessionHistory != nil {
			r.sessionHistory.Append(input.SessionID, input.AccountID, messages[turnStart:])
		}
	}

	// ── 提示词拼好了 ────────────────────────────────────────
	//
	// **记结构，不记全文**：system 每轮都是那几千字，历史是家里的话，
	// 全量落盘会让日志既大又把内容复制一份到别处。
	// 账号、消息号由 ctx 带过来（见 `internal/config/scope.go`），
	// 所以这里只说「这一步长什么样」。
	//
	// 提交与返回那两条**不在这里打**——那一层（`llm` 的 provider）才知道
	// 打到了哪个 host、用了多久、推理有多长。两边都打就重了，排查时
	// 看到两条意思相近的记录反而不知道该信哪条。
	config.Log().Debug(config.TypePRM, "提示词已拼接", config.Fields(ctx, config.Context{
		"session": input.SessionID, "systemChars": len([]rune(system)),
		"promptSections": len(r.tools.PromptSections(tc)),
		"historyKept":    len(kept.Messages), "historyDropped": kept.Dropped,
		"toolDefs": len(toolDefs), "messages": len(messages),
		"contextBudget": budget, "fixedTokens": fixed,
		"question": input.Question, "images": len(input.Images),
	}))

	usedTools := make([]string, 0, 8)
	toolEvents := make([]ToolEvent, 0, 8)
	steps := 0

	for index := 1; index <= maxSteps; index++ {
		steps = index

		result, err := r.chat(ctx, messages, toolDefs)
		if err != nil {
			// 模型/接口的错**往上抛**：降级的措辞是调用方的知识
			return RunResult{}, err
		}

		// 没有工具调用 = 它觉得可以答了。这就是最终答案
		if len(result.ToolCalls) == 0 {
			config.Log().Debug(config.TypeLLM, "工具循环结束：模型给出回答", config.Fields(ctx, config.Context{
				"model": r.Model, "steps": index,
				"tools": len(usedTools), "chars": len([]rune(result.Content)),
			}))

			messages = append(messages, llm.ChatMessage{
				Role: llm.RoleAssistant, Content: result.Content,
			})
			persistTurn()

			return RunResult{
				Text: result.Content, UsedTools: usedTools,
				ToolEvents: toolEvents,
				Steps:      index, StoppedBy: StoppedByAnswered,
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
			outcome := r.runToolCall(ctx, call, tc)
			toolEvents = append(toolEvents, ToolEvent{
				Name:      call.Name,
				Arguments: call.Arguments,
				Result:    outcome,
			})
			messages = append(messages, llm.ChatMessage{
				Role:       llm.RoleTool,
				ToolCallID: call.ID,
				Content:    outcome,
			})
		}
	}

	// 步数用尽：**不再给工具**，只让它基于已有结果写答案
	return r.forcedAnswer(ctx, messages, input, usedTools, toolEvents, steps, persistTurn), nil
}

// forcedAnswer 步数用尽后的收尾。
//
// 不带工具再问一次，而不是直接回一句「查不动了」——此时手上已经有若干工具结果，
// 让模型把它们收拢成一段话，用户至少能拿到答案的一部分。这一路**故意兜错**：
// 收尾失败也不该把整轮问答带走，回一句实话即可。
func (r *Runner) forcedAnswer(
	ctx context.Context,
	messages []llm.ChatMessage,
	input RunnerInput,
	usedTools []string,
	toolEvents []ToolEvent,
	steps int,
	persistTurn func(),
) RunResult {
	result, err := r.chat(ctx, messages, nil)
	if err != nil {
		config.Log().Warn(config.TypeLLM, "工具循环到达步数上限，收尾也失败了", config.Fields(ctx, config.Context{
			"model": r.Model, "steps": steps, "error": err.Error(),
		}))
		return RunResult{
			Text: MaxStepsAnswer, UsedTools: usedTools, ToolEvents: toolEvents,
			Steps: steps, StoppedBy: StoppedByMaxSteps,
		}
	}

	config.Log().Warn(config.TypeLLM, "工具循环到达步数上限，已用无工具收尾", config.Fields(ctx, config.Context{
		"model": r.Model, "steps": steps, "tools": len(usedTools),
	}))

	messages = append(messages, llm.ChatMessage{Role: llm.RoleAssistant, Content: result.Content})
	persistTurn()

	text := result.Content
	if text == "" {
		text = MaxStepsAnswer
	}
	return RunResult{Text: text, UsedTools: usedTools, ToolEvents: toolEvents, Steps: steps, StoppedBy: StoppedByMaxSteps}
}

// runToolCall 执行一次工具调用，返回**要给模型看的那句话**。
//
// ## 为什么这里只取 Content，丢掉 Result.OK
//
// `Result.OK` 是**如实搬过 MCP 边界**的（mcp/source.go 把 SDK 的 IsError 映射上来），
// 但对模型来说工具只有一条通道：这段文字。所以成败都照原样喂回去，**不因为
// OK=false 就换一句话、不重试、不短路**——那样等于把注册表已经编好的中文判语
// 再翻译一遍，而模型能改的只有「下一轮换个工具名」。
//
// 这个丢弃是**刻意的**，钉它的用例是 `TestRun_工具失败也喂回去让模型改`。
func (r *Runner) runToolCall(ctx context.Context, call llm.ToolCall, tc tools.Context) string {
	args, ok := ParseToolArguments(call.Arguments)
	if !ok {
		// 模型把 JSON 写坏了。**不返错**——把这件事告诉它，下一轮它会写对
		return "参数不是合法的 JSON 对象：" + llm.Clamp(call.Arguments, 200)
	}

	result := r.tools.Call(ctx, call.Name, args, tc)
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
func userContent(input RunnerInput) string {
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
