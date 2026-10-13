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
			if msg.Role == llm.RoleUser && len(msg.ImageAttachments) > 0 {
				m.sawUser = true
				m.content = msg.Content
				m.images = append(m.images, msg.ImageAttachments...)
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
	if model.content != "请看看我发的图片。" {
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
	if model.content != "这是什么" {
		t.Errorf("有文字时不该被默认提示语覆盖：%q", model.content)
	}
	if len(model.images) != 1 {
		t.Errorf("该带 1 张图，实际 %d", len(model.images))
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
