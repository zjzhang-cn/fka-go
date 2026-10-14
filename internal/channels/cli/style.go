// 本文件是终端的**排版零件**：调色板、上色开关、终端判定、压行。
//
// ## 颜色为什么是手写的 ANSI
//
// 上色本身不值得引一门框架。默认**只在真终端上上色**：管道、重定向、`NO_COLOR`、
// `TERM=dumb` 都自动关掉——给一个文件写 ANSI 转义是把文件弄脏，不是让输出好看。
//
// 这些零件原样从 `cmd/fka` 搬来，因为 CLI 现在是**第三个渠道**：它自己的排版
// 不该再散在命令行的入口代码里。
package cli

import (
	"fmt"
	"os"
	"strings"
)

// Palette 只有「开/关」一个状态。**关掉时每个方法都原样返回**，所以调用点不必到处
// 判「现在有没有颜色」——那种判散开就一定会漏一处，漏的那处会在重定向的输出里留下
// 一串 `\x1b[0m`。
type Palette struct{ enabled bool }

// NewPalette 造一个调色板。enabled=false 时所有方法原样返回。
func NewPalette(enabled bool) Palette { return Palette{enabled: enabled} }

const (
	ansiReset  = "\x1b[0m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[96m"
	ansiDim    = "\x1b[2m"
)

func (p Palette) paint(code, s string) string {
	if !p.enabled {
		return s
	}
	return code + s + ansiReset
}

func (p Palette) User(s string) string      { return p.paint(ansiCyan, s) }
func (p Palette) Assistant(s string) string { return p.paint(ansiGreen, s) }
func (p Palette) Tool(s string) string      { return p.paint(ansiYellow, s) }
func (p Palette) Err(s string) string       { return p.paint(ansiRed, s) }
func (p Palette) Dim(s string) string       { return p.paint(ansiDim, s) }

// ShouldColor 这一路输出上不上色。**从上往下第一条命中的说了算**：
//
//	--no-color 参数   → 关（用户当场说不要）
//	FKA_COLOR=always  → 开（演示/截图；也是测试唯一能强制开的口子）
//	FKA_COLOR=never   → 关
//	NO_COLOR 已设置   → 关（这是那个约定的语义：**只要存在**就关，值无关）
//	TERM=dumb         → 关
//	不是字符设备      → 关（管道/重定向/文件）
//
// 顺序有意：参数优先于环境变量，环境变量优先于自动探测。反过来会让
// `NO_COLOR=1 fka chat --no-color` 之类的组合出现「谁说了算」的不确定。
func ShouldColor(w *os.File, noColorFlag bool) bool {
	if noColorFlag {
		return false
	}
	if value := strings.ToLower(strings.TrimSpace(os.Getenv("FKA_COLOR"))); value != "" {
		switch value {
		case "always", "1", "true", "on", "yes":
			return true
		case "never", "0", "false", "off", "no":
			return false
		}
	}
	if _, present := os.LookupEnv("NO_COLOR"); present {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb") {
		return false
	}
	return IsCharDevice(w)
}

// IsCharDevice 这个文件是不是终端。**不引 isatty 库**：它已经作为 MCP SDK 的间接
// 依赖躺在 go.sum 里，用它就会平白多一个直接依赖，而这一行 `Stat` 就是全部所需
// （真正需要终端能力的行编辑已经交给 readline）。
//
// 字符设备只是**近似**（`/dev/null` 也是字符设备），但对「要不要上色」这个用途，
// 近似的两个方向代价都很小：真终端被误判成不是 → 少点颜色；反之 → 多几个转义，
// 而 `NO_COLOR` / `--no-color` 都能救。精确判定需要 ioctl，不值得。
func IsCharDevice(w *os.File) bool {
	if w == nil {
		return false
	}
	info, err := w.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// firstLine 把一段可能带换行的文字压成一行并截到 60 字。工具参数与结果动辄上千字，
// 全量铺开会把对话淹掉；完整内容仍在会话历史里。
func firstLine(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	runes := []rune(text)
	if len(runes) > 60 {
		return string(runes[:60]) + "…"
	}
	return text
}

// usageLine 把一轮的真实用量渲染成一句话。**纯函数**，ask 与 chat 共用：
//
//	全零            → ""（服务端没给，不打）
//	有缓存命中      → 「入 X（缓存命中 C）/ 出 Y（共 Z）」
//	没给总数        → 省略「（共 Z）」
//
// 缓存命中是**输入的一部分**，所以贴在「入」后面而不是单列。前缀（`[用量] ` /
// `· 用量：`）与颜色由 `flushUsage` 按模式加。
func usageLine(in, out, total, cached int) string {
	if in == 0 && out == 0 && total == 0 && cached == 0 {
		return ""
	}
	line := fmt.Sprintf("入 %d", in)
	if cached > 0 {
		line += fmt.Sprintf("（缓存命中 %d）", cached)
	}
	line += fmt.Sprintf(" / 出 %d", out)
	if total > 0 {
		line += fmt.Sprintf("（共 %d）", total)
	}
	return line
}

// mediaStoredLine 把已落沙盒的**结构化路径**拼成一行系统提示。**纯函数**：服务端
// 只给路径（`channels.MediaStoredNotice.Paths`），措辞由前端自己定——这条与
// `usageLine` 同一条规矩。没有可解析的路径时退回一句不带列表的。
func mediaStoredLine(paths []string) string {
	if len(paths) == 0 {
		return "文件已存入沙盒。请说要我拿它做什么。"
	}
	return "已存入沙盒：" + strings.Join(paths, "、") + "。请说要我拿它做什么。"
}
