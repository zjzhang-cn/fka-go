// Package mcp 是 MCP 工具源：把配置里的 MCP 服务器变成模型能调的工具。
//
// ## 一个聚合源，不是「每服务器一个」
//
// 所有服务器都从**一个** Source（ID `mcp`）出，工具短名是 `<server>__<tool>`，
// 模型看到的是 `mcp__<server>__<tool>`。理由：源前缀必须唯一，服务器名超长或
// 归一化后撞名会让注册表**整源跳过**；聚合源的前缀一定唯一。一个 Close 也正好
// 关掉全部连接。
//
// ## 全是 external
//
// MCP 工具会出网或拉起别的进程，一律声明 EffectExternal——默认不放行，要
// LLM_TOOL_EFFECTS=external 才交给模型（见 tools/policy.go）。**不给单个服务器/
// 工具开口子**：那会把审查责任从代码挪到配置里。
//
// ## 连不上就跳过，不拖垮服务
//
// 服务器起不来（命令错、缺 key、网断）只记一条警告并跳过它，别的服务器与其余
// 工具照常。与「技能目录为空」同一条原则：少一批工具不等于服务坏了。连接是
// **惰性**的——第一次列工具时才连，不在启动期阻塞。
package mcp

import (
	"context"
	"encoding/base64"
	"errors"
	"path"
	"strings"
	"time"

	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/llm"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// ToolInfo 一件 MCP 工具。InputSchema 是 JSON Schema，直接当作 Spec.Parameters。
type ToolInfo struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// CallResult 一次 MCP 调用的结果，已归一成工具接缝的 Result 形状。
type CallResult struct {
	OK      bool
	Content string
	// Images 结果里**给模型**的 image 内容块，已转成 data URI 附件。空 = 没有图片。
	Images []llm.ImageAttachment
	// Deliver 结果里**标注给用户**的内容块（audience 含 user）的原始字节。
	// 由循环经渠道发给用户，不进模型上下文。
	Deliver []tools.Attachment
}

// Connection 一条连上的 MCP 连接。
type Connection interface {
	ListTools(ctx context.Context) ([]ToolInfo, error)
	CallTool(ctx context.Context, name string, args map[string]any) (CallResult, error)
	Close() error
}

// ConnectFunc 建立连接的函数。**测试注入假的**就能离线覆盖「连接失败 / 列举 /
// 调用 / 结果映射」，不必真起 MCP 服务器进程。
type ConnectFunc func(ctx context.Context, name string, cfg ServerConfig) (Connection, error)

const (
	// ConnectTimeoutMs 连接与握手的默认上限。服务器卡住不该把一次问答拖死
	ConnectTimeoutMs = 20_000
	// RequestTimeoutMs 单次 tools/list 或 tools/call 的默认上限
	RequestTimeoutMs = 60_000
)

// ConnectMcpServer 连一个 MCP 服务器并做初始化握手。失败返错，由源决定跳过。
func ConnectMcpServer(ctx context.Context, name string, cfg ServerConfig) (Connection, error) {
	connectCtx, cancel := context.WithTimeout(ctx, ConnectTimeoutMs*time.Millisecond)
	defer cancel()

	client, err := newClient(connectCtx, cfg)
	if err != nil {
		// 握手失败/超时都要把子进程收掉，否则 stdio 服务器会一直挂着
		if client != nil {
			_ = client.Close()
		}
		return nil, err
	}

	return &connection{name: name, client: client}, nil
}

// withTimeout 给一次请求套上限。超时返错——由调用方决定跳过还是报给模型。
func withTimeout[T any](ctx context.Context, ms int, what string, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
	defer cancel()

	type outcome struct {
		value T
		err   error
	}
	done := make(chan outcome, 1)

	go func() {
		value, err := fn(callCtx)
		done <- outcome{value: value, err: err}
	}()

	select {
	case result := <-done:
		return result.value, result.err
	case <-callCtx.Done():
		// 底层请求带着 callCtx 走，ctx 一取消它就返回；这里多等一下拿到真正的错误，
		// 免得把「超时」报成「连接被拒」——两者的排查方向完全不同
		result := <-done
		if result.err != nil {
			return zero, errors.New(what + " 超时（" + itoa(ms) + "ms）：" + result.err.Error())
		}
		return zero, errors.New(what + " 超时（" + itoa(ms) + "ms）")
	}
}

// contentToText 结果里的 content 块 → 一段给模型看的文字。**MCP 的每种内容类型
// 都有分支**，非文本块说清它是什么，**不假装是文本、也不静默丢**。
//
// 图片另有 image 附件走（见 imagesFromContent），这里只留一行占位；音频与其它
// 资源块**没法随消息发送**（当前 OpenAI 兼容的内容块只有 text 与 image_url），
// 所以在这里如实说一句「未发送」，而不是让模型以为看到了内容。
func contentToText(content []mcpContent) string {
	parts := make([]string, 0, len(content))

	for _, block := range content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				parts = append(parts, block.Text)
			}
		case "image":
			parts = append(parts, "[图片 "+block.MimeType+"]")
		case "audio":
			parts = append(parts, "[音频 "+block.MimeType+"，当前模型接口不支持音频内容块，未随消息发送]")
		case "resource":
			// 嵌入式资源：文本型直接展开，二进制型只报位置与类型（图片型的字节
			// 另走 imagesFromContent 作为附件）
			switch {
			case block.Resource.Text != "":
				parts = append(parts, block.Resource.Text)
			case block.Resource.URI != "":
				parts = append(parts, "[资源 "+block.Resource.URI+" "+block.Resource.MimeType+"]")
			default:
				parts = append(parts, "[资源]")
			}
		case "resource_link":
			label := block.Name
			if label == "" {
				label = block.URI
			}
			parts = append(parts, "[资源链接 "+label+" "+block.URI+"]")
		default:
			parts = append(parts, block.Raw)
		}
	}

	return strings.Join(parts, "\n")
}

// maxToolImageBytes 一次工具结果里所有图片解出来的总字节上限。
//
// 内容由外部 server 决定，没有上限就等于让它决定这一轮往上下文里塞多少字节
// （失败形态是 OOM 或把上下文撑爆）。超限的图**跳过并记一条 Warn**，其余照常。
const maxToolImageBytes = 5 << 20

// imagesFromContent 把结果里的图片转成附件——包括 `type:"image"` 块，以及 blob 型
// 且 mime 是 image/* 的嵌入式 `resource` 块。**base64 不拼进文本**：真正的字节从
// 这里走，由 agent 以一条 user 消息附件发给模型；文本里 contentToText 另留一行占位。
func imagesFromContent(content []mcpContent) []llm.ImageAttachment {
	var images []llm.ImageAttachment
	total := 0
	for _, block := range content {
		// 标注给用户的块走 Deliver，不给模型——受众是 server 声明的
		if audienceForUser(block) {
			continue
		}
		data, mime := "", ""
		switch block.Type {
		case "image":
			data, mime = block.Data, block.MimeType
		case "resource":
			if strings.HasPrefix(block.Resource.MimeType, "image/") {
				data, mime = block.Resource.Blob, block.Resource.MimeType
			}
		}
		if data == "" {
			continue
		}
		size := base64.StdEncoding.DecodedLen(len(data))
		if size > maxToolImageBytes || total+size > maxToolImageBytes {
			config.Log().Warn(config.TypeSYS, "MCP 结果里的图片超过上限，已跳过",
				config.Context{"mime": mime, "bytes": size, "limit": maxToolImageBytes})
			continue
		}
		total += size
		images = append(images, llm.ImageAttachment{
			Name:    mime,
			DataURI: "data:" + mime + ";base64," + data,
		})
	}
	return images
}

// audienceForUser 该内容块是否标注「给用户」。没写 audience 的按**给模型**算——
// 这与现状一致：现有 server 不写注解，行为不变。
func audienceForUser(block mcpContent) bool {
	if block.Annotations == nil {
		return false
	}
	for _, role := range block.Annotations.Audience {
		if strings.EqualFold(strings.TrimSpace(role), "user") {
			return true
		}
	}
	return false
}

// maxToolDeliverBytes 一次工具结果里「给用户」的附件总量上限。
//
// 与图片上限分开：交付不进上下文，可以大一些；但字节要先解 base64、渠道再加密，
// 峰值是数倍。32 MiB 是这个倍数的折中——渠道侧另外还有 64 MiB 的硬上限。
const maxToolDeliverBytes = 32 << 20

// attachmentsFromContent 把标注给用户的块解成原始字节附件，交给循环发往渠道。
//
// **不认来源、只认 audience**：任何 MCP server（stdio 或 SSE）返回带
// `annotations.audience=["user"]` 的 image / audio / resource blob，都会被交付；
// agent 不知道它来自哪台服务器。这是「沙盒与 agent 协议解耦」的落点。
func attachmentsFromContent(content []mcpContent) []tools.Attachment {
	var out []tools.Attachment
	total := 0

	for _, block := range content {
		if !audienceForUser(block) {
			continue
		}
		encoded, mime, name := "", "", ""
		switch block.Type {
		case "image":
			encoded, mime = block.Data, block.MimeType
		case "audio":
			encoded, mime = block.Data, block.MimeType
		case "resource":
			encoded, mime = block.Resource.Blob, block.Resource.MimeType
			if block.Resource.URI != "" {
				name = path.Base(block.Resource.URI)
			}
		}
		if encoded == "" {
			continue
		}

		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			config.Log().Warn(config.TypeTOOL, "MCP 结果里给用户的附件 base64 解不开，已跳过",
				config.Context{"mime": mime})
			continue
		}
		if total+len(raw) > maxToolDeliverBytes {
			config.Log().Warn(config.TypeTOOL, "MCP 结果里给用户的附件超过上限，已跳过",
				config.Context{"mime": mime, "bytes": len(raw), "limit": maxToolDeliverBytes})
			continue
		}
		total += len(raw)
		out = append(out, tools.Attachment{Name: name, MimeType: mime, Data: raw})
	}

	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := make([]byte, 0, 12)
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
