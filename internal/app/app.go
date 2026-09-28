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
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zjzhang-cn/fka-go/internal/agent"
	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/llm"
	llmopenai "github.com/zjzhang-cn/fka-go/internal/llm/openai"
	"github.com/zjzhang-cn/fka-go/internal/tools"
	"github.com/zjzhang-cn/fka-go/internal/tools/mcp"
)

// App 组装好的整体。
type App struct {
	// Tools 工具注册表
	Tools tools.Service
	// Policy 放行策略（`fka tools` 要展示）
	Policy tools.Policy
	// LLM 模型服务。**没配时 LLMReady 为 false**
	LLM llm.Provider
	// LLMConfig 模型配置。没配时是零值
	LLMConfig llm.Config
	// LLMReady 有没有配好模型服务
	LLMReady bool
	// Chat 带工具的调用函数。LLMReady 时非 nil
	Chat llm.ChatClient
	// Composer 单发 composer。LLMReady 时非 nil
	Composer llm.Composer
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
}

// Options 装配的可选项。
type Options struct {
	// StorageRoot 存储根。给文档类工具定位原文件用
	StorageRoot string
	// AdminWxid 管理员微信 ID
	AdminWxid string
	// SkipEnv 不读 .env。测试与嵌入式用法
	SkipEnv bool
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
	app.Policy = tools.ReadToolPolicy()
	app.Tools = tools.NewRegistry(nil, app.Policy)
	app.registerMcp()

	// ── 模型 ──────────────────────────────────────────────
	provider := llmopenai.Provider{}
	app.LLM = provider

	if cfg, ok := provider.ReadConfig(); ok {
		app.LLMReady = true
		app.LLMConfig = cfg
		app.Chat = provider.CreateChat(cfg)
		app.Composer = provider.CreateComposer(cfg)
	} else {
		// 缺 key 或 model 就整块缺席，问答退回「回原文片段」
		config.Log().Info("没配 LLM_API_KEY / LLM_MODEL，问答将不走模型", config.Context{})
	}

	// ── 会话历史 ──────────────────────────────────────────
	app.History = llm.NewDefaultSessionHistory()
	if app.History == nil {
		config.Log().Info("SESSION_HISTORY 已关闭，历史不落盘", config.Context{})
	}

	// ── 工具循环 ──────────────────────────────────────────
	if app.LLMReady {
		app.Agent = agent.NewRunner(app.Chat, app.Tools, agent.RunnerOptions{
			ContextTokens:   app.LLMConfig.ContextTokens,
			StorageRoot:     opts.StorageRoot,
			AdminWxid:       opts.AdminWxid,
			SessionHistory:  app.History,
			Model:           app.LLMConfig.Model,
			Host:            llmopenai.HostOf(app.LLMConfig.BaseURL),
			TimeoutMs:       app.LLMConfig.TimeoutMs,
			StreamTimeoutMs: app.LLMConfig.StreamTimeoutMs,
		})
		if app.Agent == nil {
			config.Log().Info("LLM_TOOLS 已关闭，问答走单次路径", config.Context{})
		}
	}

	return app
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
			config.Log().Error("MCP 配置读不了，本次不接 MCP", config.Context{
				"error": configErr.Message, "hint": configErr.Hint, "path": a.McpConfigPath,
			})
		} else {
			config.Log().Error("MCP 配置读不了，本次不接 MCP", config.Context{
				"error": err.Error(), "path": a.McpConfigPath,
			})
		}
		return
	}

	if read == nil {
		config.Log().Info("未配置 MCP 服务器（"+a.McpConfigPath+" 不存在）", config.Context{})
		return
	}
	if len(read.Servers) == 0 {
		config.Log().Info("未配置 MCP 服务器（"+read.Path+" 为空）", config.Context{})
		return
	}

	a.McpConfigured = true
	a.Tools.Use(mcp.NewSource(mcp.SourceOptions{Servers: read.Servers}))
	for name := range read.Servers {
		a.McpServers = append(a.McpServers, name)
	}
	sortStrings(a.McpServers)

	config.Log().Info("MCP 源就绪", config.Context{
		"path": read.Path, "servers": len(read.Servers),
	})
}

// Close 收尾。**逆序**：先断 MCP 子进程，再关日志。
func (a *App) Close() {
	if a.Tools != nil {
		if err := a.Tools.Close(); err != nil {
			config.Log().Warn("工具源关闭有问题", config.Context{"error": err.Error()})
		}
	}
	_ = config.Log().Close()
}

func sortStrings(values []string) { sort.Strings(values) }

func joinPath(base, name string) string { return filepath.Join(base, name) }

// StorageRootFromEnv 按约定解析存储根：FILE_STORE_PATH 压倒一切，其次 NAS 挂载，
// 最后退回安装根下的 data/nas。**回退必须显式**——静默回退会让「生产忘了挂 NAS」
// 表现为「文件存在但找不到」。
func StorageRootFromEnv() (string, string) {
	if explicit := strings.TrimSpace(os.Getenv("FILE_STORE_PATH")); explicit != "" {
		return explicit, "explicit"
	}
	nasMount := strings.TrimSpace(os.Getenv("NAS_MOUNT_PATH"))
	if nasMount == "" {
		nasMount = "/mnt/nas"
	}
	if info, err := os.Stat(nasMount); err == nil && info.IsDir() {
		return joinPath(nasMount, "family-knowledge"), "nas"
	}
	return config.DataPath("nas"), "fallback"
}

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
		config.Log().Warn("MCP 预热有问题", config.Context{"error": err.Error()})
	}
}
