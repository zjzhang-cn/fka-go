// 把各 MCP 服务器暴露的 resources / prompts 汇总成清单，供 `fka list` 展示。
//
// ## 为什么要单独一层
//
// resources / prompts 平时是包成工具的（见 primitives.go），模型按需调用。但
// 「这台服务器到底有哪些资源/提示词」是个**静态事实**，人排查时想看的是清单本身，
// 而不是让模型去调 list_xxx 工具再念回来。所以 source 额外实现 Catalog 这个可选接口，
// 装配根把清单端给 CLI——工具接缝（tools.Source）一行不改。
//
// ## 为什么所有服务器都尽力列，而不是一失败就整体报错
//
// 与连接同一条原则：某台服务器列不出来（老版本没实现 templates/list、进程刚挂）
// 只该让它的条目缺席，别的服务器照常。失败已由 connection 记过日志。
package mcp

import (
	"context"
	"sort"

	"github.com/zjzhang-cn/fka-go/internal/config"
)

// ServerResource 一台服务器暴露的一件资源。Template 为真时 URI 是模板（RFC 6570）。
type ServerResource struct {
	Server      string
	URI         string
	Name        string
	MIMEType    string
	Description string
	Template    bool
}

// ServerPrompt 一台服务器提供的一个提示词。
type ServerPrompt struct {
	Server      string
	Name        string
	Description string
	Arguments   []PromptArg
}

// Catalog 能列出资源/提示词清单的 MCP 源。**可选接口**：只有 *source 实现它，
// 装配根据此把清单交给 CLI；不实现也不影响任何一个工具源。
type Catalog interface {
	Resources(ctx context.Context) []ServerResource
	Prompts(ctx context.Context) []ServerPrompt
}

// Resources 汇总所有服务器声明的资源与资源模板。只认**声明了 resources 能力**的
// 服务器——没声明的调了也只会拿到 method-not-found，没必要制造一条告警。
func (s *source) Resources(ctx context.Context) []ServerResource {
	s.ensure(ctx)
	servers := s.serverSnapshot()

	var out []ServerResource
	for _, server := range servers {
		if !server.connection.Capabilities().Resources {
			continue
		}
		resources, templates, err := server.connection.ListResources(ctx)
		if err != nil {
			config.Log().Warn(config.TypeTOOL, "列举 MCP 资源失败，跳过该服务器",
				config.Context{"server": server.name, "error": err.Error()})
			continue
		}
		for _, item := range resources {
			out = append(out, ServerResource{
				Server: server.name, URI: item.URI, Name: item.Name,
				MIMEType: item.MIMEType, Description: item.Description,
			})
		}
		for _, item := range templates {
			out = append(out, ServerResource{
				Server: server.name, URI: item.URITemplate, Name: item.Name,
				MIMEType: item.MIMEType, Description: item.Description, Template: true,
			})
		}
	}
	return out
}

// Prompts 汇总所有服务器提供的提示词。
func (s *source) Prompts(ctx context.Context) []ServerPrompt {
	s.ensure(ctx)
	servers := s.serverSnapshot()

	var out []ServerPrompt
	for _, server := range servers {
		if !server.connection.Capabilities().Prompts {
			continue
		}
		prompts, err := server.connection.ListPrompts(ctx)
		if err != nil {
			config.Log().Warn(config.TypeTOOL, "列举 MCP 提示词失败，跳过该服务器",
				config.Context{"server": server.name, "error": err.Error()})
			continue
		}
		for _, item := range prompts {
			out = append(out, ServerPrompt{
				Server: server.name, Name: item.Name,
				Description: item.Description, Arguments: item.Arguments,
			})
		}
	}
	return out
}

// serverSnapshot 已连接的服务器，按配置里的原始名字排序（输出稳定，便于比对）。
func (s *source) serverSnapshot() []*connectedServer {
	s.mu.Lock()
	servers := make([]*connectedServer, 0, len(s.connected))
	for _, server := range s.connected {
		servers = append(servers, server)
	}
	s.mu.Unlock()

	sort.Slice(servers, func(i, j int) bool { return servers[i].name < servers[j].name })
	return servers
}
