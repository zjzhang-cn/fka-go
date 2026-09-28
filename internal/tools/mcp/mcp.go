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
	"errors"
	"strings"
	"time"
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

// contentToText 结果里的 content 块 → 一段给模型看的文字。
// 非文本块说清它是什么，**不假装是文本**。
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
			parts = append(parts, "[音频 "+block.MimeType+"]")
		case "resource":
			if block.Resource.Text != "" {
				parts = append(parts, block.Resource.Text)
			} else if block.Resource.URI != "" {
				parts = append(parts, "[资源 "+block.Resource.URI+"]")
			}
		default:
			parts = append(parts, block.Raw)
		}
	}

	return strings.Join(parts, "\n")
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
