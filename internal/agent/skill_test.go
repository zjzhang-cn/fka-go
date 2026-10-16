package agent

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/llm"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// importSkillSpec 一个模拟沙盒 import_skill 的工具声明。
func importSkillSpec() tools.Spec {
	return tools.Spec{Name: "import_skill", Description: "把技能自带脚本落进沙盒", Effect: tools.EffectRead}
}

// toolResultContains 看某个 chat 调用收到的 messages 里有没有一条工具结果包含 want。
func toolResultContains(messages []llm.ChatMessage, want string) bool {
	for _, message := range messages {
		if message.Role == llm.RoleTool && strings.Contains(message.Content, want) {
			return true
		}
	}
	return false
}

// TestRun_技能脚本注入被点名的落点工具 只有工具全名等于 SkillSink 时，循环才按模型
// 给的 `skill` 把该技能的脚本字节填进 `files`——子目录结构要保留，字节要原样。
func TestRun_技能脚本注入被点名的落点工具(t *testing.T) {
	source := &fakeSource{specs: []tools.Spec{importSkillSpec()}}
	registry := tools.NewRegistry([]tools.Source{source}, tools.DefaultPolicy())

	calls := 0
	chat := func(_ context.Context, _ []llm.ChatMessage, _ []llm.ToolDef) (llm.ChatResult, error) {
		calls++
		if calls == 1 {
			return llm.ChatResult{ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "fake__import_skill", Arguments: `{"skill":"备份"}`},
			}}, nil
		}
		return llm.ChatResult{Content: "好了"}, nil
	}
	runner := &Runner{
		chat: chat, tools: registry, MaxSteps: 2, enabled: true,
		SkillSink: "fake__import_skill",
		SkillFiles: func(name string) ([]tools.Attachment, bool) {
			if name != "备份" {
				return nil, false
			}
			return []tools.Attachment{
				{Name: "scripts/run.py", Data: []byte("print(1)")},
				{Name: "SKILL.md", Data: []byte("步骤")},
			}, true
		},
	}

	if _, err := runner.Run(context.Background(), RunnerInput{SessionID: "s1", Question: "跑一下备份"}); err != nil {
		t.Fatalf("Run 返错：%v", err)
	}

	if len(source.calls) != 1 {
		t.Fatalf("该调一次落点工具，实际 %d", len(source.calls))
	}
	files, ok := source.calls[0].args["files"].([]map[string]any)
	if !ok || len(files) != 2 {
		t.Fatalf("脚本没注入：%#v", source.calls[0].args)
	}
	if files[0]["name"] != "scripts/run.py" {
		t.Errorf("相对路径该保留：%#v", files[0])
	}
	if want := base64.StdEncoding.EncodeToString([]byte("print(1)")); files[0]["data"] != want {
		t.Errorf("字节不对：%#v", files[0])
	}
}

// TestRun_技能不存在当场回错 模型把技能名叫错时，要把它挡在注册表之前并回一句
// 「没有叫 X 的技能」，而不是注入空 files 让沙盒去报一句含糊的错。
func TestRun_技能不存在当场回错(t *testing.T) {
	source := &fakeSource{specs: []tools.Spec{importSkillSpec()}}
	registry := tools.NewRegistry([]tools.Source{source}, tools.DefaultPolicy())

	var seen [][]llm.ChatMessage
	calls := 0
	chat := func(_ context.Context, messages []llm.ChatMessage, _ []llm.ToolDef) (llm.ChatResult, error) {
		calls++
		seen = append(seen, messages)
		if calls == 1 {
			return llm.ChatResult{ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "fake__import_skill", Arguments: `{"skill":"不存在的技能"}`},
			}}, nil
		}
		return llm.ChatResult{Content: "好"}, nil
	}
	runner := &Runner{
		chat: chat, tools: registry, MaxSteps: 2, enabled: true,
		SkillSink:  "fake__import_skill",
		SkillFiles: func(string) ([]tools.Attachment, bool) { return nil, false },
	}

	if _, err := runner.Run(context.Background(), RunnerInput{SessionID: "s1", Question: "跑"}); err != nil {
		t.Fatalf("Run 返错：%v", err)
	}
	if len(source.calls) != 0 {
		t.Fatalf("技能不存在时不该进注册表，实际 %d 次", len(source.calls))
	}
	if len(seen) < 2 || !toolResultContains(seen[len(seen)-1], "没有叫") {
		t.Errorf("该把「没有叫」告诉模型：%#v", seen)
	}
}

// TestRun_技能没脚本当场回错 技能存在但没有自带脚本时，同样在注入前说清，
// 免得模型对着一个空 files 反复试。
func TestRun_技能没脚本当场回错(t *testing.T) {
	source := &fakeSource{specs: []tools.Spec{importSkillSpec()}}
	registry := tools.NewRegistry([]tools.Source{source}, tools.DefaultPolicy())

	var seen [][]llm.ChatMessage
	calls := 0
	chat := func(_ context.Context, messages []llm.ChatMessage, _ []llm.ToolDef) (llm.ChatResult, error) {
		calls++
		seen = append(seen, messages)
		if calls == 1 {
			return llm.ChatResult{ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "fake__import_skill", Arguments: `{"skill":"纯文本技能"}`},
			}}, nil
		}
		return llm.ChatResult{Content: "好"}, nil
	}
	runner := &Runner{
		chat: chat, tools: registry, MaxSteps: 2, enabled: true,
		SkillSink:  "fake__import_skill",
		SkillFiles: func(string) ([]tools.Attachment, bool) { return nil, true },
	}

	if _, err := runner.Run(context.Background(), RunnerInput{SessionID: "s1", Question: "跑"}); err != nil {
		t.Fatalf("Run 返错：%v", err)
	}
	if len(source.calls) != 0 {
		t.Fatalf("没有脚本时不该进注册表，实际 %d 次", len(source.calls))
	}
	if len(seen) < 2 || !toolResultContains(seen[len(seen)-1], "没有自带脚本") {
		t.Errorf("该说清技能没有脚本：%#v", seen)
	}
}

// TestRun_没点名技能落点就不注入 没配 SkillSink 时，模型调同名工具也拿不到脚本字节。
func TestRun_没点名技能落点就不注入(t *testing.T) {
	source := &fakeSource{specs: []tools.Spec{importSkillSpec()}}
	registry := tools.NewRegistry([]tools.Source{source}, tools.DefaultPolicy())

	calls := 0
	chat := func(_ context.Context, _ []llm.ChatMessage, _ []llm.ToolDef) (llm.ChatResult, error) {
		calls++
		if calls == 1 {
			return llm.ChatResult{ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "fake__import_skill", Arguments: `{"skill":"备份"}`},
			}}, nil
		}
		return llm.ChatResult{Content: "好"}, nil
	}
	runner := &Runner{
		chat: chat, tools: registry, MaxSteps: 2, enabled: true,
		SkillFiles: func(string) ([]tools.Attachment, bool) { return []tools.Attachment{{Name: "x.sh"}}, true },
	}

	if _, err := runner.Run(context.Background(), RunnerInput{SessionID: "s1", Question: "跑"}); err != nil {
		t.Fatalf("Run 返错：%v", err)
	}
	if len(source.calls) != 1 {
		t.Fatalf("没点名时该照常进注册表，实际 %d 次", len(source.calls))
	}
	if _, injected := source.calls[0].args["files"]; injected {
		t.Errorf("没点名不该注入 files：%#v", source.calls[0].args)
	}
}
