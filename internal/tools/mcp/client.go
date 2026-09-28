package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcp "github.com/mark3labs/mcp-go/mcp"
)

// clientInfo 我们在握手里报的身份。**版本号与主程序一致**——服务器可能按它做兼容
// 判断，写死一个不会变的字符串会让排查时看不出客户端是哪个版本。
const (
	clientName    = "family-knowledge-agent"
	clientVersion = "0.1.0"
)

// mcpContent 一个 content 块。**刻意自己解而不是用 SDK 的 Content 接口**：
// SDK 的 Content 是带私有方法的 sealed interface，第三方类型实现不了，只能靠
// 类型断言逐个认。这里解成普通结构，未知类型保留原文——
// 「不假装是文本」比「认全所有类型」更重要。
type mcpContent struct {
	Type     string
	Text     string
	MimeType string
	Resource struct {
		URI  string
		Text string
	}
	// Raw 该块的原始 JSON。未知类型时原样交给模型
	Raw string
}

// connection 基于 mark3labs 客户端的一条连接。
type connection struct {
	name   string
	client *mcpclient.Client
}

// newClient 按配置建客户端并完成 initialize 握手。
func newClient(ctx context.Context, cfg ServerConfig) (*mcpclient.Client, error) {
	var (
		client *mcpclient.Client
		err    error
	)

	if cfg.Command != "" {
		// 不给 env 时 SDK 只继承一小撮安全变量（PATH 等）；给了就完全用它——
		// 与 exec 的语义一致，密钥写在 mcp.json 里就能传进去
		env := make([]string, 0, len(cfg.Env))
		for key, value := range cfg.Env {
			env = append(env, key+"="+value)
		}
		// 顺序不定会让「同一份配置起两个进程时环境不同」，按 key 排一下
		sortStrings(env)

		// NOTICE: 这个构造函数**会自动 Start**，连接失败在这里就返回
		client, err = mcpclient.NewStdioMCPClient(cfg.Command, env, cfg.Args...)
	} else {
		client, err = mcpclient.NewStreamableHttpClient(cfg.URL)
	}
	if err != nil {
		return client, fmt.Errorf("起 %s 的连接失败：%w", cfg.describe(), err)
	}

	_, err = client.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: clientName, Version: clientVersion},
		},
	})
	if err != nil {
		return client, fmt.Errorf("%s 握手失败：%w", cfg.describe(), err)
	}

	return client, nil
}

func (c *connection) ListTools(ctx context.Context) ([]ToolInfo, error) {
	return withTimeout(ctx, RequestTimeoutMs, "列举 MCP 工具 "+c.name,
		func(ctx context.Context) ([]ToolInfo, error) {
			result, err := c.client.ListTools(ctx, mcp.ListToolsRequest{})
			if err != nil {
				return nil, err
			}

			out := make([]ToolInfo, 0, len(result.Tools))
			for _, tool := range result.Tools {
				out = append(out, ToolInfo{
					Name:        tool.Name,
					Description: tool.Description,
					// 缺 schema 时给一个空的 object：模型看到 `parameters` 缺失会
					// 当成「无参数工具」而随手编参数，显式空对象更诚实
					InputSchema: toolSchema(tool),
				})
			}
			return out, nil
		})
}

// toolSchema 从 SDK 的 schema 结构里取出 properties/required 那部分。
//
// SDK 用 `invopop/jsonschema` 反射生成 `ToolInputSchema`，直接 json.Marshal 它会
// 把一堆只用于生成的零值字段也带上。所以只取模型真正看得懂的那两个键。
func toolSchema(tool mcp.Tool) map[string]any {
	schema := map[string]any{"type": "object"}

	if len(tool.InputSchema.Properties) > 0 {
		schema["properties"] = tool.InputSchema.Properties
	}
	if len(tool.InputSchema.Required) > 0 {
		schema["required"] = tool.InputSchema.Required
	}
	return schema
}

func (c *connection) CallTool(ctx context.Context, name string, args map[string]any) (CallResult, error) {
	return withTimeout(ctx, RequestTimeoutMs, "调用 MCP 工具 "+c.name+"__"+name,
		func(ctx context.Context) (CallResult, error) {
			if args == nil {
				args = map[string]any{}
			}
			result, err := c.client.CallTool(ctx, mcp.CallToolRequest{
				Params: mcp.CallToolParams{Name: name, Arguments: args},
			})
			if err != nil {
				return CallResult{}, err
			}

			text := contentToText(decodeContent(result.Content))
			return CallResult{
				// isError 是**给模型看的业务失败**（参数不对、查不到），不是协议层错误。
				// 交给模型自己改正，所以这里 ok=false 但不是异常
				OK:      !result.IsError,
				Content: fallbackText(text),
			}, nil
		})
}

func (c *connection) Close() error {
	if c.client == nil {
		return nil
	}
	// stdio 服务器是被我们拉起的子进程：**不 Close 就一直挂着**，
	// fka 退出后它还在跑，白占一份内存与一个文件句柄
	return c.client.Close()
}

// decodeContent 把 SDK 的 Content 接口解成自己的结构。
func decodeContent(content []mcp.Content) []mcpContent {
	out := make([]mcpContent, 0, len(content))

	for _, block := range content {
		encoded, err := json.Marshal(block)
		if err != nil {
			continue
		}
		var decoded mcpContent
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			continue
		}
		decoded.Raw = string(encoded)
		out = append(out, decoded)
	}

	return out
}

func fallbackText(text string) string {
	if text != "" {
		return text
	}
	return "（工具没有返回内容）"
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
