package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/llm"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// fakeSource 一个可控的工具源：按预设脚本回答。
type fakeSource struct {
	specs []tools.Spec
	// replies 工具名 → 返回内容（成功）
	replies map[string]string
	// complaints 工具名 → 返回内容（**ok=false**，但工具名是登记过的）
	//
	// 分开是因为「注册表说没有这个工具」与「工具有话说」走的是两条不同的路，
	// 而只有后者能验证 Result.OK 被搬上来了又被循环丢掉。
	complaints map[string]string
	// images 工具名 → 随结果返回的图片（模拟 MCP 的 image 内容块）
	images map[string][]llm.ImageAttachment
	// deliver 工具名 → 随结果返回的、标注给用户的附件（模拟 audience=user）
	deliver map[string][]tools.Attachment
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

	result := tools.FailResult("没有叫 %s 的东西", name)
	if reply, ok := f.replies[name]; ok {
		result = tools.OKResult(reply)
	} else if complaint, ok := f.complaints[name]; ok {
		result = tools.FailResult("%s", complaint)
	}
	result.Images = f.images[name]
	result.Deliver = f.deliver[name]
	return result, nil
}

// fakeReply 记录经它发出的东西。只关心「发没发、发的是什么」。
type fakeReply struct {
	files  []string
	images []string
}

func (f *fakeReply) Text(context.Context, string) error          { return nil }
func (f *fakeReply) File(context.Context, string, string) error  { return nil }
func (f *fakeReply) Image(context.Context, string, string) error { return nil }
func (f *fakeReply) FileBytes(_ context.Context, name, _ string, _ []byte) error {
	f.files = append(f.files, name)
	return nil
}
func (f *fakeReply) ImageBytes(_ context.Context, name, _ string, _ []byte) error {
	f.images = append(f.images, name)
	return nil
}

func (f *fakeSource) PromptSection(tc tools.Context) (string, error) { return "", nil }
func (f *fakeSource) Close() error                                   { f.closed = true; return nil }

// newTestRunner 只装配依赖的 runner。maxSteps 省略时是 0，也就是「用 DefaultMaxSteps」。
//
// 这些用例以前调的是自由函数 `Run(ctx, RunInput, Deps{})`，那份 `Deps` 是 `Runner`
// 私有字段的镜像。现在只有 `(*Runner).Run` 一个入口，所以测试直接装配 `Runner`——
// **要钉的是行为，装配只是手段**。
func newTestRunner(chat llm.ChatClient, registry tools.Service, maxSteps ...int) *Runner {
	steps := 0
	if len(maxSteps) > 0 {
		steps = maxSteps[0]
	}
	return &Runner{chat: chat, tools: registry, MaxSteps: steps, enabled: true}
}

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

	result, err := newTestRunner(chat, registry).Run(context.Background(), RunnerInput{
		SessionID:   "s1",
		Question:    "房产证在哪",
		PrincipalID: "wx1",
	})
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

	// 可见记录：给 CLI/TUI 显示用。名字是模型看到的全名，参数是**原样字符串**，
	// 结果是喂回去的那段——三者都要在（chat 据此逐条渲染）。
	if len(result.ToolEvents) != 1 {
		t.Fatalf("ToolEvents = %d，期望 1", len(result.ToolEvents))
	}
	event := result.ToolEvents[0]
	if event.Name != "fake__search" || event.Arguments != `{"q":"房产证"}` || event.Result != "房产证在抽屉里" {
		t.Errorf("ToolEvent 不对：%+v", event)
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

	result, err := newTestRunner(chat, registry).Run(context.Background(), RunnerInput{
		SessionID: "s1", Question: "查不到", PrincipalID: "wx1",
	})
	if err != nil {
		t.Fatalf("工具失败不该让整轮返错：%v", err)
	}
	if result.Text != "换个词再试" {
		t.Errorf("Text = %q", result.Text)
	}
}

// TestRun_成不成功都只有一条通道 钉住「循环不读 Result.OK」。
//
// `Result.OK` 是**如实搬过 MCP 边界**的（`mcp/source.go` 把 SDK 的 IsError 映射上来），
// 但工具对模型只有一条通道：那段文字。所以 ok=false 的结果**照样原样**变成
// role=tool 的正文——不换一句话、不短路、不重试。
//
// 与上面那条的区别：那条的失败来自**注册表**（名字不在表里），这条来自**工具本身**
// 有话说（名字在表里，OK=false）。后者才真的经过 Result.OK 这一路。
func TestRun_成不成功都只有一条通道(t *testing.T) {
	source := &fakeSource{
		specs:      []tools.Spec{searchSpec()},
		complaints: map[string]string{"search": "没找到「房产证」这一项"},
	}
	registry := tools.NewRegistry([]tools.Source{source}, tools.DefaultPolicy())

	chat, seen := scriptedChat(t, "fake__search", `{"q":"房产证"}`, "我这边也没有")

	result, err := newTestRunner(chat, registry).Run(context.Background(), RunnerInput{
		SessionID: "s1", Question: "房产证在哪", PrincipalID: "wx1",
	})
	if err != nil {
		t.Fatalf("工具失败不该让整轮返错：%v", err)
	}
	if result.StoppedBy != StoppedByAnswered {
		t.Errorf("StoppedBy = %q，期望 %q——失败不该变成收尾那一路", result.StoppedBy, StoppedByAnswered)
	}

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
	if toolMsg.Content != "没找到「房产证」这一项" {
		t.Errorf("ok=false 的那句必须原文喂回去，实际 %q", toolMsg.Content)
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

	result, err := newTestRunner(chat, registry, 2).Run(context.Background(), RunnerInput{
		SessionID: "s1", Question: "整理所有资料", PrincipalID: "wx1",
	})
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

	result, err := newTestRunner(chat, registry, 1).Run(context.Background(), RunnerInput{
		SessionID: "s1", Question: "q", PrincipalID: "wx1",
	})
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

	if _, err := newTestRunner(chat, registry).Run(context.Background(), RunnerInput{
		SessionID: "s1", Question: "q", PrincipalID: "wx1",
	}); err == nil {
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
	if got := userContent(RunnerInput{Question: "在哪"}); got != "在哪" {
		t.Errorf("没传引用时不该加东西：%q", got)
	}
	got := userContent(RunnerInput{Question: "在哪", QuotedText: "我昨天说过了"})
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

// TestNewRunner_关掉时给出的是一个能自答的实现 「没有 agent」是**一个对象**，不是 nil。
//
// 以前这里是 `!= nil` 的断言。那条断言钉的是一个用指针缺省表达「缺席」的约定——而指针
// 说不出原因，于是 `cmd/fka` 不得不自己再判一次才知道该印「LLM_TOOLS=off」。
// 现在缺席由这个对象自己回答：Enabled() 回 false、Run 回 ErrNoRunner。
func TestNewRunner_关掉时给出的是一个能自答的实现(t *testing.T) {
	registry := tools.NewRegistry(nil, tools.DefaultPolicy())

	for _, off := range []string{"off", "0", "false", "no"} {
		t.Setenv("LLM_TOOLS", off)
		runner := NewRunner(nil, registry, RunnerOptions{})
		if runner == nil {
			t.Fatalf("LLM_TOOLS=%s 时不该返回 nil——缺席是一个实现", off)
		}
		if runner.Enabled() {
			t.Errorf("LLM_TOOLS=%s 时 Enabled() 应为假", off)
		}
		// **不返错**：缺席是已知状态，不是故障
		if ok, err := runner.HasTools(context.Background()); ok || err != nil {
			t.Errorf("缺席的 runner 的 HasTools = %v, %v；期望 false, nil", ok, err)
		}
		if _, err := runner.Run(context.Background(), RunnerInput{Question: "q"}); !errors.Is(err, ErrNoRunner) {
			t.Errorf("缺席的 runner 的 Run 返 %v；期望 ErrNoRunner", err)
		}
	}
}

// TestNewRunner_没接上模型时同样给出实现 chat 为 nil 与 LLM_TOOLS=off 是同一个结果：
// 调用方不该为这两种「没有」写两遍判据。
func TestNewRunner_没接上模型时同样给出实现(t *testing.T) {
	t.Setenv("LLM_TOOLS", "")
	registry := tools.NewRegistry(nil, tools.DefaultPolicy())

	runner := NewRunner(nil, registry, RunnerOptions{})
	if runner == nil || runner.Enabled() {
		t.Fatalf("chat 为 nil 时应给出一个缺席的实现，得到 %#v", runner)
	}
	if _, err := runner.Run(context.Background(), RunnerInput{}); !errors.Is(err, ErrNoRunner) {
		t.Errorf("Run 返 %v；期望 ErrNoRunner", err)
	}
}

// TestRunner_零值Runner不会炸 零值 `Runner`（也就是缺席的实现）任何方法都要能安全调用。
func TestRunner_零值Runner不会炸(t *testing.T) {
	var runner Runner
	if runner.Enabled() {
		t.Error("零值 Runner 的 Enabled() 应为假")
	}
	if ok, err := runner.HasTools(context.Background()); ok || err != nil {
		t.Errorf("零值 Runner 的 HasTools = %v, %v", ok, err)
	}
	if _, err := runner.Run(context.Background(), RunnerInput{}); !errors.Is(err, ErrNoRunner) {
		t.Errorf("零值 Runner 的 Run 返 %v；期望 ErrNoRunner", err)
	}
}

// TestRun_工具声明原样进模型 模型看到的是**带源前缀的全名**与**原样透传的参数 schema**。
//
// （这条用例以前叫 TestDeps_、注释写着「工具声明进了预算」，但它断言的其实是模型
// 看到什么。名字里那个类型已经不存在了，所以一并改掉。）
func TestRun_工具声明原样进模型(t *testing.T) {
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

	if _, err := newTestRunner(chat, registry).Run(context.Background(), RunnerInput{
		SessionID: "s", Question: "q", PrincipalID: "wx",
	}); err != nil {
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

// toolMessageContent 找第二轮请求里那条 tool 消息的正文。找不到返回空串。
func toolMessageContent(seen [][]llm.ChatMessage) string {
	if len(seen) < 2 {
		return ""
	}
	for _, message := range seen[1] {
		if message.Role == llm.RoleTool {
			return message.Content
		}
	}
	return ""
}

// TestRun_标注给用户的附件经渠道发出 工具结果里 audience=user 的附件由循环投递，
// 且**在给模型的 tool 消息里留下一句「发了几个」**——否则模型会以为没做到而重试。
func TestRun_标注给用户的附件经渠道发出(t *testing.T) {
	source := &fakeSource{
		specs:   []tools.Spec{searchSpec()},
		replies: map[string]string{"search": "导出完成"},
		deliver: map[string][]tools.Attachment{"search": {
			{Name: "报告.pdf", MimeType: "application/pdf", Data: []byte("pdf")},
			{Name: "图.png", MimeType: "image/png", Data: []byte("png")},
		}},
	}
	registry := tools.NewRegistry([]tools.Source{source}, tools.DefaultPolicy())
	chat, seen := scriptedChat(t, "fake__search", `{"q":"x"}`, "好了")

	runner := newTestRunner(chat, registry)
	runner.allowSend = true
	reply := &fakeReply{}

	if _, err := runner.Run(context.Background(), RunnerInput{
		SessionID: "s", AccountID: "a", Question: "生成报告", Reply: reply,
	}); err != nil {
		t.Fatalf("Run 返错：%v", err)
	}

	if len(reply.files) != 1 || reply.files[0] != "报告.pdf" {
		t.Errorf("文件附件没走 FileBytes：%v", reply.files)
	}
	if len(reply.images) != 1 || reply.images[0] != "图.png" {
		t.Errorf("图片附件该走 ImageBytes（mime 是 image/）：%v", reply.images)
	}
	if body := toolMessageContent(*seen); !strings.Contains(body, "发给用户") {
		t.Errorf("tool 消息里该有一句投递结果：%q", body)
	}
}

// TestRun_未放行send时不投递 MCP 工具都是 external，不放行 send 就不该有任何附件
// 被送出去——这道闸拦的是「任意被配置的 server 让 agent 往用户发东西」。
func TestRun_未放行send时不投递(t *testing.T) {
	source := &fakeSource{
		specs:   []tools.Spec{searchSpec()},
		replies: map[string]string{"search": "ok"},
		deliver: map[string][]tools.Attachment{"search": {
			{Name: "a.txt", MimeType: "text/plain", Data: []byte("x")},
		}},
	}
	registry := tools.NewRegistry([]tools.Source{source}, tools.DefaultPolicy())
	chat, seen := scriptedChat(t, "fake__search", `{"q":"x"}`, "好了")

	reply := &fakeReply{}
	if _, err := newTestRunner(chat, registry).Run(context.Background(), RunnerInput{
		SessionID: "s", AccountID: "a", Question: "q", Reply: reply,
	}); err != nil {
		t.Fatalf("Run 返错：%v", err)
	}

	if len(reply.files)+len(reply.images) != 0 {
		t.Errorf("未放行 send 时不该投递，实际 files=%v images=%v", reply.files, reply.images)
	}
	if body := toolMessageContent(*seen); !strings.Contains(body, "未启用发送能力") {
		t.Errorf("该告诉模型为什么没发：%q", body)
	}
}
