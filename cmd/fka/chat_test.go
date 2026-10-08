package main

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/agent"
)

// fakeEngine 假的工具循环。**只记下每次 Run 的输入**，回不回答由 reply 决定。
// 用它才能把「多轮到底有没有复用同一个会话」这类事实钉住——真 runner 要模型。
type fakeEngine struct {
	mu    sync.Mutex
	seen  []agent.RunnerInput
	reply func(agent.RunnerInput) (agent.RunResult, error)
}

func (f *fakeEngine) Run(_ context.Context, input agent.RunnerInput) (agent.RunResult, error) {
	f.mu.Lock()
	f.seen = append(f.seen, input)
	f.mu.Unlock()
	if f.reply != nil {
		return f.reply(input)
	}
	return agent.RunResult{
		Text: "答：" + input.Question, Steps: 1, StoppedBy: agent.StoppedByAnswered,
	}, nil
}

func (f *fakeEngine) inputs() []agent.RunnerInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.RunnerInput(nil), f.seen...)
}

// newTestREPL 造一个读 `input`、写两个缓冲的 REPL。颜色**默认关**，这样断言里
// 没有转义噪声；要验颜色那条单独把 pal 打开。
func newTestREPL(input string, engine chatEngine) (*chatREPL, *strings.Builder, *strings.Builder) {
	out := &strings.Builder{}
	ui := &strings.Builder{}
	return &chatREPL{
		engine:    engine,
		in:        bufio.NewReader(strings.NewReader(input)),
		out:       out,
		ui:        ui,
		pal:       palette{},
		principal: "tester",
		session:   "cli-fixed",
	}, out, ui
}

// Test会话_多轮复用同一个会话 是 chat 的**存在理由**：同一个 session 重复跑，
// 每轮都带上同一份历史前缀。若每轮换了 id，多轮就退化成串起来的单问单答。
func Test会话_多轮复用同一个会话(t *testing.T) {
	engine := &fakeEngine{}
	repl, out, _ := newTestREPL("第一问\n第二问\n/quit\n", engine)

	if code := repl.loop(context.Background()); code != exitOK {
		t.Fatalf("退出码 = %d，想要 %d", code, exitOK)
	}

	inputs := engine.inputs()
	if len(inputs) != 2 {
		t.Fatalf("跑了 %d 轮，想要 2 轮", len(inputs))
	}
	for index, input := range inputs {
		if input.SessionID != "cli-fixed" {
			t.Errorf("第 %d 轮 session = %q，想要 cli-fixed（换了 id 就没有多轮了）", index+1, input.SessionID)
		}
		if input.PrincipalID != "tester" {
			t.Errorf("第 %d 轮身份 = %q，想要 tester", index+1, input.PrincipalID)
		}
	}
	if inputs[0].Question != "第一问" || inputs[1].Question != "第二问" {
		t.Errorf("问题传丢了：%q / %q", inputs[0].Question, inputs[1].Question)
	}
	if !strings.Contains(out.String(), "答：第一问") || !strings.Contains(out.String(), "答：第二问") {
		t.Errorf("两条回答没都写出去：%q", out.String())
	}
}

// Test会话_回答只进stdout标签只进stderr 钉的是本仓那条不变量：**stdout 只有结果**。
// `fka chat < 问.txt > 答.txt` 之后那份文件若混着「● 助手」，再 grep 就分不清
// 哪一行是模型说的、哪一行是我们加的。
func Test会话_回答只进stdout标签只进stderr(t *testing.T) {
	engine := &fakeEngine{}
	repl, out, ui := newTestREPL("问题\n/quit\n", engine)

	repl.loop(context.Background())

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

// Test会话_颜色开启时stdout仍然干净 是上一条的加强版：**即使开了颜色，回答那一侧
// 也不许有转义**。转义只属于给人看的提示，写进重定向文件就是把文件弄脏。
func Test会话_颜色开启时stdout仍然干净(t *testing.T) {
	engine := &fakeEngine{}
	repl, out, ui := newTestREPL("问题\n/quit\n", engine)
	repl.pal = palette{enabled: true}

	repl.loop(context.Background())

	if !strings.Contains(ui.String(), "\x1b[") {
		t.Errorf("开了颜色但 stderr 没有转义：%q", ui.String())
	}
	if strings.Contains(out.String(), "\x1b[") {
		t.Errorf("stdout 里出现了转义：%q", out.String())
	}
}

// Test会话_一轮失败不带走会话：模型偶发失败之后，下一行还能继续问。
// 若失败就退出，用户会因为一次网络抖动丢掉整场对话。
func Test会话_一轮失败不带走会话(t *testing.T) {
	engine := &fakeEngine{}
	engine.reply = func(input agent.RunnerInput) (agent.RunResult, error) {
		if input.Question == "会失败的问题" {
			return agent.RunResult{}, errors.New("接口 500")
		}
		return agent.RunResult{Text: "答：" + input.Question}, nil
	}
	repl, out, ui := newTestREPL("会失败的问题\n还能继续\n/quit\n", engine)

	if code := repl.loop(context.Background()); code != exitOK {
		t.Fatalf("退出码 = %d，想要 %d", code, exitOK)
	}

	if len(engine.inputs()) != 2 {
		t.Errorf("只跑了 %d 轮，失败后应该继续问下一句", len(engine.inputs()))
	}
	if !strings.Contains(ui.String(), "问答失败：接口 500") {
		t.Errorf("失败没如实报出来：%q", ui.String())
	}
	if !strings.Contains(out.String(), "答：还能继续") {
		t.Errorf("失败之后的回答没写出去：%q", out.String())
	}
}

// Test会话_认不出的命令不当问题：`/sesion`（拼错）若当问题发出去，模型会认真回答
// 一个莫名其妙的字符串，而界面上看不出是拼错了。这与 parseFlags 对未知参数的处理
// 是同一条规矩。
func Test会话_认不出的命令不当问题(t *testing.T) {
	engine := &fakeEngine{}
	repl, _, ui := newTestREPL("/sesion\n/quit\n", engine)

	repl.loop(context.Background())

	if len(engine.inputs()) != 0 {
		t.Errorf("命令被当成问题发出去了：%+v", engine.inputs())
	}
	if !strings.Contains(ui.String(), "认不出的命令：/sesion") {
		t.Errorf("没报出不认识的命令：%q", ui.String())
	}
}

// Test会话_斜杠命令不发模型：会话/工具/帮助/换会话这几条都不该花一次模型调用。
func Test会话_斜杠命令不发模型(t *testing.T) {
	engine := &fakeEngine{}
	repl, _, _ := newTestREPL("/help\n/session\n/tools\n/new\n/quit\n", engine)

	if code := repl.loop(context.Background()); code != exitOK {
		t.Fatalf("退出码 = %d，想要 %d", code, exitOK)
	}
	if len(engine.inputs()) != 0 {
		t.Errorf("斜杠命令触发了模型调用：%+v", engine.inputs())
	}
}

// Test会话_new换会话：`/new` 之后的那一轮必须用一个**新的** id，否则「重开一个
// 会话」只是换了个显示，历史照旧串在一起。
func Test会话_new换会话(t *testing.T) {
	engine := &fakeEngine{}
	repl, _, _ := newTestREPL("/new\n问题\n/quit\n", engine)

	repl.loop(context.Background())

	inputs := engine.inputs()
	if len(inputs) != 1 {
		t.Fatalf("跑了 %d 轮，想要 1 轮", len(inputs))
	}
	if inputs[0].SessionID == "cli-fixed" {
		t.Errorf("/new 之后 session 没有换：%q", inputs[0].SessionID)
	}
	if !strings.HasPrefix(inputs[0].SessionID, cliSessionPrefix) {
		t.Errorf("新 session 形状不对：%q", inputs[0].SessionID)
	}
}

// Test会话_EOF与空行：Ctrl-D（末行没有换行也算）干净退出；空行什么都不做。
func Test会话_EOF与空行(t *testing.T) {
	// 末行没有换行符：readLine 必须把内容留下、下一次才是干净的 EOF
	engine := &fakeEngine{}
	repl, out, ui := newTestREPL("只有这一句", engine)

	if code := repl.loop(context.Background()); code != exitOK {
		t.Fatalf("退出码 = %d，想要 %d", code, exitOK)
	}
	if len(engine.inputs()) != 1 {
		t.Fatalf("末行被丢了：跑了 %d 轮", len(engine.inputs()))
	}
	if !strings.Contains(out.String(), "答：只有这一句") {
		t.Errorf("末行回答没写出去：%q", out.String())
	}
	if !strings.Contains(ui.String(), "再见") {
		t.Errorf("EOF 没有干净的道别：%q", ui.String())
	}

	// 空行完全忽略
	engine = &fakeEngine{}
	repl, _, _ = newTestREPL("\n   \n\n/quit\n", engine)
	repl.loop(context.Background())
	if len(engine.inputs()) != 0 {
		t.Errorf("空行触发了模型调用：%+v", engine.inputs())
	}
}

// Test会话_中断干净退出：run() 的 signal ctx 取消后再进循环，应当**当成用户要走**
// 而不是报错——Ctrl-C 之后还回一句红字没有任何意义。
func Test会话_中断干净退出(t *testing.T) {
	engine := &fakeEngine{}
	repl, _, ui := newTestREPL("", engine)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if code := repl.loop(ctx); code != exitOK {
		t.Errorf("退出码 = %d，想要 %d", code, exitOK)
	}
	if len(engine.inputs()) != 0 {
		t.Errorf("取消后还调了模型：%+v", engine.inputs())
	}
	if !strings.Contains(ui.String(), "再见") {
		t.Errorf("没有干净的道别：%q", ui.String())
	}
}

// Test拆斜杠命令_参数保留空格：命令名只到第一个空格，剩下原样留着，将来的
// `/save 我的问题` 这类带空格参数才不会被吃掉后半截。
func Test拆斜杠命令_参数保留空格(t *testing.T) {
	for _, tc := range []struct {
		line string
		name string
		arg  string
	}{
		{"/help", "/help", ""},
		{"/help   ", "/help", ""},
		{"/save 我的 问题", "/save", "我的 问题"},
		{"  /quit  ", "/quit", ""},
	} {
		name, arg := splitSlash(tc.line)
		if name != tc.name || arg != tc.arg {
			t.Errorf("splitSlash(%q) = (%q, %q)，想要 (%q, %q)",
				tc.line, name, arg, tc.name, tc.arg)
		}
	}
}

// Test颜色开关_第一条命中说了算：参数 > 环境变量 > 自动探测。顺序反了会出现
// 「谁说了算」的不确定，比如 `--no-color` 被一个 FKA_COLOR=always 顶回去。
func Test颜色开关_第一条命中说了算(t *testing.T) {
	// 一个普通文件：**不是**字符设备，所以自动探测那条一律为假
	plain, err := os.Create(filepath.Join(t.TempDir(), "not-a-tty"))
	if err != nil {
		t.Fatalf("建临时文件失败：%v", err)
	}
	defer plain.Close()

	for name, tc := range map[string]struct {
		noColorFlag bool
		env         map[string]string
		want        bool
	}{
		"都不是就不上色": {false, map[string]string{}, false},
		"NO_COLOR 设置了就关": {
			false, map[string]string{"NO_COLOR": "1"}, false,
		},
		"NO_COLOR 空值也要关":   {false, map[string]string{"NO_COLOR": ""}, false},
		"TERM=dumb 就关":     {false, map[string]string{"TERM": "dumb"}, false},
		"FKA_COLOR=always": {false, map[string]string{"FKA_COLOR": "always"}, true},
		"FKA_COLOR=never":  {false, map[string]string{"FKA_COLOR": "never"}, false},
		"参数优先于环境变量": {
			true, map[string]string{"FKA_COLOR": "always"}, false,
		},
		"never 压过 NO_COLOR": {
			false, map[string]string{"FKA_COLOR": "never", "NO_COLOR": ""}, false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			// t.Setenv 会自动还原；先用空串把可能残留的键清掉
			for _, key := range []string{"NO_COLOR", "TERM", "FKA_COLOR"} {
				t.Setenv(key, "")
			}
			os.Unsetenv("NO_COLOR")
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			if got := shouldColor(plain, tc.noColorFlag); got != tc.want {
				t.Errorf("shouldColor = %v，想要 %v", got, tc.want)
			}
		})
	}
}

// Test调色板关掉时原样返回：关掉不是「少几个转义」而是**一个都不许有**。
// 每个方法各自判一次开关的话，漏掉的那处会把 `\x1b[0m` 写进重定向的文件。
func Test调色板关掉时原样返回(t *testing.T) {
	off := palette{}
	on := palette{enabled: true}

	if got := off.err("坏了"); got != "坏了" {
		t.Errorf("关掉时 err = %q，想要原样", got)
	}
	for _, got := range []string{on.err("坏了"), on.assistant("助手"), on.user("> "), on.tool("x"), on.dim("y")} {
		if !strings.Contains(got, "\x1b[") || !strings.HasSuffix(got, ansiReset) {
			t.Errorf("开启时应当包一层转义并以 reset 收尾：%q", got)
		}
	}
}
