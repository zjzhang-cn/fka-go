package mcp

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/zjzhang-cn/fka-go/internal/config"
)

// samplePNGBase64 一段可以当图片数据的 base64。
var samplePNGBase64 = base64.StdEncoding.EncodeToString([]byte("png-bytes"))

// newEchoServer 一个带 echo 工具的 MCP server。
func newEchoServer() *mcpserver.MCPServer {
	server := mcpserver.NewMCPServer("fka-test", "0.0.1")
	server.AddTool(
		mcpapi.NewTool("echo",
			mcpapi.WithDescription("原样回显"),
			mcpapi.WithString("text", mcpapi.Description("要说的话"))),
		func(_ context.Context, request mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
			return mcpapi.NewToolResultText(request.GetString("text", "（空）")), nil
		},
	)
	return server
}

// newPictureServer 一个带 picture 工具的 MCP server：调用返回一个 image 内容块。
func newPictureServer() *mcpserver.MCPServer {
	server := mcpserver.NewMCPServer("fka-picture", "0.0.1")
	server.AddTool(
		mcpapi.NewTool("picture", mcpapi.WithDescription("回一张图")),
		func(_ context.Context, _ mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
			return mcpapi.NewToolResultImage("图", samplePNGBase64, "image/png"), nil
		},
	)
	return server
}

// serveTestServer 起一个真的 MCP server，带一个会回显的工具。
//
// **两种传输各起一个 httptest**：HTTP 那条路曾经一次都没被真跑过，
// 于是「url 配了没生效」只能靠读代码判断——而它错的两种方式
// （404 session terminated / transport not started yet）都与真实原因无关。
func serveTestServer(t *testing.T, kind string) string {
	t.Helper()

	// 路径**按传输分开**，与真实服务器一致：老 SSE 挂在 /sse，新的挂在 /mcp。
	// 顺带让 pickTransport 的自动那条路也有东西可猜
	var handler http.Handler
	endpoint := "/mcp"
	if kind == transportSSE {
		handler, endpoint = mcpserver.NewSSEServer(newEchoServer()), "/sse"
	} else {
		handler = mcpserver.NewStreamableHTTPServer(newEchoServer())
	}

	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return httpServer.URL + endpoint
}

// TestHTTP服务器_两种传输都能连上并调得动 mcp.json 里写 `url` 却**没写**
// `transport` 是常态，所以自动那条路必须钉住。
func TestHTTP服务器_两种传输都能连上并调得动(t *testing.T) {
	for _, kind := range []string{transportSSE, transportHTTP} {
		t.Run(kind, func(t *testing.T) {
			url := serveTestServer(t, kind)

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			// **故意不写 transport**：让 pickTransport 自己选
			connection, err := ConnectMcpServer(ctx, "remote", ServerConfig{URL: url})
			if err != nil {
				t.Fatalf("该连上：%v", err)
			}
			defer func() { _ = connection.Close() }()

			if got := pickTransport(ServerConfig{URL: url}); got != kind {
				t.Errorf("自动选了 %q，期望 %q", got, kind)
			}

			listed, err := connection.ListTools(ctx)
			if err != nil {
				t.Fatalf("列举工具失败：%v", err)
			}
			if len(listed) != 1 || listed[0].Name != "echo" {
				t.Fatalf("该看到 echo 一个工具，实际 %+v", listed)
			}

			result, err := connection.CallTool(ctx, "echo", map[string]any{"text": "你好"})
			if err != nil {
				t.Fatalf("调用失败：%v", err)
			}
			if !result.OK || result.Content != "你好" {
				t.Errorf("该回显「你好」，实际 OK=%v 内容 %q", result.OK, result.Content)
			}
		})
	}
}

// TestHTTP服务器_显式transport压过自动 猜的那条规则只认 `/sse` 结尾，而真实
// 服务器的路径千奇百怪——所以显式指定必须能覆盖它。
//
// **反过来构造**：把一台 **streamable** 服务器挂在 `/api/sse` 上，于是自动一定
// 猜成 sse（错），只有显式写 `http` 才连得上。失败版本（连不上）要么报 404、
// 要么干等 60 秒，两种都不好看也都不好断；这样钉住的是同一件事，且是条会成功的
// 连接——**顺带证明「猜错了」不是空想**。
func TestHTTP服务器_显式transport压过自动(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/api/sse", mcpserver.NewStreamableHTTPServer(newEchoServer()))
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	url := httpServer.URL + "/api/sse"
	if got := pickTransport(ServerConfig{URL: url}); got != transportSSE {
		t.Fatalf("这条用例自己就失效了：%q 该被猜成 sse，实际 %q", url, got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	connection, err := ConnectMcpServer(ctx, "remote", ServerConfig{URL: url, Transport: transportHTTP})
	if err != nil {
		t.Fatalf("显式写 http 就该连上：%v", err)
	}
	defer func() { _ = connection.Close() }()

	if listed, err := connection.ListTools(ctx); err != nil || len(listed) != 1 {
		t.Errorf("该列出一个工具，实际 %d 个（%v）", len(listed), err)
	}
}

// TestHTTP服务器_headers真的发出去了 `mcp.json` 里写了 `Authorization:`
// 而没发出去，需要鉴权的服务器只会回 401，**而配置读回来是完整的**——
// 一眼看去像是「服务器坏了」。所以让服务器把收到的头记下来。
func TestHTTP服务器_headers真的发出去了(t *testing.T) {
	var (
		mu    sync.Mutex
		seen  string
		inner = mcpserver.NewStreamableHTTPServer(newEchoServer())
	)
	httpServer := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			if value := r.Header.Get("Authorization"); value != "" {
				seen = value
			}
			mu.Unlock()
			inner.ServeHTTP(w, r)
		}))
	t.Cleanup(httpServer.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	connection, err := ConnectMcpServer(ctx, "remote", ServerConfig{
		URL:     httpServer.URL + "/mcp",
		Headers: map[string]string{"Authorization": "Bearer test-token"},
	})
	if err != nil {
		t.Fatalf("该连上：%v", err)
	}
	defer func() { _ = connection.Close() }()

	if _, err := connection.ListTools(ctx); err != nil {
		t.Fatalf("列举工具失败：%v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if seen != "Bearer test-token" {
		t.Errorf("服务器该收到 Authorization: Bearer test-token，实际 %q", seen)
	}
}

// TestHTTP服务器_image块抽成附件 钉住「图片经 MCP 交给模型」这条链路的接缝：服务器
// 返回 `type:"image"` 内容块，客户端必须把它抽成 `CallResult.Images`（data URI），
// 而不是只在文本里留一行「[图片 …]」。真起一个 MCP HTTP server，走完整条解码路径。
func TestHTTP服务器_image块抽成附件(t *testing.T) {
	httpServer := httptest.NewServer(mcpserver.NewStreamableHTTPServer(newPictureServer()))
	t.Cleanup(httpServer.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	connection, err := ConnectMcpServer(ctx, "pic", ServerConfig{URL: httpServer.URL + "/mcp"})
	if err != nil {
		t.Fatalf("该连上：%v", err)
	}
	defer func() { _ = connection.Close() }()

	result, err := connection.CallTool(ctx, "picture", map[string]any{})
	if err != nil {
		t.Fatalf("调用失败：%v", err)
	}
	if len(result.Images) != 1 {
		t.Fatalf("image 块该抽成 1 个附件，实际 %d 个", len(result.Images))
	}
	if want := "data:image/png;base64," + samplePNGBase64; result.Images[0].DataURI != want {
		t.Errorf("DataURI = %q，期望 %q", result.Images[0].DataURI, want)
	}
}

// newMetaServer 一个把收到的 `_meta` 身份原样回显的工具 server。
func newMetaServer() *mcpserver.MCPServer {
	server := mcpserver.NewMCPServer("fka-meta", "0.0.1")
	server.AddTool(
		mcpapi.NewTool("who", mcpapi.WithDescription("回显调用方身份")),
		func(_ context.Context, request mcpapi.CallToolRequest) (*mcpapi.CallToolResult, error) {
			got := ""
			if request.Params.Meta != nil {
				if v, ok := request.Params.Meta.AdditionalFields[MetaPrincipalKey]; ok {
					got, _ = v.(string)
				}
			}
			return mcpapi.NewToolResultText(got), nil
		},
	)
	return server
}

// Test工具调用_带上_meta身份 ctx 上有 principal 时，client 要把它经标准 `_meta`
// 传给 server；没有就不带。**模型改不了这个字段**——bash 的多租户隔离靠它。
func Test工具调用_带上_meta身份(t *testing.T) {
	httpServer := httptest.NewServer(mcpserver.NewStreamableHTTPServer(newMetaServer()))
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	connection, err := ConnectMcpServer(ctx, "meta", ServerConfig{URL: httpServer.URL})
	if err != nil {
		t.Fatalf("该连上：%v", err)
	}
	defer func() { _ = connection.Close() }()

	// 没有身份：server 收到空串
	plain, err := connection.CallTool(ctx, "who", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 空内容会被 client 换成「（工具没有返回内容）」——只要不是身份即可
	if strings.Contains(plain.Content, "web:alice") {
		t.Errorf("ctx 上没有 principal 时不该带身份，实际 %q", plain.Content)
	}

	// 有身份：应经 _meta 传到 server
	bound := config.Bind(ctx, config.Context{"principal": "web:alice"})
	result, err := connection.CallTool(bound, "who", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "web:alice" {
		t.Errorf("身份该经 _meta 传过去，实际 %q", result.Content)
	}
}
