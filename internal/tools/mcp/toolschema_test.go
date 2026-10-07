package mcp

import (
	"testing"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// TestMCP工具的必填参数真的被校验 这条链以前是**断的，而且没有任何红灯**。
//
// ## 断在哪
//
// `toolSchema` 把 SDK 的 `Tool.InputSchema.Required`（类型是 `[]string`，见
// `mcp-go/mcp/tools.go`）整个塞进 `map[string]any`，而 `tools.ValidateArgs` 只
// 断言 `raw.([]any)`——**恒 false**，于是整段必填校验被跳过。
//
// 它没被发现，是因为 `properties` 是 `map[string]any`、逐属性的 type 校验照常
// 生效，老用例又是从 JSON 反序列化出来的 `[]any`，形状恰好对得上。**两边都对、
// 中间那个接缝错了，而接缝正是最该被测的地方。**
//
// 后果：模型省掉一个必填参数，本地校验放行，请求直达外部 MCP server，由那边
// 回一句模型看不懂的错——而我们这边看起来「已经校验过了」。
//
// 所以这里走**真实的那条路**：SDK 构造一个带 `Required()` 的工具 → `toolSchema`
// → `tools.ValidateArgs`，一步都不绕。
func TestMCP工具的必填参数真的被校验(t *testing.T) {
	tool := mcp.NewTool("search_memories",
		mcp.WithString("query", mcp.Required()),
		mcp.WithString("note"),
	)
	schema := toolSchema(tool)

	// 钉住形状本身：这是以前那条断言恒 false 的原因，形状一变这条就该红
	if _, ok := schema["required"].([]string); !ok {
		t.Fatalf("required 该是 SDK 的 []string，实际 %T（形状变了就去看 ValidateArgs 认不认）",
			schema["required"])
	}

	if got := tools.ValidateArgs(schema, map[string]any{}); got != "缺少必填参数 query" {
		t.Errorf("缺必填该被拦下，实际 %q", got)
	}
	if got := tools.ValidateArgs(schema, map[string]any{"query": nil}); got != "缺少必填参数 query" {
		t.Errorf("必填给了 null 视同缺，实际 %q", got)
	}
	if got := tools.ValidateArgs(schema, map[string]any{"query": "选课通知"}); got != "" {
		t.Errorf("补齐后该放行，实际 %q", got)
	}
	// type 校验那条路本来就是通的（properties 是 map[string]any），一起钉住，
	// 免得修 required 时把它弄坏
	if got := tools.ValidateArgs(schema, map[string]any{"query": 1}); got != "参数 query 应为 string" {
		t.Errorf("类型不对该被拦下，实际 %q", got)
	}
	// 可选参数缺席是合法的
	if got := tools.ValidateArgs(schema, map[string]any{"query": "q"}); got != "" {
		t.Errorf("可选的 note 不填该放行，实际 %q", got)
	}
}

// Test没有required的工具不拦 连接那些没有必填参数的工具（以及 schema 里压根
// 没写 required）时，一条都拿不到名字——那是「不校验」，不是「全部必填」。
func Test没有required的工具不拦(t *testing.T) {
	tool := mcp.NewTool("list_memories", mcp.WithString("note"))
	schema := toolSchema(tool)

	if _, present := schema["required"]; present {
		t.Fatalf("没声明必填时 schema 里不该有 required，实际 %#v", schema["required"])
	}
	if got := tools.ValidateArgs(schema, map[string]any{}); got != "" {
		t.Errorf("空参数该放行，实际 %q", got)
	}
}
