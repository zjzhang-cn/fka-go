// Package cli 把命令行做成**第三个渠道**：`fka ask` / `fka chat` 与微信、网页
// 走同一条问答路径（`messages.Ask`）。
//
// ## 为什么命令行也算一个渠道
//
// 「加一个渠道 = 一个 provider + 装配一行、业务层零改」这条判据，以前对 CLI 并不
// 成立——CLI 绕开接缝长大，把判定、回显、会话策略各写了一遍。做成渠道之后，
// 同一个问题在四个前端得到同一套规则；`internal/messages` 与 `internal/agent`
// 不为 CLI 增删任何渠道专属逻辑。
//
// ## 它**不**实现 `channels.Provider`
//
// provider 那条契约是「一个种类、多个账号、可注册的实例工厂」，而命令行没有账号
// 维度、也没有可注册的实例工厂。所以它只实现 `channels.Channel`，由 `cmd/fka`
// 自己持有生命周期——`serve.go` 一行不用改，CLI 也不进 `Service.StartAll`。
//
// ## 答案通道唯一
//
// 答复一律经 `Senders().Text`（`say`）发出：标签进 UI（stderr），正文进 Out
// （stdout）。回显（推理 / 工具行）走 `EmitterProvider`，其中 `Answer` 是空的
// ——答案若也从回显写，就会写两份。
package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// ID / Label 渠道的身份标识。ID 会写进 `InboundMessage.ChannelID`。
const (
	ID    = "cli"
	Label = "命令行"
	// accountLocal 命令行没有账号维度，固定一个「本地」账号——`ValidateChannel`
	// 要求账号非空，而 `渠道:账号:会话` 这条会话键也缺不了它。
	accountLocal = "local"
)

// Mode 命令行这一场是「一问就走」还是「多轮 REPL」。
type Mode int

const (
	// ModeAsk 跑完 Options.Question 就走：不读 stdin、不画 banner。
	ModeAsk Mode = iota
	// ModeChat 进 REPL：banner、逐行读、命令族。
	ModeChat
)

// LineReader 一行输入的来源（真终端上是 `*readline.Instance`）。**接口声明在
// 消费者这一侧**——测试能注入假的，管道与重定向下为 nil，走 bufio 整行读。
type LineReader interface {
	Readline() (string, error)
	Close() error
}

// ToolLister 是 `/tools` 需要的那部分注册表。`tools.Service` 满足它。
type ToolLister interface {
	Tools(ctx context.Context, tc tools.Context) ([]tools.RegisteredTool, error)
}

// Options 造一个命令行渠道的全部材料。**装配在 `cmd/fka`**：展开 `@引用`、决定
// 退出码、建行编辑器都是命令行的知识，渠道不该替它决定。
type Options struct {
	Mode      Mode
	Session   string
	Principal string
	// Question ModeAsk 的问题（已展开 `@引用`）
	Question string
	// In 逐行输入（ModeChat）。行编辑器缺席时走它
	In io.Reader
	// Out 答案正文的落点（stdout）
	Out io.Writer
	// UI 提示、标签、回显的落点（stderr）
	UI io.Writer
	// Editor 行编辑器（可为 nil）。有它时输入只来自它
	Editor LineReader
	// Palette 上色开关
	Palette Palette
	// Spinner 转圈（只在两侧都是真终端时开）
	Spinner bool
	// Lister `/tools` 的数据源（可为 nil）
	Lister ToolLister
	// HistoryPath 会话 id → 历史文件路径，banner 里展示用（可为 nil）
	HistoryPath func(session string) string
}

// Channel 一个命令行渠道。**单实例、单使用者**：它不像 web 那样服务多个用户，
// 整个进程里同时只有一个会话在跑。
type Channel struct {
	mode        Mode
	session     string
	principal   string
	question    string
	in          *bufio.Reader
	out         io.Writer
	ui          io.Writer
	editor      LineReader
	palette     Palette
	spinner     bool
	lister      ToolLister
	historyPath func(string) string

	mu     sync.Mutex
	status channels.Status
	// pendingUsage / hasPendingUsage 工具循环在轮末经 Notify 交来的用量。
	// **缓冲到 say 再打**：答复先写、用量后写（`say` 在 Deliver 里被调），
	// 与「答复 → 用量 → debug」的呈现顺序一致。
	pendingUsage    channels.TokenUsage
	hasPendingUsage bool
}

// New 造一个命令行渠道。
func New(opts Options) *Channel {
	c := &Channel{
		mode:        opts.Mode,
		session:     opts.Session,
		principal:   opts.Principal,
		question:    opts.Question,
		out:         opts.Out,
		ui:          opts.UI,
		editor:      opts.Editor,
		palette:     opts.Palette,
		spinner:     opts.Spinner,
		lister:      opts.Lister,
		historyPath: opts.HistoryPath,
		status:      channels.StatusOffline,
	}
	if opts.In != nil {
		c.in = bufio.NewReader(opts.In)
	}
	return c
}

// ── Identity ────────────────────────────────────────────

func (c *Channel) ID() string        { return ID }
func (c *Channel) Label() string     { return Label }
func (c *Channel) AccountID() string { return accountLocal }

func (c *Channel) Status() channels.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// ── Outbound ────────────────────────────────────────────

// Capabilities 只声明文本收发。命令行没有入站媒体那条路（`internal/messages`
// 收到媒体如实拒答），声明收不进来也发不出去的能力是「声明一个跑不了的」。
func (c *Channel) Capabilities() channels.Capabilities {
	return channels.Capabilities{
		Text: channels.KindCapability{Send: true, Receive: true},
	}
}

func (c *Channel) Senders() channels.Senders {
	return channels.Senders{Text: c.sendText}
}

// sendText 把答复写到 Out。**标签（`[助手]` / `● 助手`）进 UI**——
// `fka ask "…" > 答案.txt` 因此是一份干净答案（本仓不变量）。
func (c *Channel) sendText(ctx context.Context, p channels.SendTextParams) (channels.SendResult, error) {
	c.say(p.Text)
	return channels.SendResult{MessageID: "cli-" + newUUIDv4()}, nil
}

// say 打一行「角色标签 + 正文」：标签按模式分风格，正文只进 Out。之后再补一行
// 用量（工具循环轮末经 `Notify` 交来、缓存在这里）。
func (c *Channel) say(text string) {
	label := "[助手]"
	if c.mode == ModeChat {
		label = c.palette.Assistant("● 助手")
	}
	fmt.Fprintln(c.ui, label)
	fmt.Fprintln(c.out, text)
	c.flushUsage()
}

// Notify 收到工具循环的通知。用量**缓冲**下来等 `say` 打答复时补一行；沙盒落盘通知
// （`MediaStoredNotice`）没有后续答复，**立即**打一行系统提示。
func (c *Channel) Notify(notice channels.Notice) {
	if stored, ok := notice.(channels.MediaStoredNotice); ok {
		if strings.TrimSpace(stored.Line) == "" {
			return
		}
		if c.mode == ModeChat {
			fmt.Fprintf(c.ui, "%s\n", c.palette.Dim("· "+stored.Line))
			return
		}
		fmt.Fprintf(c.ui, "[文件] %s\n", stored.Line)
		return
	}

	usage, ok := notice.(channels.UsageNotice)
	if !ok {
		return
	}
	c.mu.Lock()
	c.pendingUsage = usage.Usage
	c.hasPendingUsage = true
	c.mu.Unlock()
}

// flushUsage 打一行用量并清掉缓冲。全零不打、只打一次。
func (c *Channel) flushUsage() {
	c.mu.Lock()
	usage, ok := c.pendingUsage, c.hasPendingUsage
	c.hasPendingUsage = false
	c.pendingUsage = channels.TokenUsage{}
	c.mu.Unlock()
	if !ok {
		return
	}
	line := usageLine(usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, usage.CachedTokens)
	if line == "" {
		return
	}
	if c.mode == ModeChat {
		fmt.Fprintf(c.ui, "%s\n", c.palette.Dim("· 用量："+line))
		return
	}
	fmt.Fprintf(c.ui, "[用量] %s\n", line)
}

// ── Lifecycle ───────────────────────────────────────────

// Start 开始一场会话并**阻塞到它结束**。命令行由 `cmd/fka` 直接持有它，不进
// `Service.StartAll`。
//
// `onMessage` 是命令行提供的「跑一轮」回调：渠道只负责产出 `InboundMessage`
// （带会话、身份、问题）并把它交出去，问答与呈现都在回调里（走 `messages.Ask`）。
func (c *Channel) Start(ctx context.Context, onMessage func(channels.InboundMessage)) error {
	c.mu.Lock()
	c.status = channels.StatusOnline
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.status = channels.StatusOffline
		c.mu.Unlock()
	}()

	if c.mode == ModeAsk {
		// 一问就走：不读 stdin、不画 banner。回调同步跑完一轮，Start 返回。
		onMessage(c.message(c.question))
		return nil
	}
	return c.loop(ctx, onMessage)
}

// Stop 停掉命令行渠道。Start 结束时会自己落地为 offline，这里主要给「还没起就停」
// 或外部主动停用——它不打断已经在跑的那一轮（回调由调用方管）。
func (c *Channel) Stop(ctx context.Context) error {
	c.mu.Lock()
	c.status = channels.StatusOffline
	c.mu.Unlock()
	return nil
}

// ── EmitterProvider（可选能力）──────────────────────────

// Emitter 为一条消息提供回显。命令行只有一种回显，不依赖 ctx 的归属。
func (c *Channel) Emitter(ctx context.Context) channels.Emitter {
	return newEcho(c.ui, c.palette, c.mode)
}

// message 造一条入站消息。会话键（`ConversationID`）由渠道给：`/new` 换的
// 就是它，回调据此决定读哪份历史。
func (c *Channel) message(question string) channels.InboundMessage {
	return channels.InboundMessage{
		ChannelID:      ID,
		AccountID:      accountLocal,
		MessageID:      "cli-" + newUUIDv4(),
		SenderID:       c.principal,
		PrincipalID:    c.principal,
		ConversationID: c.session,
		Parts:          []channels.Part{channels.TextPart(question)},
		Timestamp:      time.Now().UnixMilli(),
	}
}

// trimQuestion 去掉首尾空白——空行不该被当成一个问题。
func trimQuestion(line string) string { return strings.TrimSpace(line) }

// 确保满足渠道契约。
var _ channels.Channel = (*Channel)(nil)
var _ channels.EmitterProvider = (*Channel)(nil)
var _ channels.Notifier = (*Channel)(nil)
