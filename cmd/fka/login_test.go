package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/config"
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

// accountEnvKeys 账号 1 在 .env 里的那组键。测试要能一次性把它们从**进程环境**
// 里摘掉——LoadEnv 不覆盖已存在的变量，所以只要有一个残留，测的就是一个被污染
// 的进程，「登录前读了 .env」这件事根本没被验证到。
var accountEnvKeys = []string{
	"ILINK_ACCOUNT_1_ID",
	"ILINK_ACCOUNT_1_BOT_TOKEN",
	"ILINK_ACCOUNT_1_BASE_URL",
	"ILINK_ACCOUNT_1_BOT_ID",
	"ILINK_ACCOUNT_1_USER_ID",
}

// Test登录第二个账号不覆盖第一个 账号表是从环境变量读的，而 `fka login` 不经
// app.Build()——不自己 LoadEnv 的话表是空的，pickSlot 于是永远挑中槽位 1，
// 把已登录的账号 1 原地覆盖掉。
//
// **断言选在「表里认不认得出已登录的账号」**：那是 pickSlot 会跳过槽位 1 的前提，
// 也是这条链路上唯一会静默失效的一环——覆盖成功时界面照样显示「登录成功」。
func Test登录第二个账号不覆盖第一个(t *testing.T) {
	setupInstallRoot(t, withAccount1Env()...)
	clearAccountEnv(t)

	provider, err := loginProvider(context.Background())
	if err != nil {
		t.Fatalf("造登录用的 provider 失败：%v", err)
	}

	described := provider.DescribeAccounts()
	if !strings.Contains(described, "account_001") {
		t.Errorf("该认得已登录的账号 1（认得才会跳过槽位 1），实际：%s", described)
	}
}

// Test没登录过就看到空表 上一条的反向对照：`.env` 里没有账号时，表就该是空的。
// 没有它的话，上一条可能因为别的原因恒真。
func Test没登录过就看到空表(t *testing.T) {
	setupInstallRoot(t)
	clearAccountEnv(t)

	provider, err := loginProvider(context.Background())
	if err != nil {
		t.Fatalf("造登录用的 provider 失败：%v", err)
	}

	described := provider.DescribeAccounts()
	if !strings.Contains(described, "当前没有配置任何 iLink 账号") {
		t.Errorf("没有账号时该报「没配置任何账号」，实际：%s", described)
	}
}

// installRoot 造一个临时安装根，并把它指给 config。
//
// **Home() 是进程内只解析一次的全局不变量**（config.Home 的 sync.Once），
// 所以要显式 SetHome——只设 FKA_HOME 的话，先跑过哪个用例就会留下哪个的值。
func setupInstallRoot(t *testing.T, envLines ...string) {
	t.Helper()

	home := t.TempDir()
	config.SetHome(home)
	t.Setenv("FKA_HOME", home)

	if len(envLines) == 0 {
		return
	}
	body := append([]string{"# 账号 1"}, envLines...)
	if err := os.WriteFile(filepath.Join(home, ".env"),
		[]byte(strings.Join(body, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// withAccount1Env 「账号 1 已登录」的那几行。
func withAccount1Env() []string {
	return []string{
		"ILINK_ACCOUNT_1_ID=account_001",
		"ILINK_ACCOUNT_1_BOT_TOKEN=token-1",
		"ILINK_ACCOUNT_1_BASE_URL=https://example.invalid",
		"ILINK_ACCOUNT_1_BOT_ID=bot-1",
		"ILINK_ACCOUNT_1_USER_ID=user-1",
	}
}

// clearAccountEnv 把账号 1 的键从进程环境里摘掉（摘完复原）。
func clearAccountEnv(t *testing.T) {
	t.Helper()

	for _, key := range accountEnvKeys {
		if old, ok := os.LookupEnv(key); ok {
			t.Cleanup(func() { _ = os.Setenv(key, old) })
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
		}
	}
}
