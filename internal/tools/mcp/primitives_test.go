package mcp

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpapi "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// fakeConn 一个可编程的 Connection：离线覆盖「按能力注册」与「合成工具分发」，
// 不必起真服务器。
type fakeConn struct {
	caps      ServerCapabilities
	tools     []ToolInfo
	resources []ResourceInfo
	templates []ResourceTemplateInfo
	prompts   []PromptInfo
	read      CallResult
	readErr   error
	prompt    CallResult
	promptErr error
}

func (f *fakeConn) ListTools(context.Context) ([]ToolInfo, error) { return f.tools, nil }
func (f *fakeConn) CallTool(context.Context, string, map[string]any) (CallResult, error) {
	return CallResult{OK: true, Content: "工具结果"}, nil
}
func (f *fakeConn) Capabilities() ServerCapabilities { return f.caps }
func (f *fakeConn) ListResources(context.Context) ([]ResourceInfo, []ResourceTemplateInfo, error) {
	return f.resources, f.templates, nil
}
func (f *fakeConn) ReadResource(context.Context, string) (CallResult, error) {
	return f.read, f.readErr
}
func (f *fakeConn) ListPrompts(context.Context) ([]PromptInfo, error) { return f.prompts, nil }
func (f *fakeConn) GetPrompt(context.Context, string, map[string]string) (CallResult, error) {
	return f.prompt, f.promptErr
}
func (f *fakeConn) Close() error { return nil }

func sourceWith(t *testing.T, conn *fakeConn) tools.Source {
	t.Helper()
	return NewSource(SourceOptions{
		Servers: map[string]ServerConfig{"svc": {Command: "x"}},
		Connect: func(context.Context, string, ServerConfig) (Connection, error) { return conn, nil },
	})
}

func specNames(t *testing.T, src tools.Source) []string {
	t.Helper()
	specs, err := src.List(context.Background(), tools.Context{})
	if err != nil {
		t.Fatalf("列举失败：%v", err)
	}
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}

func hasSpec(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestSource_只对声明了的原语生成合成工具 服务器没声明 resources/prompts 时，
// 硬塞工具等于给模型一个永远失败的工具——比起不声明更糟。
func TestSource_只对声明了的原语生成合成工具(t *testing.T) {
	t.Run("都不声明", func(t *testing.T) {
		names := specNames(t, sourceWith(t, &fakeConn{tools: []ToolInfo{{Name: "echo"}}}))
		if !hasSpec(names, "svc__echo") {
			t.Fatalf("真实工具该在：%v", names)
		}
		for _, unwanted := range []string{"svc__list_resources", "svc__read_resource", "svc__list_prompts", "svc__get_prompt"} {
			if hasSpec(names, unwanted) {
				t.Errorf("没声明能力不该有 %s：%v", unwanted, names)
			}
		}
	})

	t.Run("只声明 resources", func(t *testing.T) {
		names := specNames(t, sourceWith(t, &fakeConn{
			caps:  ServerCapabilities{Resources: true},
			tools: []ToolInfo{{Name: "echo"}},
		}))
		for _, want := range []string{"svc__list_resources", "svc__read_resource"} {
			if !hasSpec(names, want) {
				t.Errorf("该有 %s：%v", want, names)
			}
		}
		if hasSpec(names, "svc__list_prompts") || hasSpec(names, "svc__get_prompt") {
			t.Errorf("没声明 prompts 不该有提示词工具：%v", names)
		}
	})

	t.Run("只声明 prompts", func(t *testing.T) {
		names := specNames(t, sourceWith(t, &fakeConn{
			caps:  ServerCapabilities{Prompts: true},
			tools: []ToolInfo{{Name: "echo"}},
		}))
		for _, want := range []string{"svc__list_prompts", "svc__get_prompt"} {
			if !hasSpec(names, want) {
				t.Errorf("该有 %s：%v", want, names)
			}
		}
		if hasSpec(names, "svc__list_resources") {
			t.Errorf("没声明 resources 不该有资源工具：%v", names)
		}
	})
}

// TestSource_合成工具撞名时让位 真实工具先注册：真有一个叫 list_resources 的工具时，
// 合成工具必须退让，而不是覆盖它或报重复。
func TestSource_合成工具撞名时让位(t *testing.T) {
	names := specNames(t, sourceWith(t, &fakeConn{
		caps:  ServerCapabilities{Resources: true},
		tools: []ToolInfo{{Name: "list_resources"}},
	}))
	count := 0
	for _, n := range names {
		if n == "svc__list_resources" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("撞名该只留一个 svc__list_resources，实际 %d 个：%v", count, names)
	}
	// 真实工具优先：它的描述是服务器给的，不是合成的
	specs, _ := sourceWith(t, &fakeConn{
		caps:  ServerCapabilities{Resources: true},
		tools: []ToolInfo{{Name: "list_resources", Description: "服务器自己的"}},
	}).List(context.Background(), tools.Context{})
	for _, s := range specs {
		if s.Name == "svc__list_resources" && s.Description != "服务器自己的" {
			t.Errorf("撞名时真实工具该优先，实际描述 %q", s.Description)
		}
	}
}

// TestSource_read_resource分发 合成工具走各自的方法，不是 CallTool；缺 uri 给模型
// 一句能自己改的话，而不是异常。
func TestSource_read_resource分发(t *testing.T) {
	conn := &fakeConn{
		caps: ServerCapabilities{Resources: true},
		read: CallResult{OK: true, Content: "资源正文"},
	}
	src := sourceWith(t, conn)
	// 触发连接与注册
	_, _ = src.List(context.Background(), tools.Context{})

	result, err := src.Call(context.Background(), "svc__read_resource", map[string]any{"uri": "fka://a"}, tools.Context{})
	if err != nil {
		t.Fatalf("不该返异常：%v", err)
	}
	if !result.OK || result.Content != "资源正文" {
		t.Errorf("该拿到资源正文，实际 OK=%v %q", result.OK, result.Content)
	}

	missing, err := src.Call(context.Background(), "svc__read_resource", map[string]any{}, tools.Context{})
	if err != nil {
		t.Fatalf("不该返异常：%v", err)
	}
	if missing.OK || !strings.Contains(missing.Content, "uri") {
		t.Errorf("缺 uri 该给模型一句可改正的话，实际 OK=%v %q", missing.OK, missing.Content)
	}
}

// TestSource_list_resources分发 列表把 uri 与名字带给模型。
func TestSource_list_resources分发(t *testing.T) {
	conn := &fakeConn{
		caps:      ServerCapabilities{Resources: true},
		resources: []ResourceInfo{{URI: "fka://greeting", Name: "greeting", MIMEType: "text/plain"}},
	}
	src := sourceWith(t, conn)
	_, _ = src.List(context.Background(), tools.Context{})

	result, err := src.Call(context.Background(), "svc__list_resources", map[string]any{}, tools.Context{})
	if err != nil {
		t.Fatalf("不该返异常：%v", err)
	}
	if !strings.Contains(result.Content, "fka://greeting") {
		t.Errorf("列表该带上 uri：%q", result.Content)
	}
}

// TestSource_get_prompt分发 name 从 list_prompts 得到；缺 name 给模型一句可改正的话。
func TestSource_get_prompt分发(t *testing.T) {
	conn := &fakeConn{
		caps:   ServerCapabilities{Prompts: true},
		prompt: CallResult{OK: true, Content: "【user】\n你好\n"},
	}
	src := sourceWith(t, conn)
	_, _ = src.List(context.Background(), tools.Context{})

	result, err := src.Call(context.Background(), "svc__get_prompt", map[string]any{"name": "greet"}, tools.Context{})
	if err != nil {
		t.Fatalf("不该返异常：%v", err)
	}
	if !strings.Contains(result.Content, "你好") {
		t.Errorf("该拿到渲染后的提示词：%q", result.Content)
	}

	missing, _ := src.Call(context.Background(), "svc__get_prompt", map[string]any{}, tools.Context{})
	if missing.OK || !strings.Contains(missing.Content, "name") {
		t.Errorf("缺 name 该给一句可改正的话：OK=%v %q", missing.OK, missing.Content)
	}
}

// ── 端到端：一台真服务器，走 HTTP ───────────────────────

// newResourcePromptServer 一台同时暴露 resource 与 prompt 的 MCP server。
func newResourcePromptServer() *mcpserver.MCPServer {
	server := mcpserver.NewMCPServer("fka-rp", "0.0.1")
	server.AddResource(
		mcpapi.NewResource("fka://greeting", "greeting", mcpapi.WithMIMEType("text/plain")),
		func(context.Context, mcpapi.ReadResourceRequest) ([]mcpapi.ResourceContents, error) {
			return []mcpapi.ResourceContents{
				mcpapi.TextResourceContents{URI: "fka://greeting", MIMEType: "text/plain", Text: "你好，世界"},
			}, nil
		},
	)
	server.AddPrompt(
		mcpapi.NewPrompt("greet",
			mcpapi.WithPromptDescription("打个招呼"),
			mcpapi.WithArgument("name", mcpapi.ArgumentDescription("名字"), mcpapi.RequiredArgument())),
		func(_ context.Context, request mcpapi.GetPromptRequest) (*mcpapi.GetPromptResult, error) {
			return mcpapi.NewGetPromptResult("问候", []mcpapi.PromptMessage{
				mcpapi.NewPromptMessage(mcpapi.RoleUser, mcpapi.NewTextContent("你好 "+request.Params.Arguments["name"])),
			}), nil
		},
	)
	return server
}

// Test端到端_resources与prompts经真服务器 真起一台 HTTP MCP server，验证能力
// 探测、资源读取、提示词渲染三条都通——SDK 那层结构转换最容易在这里出错。
func Test端到端_resources与prompts经真服务器(t *testing.T) {
	httpServer := httptest.NewServer(mcpserver.NewStreamableHTTPServer(newResourcePromptServer()))
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	connection, err := ConnectMcpServer(ctx, "rp", ServerConfig{URL: httpServer.URL + "/mcp"})
	if err != nil {
		t.Fatalf("该连上：%v", err)
	}
	defer func() { _ = connection.Close() }()

	caps := connection.Capabilities()
	if !caps.Resources || !caps.Prompts {
		t.Fatalf("该探测到 resources 与 prompts，实际 %+v", caps)
	}

	resources, _, err := connection.ListResources(ctx)
	if err != nil {
		t.Fatalf("列资源失败：%v", err)
	}
	if len(resources) != 1 || resources[0].URI != "fka://greeting" {
		t.Fatalf("该看到一个资源，实际 %+v", resources)
	}

	read, err := connection.ReadResource(ctx, "fka://greeting")
	if err != nil {
		t.Fatalf("读资源失败：%v", err)
	}
	if !strings.Contains(read.Content, "你好，世界") {
		t.Errorf("读到的内容不对：%q", read.Content)
	}

	prompts, err := connection.ListPrompts(ctx)
	if err != nil {
		t.Fatalf("列提示词失败：%v", err)
	}
	if len(prompts) != 1 || prompts[0].Name != "greet" {
		t.Fatalf("该看到一个 prompt，实际 %+v", prompts)
	}
	if len(prompts[0].Arguments) != 1 || !prompts[0].Arguments[0].Required {
		t.Errorf("该带一个必填参数：%+v", prompts[0].Arguments)
	}

	prompt, err := connection.GetPrompt(ctx, "greet", map[string]string{"name": "小明"})
	if err != nil {
		t.Fatalf("取提示词失败：%v", err)
	}
	if !strings.Contains(prompt.Content, "小明") {
		t.Errorf("渲染结果里该有参数值：%q", prompt.Content)
	}
	if !strings.Contains(prompt.Content, "user") {
		t.Errorf("渲染结果该标出角色：%q", prompt.Content)
	}
}

// TestURI模板取值 SDK 把 RFC 6570 模板解析成结构，工具说明里要的是可读回原文。
func TestURI模板取值(t *testing.T) {
	tpl := mcpapi.NewResourceTemplate("sandbox:///{path}", "沙盒文件")
	if got := uriTemplateString(tpl.URITemplate); got != "sandbox:///{path}" {
		t.Errorf("= %q，期望 sandbox:///{path}", got)
	}
	if got := uriTemplateString(nil); got != "" {
		t.Errorf("nil 该返空：%q", got)
	}
}

// TestArgStringMap 非字符串值丢弃：MCP 的 prompt 参数按规范就是字符串。
func TestArgStringMap(t *testing.T) {
	got := argStringMap(map[string]any{"arguments": map[string]any{"a": "1", "b": 2}}, "arguments")
	if len(got) != 1 || got["a"] != "1" {
		t.Errorf("该只留字符串值：%+v", got)
	}
	if argStringMap(nil, "arguments") != nil {
		t.Errorf("nil 参数该返 nil")
	}
}
