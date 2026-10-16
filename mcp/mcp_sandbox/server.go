// sandbox MCP server 的工具实现。
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
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/zjzhang-cn/fka-go/mcp/mcp_sandbox/internal/log"
)

// 工具名。**刻意不带前缀**——前缀由上层注册表加（`mcp__sandbox__`）。
const (
	ToolRun         = "run"
	ToolRead        = "read"
	ToolExport      = "export"
	ToolImport      = "import"
	ToolImportSkill = "import_skill"
)

// ResourceRootURI 资源根。固定资源 `sandbox:///` 读出本租户文件清单；资源模板
// `sandbox:///{+path}` 按路径读文件。**按租户**：身份仍走 `_meta`，缺身份 fail-closed。
const ResourceRootURI = "sandbox:///"

// runDescription 生成 run 工具的说明。**按部署能力动态拼**：能不能 pip/npm 联网装包
// 取决于这次部署（网络与代理），所以那句不能写死。提成函数也让用例能钉住它必须提到的
// 几件事（脚本执行、装包能力、解释器可能不在）。
func runDescription(allowPip bool) string {
	packages := "注意：沙盒**默认无外网**，`pip install` / `npm install` 装不了包——只能用环境里已经装好的库。"
	if allowPip {
		packages = "本部署允许联网装包：需要时可以用 `pip install <包>` 或 `npm install <包>`（走部署配置好的网络/代理）。"
	}
	return "在一个受限沙盒里执行一条 shell 命令，返回退出码与输出。" +
		"命令的工作目录固定在沙盒根或其相对子目录内；有超时与输出上限；" +
		"危险命令可能被策略拒绝。这是**护栏不是越狱墙**——不要用它去碰系统。" +
		"\n\n需要写逻辑（算数、处理数据、生成文件）时，可以用沙盒里的 python3 或 node 跑脚本：" +
		"先把脚本落盘（如 `printf … > x.py` 或 `cat > x.py <<'PY' … PY`），再 `python3 x.py` / `node x.js`。" +
		packages +
		"若解释器不存在，表现是 `command not found`（这套安装没把它挂进沙盒），换回 shell 或别的办法，别反复重试。"
}

// metaPrincipalKey agent 经 MCP `_meta` 传来的调用方身份键。
//
// **必须与 agent 侧 internal/tools/mcp 的同名常量逐字一致**——sandbox 自给自足
// （边界测试不许 import 树内别的包），所以这串字面量在两边各写一份，改动要一起改。
const metaPrincipalKey = "fka/principal"

// sandboxServer 工具实现。**只有本目录内的 main 与测试用得到**。
//
// ## 多租户：每个 principal 一个沙盒根
//
// `opts.Root` 是**基根**；真正跑命令的根是 `<基根>/<safe(principal)>`，按需创建。
// 身份从 MCP `_meta` 来（agent 注入，模型改不了）——没有身份时默认 **fail-closed**，
// 绝不落到共享目录；只有显式配了 `Options.DefaultPrincipal` 才回落到那个具名租户。
type sandboxServer struct {
	opts    Options
	execLog *ExecLog

	mu        sync.Mutex
	sandboxes map[string]*Sandbox
}

// register 把工具挂到 MCP server 上。`opts.Root` 是基根。
func register(mcpServer *server.MCPServer, opts Options, execLog *ExecLog) {
	s := &sandboxServer{opts: opts, execLog: execLog, sandboxes: map[string]*Sandbox{}}

	mcpServer.AddTool(mcp.NewTool(ToolRun,
		mcp.WithDescription(runDescription(opts.AllowPip)),
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

	// import **不接受模型填的参数**：字节由 agent 自动注入（见 internal/agent 的
	// injectInbound）。所以这里不声明 `files` 字段——让模型看见一个它填不了的参数只会
	// 诱导它瞎编。工具描述说明「把用户本轮发来的图片存进沙盒」即可。
	mcpServer.AddTool(mcp.NewTool(ToolImport,
		mcp.WithDescription(
			"把**用户本轮发来的图片**存进你的沙盒，返回沙盒内的相对路径。之后用 read 查看、"+
				"用 run 处理（转换、识别、裁剪等）。字节由 agent 自动填入，**你不需要也不应该传任何参数**；"+
				"只有在需要把用户刚发的图落到文件上时才调用。"),
	), s.handleImport)

	// import_skill：把**某个技能自带的脚本**落到本租户沙盒的 `skills/<技能>/` 下，
	// 之后用 run 跑、用 read 看。与 import 同一套注入骨架：`files` 由 agent 填
	// （它从技能目录读出字节），模型只填 `skill` 名字。**declared 参数只有 skill**
	// ——让模型看见一个它填不了的 `files` 只会诱导它瞎编（同 import）。
	mcpServer.AddTool(mcp.NewTool(ToolImportSkill,
		mcp.WithDescription(
			"把某个技能自带的脚本落到你的沙盒 `skills/<技能>/` 下，返回可用的相对路径；"+
				"之后用 run 执行（如 `bash skills/<技能>/run.sh`）、用 read 查看。"+
				"`skill` 填系统提示里列出的技能名。字节由 agent 自动填入，**你不必也不应传文件内容**。"+
				"只有要用某个技能自带的脚本时才调用它。"),
		mcp.WithString("skill",
			mcp.Description("技能名，见系统提示里的可用技能清单。"),
			mcp.Required(),
		),
	), s.handleImportSkill)

	// resources：把本租户沙盒里的文件也按 MCP 标准原语暴露出去，方便**别的 MCP 客户端**
	// （如 Claude Desktop 直连 fka-sandbox）列举与读取。读取的合取与 read 工具同一套分类；
	// 身份同样走 `_meta`，缺身份 fail-closed。
	//
	// ⚠️ 列举只能**静态**：SDK 的资源模板没有 list 回调（不像 JS SDK），所以按租户动态列
	// 文件做不到——用一个固定资源 `sandbox:///` 读出来是「本租户文件清单」兜底。
	mcpServer.AddResource(mcp.NewResource(
		ResourceRootURI, "沙盒文件清单",
		mcp.WithResourceDescription("列出本租户沙盒根下的普通文件（相对路径与大小）。"),
		mcp.WithMIMEType("text/plain"),
	), s.handleResourceRoot)

	mcpServer.AddResourceTemplate(mcp.NewResourceTemplate(
		ResourceRootURI+"{+path}", "沙盒文件",
		mcp.WithTemplateDescription("按路径读取本租户沙盒里的一个文件（与 read 工具同一套分类）。"),
		mcp.WithTemplateMIMEType("application/octet-stream"),
	), s.handleResourceFile)
}

// ── 多租户：身份 → 沙盒 ─────────────────────────────────

// sandboxOf 取出这次调用的沙盒。**缺身份 fail-closed**：拿不到身份就拒绝，绝不落到
// 共享目录——那等于「绕过 agent 就全共享」，是多租户下最坏的失败形态。
//
// 唯一例外是显式配了 `Options.DefaultPrincipal`：此时「没带身份」回落到那个具名租户，
// 而不是匿名共享。没配（默认）仍然拒绝。
func (s *sandboxServer) sandboxOf(request mcp.CallToolRequest) (*Sandbox, *mcp.CallToolResult) {
	principal := s.effectivePrincipal(request)
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

// effectivePrincipal 这次调用实际归属的租户：`_meta` 身份 优先，其次配置的
// DefaultPrincipal（默认空）。**模型改不了 `_meta`**，所以默认值也不会被模型改。
func (s *sandboxServer) effectivePrincipal(request mcp.CallToolRequest) string {
	if principal := principalOf(request); principal != "" {
		return principal
	}
	return strings.TrimSpace(s.opts.DefaultPrincipal)
}

// sandboxFor 取该租户的沙盒，没有就**按需建**（懒创建，缓存起来）。
func (s *sandboxServer) sandboxFor(principal string) (*Sandbox, error) {
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
	return principalOfMeta(request.Params.Meta)
}

// principalOfMeta 从 MCP `_meta` 取调用方身份。工具调用与资源读取都走这里。
func principalOfMeta(meta *mcp.Meta) string {
	if meta == nil {
		return ""
	}
	raw, ok := meta.AdditionalFields[metaPrincipalKey]
	if !ok {
		return ""
	}
	value, _ := raw.(string)
	return strings.TrimSpace(value)
}

// sandboxForMeta 按 `_meta` 身份取沙盒（资源读取用）。缺身份 fail-closed。
func (s *sandboxServer) sandboxForMeta(meta *mcp.Meta) (*Sandbox, error) {
	principal := principalOfMeta(meta)
	if principal == "" {
		principal = strings.TrimSpace(s.opts.DefaultPrincipal)
	}
	if principal == "" {
		return nil, errors.New("这次调用没带调用方身份（_meta." + metaPrincipalKey + "），已拒绝。")
	}
	sb, err := s.sandboxFor(principal)
	if err != nil {
		log.Log().Warn("建租户沙盒失败", log.Context{"principal": principal, "error": err.Error()})
		return nil, err
	}
	return sb, nil
}

// ── resources ───────────────────────────────────────────

// resourcePathArg 从 SDK 塞进来的模板变量里取路径。**它可能是 `[]string`**：
// uritemplate 的 `Values` 把每个变量存成字符串列表（`+path` 展开后是 `["inbox/a.png"]`），
// 直接断言成 string 会恒为空——症状是「资源路径为空」。
func resourcePathArg(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []string:
		if len(typed) > 0 {
			return typed[0]
		}
	case []any:
		if len(typed) > 0 {
			first, _ := typed[0].(string)
			return first
		}
	}
	return ""
}

// handleResourceRoot 资源 `sandbox:///`：读出本租户的文件清单（一行一个「相对路径 \t 大小」）。
func (s *sandboxServer) handleResourceRoot(_ context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	sb, err := s.sandboxForMeta(request.Params.Meta)
	if err != nil {
		return nil, err
	}
	listing, err := sb.List()
	if err != nil {
		return nil, err
	}
	return []mcp.ResourceContents{mcp.TextResourceContents{
		URI: ResourceRootURI, MIMEType: "text/plain", Text: listing,
	}}, nil
}

// handleResourceFile 资源模板 `sandbox:///{+path}`：按路径读本租户沙盒里的文件。
// 分类与 read 工具一致：文本 text 块、图片/音频 blob 块、其它二进制一行元信息。
func (s *sandboxServer) handleResourceFile(_ context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	sb, err := s.sandboxForMeta(request.Params.Meta)
	if err != nil {
		return nil, err
	}
	raw := resourcePathArg(request.Params.Arguments["path"])
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("资源路径为空")
	}

	file, err := sb.ReadFile(raw)
	if err != nil {
		return nil, err
	}
	uri := ResourceRootURI + strings.TrimPrefix(filepath.ToSlash(raw), "/")

	switch file.Kind {
	case kindImage:
		return []mcp.ResourceContents{mcp.BlobResourceContents{URI: uri, MIMEType: file.MIME, Blob: file.ImageBase64}}, nil
	case kindAudio:
		return []mcp.ResourceContents{mcp.BlobResourceContents{URI: uri, MIMEType: file.MIME, Blob: file.AudioBase64}}, nil
	case kindBinary:
		return []mcp.ResourceContents{mcp.TextResourceContents{
			URI: uri, MIMEType: "text/plain",
			Text: fmt.Sprintf("（二进制文件 %s，%s；内容未附）", file.MIME, humanBytes(file.Size)),
		}}, nil
	default:
		text := file.Text
		if file.Truncated {
			text += fmt.Sprintf("\n…（超过 %d 字节，已截断）", ReadTextLimit)
		}
		return []mcp.ResourceContents{mcp.TextResourceContents{URI: uri, MIMEType: "text/plain", Text: text}}, nil
	}
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

func (s *sandboxServer) handleRun(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
	principal := s.effectivePrincipal(request)

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
func (s *sandboxServer) recordExec(rec execRecord) {
	s.execLog.Record(rec)
}

// handleRead 把沙盒文件按类别交给模型。**版式对齐 CLI `@引用`**：每个文件一段
//
//	──── 路径 ────
//
// 文本给一个 text 内容块；图片给一个 text 说明 + 一个 **`type:"image"` 内容块**
// （base64 + mimeType），模型因此能直接把图当图看；二进制只说明类型与大小。数据全部
// 经 `CallToolResult` 走 MCP，不依赖 agent 能读本地路径。
func (s *sandboxServer) handleRead(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
// `annotations.audience=["user"]` 表达，agent 据此经渠道送出，**不需要认识 sandbox**。
func (s *sandboxServer) handleExport(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
	mime := detectMIME(raw, data)

	resource := mcp.NewEmbeddedResource(mcp.BlobResourceContents{
		URI:      "sandbox:///" + strings.TrimPrefix(raw, "/"),
		MIMEType: mime,
		Blob:     base64.StdEncoding.EncodeToString(data),
	})
	// **意图就在这一行**：受众是用户。agent 只认这个字段，不认它来自 sandbox。
	resource.Annotations = &mcp.Annotations{Audience: []mcp.Role{mcp.RoleUser}}

	header := fmt.Sprintf("──── %s（%s，%s）────\n（已交给渠道发给用户）",
		raw, mime, humanBytes(info.Size()))
	return &mcp.CallToolResult{Content: []mcp.Content{
		mcp.NewTextContent(header),
		resource,
	}}, nil
}

// ImportMaxBytes import 单文件上限。与 read 无关、与 agent 侧消息层的入站上限一致，
// 免得「网页收下了、沙盒又拒了」。是 var 便于用例调小。
var ImportMaxBytes int64 = 8 << 20

// InboxTTL 入站文件在 `inbox/` 里保留多久，超出即清。**必须有**：FKA_MEDIA_MODE=both/
// sandbox 下每收到一次用户文件就写一份，没有 TTL 会无限堆积。是 var 便于用例调小。
var InboxTTL = 24 * time.Hour

// sweepInbox 删掉 inbox/ 里超过 InboxTTL 的普通文件。**出错就算了**——清理是旁路，
// 不该让它挡住这次导入。
func sweepInbox(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-InboxTTL)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}

// handleImport 把 agent 注进来的入站附件写到本租户沙盒的 `inbox/` 下。
//
// ## 为什么字节走工具参数而不是 `_meta`
//
// `_meta` 给的是**调用方身份**这种小元数据；图片是可能几 MB 的载荷，塞进每次调用的
// `_meta` 不合适。工具参数是载体，且**只有被 agent 点名的落点工具**才拿到字节（见
// internal/agent）。模型手里没有 base64，所以 `files` 是 agent 填的、覆盖任何同名值。
//
// ## 为什么落 `inbox/`
//
// 用户内容与模型自己造的文件分开，便于用户知道「哪些是发进来的」。路径仍受沙盒根
// 约束（词法 + 符号链接），文件名只取 basename、重名自动让位。
func (s *sandboxServer) handleImport(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sb, problem := s.sandboxOf(request)
	if problem != nil {
		return problem, nil
	}

	raw, _ := request.GetArguments()["files"].([]any)
	if len(raw) == 0 {
		return fail("没有可导入的文件：这个工具由 agent 自动填入本轮图片，模型不必传参数。"), nil
	}

	inbox := filepath.Join(sb.Root, "inbox")
	// inbox 也可能被软链顶掉（模型能经 run 造 ln -s）：先过**词法 + 符号链接**双检再建
	// 目录，否则 MkdirAll 会顺着软链把目录建到租户根外面——写文件发生在 server 进程，
	// 不受 bwrap 约束，光靠词法 within 挡不住。
	if _, err := sb.resolveWithin("inbox", "inbox"); err != nil {
		return fail(err.Error()), nil
	}
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		return fail("建 inbox 目录失败：" + err.Error()), nil
	}
	sweepInbox(inbox)

	var written []string
	for index, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		encoded, _ := entry["data"].(string)
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return fail(fmt.Sprintf("第 %d 个文件不是合法的 base64。", index+1)), nil
		}
		if int64(len(data)) > ImportMaxBytes {
			return fail(fmt.Sprintf("第 %d 个文件超过上限 %s。", index+1, humanBytes(ImportMaxBytes))), nil
		}

		name, _ := entry["name"].(string)
		relative := filepath.ToSlash(filepath.Join("inbox", uniqueName(inbox, safeBaseName(name, index))))
		// 与 read 同一套双检：只查词法的话，上一级若是软链，WriteFile 会跟出去
		target, err := sb.resolveWithin(relative, "文件路径")
		if err != nil {
			return fail(err.Error()), nil
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return fail("写入失败：" + err.Error()), nil
		}
		written = append(written, relative)
	}

	return text("已把这些文件存入沙盒：\n- " + strings.Join(written, "\n- ") +
		"\n用 read 查看内容、run 处理（转换/识别/裁剪等）。"), nil
}

// handleImportSkill 把 agent 注进来的技能脚本写到本租户沙盒的 `skills/<技能>/` 下。
//
// ## 为什么与 import 分开
//
// import 落的是「用户本轮发来的东西」，落点是 `inbox/`，文件名只取 basename、
// 重名让位；技能脚本落点是 `skills/<技能>/`，要**保留子目录结构**（`scripts/x.py`）、
// 且**重名覆盖**（技能是安装方维护的，改一次就该生效，不该攒出 `x-1.py`）。
// 两者语义相反，塞进一个工具只会让每个参数都得判一次模式。
//
// ## 命名空间：抗冲突
//
// `safeSegment` 会把非 ASCII 全压成 `_`，于是「备份照片」与「照片备份」会撞成同一个
// 目录、互相覆盖脚本。所以加上原名的短哈希——**同一技能名稳定、不同技能名不撞**。
// 具体路径由返回值给出，模型不必自己拼。
func (s *sandboxServer) handleImportSkill(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sb, problem := s.sandboxOf(request)
	if problem != nil {
		return problem, nil
	}

	skill := strings.TrimSpace(request.GetString("skill", ""))
	if skill == "" {
		return fail("skill 是必填的：要落哪个技能自带的脚本。技能名见系统提示里的清单。"), nil
	}

	raw, _ := request.GetArguments()["files"].([]any)
	if len(raw) == 0 {
		return fail("没有可落地的脚本：这个工具由 agent 自动填入技能脚本，模型只需给 `skill`。"), nil
	}

	skillDir := "skills/" + skillNamespace(skill)
	// 与 import 的 inbox 同理：skills/<命名空间> 若被软链顶掉，MkdirAll 会顺着它在
	// 租户根外面建目录。先双检再建，每个文件落点再各自验一遍。
	if _, err := sb.resolveWithin(skillDir, "技能目录"); err != nil {
		return fail(err.Error()), nil
	}
	dest := filepath.Join(sb.Root, filepath.FromSlash(skillDir))
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fail("建技能目录失败：" + err.Error()), nil
	}

	var written []string
	for index, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		encoded, _ := entry["data"].(string)
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return fail(fmt.Sprintf("第 %d 个脚本不是合法的 base64。", index+1)), nil
		}
		if int64(len(data)) > ImportMaxBytes {
			return fail(fmt.Sprintf("第 %d 个脚本超过上限 %s。", index+1, humanBytes(ImportMaxBytes))), nil
		}

		name, _ := entry["name"].(string)
		cleaned, ok := safeRelPath(name)
		if !ok {
			return fail(fmt.Sprintf("第 %d 个脚本的路径不合法：%q。", index+1, name)), nil
		}
		relative := filepath.ToSlash(filepath.Join(skillDir, cleaned))
		// 只查词法挡不住中间某级是软链：WriteFile 发生在 server 进程（不在 bwrap 内），
		// 会跟着软链写出去。与 read 同一套双检。
		target, err := sb.resolveWithin(relative, "脚本路径")
		if err != nil {
			return fail(err.Error()), nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fail("建脚本目录失败：" + err.Error()), nil
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return fail("写入失败：" + err.Error()), nil
		}
		// 返回**相对沙盒根**的路径：模型直接拿去 run/read，不必知道 skills/ 与命名空间怎么拼
		written = append(written, relative)
	}

	return text("已把技能「" + skill + "」自带的脚本存入沙盒：\n- " + strings.Join(written, "\n- ") +
		"\n用 run 执行（如 `bash " + written[0] + "`）、用 read 查看。"), nil
}

// skillNamespace 技能名 → 沙盒里 `skills/` 下的目录名。
//
// **标准名（safeSegment 无损，即全 ASCII 的字母/数字/`-`/`_`/`.`）直接用原名**——
// 这样标准 Agent Skills 的技能稳定落在 `skills/<name>/`，正文里写的相对路径可预测。
// 有损的名字（中文等会被压成一串 `_`）才补原名的短哈希，避免「备份照片」与
// 「照片备份」撞进同一个目录、互相覆盖脚本。
func skillNamespace(skill string) string {
	sanitized := safeSegment(skill)
	if sanitized == strings.TrimSpace(skill) {
		return sanitized
	}
	digest := sha1.Sum([]byte(skill))
	return sanitized + "-" + fmt.Sprintf("%x", digest)[:8]
}

// safeRelPath 校验一个「技能目录内的相对路径」：拒绝空、绝对、以及任何 `..` 逃逸，
// 但**保留子目录结构**（与 import 的 safeBaseName 不同，后者只取 basename）。
func safeRelPath(name string) (string, bool) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", false
	}
	cleaned := filepath.Clean(filepath.FromSlash(trimmed))
	if cleaned == "." || cleaned == ".." || filepath.IsAbs(cleaned) {
		return "", false
	}
	if strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", false
	}
	return cleaned, true
}

// safeBaseName 取上传文件的安全 basename：去掉路径分隔符，空则给个默认名。
func safeBaseName(name string, index int) string {
	base := filepath.Base(filepath.FromSlash(strings.TrimSpace(name)))
	if base == "" || base == "." || base == string(filepath.Separator) || base == ".." {
		return fmt.Sprintf("upload-%d", index+1)
	}
	return base
}

// uniqueName 在 dir 里没被占用，就在扩展名前加 `-1`、`-2`……避免覆盖同名文件。
func uniqueName(dir, name string) string {
	if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
		return name
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if _, err := os.Stat(filepath.Join(dir, candidate)); os.IsNotExist(err) {
			return candidate
		}
	}
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
