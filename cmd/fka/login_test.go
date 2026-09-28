package main

import (
	"strings"
	"testing"
)

// Test二维码渲染得出字符码 渲染不出来时该返回空串，让调用方打链接兜底——
// 为一个二维码渲染失败而让整个登录流程挂掉不划算。
func Test二维码渲染得出字符码(t *testing.T) {
	ascii := renderQRCode("https://liteapp.weixin.qq.com/q/7GiQu1?qrcode=abc123&bot_type=3")
	if ascii == "" {
		t.Fatal("该渲染得出字符码")
	}
	// 至少得有几十行，且用半块字符画出来——那是二维码的「暗模块」
	if lines := strings.Split(strings.TrimRight(ascii, "\n"), "\n"); len(lines) < 20 {
		t.Errorf("行数太少，不像二维码：%d 行", len(lines))
	}
	if !strings.ContainsAny(ascii, "▀▄█") {
		t.Error("该用半块字符画出来")
	}
}

func Test空内容渲染不出(t *testing.T) {
	for _, content := range []string{"", "   "} {
		if got := renderQRCode(content); got != "" {
			t.Errorf("空内容该返回空串，实际 %d 字符", len(got))
		}
	}
}

// Test登录事件都有人渲染 协议层只描述「发生了什么」——**漏一个事件的表现是
// 用户盯着一个不动的屏幕**，而那比任何错误提示都难查。
func Test登录事件都有人渲染(t *testing.T) {
	cases := map[string]any{
		"qrcode:fetching": nil,
		"qrcode:ready":    map[string]any{"content": "https://liteapp.weixin.qq.com/q/x?qrcode=y"},
		"qrcode:wait":     nil,
		"qrcode:scaned":   nil,
		"qrcode:expired":  nil,
		"login:done":      nil,
		"没听过的事件":          map[string]any{"x": 1},
		// **data 形状不对时也不该 panic**——协议层将来加字段、改形状都不会
		// 让整个登录流程崩在一个类型断言上
		"qrcode:ready 给的不是 map": "字符串",
		"qrcode:ready 是 nil":    nil,
	}

	var rendered strings.Builder
	for event, data := range cases {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("渲染 %q 时 panic 了：%v", event, recovered)
				}
			}()
			renderLoginEvent(event, data, &rendered)
		}()
	}

	// 出码那一步**必须带链接兜底**：终端里的码受字体与字号影响，
	// 扫不出来很常见，而这个链接与码面是同一个内容
	if !strings.Contains(rendered.String(), "liteapp.weixin.qq.com") {
		t.Errorf("出码那步该打出链接兜底：\n%s", rendered.String())
	}
}
