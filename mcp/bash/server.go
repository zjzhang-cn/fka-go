// bash MCP server 的工具实现。
//
// ## 这个 server 提供什么
//
// 一个 **`run` 工具**：在沙盒里执行一条 shell 命令，返回退出码与 stdout/stderr。
// 沙盒的边界（工作目录、超时、输出上限、命令白名单/黑名单）由 sandbox.go 强制，
// 这里只负责把工具参数读出来、把结果说成模型看得懂的一段话。
//
// ## 为什么失败也返 `IsError` 的普通结果，而不是抛异常
//
// 与记忆 server 同一条：抛异常会让模型只看到「工具坏了」，不知道下一步该改什么
// （cwd 越界？命令被禁？超时？）。返回一段点名原因的文字，它下一轮就能自己修。
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/zjzhang-cn/fka-go/mcp/bash/internal/log"
)

// 工具名。**刻意不带前缀**——前缀由上层注册表加（`mcp__bash__`）。
const (
	ToolRun    = "run"
	ToolRead   = "read"
	ToolExport = "export"
)

// metaPrincipalKey agent 经 MCP `_meta` 传来的调用方身份键。
//
// **必须与 agent 侧 internal/tools/mcp 的同名常量逐字一致**——bash 自给自足
// （边界测试不许 import 树内别的包），所以这串字面量在两边各写一份，改动要一起改。
const metaPrincipalKey = "fka/principal"

// bashServer 工具实现。**只有本目录内的 main 与测试用得到**。
//
// ## 多租户：每个 principal 一个沙盒根
//
// `opts.Root` 是**基根**；真正跑命令的根是 `<基根>/<safe(principal)>`，按需创建。
// 身份从 MCP `_meta` 来（agent 注入，模型改不了）——没有身份就 **fail-closed**，
// 绝不落到共享目录。
type bashServer struct {
	opts    Options
	execLog *ExecLog

	mu        sync.Mutex
	sandboxes map[string]*Sandbox
}

// register 把工具挂到 MCP server 上。`opts.Root` 是基根。
func register(mcpServer *server.MCPServer, opts Options, execLog *ExecLog) {
	s := &bashServer{opts: opts, execLog: execLog, sandboxes: map[string]*Sandbox{}}

	mcpServer.AddTool(mcp.NewTool(ToolRun,
		mcp.WithDescription(
			"在一个受限沙盒里执行一条 shell 命令，返回退出码与输出。"+
				"命令的工作目录固定在沙盒根或其相对子目录内；有超时与输出上限；"+
				"危险命令可能被策略拒绝。这是**护栏不是越狱墙**——不要用它去碰系统。"),
		mcp.WithString("command",
			mcp.Description("要执行的 shell 命令（会交给 bash -c）。"),
			mcp.Required(),
		),
		mcp.WithString("cwd",
			mcp.Description("沙盒根下的相对工作目录，默认就是沙盒根。不允许绝对路径或 .. 越界。"),
		),
		mcp.WithNumber("timeout_sec",
			mcp.Description("超时秒数。不填用默认值，超过上限会被夹到上限。"),
		),
	), s.handleRun)

	mcpServer.AddTool(mcp.NewTool(ToolRead,
		mcp.WithDescription(
			"读沙盒里的一个文件，经 MCP 把内容交给模型（与 CLI 的 @引用同一套语义）。"+
				"文本返回 text 内容块（有上限、超出截断）；图片返回带 base64 的 image 内容块；"+
				"其它二进制只回类型与大小。路径必须是沙盒根下的相对路径，越界会被拒。"),
		mcp.WithString("path",
			mcp.Description("沙盒根下的相对文件路径。"),
			mcp.Required(),
		),
	), s.handleRead)

	mcpServer.AddTool(mcp.NewTool(ToolExport,
		mcp.WithDescription(
			"把一个沙盒文件**发给用户**（不是发给模型）。返回的是一段标注了 audience=user "+
				"的资源，agent 会经用户所在的渠道把它送出去。要「把结果文件给用户」时用它；"+
				"只是想自己看内容用 read。路径必须是沙盒根下的相对路径，越界会被拒。"),
		mcp.WithString("path",
			mcp.Description("沙盒根下的相对文件路径。"),
			mcp.Required(),
		),
	), s.handleExport)
}

// ── 多租户：身份 → 沙盒 ─────────────────────────────────

// sandboxOf 取出这次调用的沙盒。**fail-closed**：拿不到身份就拒绝，绝不落到共享
// 目录——那等于「绕过 agent 就全共享」，是多租户下最坏的失败形态。
func (s *bashServer) sandboxOf(request mcp.CallToolRequest) (*Sandbox, *mcp.CallToolResult) {
	principal := principalOf(request)
	if principal == "" {
		return nil, fail("这次调用没带调用方身份（_meta." + metaPrincipalKey + "），已拒绝执行。")
	}
	sb, err := s.sandboxFor(principal)
	if err != nil {
		log.Log().Warn("建租户沙盒失败", log.Context{"principal": principal, "error": err.Error()})
		return nil, fail("建租户沙盒失败：" + err.Error())
	}
	return sb, nil
}

// sandboxFor 取该租户的沙盒，没有就**按需建**（懒创建，缓存起来）。
func (s *bashServer) sandboxFor(principal string) (*Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sb, ok := s.sandboxes[principal]; ok {
		return sb, nil
	}
	opts := s.opts
	opts.Root = filepath.Join(s.opts.Root, safeSegment(principal))
	sb, err := NewSandbox(opts)
	if err != nil {
		return nil, err
	}
	s.sandboxes[principal] = sb
	return sb, nil
}

// principalOf 从 MCP `_meta` 取调用方身份。没有就返回空串。
func principalOf(request mcp.CallToolRequest) string {
	if request.Params.Meta == nil {
		return ""
	}
	raw, ok := request.Params.Meta.AdditionalFields[metaPrincipalKey]
	if !ok {
		return ""
	}
	value, _ := raw.(string)
	return strings.TrimSpace(value)
}

// safeSegment 把 principal 压成能安全当目录名的形式：非法字符换 `_`，长度设限。
//
// 与 internal/llm 那份同名同规则（那边给历史文件用），但**本包不能 import 它**
// （自给自足）——所以各写一份，规则要保持一致。
func safeSegment(value string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, value)
	cleaned = strings.Trim(cleaned, ".")
	if cleaned == "" {
		cleaned = "_unknown"
	}
	if len(cleaned) > 64 {
		cleaned = cleaned[:64]
	}
	return cleaned
}

func (s *bashServer) handleRun(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	command := strings.TrimSpace(request.GetString("command", ""))
	if command == "" {
		return fail("command 是必填的：要执行哪条 shell 命令。"), nil
	}

	// **声明是 number（`mcp.WithNumber`），就必须按数字读。**
	// `GetString` 只在值是 Go `string` 时返回，而 JSON 数字解出来是 `float64`——
	// 于是超时参数会永远取默认值，而界面上看不出任何异常（见 mcp/memory 的同款坑）。
	timeoutSec := request.GetInt("timeout_sec", 0)

	sb, problem := s.sandboxOf(request)
	if problem != nil {
		return problem, nil
	}
	principal := principalOf(request)

	cwd := request.GetString("cwd", "")
	result, err := sb.Run(ctx, RunRequest{
		Command: command,
		Cwd:     cwd,
		Timeout: time.Duration(timeoutSec) * time.Second,
	})
	if err != nil {
		// 策略拒绝 / cwd 越界 / 启动失败：这是**没能跑起来**，不是命令跑完的退出码。
		// 也照样记一条执行日志——「模型试过什么但被挡下」正是审计要看的。
		log.Log().Warn("命令被拒或没能启动", log.Context{"error": err.Error()})
		s.recordExec(execRecord{Principal: principal, Cwd: cwd, Command: command, Error: err.Error()})
		return fail(err.Error()), nil
	}

	rec := execRecord{
		Principal:  principal,
		Cwd:        cwd,
		Command:    command,
		TimedOut:   result.TimedOut,
		Truncated:  result.Truncated,
		DurationMS: result.Duration.Milliseconds(),
	}
	// 超时时命令是被杀的，退出码没有意义，不记（免得 0 被读成「正常结束」）
	if !result.TimedOut {
		code := result.ExitCode
		rec.ExitCode = &code
	}
	s.recordExec(rec)

	body := formatResult(result)
	if result.TimedOut {
		return fail(body), nil
	}
	return text(body), nil
}

// recordExec 落一条执行日志。execLog 为 nil（测试）时静默跳过。
func (s *bashServer) recordExec(rec execRecord) {
	s.execLog.Record(rec)
}

// handleRead 把沙盒文件按类别交给模型。**版式对齐 CLI `@引用`**：每个文件一段
//
//	──── 路径 ────
//
// 文本给一个 text 内容块；图片给一个 text 说明 + 一个 **`type:"image"` 内容块**
// （base64 + mimeType），模型因此能直接把图当图看；二进制只说明类型与大小。数据全部
// 经 `CallToolResult` 走 MCP，不依赖 agent 能读本地路径。
func (s *bashServer) handleRead(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := strings.TrimSpace(request.GetString("path", ""))
	if path == "" {
		return fail("path 是必填的：要读沙盒里的哪个文件。"), nil
	}

	sb, problem := s.sandboxOf(request)
	if problem != nil {
		return problem, nil
	}
	file, err := sb.ReadFile(path)
	if err != nil {
		log.Log().Warn("读沙盒文件失败", log.Context{"error": err.Error()})
		return fail(err.Error()), nil
	}

	switch file.Kind {
	case kindImage:
		// **图片必须是一个独立的 image 内容节点**：base64 数据 + mimeType，不能拼进文本。
		header := fmt.Sprintf("──── %s（%s，%s）────\n（图片已作为内容块发送，请直接查看）",
			file.Path, file.MIME, humanBytes(file.Size))
		return &mcp.CallToolResult{Content: []mcp.Content{
			mcp.NewTextContent(header),
			mcp.NewImageContent(file.ImageBase64, file.MIME),
		}}, nil
	case kindAudio:
		// 音频以 `type:"audio"` 内容节点返回（base64 + mimeType）。
		header := fmt.Sprintf("──── %s（%s，%s）────\n（音频已作为内容块发送）",
			file.Path, file.MIME, humanBytes(file.Size))
		return &mcp.CallToolResult{Content: []mcp.Content{
			mcp.NewTextContent(header),
			mcp.NewAudioContent(file.AudioBase64, file.MIME),
		}}, nil
	case kindBinary:
		return text(fmt.Sprintf("──── %s（%s，%s）────\n（二进制文件，内容未附）",
			file.Path, file.MIME, humanBytes(file.Size))), nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "──── %s ────\n%s", file.Path, file.Text)
		if file.Truncated {
			fmt.Fprintf(&b, "\n…（文件超过 %d 字节，已截断）", ReadTextLimit)
		}
		return text(b.String()), nil
	}
}

// ExportMaxBytes export 单文件上限。比 read 大得多（那是给模型看的），但仍有上限：
// 整份文件要先读进内存、再 base64（+33%），字节是模型指名的。
//
// **是 var 而不是 const**：用例要把它调小，否则验一次超限得造 32MB 的文件
// ——与 send.go 的 maxSendBytes 同一条理由。
var ExportMaxBytes int64 = 32 << 20

// handleExport 把沙盒文件作为**标注给用户**的嵌入资源返回。
//
// ## 为什么用内容块而不是路径
//
// 与 read 同一条理由：server 可能在别的机器上（HTTP/SSE），文件内容只能以数据穿过
// MCP 通道。区别是**受众**——read 给模型看，export 给用户收。这个意图用 MCP 标准的
// `annotations.audience=["user"]` 表达，agent 据此经渠道送出，**不需要认识 bash**。
func (s *bashServer) handleExport(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	raw := strings.TrimSpace(request.GetString("path", ""))
	if raw == "" {
		return fail("path 是必填的：要发给用户的沙盒文件。"), nil
	}

	sb, problem := s.sandboxOf(request)
	if problem != nil {
		return problem, nil
	}
	path, err := sb.ResolveFile(raw)
	if err != nil {
		return fail(err.Error()), nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fail("读不到这个文件：" + raw), nil
	}
	if !info.Mode().IsRegular() {
		return fail("只能发普通文件：" + raw), nil
	}
	if info.Size() > ExportMaxBytes {
		return fail(fmt.Sprintf("文件 %s 超过上限 %s，未导出", humanBytes(info.Size()), humanBytes(ExportMaxBytes))), nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fail("读取失败：" + err.Error()), nil
	}
	mime := http.DetectContentType(data)

	resource := mcp.NewEmbeddedResource(mcp.BlobResourceContents{
		URI:      "sandbox:///" + strings.TrimPrefix(raw, "/"),
		MIMEType: mime,
		Blob:     base64.StdEncoding.EncodeToString(data),
	})
	// **意图就在这一行**：受众是用户。agent 只认这个字段，不认它来自 bash。
	resource.Annotations = &mcp.Annotations{Audience: []mcp.Role{mcp.RoleUser}}

	header := fmt.Sprintf("──── %s（%s，%s）────\n（已交给渠道发给用户）",
		raw, mime, humanBytes(info.Size()))
	return &mcp.CallToolResult{Content: []mcp.Content{
		mcp.NewTextContent(header),
		resource,
	}}, nil
}

// formatResult 把一条命令的结果拼成给模型看的一段话。
//
// **退出码非 0 不算工具失败**：命令跑了、返回了，那是一个正常结果；模型要的正是
// 退出码与 stderr。只有「没能跑起来」和「超时被杀」才走 IsError（在 handler 里判）。
func formatResult(r Result) string {
	var b strings.Builder
	switch {
	case r.TimedOut:
		fmt.Fprintf(&b, "命令超时（跑了 %s）已被终止。\n", r.Duration.Round(time.Millisecond))
	case r.ExitCode != 0:
		fmt.Fprintf(&b, "命令以退出码 %d 结束。\n", r.ExitCode)
	default:
		b.WriteString("命令执行成功（退出码 0）。\n")
	}

	writeStream(&b, "stdout", r.Stdout)
	writeStream(&b, "stderr", r.Stderr)
	if r.Stdout == "" && r.Stderr == "" {
		b.WriteString("（没有输出）\n")
	}
	if r.Truncated {
		b.WriteString("（输出超过上限，已截断）\n")
	}
	return b.String()
}

func writeStream(b *strings.Builder, name, body string) {
	if body == "" {
		return
	}
	b.WriteString(name)
	b.WriteString(":\n")
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteByte('\n')
	}
}

// text 造一条成功结果。
func text(body string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{mcp.NewTextContent(body)}}
}

// fail 造一条**给模型看**的失败。isError=true 让它知道这次没成功。
func fail(body string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{mcp.NewTextContent(body)}}
}
