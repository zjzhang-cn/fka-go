// 本文件是**工具层**的用例：参数怎么读、结果怎么说给人听。
//
// 这一层最容易出的错**不报错**：声明与读取方式不一致时（`mcp.WithNumber` 声明、
// `GetString` 读），参数会静默失效——照样返回结果，只是超时不由调用方定。
package main

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// callRequest 造一个工具调用请求。**数字参数用 float64**：JSON 解出来的数字就是
// 这个类型，用 `"1"` 去测会把「声明是 number 却按 string 读」那个 bug 测没。
func callRequest(args map[string]any) mcp.CallToolRequest {
	var request mcp.CallToolRequest
	request.Params.Arguments = args
	return request
}

func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) == 0 {
		t.Fatal("结果里没有内容")
	}
	return mcp.GetTextFromContent(result.Content[0])
}

// TestRun工具_缺command要拒 没有命令就没得跑——回一句给模型看的话，不抛异常。
func TestRun工具_缺command要拒(t *testing.T) {
	impl := &bashServer{sandbox: newTestSandbox(t)}

	result, err := impl.handleRun(context.Background(), callRequest(map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Error("缺 command 该回一条 IsError 的结果")
	}
	if body := resultText(t, result); !strings.Contains(body, "command") {
		t.Errorf("该说清缺的是哪个参数：%s", body)
	}
}

// TestRun工具_超时参数按数字读 声明是 `mcp.WithNumber`，读取就必须用 `GetInt`：
// `GetString` 只认 Go string，而 JSON 数字是 float64——于是超时永远取默认值。
// 用 `sleep 5` + `timeout_sec: 1` 钉住：读对了 1 秒就超时，读错了会等满 5 秒。
func TestRun工具_超时参数按数字读(t *testing.T) {
	requireBash(t)
	impl := &bashServer{sandbox: newTestSandbox(t)}

	result, err := impl.handleRun(context.Background(), callRequest(map[string]any{
		"command":     "sleep 5",
		"timeout_sec": float64(1),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Error("超时该回 IsError 的结果")
	}
	if body := resultText(t, result); !strings.Contains(body, "超时") {
		t.Errorf("该说清是超时：%s", body)
	}
}

// TestRun工具_成功结果不带IsError 命令跑完、退出码 0，是正常结果。
func TestRun工具_成功结果不带IsError(t *testing.T) {
	requireBash(t)
	impl := &bashServer{sandbox: newTestSandbox(t)}

	result, err := impl.handleRun(context.Background(), callRequest(map[string]any{
		"command": `printf sandbox-ok`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Errorf("正常执行不该是 IsError：%s", resultText(t, result))
	}
	if body := resultText(t, result); !strings.Contains(body, "sandbox-ok") {
		t.Errorf("结果里该有命令输出：%s", body)
	}
}

// TestRun工具_策略拒绝是IsError 被策略拦下的命令是「没能跑起来」，该让模型看到原因。
func TestRun工具_策略拒绝是IsError(t *testing.T) {
	impl := &bashServer{sandbox: newTestSandbox(t)}

	result, err := impl.handleRun(context.Background(), callRequest(map[string]any{
		"command": "shutdown -h now",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Error("被策略拒绝该回 IsError")
	}
	if body := resultText(t, result); !strings.Contains(body, "禁用列表") {
		t.Errorf("该说清是被禁用列表拦下：%s", body)
	}
}
