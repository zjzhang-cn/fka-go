package llm

import (
	"strings"
	"testing"
)

// 命令行那一轮的会话 id 的形状：`cli-` + UUID（v4）。**正好 40 字符**。
const cliSessionShape = "cli-3f2a9c1e-7b4d-4a2f-8e6c-1d5b0a9c3e7f"

// Test历史文件名_命令行会话不被截断 `safeSegment` 把会话段截到 40 字符，而
// `fka ask` 没给 `--session` 时用的 `cli-<uuid>` **正好 40**。
//
// 截断是**静默**的：两个名字只在被截掉那段不同的会话会落进同一个历史文件，
// 于是「上一条 CLI 问的什么」又跟着下一条命令进上下文——而命令行里什么都没变。
// 这条钉住「一个字符都不浪费」：前缀一改长就红。
func Test历史文件名_命令行会话不被截断(t *testing.T) {
	if got := len(cliSessionShape); got != 40 {
		t.Fatalf("这条用例自己就失效了：会话 id 是 %d 字符，不是 40", got)
	}

	got := historyFileName(cliSessionShape)
	if got != cliSessionShape+".jsonl" {
		t.Errorf("40 字符的会话 id 该原样进文件名，实际 %q", got)
	}
}

// Test历史文件名_超长会话截到40 截断本身是有意的（会话 id 来自外部，可能很长）。
// **要钉住的不是「不截」，而是「截到哪」**——不然改了预算没人知道会话名从此会撞车，
// 而那个错只在**恰好问到相关话题**时显形。
func Test历史文件名_超长会话截到40(t *testing.T) {
	long := strings.Repeat("x", 60)

	got := historyFileName(long)
	if want := strings.Repeat("x", 40) + ".jsonl"; got != want {
		t.Errorf("= %q，期望 %q", got, want)
	}
}

// Test历史文件名_渠道会话不带冗余账号 渠道的会话 id 已经是 `渠道:账号:会话`
// （见 channels.SessionKeyOf），账号**已经含在里面**——文件名只按它取，不再前缀
// 账号，否则会写出 `web_web_web_room1` 这种把同一段写三遍的名字。
func Test历史文件名_渠道会话不带冗余账号(t *testing.T) {
	if got := historyFileName("web:web:room1"); got != "web_web_room1.jsonl" {
		t.Errorf("= %q，期望 web_web_room1.jsonl", got)
	}
	if got := historyFileName("ilink:account_001:o9cq80_zhang"); got != "ilink_account_001_o9cq80_zhang.jsonl" {
		t.Errorf("= %q，期望 ilink_account_001_o9cq80_zhang.jsonl", got)
	}
}

// Test安全段_非法字符换下划线 会话标识来自外部，可能带斜杠/空格；压成能当文件名的
// 形式比报错中断历史划算。
func Test安全段_非法字符换下划线(t *testing.T) {
	if got := safeSegment("a/b c"); got != "a_b_c" {
		t.Errorf("= %q，期望 a_b_c", got)
	}
}
