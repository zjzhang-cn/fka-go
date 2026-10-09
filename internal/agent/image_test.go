package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/llm"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// TestRun_图片只在本轮请求不落盘 钉住图片附件的两个关键点：它**进了本轮发给模型的
// 消息**（模型看得到图），但**不落进会话历史**（base64 不该每轮重放一遍）。
func TestRun_图片只在本轮请求不落盘(t *testing.T) {
	dir := t.TempDir()

	var requested []llm.ChatMessage
	chat := func(_ context.Context, messages []llm.ChatMessage, _ []llm.ToolDef) (llm.ChatResult, error) {
		requested = append([]llm.ChatMessage(nil), messages...)
		return llm.ChatResult{Content: "看到了"}, nil
	}
	runner := &Runner{
		chat: chat, tools: tools.NewRegistry(nil, tools.DefaultPolicy()),
		MaxSteps: 1, enabled: true, sessionHistory: llm.NewSessionHistory(dir),
	}

	if _, err := runner.Run(context.Background(), RunnerInput{
		SessionID: "s1", Question: "这是什么",
		Images: []llm.ImageAttachment{{Name: "a.png", DataURI: "data:image/png;base64,AAAA"}},
	}); err != nil {
		t.Fatalf("Run 返错：%v", err)
	}

	// 请求里带上了图片
	found := false
	for _, message := range requested {
		if len(message.ImageAttachments) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("发给模型的请求里没有图片附件")
	}

	// 落盘里一个字都不该有
	data, err := os.ReadFile(filepath.Join(dir, "s1.jsonl"))
	if err != nil {
		t.Fatalf("读历史失败：%v", err)
	}
	if strings.Contains(string(data), "data:image") {
		t.Errorf("图片字节被写进了会话历史：%s", data)
	}
	if !strings.Contains(string(data), "这是什么") {
		t.Errorf("问题本身该照常落盘：%s", data)
	}
}

// TestRun_工具图片经额外user消息进请求且不落盘 钉住工具图片这条搬运：role=tool 的
// 消息只能装字符串，图片必须**另起一条 user 消息**作为附件发给模型，且排在整轮
// tool 消息**之后**（tool 消息要紧跟对应的 tool_calls）。与 `@图片` 同一条规矩：
// 只在本轮请求里，图与那句 transient 说明都不落盘。
func TestRun_工具图片经额外user消息进请求且不落盘(t *testing.T) {
	dir := t.TempDir()
	source := &fakeSource{
		specs:   []tools.Spec{searchSpec()},
		replies: map[string]string{"search": "（已生成图表）"},
		images: map[string][]llm.ImageAttachment{
			"search": {{Name: "chart.png", DataURI: "data:image/png;base64,AAAA"}},
		},
	}
	registry := tools.NewRegistry([]tools.Source{source}, tools.DefaultPolicy())

	var requests [][]llm.ChatMessage
	calls := 0
	chat := func(_ context.Context, messages []llm.ChatMessage, _ []llm.ToolDef) (llm.ChatResult, error) {
		calls++
		requests = append(requests, append([]llm.ChatMessage(nil), messages...))
		if calls == 1 {
			return llm.ChatResult{ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "fake__search", Arguments: `{"q":"x"}`},
			}}, nil
		}
		return llm.ChatResult{Content: "看到图了"}, nil
	}

	runner := &Runner{
		chat: chat, tools: registry, MaxSteps: 2, enabled: true,
		sessionHistory: llm.NewSessionHistory(dir),
	}
	if _, err := runner.Run(context.Background(), RunnerInput{
		SessionID: "s1", Question: "画个图", PrincipalID: "wx1",
	}); err != nil {
		t.Fatalf("Run 返错：%v", err)
	}

	second := requests[1]
	imageIndex := -1
	lastToolIndex := -1
	for i, message := range second {
		if message.Role == llm.RoleTool {
			lastToolIndex = i
		}
		if message.Role == llm.RoleUser && len(message.ImageAttachments) > 0 {
			imageIndex = i
		}
	}
	if imageIndex < 0 {
		t.Fatal("工具图片没有以 user 消息附件进请求——模型看不到图")
	}
	if imageIndex < lastToolIndex {
		t.Error("图片 user 消息排在 tool 消息之前，会打断「tool 消息紧跟 tool_calls」")
	}
	if got := second[imageIndex].ImageAttachments[0].DataURI; got != "data:image/png;base64,AAAA" {
		t.Errorf("图片附件不对：%q", got)
	}
	if second[imageIndex].Transient != true {
		t.Error("这条补出来的 user 消息该标成 transient（不落盘）")
	}

	data, err := os.ReadFile(filepath.Join(dir, "s1.jsonl"))
	if err != nil {
		t.Fatalf("读历史失败：%v", err)
	}
	if strings.Contains(string(data), "data:image") {
		t.Errorf("图片字节被写进了会话历史：%s", data)
	}
	if strings.Contains(string(data), "工具返回了") {
		t.Errorf("transient 说明被写进了会话历史：%s", data)
	}
	if !strings.Contains(string(data), "画个图") {
		t.Errorf("问题本身该照常落盘：%s", data)
	}
}
