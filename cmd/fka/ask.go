package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/zjzhang-cn/fka-go/internal/agent"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// runAsk 无头跑一轮问答。
//
// ## 为什么现在就有这个命令
//
// 渠道层（微信接入）还没搬，但它**不是验证 agent 的必要条件**：agent 的输入就是
// 「一段问题 + 身份 + 工具」，这里三样都有了。少一个渠道，agent 就多一层「只能靠
// 肉眼验」的裸露——而工具循环恰恰是最需要反复调的一环（提示词、阈值、工具描述
// 全靠试出来）。
func runAsk(ctx context.Context, args []string) int {
	question := strings.TrimSpace(strings.Join(args, " "))
	if question == "" {
		fmt.Fprintln(os.Stderr, "用法：fka ask <问题>")
		return exitUsage
	}

	viewer := flagOrEnv(args, "--viewer", "FKA_VIEWER", "cli")
	session := flagOrEnv(args, "--session", "FKA_SESSION", "cli")

	application := build()
	defer application.Close()

	application.WarmMcp(ctx)

	if !application.LLMReady {
		fmt.Fprintln(os.Stderr, "没配 LLM_API_KEY / LLM_MODEL，ask 没法跑。")
		fmt.Fprintln(os.Stderr, "（这一条在「有工具」与「无工具」两条路上都需要模型——"+
			"单次问答也要模型把片段写成答案。）")
		return exitFail
	}

	hasTools, err := application.Agent.HasTools(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "列工具失败：%s\n", err.Error())
		return exitFail
	}

	if application.Agent == nil {
		fmt.Fprintln(os.Stderr, "工具循环未启用（LLM_TOOLS=off）。")
		return exitFail
	}

	if !hasTools {
		// 与消息层同一条规则：**没有任何工具就走单次问答**。
		// CLI 这里没有「回原文片段」的降级（那需要检索，接缝还没搬），
		// 所以直接说清楚缺什么，而不是给一个空答案
		fmt.Fprintln(os.Stderr, "现在没有任何工具被放行，ask 无从跑起。")
		fmt.Fprintln(os.Stderr, "  记忆与文档走 MCP，而 MCP 工具一律是 external 类：")
		fmt.Fprintf(os.Stderr, "  请设 LLM_TOOL_EFFECTS=read,external，并在 %s 里配好服务器。\n",
			application.McpConfigPath)
		fmt.Fprintln(os.Stderr, "  跑 `fka tools` 看现在放行了什么。")
		return exitFail
	}

	result, err := application.Agent.Run(ctx, agent.RunnerInput{
		SessionID:  session,
		ViewerWxid: viewer,
		Question:   question,
	})
	if err != nil {
		// 模型的错如实报，**不静默降级**——与原实现同一条理由：
		// 「稍后再试」会把「接口没配好」伪装成「模型偶尔不回」
		fmt.Fprintf(os.Stderr, "问答失败：%s\n", err.Error())
		return exitFail
	}

	fmt.Println(result.Text)

	if debugEnabled() {
		fmt.Fprintf(os.Stderr, "\n[debug] steps=%d stoppedBy=%s tools=%s\n",
			result.Steps, result.StoppedBy, strings.Join(result.UsedTools, ", "))
	}
	return exitOK
}

// runTools 列出模型现在能看到的工具，以及被挡下的那些与原因。
func runTools(ctx context.Context, args []string) int {
	application := build()
	defer application.Close()

	application.WarmMcp(ctx)

	asJSON := hasFlag(args, "--json")
	listed, err := application.Tools.Tools(ctx, tools.Context{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "列工具失败：%s\n", err.Error())
		return exitFail
	}

	policy := application.Policy

	if asJSON {
		payload := map[string]any{
			"effects":     policy.Allowed(),
			"policy":      tools.DescribeToolPolicy(policy),
			"tools":       listed,
			"mcp_servers": application.McpServers,
		}
		encoded, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "序列化失败：%s\n", err.Error())
			return exitFail
		}
		fmt.Println(string(encoded))
		return exitOK
	}

	fmt.Printf("放行范围：%s\n", tools.DescribeToolPolicy(policy))
	fmt.Println()

	if len(listed) == 0 {
		fmt.Println("模型现在看不到任何工具。")
		fmt.Printf("MCP 配置：%s", application.McpConfigPath)
		if application.McpConfigured {
			fmt.Printf("（已配 %d 个服务器：%s）", len(application.McpServers),
				strings.Join(application.McpServers, "、"))
		} else {
			fmt.Print("（没有配）")
		}
		fmt.Println()
		return exitOK
	}

	// 按源分组，源内按注册顺序——与模型看到的顺序一致，排查时少一层猜测
	current := ""
	for _, tool := range listed {
		if tool.SourceID != current {
			current = tool.SourceID
			fmt.Printf("── %s（%s）\n", tool.Label, tool.SourceID)
		}
		fmt.Printf("   %-44s %-9s %s\n", tool.FullName, tool.Spec.Effect, firstLine(tool.Spec.Description))
	}
	return exitOK
}

// flagOrEnv 从 args 里取 --name value，没有则退回环境变量，再没有则用兜底。
func flagOrEnv(args []string, flag string, env string, fallback string) string {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
		if value, ok := cutPrefix(arg, flag+"="); ok {
			return value
		}
	}
	if value := strings.TrimSpace(os.Getenv(env)); value != "" {
		return value
	}
	return fallback
}

func hasFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
		if _, ok := cutPrefix(arg, flag+"="); ok {
			return true
		}
	}
	return false
}

func cutPrefix(value, prefix string) (string, bool) {
	if strings.HasPrefix(value, prefix) {
		return value[len(prefix):], true
	}
	return "", false
}

func debugEnabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("FKA_DEBUG")))
	return value == "1" || value == "true" || value == "on"
}

func firstLine(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	runes := []rune(text)
	if len(runes) > 60 {
		return string(runes[:60]) + "…"
	}
	return text
}

// 保留 llm 包的引用：单次路径的类型在工具链补齐前用不到，但装配根已经产出它。
