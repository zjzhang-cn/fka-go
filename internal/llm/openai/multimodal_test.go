package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/llm"
)

// Test图片附件_发成内容块数组 钉住多模态的线格式：带图的 user 消息 content 是
// 内容块数组（text 块 + image_url 块），而不是字符串。
func Test图片附件_发成内容块数组(t *testing.T) {
	messages := toAPIMessages([]llm.ChatMessage{{
		Role:    llm.RoleUser,
		Content: "这是什么",
		ImageAttachments: []llm.ImageAttachment{
			{Name: "a.png", DataURI: "data:image/png;base64,AAAA"},
		},
	}})
	if len(messages) != 1 {
		t.Fatalf("翻译出 %d 条消息，想要 1", len(messages))
	}
	encoded, err := json.Marshal(messages[0])
	if err != nil {
		t.Fatalf("序列化失败（Content 与 MultiContent 不能同时给）：%v", err)
	}
	got := string(encoded)
	for _, want := range []string{`"content":[`, `"type":"text"`, "这是什么", `"type":"image_url"`, "data:image/png;base64,AAAA"} {
		if !strings.Contains(got, want) {
			t.Errorf("请求体缺 %s：%s", want, got)
		}
	}
}

// Test图片附件_纯图无文本：只有图、没有文字时不该造一个空的 text 块。
func Test图片附件_纯图无文本(t *testing.T) {
	messages := toAPIMessages([]llm.ChatMessage{{
		Role:             llm.RoleUser,
		ImageAttachments: []llm.ImageAttachment{{Name: "a.png", DataURI: "data:image/png;base64,AAAA"}},
	}})
	encoded, err := json.Marshal(messages[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"type":"text"`) {
		t.Errorf("没有文本就不该造 text 块：%s", encoded)
	}
}

// Test普通消息仍是字符串content：没有图片时线格式一字不变（回归）。
func Test普通消息仍是字符串content(t *testing.T) {
	messages := toAPIMessages([]llm.ChatMessage{{Role: llm.RoleUser, Content: "你好"}})
	encoded, err := json.Marshal(messages[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"content":"你好"`) {
		t.Errorf("普通消息该是字符串 content：%s", encoded)
	}
	if strings.Contains(string(encoded), `"content":[`) {
		t.Errorf("普通消息不该变成内容块数组：%s", encoded)
	}
}
