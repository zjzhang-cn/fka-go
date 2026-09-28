package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/llm"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// fakeSource 一个可控的工具源：按预设脚本回答。
type fakeSource struct {
	specs []tools.Spec
	// replies 工具名 → 返回内容
	replies map[string]string
	// calls 记录被调过的（工具名, 参数）
	calls  []recorded
	closed bool
}

type recorded struct {
	name string
	args map[string]any
}

func (f *fakeSource) ID() string    { return "fake" }
func (f *fakeSource) Label() string { return "假源" }

func (f *fakeSource) List(ctx context.Context, tc tools.Context) ([]tools.Spec, error) {
	return f.specs, nil
}

func (f *fakeSource) Call(ctx context.Context, name string, args map[string]any, tc tools.Context) (tools.Result, error) {
	f.calls = append(f.calls, recorded{name: name, args: args})
	if reply, ok := f.replies[name]; ok {
		return tools.OKResult(reply), nil
	}
	return tools.FailResult("没有叫 %s 的东西", name), nil
}

func (f *fakeSource) PromptSection(tc tools.Context) (string, error) { return "", nil }
func (f *fakeSource) Close() error                                   { f.closed = true; return nil }

func searchSpec() tools.Spec {
	return tools.Spec{
		Name:        "search",
		Description: "搜资料",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"q": map[string]any{"type": "string"}},
			"required":   []any{"q"},
		},
		Effect: tools.EffectRead,
	}
}

// scriptedChat 按脚本逐次返回：第一次要工具，第二次给答案。
func scriptedChat(t *testing.T, toolName, args, finalAnswer string) (llm.ChatClient, *[][]llm.ChatMessage) {
	t.Helper()
	calls := 0
	seen := &[][]llm.ChatMessage{}

	return func(ctx context.Context, messages []llm.ChatMessage, defs []llm.ToolDef) (llm.ChatResult, error) {
		calls++
		snapshot := make([]llm.ChatMessage, len(messages))
		copy(snapshot, messages)
		*seen = append(*seen, snapshot)

		if calls == 1 {
			return llm.ChatResult{
				ToolCalls: []llm.ToolCall{{ID: "call_1", Name: toolName, Arguments: args}},
			}, nil
		}
		return llm.ChatResult{Content: finalAnswer}, nil
	}, seen
}

func TestRun_一轮工具调用后给出答案(t *testing.T) {
	source := &fakeSource{specs: []tools.Spec{searchSpec()}, replies: map[string]string{"search": "房产证在抽屉里"}}
	registry := tools.NewRegistry([]tools.Source{source}, tools.DefaultPolicy())

	chat, seen := scriptedChat(t, "fake__search", `{"q":"房产证"}`, "在抽屉里")

	result, err := Run(context.Background(), RunInput{
		SessionID:   "s1",
		Question:    "房产证在哪",
		ToolContext: tools.Context{ViewerWxid: "wx1"},
	}, Deps{Chat: chat, Tools: registry})
	if err != nil {
		t.Fatalf("Run 返错：%v", err)
	}

	if result.StoppedBy != StoppedByAnswered {
		t.Errorf("StoppedBy = %q，期望 %q", result.StoppedBy, StoppedByAnswered)
	}
	if result.Steps != 2 {
		t.Errorf("Steps = %d，期望 2（一次要工具、一次作答）", result.Steps)
	}
	if result.Text != "在抽屉里" {
		t.Errorf("Text = %q", result.Text)
	}
	// 源收到的是**短名**——前缀只存在于「模型看到的名字」那一层
	if len(source.calls) != 1 || source.calls[0].name != "search" {
		t.Fatalf("工具没被调用：%+v", source.calls)
	}
	if source.calls[0].args["q"] != "房产证" {
		t.Errorf("参数没解析对：%+v", source.calls[0].args)
	}

	// 第二轮请求里必须有一条 role=tool，且 tool_call_id 指向那次调用——
	// **provider 要求 tool 消息紧跟对应的 tool_calls**，少一条就整轮 400
	second := (*seen)[1]
	var toolMsg *llm.ChatMessage
	for i := range second {
		if second[i].Role == llm.RoleTool {
			toolMsg = &second[i]
			break
		}
	}
	if toolMsg == nil {
		t.Fatal("第二轮请求里没有 role=tool 消息")
	}
	if toolMsg.ToolCallID != "call_1" {
		t.Errorf("tool_call_id = %q，期望 call_1", toolMsg.ToolCallID)
	}
	if toolMsg.Content != "房产证在抽屉里" {
		t.Errorf("tool 结果没喂回去：%q", toolMsg.Content)
	}
}

func TestRun_工具失败也喂回去让模型改(t *testing.T) {
	// 源不认这个工具名 → 注册表回「没有叫…」的 ok=false
	source := &fakeSource{specs: []tools.Spec{searchSpec()}, replies: map[string]string{}}
	registry := tools.NewRegistry([]tools.Source{source}, tools.DefaultPolicy())

	calls := 0
	chat := func(ctx context.Context, messages []llm.ChatMessage, defs []llm.ToolDef) (llm.ChatResult, error) {
		calls++
		if calls == 1 {
			return llm.ChatResult{
				ToolCalls: []llm.ToolCall{{ID: "c1", Name: "fake__search", Arguments: `{"q":"x"}`}},
			}, nil
		}
		return llm.ChatResult{Content: "换个词再试"}, nil
	}

	result, err := Run(context.Background(), RunInput{
		SessionID: "s1", Question: "查不到", ToolContext: tools.Context{ViewerWxid: "wx1"},
	}, Deps{Chat: chat, Tools: registry})
	if err != nil {
		t.Fatalf("工具失败不该让整轮返错：%v", err)
	}
	if result.Text != "换个词再试" {
		t.Errorf("Text = %q", result.Text)
	}
}

// TestRun_到步数上限用无工具收尾 钉住「上限是硬约束」这条：到顶之后必须**不再给工具**，
// 只让模型把已有结果收拢成一段话。
func TestRun_到步数上限用无工具收尾(t *testing.T) {
	source := &fakeSource{specs: []tools.Spec{searchSpec()}, replies: map[string]string{"search": "一段结果"}}
	registry := tools.NewRegistry([]tools.Source{source}, tools.DefaultPolicy())

	toolDefsEachCall := make([]int, 0, 4)
	calls := 0
	chat := func(ctx context.Context, messages []llm.ChatMessage, defs []llm.ToolDef) (llm.ChatResult, error) {
		calls++
		toolDefsEachCall = append(toolDefsEachCall, len(defs))
		if calls <= 2 {
			return llm.ChatResult{
				ToolCalls: []llm.ToolCall{{
					ID: "c" + string(rune('0'+calls)), Name: "fake__search", Arguments: `{"q":"x"}`,
				}},
			}, nil
		}
		return llm.ChatResult{Content: "收拢后的答案"}, nil
	}

	result, err := Run(context.Background(), RunInput{
		SessionID: "s1", Question: "整理所有资料", ToolContext: tools.Context{ViewerWxid: "wx1"},
	}, Deps{Chat: chat, Tools: registry, MaxSteps: 2})
	if err != nil {
		t.Fatalf("Run 返错：%v", err)
	}

	if result.StoppedBy != StoppedByMaxSteps {
		t.Errorf("StoppedBy = %q，期望 %q", result.StoppedBy, StoppedByMaxSteps)
	}
	if result.Steps != 2 {
		t.Errorf("Steps = %d，期望 2", result.Steps)
	}
	if result.Text != "收拢后的答案" {
		t.Errorf("Text = %q", result.Text)
	}

	// 第三次调用（收尾那次）**不能带工具**
	if len(toolDefsEachCall) != 3 {
		t.Fatalf("模型调用次数 = %d，期望 3", len(toolDefsEachCall))
	}
	if toolDefsEachCall[2] != 0 {
		t.Errorf("收尾那次仍带了 %d 件工具，期望 0", toolDefsEachCall[2])
	}
}

// TestRun_收尾失败也返回实话 收尾这一路**故意兜错**：收尾失败不该把整轮带走。
func TestRun_收尾失败也返回实话(t *testing.T) {
	source := &fakeSource{specs: []tools.Spec{searchSpec()}, replies: map[string]string{"search": "r"}}
	registry := tools.NewRegistry([]tools.Source{source}, tools.DefaultPolicy())

	calls := 0
	chat := func(ctx context.Context, messages []llm.ChatMessage, defs []llm.ToolDef) (llm.ChatResult, error) {
		calls++
		if calls == 1 {
			return llm.ChatResult{
				ToolCalls: []llm.ToolCall{{ID: "c1", Name: "fake__search", Arguments: `{"q":"x"}`}},
			}, nil
		}
		return llm.ChatResult{}, errFake
	}

	result, err := Run(context.Background(), RunInput{
		SessionID: "s1", Question: "q", ToolContext: tools.Context{ViewerWxid: "wx1"},
	}, Deps{Chat: chat, Tools: registry, MaxSteps: 1})
	if err != nil {
		t.Fatalf("收尾失败不该往上抛：%v", err)
	}
	if result.Text != MaxStepsAnswer {
		t.Errorf("Text = %q，期望兜底话术", result.Text)
	}
}

// TestRun_模型的错往上抛 「模型/接口失败」与「工具失败」是两类，处置相反。
func TestRun_模型的错往上抛(t *testing.T) {
	source := &fakeSource{specs: []tools.Spec{searchSpec()}}
	registry := tools.NewRegistry([]tools.Source{source}, tools.DefaultPolicy())

	chat := func(ctx context.Context, messages []llm.ChatMessage, defs []llm.ToolDef) (llm.ChatResult, error) {
		return llm.ChatResult{}, errFake
	}

	if _, err := Run(context.Background(), RunInput{
		SessionID: "s1", Question: "q", ToolContext: tools.Context{ViewerWxid: "wx1"},
	}, Deps{Chat: chat, Tools: registry}); err == nil {
		t.Fatal("模型失败应当往上抛，让调用方决定降级")
	}
}

var errFake = &fakeError{}

type fakeError struct{}

func (*fakeError) Error() string { return "假的" }

func TestParseToolArguments(t *testing.T) {
	cases := []struct {
		raw     string
		wantOK  bool
		wantKey string
	}{
		{"", true, ""},               // 空串按「没有参数」
		{"   ", true, ""},            // 纯空白同上
		{"{}", true, ""},             // 空对象
		{`{"q":"房产证"}`, true, "房产证"}, // 正常
		{`{"q":"x",}`, false, ""},    // 尾逗号 = 非法 JSON
		{`[1,2]`, false, ""},         // 数组不是对象
		{`"字符串"`, false, ""},         // 标量不是对象
		{`null`, false, ""},          // null 不是对象
	}

	for _, tc := range cases {
		args, ok := ParseToolArguments(tc.raw)
		if ok != tc.wantOK {
			t.Errorf("ParseToolArguments(%q) ok = %v，期望 %v", tc.raw, ok, tc.wantOK)
			continue
		}
		if ok && tc.wantKey != "" && args["q"] != tc.wantKey {
			t.Errorf("ParseToolArguments(%q)[\"q\"] = %v，期望 %q", tc.raw, args["q"], tc.wantKey)
		}
	}
}

func TestUserContent_引用只在传了的时候才贴(t *testing.T) {
	if got := userContent(RunInput{Question: "在哪"}); got != "在哪" {
		t.Errorf("没传引用时不该加东西：%q", got)
	}
	got := userContent(RunInput{Question: "在哪", QuotedText: "我昨天说过了"})
	if !strings.Contains(got, "我昨天说过了") {
		t.Errorf("引用没贴进去：%q", got)
	}
}

func TestReadConfig(t *testing.T) {
	t.Setenv("LLM_TOOLS", "")
	t.Setenv("LLM_MAX_STEPS", "")
	cfg := ReadConfig()
	if !cfg.Enabled || cfg.MaxSteps != DefaultMaxSteps {
		t.Errorf("默认应开、5 步：%+v", cfg)
	}

	for _, off := range []string{"off", "0", "false", "no", "OFF"} {
		t.Setenv("LLM_TOOLS", off)
		if ReadConfig().Enabled {
			t.Errorf("LLM_TOOLS=%s 应当关掉工具循环", off)
		}
	}

	t.Setenv("LLM_TOOLS", "on")
	if !ReadConfig().Enabled {
		t.Error("LLM_TOOLS=on 应当开着")
	}

	// 超范围夹住而不是返错——启动期的配置笔误不该让服务起不来
	t.Setenv("LLM_MAX_STEPS", "99999")
	if got := ReadConfig().MaxSteps; got != MaxMaxSteps {
		t.Errorf("MaxSteps = %d，期望夹到 %d", got, MaxMaxSteps)
	}
	t.Setenv("LLM_MAX_STEPS", "0")
	if got := ReadConfig().MaxSteps; got != DefaultMaxSteps {
		t.Errorf("MaxSteps=0 非法，应退回默认：%d", got)
	}
}

// TestNewRunner_关掉时返回nil 「没有 agent」用 nil 表达，不另设一个布尔。
func TestNewRunner_关掉时返回nil(t *testing.T) {
	t.Setenv("LLM_TOOLS", "off")
	registry := tools.NewRegistry(nil, tools.DefaultPolicy())

	if NewRunner(nil, registry, RunnerOptions{}) != nil {
		t.Error("LLM_TOOLS=off 时 NewRunner 应返回 nil")
	}

	t.Setenv("LLM_TOOLS", "")
	if NewRunner(nil, registry, RunnerOptions{}) != nil {
		t.Error("chat 为 nil 时 NewRunner 应返回 nil")
	}
}

// TestRunner_无方法时HasTools为假 nil 接收者必须安全——调用方会先问它再决定走哪条路。
func TestRunner_无方法时HasTools为假(t *testing.T) {
	var runner *Runner
	ok, err := runner.HasTools(context.Background())
	if err != nil || ok {
		t.Errorf("nil runner 的 HasTools = %v, %v；期望 false, nil", ok, err)
	}
}

// TestDeps_工具声明进了预算 计算 token 时要算上工具声明——工具一多它也能占不少。
func TestDeps_工具声明不参与消息历史(t *testing.T) {
	source := &fakeSource{specs: []tools.Spec{searchSpec()}, replies: map[string]string{"fake__search": "x"}}
	registry := tools.NewRegistry([]tools.Source{source}, tools.DefaultPolicy())

	var sentDefs []llm.ToolDef
	calls := 0
	chat := func(ctx context.Context, messages []llm.ChatMessage, defs []llm.ToolDef) (llm.ChatResult, error) {
		calls++
		sentDefs = defs
		if calls == 1 {
			return llm.ChatResult{
				ToolCalls: []llm.ToolCall{{ID: "c1", Name: "fake__search", Arguments: `{"q":"x"}`}},
			}, nil
		}
		return llm.ChatResult{Content: "答"}, nil
	}

	if _, err := Run(context.Background(), RunInput{
		SessionID: "s", Question: "q", ToolContext: tools.Context{ViewerWxid: "wx"},
	}, Deps{Chat: chat, Tools: registry}); err != nil {
		t.Fatalf("Run 返错：%v", err)
	}

	if len(sentDefs) != 1 || sentDefs[0].Name != "fake__search" {
		t.Fatalf("模型看到的工具名应带源前缀：%+v", sentDefs)
	}
	// 参数原样透传
	encoded, _ := json.Marshal(sentDefs[0].Parameters)
	if !strings.Contains(string(encoded), "\"q\"") {
		t.Errorf("工具参数没透传：%s", encoded)
	}
}
