package tools

import (
	"context"
	"testing"
)

func source(id string, specs ...Spec) Source {
	return SourceFuncs{
		SourceID:    id,
		SourceLabel: id + " 源",
		ListFunc:    func(ctx context.Context, tc Context) ([]Spec, error) { return specs, nil },
		CallFunc: func(ctx context.Context, name string, args map[string]any, tc Context) (Result, error) {
			return OKResult("调了 " + name), nil
		},
	}
}

func readSpec(name string) Spec {
	return Spec{Name: name, Description: "d", Parameters: map[string]any{"type": "object"}, Effect: EffectRead}
}

// TestRegistry_加源前缀 模型看到的是全名，源只认自己的短名。
func TestRegistry_加源前缀(t *testing.T) {
	registry := NewRegistry([]Source{source("skills", readSpec("load"), readSpec("list"))}, DefaultPolicy())

	tools, err := registry.Tools(context.Background(), Context{})
	if err != nil {
		t.Fatalf("Tools 返错：%v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("应有 2 件：%+v", tools)
	}
	if tools[0].FullName != "skills__load" || tools[1].FullName != "skills__list" {
		t.Errorf("全名不对：%s / %s", tools[0].FullName, tools[1].FullName)
	}
}

func TestRegistry_前缀重复整源跳过(t *testing.T) {
	// 两个源共用一个前缀：宁可整个跳过后一个，也不让它覆盖前一个
	registry := NewRegistry([]Source{
		source("skills", readSpec("load")),
		source("skills", readSpec("other")),
	}, DefaultPolicy())

	tools, err := registry.Tools(context.Background(), Context{})
	if err != nil {
		t.Fatalf("Tools 返错：%v", err)
	}
	if len(tools) != 1 || tools[0].FullName != "skills__load" {
		t.Errorf("后一个源应整源跳过：%+v", tools)
	}
}

// TestRegistry_未放行的工具不告诉模型 「不告诉」而不是「告诉它、等它调、再回没权限」：
// 后者会让模型以为工具只是暂时不可用，于是换个名字再试一次，白烧一轮。
func TestRegistry_未放行的工具不告诉模型(t *testing.T) {
	deleting := readSpec("delete_document")
	deleting.Effect = EffectDelete
	registry := NewRegistry([]Source{source("builtin", readSpec("search_documents"), deleting)}, DefaultPolicy())

	tools, err := registry.Tools(context.Background(), Context{})
	if err != nil {
		t.Fatalf("Tools 返错：%v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("默认只放行 read，应只剩 1 件：%+v", tools)
	}

	// 但被叫到时要给一句人话，而且要说清是「没启用」不是「不存在」
	result := registry.Call(context.Background(), "builtin__delete_document", map[string]any{}, Context{})
	if result.OK {
		t.Fatal("未放行的工具不该执行成功")
	}
	if !contains(result.Content, "delete") || !contains(result.Content, "没有启用") {
		t.Errorf("回给模型的话要说清是权限门：%q", result.Content)
	}
}

func TestRegistry_名字不存在与权限门是两句话(t *testing.T) {
	registry := NewRegistry([]Source{source("builtin", readSpec("search_documents"))}, DefaultPolicy())

	result := registry.Call(context.Background(), "builtin__压根没有", map[string]any{}, Context{})
	if contains(result.Content, "没有启用") {
		t.Errorf("名字错了不该说成权限问题：%q", result.Content)
	}
	if !contains(result.Content, "没有叫") {
		t.Errorf("应说没有这个工具：%q", result.Content)
	}
}

func TestRegistry_五类放行逐类独立(t *testing.T) {
	specs := []Spec{}
	for _, effect := range AllEffects {
		spec := readSpec(string(effect) + "_tool")
		spec.Effect = effect
		specs = append(specs, spec)
	}

	registry := NewRegistry([]Source{source("t", specs...)},
		newPolicy([]Effect{EffectRead, EffectExternal}))

	tools, err := registry.Tools(context.Background(), Context{})
	if err != nil {
		t.Fatalf("Tools 返错：%v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("只放行 read + external，应剩 2 件：%+v", tools)
	}
	byName := map[string]bool{}
	for _, tool := range tools {
		byName[tool.FullName] = true
	}
	if !byName["t__read_tool"] || !byName["t__external_tool"] {
		t.Errorf("放行的两类都在：%+v", byName)
	}
}

func TestValidateArgs(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"q":    map[string]any{"type": "string"},
			"n":    map[string]any{"type": "number"},
			"flag": map[string]any{"type": "boolean"},
			"list": map[string]any{"type": "array"},
			"any":  map[string]any{"type": "从没见过的类型"},
		},
		"required": []any{"q"},
	}

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"正常", map[string]any{"q": "房产证"}, ""},
		{"缺必填", map[string]any{"n": float64(1)}, "缺少必填参数 q"},
		{"必填给了 null 视同缺", map[string]any{"q": nil}, "缺少必填参数 q"},
		{"类型不对", map[string]any{"q": 1}, "参数 q 应为 string"},
		{"数字对", map[string]any{"q": "x", "n": 1.5}, ""},
		{"布尔对", map[string]any{"q": "x", "flag": true}, ""},
		{"数组对", map[string]any{"q": "x", "list": []any{1}}, ""},
		{"未声明的属性不拦", map[string]any{"q": "x", "未知": 1}, ""},
		// 声明里写了不认识的类型时**不拦**：拦了会把「声明写得比校验更宽」变成工具用不了
		{"不认识的类型不拦", map[string]any{"q": "x", "any": 1}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidateArgs(schema, tc.args); got != tc.want {
				t.Errorf("ValidateArgs = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestValidateArgs_认两种required形状 上面那张表的 `required` 是 `[]any`，那是
// **技能源**的形状（schema 来自 JSON 反序列化）。MCP 源走的是另一条：
// `toolSchema` 直接放 SDK 的 `Tool.InputSchema.Required`，那是 **`[]string`**。
//
// 只认 `[]any` 时 MCP 那条路的断言恒 false，整段必填校验被静默跳过，而
// type 校验照常生效、用例照常全绿——见 `internal/tools/mcp/toolschema_test.go`
// 里那条走真实链路的用例。
func TestValidateArgs_认两种required形状(t *testing.T) {
	properties := map[string]any{"q": map[string]any{"type": "string"}}

	cases := []struct {
		name     string
		required any
		want     string
	}{
		{"技能源的 []any", []any{"q"}, "缺少必填参数 q"},
		{"MCP 源的 []string", []string{"q"}, "缺少必填参数 q"},
		{"[]any 里混了非字符串就跳过那一项", []any{1}, ""},
		// 认不出的形状**不拦必填**：schema 写得不合法是它自己的问题，
		// 不该让工具整个用不了（与 matchesType 对未知类型不拦同一个取舍）
		{"认不出的形状不拦必填", "q", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			schema := map[string]any{
				"type": "object", "properties": properties, "required": tc.required,
			}
			if got := ValidateArgs(schema, map[string]any{}); got != tc.want {
				t.Errorf("ValidateArgs = %q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestSanitizeSourceID(t *testing.T) {
	cases := map[string]string{
		"mcp":          "mcp",
		"  Skills  ":   "skills",
		"my-source":    "my_source",
		"weird!!chars": "weird__chars",
		"__trim__":     "trim",
		"---":          "",
	}
	for input, want := range cases {
		if got := SanitizeSourceID(input); got != want {
			t.Errorf("SanitizeSourceID(%q) = %q，期望 %q", input, got, want)
		}
	}
	// 超长截断
	long := ""
	for i := 0; i < 40; i++ {
		long += "a"
	}
	if got := SanitizeSourceID(long); len(got) != MaxSourceIDChars {
		t.Errorf("超长应截到 %d，实际 %d", MaxSourceIDChars, len(got))
	}
}

func TestReadToolPolicy_认不出时退回只读(t *testing.T) {
	t.Setenv(ToolEffectsEnv, "read,write,fly")
	policy := ReadToolPolicy()
	if !policy.Allows(EffectRead) {
		t.Error("read 应放行")
	}
	// 认不出的只警告并忽略，**不整个作废**
	if !policy.Allows(EffectRead) {
		t.Error("一个认不出不该把合法的也丢掉")
	}
}

func TestReadToolPolicy_全认不出退回只读(t *testing.T) {
	// 空集等于「什么都没放行」，那与 LLM_TOOLS=0 语义重叠。
	// 配置写坏要退回**更安全**的一边
	t.Setenv(ToolEffectsEnv, "write,fly")
	if got := ReadToolPolicy().Allowed(); len(got) != 1 || got[0] != EffectRead {
		t.Errorf("全认不出时应退回只读：%v", got)
	}
}

func TestReadToolPolicy_大小写与空格(t *testing.T) {
	t.Setenv(ToolEffectsEnv, " READ , Memory ,  DELETE ")
	policy := ReadToolPolicy()
	for _, effect := range []Effect{EffectRead, EffectMemory, EffectDelete} {
		if !policy.Allows(effect) {
			t.Errorf("%s 应放行", effect)
		}
	}
}

func TestDescribeToolPolicy(t *testing.T) {
	if got := DescribeToolPolicy(DefaultPolicy()); got != "read（memory、send、delete、external未启用）" {
		t.Errorf("= %q", got)
	}
	all := newPolicy(AllEffects)
	if got := DescribeToolPolicy(all); got != "read、memory、send、delete、external（全部）" {
		t.Errorf("= %q", got)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
