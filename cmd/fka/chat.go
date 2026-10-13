// 本文件是 `fka chat`：起一场**多轮**的交互式问答。
//
// ## 代码去哪了
//
// REPL（banner / 逐行读 / 命令族 / 转圈）、回显、调色板、会话 id 都搬进了
// `internal/channels/cli`——命令行现在是**第三个渠道**，与微信、网页走同一条
// 问答路径（`messages.Ask`）。这里只剩命令行自己的知识：启动期前置检查、建行编辑器、
// 把一条消息跑成一轮问答（`chatTurn`）、以及 `@引用` 的展开。
//
// ## stdout 与 stderr 的分工（这是本仓的不变量）
//
// 回答是**结果**，只进 stdout；提示符、角色标签、工具回显、错误、帮助全部进 stderr。
// 所以 `fka chat < 提问.txt > 回答.txt` 里那份文件是一串干净的回答。这一条现在由
// `cli.Channel` 的 `say` 与回显实现守住（见 cli 包）。
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ergochat/readline"

	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/channels/cli"
	"github.com/zjzhang-cn/fka-go/internal/llm"
	"github.com/zjzhang-cn/fka-go/internal/messages"
)

// runChat 起一场交互式多轮问答。
//
// 前置检查与 `ask` **逐条一致**（没配模型 / 工具循环关了 / 一个工具都没放行各自
// 说清缺什么）。不一致的话会出现「ask 报得出原因、chat 只回一句没接上」这种
// 同一个环境两种诊断的分叉。
func runChat(ctx context.Context, parsed cliArgs) int {
	application := build(ctx)
	defer application.Close()

	application.WarmMcp(ctx)

	if !application.LLMReady {
		fmt.Fprintln(os.Stderr, "没配 LLM_API_KEY / LLM_MODEL，chat 没法跑。")
		return exitFail
	}
	if !application.Agent.Enabled() {
		fmt.Fprintln(os.Stderr, "工具循环未启用（LLM_TOOLS=off）。")
		return exitFail
	}

	hasTools, err := application.Agent.HasTools(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "列工具失败：%s\n", err.Error())
		return exitFail
	}
	if !hasTools {
		fmt.Fprintln(os.Stderr, "现在没有任何工具被放行，chat 无从跑起。")
		fmt.Fprintf(os.Stderr, "  请设 LLM_TOOL_EFFECTS=read,external，并在 %s 里配好服务器。\n",
			application.McpConfigPath)
		fmt.Fprintln(os.Stderr, "  跑 `fka list tool` 看现在放行了什么。")
		return exitFail
	}

	colors := cli.NewPalette(cli.ShouldColor(os.Stderr, hasFlag(parsed, "--no-color")))

	// **历史不落盘时多轮是假的**：每轮 `LoadHistoryPrefix` 都拿不到东西，于是
	// 「接着上一句说」变成一句空话，而界面上完全看不出来。所以这里必须响。
	if application.History == nil {
		fmt.Fprintln(os.Stderr, colors.Err("SESSION_HISTORY=0：历史不落盘，"+
			"这个会话带不上上一轮。要真正的多轮请去掉它（默认就是开的）。"))
	}

	// 只有**读写两侧都是真终端**才接管行编辑：管道/重定向（含 `fka chat < 问.txt`）
	// 与测试继续走 bufio 整行读，那条路一字不变。提示符与回显都指向 stderr，
	// 所以 stderr 不是终端时也不接管。
	var editor cli.LineReader
	if cli.IsCharDevice(os.Stdin) && cli.IsCharDevice(os.Stderr) {
		if rl, err := readline.NewEx(&readline.Config{
			Prompt:          colors.User("> "),
			Stdin:           os.Stdin,
			Stdout:          os.Stderr,
			Stderr:          os.Stderr,
			InterruptPrompt: "\n",
			EOFPrompt:       "\n",
		}); err == nil {
			editor = rl
			defer rl.Close()
		} else {
			fmt.Fprintln(os.Stderr, colors.Dim("行编辑没起来，退回最简输入："+err.Error()))
		}
	}

	spinner := cli.IsCharDevice(os.Stderr) && cli.IsCharDevice(os.Stdin)
	channel := cli.New(cli.Options{
		Mode:        cli.ModeChat,
		Session:     sessionID(parsed),
		Principal:   flagOrEnv(parsed, principalFlag, principalEnv, "cli"),
		In:          os.Stdin,
		Out:         os.Stdout,
		UI:          os.Stderr,
		Editor:      editor,
		Palette:     colors,
		Spinner:     spinner,
		Lister:      application.Tools,
		HistoryPath: llm.SessionPath,
	})

	if err := channel.Start(ctx, func(in channels.InboundMessage) {
		chatTurn(ctx, application.Messages, channel, in, os.Stderr, colors, spinner)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "起聊天失败：%s\n", err.Error())
		return exitFail
	}
	return exitOK
}

// chatTurn 把一条入站消息跑成一轮问答并呈现。
//
// 顺序是硬要求：**先展开 `@引用`（本轮问题的一部分）→ 起转圈 → 跑 Ask（过程回显
// 落在转圈下面）→ 停转圈 → 呈现结果**。标签与答案由渠道的 `say`（经 `Deliver`）
// 写出去，答案只进 stdout。
//
// 它抽成纯函数是为了能在 cmd 侧单独钉「`@引用` 进模型问题」这条接线——REPL 的
// 行为在 cli 包里验。
func chatTurn(ctx context.Context, handler *messages.Handler, channel channels.Channel,
	in channels.InboundMessage, ui io.Writer, pal cli.Palette, spinner bool) {
	// `@路径` 的引用在这里展开：文本正文追加在问题末尾，图片作为本轮附件；另起一行
	// 提示引了哪些。展开放在转圈之前——读文件是本地的、快的，不必占着「思考中」那一行。
	expanded := expandFileRefs(in.Text(), ui)
	in.Parts = []channels.Part{channels.TextPart(expanded.text)}
	if len(expanded.names) > 0 {
		fmt.Fprintf(ui, "%s\n", pal.Tool("· 引用文件："+strings.Join(expanded.names, "、")))
	}

	stop := cli.StartSpinner(ui, pal, spinner)
	outcome := handler.Ask(ctx, messages.AskInput{
		Channel: channel, Message: in, SessionID: in.ConversationID,
		Images: expanded.images,
	})
	stop()

	if outcome.Cause != nil {
		// 被 Ctrl-C 掐断的：舞一句错误就走，别诱导用户再问一轮
		if ctx.Err() != nil {
			fmt.Fprintln(ui, pal.Dim("已中断。"))
			return
		}
		// 模型的错**如实报**，不静默降级——与 ask 同一条理由
		fmt.Fprintln(ui, pal.Err("问答失败："+outcome.Cause.Error()))
		return
	}

	handler.Deliver(ctx, channel, in, outcome.Text())

	// 用量默认就显示，不设开关；服务端给了才打。放在答复之后、debug 之前。
	usage := outcome.Result.Usage
	if line := usageLine(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens); line != "" {
		fmt.Fprintf(ui, "%s\n", pal.Dim("· 用量："+line))
	}

	if debugEnabled() {
		fmt.Fprintf(ui, "%s\n", pal.Dim(fmt.Sprintf(
			"[debug] session=%s steps=%d stoppedBy=%s", in.ConversationID,
			outcome.Result.Steps, outcome.Result.StoppedBy)))
	}
	fmt.Fprintln(ui)
}
