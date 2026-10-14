// 本文件是**入站媒体唯一的转换点**：把渠道产出的媒体 part 取回字节，按类别交给模型
// 与工具——**与沙盒 `read`、CLI `@引用` 是同一套三类语义**：
//
//   - 图片  → `llm.ImageAttachment`（base64 data URI，模型当图看）
//   - 文本  → 正文并进本轮问题（有上限、超出截断）
//   - 二进制（音频 / 视频 / 压缩包…）→ 只给「名字 + 类型 + 大小」的元信息
//
// ## 为什么在这里，而不是每个渠道各写一遍
//
// 微信与网页的取回方式完全不同（前者走 CDN 解密，后者读内存暂存），但它们对模型的
// 表现必须一模一样。所以差异收在渠道自己的 `MediaFetcher` 里，这个文件只认
// `channels.MediaFetcher` 这个接口——**agent 与 llm 里不出现任何渠道分支**。
//
// ## 落盘与否
//
// 文本并进问题会**进会话历史**（与 CLI 一样，下一轮还能引用到那句正文）；图片是 data
// URI、`json:"-"`，只活在当前这一轮（历史里不留 base64）。二进制只留元信息。被点名时
// 原始字节另走 `tools.Context.Inbound` 交给工具（如沙盒 import）。
package messages

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/llm"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

const (
	// maxInboundMedia 一条消息最多处理几个媒体 part。有上限是必须的：没有上限就是让
	// 发送方决定这一轮请求有多大。
	maxInboundMedia = 8
	// maxInboundMediaBytes 单个媒体件的原始字节上限。**取回后判**——渠道的
	// `MediaRef.Size` 可能为 0（协议不总是报），不能只信它。
	maxInboundMediaBytes = 8 << 20
	// maxInboundTextBytes 一段文本并入提示词的上限，与 CLI `@引用` / 沙盒 `read` 一致。
	maxInboundTextBytes = 32 << 10
)

// MediaMode 入站媒体去哪。
//
//   - MediaModeLLM（默认）：进模型（图片 vision / 文本并进问题 / 二进制元信息），
//     沙盒**按需**——模型当轮调 FKA_MEDIA_SINK 那个工具时才落。
//   - MediaModeBoth：既进模型，又在开轮时**自动**落一次沙盒（路径进历史，后续轮可引用）。
//   - MediaModeSandbox：**不进模型**，自动落沙盒，把路径交给模型（适合非视觉模型 /
//     只想让工具处理）。
type MediaMode string

const (
	MediaModeLLM     MediaMode = "llm"
	MediaModeBoth    MediaMode = "both"
	MediaModeSandbox MediaMode = "sandbox"
)

// MediaModeEnv 决定入站媒体去向的环境变量名。**仓库头部也记了它的语义。**
const MediaModeEnv = "FKA_MEDIA_MODE"

// FeedsModel 这种模式下媒体是否还喂给模型。
func (m MediaMode) FeedsModel() bool { return m != MediaModeSandbox }

// AutoImports 这种模式下是否在开轮时自动把入站字节落沙盒。
func (m MediaMode) AutoImports() bool { return m == MediaModeBoth || m == MediaModeSandbox }

// NormalizeMediaMode 认一个取值。**空或认不出退回 llm（现状）**，并对认不出的记一条
// Warn——配置笔误不该改变行为，但必须留下痕迹。
func NormalizeMediaMode(raw string) MediaMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(MediaModeLLM):
		return MediaModeLLM
	case string(MediaModeBoth):
		return MediaModeBoth
	case string(MediaModeSandbox):
		return MediaModeSandbox
	default:
		config.Log().Warn(config.TypeMSG, MediaModeEnv+" 认不出的取值，按 llm 处理",
			config.Context{"value": raw})
		return MediaModeLLM
	}
}

// inboundKind 一个媒体件的类别。对齐沙盒 `read` 的 fileKind。
type inboundKind int

const (
	inboundText inboundKind = iota
	inboundImage
	inboundBinary
)

// inboundSection 一块要拼进问题的说明：文本给正文，图片/二进制给元信息。
type inboundSection struct {
	name      string
	kind      inboundKind
	text      string // kind == inboundText 时的正文
	mime      string
	size      int64
	truncated bool
}

// inboundMedia 本轮入站媒体的转换结果。
type inboundMedia struct {
	// images base64 data URI，给模型看（vision）
	images []llm.ImageAttachment
	// files 所有取回的原始字节，交给工具层（落点导入，见 agent 的 injectInbound）
	files []tools.Attachment
	// sections 拼进问题的说明，按入站顺序
	sections []inboundSection
	// failures 取不回来的那些的说明（日志与「一件都没取到」时用），不是错误
	failures []string
}

// mediaPartsOf 挑出消息里所有**非文本**的 part（文字由 `InboundMessage.Text()` 给）。
func mediaPartsOf(message channels.InboundMessage) []channels.Part {
	var out []channels.Part
	for _, part := range message.Parts {
		if part.Kind != channels.KindText {
			out = append(out, part)
		}
	}
	return out
}

// collectInboundMedia 取回并分类所有媒体 part。渠道没实现 `MediaFetcher` 时返回一条
// 说明、零结果，由调用方决定怎么说。
func collectInboundMedia(ctx context.Context, channel channels.Channel, parts []channels.Part) inboundMedia {
	if len(parts) == 0 {
		return inboundMedia{}
	}

	fetcher, ok := channel.(channels.MediaFetcher)
	if !ok {
		return inboundMedia{failures: []string{fmt.Sprintf("渠道 %s 不能取回文件", channel.Label())}}
	}

	var out inboundMedia
	for index, part := range parts {
		if len(out.sections) >= maxInboundMedia {
			out.failures = append(out.failures, fmt.Sprintf("只处理前 %d 个文件", maxInboundMedia))
			break
		}

		name := partName(part, index)

		// 语音的转写文本往往比音频本身更有用——有它就当一段文本，但仍取回音频字节给工具。
		if transcript := strings.TrimSpace(part.Transcript); transcript != "" {
			out.sections = append(out.sections, textSection(name+"（语音转写）", transcript))
		}

		data, err := fetcher.FetchMedia(ctx, part.Media)
		if err != nil {
			config.Log().Warn(config.TypeMSG, "入站文件取回失败", config.Context{
				"channel": channel.ID(), "mediaId": part.Media.ID, "error": err.Error(),
			})
			out.failures = append(out.failures, fmt.Sprintf("「%s」没能取回来", name))
			continue
		}
		if len(data) == 0 {
			out.failures = append(out.failures, fmt.Sprintf("「%s」是空的", name))
			continue
		}
		if len(data) > maxInboundMediaBytes {
			out.failures = append(out.failures, fmt.Sprintf("「%s」超过 %d MiB", name, maxInboundMediaBytes>>20))
			continue
		}

		mime, kind := classifyMedia(part.Media.MimeType, data)
		out.files = append(out.files, tools.Attachment{Name: name, MimeType: mime, Data: data})

		// 语音已有转写：文本已加过，字节也已收进 files，不必再给一份元信息
		if strings.TrimSpace(part.Transcript) != "" {
			continue
		}

		switch kind {
		case inboundImage:
			out.images = append(out.images, llm.ImageAttachment{
				Name:    name,
				DataURI: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data),
			})
			out.sections = append(out.sections, inboundSection{name: name, kind: inboundImage, mime: mime, size: int64(len(data))})
		case inboundText:
			out.sections = append(out.sections, textSection(name, string(data)))
		default:
			out.sections = append(out.sections, inboundSection{name: name, kind: inboundBinary, mime: mime, size: int64(len(data))})
		}
	}
	return out
}

// textSection 造一个文本 section，超出上限就截断并标记。
func textSection(name, content string) inboundSection {
	section := inboundSection{name: name, kind: inboundText}
	if len(content) > maxInboundTextBytes {
		content = content[:maxInboundTextBytes]
		section.truncated = true
	}
	section.text = content
	return section
}

// questionSuffix 把各 section 拼成追加到本轮问题末尾的一段说明。**没有 section 就返回空串**。
func (m inboundMedia) questionSuffix() string {
	if len(m.sections) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n（用户发来了这些内容）")
	for _, section := range m.sections {
		switch section.kind {
		case inboundText:
			fmt.Fprintf(&b, "\n\n──── %s ────\n%s", section.name, section.text)
			if section.truncated {
				fmt.Fprintf(&b, "\n…（超过 %d 字节，已截断）", maxInboundTextBytes)
			}
		case inboundImage:
			fmt.Fprintf(&b, "\n\n──── %s（%s，%s）────\n（图片已作为本条消息的附件发送，请直接查看）",
				section.name, section.mime, humanBytes(section.size))
		default:
			fmt.Fprintf(&b, "\n\n──── %s（%s，%s）────\n（二进制文件，内容未附；如需处理可先存进沙盒或用工具）",
				section.name, section.mime, humanBytes(section.size))
		}
	}
	return b.String()
}

// partName 一个 part 的显示名：文件名优先，否则按类别与序号给个默认。
func partName(part channels.Part, index int) string {
	if name := strings.TrimSpace(part.Media.FileName); name != "" {
		return name
	}
	label := "文件"
	switch part.Kind {
	case channels.KindImage:
		label = "图片"
	case channels.KindVoice:
		label = "语音"
	case channels.KindVideo:
		label = "视频"
	}
	return fmt.Sprintf("%s%d", label, index+1)
}

// classifyMedia 定一个媒体件的 MIME 与类别。**先看 MIME，再看内容**：
//
//   - `image/*` → 图片；`audio/*`、`video/*` → 二进制；
//   - **明确的文本类型**（`text/*`、`application/json` 等）→ 文本；
//   - **明确的二进制类型**（`application/pdf`、`application/zip`、Office…）→ 二进制；
//   - 只剩「嗅探说不出具体类型」（`application/octet-stream`）时，才回退按 NUL 判文本。
//
// 只按 NUL 判是**错的**：很多 PDF / 压缩包的开头是 ASCII、前 512 字节没有 NUL，会被
// 误当文本，把原始字节塞进提示词（模型读不懂，还白烧预算）。
func classifyMedia(declared string, data []byte) (string, inboundKind) {
	head := data
	if len(head) > 512 {
		head = head[:512]
	}

	mime := strings.ToLower(strings.TrimSpace(declared))
	if mime == "" || genericMime(mime) {
		mime = strings.ToLower(http.DetectContentType(head))
	}

	switch {
	case strings.HasPrefix(mime, "image/"):
		return mime, inboundImage
	case strings.HasPrefix(mime, "audio/"), strings.HasPrefix(mime, "video/"):
		return mime, inboundBinary
	case isTextMime(mime):
		return mime, inboundText
	case genericMime(mime) && bytes.IndexByte(head, 0) < 0:
		// 嗅探只说「不知道」（octet-stream），且开头没有 NUL——当文本
		return mime, inboundText
	default:
		// 其余具体类型（pdf/zip/office/octet-stream 带 NUL…）都是二进制
		return mime, inboundBinary
	}
}

// isTextMime MIME 是不是明确的文本类型。`text/*` 之外，还认几个常见的文本型
// application 类型——它们该当文本读，不该只给元信息。
func isTextMime(m string) bool {
	if strings.HasPrefix(m, "text/") {
		return true
	}
	switch m {
	case "application/json", "application/ld+json", "application/xml", "application/xhtml+xml",
		"application/javascript", "application/x-javascript", "application/ecmascript",
		"application/x-sh", "application/x-shellscript", "application/x-yaml", "application/yaml",
		"application/toml", "application/sql", "application/csv", "application/rtf",
		"application/x-httpd-php", "application/x-python", "application/x-ruby":
		return true
	}
	return false
}

// genericMime 嗅探/声明「没说出具体类型」：任意字节是 octet-stream，文本是 text/*。
// 与沙盒 `read.go` 的 genericMIME 同规则。
func genericMime(m string) bool {
	return m == "application/octet-stream" || strings.HasPrefix(m, "text/")
}

// humanBytes 把字节数写成人看的大小。与沙盒 `read.go` 的同名函数一致。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
