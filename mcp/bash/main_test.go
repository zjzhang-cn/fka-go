// 本文件钉住**启动方式由参数决定**这条：同一个可执行程序，`--transport` 换成
// sse / http 时监听地址归谁解析、认不出的取值会不会静默退回 stdio。
//
// 静默退回是最坏的形态——「我明明配了 sse」却照常起成 stdio，界面上看不出任何异常。
// 所以认不出的取值必须返回空串，由 run 按用法错退出（2），而不是当默认值用。
package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// TestTransport参数决定启动方式 逐个钉：不给默认 stdio；显式与别名都认；
// **大写也算数**（配置常被手写，大小写不该是坑）；认不出的返回空串。
func TestTransport参数决定启动方式(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  string
		want string
	}{
		{"不给默认 stdio", nil, "", transportStdio},
		{"显式 stdio", []string{"--transport", "stdio"}, "", transportStdio},
		{"显式 sse", []string{"--transport", "sse"}, "", transportSSE},
		{"显式 http", []string{"--transport", "http"}, "", transportHTTP},
		{"等号写法", []string{"--transport=sse"}, "", transportSSE},
		{"大写归一到小写", []string{"--transport", "SSE"}, "", transportSSE},
		{"两侧空白忽略", []string{"--transport", "  http  "}, "", transportHTTP},
		{"环境变量兜底", nil, "sse", transportSSE},
		{"参数压过环境变量", []string{"--transport", "http"}, "sse", transportHTTP},
		{"空环境变量等于没给", nil, "   ", transportStdio},
		{"认不出的取值返空", []string{"--transport", "websocket"}, "", ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("BASH_MCP_TRANSPORT", c.env)
			if got := resolveTransport(c.args); got != c.want {
				t.Errorf("resolveTransport = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestAddr参数决定监听地址 顺序必须是「参数 > 环境变量 > 默认」，且默认**只绑本地**。
func TestAddr参数决定监听地址(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  string
		want string
	}{
		{"不给走默认", nil, "", defaultAddr},
		{"显式地址", []string{"--addr", "0.0.0.0:9000"}, "", "0.0.0.0:9000"},
		{"等号写法", []string{"--addr=:7070"}, "", ":7070"},
		{"环境变量兜底", nil, "127.0.0.1:7777", "127.0.0.1:7777"},
		{"参数压过环境变量", []string{"--addr", ":1"}, ":2", ":1"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("BASH_MCP_ADDR", c.env)
			if got := resolveAddr(c.args); got != c.want {
				t.Errorf("resolveAddr = %q，期望 %q", got, c.want)
			}
		})
	}
}

// Test默认地址只绑回环 默认监听地址不能是 0.0.0.0 / 空主机：这个 server 能在沙盒里
// 跑命令，默认暴露到同网段等于把命令执行权敞开。要对外必须显式写 --addr。
func Test默认地址只绑回环(t *testing.T) {
	os.Unsetenv("BASH_MCP_ADDR")
	if got := resolveAddr(nil); got != "127.0.0.1:8080" {
		t.Errorf("默认地址 = %q，必须是本地回环", got)
	}
}

// TestSSE传输_工具经HTTP能调得动 真的起一个 SSE server、用真客户端连上、把 `run`
// 调起来。`--transport sse` 换的是「谁把 JSON-RPC 送进来」，工具实现一行不改——
// 这条钉的就是那句「一行不改」在真链路上成立，而不是只在注释里成立。
func TestSSE传输_工具经HTTP能调得动(t *testing.T) {
	requireBash(t)

	mcpServer := server.NewMCPServer("fka-bash", "0.1.0",
		server.WithToolCapabilities(true))
	register(mcpServer, newTestSandbox(t), nil)

	httpServer := httptest.NewServer(server.NewSSEServer(mcpServer))
	t.Cleanup(httpServer.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client, err := mcpclient.NewSSEMCPClient(httpServer.URL + "/sse")
	if err != nil {
		t.Fatalf("建 SSE 客户端失败：%v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// SSE 必须显式 Start：那条流要先开起来、服务端把 endpoint 事件发过来
	// （与 internal/tools/mcp 里同一条理由，见 client.go 的注释）
	if err := client.Start(context.WithoutCancel(ctx)); err != nil {
		t.Fatalf("开 SSE 流失败：%v", err)
	}
	if _, err := client.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_LEGACY_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "bash-sse-test", Version: "0.0.1"},
		},
	}); err != nil {
		t.Fatalf("握手失败：%v", err)
	}

	listed, err := client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("列举工具失败：%v", err)
	}
	names := map[string]bool{}
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	if !names[ToolRun] || !names[ToolRead] {
		t.Fatalf("该看到 run 与 read，实际 %v", names)
	}

	result, err := client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      ToolRun,
			Arguments: map[string]any{"command": "echo hello-over-sse"},
		},
	})
	if err != nil {
		t.Fatalf("调用 run 失败：%v", err)
	}
	if result.IsError {
		t.Fatalf("run 不该以工具错误返回：%+v", result.Content)
	}
	if body := mcp.GetTextFromContent(result.Content[0]); !strings.Contains(body, "hello-over-sse") {
		t.Errorf("结果里该有命令输出，实际 %q", body)
	}
}

// TestExecLogDir_落在安装根logs下 执行日志默认落在 <安装根>/logs，FKA_LOG_DIR 可盖。
// 目录必须是**安装根**而不是临时目录：审计日志要能被人找到，且与沙盒根同一套锚点。
func TestExecLogDir_落在安装根logs下(t *testing.T) {
	t.Setenv("FKA_LOG_DIR", "/custom/logs")
	if got := execLogDir(); got != "/custom/logs" {
		t.Errorf("FKA_LOG_DIR 该压过安装根，实际 %q", got)
	}

	t.Setenv("FKA_LOG_DIR", "")
	t.Setenv("FKA_HOME", "/home-x")
	if got := execLogDir(); got != filepath.Join("/home-x", "logs") {
		t.Errorf("该落在 <安装根>/logs，实际 %q", got)
	}
}

// TestHelp参数直接以0退出 帮助必须**早于沙盒构造与 ServeStdio**：晚于沙盒构造时，
// 没装 bwrap 的机器会让 `-h` 以 1 退出；晚于 ServeStdio 时，装了 bwrap 的机器上
// `-h` 会真的起成 stdio server 把进程挂住（测试会一直等）。两种都不该发生。
func TestHelp参数直接以0退出(t *testing.T) {
	// 帮助走 stderr，测试里把它接到 /dev/null，免得刷屏
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = devnull
	defer func() {
		os.Stderr = old
		_ = devnull.Close()
	}()

	for _, args := range [][]string{
		{"-h"},
		{"--help"},
		{"help"},
		{"--transport", "sse", "--help"},
		{"--root", "/definitely/not/a/real/root", "-h"},
	} {
		if code := run(args); code != 0 {
			t.Errorf("%v 该以 0 退出，实际 %d", args, code)
		}
	}
}
