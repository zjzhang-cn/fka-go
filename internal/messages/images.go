// 本文件是**入站图片唯一的转换点**：把渠道产出的图片 part 取回字节，统一变成
// `llm.ImageAttachment`，随本轮 user 消息发给模型。
//
// ## 为什么在这里，而不是每个渠道各写一遍
//
// 微信与网页的取图方式完全不同（前者走 CDN 解密，后者读内存暂存），但它们对模型
// 的表现必须一模一样：一个 data URI 附件。所以差异收在渠道自己的 `MediaFetcher`
// 里，这个文件只认 `channels.MediaFetcher` 这个接口——**agent 与 llm 里不出现任何
// 渠道分支**。加第三个渠道时，这个文件一行不改。
//
// ## 为什么图片不带进会话历史
//
// 字节是 base64 的 data URI，写进 `data/history/*.jsonl` 会把历史撑爆、还要每轮
// 重放。`llm.ChatMessage.ImageAttachments` 是 `json:"-"`，只活在当前这一轮请求里。
// 代价是下一轮追问「那张图」时图已不在上下文——这与 CLI 的 `@图片` 是同一条规矩。
package messages

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/llm"
)

const (
	// maxInboundImages 一条消息最多带几张图。有上限是必须的：图片是 base64 附件，
	// 没有上限就是让发送方决定这一轮请求有多大。
	maxInboundImages = 4
	// maxInboundImageBytes 单张入站图片的字节上限。**在取回后判**——渠道的
	// `MediaRef.Size` 可能为 0（协议不总是报），不能只信它。超过的跳过并如实记一句。
	maxInboundImageBytes = 8 << 20
)

// defaultImageQuestion 只发图、不带文字时补的问题。**必须补一句**：各家模型对
// 「空文本 + 图片块」的接受度不一致，给一句明确指令最稳，用户也不必再补一句。
const defaultImageQuestion = "请看看我发的图片。"

// imagePartsOf 挑出消息里的图片 part。文件/语音/视频不在本文件的范围（业务层
// 仍如实拒答，见 `replyNotText`）。
func imagePartsOf(message channels.InboundMessage) []channels.Part {
	var out []channels.Part
	for _, part := range message.Parts {
		if part.Kind == channels.KindImage {
			out = append(out, part)
		}
	}
	return out
}

// fetchInboundImages 把图片 part 取回成附件。第二个返回值是本轮取不回来的那些的
// 说明（给日志与「一张都没取到」时的答复用），**不是错误**——取图失败不该把有文字
// 的那一轮也带走。
//
// 渠道没实现 `MediaFetcher`（无状态、或压根不产图片）时返回一条说明、零附件，由
// 调用方决定怎么说。
func fetchInboundImages(ctx context.Context, channel channels.Channel, parts []channels.Part) ([]llm.ImageAttachment, []string) {
	if len(parts) == 0 {
		return nil, nil
	}

	fetcher, ok := channel.(channels.MediaFetcher)
	if !ok {
		return nil, []string{fmt.Sprintf("渠道 %s 不能取回图片", channel.Label())}
	}

	images := make([]llm.ImageAttachment, 0, len(parts))
	var failures []string
	for index, part := range parts {
		if len(images) >= maxInboundImages {
			failures = append(failures, fmt.Sprintf("只处理前 %d 张图片", maxInboundImages))
			break
		}

		data, err := fetcher.FetchMedia(ctx, part.Media)
		if err != nil {
			config.Log().Warn(config.TypeMSG, "入站图片取回失败", config.Context{
				"channel": channel.ID(), "mediaId": part.Media.ID, "error": err.Error(),
			})
			failures = append(failures, "有一张图片没能取回来")
			continue
		}
		if len(data) == 0 {
			failures = append(failures, "有一张图片是空的")
			continue
		}
		if len(data) > maxInboundImageBytes {
			failures = append(failures, fmt.Sprintf("有一张图片超过 %d MiB", maxInboundImageBytes>>20))
			continue
		}

		name := strings.TrimSpace(part.Media.FileName)
		if name == "" {
			name = fmt.Sprintf("图片%d", index+1)
		}
		images = append(images, llm.ImageAttachment{
			Name:    name,
			DataURI: "data:" + imageMime(part.Media.MimeType, data) + ";base64," + base64.StdEncoding.EncodeToString(data),
		})
	}
	return images, failures
}

// imageMime 定这张图在 data URI 里的 MIME。优先用渠道声明的（它可能比嗅探更准，
// 比如 SVG），不是 `image/*` 就按内容嗅探——微信的图片 part 不带 MIME，只能靠嗅探。
func imageMime(declared string, data []byte) string {
	declared = strings.ToLower(strings.TrimSpace(declared))
	if strings.HasPrefix(declared, "image/") {
		return declared
	}
	if sniffed := http.DetectContentType(data); strings.HasPrefix(sniffed, "image/") {
		return sniffed
	}
	// 嗅探也认不出（少见：真实图片都认得）就退回一个具体的图片类型，别让 data URI
	// 少了 MIME——那会被 provider 直接拒掉。
	return "image/jpeg"
}
