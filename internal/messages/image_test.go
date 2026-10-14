// 入站图片的用例：微信与网页产出的图片 part，最终都由**同一个**消息层转换点变成
// `llm.ImageAttachment`。这里用假渠道 + 假模型验那条转换，不碰任何具体渠道。
package messages_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/agent"
	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/llm"
	"github.com/zjzhang-cn/fka-go/internal/messages"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// fetchingChannel 在假渠道之上补一个可选能力 `channels.MediaFetcher`。
// **单独一个类型**：必填三组之外的能力是可选的，基本假渠道不该被迫实现它——
// 那正是接缝把可选能力拆成独立接口的理由。
type fetchingChannel struct {
	*fakeChannel
	blobs map[string][]byte
	err   error
}

func (c *fetchingChannel) FetchMedia(ctx context.Context, ref channels.MediaRef) ([]byte, error) {
	if c.err != nil {
		return nil, c.err
	}
	data, ok := c.blobs[ref.ID]
	if !ok {
		return nil, errors.New("没有这个句柄")
	}
	return data, nil
}

// imageModel 记住模型看到的图片附件与 user 正文，直接作答（不调工具）。
type imageModel struct {
	mu      sync.Mutex
	images  []llm.ImageAttachment
	content string
	sawUser bool
	calls   int
}

func (m *imageModel) client() llm.ChatClient {
	return func(ctx context.Context, msgs []llm.ChatMessage, defs []llm.ToolDef) (llm.ChatResult, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.calls++
		for _, msg := range msgs {
			if msg.Role == llm.RoleUser {
				m.sawUser = true
				m.content = msg.Content
				if len(msg.ImageAttachments) > 0 {
					m.images = append(m.images, msg.ImageAttachments...)
				}
			}
		}
		return llm.ChatResult{Content: "看图了"}, nil
	}
}

func newImageHandler(m *imageModel) *messages.Handler {
	registry := tools.NewRegistry(nil, tools.ReadToolPolicy())
	runner := agent.NewRunner(m.client(), registry, agent.RunnerOptions{MaxSteps: 2, SystemPrompt: "测试用"})
	return messages.NewHandler(runner)
}

// pngHead 一段能被 http.DetectContentType 认成 image/png 的字节。
var pngHead = []byte("\x89PNG\r\n\x1a\n")

func imageMessage() channels.InboundMessage {
	return channels.InboundMessage{
		ChannelID: "fake", AccountID: "acct-1", MessageID: "m1",
		SenderID: "s1", PrincipalID: "u1", ConversationID: "c1",
		Parts: []channels.Part{{
			Kind:  channels.KindImage,
			Media: channels.MediaRef{ID: "img-1", FileName: "a.png"},
		}},
	}
}

// Test入站图片作为附件发给模型 渠道产出的图片 part → MediaFetcher 取回 → data URI
// 附件，且纯图自动补一句默认提示语。
func Test入站图片作为附件发给模型(t *testing.T) {
	channel := &fetchingChannel{
		fakeChannel: &fakeChannel{accountID: "acct-1"},
		blobs:       map[string][]byte{"img-1": pngHead},
	}
	model := &imageModel{}
	out := newImageHandler(model).Ask(context.Background(), messages.AskInput{Channel: channel, Message: imageMessage()})

	if out.Explain != "" {
		t.Fatalf("该答出来，却回了说明：%q", out.Explain)
	}
	if !model.sawUser || len(model.images) != 1 {
		t.Fatalf("模型该看到 1 张图，实际 %d", len(model.images))
	}
	if !strings.HasPrefix(model.images[0].DataURI, "data:image/png;base64,") {
		t.Errorf("附件该是 png data URI：%q", model.images[0].DataURI)
	}
	if !strings.HasPrefix(model.content, "请看看我发来的内容。") {
		t.Errorf("纯图该补默认提示语，实际正文 = %q", model.content)
	}
}

// Test入站图片带文字时保留文字 图文一起时不补默认提示语，用用户自己的文字。
func Test入站图片带文字时保留文字(t *testing.T) {
	channel := &fetchingChannel{
		fakeChannel: &fakeChannel{accountID: "acct-1"},
		blobs:       map[string][]byte{"img-1": pngHead},
	}
	message := imageMessage()
	message.Parts = append([]channels.Part{channels.TextPart("这是什么")}, message.Parts...)
	model := &imageModel{}
	out := newImageHandler(model).Ask(context.Background(), messages.AskInput{Channel: channel, Message: message})

	if out.Explain != "" {
		t.Fatalf("该答出来：%q", out.Explain)
	}
	if !strings.HasPrefix(model.content, "这是什么") {
		t.Errorf("有文字时该保留用户文字：%q", model.content)
	}
	if len(model.images) != 1 {
		t.Errorf("该带 1 张图，实际 %d", len(model.images))
	}
}

// Test文本文件内容并入问题 文本文件（无 NUL）该把正文拼进本轮问题，模型直接读得到。
func Test文本文件内容并入问题(t *testing.T) {
	channel := &fetchingChannel{
		fakeChannel: &fakeChannel{accountID: "acct-1"},
		blobs:       map[string][]byte{"f1": []byte("你好，世界\n第二行")},
	}
	model := &imageModel{}
	message := channels.InboundMessage{
		ChannelID: "fake", AccountID: "acct-1", MessageID: "m1",
		SenderID: "s1", PrincipalID: "u1", ConversationID: "c1",
		Parts: []channels.Part{{
			Kind:  channels.KindFile,
			Media: channels.MediaRef{ID: "f1", FileName: "note.txt"},
		}},
	}
	out := newImageHandler(model).Ask(context.Background(), messages.AskInput{Channel: channel, Message: message})

	if out.Explain != "" {
		t.Fatalf("该答出来：%q", out.Explain)
	}
	if len(model.images) != 0 {
		t.Errorf("文本文件不该产生图片附件，实际 %d", len(model.images))
	}
	if !strings.Contains(model.content, "你好，世界") {
		t.Errorf("文本正文该并进问题：%q", model.content)
	}
}

// Test二进制文件只给元信息 含 NUL 的字节当二进制：内容不进问题，只给名字/类型/大小的元信息。
func Test二进制文件只给元信息(t *testing.T) {
	channel := &fetchingChannel{
		fakeChannel: &fakeChannel{accountID: "acct-1"},
		blobs:       map[string][]byte{"f1": {'P', 'K', 0x03, 0x04, 0x00, 'x'}},
	}
	model := &imageModel{}
	message := channels.InboundMessage{
		ChannelID: "fake", AccountID: "acct-1", MessageID: "m1",
		SenderID: "s1", PrincipalID: "u1", ConversationID: "c1",
		Parts: []channels.Part{{
			Kind:  channels.KindFile,
			Media: channels.MediaRef{ID: "f1", FileName: "data.zip", MimeType: "application/zip"},
		}},
	}
	out := newImageHandler(model).Ask(context.Background(), messages.AskInput{Channel: channel, Message: message})

	if out.Explain != "" {
		t.Fatalf("该答出来：%q", out.Explain)
	}
	if len(model.images) != 0 {
		t.Errorf("二进制不该产生图片附件")
	}
	if !strings.Contains(model.content, "data.zip") || !strings.Contains(model.content, "二进制文件") {
		t.Errorf("该给二进制文件的元信息：%q", model.content)
	}
}

// TestPDF不当文本塞进提示词 PDF 开头是 ASCII、常常没有 NUL，但它是二进制类型：判据
// 要看 MIME，只给元信息，绝不把原始字节当正文喂给模型（模型读不懂，还白烧预算）。
func TestPDF不当文本塞进提示词(t *testing.T) {
	pdf := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\ntrailer\n<<>>\n%%EOF\n")
	channel := &fetchingChannel{
		fakeChannel: &fakeChannel{accountID: "acct-1"},
		blobs:       map[string][]byte{"f1": pdf},
	}
	model := &imageModel{}
	message := channels.InboundMessage{
		ChannelID: "fake", AccountID: "acct-1", MessageID: "m1",
		SenderID: "s1", PrincipalID: "u1", ConversationID: "c1",
		Parts: []channels.Part{{
			Kind:  channels.KindFile,
			Media: channels.MediaRef{ID: "f1", FileName: "doc.pdf"},
		}},
	}
	out := newImageHandler(model).Ask(context.Background(), messages.AskInput{Channel: channel, Message: message})

	if out.Explain != "" {
		t.Fatalf("该答出来：%q", out.Explain)
	}
	if strings.Contains(model.content, "%PDF-1.4") {
		t.Errorf("PDF 原文不该进提示词：%q", model.content)
	}
	if !strings.Contains(model.content, "doc.pdf") || !strings.Contains(model.content, "二进制文件") {
		t.Errorf("该给 PDF 的元信息：%q", model.content)
	}
}

// Test媒体去向_sandbox纯文件只发通知不跑模型 FKA_MEDIA_MODE=sandbox 纯文件：落沙盒、
// 发一条**通知**（MediaStoredNotice，一行、带路径），**不跑 LLM、不发聊天气泡**。
func Test媒体去向_sandbox纯文件只发通知不跑模型(t *testing.T) {
	channel := &fetchingChannel{
		fakeChannel: &fakeChannel{accountID: "acct-1"},
		blobs:       map[string][]byte{"img-1": pngHead},
	}
	model := &imageModel{}
	handler := newImageHandler(model)
	handler.SetMediaMode(messages.MediaModeSandbox)
	imported := false
	handler.SetInboundImporter(func(ctx context.Context, principal, sessionKey string, files []tools.Attachment) (string, error) {
		imported = true
		return "已把这些文件存入沙盒：\n- inbox/a.png\n用 read 查看。", nil
	})

	out := handler.Ask(context.Background(), messages.AskInput{Channel: channel, Message: imageMessage()})

	if !imported {
		t.Error("sandbox 模式该把文件落沙盒")
	}
	if out.Explain != "" {
		t.Errorf("该只发通知、不发文本，实际 Explain=%q", out.Explain)
	}
	if model.calls != 0 {
		t.Errorf("sandbox 纯文件不该触发 LLM，实际调了 %d 次", model.calls)
	}
	var paths []string
	for _, notice := range channel.noticesOf() {
		if stored, ok := notice.(channels.MediaStoredNotice); ok {
			paths = stored.Paths
		}
	}
	if len(paths) != 1 || paths[0] != "inbox/a.png" {
		t.Errorf("通知该带**结构化**的沙盒路径，实际 %v", paths)
	}
}

// Test媒体去向_sandbox收不到通知的渠道退回文本 iLink 这类没有 Notifier：
// 沙盒落盘后没通知可发，退回一条文本，至少让人看见。
func Test媒体去向_sandbox收不到通知的渠道退回文本(t *testing.T) {
	channel := &noNoticeChannel{blobs: map[string][]byte{"img-1": pngHead}}
	model := &imageModel{}
	handler := newImageHandler(model)
	handler.SetMediaMode(messages.MediaModeSandbox)
	handler.SetInboundImporter(func(ctx context.Context, principal, sessionKey string, files []tools.Attachment) (string, error) {
		return "已把这些文件存入沙盒：\n- inbox/a.png", nil
	})

	out := handler.Ask(context.Background(), messages.AskInput{Channel: channel, Message: imageMessage()})

	if !strings.Contains(out.Explain, "inbox/a.png") {
		t.Errorf("收不到通知的渠道该退回文本（含路径），实际 %q", out.Explain)
	}
	if model.calls != 0 {
		t.Errorf("仍不该触发 LLM，实际 %d", model.calls)
	}
}

// noNoticeChannel 一个有媒体能力的渠道，但**不实现 Notifier/EmitterProvider**——用它验
// 「收不到通知就退回文本」。**手写整份**而不是嵌入 fakeChannel：嵌入会把 Notify 一起提上来，
// 那就又变成 Notifier 了。
type noNoticeChannel struct{ blobs map[string][]byte }

func (c *noNoticeChannel) ID() string                                                 { return "plain" }
func (c *noNoticeChannel) Label() string                                              { return "无通知渠道" }
func (c *noNoticeChannel) AccountID() string                                          { return "acct-1" }
func (c *noNoticeChannel) Status() channels.Status                                    { return channels.StatusOnline }
func (c *noNoticeChannel) Start(context.Context, func(channels.InboundMessage)) error { return nil }
func (c *noNoticeChannel) Stop(context.Context) error                                 { return nil }
func (c *noNoticeChannel) Capabilities() channels.Capabilities {
	return channels.Capabilities{
		Text:  channels.KindCapability{Send: true, Receive: true},
		File:  channels.KindCapability{Send: false, Receive: true},
		Image: channels.KindCapability{Send: false, Receive: true},
	}
}
func (c *noNoticeChannel) Senders() channels.Senders {
	return channels.Senders{Text: func(context.Context, channels.SendTextParams) (channels.SendResult, error) {
		return channels.SendResult{}, nil
	}}
}
func (c *noNoticeChannel) FetchMedia(_ context.Context, ref channels.MediaRef) ([]byte, error) {
	data, ok := c.blobs[ref.ID]
	if !ok {
		return nil, errors.New("没有这个句柄")
	}
	return data, nil
}

// Test媒体去向_sandbox下一轮带指令时并进沙盒路径 sandbox 模式挂起的沙盒说明，会在
// 下一条（带指令的）消息里并进问题，模型据此知道文件在哪。
func Test媒体去向_sandbox下一轮带指令时并进沙盒路径(t *testing.T) {
	channel := &fetchingChannel{
		fakeChannel: &fakeChannel{accountID: "acct-1"},
		blobs:       map[string][]byte{"img-1": pngHead},
	}
	model := &imageModel{}
	handler := newImageHandler(model)
	handler.SetMediaMode(messages.MediaModeSandbox)
	handler.SetInboundImporter(func(ctx context.Context, principal, sessionKey string, files []tools.Attachment) (string, error) {
		return "已存入沙盒：inbox/a.png", nil
	})

	// 第一轮：纯文件 → 只存不答
	handler.Ask(context.Background(), messages.AskInput{Channel: channel, Message: imageMessage()})

	// 第二轮：给指令 → 跑模型，问题里带上沙盒路径
	follow := imageMessage()
	follow.MessageID = "m2"
	follow.Parts = []channels.Part{channels.TextPart("把图里的话提取出来")}
	out := handler.Ask(context.Background(), messages.AskInput{Channel: channel, Message: follow})

	if out.Explain != "" {
		t.Fatalf("该答出来：%q", out.Explain)
	}
	if !strings.Contains(model.content, "inbox/a.png") {
		t.Errorf("下一轮该带上沙盒路径：%q", model.content)
	}
}

// Test媒体去向_both既喂模型又标自动落沙盒 FKA_MEDIA_MODE=both 时媒体照常进模型，
// 同时落沙盒、路径并进问题。
func Test媒体去向_both既喂模型又自动落沙盒(t *testing.T) {
	channel := &fetchingChannel{
		fakeChannel: &fakeChannel{accountID: "acct-1"},
		blobs:       map[string][]byte{"img-1": pngHead},
	}
	model := &imageModel{}
	handler := newImageHandler(model)
	handler.SetMediaMode(messages.MediaModeBoth)
	handler.SetInboundImporter(func(ctx context.Context, principal, sessionKey string, files []tools.Attachment) (string, error) {
		return "已存入沙盒：inbox/a.png", nil
	})

	out := handler.Ask(context.Background(), messages.AskInput{Channel: channel, Message: imageMessage()})

	if out.Explain != "" {
		t.Fatalf("该答出来：%q", out.Explain)
	}
	if len(model.images) != 1 {
		t.Errorf("both 模式该照常喂图片，实际 %d", len(model.images))
	}
	if !strings.Contains(model.content, "inbox/a.png") {
		t.Errorf("both 模式该把沙盒路径并进问题：%q", model.content)
	}
}

// TestMediaMode_归一 取值归一：空/认不出退回 llm（现状），不改变行为。
func TestMediaMode_归一(t *testing.T) {
	cases := map[string]messages.MediaMode{
		"":        messages.MediaModeLLM,
		"llm":     messages.MediaModeLLM,
		"BOTH":    messages.MediaModeBoth,
		" both ":  messages.MediaModeBoth,
		"sandbox": messages.MediaModeSandbox,
		"nope":    messages.MediaModeLLM,
	}
	for raw, want := range cases {
		if got := messages.NormalizeMediaMode(raw); got != want {
			t.Errorf("NormalizeMediaMode(%q) = %q，想要 %q", raw, got, want)
		}
	}
}

// Test语音转写当文本 渠道提供了转写文本时，直接当一段文本用。
func Test语音转写当文本(t *testing.T) {
	channel := &fetchingChannel{
		fakeChannel: &fakeChannel{accountID: "acct-1"},
		blobs:       map[string][]byte{"v1": {'\x00', 0x01, 0x02}},
	}
	model := &imageModel{}
	message := channels.InboundMessage{
		ChannelID: "fake", AccountID: "acct-1", MessageID: "m1",
		SenderID: "s1", PrincipalID: "u1", ConversationID: "c1",
		Parts: []channels.Part{{
			Kind: channels.KindVoice, Transcript: "提醒我买牛奶",
			Media: channels.MediaRef{ID: "v1", FileName: "voice.amr"},
		}},
	}
	out := newImageHandler(model).Ask(context.Background(), messages.AskInput{Channel: channel, Message: message})

	if out.Explain != "" {
		t.Fatalf("该答出来：%q", out.Explain)
	}
	if !strings.Contains(model.content, "提醒我买牛奶") {
		t.Errorf("语音转写该并进问题：%q", model.content)
	}
}

// Test图片取不回来时不空猜 渠道取图失败又没文字，回一句实话，不调模型。
func Test图片取不回来时不空猜(t *testing.T) {
	channel := &fetchingChannel{
		fakeChannel: &fakeChannel{accountID: "acct-1"},
		err:         errors.New("CDN 403"),
	}
	model := &imageModel{}
	out := newImageHandler(model).Ask(context.Background(), messages.AskInput{Channel: channel, Message: imageMessage()})

	if out.Explain == "" {
		t.Fatal("图取不回来又没文字，该回一句说明")
	}
	if model.calls != 0 {
		t.Errorf("不该调模型空猜，实际调了 %d 次", model.calls)
	}
}
