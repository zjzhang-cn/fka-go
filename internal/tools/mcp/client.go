package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcp "github.com/mark3labs/mcp-go/mcp"
)

// clientInfo 我们在握手里报的身份。**版本号与主程序一致**——服务器可能按它做兼容
// 判断，写死一个不会变的字符串会让排查时看不出客户端是哪个版本。
const (
	clientName    = "family-knowledge-agent"
	clientVersion = "0.1.0"
)

// mcpResource 嵌入式资源（content 块里的 resource.resource）。
type mcpResource struct {
	URI      string
	MimeType string
	// Text 文本型资源的内容
	Text string
	// Blob base64 型的资源内容（mimeType 为非文本时）
	Blob string
}

// mcpContent 一个 content 块。**刻意自己解而不是用 SDK 的 Content 接口**：
// SDK 的 Content 是带私有方法的 sealed interface，第三方类型实现不了，只能靠
// 类型断言逐个认。这里把 MCP **全部**内容类型都解出来（text / image / audio /
// resource / resource_link），未知类型保留原文——「不假装是文本」比「认全所有
// 类型」更重要。
type mcpContent struct {
	Type string
	Text string
	// Data image / audio 块的 base64 数据。
	Data     string
	MimeType string
	// URI / Name 是 resource_link 的顶层字段
	URI  string
	Name string
	// Resource 是嵌入式资源（type=resource）
	Resource mcpResource
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
		// **SDK 是「全量继承 + 追加覆盖」，不是白名单**：它的默认路径与下面的
		// stdioCommand 都是 `cmd.Env = append(os.Environ(), env...)`，cfg.Env
		// 只会追加、同名覆盖。也就是说宿主环境里的一切（含 .env 载入的
		// LLM_API_KEY）子进程都能拿到——外部能力进程默认被**完全信任**；
		// 要划 env 边界得自己换 CommandFunc。密钥写在 mcp.json 里当然也传得进去
		env := make([]string, 0, len(cfg.Env))
		for key, value := range cfg.Env {
			env = append(env, key+"="+value)
		}
		// 顺序不定会让「同一份配置起两个进程时环境不同」，按 key 排一下
		sortStrings(env)

		// 配了 cwd 才接管子进程的构造。没配时走 SDK 默认路径，少一处要跟
		// SDK 保持一致的地方
		var options []transport.StdioOption
		if cfg.Cwd != "" {
			if err := checkWorkDir(cfg.Cwd); err != nil {
				return nil, fmt.Errorf("%s 的工作目录不可用：%w", cfg.describe(), err)
			}
			options = append(options, transport.WithCommandFunc(stdioCommand(cfg.Cwd)))
		}

		// NOTICE: 这个构造函数**会自动 Start**，连接失败在这里就返回
		//（不传 options 时与 NewStdioMCPClient 完全等价，SDK 自己就是这么转的）
		client, err = mcpclient.NewStdioMCPClientWithOptions(cfg.Command, env, cfg.Args, options...)
	} else {
		client, err = newHTTPClient(ctx, cfg)
	}
	if err != nil {
		return client, fmt.Errorf("起 %s 的连接失败：%w", cfg.describe(), err)
	}

	// **报的是 legacy 那个版本，不是 mcp.LATEST_PROTOCOL_VERSION**：
	// 后者在 SDK v1.1.1 起是 `2026-07-28`——无会话的 stateless 协议，
	// 文档明说「没有 initialize 握手」。而我们恰恰在走 initialize 握手，
	// 于是客户端会认定自己 stateless、**不再发 `Mcp-Session-Id`**，
	// 而服务器那边会话已经建好了——第二次请求就得到
	// `session terminated (404). need to re-initialize`。
	// 报 2025-11-25（仍用握手的最新版）让两边对「有没有会话」的说法一致。
	_, err = client.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_LEGACY_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: clientName, Version: clientVersion},
		},
	})
	if err != nil {
		return client, fmt.Errorf("%s 握手失败：%w", cfg.describe(), err)
	}

	return client, nil
}

// stdioCommand 造一个把子进程按指定目录起起来的命令工厂。
//
// **只设 Dir，其余保持 SDK 默认**——不在这里重排 env、不自己接管道，是为了让
// 「配没配 cwd」的唯一差别就是那个工作目录，而不是子进程的其他行为也一起变了。
func stdioCommand(dir string) transport.CommandFunc {
	return func(ctx context.Context, command string, env []string, args []string) (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, command, args...)
		cmd.Env = append(os.Environ(), env...)
		cmd.Dir = dir
		return cmd, nil
	}
}

// checkWorkDir 起进程前先看一眼目录。
//
// 交给 exec 的话，目录不存在只会得到一句 `chdir …: no such file or directory`——
// 同一个进程上还有别的 MCP 服务器在起，看不出是哪个服务器的哪个字段错了。
// 多一次 stat 换一条能定位的消息，划算。
func checkWorkDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("不是目录：" + dir)
	}
	return nil
}

// newHTTPClient 按配置建一个 HTTP 客户端并完成握手。
//
// ## 两种传输在这里分叉，不是「一个 http 客户端」
//
// 老式 SSE：GET 开着一条流，服务端先给一个 `endpoint` 事件告诉你往哪 POST，
// 之后 POST 只回 `202 Accepted`，**真正的响应从那条流上回来**。
// streamable HTTP：直接 POST 那个 url，响应就在响应体里。
// 拿错的表现是 `404 session terminated`——配置看上去完全正确。
//
// ## headers **必须真的传出去**
//
// `mcp.json` 里写了 `Authorization: Bearer …` 而没发出去，需要鉴权的服务器
// 只会回 401，于是**整个服务器被跳过**、而配置读回来是完整的。这类失败
// 一眼看去像是「服务器坏了」，所以两个分支都把 headers 交给 SDK。
func newHTTPClient(ctx context.Context, cfg ServerConfig) (*mcpclient.Client, error) {
	var (
		client *mcpclient.Client
		err    error
	)

	switch pickTransport(cfg) {
	case transportSSE:
		var options []transport.ClientOption
		if len(cfg.Headers) > 0 {
			options = append(options, transport.WithHeaders(cfg.Headers))
		}
		client, err = mcpclient.NewSSEMCPClient(cfg.URL, options...)
		if err == nil {
			// **SSE 必须显式 Start**：那条 GET 流要先开起来、服务端把
			// `endpoint` 事件发过来，客户端才知道往哪 POST。与 stdio 那个
			// 「构造函数自动 Start」的构造器不一样，漏掉这一步的报错是
			// `transport not started yet` —— 一个和真实原因毫无关系的词。
			//
			// ## 为什么 Start 拿到的是「不可取消」的 ctx
			//
			// 那条流要活过**整个连接期**，而连接期的 ctx 在握手一返回就被
			// `ConnectMcpServer` 的 `defer cancel()` 取消了。绑着它的话，
			// `initialize` 之后那条流就没了：本地服务器表现为
			// `Invalid session ID`（它那边会话随流一起关了），
			// 真实服务器则是**等不到响应**、60 秒后超时——
			// 而 initialize 是成功的，所以「握手过了」这个直觉完全帮不上忙。
			//
			// 去掉取消**不等于泄漏**：传输自己记着这条流的 canceler，
			// `Close()` 会收掉它（source 关闭与 fka 退出都会走到）。
			// 端点等待仍有上限——SDK 自己的 30 秒 `endpointTimeout`。
			err = client.Start(context.WithoutCancel(ctx))
		}
	default:
		var options []transport.StreamableHTTPCOption
		if len(cfg.Headers) > 0 {
			options = append(options, transport.WithHTTPHeaders(cfg.Headers))
		}
		client, err = mcpclient.NewStreamableHttpClient(cfg.URL, options...)
	}
	if err != nil {
		// **只把提示缀在 SDK 的错后面**：外层的「起 … 的连接失败」由 newClient
		// 统一加一次。两处各加一遍的话，那句话会出现两次
		return client, fmt.Errorf("%w%s", err, transportHint(cfg))
	}
	return client, nil
}

// transportHint 传输是**猜**出来的时候，把「怎么写才对」放进错误里。
//
// 猜错的那一次是整条链上唯一一次能说出正确答案的机会：再往后用户看到的只有
// 一个 404，而「配了但没生效」是这类问题里最难查的一种。
func transportHint(cfg ServerConfig) string {
	if cfg.Transport != "" {
		return ""
	}
	return fmt.Sprintf("。这台服务器若是老式的 HTTP+SSE，在 mcp.json 里给它写 %q",
		`"transport": "`+transportSSE+`"`)
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

			decoded := decodeContent(result.Content)
			return CallResult{
				// isError 是**给模型看的业务失败**（参数不对、查不到），不是协议层错误。
				// 交给模型自己改正，所以这里 ok=false 但不是异常
				OK:      !result.IsError,
				Content: fallbackText(contentToText(decoded)),
				// 图片块**另立门户**：文字里只留一行「[图片 …]」占位，真正的字节
				// 从这里带出去，由 agent 以一条 user 消息附件发给模型。
				Images: imagesFromContent(decoded),
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
