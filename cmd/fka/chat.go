// 本文件是 `fka chat`：一个**多轮**的交互式问答循环，参照 `ask`，区别只在「会话续着走」。
//
// ## 与 ask 的唯一区别就是会话
//
// `ask` 没给 `--session` 时每次都新生成一个 id，所以它是一次性的；`chat` 整场复用
// 同一个 id，于是 `Runner` 每轮读到的历史前缀就是上一轮的对话，收尾再追加回去
// （见 `agent/loop` 的 `persistTurn` 与 `internal/llm/history`）。除了这一点，两者
// 走的是同一套装配、同一个模型、同一条工具链。
//
// ## stdout 与 stderr 的分工（这是本仓的不变量）
//
// 回答（`result.Text`）是**结果**，只进 stdout；提示符、角色标签、「用了工具」、
// 错误、帮助全部进 stderr。所以 `fka chat < 提问.txt > 回答.txt` 里那份文件是一串
// 干净的回答，而终端上照样看得到带色的交互。这与 `make smoke` 钉的
// 「CLI 的 stdout 只有结果」是同一条规矩——把标签混进 stdout，重定向后再 grep
// 就分不清哪一行是模型说的、哪一行是我们加的。
//
// ## 颜色为什么是手写的 ANSI
//
// 上色本身不值得引一门框架。默认**只在真终端上上色**：管道、重定向、`NO_COLOR`、
// `TERM=dumb` 都自动关掉——给一个文件写 ANSI 转义是把文件弄脏，不是让输出好看。
//
// ## 为什么单独引了一个行编辑库
//
// **行编辑必须自己接管**，否则中文回退会残留：不接管时回退键由内核 canonical 模式
// 处理，而内核擦除一个字符只回显 `\b \b`（**一列**），双列宽的汉字于是被擦掉一半、
// 另一半留在屏幕上（缓冲里其实整字已删）。没有任何 termios 开关能让 canonical 的回显
// 按显示宽度擦除（`IUTF8` 只按字符而非按字节删除，回显仍是 `\b \b`）。所以真终端上用
// `github.com/ergochat/readline` 接管：纯 Go、CJK 宽度按 `x/text/width` 算、跨平台。
//
// 它是**行编辑库，不是全屏 TUI**：提示符与回显仍写 stderr，回答仍只写 stdout，
// `fka chat < 问.txt > 答.txt` 照旧是一串干净回答。管道/重定向与测试不经过它，
// 走下面的 bufio 整行读，行为一字不变。
//
// ## 为什么没有逐字流式回显
//
// `ChatClient` 只交出拼好的整段 `ChatResult`（`internal/llm/openai` 内部虽是流式，
// 但不把 token 往外吐）。所以这里只能「思考中…」然后整段出现。要做真流式，得先改
// 模型契约；那是另一件事。
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ergochat/readline"

	"github.com/zjzhang-cn/fka-go/internal/agent"
	"github.com/zjzhang-cn/fka-go/internal/llm"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// chatEngine 是 chat 循环用到的那部分工具循环。
//
// **接口声明在消费者这一侧**，宽度就一个方法：`ask` 与 `messages` 用的是同一个
// `*agent.Runner`，可替换成本全在这行签名上。测试注入假引擎走的就是它。
type chatEngine interface {
	Run(ctx context.Context, input agent.RunnerInput) (agent.RunResult, error)
}

// chatLineReader 是 REPL 那一行输入的来源：真终端上是 `*readline.Instance`，管道与
// 测试下为 nil（走 bufio）。**接口声明在消费者这一侧**，与 chatEngine 同一条规矩——
// 测试注入假 reader 走的就是它。
type chatLineReader interface {
	Readline() (string, error)
	Close() error
}

// chatToolLister 是 `/tools` 需要的那部分注册表。`tools.Service` 满足它。
type chatToolLister interface {
	Tools(ctx context.Context, tc tools.Context) ([]tools.RegisteredTool, error)
}

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
		fmt.Fprintln(os.Stderr, "  跑 `fka tools` 看现在放行了什么。")
		return exitFail
	}

	colors := palette{enabled: shouldColor(os.Stderr, hasFlag(parsed, "--no-color"))}

	// **历史不落盘时多轮是假的**：每轮 `LoadHistoryPrefix` 都拿不到东西，于是
	// 「接着上一句说」变成一句空话，而界面上完全看不出来。所以这里必须响。
	if application.History == nil {
		fmt.Fprintln(os.Stderr, colors.err("SESSION_HISTORY=0：历史不落盘，"+
			"这个会话带不上上一轮。要真正的多轮请去掉它（默认就是开的）。"))
	}

	repl := &chatREPL{
		engine:    application.Agent,
		lister:    application.Tools,
		in:        bufio.NewReader(os.Stdin),
		out:       os.Stdout,
		ui:        os.Stderr,
		pal:       colors,
		spinner:   isCharDevice(os.Stderr) && isCharDevice(os.Stdin),
		debug:     debugEnabled(),
		principal: flagOrEnv(parsed, principalFlag, principalEnv, "cli"),
		session:   sessionID(parsed),
	}

	// 只有**读写两侧都是真终端**才接管行编辑：管道/重定向（含 `fka chat < 问.txt`、
	// `fka chat 2>err.txt`）与测试继续走 bufio 整行读，那条路一字不变。回显写 stderr，
	// 所以 stderr 不是终端时也不接管——否则 readline 会被判成非交互、连回显都没有，
	// 而 canonical 模式的内核回显本来还能看见。提示符与回显都指向 stderr，stdout 仍只收
	// 回答；历史留在内存里（HistoryFile 留空 → 不落盘），本次会话内 ↑ 能翻回上一句。
	if isCharDevice(os.Stdin) && isCharDevice(os.Stderr) {
		if rl, err := readline.NewEx(&readline.Config{
			Prompt:          colors.user("> "),
			Stdin:           os.Stdin,
			Stdout:          os.Stderr,
			Stderr:          os.Stderr,
			InterruptPrompt: "\n",
			EOFPrompt:       "\n",
		}); err == nil {
			repl.rl = rl
			defer rl.Close()
		} else {
			fmt.Fprintln(os.Stderr, colors.dim("行编辑没起来，退回最简输入："+err.Error()))
		}
	}

	return repl.loop(ctx)
}

// chatREPL 一场会话的全部状态。
//
// `out` 与 `ui` **刻意分开**：前者只收回答，后者收一切给人看的提示。合成一个
// writer 之后「stdout 只有结果」就守不住了，而那条不变量正是 `--json` / `version`
// 那些闸门在钉的东西。
type chatREPL struct {
	engine chatEngine
	lister chatToolLister
	in     *bufio.Reader
	out    io.Writer
	ui     io.Writer
	pal    palette
	// rl 非 nil 时接管一行输入（真终端 + readline 起得来），否则走 in 的 bufio 整行读。
	// **两条路只走一条**：用 rl 时提示符由它自己写，免得与 prompt() 重复。
	rl chatLineReader
	// spinner 只在**两侧都是真终端**时为真。管道下转圈会把日志弄脏，而「思考中」
	// 对脚本没有任何意义。
	spinner bool
	// debug 输出会话/步数等排查信息（FKA_DEBUG=1）。与 `ask` 同一个开关。
	debug     bool
	principal string
	session   string
}

// loop 是 REPL 主体。返回退出码。
func (r *chatREPL) loop(ctx context.Context) int {
	r.banner()

	for {
		// Ctrl-C：`run` 里的 signal ctx 已取消。把它当**干净退出**而不是错误——
		// 用户按 Ctrl-C 就是想走，再回一句红字没有意义。
		if ctx.Err() != nil {
			r.bye()
			return exitOK
		}

		line, err := r.readLine()
		if err != nil {
			// EOF（Ctrl-D）与 readline 的 Ctrl-C 都是正常结束，不是错
			r.bye()
			return exitOK
		}

		text := strings.TrimSpace(line)
		if text == "" {
			continue
		}

		if strings.HasPrefix(text, "/") {
			if done, code := r.command(ctx, text); done {
				return code
			}
			continue
		}

		// **一轮失败不带走整场会话**：模型接口偶发、单条问题太长，都不该让用户
		// 从头再来。错误已经打印过，这里只继续读下一行。
		r.ask(ctx, text)
	}
}

// banner 开场白。**只有它出现在第一行**，所以把「会话 id 与历史文件」一起说了
// ——「我上一句哪去了」是 chat 最常被问的问题，答案就在那两行里。
func (r *chatREPL) banner() {
	fmt.Fprintf(r.ui, "%s %s\n", r.pal.assistant("fka chat"), r.pal.dim("多轮工具循环问答"))
	fmt.Fprintf(r.ui, "%s\n", r.pal.dim("会话 "+r.session+"　历史 "+llm.SessionPath(r.session, "")))
	fmt.Fprintf(r.ui, "%s\n", r.pal.dim("/help 看命令，/new 重开一个会话，Ctrl-D 退出。"))
	fmt.Fprintln(r.ui)
}

func (r *chatREPL) bye() {
	fmt.Fprintln(r.ui)
	fmt.Fprintln(r.ui, r.pal.dim("再见。"))
}

// prompt 提问符。**只在没有行编辑器时用**：有 readline 时提示符由它自己写，还会
// 随输入一起重绘，这里再写一遍就重复了。
func (r *chatREPL) prompt() {
	fmt.Fprint(r.ui, r.pal.user("> "))
}

// readLine 读一行去掉行尾。EOF 与真错误分两种。
//
// 有行编辑器时整行交给它（它自己写提示符、自己上色、按 CJK 宽度重绘）；否则走 bufio，
// 末行没有换行（`printf '问' | fka chat`）时 `ReadString` 会同时给出内容与 `io.EOF`
// ——**那一行不能丢**，下一次读才是干净的 EOF。
func (r *chatREPL) readLine() (string, error) {
	if r.rl != nil {
		return r.rl.Readline()
	}
	r.prompt()
	line, err := r.in.ReadString('\n')
	text := strings.TrimRight(line, "\r\n")
	switch {
	case err == nil:
		return text, nil
	case errors.Is(err, io.EOF) && text != "":
		return text, nil
	default:
		return text, err
	}
}

// command 处理一条 `/` 开头的行。done=true 表示该结束整场会话。
func (r *chatREPL) command(ctx context.Context, line string) (done bool, code int) {
	name, _ := splitSlash(line)
	switch name {
	case "/quit", "/exit", "/q":
		r.bye()
		return true, exitOK
	case "/help", "/h", "/?":
		r.help()
		return false, exitOK
	case "/new":
		r.session = newCliSessionID()
		fmt.Fprintf(r.ui, "%s %s\n", r.pal.assistant("新会话"), r.pal.dim(r.session))
		return false, exitOK
	case "/session":
		fmt.Fprintf(r.ui, "%s\n", r.pal.dim("会话 "+r.session+"　历史 "+llm.SessionPath(r.session, "")))
		return false, exitOK
	case "/tools":
		r.showTools(ctx)
		return false, exitOK
	default:
		// 认不出的命令**不能当问题发出去**：`/sesion`（拼错）会让模型认真回答一个
		// 莫名其妙的字符串。这条规矩与 `parseFlags` 对未知参数的处理是同一条。
		fmt.Fprintf(r.ui, "%s\n", r.pal.err("认不出的命令："+name+"（/help 看全部）"))
		return false, exitOK
	}
}

// ask 问一轮并把回答写出去。
//
// 顺序是硬要求：**先停转圈，再写角色标签，最后写回答**。标签与回答分处两个
// writer（ui / out），转圈还占着 ui 那一行时写标签会叠字。
func (r *chatREPL) ask(ctx context.Context, question string) {
	// `@路径` 的引用在这里展开：文本正文追加在问题末尾，图片作为本轮附件；另起一行
	// 提示引了哪些。展开放在转圈之前——读文件是本地的、快的，不必占着「思考中」那一行。
	expanded := expandFileRefs(question, r.ui)
	question = expanded.text
	if len(expanded.names) > 0 {
		fmt.Fprintf(r.ui, "%s\n", r.pal.tool("· 引用文件："+strings.Join(expanded.names, "、")))
	}

	// 回显：工具行与「● 助手 + 答案」都由 emitter 写出（见 chatEmitter）。
	// 与转圈同属「给人看」的一侧，答案仍只进 stdout。
	emitter := &chatEmitter{ui: r.ui, out: r.out, pal: r.pal}

	stop := startSpinner(r.ui, r.pal, r.spinner)

	result, err := r.engine.Run(ctx, agent.RunnerInput{
		SessionID:   r.session,
		PrincipalID: r.principal,
		Question:    question,
		Images:      expanded.images,
		Emitter:     emitter,
	})
	stop()

	if err != nil {
		// 被 Ctrl-C 掐断的：舞一句错误就走，别诱导用户再问一轮
		if ctx.Err() != nil {
			fmt.Fprintln(r.ui, r.pal.dim("已中断。"))
			return
		}
		// 模型的错**如实报**，不静默降级——与 ask 同一条理由
		fmt.Fprintln(r.ui, r.pal.err("问答失败："+err.Error()))
		return
	}

	if r.debug {
		fmt.Fprintf(r.ui, "%s\n", r.pal.dim(fmt.Sprintf(
			"[debug] session=%s steps=%d stoppedBy=%s", r.session, result.Steps, result.StoppedBy)))
	}
	fmt.Fprintln(r.ui)
}

// chatEmitter 是 chat 的「回显」实现：把工具循环推来的事件画成带色的一行行。
// **只写 ui（stderr）；答案写 out（stdout）**——见文件头那条不变量。
//
// ## 为什么要逐条工具行，而不是只报「用了哪些」
//
// 只报名字回答不了「它到底查到了什么」——而工具循环出问题时，人第一个想看的正是
// 这一步的输入与输出。所以每次调用给一行：名字 + 原样参数 + 结果。三者都截断：
// 结果动辄上千字，全量铺开会把对话淹掉（完整内容仍在 transcript 与会话历史里）。
// 参数保持原样字符串、不解析——解析再序列化会改变字节序（见 llm.ToolCall）。
type chatEmitter struct {
	ui  io.Writer
	out io.Writer
	pal palette

	reasoningStarted bool
}

// Reasoning 推理增量。首块打一次 `[推理] ` 前缀，之后逐块原样；流结束会来一个换行
// 把这一行收掉（见 openai 的 reasoningSink）。
func (e *chatEmitter) Reasoning(text string) {
	if text == "" {
		return
	}
	if !e.reasoningStarted {
		fmt.Fprint(e.ui, e.pal.dim("[推理] "))
		e.reasoningStarted = true
	}
	fmt.Fprint(e.ui, e.pal.dim(text))
}

func (e *chatEmitter) Tool(event agent.ToolEvent) {
	call := e.pal.tool(event.Name)
	if args := firstLine(event.Arguments); args != "" {
		call += e.pal.dim("（" + args + "）")
	}
	fmt.Fprintf(e.ui, "%s %s %s\n", e.pal.tool("·"), call, e.pal.dim("→ "+firstLine(event.Result)))
}

// Answer 最终答案：角色标签进 ui，答案进 out。
func (e *chatEmitter) Answer(text string) {
	fmt.Fprintln(e.ui, e.pal.assistant("● 助手"))
	fmt.Fprintln(e.out, text)
}

func (r *chatREPL) showTools(ctx context.Context) {
	if r.lister == nil {
		fmt.Fprintln(r.ui, r.pal.dim("（没有注册表可查）"))
		return
	}
	listed, err := r.lister.Tools(ctx, tools.Context{})
	if err != nil {
		fmt.Fprintln(r.ui, r.pal.err("列工具失败："+err.Error()))
		return
	}
	if len(listed) == 0 {
		fmt.Fprintln(r.ui, r.pal.dim("模型现在看不到任何工具。"))
		return
	}
	for _, tool := range listed {
		fmt.Fprintf(r.ui, "%s %s %s\n",
			r.pal.tool(fmt.Sprintf("%-40s", tool.FullName)),
			r.pal.dim(fmt.Sprintf("%-9s", tool.Spec.Effect)),
			firstLine(tool.Spec.Description))
	}
}

func (r *chatREPL) help() {
	lines := []string{
		"/help     这份帮助",
		"/new      重开一个会话（换一个 session id 与历史文件）",
		"/session  显示当前会话 id 与历史文件路径",
		"/tools    列出模型现在能看到的工具",
		"/quit     退出（Ctrl-D 同效）",
		"",
		"直接输入问题就是问一轮；本会话的每一轮都带着上一轮的上下文。",
		"问题里可用 @路径 引用本地文件：文本附正文、图片作为附件发给模型、其它二进制只附类型与大小。",
		"含空格或中文写 @\"路径\"。图片只在本轮发送，下一轮追问请重新引用。",
		"回答写 stdout，提示与标签写 stderr——重定向时那份文件是一串干净的回答。",
	}
	for _, line := range lines {
		fmt.Fprintf(r.ui, "%s\n", r.pal.dim(line))
	}
}

// splitSlash 把一个 `/` 开头的行拆成命令名与剩下的参数。
//
// 命令名是**第一个空格之前**的全部；参数保留原样（含空格），这样将来的
// `/save 我的问题` 这类带空格参数不会被吃掉后半截。
func splitSlash(line string) (name, arg string) {
	trimmed := strings.TrimSpace(line)
	if index := strings.IndexAny(trimmed, " \t"); index >= 0 {
		return trimmed[:index], strings.TrimSpace(trimmed[index+1:])
	}
	return trimmed, ""
}

// ── 颜色 ──────────────────────────────────────────────

// palette 只有「开/关」一个状态。**关掉时每个方法都原样返回**，所以调用点不必到处
// 判「现在有没有颜色」——那种判散开就一定会漏一处，漏的那处会在重定向的输出里留下
// 一串 `\x1b[0m`。
type palette struct{ enabled bool }

const (
	ansiReset  = "\x1b[0m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[96m"
	ansiDim    = "\x1b[2m"
)

func (p palette) paint(code, s string) string {
	if !p.enabled {
		return s
	}
	return code + s + ansiReset
}

func (p palette) user(s string) string      { return p.paint(ansiCyan, s) }
func (p palette) assistant(s string) string { return p.paint(ansiGreen, s) }
func (p palette) tool(s string) string      { return p.paint(ansiYellow, s) }
func (p palette) err(s string) string       { return p.paint(ansiRed, s) }
func (p palette) dim(s string) string       { return p.paint(ansiDim, s) }

// shouldColor 这一路输出上不上色。**从上往下第一条命中的说了算**：
//
//	--no-color 参数   → 关（用户当场说不要）
//	FKA_COLOR=always  → 开（演示/截图；也是测试唯一能强制开的口子）
//	FKA_COLOR=never   → 关
//	NO_COLOR 已设置   → 关（这是那个约定的语义：**只要存在**就关，值无关）
//	TERM=dumb         → 关
//	不是字符设备      → 关（管道/重定向/文件）
//
// 顺序有意：参数优先于环境变量，环境变量优先于自动探测。反过来会让
// `NO_COLOR=1 fka chat --no-color` 之类的组合出现「谁说了算」的不确定。
func shouldColor(w *os.File, noColorFlag bool) bool {
	if noColorFlag {
		return false
	}
	if value := strings.ToLower(strings.TrimSpace(os.Getenv("FKA_COLOR"))); value != "" {
		switch value {
		case "always", "1", "true", "on", "yes":
			return true
		case "never", "0", "false", "off", "no":
			return false
		}
	}
	if _, present := os.LookupEnv("NO_COLOR"); present {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb") {
		return false
	}
	return isCharDevice(w)
}

// isCharDevice 这个文件是不是终端。**不引 isatty 库**：它已经作为 MCP SDK 的间接
// 依赖躺在 go.sum 里，用它就会平白多一个直接依赖，而这一行 `Stat` 就是全部所需
// （真正需要终端能力的行编辑已经交给 readline，见文件头）。
//
// 字符设备只是**近似**（`/dev/null` 也是字符设备），但对「要不要上色」这个用途，
// 近似的两个方向代价都很小：真终端被误判成不是 → 少点颜色；反之 → 多几个转义，
// 而 `NO_COLOR` / `--no-color` 都能救。精确判定需要 ioctl，不值得。
func isCharDevice(w *os.File) bool {
	if w == nil {
		return false
	}
	info, err := w.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// ── 转圈 ──────────────────────────────────────────────

// startSpinner 在等待模型时原地转圈，返回一个「停下并擦掉这一行」的函数。
//
// 关闭时返回的是个空操作，**不是 nil**——调用点就能无条件 `defer stop()` 而不必
// 再判一次开关，少一处会漏的分支。
//
// 擦除用 `\r\x1b[K`（回车 + 清到行尾）而不是补空格：转圈宽度会变，
// 补的空格数算错就留下残渣。
func startSpinner(w io.Writer, p palette, enabled bool) func() {
	if !enabled {
		return func() {}
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		frames := [...]string{"|", "/", "-", "\\"}
		ticker := time.NewTicker(90 * time.Millisecond)
		defer ticker.Stop()
		index := 0
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fmt.Fprintf(w, "\r\x1b[K%s", p.dim(frames[index%len(frames)]+" 思考中…"))
				index++
			}
		}
	}()

	return func() {
		close(done)
		wg.Wait()
		fmt.Fprint(w, "\r\x1b[K")
	}
}
