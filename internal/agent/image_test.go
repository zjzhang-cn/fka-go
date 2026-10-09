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
