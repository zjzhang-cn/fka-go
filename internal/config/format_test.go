package config

import (
	"strings"
	"testing"
	"time"
)

// Test控制台那行是三格前缀 格式就是给人扫的：`[级别][账号][哪一段]`。
// 钉住它是因为**改坏了不会报错**——只会变成另一种更难扫的排版。
func Test控制台那行是三格前缀(t *testing.T) {
	got := renderLine(LevelInfo, TypeLLM, "提交模型请求", Context{
		AccountField: "account_002", "model": "deepseek",
	})

	// 字段是 `键=值` 而不是 JSON：**日志是拿 grep/tail 看的**，
	// 而 `grep 'account=account_002'` 这种朴素写法在 JSON 里写不出来
	want := `[INFO][account_002][LLM] 提交模型请求 account=account_002 model=deepseek`
	if got != want {
		t.Errorf("格式不对\n  实际 %s\n  期望 %s", got, want)
	}
}

// Test没有账号就印横杠 启动、配置、MCP 连接这些事本来就没有账号。
// **印 `-` 而不是留空**：两格都空的行看起来像坏了，`-` 是明确的一条信息
func Test没有账号就印横杠(t *testing.T) {
	for name, fields := range map[string]Context{
		"完全没有字段":  nil,
		"只有别的字段":  Context{"model": "x"},
		"账号是空串":   {AccountField: ""},
		"账号是 nil": {AccountField: nil},
		"账号是空格":   {AccountField: "   "},
	} {
		got := renderLine(LevelInfo, TypeSYS, "服务就绪", fields)
		if !strings.Contains(got, "[-][SYS]") {
			t.Errorf("%s：该印 [-][SYS]，实际 %s", name, got)
		}
	}
}

// Test级别与阶段都大写 级别大写是为了在一堆小写的消息里一眼定位
func Test级别与阶段都大写(t *testing.T) {
	got := renderLine(LevelError, TypeTOOL, "工具炸了", nil)
	if !strings.HasPrefix(got, "[ERROR][-][TOOL] ") {
		t.Errorf("实际 %s", got)
	}
}

// Test没给阶段就当启动阶段 **不给就该有确定的落点**——
// 空字符串会让前缀出现一格空的 `[]`，看起来像坏了
func Test没给阶段就当启动阶段(t *testing.T) {
	if got := renderLine(LevelInfo, "", "没说清", nil); !strings.HasPrefix(got, "[INFO][-][SYS] ") {
		t.Errorf("实际 %s", got)
	}
	if Type("").String() != string(TypeSYS) {
		t.Error("空阶段该退成 SYS")
	}
}

// Test账号不只是一格前缀 它**同时**还在行尾的字段里。
// 这是刻意的：前缀是给人扫的，字段是给 grep 用的——少任何一份都会有人找不到。
// 而 accountOf 只读不删，所以两者都在。
func Test账号不只是一格前缀(t *testing.T) {
	got := renderLine(LevelInfo, TypeMSG, "收到消息", Context{AccountField: "acct-1"})

	prefix := got[:strings.Index(got, "account=")]
	if !strings.HasPrefix(prefix, "[INFO][acct-1][MSG] ") {
		t.Errorf("前缀该带账号与阶段，实际 %s", prefix)
	}
	if !strings.Contains(got, "account=acct-1") {
		t.Errorf("字段里也该留着账号，实际 %s", got)
	}
}

// Test账号格取的不是任何字段 只有 AccountField 那个键算数。
// 传一个叫 "acct" 或 "user" 的字段不该被当成账号——
// 那会让日志的账号格偶尔有值偶尔没有，而没人说得清是哪种情况
func Test账号格取的不是任何字段(t *testing.T) {
	got := renderLine(LevelInfo, TypeMSG, "收到", Context{"acct": "x", "user": "y"})
	if !strings.Contains(got, "[-][MSG]") {
		t.Errorf("不该从别的键里猜账号，实际 %s", got)
	}
}

// Test字段按字典序排 Go 遍历 map 的顺序是随机的，不排序的话同一份字段两次落盘
// 排出来的行不一样，**diff 出来的全是噪音**
func Test字段按字典序排(t *testing.T) {
	want := " account=acct-1 chars=3 model=x"

	if got := renderFields(Context{"model": "x", "chars": 3, AccountField: "acct-1"}); got != want {
		t.Errorf("字段该按字典序排\n  实际 %s\n  期望 %s", got, want)
	}
}

// Test文件那一行是普通文本 不是 JSON：一行一条，**前面带 UTC 时间戳**，
// 后面的排版与控制台**完全一样**（只多一个时间戳）。
//
// ## 钉住「一行一条」是因为坏了看不出来
//
// 文件按天累积、只 append，`tail -f` 与 `grep` 都拿它当唯一真相。
// 换成 JSON 之后最容易丢的就是这条：字段带换行时一条日志会摊成两行。
func Test文件那一行是普通文本(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	got := renderEntry(now, LevelWarn, TypeMSG, "入站队列已满", Context{AccountField: "acct-1"})

	want := "2026-01-02T03:04:05.000Z [WARN][acct-1][MSG] 入站队列已满 account=acct-1"
	if got != want {
		t.Errorf("文件行不对\n  实际 %s\n  期望 %s", got, want)
	}
	if strings.HasPrefix(strings.TrimSpace(got), "{") || strings.Contains(got, `":`) {
		t.Errorf("不该再有 JSON：%s", got)
	}
}

// Test带换行的值不把一行撑开 工具参数、推理片段、数据库驱动的报错都能带换行。
// 不转义的话一条日志会摊成两三行，后半截看起来像**另一条**的，
// `cut -d' ' -f3` 出来的级别与阶段就全错了
func Test带换行的值不把一行撑开(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	got := renderEntry(now, LevelInfo, TypeTOOL, "工具调用", Context{
		AccountField: "acct-1",
		"text":       "第一行\n第二行",
		"args":       map[string]any{"query": "三亚\n机票"},
	})

	// 换行被换成看得见的 `\n`：**值照原样，只是不再撑行**
	want := `2026-01-02T03:04:05.000Z [INFO][acct-1][TOOL] 工具调用 ` +
		`account=acct-1 args=map[query:三亚\n机票] text=第一行\n第二行`
	if got != want {
		t.Errorf("换行该被转义成 \\n\n  实际 %q\n  期望 %q", got, want)
	}
}

// Test换行与空格一起出现时加引号 带空格的走 `strconv.Quote`，换行由它转义。
// 这条路**不能先过 `oneLine`**——真换行先变成 `\n` 两个字符再 Quote 的话，
// 出来的是 `\\n`，那是在骗人
func Test换行与空格一起出现时加引号(t *testing.T) {
	got := renderFields(Context{"text": "第一行\n第二行 尾巴"})

	if got != ` text="第一行\n第二行 尾巴"` {
		t.Errorf("实际 %q", got)
	}
	if strings.Contains(got, `\\n`) {
		t.Errorf("换行被转义了两次：%q", got)
	}
}

// Test带空格的值加引号 不加的话 `text=第一句 第二句` 会被读成两个字段——
// 排查时那个多出来的「字段」根本不存在
func Test带空格的值加引号(t *testing.T) {
	got := renderLine(LevelInfo, TypeMSG, "已作答", Context{
		"question": "去年三亚 玩得怎么样", "chars": 3,
	})

	want := `[INFO][-][MSG] 已作答 chars=3 question="去年三亚 玩得怎么样"`
	if got != want {
		t.Errorf("带空格的值该加引号\n  实际 %s\n  期望 %s", got, want)
	}
}

// Test引号与等号也进引号 这两个字符会让人把一个字段读成两个
// （`text=a=b` 分不清等号是值的一部分还是分隔符）
func Test引号与等号也进引号(t *testing.T) {
	for name, value := range map[string]any{
		"带等号":  "a=b",
		"带引号":  `say "hi"`,
		"全角空格": "三亚　机票",
	} {
		got := renderFields(Context{"v": value})
		if !strings.HasPrefix(got, " v=\"") {
			t.Errorf("%s：该加引号，实际 %s", name, got)
		}
	}
}

// Test空值就是空的 `tools=` 说明「这个字段有，值是空的」，
// 与「没这个字段」（行尾没有它）不是一回事
func Test空值就是空的(t *testing.T) {
	if got := renderFields(Context{"tools": ""}); got != " tools=" {
		t.Errorf("实际 %q", got)
	}
	if got := renderFields(nil); got != "" {
		t.Errorf("没有字段就该什么都不加，实际 %q", got)
	}
}

// Test非字符串的字段照原样印出来 int / bool 都常见（耗时、工具数、成败），
// **印 Go 的写法**而不是加引号——`chars=11`、`ok=true` 才扫得出来
func Test非字符串的字段照原样印出来(t *testing.T) {
	want := " chars=11 ms=3 ok=true"
	if got := renderFields(Context{"ms": 3, "ok": true, "chars": 11}); got != want {
		t.Errorf("实际 %s，期望 %s", got, want)
	}
}
