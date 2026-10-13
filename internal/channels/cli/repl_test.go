package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/tools"
)

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

// fakeLister 假的工具表。err 非 nil 时列工具失败。
type fakeLister struct {
	tools []tools.RegisteredTool
	err   error
}

func (l *fakeLister) Tools(ctx context.Context, tc tools.Context) ([]tools.RegisteredTool, error) {
	return l.tools, l.err
}

// Test工具表_有Lister时列出放行的工具 `/tools` 要让人看到模型现在能调什么。
func Test工具表_有Lister时列出放行的工具(t *testing.T) {
	var ui strings.Builder
	c := New(Options{
		Mode: ModeChat, Session: "cli-x", Principal: "tester",
		Out: &strings.Builder{}, UI: &ui, Palette: NewPalette(false),
		Lister: &fakeLister{tools: []tools.RegisteredTool{{
			FullName: "mcp__memory__search_memories",
			Spec: tools.Spec{
				Name: "search_memories", Description: "查记忆", Effect: tools.EffectRead,
			},
		}}},
	})

	c.showTools(context.Background())

	got := ui.String()
	for _, want := range []string{"mcp__memory__search_memories", "read", "查记忆"} {
		if !strings.Contains(got, want) {
			t.Errorf("/tools 输出该含 %q：%q", want, got)
		}
	}
}

// Test工具表_三种空情形分得清 没有注册表 / 注册表为空 / 列工具报错——**各说各的**。
// 混成一句「没有工具」会让人分不清是「真没工具」还是「根本没接上」。
func Test工具表_三种空情形分得清(t *testing.T) {
	cases := map[string]struct {
		lister ToolLister
		want   string
	}{
		"没有注册表": {nil, "没有注册表可查"},
		"注册表为空": {&fakeLister{}, "看不到任何工具"},
		"列工具报错": {&fakeLister{err: errors.New("连接断了")}, "列工具失败"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var ui strings.Builder
			c := New(Options{
				Mode: ModeChat, Session: "cli-x", Principal: "tester",
				Out: &strings.Builder{}, UI: &ui, Palette: NewPalette(false), Lister: tc.lister,
			})
			c.showTools(context.Background())
			if !strings.Contains(ui.String(), tc.want) {
				t.Errorf("该说清是哪种情形（含 %q），实际 %q", tc.want, ui.String())
			}
		})
	}
}
