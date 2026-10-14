package cli

import (
	"strings"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/channels"
)

// Test落沙盒行_按路径拼措辞 mediaStoredLine 是纯函数：服务端只给**结构化路径**，
// 措辞由这里拼。给了路径列出来、没给退回一句不带列表的。
func Test落沙盒行_按路径拼措辞(t *testing.T) {
	for name, tc := range map[string]struct {
		paths []string
		want  string
	}{
		"有路径":      {[]string{"inbox/a.png", "inbox/b.pdf"}, "已存入沙盒：inbox/a.png、inbox/b.pdf。请说要我拿它做什么。"},
		"没有可解析的路径": {nil, "文件已存入沙盒。请说要我拿它做什么。"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := mediaStoredLine(tc.paths); got != tc.want {
				t.Errorf("mediaStoredLine(%v) = %q，想要 %q", tc.paths, got, tc.want)
			}
		})
	}
}

// Test落沙盒通知_chat立即打一行 落沙盒通知没有后续答复，`Notify` 立即打一行系统提示，
// 且不进 stdout（是系统提示，不是聊天气泡）。
func Test落沙盒通知_chat立即打一行(t *testing.T) {
	c, out, ui := newChat("")
	c.Notify(channels.MediaStoredNotice{Paths: []string{"inbox/a.png"}})

	if !strings.Contains(ui.String(), "已存入沙盒：inbox/a.png。请说要我拿它做什么。") {
		t.Errorf("stderr 缺落沙盒系统提示：%q", ui.String())
	}
	if out.String() != "" {
		t.Errorf("系统提示不该进 stdout，实际 %q", out.String())
	}
}

// Test落沙盒通知_ask方括号行 Ask 模式打 `[文件] …`（方括号、不上色）。
func Test落沙盒通知_ask方括号行(t *testing.T) {
	out := &strings.Builder{}
	ui := &strings.Builder{}
	c := New(Options{
		Mode: ModeAsk, Session: "cli-x", Principal: "tester", Question: "问",
		Out: out, UI: ui, Palette: NewPalette(false),
	})
	c.Notify(channels.MediaStoredNotice{Paths: []string{"inbox/a.png"}})

	if !strings.Contains(ui.String(), "[文件] 已存入沙盒：inbox/a.png。请说要我拿它做什么。") {
		t.Errorf("stderr 缺方括号落沙盒行：%q", ui.String())
	}
}
