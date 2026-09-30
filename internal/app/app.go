// Package app 是**装配根**：把各模块接成一个能跑的整体。
//
// ## 为什么没有 cordis.yml
//
// 原实现用 Cordis 的插件树 + YAML 清单决定「谁 inject 谁、谁 provide 什么」。
// Go 版改成**构造函数链**，理由有三条，每条都在 Node 版自己的注释里写着痛点：
//
//  1. **顺序错误从运行期挪到编译期。** 原 Cordis 里 `loader.await()` 不等插件的异步
//     setup，所以入口与测试必须显式等 services，忘了就启动到一半才炸。构造函数链
//     里「B 需要 A」是函数签名上的依赖，编译器管。
//  2. **ctx 动态属性在 Go 里是负资产。** Cordis 的 `ctx.db` / `ctx.tools` 是运行时
//     注入的属性，Go 的结构体 + 接口在编译期就把「谁有什么」写死了。
//  3. **effect 逆序卸载在 Go 里是 defer。** 不用再造一套生命周期。
//
// 代价是「加一个 provider 要改一行 Go 而不是一行 YAML」——**这在 Go 里是好事**：
// 改完就编译过，不需要重启才知道配错了。
//
// ## 这里是唯一的装配点
//
// 业务包（llm / tools / agent / storage …）**互相不认识**，只认各自的契约；
// 具体的组装只在这里发生。加一个工具源 = 在 Build 里加一行 Use。
package app

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/zjzhang-cn/fka-go/internal/agent"
	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/llm"
	llmopenai "github.com/zjzhang-cn/fka-go/internal/llm/openai"
	"github.com/zjzhang-cn/fka-go/internal/messages"
	"github.com/zjzhang-cn/fka-go/internal/prompts"
	"github.com/zjzhang-cn/fka-go/internal/tools"
	"github.com/zjzhang-cn/fka-go/internal/tools/mcp"
	"github.com/zjzhang-cn/fka-go/internal/tools/skills"
)

// App 组装好的整体。
type App struct {
	// Tools 工具注册表
	Tools tools.Service
	// Policy 放行策略（`fka tools` 要展示）
	Policy tools.Policy
	// LLMProvider 接上的模型实现（**没配模型时是空串**）。给 `fka tools` 与错误提示用
	LLMProvider string
	// LLMReady 有没有配好模型服务
	LLMReady bool
	// Chat 带工具的调用函数。LLMReady 时非 nil
	Chat llm.ChatClient
	// Agent 工具循环。LLM_TOOLS=off 时为 nil
	Agent *agent.Runner
	// History 会话历史存储。SESSION_HISTORY=0 时为 nil
	History llm.SessionHistoryStore
	// McpServers 读到的 MCP 服务器名（按字母序）。给 `fka tools` 展示
	McpServers []string
	// McpConfigPath 实际读到的 mcp.json 路径
	McpConfigPath string
	// McpConfigured 有没有读到 MCP 配置
	McpConfigured bool
	// SkillsDir 技能目录（按配置顺序，后一个覆盖前一个的同名技能）
	SkillsDir []string

	// llmConfig 装配时读到的模型配置。**只在 Build 内部用**——以前它是导出的
	// `LLMConfig`，而除了 Build 自己没有一处读过它。
	llmConfig llm.Config

	// Channels 渠道接缝。**永远非 nil**——没有配任何渠道时它是个空接缝，
	// 不是缺席。接缝是「渠道从哪来」的唯一出口，业务层只认它。
	Channels *channels.Service
	// ChannelKinds 已注册的渠道**种类**（按注册顺序）
	ChannelKinds []string
	// Messages 消息处理器。**Runner 为 nil 时它仍在**，每条消息会得到一句
	// 「没接上模型」——那比服务安静地不收消息好
	Messages *messages.Handler
}

// Options 装配的可选项。
type Options struct {
	// SkipEnv 不读 .env。测试与嵌入式用法
	SkipEnv bool
	// LLMProviders 模型实现的候选。**空 = 用 defaultLLMProviders()**。
	//
	// 这就是「换一个模型实现」的唯一改动点：加一个实现，在这里多一行（或整个列表
	// 换掉）。别的包不认识任何 provider——`internal/llm/boundary_test.go` 用 AST 扫
	// import 守住这条。
	LLMProviders []llm.Provider

	// ChannelProviders 要接的渠道种类。**由装配调用方给**——
	//
	// 刻意不写配置文件：加一个渠道 = 在这里多传一个 provider，而 `cordis.yml`
	// 那种「配错一行要到运行期才发现」的问题在 Go 里不存在（改完就编译过）。
	// 接缝自己不认任何实现，装配根是唯一认识它们的地方。
	ChannelProviders []channels.Provider
}

// Build 装配。**每一步的错误都在这里就地降级，绝不整体返错**——理由与原实现
// 一样：可选能力缺失（没配模型、没有 MCP、没装技能）不该让服务起不来，只该让
// 对应的能力缺席。
func Build(opts Options) *App {
	if !opts.SkipEnv {
		// **必须在读任何环境变量之前**：默认路径与 .env 都按安装根解析
		config.LoadEnv()
	}

	app := &App{}

	// ── 工具 ──────────────────────────────────────────────
	//
	// **只有两条能力来源：MCP 与技能。** 这个 agent 刻意不带任何内置能力——
	// 文档、记忆、检索全在 MCP server 里，它自己不拥有任何数据。所以这里注册
	// 的两个源就是它的全部「本事」。
	app.Policy = tools.ReadToolPolicy()
	app.Tools = tools.NewRegistry(nil, app.Policy)
	app.SkillsDir = skills.ResolveDirs()
	app.Tools.Use(skills.NewSource(app.SkillsDir))
	app.registerMcp()

	// ── 模型 ──────────────────────────────────────────────
	app.registerLLM(opts.LLMProviders)

	// ── 渠道 ──────────────────────────────────────────────
	app.Channels = channels.NewService()
	app.registerChannels(opts.ChannelProviders)

	// ── 会话历史 ──────────────────────────────────────────
	app.History = llm.NewDefaultSessionHistory()
	if app.History == nil {
		config.Log().Info(config.TypeSYS, "SESSION_HISTORY 已关闭，历史不落盘", config.Context{})
	}

	// ── 工具循环 ──────────────────────────────────────────
	//
	// **Agent 永远非 nil**（见 agent 包头）。所以这里不能再判指针，只能问它自己。
	// 没配模型时连 NewRunner 都不调——那会让它拿一个空 chat 去构造。
	if app.LLMReady {
		app.Agent = agent.NewRunner(app.Chat, app.Tools, agent.RunnerOptions{
			ContextTokens:   app.llmConfig.ContextTokens,
			SessionHistory:  app.History,
			SystemPrompt:    prompts.Agent,
			Model:           app.llmConfig.Model,
			Host:            llmopenai.HostOf(app.llmConfig.BaseURL),
			TimeoutMs:       app.llmConfig.TimeoutMs,
			StreamTimeoutMs: app.llmConfig.StreamTimeoutMs,
		})
		if !app.Agent.Enabled() {
			config.Log().Info(config.TypeSYS, "LLM_TOOLS 已关闭，问答不走工具循环", config.Context{})
		}
	}

	// ── 消息层 ────────────────────────────────────────────
	//
	// **必须建在 Agent 之后**：它持有 runner，而 runner 可能因为 LLM_TOOLS=off
	// 或没配模型而缺席。先建后赋值会让它永远拿到 nil——症状是「每条消息都回
	// 没接上模型」，而配置看上去完全正常。
	app.Messages = messages.NewHandler(app.Agent)

	return app
}

// defaultLLMProviders 默认可用的模型实现。
//
// **这里新增一行就是新增一个可选模型实现**，而这是全仓唯一允许 import 具体 provider
// 的地方之一（另一个是 cmd/fka 里打印 host 的那处）。
//
// `Reasoning: os.Stderr` 是**装配根该做的那个决定**：推理是过程信息，而 stdout 只该有
// 结果（`fka ask` 的答案、`fka tools --json` 的 JSON）。以前 provider 直接 `fmt.Print`
// 到 stdout，于是 `fka ask > 答案.txt` 里混着半截推理。想彻底关掉用
// `LLM_SHOW_REASONING=0`。
func defaultLLMProviders() []llm.Provider {
	return []llm.Provider{llmopenai.Provider{Reasoning: os.Stderr}}
}

// registerLLM 从候选里挑第一个配好的。
//
// ## 为什么是「挑第一个」而不是按名字选
//
// 候选只有一个，所以「选谁」与「有没有」是同一件事。想要按环境变量选实现时，
// `llm.Provider.ID()` 就是为此留的（它是这条契约上唯一为了**辨识**而存在的方法）。
// 现在不实现那条：没有第二个实现时它无法被验证，只会是一段没人跑过的分支。
func (a *App) registerLLM(candidates []llm.Provider) {
	if len(candidates) == 0 {
		candidates = defaultLLMProviders()
	}

	for _, provider := range candidates {
		cfg, ok := provider.ReadConfig()
		if !ok {
			// **不是错**：这个实现没配好，试下一个。全试完才算缺席
			continue
		}
		a.LLMReady = true
		a.LLMProvider = provider.ID()
		a.Chat = provider.CreateChat(cfg)
		a.llmConfig = cfg
		return
	}

	config.Log().Info(config.TypeSYS, "没配 LLM_API_KEY / LLM_MODEL，问答将不走模型", config.Context{
		"candidates": providerIDs(candidates),
	})
}

func providerIDs(providers []llm.Provider) []string {
	ids := make([]string, 0, len(providers))
	for _, provider := range providers {
		ids = append(ids, provider.ID())
	}
	return ids
}

// registerMcp 把 MCP 源接进注册表。
//
// **没配 mcp.json 就什么都不做**（正常状态，不告警）；**配错了记一条错误后跳过**，
// 不让一个坏配置拖垮整个服务——但必须留下痕迹，否则用户以为「配了但没生效」。
func (a *App) registerMcp() {
	a.McpConfigPath = mcp.ResolveConfigPath()

	read, err := mcp.ReadConfig()
	if err != nil {
		if configErr, ok := mcp.AsConfigError(err); ok {
			config.Log().Error(config.TypeSYS, "MCP 配置读不了，本次不接 MCP", config.Context{
				"error": configErr.Message, "hint": configErr.Hint, "path": a.McpConfigPath,
			})
		} else {
			config.Log().Error(config.TypeSYS, "MCP 配置读不了，本次不接 MCP", config.Context{
				"error": err.Error(), "path": a.McpConfigPath,
			})
		}
		return
	}

	if read == nil {
		config.Log().Info(config.TypeSYS, "未配置 MCP 服务器（"+a.McpConfigPath+" 不存在）", config.Context{})
		return
	}
	if len(read.Servers) == 0 {
		config.Log().Info(config.TypeSYS, "未配置 MCP 服务器（"+read.Path+" 为空）", config.Context{})
		return
	}

	a.McpConfigured = true
	a.Tools.Use(mcp.NewSource(mcp.SourceOptions{Servers: read.Servers}))
	for name := range read.Servers {
		a.McpServers = append(a.McpServers, name)
	}
	sortStrings(a.McpServers)

	config.Log().Info(config.TypeSYS, "MCP 源就绪", config.Context{
		"path": read.Path, "servers": len(read.Servers),
	})
}

// registerChannels 注册渠道 provider。
//
// ## 一个渠道起不来，不该把别的渠道一起带走
//
// 逐个注册、逐个记错。**唯一的例外是同一个 provider 自己起不来**——那通常是配置
// 写错了（比如 .env 里没有账号），此时继续跑只会让人以为「配了但没生效」。
func (a *App) registerChannels(providers []channels.Provider) {
	if len(providers) == 0 {
		config.Log().Info(config.TypeSYS, "未配置任何渠道，只跑无头问答", config.Context{})
		return
	}

	ctx := context.Background()
	for _, provider := range providers {
		if _, err := a.Channels.Register(ctx, provider); err != nil {
			config.Log().Error(config.TypeSYS, "渠道起不来，已跳过", config.Context{
				"kind": provider.ID(), "error": err.Error(),
			})
			continue
		}
		a.ChannelKinds = append(a.ChannelKinds, provider.ID())
	}
}

// drainTimeout 停机时等消息层排空的上限。
//
// **必须有上限**：worker 里可能正跑着一整轮问答（模型整体超时 120s），干等下去
// 会让 Ctrl-C 之后进程两分钟不退出——那比丢掉几条已排队的消息更让人困惑。
const drainTimeout = 10 * time.Second

// Serve 常驻：订阅入站 → 跑消息循环 → 收到信号停机。
//
// ## 顺序是硬要求
//
// **先 Subscribe，再 StartAll。** 反过来会有一个丢消息的窗口——渠道一开收就可能
// 来消息，而那时还没有订阅者。
func (a *App) Serve(ctx context.Context) error {
	subscription := a.Channels.Subscribe()
	defer subscription.Close()

	// goroutine 代替 Node 版的 worker：**不阻塞 StartAll**，而接缝的投递是非阻塞的。
	//
	// dispatcher **按账号分片**——同账号串行、账号间并行，见 messages/dispatch.go
	// 里为什么这么切。顺带一提，并发上限不用配：worker 数 = 账号数。
	dispatched := make(chan struct{})
	go func() {
		defer close(dispatched)
		messages.Dispatch(a.Messages, subscription)()
	}()

	a.Channels.StartAll(ctx)

	if len(a.Channels.Instances()) == 0 {
		return fmt.Errorf("没有接上任何渠道，无事可做")
	}
	config.Log().Info(config.TypeSYS, "服务就绪，等待消息", config.Context{"kinds": strings.Join(a.ChannelKinds, "、")})

	<-ctx.Done()
	a.Channels.StopAll(context.WithoutCancel(ctx))

	// **要等 dispatcher 排空再返回**，否则下面 `Close()` 会在某个 worker 正调
	// MCP 工具时把子进程杀了——症状是「停机时最后一条消息报工具调用失败」。
	// 提前退订让 dispatcher 收尾（`Close` 内部有 sync.Once，多调一次无妨）。
	subscription.Close()
	select {
	case <-dispatched:
	case <-time.After(drainTimeout):
		config.Log().Warn(config.TypeSYS, "消息层没能在停机时限内排空", config.Context{"timeout": drainTimeout})
	}
	return nil
}

// Close 收尾。**逆序**：先停渠道，再断 MCP 子进程，最后关日志。
func (a *App) Close() {
	if a.Channels != nil {
		a.Channels.StopAll(context.Background())
	}
	if a.Tools != nil {
		if err := a.Tools.Close(); err != nil {
			config.Log().Warn(config.TypeSYS, "工具源关闭有问题", config.Context{"error": err.Error()})
		}
	}
	_ = config.Log().Close()
}

func sortStrings(values []string) { sort.Strings(values) }

// WarmMcp 在启动期就发起 MCP 连接。
//
// 惰性连接是「不阻塞启动」的正确设计，但代价是**第一条消息才看到「哪个服务器连上了」**。
// 这里主动预热：失败在源内部已按服务器跳过，不会让启动失败。
func (a *App) WarmMcp(ctx context.Context) {
	if !a.McpConfigured {
		return
	}
	// 一次空 Context 足够——列举工具只需要连接，不依赖任何身份
	if _, err := a.Tools.Tools(ctx, tools.Context{}); err != nil {
		config.Log().Warn(config.TypeSYS, "MCP 预热有问题", config.Context{"error": err.Error()})
	}
}
