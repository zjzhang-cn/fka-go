// MCP resources 与 prompts 两个原语的客户端实现，以及把它们包成工具的那一层。
//
// ## 为什么不把资源/提示词塞进 system prompt
//
// 最省事的做法是连上后把「这台服务器有哪些 resource/prompt」写进系统提示词。source.go
// 的 PromptSection 刻意不这么做：**服务器名与内容是配置里的、可被外部影响的文本**，
// 进 system prompt 等于给了一段能改写行为的东西。包成工具则一切走模型的显式调用，
// 与其它 MCP 工具同一条 `external` 闸门。
//
// ## 为什么按 Capabilities 注册
//
// MCP 规范里，server 不声明 resources/prompts 就是**没实现**这两个方法——调用会拿到
// method-not-found。硬注册会让模型看见一个永远失败的工具，比不注册更糟。所以只对
// initialize 握手里声明了的原语生成工具。
//
// ## 结果复用 CallResult
//
// read_resource / get_prompt 的产出与工具结果同形状：文本、图片、以及带 audience 的
// 交付。全部经 mcpContent 归一，直接复用 contentToText / imagesFromContent /
// attachmentsFromContent 三条既有通道，不另起一套解释。
package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// 合成工具名（源内短名，前缀 `mcp__<server>__` 由注册表加）。
const (
	toolListResources = "list_resources"
	toolReadResource  = "read_resource"
	toolListPrompts   = "list_prompts"
	toolGetPrompt     = "get_prompt"
)

// ── 客户端：把 SDK 的方法收成自己的形状 ──────────────────

// Capabilities 服务器在握手里声明的原语。
func (c *connection) Capabilities() ServerCapabilities {
	caps := c.client.GetServerCapabilities()
	return ServerCapabilities{
		Resources: caps.Resources != nil,
		Prompts:   caps.Prompts != nil,
	}
}

// resourceListing withTimeout 只收一个返回值，资源与模板两列就打成这个结构。
type resourceListing struct {
	resources []ResourceInfo
	templates []ResourceTemplateInfo
}

// ListResources 列资源与资源模板。**两者都尽力而为**：一个失败不改写另一个的结论，
// 只有两个都失败才返错——有些 server 只实现其中一个。
func (c *connection) ListResources(ctx context.Context) ([]ResourceInfo, []ResourceTemplateInfo, error) {
	listing, err := withTimeout(ctx, RequestTimeoutMs, "列举 MCP 资源 "+c.name,
		func(ctx context.Context) (resourceListing, error) {
			var out resourceListing
			listed, resErr := c.client.ListResources(ctx, mcp.ListResourcesRequest{})
			if resErr != nil {
				config.Log().Warn(config.TypeTOOL, "列举 MCP 资源失败",
					config.Context{"server": c.name, "error": resErr.Error()})
			} else {
				for _, r := range listed.Resources {
					out.resources = append(out.resources, ResourceInfo{
						URI:         r.URI,
						Name:        firstNonEmpty(r.Name, r.Title),
						MIMEType:    r.MIMEType,
						Description: r.Description,
					})
				}
			}

			tplList, tplErr := c.client.ListResourceTemplates(ctx, mcp.ListResourceTemplatesRequest{})
			if tplErr != nil {
				config.Log().Warn(config.TypeTOOL, "列举 MCP 资源模板失败",
					config.Context{"server": c.name, "error": tplErr.Error()})
			} else {
				for _, t := range tplList.ResourceTemplates {
					out.templates = append(out.templates, ResourceTemplateInfo{
						URITemplate: uriTemplateString(t.URITemplate),
						Name:        firstNonEmpty(t.Name, t.Title),
						MIMEType:    t.MIMEType,
						Description: t.Description,
					})
				}
			}

			if resErr != nil && tplErr != nil {
				return resourceListing{}, fmt.Errorf("列举资源失败（%v）；列举资源模板也失败（%v）", resErr, tplErr)
			}
			return out, nil
		})
	if err != nil {
		return nil, nil, err
	}
	return listing.resources, listing.templates, nil
}

// ReadResource 按 uri 读一个资源。结果里的每个内容节点转成标准 mcpContent，
// 于是图片走 imagesFromContent、标注给用户的块走 attachmentsFromContent。
func (c *connection) ReadResource(ctx context.Context, uri string) (CallResult, error) {
	return withTimeout(ctx, RequestTimeoutMs, "读 MCP 资源 "+c.name,
		func(ctx context.Context) (CallResult, error) {
			request := mcp.ReadResourceRequest{Params: mcp.ReadResourceParams{URI: uri}}
			request.Params.Meta = principalMeta(ctx)
			result, err := c.client.ReadResource(ctx, request)
			if err != nil {
				return CallResult{}, err
			}
			return callResultFromBlocks(resourceBlocks(result.Contents)), nil
		})
}

// ListPrompts 列提示词模板。
func (c *connection) ListPrompts(ctx context.Context) ([]PromptInfo, error) {
	return withTimeout(ctx, RequestTimeoutMs, "列举 MCP 提示词 "+c.name,
		func(ctx context.Context) ([]PromptInfo, error) {
			listed, err := c.client.ListPrompts(ctx, mcp.ListPromptsRequest{})
			if err != nil {
				return nil, err
			}
			out := make([]PromptInfo, 0, len(listed.Prompts))
			for _, p := range listed.Prompts {
				info := PromptInfo{Name: p.Name, Description: firstNonEmpty(p.Description, p.Title)}
				for _, a := range p.Arguments {
					info.Arguments = append(info.Arguments, PromptArg{
						Name:        a.Name,
						Description: a.Description,
						Required:    a.Required,
					})
				}
				out = append(out, info)
			}
			return out, nil
		})
}

// GetPrompt 渲染一个提示词模板。消息的角色（user/assistant）**保留在文本里**：
// 结果最终是一条工具消息，不标出角色，模型就分不清哪段是它自己该说的。
//
// ## 为什么不直接用 SDK 的 client.GetPrompt
//
// SDK 的 `GetPromptParams.Arguments` 带 `omitempty`，参数为空时 `arguments` 会被整个
// 从 JSON 里丢掉。而官方 TS SDK 的 server 只要 prompt 声明了参数 schema，收到**缺省**
// 的 `arguments` 就直接报 `expected object, received undefined`——于是「不传参数」这个
// 完全正常的用法反而失败。空参数必须发成 `{}`，SDK 的结构体给不了这个，只能自己拼
// 一个 `arguments` 不带 `omitempty` 的参数对象，经底层 transport 发同一帧 `prompts/get`。
//
// 代价是绕过了 SDK 的多轮往返（elicitation）处理。prompt 基本不触发它，这里认了。
func (c *connection) GetPrompt(ctx context.Context, name string, args map[string]string) (CallResult, error) {
	return withTimeout(ctx, RequestTimeoutMs, "取 MCP 提示词 "+c.name,
		func(ctx context.Context) (CallResult, error) {
			result, err := c.rawGetPrompt(ctx, name, args)
			if err != nil {
				return CallResult{}, err
			}

			var blocks []mcpContent
			var text strings.Builder
			for _, message := range result.Messages {
				decoded := decodeContent([]mcp.Content{message.Content})
				body := contentToText(decoded)
				if body != "" {
					fmt.Fprintf(&text, "【%s】\n%s\n", message.Role, body)
				}
				blocks = append(blocks, decoded...)
			}

			out := callResultFromBlocks(blocks)
			if text.Len() > 0 {
				out.Content = text.String()
			}
			return out, nil
		})
}

// getPromptParams prompts/get 的参数。`Arguments` **不带 omitempty**——这正是这个类型
// 存在的理由：空参数也要序列化成 `{}`（见 GetPrompt 头注释）。
type getPromptParams struct {
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments"`
	Meta      *mcp.Meta         `json:"_meta,omitempty"`
}

// promptRequestSeq 自己发 `prompts/get` 时用的请求 id 序号。用**字符串** id（SDK 只发
// 数字 id），两条 id 空间不会撞，底层按 `RequestId.String()` 匹配响应。
var promptRequestSeq atomic.Uint64

// rawGetPrompt 经底层 transport 发一帧 prompts/get。
func (c *connection) rawGetPrompt(ctx context.Context, name string, args map[string]string) (*mcp.GetPromptResult, error) {
	if args == nil {
		args = map[string]string{}
	}
	request := transport.JSONRPCRequest{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      mcp.NewRequestId("fka-prompt-" + strconv.FormatUint(promptRequestSeq.Add(1), 10)),
		Method:  string(mcp.MethodPromptsGet),
		Params: getPromptParams{
			Name:      name,
			Arguments: args,
			Meta:      principalMeta(ctx),
		},
	}
	response, err := c.client.GetTransport().SendRequest(ctx, request)
	if err != nil {
		return nil, err
	}
	if response.Error != nil {
		return nil, response.Error.AsError()
	}
	return mcp.ParseGetPromptResult(&response.Result)
}

// principalMeta 把调用方身份塞进 `_meta`，与 CallTool 同一条（模型碰不到）。
// 没有身份时返回 nil——不写一个空对象，免得把「无身份」和「有身份但空」混淆。
func principalMeta(ctx context.Context) *mcp.Meta {
	principal := principalOf(ctx)
	if principal == "" {
		return nil
	}
	return mcp.NewMetaFromMap(map[string]any{MetaPrincipalKey: principal})
}

// resourceBlocks 把 SDK 的资源内容节点转成 mcpContent。SDK 会把内容解成值类型，
// 但接口两边都实现得了（值 / 指针），两种都认，免得漏掉一种。
func resourceBlocks(contents []mcp.ResourceContents) []mcpContent {
	out := make([]mcpContent, 0, len(contents))
	for _, item := range contents {
		switch value := item.(type) {
		case mcp.TextResourceContents:
			out = append(out, resourceBlock(value.URI, value.MIMEType, value.Text, ""))
		case *mcp.TextResourceContents:
			out = append(out, resourceBlock(value.URI, value.MIMEType, value.Text, ""))
		case mcp.BlobResourceContents:
			out = append(out, resourceBlock(value.URI, value.MIMEType, "", value.Blob))
		case *mcp.BlobResourceContents:
			out = append(out, resourceBlock(value.URI, value.MIMEType, "", value.Blob))
		}
	}
	return out
}

func resourceBlock(uri, mime, text, blob string) mcpContent {
	return mcpContent{Type: "resource", Resource: mcpResource{URI: uri, MimeType: mime, Text: text, Blob: blob}}
}

// callResultFromBlocks 把一组 mcpContent 归成 CallResult，复用既有三条通道。
func callResultFromBlocks(blocks []mcpContent) CallResult {
	return CallResult{
		OK:      true,
		Content: fallbackText(contentToText(blocks)),
		Images:  imagesFromContent(blocks),
		Deliver: attachmentsFromContent(blocks),
	}
}

// uriTemplateString 取 URI 模板的原始字符串。SDK 把 RFC 6570 模板解析成结构，
// 这里要的是可读回原文——Raw() 正是它 JSON 序列化时用的那个值。
func uriTemplateString(t *mcp.URITemplate) string {
	if t == nil || t.Template == nil {
		return ""
	}
	return t.Raw()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ── source 端：注册与分发 ────────────────────────────────

// addPrimitiveTools 按服务器声明的能力补上合成工具。**必须在持有 s.mu 时调用**，
// 因为它与真实工具共用 specs / toolIndex。真实工具先注册，撞名时合成工具让位。
func (s *source) addPrimitiveTools(key, name string, caps ServerCapabilities) {
	add := func(toolName, description string, schema map[string]any, kind toolKind) {
		shortName := key + tools.ToolNameSeparator + toolName
		if _, exists := s.toolIndex[shortName]; exists {
			config.Log().Warn(config.TypeSYS, "MCP 合成工具名与已有工具重复，已跳过",
				config.Context{"tool": shortName, "server": name})
			return
		}
		s.toolIndex[shortName] = toolLocator{serverKey: key, toolName: toolName, kind: kind}
		s.specs = append(s.specs, tools.Spec{
			Name:        shortName,
			Description: description,
			Parameters:  schema,
			Effect:      tools.EffectExternal,
		})
	}

	if caps.Resources {
		add(toolListResources, listResourcesDescription, emptySchema(), kindListResources)
		add(toolReadResource, readResourceDescription, uriSchema(), kindReadResource)
	}
	if caps.Prompts {
		add(toolListPrompts, listPromptsDescription, emptySchema(), kindListPrompts)
		add(toolGetPrompt, getPromptDescription, promptSchema(), kindGetPrompt)
	}
}

// callPrimitive 分发四个合成工具。返 (Result, nil) 表示「调用完成、结果给模型看」，
// 与 CallTool 同一条约定：失败包成 FailResult 而不是异常。
func (s *source) callPrimitive(ctx context.Context, conn Connection, locator toolLocator, args map[string]any) (tools.Result, error) {
	switch locator.kind {
	case kindListResources:
		resources, templates, err := conn.ListResources(ctx)
		if err != nil {
			config.Log().Warn(config.TypeTOOL, "MCP 列举资源失败", config.Context{"error": err.Error()})
			return tools.FailResult("列举资源失败：%s", err.Error()), nil
		}
		return tools.OKResult(formatResources(resources, templates)), nil

	case kindReadResource:
		uri := argString(args, "uri")
		if uri == "" {
			return tools.FailResult("uri 是必填的：要读哪个 resource（先用 %s 查）。", toolListResources), nil
		}
		result, err := conn.ReadResource(ctx, uri)
		if err != nil {
			config.Log().Warn(config.TypeTOOL, "MCP 读资源失败",
				config.Context{"uri": uri, "error": err.Error()})
			return tools.FailResult("读资源失败：%s", err.Error()), nil
		}
		return toToolsResult(result), nil

	case kindListPrompts:
		prompts, err := conn.ListPrompts(ctx)
		if err != nil {
			config.Log().Warn(config.TypeTOOL, "MCP 列举提示词失败", config.Context{"error": err.Error()})
			return tools.FailResult("列举提示词失败：%s", err.Error()), nil
		}
		return tools.OKResult(formatPrompts(prompts)), nil

	case kindGetPrompt:
		name := argString(args, "name")
		if name == "" {
			return tools.FailResult("name 是必填的：要取哪个 prompt（先用 %s 查）。", toolListPrompts), nil
		}
		result, err := conn.GetPrompt(ctx, name, argStringMap(args, "arguments"))
		if err != nil {
			config.Log().Warn(config.TypeTOOL, "MCP 取提示词失败",
				config.Context{"prompt": name, "error": err.Error()})
			return tools.FailResult("取提示词失败：%s", err.Error()), nil
		}
		return toToolsResult(result), nil
	}

	return tools.FailResult("内部错误：未知的合成工具类型。"), nil
}

// toToolsResult 把 CallResult 搬进工具接缝的形状。
func toToolsResult(result CallResult) tools.Result {
	return tools.Result{
		OK:      result.OK,
		Content: result.Content,
		Images:  result.Images,
		Deliver: result.Deliver,
	}
}

// formatResources 把资源与模板拼成一段给模型看的清单。
func formatResources(resources []ResourceInfo, templates []ResourceTemplateInfo) string {
	if len(resources) == 0 && len(templates) == 0 {
		return "这个服务没有暴露任何 resource。"
	}

	var b strings.Builder
	if len(resources) > 0 {
		b.WriteString("可读资源（用 " + toolReadResource + " 读，参数 uri）：\n")
		for _, r := range resources {
			writeInfoLine(&b, firstNonEmpty(r.Name, r.URI), r.URI, r.MIMEType, r.Description)
		}
	}
	if len(templates) > 0 {
		b.WriteString("资源模板（把占位符换成实际值得到 uri，再用 " + toolReadResource + " 读）：\n")
		for _, t := range templates {
			writeInfoLine(&b, firstNonEmpty(t.Name, t.URITemplate), t.URITemplate, t.MIMEType, t.Description)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatPrompts 把提示词模板拼成一段给模型看的清单。
func formatPrompts(prompts []PromptInfo) string {
	if len(prompts) == 0 {
		return "这个服务没有提供任何 prompt。"
	}

	var b strings.Builder
	b.WriteString("提示词模板（用 " + toolGetPrompt + " 取，参数 name / arguments）：\n")
	for _, p := range prompts {
		line := "- " + p.Name
		if p.Description != "" {
			line += "：" + p.Description
		}
		if len(p.Arguments) > 0 {
			names := make([]string, 0, len(p.Arguments))
			for _, a := range p.Arguments {
				label := a.Name
				if a.Required {
					label += "（必填）"
				}
				if a.Description != "" {
					label += " " + a.Description
				}
				names = append(names, label)
			}
			line += "；参数：" + strings.Join(names, "、")
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func writeInfoLine(b *strings.Builder, label, uri, mime, description string) {
	line := "- " + label
	if uri != "" && uri != label {
		line += " — " + uri
	}
	if mime != "" {
		line += "（" + mime + "）"
	}
	if description != "" {
		line += "：" + description
	}
	b.WriteString(line + "\n")
}

// ── 参数读取与 schema ────────────────────────────────────

func argString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	value, _ := args[key].(string)
	return strings.TrimSpace(value)
}

// argStringMap 读一个「字符串→字符串」的对象参数（prompt 的 arguments）。
// 非字符串的值丢弃：MCP 的 prompt 参数按规范就是字符串。
func argStringMap(args map[string]any, key string) map[string]string {
	if args == nil {
		return nil
	}
	raw, ok := args[key].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func emptySchema() map[string]any {
	return map[string]any{"type": "object"}
}

func uriSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"uri": map[string]any{
				"type":        "string",
				"description": "要读的 resource 的 uri（从 " + toolListResources + " 得到，或按资源模板拼出）。",
			},
		},
		"required": []string{"uri"},
	}
}

func promptSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "要取的 prompt 名字（从 " + toolListPrompts + " 得到）。",
			},
			"arguments": map[string]any{
				"type":                 "object",
				"description":          "该 prompt 的参数，键是参数名、值是字符串。",
				"additionalProperties": map[string]any{"type": "string"},
			},
		},
		"required": []string{"name"},
	}
}

const (
	listResourcesDescription = "列出这个外部服务通过 MCP 暴露的 resources 与资源模板（uri、名字、类型、说明）。" +
		"resource 是服务端可读的数据；拿到 uri 后用 " + toolReadResource + " 读它的内容。本工具只列不读。"
	readResourceDescription = "按 uri 读取一个 MCP resource 的内容。文本直接返回；图片作为内容块返回。" +
		"uri 从 " + toolListResources + " 得到，或按资源模板拼出。"
	listPromptsDescription = "列出这个外部服务提供的 MCP prompts（名字、说明、参数）。" +
		"prompt 是服务端预置的提示词模板，用 " + toolGetPrompt + " 渲染出消息。"
	getPromptDescription = "按名字渲染一个 MCP prompt（可带 arguments），返回渲染后的消息（含角色）。" +
		"名字从 " + toolListPrompts + " 得到。"
)
