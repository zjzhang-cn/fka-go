// Package reply 把「Bot 回话」这件事做成工具，交给模型调度。
//
// ## 为什么这是**唯一**的内置工具源
//
// 这个 agent 的原则是「不带任何内置能力，能力只从 MCP 与 skill 进来」——
// 文档、记忆、检索、执行全在别的进程里。`reply` 是**有意开的一个例外**，因为
// 「往当前会话回话」不是一项外部能力，而是**输出通道**：最终答案本来就走这条路，
// 只是把同样的出口也交给模型，让它能先发一段说明再发文件、或分几条发。
//
// 它不拥有任何数据、不出网、不拉进程——所以没有破坏那条原则：能力仍然只从
// MCP 与 skill 进来，这里只是把「回话」的调度权交还给模型。
//
// ## 它是「输出通道」，所以必须有人给它定边界
//
// `File`/`Image` 收的是**本机路径**。模型给的路径如果不受限，就等于给了一个
// 任意文件外带口（`/etc/passwd`、密钥、别处的数据），而提示注入是这份产品的
// 结构性暴露。所以这里的每次调用都要过三道闸（见 `resolve`）：
//
//   - 只认**相对路径**，`..` 与绝对路径一律拒；
//   - 求值符号链接后必须仍在根内（挡 `link -> /etc`）；
//   - 只发**普通文件**、且不超过大小上限。
//
// 根默认是 `<安装根>/sandbox`——与 `fka-bash` 的沙盒根同一个目录，于是「模型用
// bash 造一个文件、再把它发出去」天然成立，而模型能发的也只有它本就能读写的那些
// 文件。要放宽（例如允许发某个共享目录）就设 `FKA_SEND_ROOT`。
//
// ## 默认关着
//
// 三个工具的 effect 都是 `send`，而 `LLM_TOOL_EFFECTS` 默认只有 `read`——
// 不给 `send` 就一个都看不到。这与 `policy.go` 的「少给一个工具不会出错，
// 多给一个会」一致：能主动往用户那边发东西，是部署方要显式打开的。
package reply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// 工具名。**刻意不带前缀**——前缀由注册表加（`reply__`）。
const (
	ToolText  = "send_text"
	ToolFile  = "send_file"
	ToolImage = "send_image"
)

// SourceID 源前缀，出现在工具名里（`reply__send_text`）。
const SourceID = "reply"

// DefaultMaxBytes 单文件大小上限。与 iLink 出站的上限（64MiB）取齐：
// 比它再大这里放行、渠道那边也会拒，不如在这一层早说。
const DefaultMaxBytes int64 = 64 << 20

// EnvRoot 覆盖发送根的环境变量。
const EnvRoot = "FKA_SEND_ROOT"

// sandboxRootEnv 与 `fka-bash` 共用的沙盒根环境变量。默认根跟着它走，
// 好让「bash 造文件、reply 发文件」在改动沙盒位置后仍然对得上。
const sandboxRootEnv = "BASH_SANDBOX_ROOT"

var (
	textSpec = tools.Spec{
		Name:        ToolText,
		Description: "往当前会话发一段文字。想先把结论或说明发出去、再发文件时用它；纯文字的原答案不需要它。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{"type": "string", "description": "要发送的文字"},
			},
			"required": []any{"text"},
		},
		Effect: tools.EffectSend,
	}
	fileSpec = mediaSpec(ToolFile, "把一个本地文件发到当前会话。path 是**发送根下的相对路径**"+
		"（见系统提示；默认是沙盒根），绝对路径与 .. 会被拒。")
	imageSpec = mediaSpec(ToolImage, "把一张本地图片发到当前会话。path 是**发送根下的相对路径**。"+
		"渠道发不了图片时自动退成文件。")
)

func mediaSpec(name, description string) tools.Spec {
	return tools.Spec{
		Name:        name,
		Description: description,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "发送根下的相对路径"},
				"file_name": map[string]any{
					"type": "string", "description": "收件人看到的文件名，缺省用路径里的文件名",
				},
			},
			"required": []any{"path"},
		},
		Effect: tools.EffectSend,
	}
}

// Options 造源的可选项。
type Options struct {
	// Root 发送根。空 = `ResolveRoot()` 按环境变量与安装根定。
	Root string
	// MaxBytes 单文件上限。<=0 = DefaultMaxBytes。
	MaxBytes int64
}

// ResolveRoot 定出发送根：FKA_SEND_ROOT > BASH_SANDBOX_ROOT > <安装根>/sandbox。
//
// **必须绝对化**：相对路径会按进程 cwd 解析，而 cwd 取决于从哪个目录拉起——
// 同一份配置在不同目录下会指向不同的根，且不报错（与 `fka-bash` 的 resolveRoot
// 是同一个教训）。
func ResolveRoot() string {
	root := strings.TrimSpace(os.Getenv(EnvRoot))
	if root == "" {
		root = strings.TrimSpace(os.Getenv(sandboxRootEnv))
	}
	if root == "" {
		root = filepath.Join(config.Home(), "sandbox")
	}
	return root
}

type source struct {
	root     string
	maxBytes int64
}

// NewSource 造回复源。
func NewSource(opts Options) tools.Source {
	root := opts.Root
	if root == "" {
		root = ResolveRoot()
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		// 拿不到绝对路径几乎只可能是「根为空」——退回配置里的安装根，别让源起不来
		abs = root
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &source{root: abs, maxBytes: maxBytes}
}

func (s *source) ID() string    { return SourceID }
func (s *source) Label() string { return "回话" }

func (s *source) List(ctx context.Context, tc tools.Context) ([]tools.Spec, error) {
	return []tools.Spec{textSpec, fileSpec, imageSpec}, nil
}

// PromptSection 把「发送根在哪」告诉模型。**必须给**：不给的话模型只知道要相对
// 路径，却不知道相对谁，只能瞎猜。这一节只在 `send` 放行时才有意义，但接缝的
// PromptSection 不带放行信息——多这一段几行的开销，换来的是模型不用试错。
func (s *source) PromptSection(tc tools.Context) (string, error) {
	return "发文件 / 图片时，path 是相对这个目录的路径：" + s.root + "（绝对路径与 .. 会被拒）。", nil
}

func (s *source) Call(ctx context.Context, name string, args map[string]any, tc tools.Context) (tools.Result, error) {
	switch name {
	case ToolText:
		return s.sendText(ctx, args, tc)
	case ToolFile:
		return s.sendMedia(ctx, args, tc, false)
	case ToolImage:
		return s.sendMedia(ctx, args, tc, true)
	}
	// 注册表只按声明分发，走到这里说明声明与实现不同步——那是代码缺陷
	return tools.FailResult("回复源没有实现 %s", name), nil
}

func (s *source) Close() error { return nil }

func (s *source) sendText(ctx context.Context, args map[string]any, tc tools.Context) (tools.Result, error) {
	reply, problem := requireReply(tc)
	if problem != "" {
		return tools.FailResult("%s", problem), nil
	}
	text, _ := args["text"].(string)
	if strings.TrimSpace(text) == "" {
		return tools.FailResult("text 不能为空"), nil
	}
	if err := reply.Text(ctx, text); err != nil {
		return tools.FailResult("发送失败：%s", err.Error()), nil
	}
	return tools.OKResult("已发出。"), nil
}

func (s *source) sendMedia(ctx context.Context, args map[string]any, tc tools.Context, image bool) (tools.Result, error) {
	reply, problem := requireReply(tc)
	if problem != "" {
		return tools.FailResult("%s", problem), nil
	}

	raw, _ := args["path"].(string)
	full, err := s.resolve(raw)
	if err != nil {
		return tools.FailResult("路径不可发：%s", err.Error()), nil
	}
	fileName, _ := args["file_name"].(string)

	if image {
		if err := reply.Image(ctx, full, fileName); err != nil {
			return tools.FailResult("发送失败：%s", err.Error()), nil
		}
	} else {
		if err := reply.File(ctx, full, fileName); err != nil {
			return tools.FailResult("发送失败：%s", err.Error()), nil
		}
	}
	return tools.OKResult("已发出。"), nil
}

// requireReply 取当前会话的回话能力。**取不到不是异常**——`fka ask` 那条路没有
// 渠道，模型不该看到这个工具；它看到了（部署方放行了 send）却没有会话时，要
// 得到一句说清原因的话，而不是一次 panic 或「工具不可用」。
func requireReply(tc tools.Context) (tools.Reply, string) {
	if tc.Reply == nil {
		return nil, "当前没有可回复的会话（这条消息不是从渠道来的）。请把内容写进最终答案。"
	}
	return tc.Reply, ""
}

// resolve 三道闸都过才放行：相对路径、符号链接后仍在根内、普通文件且不超上限。
//
// 单看每一道都不够：只查字符串前缀挡不住 `link -> /etc`；只查符号链接挡不住
// `../../etc`（因为它会先被 join 解析掉）。所以这里按「先拼接、再求值、再核对
// 真实前缀」的顺序做，和中途任何一步的顺序无关地守住同一条边界。
func (s *source) resolve(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("路径为空")
	}
	if filepath.IsAbs(raw) {
		return "", errors.New("只允许相对路径")
	}

	joined := filepath.Join(s.root, raw)

	// 根本身也要在（否则 EvalSymlinks 会失败，错误信息会含糊）
	realRoot, err := filepath.EvalSymlinks(s.root)
	if err != nil {
		return "", fmt.Errorf("发送根不存在：%s", s.root)
	}
	realPath, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", fmt.Errorf("文件不存在或不可达：%s", raw)
	}
	if !isUnder(realRoot, realPath) {
		return "", fmt.Errorf("越出了发送根：%s", raw)
	}

	info, err := os.Stat(realPath)
	if err != nil {
		return "", fmt.Errorf("读不到文件信息：%s", raw)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("只能发普通文件：%s", raw)
	}
	if info.Size() > s.maxBytes {
		return "", fmt.Errorf("文件 %d 字节，超过上限 %d", info.Size(), s.maxBytes)
	}
	return realPath, nil
}

// isUnder 判断 path 是否在 root 之内（含 root 本身）。两侧都应是求值过符号链接的
// 绝对路径。
func isUnder(root, path string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(os.PathSeparator))
}
