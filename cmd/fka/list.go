// `fka list` —— 按类型列出安装里**看得见的东西**：技能、工具、MCP 资源、提示词、服务器。
//
// ## 为什么从 `fka tools` 变成 `fka list <类型>`
//
// `tools` 只回答「模型能调什么工具」，但排查「我那个技能生效了吗」「这台服务器有
// 哪些资源」时，人想看的不是工具表本身。`list` 把几类事实各给一个类型，`all` 一次
// 全给。工具那一类（`list tool`）保持原来 `fka tools` 的输出。
//
// ## 类型是**位置参数**，不是子命令
//
// `fka list tool` 与 `fka list resource` 共用同一份参数解析（`parseFlags`），
// 于是 `--json` / `--no-color` 写在前后都认，多出来的位置参数以用法错退出。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/zjzhang-cn/fka-go/internal/app"
	"github.com/zjzhang-cn/fka-go/internal/tools"
	"github.com/zjzhang-cn/fka-go/internal/tools/mcp"
	"github.com/zjzhang-cn/fka-go/internal/tools/skills"
)

// runList 分发 `fka list <类型>`。
func runList(ctx context.Context, parsed cliArgs) int {
	kind, err := listKind(parsed)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		printListUsage(os.Stderr)
		return exitUsage
	}

	application := build(ctx)
	defer application.Close()

	// 列资源/提示词要先连上服务器；列工具/技能也顺带预热，行为统一
	application.WarmMcp(ctx)

	if hasFlag(parsed, "--json") {
		return listJSON(ctx, application, kind)
	}
	return listText(ctx, application, kind)
}

// listKind 解析并归一 `list` 的类型。单复数都收。
func listKind(parsed cliArgs) (string, error) {
	if len(parsed.positional) == 0 {
		return "", errors.New("要列什么？给一个类型：skill / tool / resource / prompt / server / all")
	}
	if len(parsed.positional) > 1 {
		return "", errors.New("只认一个类型，多了：" + strings.Join(parsed.positional[1:], " "))
	}

	switch strings.ToLower(strings.TrimSpace(parsed.positional[0])) {
	case "skill", "skills":
		return "skill", nil
	case "tool", "tools":
		return "tool", nil
	case "resource", "resources":
		return "resource", nil
	case "prompt", "prompts":
		return "prompt", nil
	case "server", "servers", "mcp":
		return "server", nil
	case "all":
		return "all", nil
	default:
		return "", fmt.Errorf("不认识的类型：%s（可用：skill、tool、resource、prompt、server、all）",
			parsed.positional[0])
	}
}

func printListUsage(w io.Writer) {
	fmt.Fprint(w, `用法：fka list <类型> [--json]

  skill      发现到的技能（名字、目录、说明）
  tool       模型现在能看到的工具与五类放行情况
  resource   MCP 服务器暴露的资源与资源模板
  prompt     MCP 服务器提供的提示词模板
  server     配置里的 MCP 服务器
  all        以上全部
`)
}

// ── 文本输出 ────────────────────────────────────────────

func listText(ctx context.Context, application *app.App, kind string) int {
	first := true
	section := func(title string) {
		if !first {
			fmt.Println()
		}
		first = false
		fmt.Println("── " + title)
	}

	if kind == "skill" || kind == "all" {
		section("技能")
		printSkills(os.Stdout, application.SkillsDir)
	}
	if kind == "tool" || kind == "all" {
		section("工具")
		printTools(ctx, application)
	}
	if kind == "resource" || kind == "all" {
		section("MCP 资源")
		printResources(ctx, application)
	}
	if kind == "prompt" || kind == "all" {
		section("MCP 提示词")
		printPrompts(ctx, application)
	}
	if kind == "server" || kind == "all" {
		section("MCP 服务器")
		printServers(application)
	}
	return exitOK
}

// printSkills 列出**发现到的技能**，不只是那两个查技能的工具。
//
// `skills__list` 的存在只说明「有个办法能查」——它回答不了「我那个技能生效了吗」。
// 而「我改了 skills/ 目录，它怎么没反应」是最常见的一类问题，答案就在这一段里。
//
// 空目录也要说清**目录在哪**：那比只说「没有技能」有用——模型能据此告诉用户
// 「放这儿就行」，人也能立刻看出自己是不是看错了地方（见 README 的「安装根」）。
func printSkills(w io.Writer, dirs []string) {
	found := discoveredSkills(dirs)

	if len(found) == 0 {
		fmt.Fprintln(w, "   （空）这是正常状态，不是没装好——往下面任一目录放 <名字>/SKILL.md：")
		for _, dir := range dirs {
			fmt.Fprintf(w, "     %s\n", dir)
		}
		return
	}
	for _, skill := range found {
		// **目录名也打出来**：front matter 里的 name 可能与目录名不同，
		// 而 skills__load 两个都认——只显示一个会让人搞不清自己建的是哪个
		mark := ""
		if !skills.IsStandardName(skill.Name) || skill.Name != skill.Dir {
			// 那类技能 Claude 等客户端会拒绝；fka 自己仍认，所以只提示不拦
			mark = "  ⚠ 名字不符合标准 Agent Skills（小写字母/数字/连字符、且与目录名一致）"
		}
		fmt.Fprintf(w, "   %-24s %-28s %s%s\n", skill.Name, "<"+skill.Dir+">", firstLine(skill.Description), mark)
		if len(skill.AllowedTools) > 0 {
			fmt.Fprintf(w, "      allowed-tools（只展示不执行）：%s\n", strings.Join(skill.AllowedTools, " "))
		}
	}
	fmt.Fprintf(w, "   来自：%s\n", strings.Join(dirs, "、"))
}

func printTools(ctx context.Context, application *app.App) {
	listed, err := application.Tools.Tools(ctx, tools.Context{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "列工具失败：%s\n", err.Error())
		return
	}

	fmt.Printf("   放行范围：%s\n", tools.DescribeToolPolicy(application.Policy))

	if len(listed) == 0 {
		fmt.Println("   模型现在看不到任何工具。")
		fmt.Printf("   MCP 配置：%s", application.McpConfigPath)
		if application.McpConfigured {
			fmt.Printf("（已配 %d 个服务器：%s）", len(application.McpServers),
				strings.Join(application.McpServers, "、"))
		} else {
			fmt.Print("（没有配）")
		}
		fmt.Println()
		return
	}

	// 按源分组，源内按注册顺序——与模型看到的顺序一致，排查时少一层猜测
	current := ""
	for _, tool := range listed {
		if tool.SourceID != current {
			current = tool.SourceID
			fmt.Printf("   · %s（%s）\n", tool.Label, tool.SourceID)
		}
		fmt.Printf("     %-44s %-9s %s\n", tool.FullName, tool.Spec.Effect, firstLine(tool.Spec.Description))
	}
}

func printResources(ctx context.Context, application *app.App) {
	if application.McpCatalog == nil {
		fmt.Printf("   （没有配置 MCP）配置：%s\n", application.McpConfigPath)
		return
	}
	items := application.McpCatalog.Resources(ctx)
	if len(items) == 0 {
		fmt.Println("   （已连上的服务器没有暴露资源）")
		return
	}
	for _, item := range items {
		kind := "资源"
		if item.Template {
			kind = "模板"
		}
		desc := firstLine(item.Description)
		if desc != "" {
			desc = " — " + desc
		}
		fmt.Printf("   %-16s %-4s %-14s %s%s\n", item.Server, kind, item.MIMEType, item.URI, desc)
	}
}

func printPrompts(ctx context.Context, application *app.App) {
	if application.McpCatalog == nil {
		fmt.Printf("   （没有配置 MCP）配置：%s\n", application.McpConfigPath)
		return
	}
	items := application.McpCatalog.Prompts(ctx)
	if len(items) == 0 {
		fmt.Println("   （已连上的服务器没有提供提示词）")
		return
	}
	for _, item := range items {
		fmt.Printf("   %-16s %-20s %s\n", item.Server, item.Name, firstLine(item.Description))
		if len(item.Arguments) > 0 {
			fmt.Printf("     %s\n", formatPromptArgs(item.Arguments))
		}
	}
}

func printServers(application *app.App) {
	if !application.McpConfigured || len(application.McpServers) == 0 {
		fmt.Printf("   （没有配置 MCP）配置：%s\n", application.McpConfigPath)
		return
	}
	for _, name := range application.McpServers {
		fmt.Printf("   %s\n", name)
	}
	fmt.Printf("   配置：%s\n", application.McpConfigPath)
}

// formatPromptArgs 把提示词参数拼成一行「名（必填）：说明」。
func formatPromptArgs(args []mcp.PromptArg) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		label := arg.Name
		if arg.Required {
			label += "（必填）"
		}
		if arg.Description != "" {
			label += " " + arg.Description
		}
		parts = append(parts, label)
	}
	return "参数：" + strings.Join(parts, "、")
}

// ── JSON 输出 ───────────────────────────────────────────

// listJSON 把请求的类型打成 JSON。`all` 一次给出全部键；其余只给相关键——
// 少而准比「恒返回一大坨」更适合脚本按需取。
func listJSON(ctx context.Context, application *app.App, kind string) int {
	payload := map[string]any{}

	if kind == "skill" || kind == "all" {
		payload["skills"] = discoveredSkills(application.SkillsDir)
		payload["skills_dirs"] = application.SkillsDir
	}
	if kind == "tool" || kind == "all" {
		listed, err := application.Tools.Tools(ctx, tools.Context{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "列工具失败：%s\n", err.Error())
			return exitFail
		}
		payload["effects"] = application.Policy.Allowed()
		payload["policy"] = tools.DescribeToolPolicy(application.Policy)
		payload["tools"] = listed
	}
	if kind == "resource" || kind == "all" {
		payload["resources"] = resourceRows(resourcesOf(ctx, application))
	}
	if kind == "prompt" || kind == "all" {
		payload["prompts"] = promptRows(promptsOf(ctx, application))
	}
	if kind == "server" || kind == "all" {
		payload["mcp_servers"] = application.McpServers
		payload["mcp_config"] = application.McpConfigPath
		payload["mcp_configured"] = application.McpConfigured
	}

	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "序列化失败：%s\n", err.Error())
		return exitFail
	}
	fmt.Println(string(encoded))
	return exitOK
}

func resourcesOf(ctx context.Context, application *app.App) []mcp.ServerResource {
	if application.McpCatalog == nil {
		return nil
	}
	return application.McpCatalog.Resources(ctx)
}

func promptsOf(ctx context.Context, application *app.App) []mcp.ServerPrompt {
	if application.McpCatalog == nil {
		return nil
	}
	return application.McpCatalog.Prompts(ctx)
}

func resourceRows(items []mcp.ServerResource) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, map[string]any{
			"server":      item.Server,
			"uri":         item.URI,
			"name":        item.Name,
			"mime":        item.MIMEType,
			"description": item.Description,
			"template":    item.Template,
		})
	}
	return out
}

func promptRows(items []mcp.ServerPrompt) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		args := make([]map[string]any, 0, len(item.Arguments))
		for _, arg := range item.Arguments {
			args = append(args, map[string]any{
				"name":        arg.Name,
				"description": arg.Description,
				"required":    arg.Required,
			})
		}
		out = append(out, map[string]any{
			"server":      item.Server,
			"name":        item.Name,
			"description": item.Description,
			"arguments":   args,
		})
	}
	return out
}

// discoveredSkills 发现到的技能。**不因为任何一个读不出来就整体失败**——
// 一个坏技能不该让整份清单消失，那正好是最需要看到清单的时候。
func discoveredSkills(dirs []string) []skills.Skill {
	found := skills.Discover(dirs)
	if found == nil {
		return []skills.Skill{}
	}
	return found
}
