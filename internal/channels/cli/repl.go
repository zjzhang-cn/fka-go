// 本文件是 chat 模式的 **REPL**：banner、逐行读、斜杠命令族、转圈。
//
// ## stdout 与 stderr 的分工（本仓不变量）
//
// 回答（答复正文）是**结果**，只进 Out（stdout）；提示符、角色标签、工具回显、
// 错误、帮助全部进 UI（stderr）。所以 `fka chat < 提问.txt > 回答.txt` 里那份文件
// 是一串干净的回答，而终端上照样看得到带色的交互。
//
// ## 为什么行编辑要单独接管
//
// 行编辑必须自己接管，否则中文回退会残留：不接管时回退键由内核 canonical 模式
// 处理，而内核擦除一个字符只回显 `\b \b`（**一列**），双列宽的汉字于是被擦掉一半。
// 真终端上用 `github.com/ergochat/readline`（纯 Go、CJK 宽度按 `x/text/width` 算）。
// 它由 `cmd/fka` 建好递进来（提示符与上色是命令行的排版决定），这里只在拿到它时
// 走它、否则回落到 bufio 整行读。
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// loop 是 REPL 主体。读取一行 → 命令或问题。**Start 返回时即会话结束**。
func (c *Channel) loop(ctx context.Context, onMessage func(channels.InboundMessage)) error {
	c.banner()

	for {
		// Ctrl-C：外部的 signal ctx 已取消。把它当**干净退出**而不是错误——
		// 用户按 Ctrl-C 就是想走，再回一句红字没有意义。
		if ctx.Err() != nil {
			c.bye()
			return nil
		}

		line, err := c.readLine()
		if err != nil {
			// EOF（Ctrl-D）与 readline 的 Ctrl-C 都是正常结束，不是错
			c.bye()
			return nil
		}

		text := trimQuestion(line)
		if text == "" {
			continue
		}

		if strings.HasPrefix(text, "/") {
			if done := c.command(ctx, text); done {
				return nil
			}
			continue
		}

		onMessage(c.message(text))
	}
}

// banner 开场白。**只有它出现在第一行**，所以把「会话 id 与历史文件」一起说了
// ——「我上一句哪去了」是 chat 最常被问的问题，答案就在那两行里。
func (c *Channel) banner() {
	fmt.Fprintf(c.ui, "%s %s\n", c.palette.Assistant("fka chat"), c.palette.Dim("多轮工具循环问答"))
	if c.historyPath != nil {
		fmt.Fprintf(c.ui, "%s\n", c.palette.Dim("会话 "+c.session+"　历史 "+c.historyPath(c.session)))
	} else {
		fmt.Fprintf(c.ui, "%s\n", c.palette.Dim("会话 "+c.session))
	}
	fmt.Fprintf(c.ui, "%s\n", c.palette.Dim("/help 看命令，/new 重开一个会话，Ctrl-D 退出。"))
	fmt.Fprintln(c.ui)
}

func (c *Channel) bye() {
	fmt.Fprintln(c.ui)
	fmt.Fprintln(c.ui, c.palette.Dim("再见。"))
}

// prompt 提问符。**只在没有行编辑器时用**：有 readline 时提示符由它自己写，
// 还会随输入一起重绘，这里再写一遍就重复了。
func (c *Channel) prompt() {
	fmt.Fprint(c.ui, c.palette.User("> "))
}

// readLine 读一行去掉行尾。EOF 与真错误分两种。
//
// 有行编辑器时整行交给它（它自己写提示符、自己上色、按 CJK 宽度重绘）；否则走
// bufio，末行没有换行（`printf '问' | fka chat`）时 `ReadString` 会同时给出内容
// 与 `io.EOF`——**那一行不能丢**，下一次读才是干净的 EOF。
func (c *Channel) readLine() (string, error) {
	if c.editor != nil {
		return c.editor.Readline()
	}
	c.prompt()
	line, err := c.in.ReadString('\n')
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
func (c *Channel) command(ctx context.Context, line string) (done bool) {
	name, _ := splitSlash(line)
	switch name {
	case "/quit", "/exit", "/q":
		c.bye()
		return true
	case "/help", "/h", "/?":
		c.help()
		return false
	case "/new":
		c.session = NewSessionID()
		fmt.Fprintf(c.ui, "%s %s\n", c.palette.Assistant("新会话"), c.palette.Dim(c.session))
		return false
	case "/session":
		if c.historyPath != nil {
			fmt.Fprintf(c.ui, "%s\n", c.palette.Dim("会话 "+c.session+"　历史 "+c.historyPath(c.session)))
		} else {
			fmt.Fprintf(c.ui, "%s\n", c.palette.Dim("会话 "+c.session))
		}
		return false
	case "/tools":
		c.showTools(ctx)
		return false
	default:
		// 认不出的命令**不能当问题发出去**：`/sesion`（拼错）会让模型认真回答一个
		// 莫名其妙的字符串。这条规矩与 `parseFlags` 对未知参数的处理是同一条。
		fmt.Fprintf(c.ui, "%s\n", c.palette.Err("认不出的命令："+name+"（/help 看全部）"))
		return false
	}
}

func (c *Channel) showTools(ctx context.Context) {
	if c.lister == nil {
		fmt.Fprintln(c.ui, c.palette.Dim("（没有注册表可查）"))
		return
	}
	listed, err := c.lister.Tools(ctx, tools.Context{})
	if err != nil {
		fmt.Fprintln(c.ui, c.palette.Err("列工具失败："+err.Error()))
		return
	}
	if len(listed) == 0 {
		fmt.Fprintln(c.ui, c.palette.Dim("模型现在看不到任何工具。"))
		return
	}
	for _, tool := range listed {
		fmt.Fprintf(c.ui, "%s %s %s\n",
			c.palette.Tool(fmt.Sprintf("%-40s", tool.FullName)),
			c.palette.Dim(fmt.Sprintf("%-9s", tool.Spec.Effect)),
			firstLine(tool.Spec.Description))
	}
}

func (c *Channel) help() {
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
		fmt.Fprintf(c.ui, "%s\n", c.palette.Dim(line))
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

// StartSpinner 在等待模型时原地转圈，返回一个「停下并擦掉这一行」的函数。
//
// 关闭时返回的是个空操作，**不是 nil**——调用点就能无条件 `defer stop()` 而不必
// 再判一次开关，少一处会漏的分支。
//
// 擦除用 `\r\x1b[K`（回车 + 清到行尾）而不是补空格：转圈宽度会变，
// 补的空格数算错就留下残渣。
func StartSpinner(w io.Writer, p Palette, enabled bool) func() {
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
				fmt.Fprintf(w, "\r\x1b[K%s", p.Dim(frames[index%len(frames)]+" 思考中…"))
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
