package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/channels/ilink"
	"github.com/zjzhang-cn/fka-go/internal/channels/ilink/bot"
)

// 登录要等人扫码，超时给足。用户扫完之前连接一直开着。
const loginTimeout = 10 * time.Minute

// runLogin 扫码登录 iLink。
//
// ## 为什么它直接调 provider 而不是经 IPC
//
// Node 版走 IPC（`fka login` → 服务 → 扫码），因为那边登录逻辑在**服务进程**里
// （多账号轮询归它管）。Go 版的长轮询是每个渠道实例自己起的 goroutine，登录
// 只是「换一张账号表 + 重启那个实例」，**不需要服务在场**——所以直接调更简单，
// 少一层 socket、少一个「服务没起就登不上」的死锁。
//
// 登录完的凭证落进 `<安装根>/.env`（0600），`fka serve` 启动时读它。
func runLogin(ctx context.Context, args []string) int {
	provider := ilink.NewProvider()

	// Create 一次：登录要往账号表里写，而表是 Create 建的
	if _, err := provider.Create(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "渠道起不来："+err.Error())
		return exitFail
	}

	loginCtx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()

	result, err := provider.Ops().Login(channels.LoginParams{
		Account: flagOrEnv(args, "--account", "FKA_LOGIN_ACCOUNT", ""),
		Ctx:     loginCtx,
		Emit:    func(event string, data any) { renderLoginEvent(event, data, os.Stdout) },
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "登录失败："+err.Error())
		return exitFail
	}

	fmt.Println()
	fmt.Println("登录成功。凭证已写进 " + bot.DefaultEnvPath())
	if report, ok := result.(map[string]any); ok {
		fmt.Printf("账号 %v（%v）\n", report["accountId"], report["status"])
	}
	fmt.Println("现在跑 `fka serve` 就会用这个账号收消息。")
	return exitOK
}

// renderLoginEvent 把登录事件渲染到终端。
//
// **这是给人看的一层**——协议层只描述「发生了什么」，怎么讲给人听是 CLI 的事。
//
// 收 io.Writer 而不是 *os.File：**测试要能把它写进 io.Discard**，否则要么污染
// 测试输出，要么只能传 nil 碰运气。
func renderLoginEvent(event string, data any, out io.Writer) {
	switch event {
	case "qrcode:fetching":
		fmt.Fprintln(out, "正在取二维码…")

	case "qrcode:ready":
		content, _ := data.(map[string]any)["content"].(string)
		fmt.Fprintln(out)
		fmt.Fprintln(out, "=====================================")
		fmt.Fprintln(out, "请用微信扫描下面的二维码")
		fmt.Fprintln(out, "=====================================")
		fmt.Fprintln(out)
		if ascii := renderQRCode(content); ascii != "" {
			fmt.Fprintln(out, ascii)
		}
		// **兜底**：终端里的码受字体与字号影响，扫不出来很常见。
		// 而这个链接与码面是**同一个内容**——在手机上打开它等同于扫码
		fmt.Fprintln(out)
		fmt.Fprintln(out, "扫不出来时，用手机打开这个链接（等同于扫码）：")
		fmt.Fprintln(out, content)
		fmt.Fprintln(out)
		fmt.Fprintln(out, "等待扫码…")

	case "qrcode:scaned":
		fmt.Fprintln(out, "扫到了，在手机上确认…")

	case "qrcode:wait":
		fmt.Fprintln(out, "等待扫码…")

	case "qrcode:expired":
		fmt.Fprintln(out, "二维码已过期，重跑一次 `fka login`。")

	case "login:done":
		fmt.Fprintln(out, "确认成功。")

	default:
		fmt.Fprintf(out, "[%s] %v\n", event, data)
	}
}

// renderQRCode 把内容渲染成终端里的字符码。
//
// **渲染不出来就返回空串**，由调用方打那个链接兜底——为一个二维码渲染失败
// 而让整个登录流程挂掉不划算。
func renderQRCode(content string) string {
	if strings.TrimSpace(content) == "" {
		return ""
	}
	code, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return ""
	}
	return code.ToSmallString(false)
}
