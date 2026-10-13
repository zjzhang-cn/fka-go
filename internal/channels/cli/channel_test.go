package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/channels"
)

// recorder 假的「跑一轮」回调：记下渠道交来的每条消息，并按约定把一句答复经渠道的
// 文本发送器写出去——这样 Out / UI 的分工就能被断言到（模拟 cmd 侧的 Ask + Deliver）。
type recorder struct {
	mu       sync.Mutex
	messages []channels.InboundMessage
	// failOn 这一句问题让回调走失败路径（只写 UI，不发送）
	failOn string
}

func (r *recorder) callback(ch *Channel) func(channels.InboundMessage) {
	return func(in channels.InboundMessage) {
		r.mu.Lock()
		r.messages = append(r.messages, in)
		fail := r.failOn != "" && in.Text() == r.failOn
		r.mu.Unlock()

		if fail {
			fmt.Fprintln(ch.ui, ch.palette.Err("问答失败：接口 500"))
			return
		}
		_, _ = ch.Senders().Text(context.Background(), channels.SendTextParams{
			Text: "答：" + in.Text(),
		})
	}
}

func (r *recorder) seen() []channels.InboundMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]channels.InboundMessage(nil), r.messages...)
}

// newChat 造一个读 input、写两个缓冲的 chat 渠道。颜色默认关，这样断言里没有转义噪声。
func newChat(input string) (*Channel, *strings.Builder, *strings.Builder) {
	out := &strings.Builder{}
	ui := &strings.Builder{}
	c := New(Options{
		Mode: ModeChat, Session: "cli-fixed", Principal: "tester",
		In: strings.NewReader(input), Out: out, UI: ui, Palette: NewPalette(false),
	})
	return c, out, ui
}

// Test渠道_多轮复用同一个会话 是 chat 的**存在理由**：同一个 session 重复跑，
// 每轮都带上同一份历史前缀。若每轮换了 id，多轮就退化成串起来的单问单答。
func Test渠道_多轮复用同一个会话(t *testing.T) {
	c, out, _ := newChat("第一问\n第二问\n/quit\n")
	rec := &recorder{}

	if err := c.Start(context.Background(), rec.callback(c)); err != nil {
		t.Fatalf("Start 出错：%v", err)
	}

	seen := rec.seen()
	if len(seen) != 2 {
		t.Fatalf("跑了 %d 轮，想要 2 轮", len(seen))
	}
	for index, in := range seen {
		if in.ConversationID != "cli-fixed" {
			t.Errorf("第 %d 轮会话 = %q，想要 cli-fixed（换了 id 就没有多轮了）", index+1, in.ConversationID)
		}
		if in.PrincipalID != "tester" {
			t.Errorf("第 %d 轮身份 = %q，想要 tester", index+1, in.PrincipalID)
		}
	}
	if seen[0].Text() != "第一问" || seen[1].Text() != "第二问" {
		t.Errorf("问题传丢了：%q / %q", seen[0].Text(), seen[1].Text())
	}
	if !strings.Contains(out.String(), "答：第一问") || !strings.Contains(out.String(), "答：第二问") {
		t.Errorf("两条回答没都写出去：%q", out.String())
	}
}

// Test渠道_回答只进Out标签只进UI 钉的是本仓那条不变量：**stdout 只有结果**。
// `fka chat < 问.txt > 答.txt` 之后那份文件若混着「● 助手」，再 grep 就分不清
// 哪一行是模型说的、哪一行是我们加的。
func Test渠道_回答只进Out标签只进UI(t *testing.T) {
	c, out, ui := newChat("问题\n/quit\n")
	rec := &recorder{}

	c.Start(context.Background(), rec.callback(c))

	if !strings.Contains(out.String(), "答：问题") {
		t.Errorf("stdout 缺回答：%q", out.String())
	}
	for _, must := range []string{"fka chat", "● 助手", "> "} {
		if strings.Contains(out.String(), must) {
			t.Errorf("stdout 混进了提示 %q：%q", must, out.String())
		}
	}
	if !strings.Contains(ui.String(), "● 助手") {
		t.Errorf("stderr 缺角色标签：%q", ui.String())
	}
	if strings.Contains(ui.String(), "答：问题") {
		t.Errorf("stderr 混进了回答：%q", ui.String())
	}
}

// Test渠道_Ask模式一问就走 Ask 不读 stdin、不画 banner：只把 Options.Question
// 交出去一次。
func Test渠道_Ask模式一问就走(t *testing.T) {
	out := &strings.Builder{}
	ui := &strings.Builder{}
	c := New(Options{
		Mode: ModeAsk, Session: "cli-ask", Principal: "tester", Question: "就这一问",
		Out: out, UI: ui, Palette: NewPalette(false),
	})
	rec := &recorder{}

	if err := c.Start(context.Background(), rec.callback(c)); err != nil {
		t.Fatalf("Start 出错：%v", err)
	}

	seen := rec.seen()
	if len(seen) != 1 || seen[0].Text() != "就这一问" {
		t.Fatalf("Ask 模式该正好交出一条问题，实际 %+v", seen)
	}
	if strings.Contains(ui.String(), "fka chat") {
		t.Errorf("Ask 模式不该画 banner：%q", ui.String())
	}
	if !strings.Contains(out.String(), "答：就这一问") {
		t.Errorf("答案没写出去：%q", out.String())
	}
}

// Test渠道_颜色开启时Out仍然干净 即使开了颜色，回答那一侧也不许有转义。
// 转义只属于给人看的提示，写进重定向文件就是把文件弄脏。
func Test渠道_颜色开启时Out仍然干净(t *testing.T) {
	c, out, ui := newChat("问题\n/quit\n")
	c.palette = NewPalette(true)
	rec := &recorder{}

	c.Start(context.Background(), rec.callback(c))

	if !strings.Contains(ui.String(), "\x1b[") {
		t.Errorf("开了颜色但 stderr 没有转义：%q", ui.String())
	}
	if strings.Contains(out.String(), "\x1b[") {
		t.Errorf("stdout 里出现了转义：%q", out.String())
	}
}

// Test渠道_回显只落在UI上 推理与工具行是过程信息，只进 UI；答案不进回显。
func Test渠道_回显只落在UI上(t *testing.T) {
	c, out, ui := newChat("")
	e := c.Emitter(context.Background())

	e.Reasoning("想一下")
	e.Tool("mcp__sandbox__run", `{"command":"ls"}`, "ok")
	e.Answer("这是答案")

	if !strings.Contains(ui.String(), "[推理] ") || !strings.Contains(ui.String(), "· mcp__sandbox__run") {
		t.Errorf("回显该落在 stderr：%q", ui.String())
	}
	if out.Len() != 0 {
		t.Errorf("回显不该写 stdout：%q", out.String())
	}
	if strings.Contains(ui.String(), "这是答案") {
		t.Errorf("答案不该从回显写出去：%q", ui.String())
	}
}

// Test渠道_一轮没答上不带走会话：模型偶发失败之后，下一行还能继续问。
func Test渠道_一轮没答上不带走会话(t *testing.T) {
	c, out, ui := newChat("会失败的问题\n还能继续\n/quit\n")
	rec := &recorder{failOn: "会失败的问题"}

	if err := c.Start(context.Background(), rec.callback(c)); err != nil {
		t.Fatalf("Start 出错：%v", err)
	}

	if len(rec.seen()) != 2 {
		t.Errorf("只跑了 %d 轮，失败后应该继续问下一句", len(rec.seen()))
	}
	if !strings.Contains(ui.String(), "问答失败：接口 500") {
		t.Errorf("失败没如实报出来：%q", ui.String())
	}
	if !strings.Contains(out.String(), "答：还能继续") {
		t.Errorf("失败之后的回答没写出去：%q", out.String())
	}
}

// Test渠道_认不出的命令不当问题：`/sesion`（拼错）若当问题发出去，模型会认真回答
// 一个莫名其妙的字符串，而界面上看不出是拼错了。
func Test渠道_认不出的命令不当问题(t *testing.T) {
	c, _, ui := newChat("/sesion\n/quit\n")
	rec := &recorder{}

	c.Start(context.Background(), rec.callback(c))

	if len(rec.seen()) != 0 {
		t.Errorf("命令被当成问题发出去了：%+v", rec.seen())
	}
	if !strings.Contains(ui.String(), "认不出的命令：/sesion") {
		t.Errorf("没报出不认识的命令：%q", ui.String())
	}
}

// Test渠道_斜杠命令不发模型：会话/工具/帮助/换会话这几条都不该交出一轮问题。
func Test渠道_斜杠命令不发模型(t *testing.T) {
	c, _, _ := newChat("/help\n/session\n/tools\n/new\n/quit\n")
	rec := &recorder{}

	if err := c.Start(context.Background(), rec.callback(c)); err != nil {
		t.Fatalf("Start 出错：%v", err)
	}
	if len(rec.seen()) != 0 {
		t.Errorf("斜杠命令交出了问题：%+v", rec.seen())
	}
}

// Test渠道_new换会话：`/new` 之后的那一轮必须用一个**新的** id，否则「重开一个
// 会话」只是换了个显示，历史照旧串在一起。
func Test渠道_new换会话(t *testing.T) {
	c, _, _ := newChat("/new\n问题\n/quit\n")
	rec := &recorder{}

	c.Start(context.Background(), rec.callback(c))

	seen := rec.seen()
	if len(seen) != 1 {
		t.Fatalf("跑了 %d 轮，想要 1 轮", len(seen))
	}
	if seen[0].ConversationID == "cli-fixed" {
		t.Errorf("/new 之后 session 没有换：%q", seen[0].ConversationID)
	}
	if !strings.HasPrefix(seen[0].ConversationID, SessionPrefix) {
		t.Errorf("新 session 形状不对：%q", seen[0].ConversationID)
	}
}

// Test渠道_EOF与空行：Ctrl-D（末行没有换行也算）干净退出；空行什么都不做。
func Test渠道_EOF与空行(t *testing.T) {
	// 末行没有换行符：readLine 必须把内容留下、下一次才是干净的 EOF
	c, out, ui := newChat("只有这一句")
	rec := &recorder{}

	if err := c.Start(context.Background(), rec.callback(c)); err != nil {
		t.Fatalf("Start 出错：%v", err)
	}
	if len(rec.seen()) != 1 {
		t.Fatalf("末行被丢了：跑了 %d 轮", len(rec.seen()))
	}
	if !strings.Contains(out.String(), "答：只有这一句") {
		t.Errorf("末行回答没写出去：%q", out.String())
	}
	if !strings.Contains(ui.String(), "再见") {
		t.Errorf("EOF 没有干净的道别：%q", ui.String())
	}

	// 空行完全忽略
	blank, _, _ := newChat("\n   \n\n/quit\n")
	rec = &recorder{}
	blank.Start(context.Background(), rec.callback(blank))
	if len(rec.seen()) != 0 {
		t.Errorf("空行交出了问题：%+v", rec.seen())
	}
}

// Test渠道_中断干净退出：外部 signal ctx 取消后再进循环，应当**当成用户要走**
// 而不是报错——Ctrl-C 之后还回一句红字没有任何意义。
func Test渠道_中断干净退出(t *testing.T) {
	c, _, ui := newChat("")
	rec := &recorder{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.Start(ctx, rec.callback(c)); err != nil {
		t.Errorf("Start 出错：%v", err)
	}
	if len(rec.seen()) != 0 {
		t.Errorf("取消后还交了问题：%+v", rec.seen())
	}
	if !strings.Contains(ui.String(), "再见") {
		t.Errorf("没有干净的道别：%q", ui.String())
	}
}

// Test渠道_状态开停对得上 渠道的 Status 要如实反映起停：跑的时候 online，
// 结束后 offline。Status 是接缝契约里唯一「谁在用」还没落地的方法，但渠道自己
// 的进出必须对得上。
func Test渠道_状态开停对得上(t *testing.T) {
	c := New(Options{
		Mode: ModeAsk, Session: "cli-status", Principal: "tester", Question: "问",
		Out: &strings.Builder{}, UI: &strings.Builder{}, Palette: NewPalette(false),
	})

	if c.Status() != channels.StatusOffline {
		t.Errorf("起之前该是 offline，实际 %s", c.Status())
	}

	var during channels.Status
	c.Start(context.Background(), func(in channels.InboundMessage) {
		during = c.Status()
	})

	if during != channels.StatusOnline {
		t.Errorf("跑的时候该是 online，实际 %s", during)
	}
	if c.Status() != channels.StatusOffline {
		t.Errorf("结束后该是 offline，实际 %s", c.Status())
	}
}

// Test渠道_声明与发送器对得上 能力声明与发送器必须一致——这是 Node 版
// `defineChannel()` 编译期保证在 Go 里的等价物。
func Test渠道_声明与发送器对得上(t *testing.T) {
	c := New(Options{
		Mode: ModeAsk, Session: "s", Principal: "tester", Question: "问",
		Out: &strings.Builder{}, UI: &strings.Builder{},
	})
	if err := channels.ValidateChannel(c); err != nil {
		t.Errorf("cli 渠道该通过 ValidateChannel：%v", err)
	}
}

// fakeLineReader 假的行编辑器。用它钉住「真终端上输入只走 readline 这一条路」——
// 若两条路都读，管道与终端会各读一半，问题就串了。
type fakeLineReader struct {
	lines []string
	err   error
	i     int
}

func (f *fakeLineReader) Readline() (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if f.i >= len(f.lines) {
		return "", io.EOF
	}
	line := f.lines[f.i]
	f.i++
	return line, nil
}

func (f *fakeLineReader) Close() error { return nil }

// Test渠道_行编辑器接管输入：有 editor 时输入只来自它，bufio 那条不碰。
func Test渠道_行编辑器接管输入(t *testing.T) {
	c, out, _ := newChat("")
	c.editor = &fakeLineReader{lines: []string{"问题一", "问题二", "/quit"}}
	rec := &recorder{}

	if err := c.Start(context.Background(), rec.callback(c)); err != nil {
		t.Fatalf("Start 出错：%v", err)
	}
	seen := rec.seen()
	if len(seen) != 2 {
		t.Fatalf("跑了 %d 轮，想要 2 轮", len(seen))
	}
	if seen[0].Text() != "问题一" || seen[1].Text() != "问题二" {
		t.Errorf("问题传丢了：%q / %q", seen[0].Text(), seen[1].Text())
	}
	if !strings.Contains(out.String(), "答：问题一") {
		t.Errorf("回答没走 Out：%q", out.String())
	}
}

// Test渠道_行编辑器EOF与报错干净退出：EOF 与 readline 的报错都该**当成用户要走**，
// 不交问题、不报红字。
func Test渠道_行编辑器EOF与报错干净退出(t *testing.T) {
	for name, reader := range map[string]*fakeLineReader{
		"EOF": {}, // 立刻 EOF
		"报错":  {err: errors.New("输入坏了")},
	} {
		t.Run(name, func(t *testing.T) {
			c, _, ui := newChat("")
			c.editor = reader
			rec := &recorder{}

			if err := c.Start(context.Background(), rec.callback(c)); err != nil {
				t.Errorf("Start 出错：%v", err)
			}
			if len(rec.seen()) != 0 {
				t.Errorf("退出前不该交问题：%+v", rec.seen())
			}
			if !strings.Contains(ui.String(), "再见") {
				t.Errorf("没有干净的道别：%q", ui.String())
			}
		})
	}
}
