package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/channels/cli"
	"github.com/zjzhang-cn/fka-go/internal/llm"
	"github.com/zjzhang-cn/fka-go/internal/messages"
)

// runAsk 无头跑一轮问答。
//
// ## 它现在只是一层薄壳
//
// 编排（判定 + 跑循环 + 归类）在 `messages.Ask`，呈现（标签 / 答案 / 回显）在
// `cli.Channel`。这里只保留命令行**自己的知识**：把问题从参数里拼出来、展开
// `@引用`、做启动期前置检查、把结果翻成退出码。
func runAsk(ctx context.Context, parsed cliArgs) int {
	question := strings.TrimSpace(strings.Join(parsed.positional, " "))
	if question == "" {
		fmt.Fprintln(os.Stderr, "用法：fka ask [参数] <问题>")
		// 会话那条**把兜底说清楚**：不给就是「每次一个新会话」，
		// 而「接着刚才那条 CLI 问的继续」要显式给 id
		fmt.Fprintf(os.Stderr, "  参数：%s <身份>（%s）、%s <会话>（%s，不给则每次新会话）\n",
			principalFlag, principalEnv, sessionFlag, sessionEnv)
		return exitUsage
	}

	principal := flagOrEnv(parsed, principalFlag, principalEnv, "cli")
	// **显式给了 `--session` / `FKA_SESSION` 才算「要续会话」**：只有那时才读历史、
	// 也只有那时才落盘。不给就是纯一次性问答，不产生（也读不到）会话历史文件——
	// 否则每次 `ask` 都会留下一个永不复用的 `cli-*.jsonl`。
	explicitSession := flagOrEnv(parsed, sessionFlag, sessionEnv, "") != ""
	session := sessionID(parsed)

	// `@路径` 的引用在这里展开：文本正文追加在末尾，图片作为本轮附件。
	// 提示写 stderr——stdout 只该有答案。
	expanded := expandFileRefs(question, os.Stderr)
	question = expanded.text
	if len(expanded.names) > 0 {
		fmt.Fprintf(os.Stderr, "引用文件：%s\n", strings.Join(expanded.names, "、"))
	}

	application := build(ctx)
	defer application.Close()

	application.WarmMcp(ctx)

	if !application.LLMReady {
		fmt.Fprintln(os.Stderr, "没配 LLM_API_KEY / LLM_MODEL，ask 没法跑。")
		fmt.Fprintln(os.Stderr, "（这一条在「有工具」与「无工具」两条路上都需要模型——"+
			"单次问答也要模型把片段写成答案。）")
		return exitFail
	}

	// **先问有没有接上**。以前这里是判指针为 nil，而 nil 说不出原因，于是「LLM_TOOLS=off」
	// 与「没配模型」都落到同一句「没接上模型」里。现在缺席的 runner 自己会答。
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
		// 与消息层同一条规则：**没有任何工具就走单次问答**。
		// CLI 这里没有「回原文片段」的降级（那需要检索，接缝还没搬），
		// 所以直接说清楚缺什么，而不是给一个空答案
		fmt.Fprintln(os.Stderr, "现在没有任何工具被放行，ask 无从跑起。")
		fmt.Fprintln(os.Stderr, "  记忆与文档走 MCP，而 MCP 工具一律是 external 类：")
		fmt.Fprintf(os.Stderr, "  请设 LLM_TOOL_EFFECTS=read,external，并在 %s 里配好服务器。\n",
			application.McpConfigPath)
		fmt.Fprintln(os.Stderr, "  跑 `fka list tool` 看现在放行了什么。")
		return exitFail
	}

	channel := cli.New(cli.Options{
		Mode: cli.ModeAsk, Session: session, Principal: principal, Question: question,
		Out: os.Stdout, UI: os.Stderr,
		Palette: cli.NewPalette(false), HistoryPath: llm.SessionPath,
	})

	// 回调是「跑一轮」：Ask 给出结论，这里翻成退出码与呈现。
	failed := false
	err = channel.Start(ctx, func(in channels.InboundMessage) {
		outcome := application.Messages.Ask(ctx, messages.AskInput{
			Channel: channel, Message: in,
			SessionID: in.ConversationID, Ephemeral: !explicitSession,
			Images: expanded.images,
		})
		if outcome.Cause != nil {
			// 模型的错如实报，**不静默降级**——与原实现同一条理由：
			// 「稍后再试」会把「接口没配好」伪装成「模型偶尔不回」
			fmt.Fprintf(os.Stderr, "问答失败：%s\n", outcome.Cause.Error())
			failed = true
			return
		}

		application.Messages.Deliver(ctx, channel, in, outcome.Text())

		// 用量默认就显示，不设开关。**服务端给了才打**——不给就没有这一行，
		// 绝不用本地估算冒充真实用量（见 llm.TokenUsage）。
		usage := outcome.Result.Usage
		if line := usageLine(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens); line != "" {
			fmt.Fprintf(os.Stderr, "[用量] %s\n", line)
		}

		if debugEnabled() {
			// **会话 id 印出来**，但只在显式给了 `--session` 时才有「续会话」的意义：
			// 没给的话这轮是 Ephemeral，什么都没落盘，拿这个 id 也续不上
			hint := ""
			if !explicitSession {
				hint = "（未落盘；要续会话请显式给 --session）"
			}
			fmt.Fprintf(os.Stderr, "\n[debug] session=%s%s steps=%d stoppedBy=%s tools=%s\n",
				in.ConversationID, hint, outcome.Result.Steps, outcome.Result.StoppedBy,
				strings.Join(outcome.Result.UsedTools, ", "))
		}
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "起问答失败：%s\n", err.Error())
		return exitFail
	}
	if failed {
		return exitFail
	}
	return exitOK
}

// sessionID 这一轮用哪个会话。**三层，从上往下**：`--session` > `FKA_SESSION` >
// **每次新生成一个**。
//
// 生成的实现（前缀 + UUID）在 `internal/channels/cli/session.go`——命令行现在是
// 一个渠道，会话 id 的形状由渠道自己管。
func sessionID(parsed cliArgs) string {
	if given := flagOrEnv(parsed, sessionFlag, sessionEnv, ""); given != "" {
		return given
	}
	return cli.NewSessionID()
}

// ask 自己的三个参数名与环境变量名。**成对放在一起**：它们只有在这一个文件里
// 出现两次（解析时 + 读值时），而两处写错一处就表现为「参数不生效」或
// 「问题里混进参数」——不报错，只是结果不对。
const (
	principalFlag = "--principal"
	principalEnv  = "FKA_PRINCIPAL"
	sessionFlag   = "--session"
	sessionEnv    = "FKA_SESSION"
)

// cliArgs 命令行参数拆解后的结果。**整份 CLI 只有这一种参数形态**——
//
// 每个子命令都拿它、不再自己扫原始 args：「认不认识一个参数」「它算不算问题」
// 这两个判断只能各有一处，散开就会出现「某个参数在某处没被认，于是混进了别的东西」。
type cliArgs struct {
	// command 第一个位置参数，也就是子命令名。
	//
	// **子命令名之前也能给参数**（`fka --log-level debug serve`），所以它和参数
	// 是一起解析的：先认参数，剩下的第一个就是命令。
	command string
	// values 参数值。**给没给与给了什么值都在这里**（布尔参数的值是空串）。
	values map[string]string
	// positional 命令名之后剩下的位置参数。`ask` 拿它拼问题。
	positional []string
}

// parseAskArgs 把 `fka ask` 后面那些参数拆成「参数」与「位置参数」。
//
// ## 这里必须真的解析，而不是 `strings.Join(args, " ")`
//
// 那样拼出来的「问题」是 **`--session aabbcc 你的名字加小航`**——参数直接进了
// 用户提示词。症状特别难认：模型答得挺好，只是**把参数当成问题的一部分**，
// 于是「刚才我说的是啥」这类追问会连着 `--session aabbcc` 一起复述，而
// `bin/data/history/<会话>.jsonl` 里那几条 user 消息就是证据。
// 参数是**给程序的**，问题才是给模型的，两者混在一起没有任何好处。
//
// ## 参数在问题前后都认
//
// `fka ask --session x 问` 与 `fka ask 问 --session x` 都得能用——
// 前一种是正着写，后一种是补参数时顺手敲在后面，只认前者会让第二种**静默失效**。
//
// ## 认不出的参数报用法错，不当问题
//
// `--sesion x`（拼错）如果被当问题，模型会拿到一句莫名其妙的话并**认真回答**。
// 这类失败必须响：报出来是 2 秒的事，静默走过去是「模型今天答得好奇怪」+半天排查。
func parseFlags(args []string) (cliArgs, error) {
	parsed := cliArgs{values: map[string]string{}}
	afterDoubleDash := false

	for i := 0; i < len(args); i++ {
		arg := args[i]

		// `--` 之后一律当问题：问题本身以 `-` 开头时唯一的办法
		if arg == "--" {
			afterDoubleDash = true
			continue
		}
		if afterDoubleDash || !strings.HasPrefix(arg, "-") {
			parsed.positional = append(parsed.positional, arg)
			continue
		}

		name, value, given := strings.Cut(arg, "=")
		if !given {
			// 没有 `=`：值在下一个参数上。**没有下一个就是少写了值**，要报出来
			if !takesValue(name) {
				if isKnownFlag(name) {
					parsed.values[name] = ""
					continue
				}
				return cliArgs{}, unknownFlagError(name)
			}
			if i+1 >= len(args) {
				return cliArgs{}, fmt.Errorf("%s 后面缺值", name)
			}
			i++
			value = args[i]
		}

		if !takesValue(name) {
			// 布尔参数带了值：`--json=1` 这种写法没人会敲，与其猜不如报
			return cliArgs{}, fmt.Errorf("%s 不取值，去掉 =%s", name, value)
		}
		parsed.values[name] = value
	}

	// 第一个位置参数是子命令名：**它不是问题的一部分**
	if len(parsed.positional) > 0 {
		parsed.command, parsed.positional = parsed.positional[0], parsed.positional[1:]
	}
	return parsed, nil
}

// knownFlags 全部已知的参数。**没有值的那些是布尔参数**。
//
// ## 它是**唯一**的一份名单
//
// 之前 `--log-level` 由 `loglevel.go` 自己扫 args、`--principal` / `--session`
// 由 `ask.go` 自己扫，两处各认各的。名单一多就会漏——而漏掉的那个参数会**直接
// 混进用户提示词**，没有任何报错。所以「认不认识」只在这里判一次。
var knownFlags = map[string]bool{
	// 布尔（不取值）
	"--json":     false,
	"--no-color": false,
	// 取一个值
	principalFlag: true,
	sessionFlag:   true,
	logLevelFlag:  true,
	"--account":   true,
}

func takesValue(name string) bool {
	takes, known := knownFlags[name]
	return known && takes
}

// isKnownFlag 这个参数名在名单里吗。**取值与「在不在」要分开问**——
// 前者决定要不要吃掉下一个参数，后者决定认不认。
func isKnownFlag(name string) bool {
	_, known := knownFlags[name]
	return known
}

func unknownFlagError(name string) error {
	message := "认不出的参数：" + name
	// **认得的那个很像它**时把候选说清楚：参数名都是英文，`--sesion` 这种敲错
	// 一眼看不出来，而「你不知道有哪些参数」比「你敲错了」难查得多
	for candidate := range knownFlags {
		if strings.HasPrefix(strings.TrimPrefix(candidate, "-"),
			strings.TrimPrefix(name, "-")) && candidate != name {
			message += "（是不是想写 " + candidate + "？）"
			break
		}
	}
	return errors.New(message + "。可用的是 " + strings.Join(flagNames(), "、"))
}

func flagNames() []string {
	names := make([]string, 0, len(knownFlags))
	for name := range knownFlags {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// flagOrEnv 取参数值，没有则退回环境变量，再没有则用兜底。
//
// **参数已经解析过了**：这里只读 `parsed.values`，不再扫一遍原始 args。
// 两处各扫一遍的坏处是「解析时认了、取值时没认」——表现是参数静默失效。
func flagOrEnv(parsed cliArgs, flag string, env string, fallback string) string {
	if value := parsed.values[flag]; value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv(env)); value != "" {
		return value
	}
	return fallback
}

// hasFlag 这个布尔参数给没给。
func hasFlag(parsed cliArgs, flag string) bool {
	_, given := parsed.values[flag]
	return given
}

func debugEnabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("FKA_DEBUG")))
	return value == "1" || value == "true" || value == "on"
}

// usageLine 把一轮的真实用量渲染成一句话。**纯函数**，ask 与 chat 共用：
//
//	全零      → ""（服务端没给，不打）
//	给了总数  → 「入 X / 出 Y（共 Z）」
//	没给总数  → 「入 X / 出 Y」
//
// 它只拼**核心那段**；前缀（`[用量] ` / `· 用量：`）与颜色由各自的前端加——
// ask 不上色、chat 用暗色。
func usageLine(in, out, total int) string {
	if in == 0 && out == 0 && total == 0 {
		return ""
	}
	line := fmt.Sprintf("入 %d / 出 %d", in, out)
	if total > 0 {
		line += fmt.Sprintf("（共 %d）", total)
	}
	return line
}

func firstLine(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	runes := []rune(text)
	if len(runes) > 60 {
		return string(runes[:60]) + "…"
	}
	return text
}
