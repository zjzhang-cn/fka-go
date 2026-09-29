package main

import (
	"strings"
	"testing"
)

// Test参数不进问题 这条是**回归**：之前 `runAsk` 直接
// `strings.Join(args, " ")` 当问题，于是
//
//	fka ask --session aabbcc 你的名字加小航
//
// 送进模型的 user 消息是 `--session aabbcc 你的名字加小航`——参数原样跟着问题走。
// 症状极难认：模型答得挺好，只是**把参数当成问题的一部分**，
// 「刚才我说的是啥」会连着 `--session aabbcc` 一起复述。
// 证据留在 `data/history/<会话>.jsonl` 里，肉眼扫日志是扫不出来的。
func Test参数不进问题(t *testing.T) {
	for name, args := range map[string][]string{
		"参数在前": {"ask", "--session", "aabbcc", "--principal", "wx_zhang", "你的名字加小航"},
		"参数在后": {"ask", "你的名字加小航", "--session", "aabbcc"},
		"等号写法": {"ask", "--session=aabbcc", "你的名字加小航"},
		"多个位置": {"ask", "--session", "aabbcc", "你的", "名字", "加小航"},
		"只有参数": {"ask", "--session", "aabbcc"},
		"全局参数": {"--log-level", "debug", "ask", "--session", "aabbcc", "聊聊音乐商店的销量"},
	} {
		parsed, err := parseFlags(args)
		if err != nil {
			t.Errorf("%s：该认得出，%v", name, err)
			continue
		}
		question := strings.Join(parsed.positional, " ")
		if strings.Contains(question, "--") || strings.Contains(question, "aabbcc") {
			t.Errorf("%s：参数混进问题了 —— %q", name, question)
		}
		if strings.Contains(question, "wx_zhang") {
			t.Errorf("%s：身份混进问题了 —— %q", name, question)
		}
	}
}

// Test问题原样留着 问题是什么就问什么，**不加工**：
// 去掉参数不是为了顺手改写用户的措辞
func Test问题原样留着(t *testing.T) {
	parsed, err := parseFlags([]string{"ask", "--session", "aabbcc", "记一条：2026年3月全家去了三亚"})
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(parsed.positional, " "); got != "记一条：2026年3月全家去了三亚" {
		t.Errorf("问题该原样，实际 %q", got)
	}
	if got := flagOrEnv(parsed, sessionFlag, sessionEnv, "cli"); got != "aabbcc" {
		t.Errorf("会话该认得出，实际 %q", got)
	}
}

// Test参数值照旧拿得到 修「参数混进问题」不能顺手把参数本身弄丢了
func Test参数值照旧拿得到(t *testing.T) {
	parsed, err := parseFlags([]string{"ask", "--principal=wx_zhang", "--session", "aabbcc", "问"})
	if err != nil {
		t.Fatal(err)
	}

	if got := flagOrEnv(parsed, principalFlag, principalEnv, "cli"); got != "wx_zhang" {
		t.Errorf("身份该是 wx_zhang，实际 %q", got)
	}
	if got := flagOrEnv(parsed, sessionFlag, sessionEnv, "cli"); got != "aabbcc" {
		t.Errorf("会话该是 aabbcc，实际 %q", got)
	}
}

// Test参数没给时退回环境变量 参数与环境变量两个入口，**参数优先**
func Test参数没给时退回环境变量(t *testing.T) {
	t.Setenv(sessionEnv, "from-env")

	parsed := mustParse(t, "ask", "问")
	if got := flagOrEnv(parsed, sessionFlag, sessionEnv, "cli"); got != "from-env" {
		t.Errorf("该退回环境变量，实际 %q", got)
	}

	parsed = mustParse(t, "ask", "--session", "from-arg", "问")
	if got := flagOrEnv(parsed, sessionFlag, sessionEnv, "cli"); got != "from-arg" {
		t.Errorf("参数该压过环境变量，实际 %q", got)
	}
}

// Test子命令名不算问题 第一个位置参数是子命令，它不是问题的一部分。
// **参数可以写在子命令之前**（`fka --log-level debug serve`），所以子命令名和参数
// 是一起解析的——这一条钉住「谁当命令、谁当问题」
func Test子命令名不算问题(t *testing.T) {
	for _, args := range [][]string{
		{"ask", "--session", "aabbcc", "问题"},
		{"--log-level", "debug", "ask", "问题"},
		{"ask", "问题"},
	} {
		parsed, err := parseFlags(args)
		if err != nil {
			t.Fatalf("%v 该认得出：%v", args, err)
		}
		if parsed.command != "ask" {
			t.Errorf("%v：命令该是 ask，实际 %q", args, parsed.command)
		}
		if got := strings.Join(parsed.positional, " "); got != "问题" {
			t.Errorf("%v：问题该是「问题」，实际 %q", args, got)
		}
	}
}

// Test双横线之后原样当问题 问题本身以 `-` 开头时唯一的办法
// （比如把一段命令粘进 `fka ask` 里问「这为什么报错」）
func Test双横线之后原样当问题(t *testing.T) {
	parsed, err := parseFlags([]string{"ask", "--session", "aabbcc", "--", "--help 是干什么的"})
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(parsed.positional, " "); got != "--help 是干什么的" {
		t.Errorf("实际 %q", got)
	}
	if got := flagOrEnv(parsed, sessionFlag, sessionEnv, "cli"); got != "aabbcc" {
		t.Errorf("双横线之前的参数该照常认，实际 %q", got)
	}
}

// Test认不出的参数报用法错 `--sesion x`（敲错）如果被当问题，模型会拿到一句
// 莫名其妙的话并**认真回答**。这类失败必须响：报出来两秒的事，
// 静默走过去是「模型今天答得好奇怪」加半天排查
func Test认不出的参数报用法错(t *testing.T) {
	for name, args := range map[string][]string{
		"拼错":     {"ask", "--sesion", "aabbcc", "问"},
		"不认识":    {"ask", "--nope", "问"},
		"少写了值":   {"ask", "--session"},
		"布尔参数带值": {"ask", "--json=1", "问"},
	} {
		if _, err := parseFlags(args); err == nil {
			t.Errorf("%s：该报错，实际认下了", name)
		}
	}
}

// Test认错时把像的那个说清楚 参数名都是英文，`--sesion` 这种敲错一眼看不出来，
// 而「你不知道有哪些参数」比「你敲错了」难查得多
func Test认错时把像的那个说清楚(t *testing.T) {
	_, err := parseFlags([]string{"ask", "--sesion", "aabbcc", "问"})
	if err == nil {
		t.Fatal("该报错")
	}

	message := err.Error()
	for _, want := range []string{sessionFlag, "认不出"} {
		if !strings.Contains(message, want) {
			t.Errorf("错误信息该提 %q，实际 %s", want, message)
		}
	}
}

// Test布尔参数给没给要分得清 `--json` 不取值，
// **「没给」与「给了」在结果上必须不同**（tools 的输出格式就靠它分）
func Test布尔参数给没给要分得清(t *testing.T) {
	if hasFlag(mustParse(t, "tools"), "--json") {
		t.Error("没给不该认成给了")
	}
	if !hasFlag(mustParse(t, "tools", "--json"), "--json") {
		t.Error("给了该认出来")
	}
	// **给在子命令之前也一样**：`fka --json tools`
	if !hasFlag(mustParse(t, "--json", "tools"), "--json") {
		t.Error("子命令之前给了也该认出来")
	}
}
