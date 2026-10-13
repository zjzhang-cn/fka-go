package cli

import (
	"strings"
	"testing"
)

// Test回显_答案不在这里写 答复走**唯一那条答案通道**（渠道的 `Senders().Text`），
// 回显的 `Answer` 是空的。若这里也写，答案会在界面上出现两次。
func Test回显_答案不在这里写(t *testing.T) {
	var ui strings.Builder
	e := newEcho(&ui, NewPalette(false), ModeChat)

	e.Answer("这是答案")

	if strings.Contains(ui.String(), "这是答案") {
		t.Errorf("答案不该从回显写出去：%q", ui.String())
	}
}

// Test回显_推理首块打前缀之后逐块原样 推理是按块来的，`[推理] ` 只打一次。
func Test回显_推理首块打前缀之后逐块原样(t *testing.T) {
	var ui strings.Builder
	e := newEcho(&ui, NewPalette(false), ModeAsk)

	e.Reasoning("先想一下")
	e.Reasoning("，再想一下")

	if got := ui.String(); got != "[推理] 先想一下，再想一下" {
		t.Errorf("推理回显 = %q", got)
	}
}

// Test回显_长内容压成一行截断 工具结果动辄上千字，全量铺开会把对话淹掉。
func Test回显_长内容压成一行截断(t *testing.T) {
	var ui strings.Builder
	e := newEcho(&ui, NewPalette(false), ModeAsk)

	long := strings.Repeat("很长", 100)
	e.Tool("mcp__memory__search_memories", `{"query":"生日"}`, long)

	got := ui.String()
	for _, want := range []string{"[工具]", "mcp__memory__search_memories", `{"query":"生日"}`, "…"} {
		if !strings.Contains(got, want) {
			t.Errorf("工具回显该含 %q：%q", want, got)
		}
	}
	if strings.Contains(got, long) {
		t.Errorf("结果没有截断：%q", got)
	}
}

// Test回显_chat风格带色圆点 chat 的工具行是黄色 `·` 打头的圆点行，与 ask 的
// 方括号行不同风格。
func Test回显_chat风格带色圆点(t *testing.T) {
	var ui strings.Builder
	e := newEcho(&ui, NewPalette(true), ModeChat)

	e.Tool("mcp__sandbox__run", `{"command":"ls"}`, "ok")

	got := ui.String()
	if !strings.Contains(got, ansiYellow+"·") {
		t.Errorf("chat 工具行该是带色圆点：%q", got)
	}
	if strings.Contains(got, "[工具]") {
		t.Errorf("chat 风格不该用方括号标签：%q", got)
	}
}
