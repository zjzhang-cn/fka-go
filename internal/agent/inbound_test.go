package agent

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/llm"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// importSpec 一个模拟沙盒 import 的工具声明。
func importSpec() tools.Spec {
	return tools.Spec{Name: "import", Description: "把用户本轮图片存进沙盒", Effect: tools.EffectRead}
}

// TestRun_入站字节注入被点名的落点工具 只有工具全名等于 InboundSink 时，循环才把本轮
// 附件的原始字节填进它的 `files`；这是「图片能落到沙盒」的关键一跳。
func TestRun_入站字节注入被点名的落点工具(t *testing.T) {
	source := &fakeSource{specs: []tools.Spec{importSpec()}}
	registry := tools.NewRegistry([]tools.Source{source}, tools.DefaultPolicy())

	calls := 0
	chat := func(_ context.Context, _ []llm.ChatMessage, _ []llm.ToolDef) (llm.ChatResult, error) {
		calls++
		if calls == 1 {
			return llm.ChatResult{ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "fake__import", Arguments: `{}`},
			}}, nil
		}
		return llm.ChatResult{Content: "好了"}, nil
	}
	runner := &Runner{chat: chat, tools: registry, MaxSteps: 2, enabled: true, InboundSink: "fake__import"}

	if _, err := runner.Run(context.Background(), RunnerInput{
		SessionID: "s1", Question: "把这张图存一下",
		Inbound: []tools.Attachment{{Name: "a.png", MimeType: "image/png", Data: []byte("PNGDATA")}},
	}); err != nil {
		t.Fatalf("Run 返错：%v", err)
	}

	if len(source.calls) != 1 {
		t.Fatalf("该调一次落点工具，实际 %d", len(source.calls))
	}
	files, ok := source.calls[0].args["files"].([]map[string]any)
	if !ok || len(files) != 1 {
		t.Fatalf("files 没注入：%#v", source.calls[0].args)
	}
	if files[0]["name"] != "a.png" || files[0]["mime"] != "image/png" {
		t.Errorf("name/mime 不对：%#v", files[0])
	}
	if want := base64.StdEncoding.EncodeToString([]byte("PNGDATA")); files[0]["data"] != want {
		t.Errorf("data = %v，期望 %q", files[0]["data"], want)
	}
}

// TestRun_没点名就不注入 没配落点（InboundSink 空）时，模型调同名工具也拿不到字节——
// 绝不按名字猜。图片仍照常喂模型（这里不验，另有用例）。
func TestRun_没点名就不注入(t *testing.T) {
	source := &fakeSource{specs: []tools.Spec{importSpec()}}
	registry := tools.NewRegistry([]tools.Source{source}, tools.DefaultPolicy())

	calls := 0
	chat := func(_ context.Context, _ []llm.ChatMessage, _ []llm.ToolDef) (llm.ChatResult, error) {
		calls++
		if calls == 1 {
			return llm.ChatResult{ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "fake__import", Arguments: `{}`},
			}}, nil
		}
		return llm.ChatResult{Content: "好了"}, nil
	}
	runner := &Runner{chat: chat, tools: registry, MaxSteps: 2, enabled: true}

	if _, err := runner.Run(context.Background(), RunnerInput{
		SessionID: "s1", Question: "存一下",
		Inbound: []tools.Attachment{{Name: "a.png", MimeType: "image/png", Data: []byte("x")}},
	}); err != nil {
		t.Fatalf("Run 返错：%v", err)
	}

	if _, ok := source.calls[0].args["files"]; ok {
		t.Errorf("没点名不该注入 files：%#v", source.calls[0].args)
	}
}
