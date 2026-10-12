package mcp

import (
	"context"
	"testing"
)

// TestCatalog_按能力列出资源与提示词 清单要覆盖资源、资源模板与提示词，
// 且模板要标出来（uri 是 RFC 6570 模式，不是可直接读的实例）。
func TestCatalog_按能力列出资源与提示词(t *testing.T) {
	src := sourceWith(t, &fakeConn{
		caps:      ServerCapabilities{Resources: true, Prompts: true},
		resources: []ResourceInfo{{URI: "fka://greeting", Name: "greeting", MIMEType: "text/plain"}},
		templates: []ResourceTemplateInfo{{URITemplate: "fka://zone/{+zone}", Name: "时区"}},
		prompts:   []PromptInfo{{Name: "greet", Description: "打招呼", Arguments: []PromptArg{{Name: "name", Required: true}}}},
	})

	catalog, ok := src.(Catalog)
	if !ok {
		t.Fatal("MCP 源该实现 Catalog")
	}

	resources := catalog.Resources(context.Background())
	if len(resources) != 2 {
		t.Fatalf("该有 1 个资源 + 1 个模板，实际 %+v", resources)
	}
	if resources[0].URI != "fka://greeting" || resources[0].Template {
		t.Errorf("第一个该是普通资源：%+v", resources[0])
	}
	if resources[1].URI != "fka://zone/{+zone}" {
		t.Errorf("模板的 uri 该是模板串：%+v", resources[1])
	}
	if !resources[1].Template {
		t.Errorf("模板要标记 Template=true：%+v", resources[1])
	}

	prompts := catalog.Prompts(context.Background())
	if len(prompts) != 1 || prompts[0].Name != "greet" {
		t.Fatalf("该有一个 prompt，实际 %+v", prompts)
	}
	if len(prompts[0].Arguments) != 1 || !prompts[0].Arguments[0].Required {
		t.Errorf("参数该带必填标记：%+v", prompts[0].Arguments)
	}
}

// TestCatalog_没声明能力就不列 服务器没声明 resources/prompts 时，即使 fake 里塞了
// 数据也不该列出来——那条路调下去只会拿到 method-not-found。
func TestCatalog_没声明能力就不列(t *testing.T) {
	src := sourceWith(t, &fakeConn{
		caps:      ServerCapabilities{},
		resources: []ResourceInfo{{URI: "fka://x"}},
		prompts:   []PromptInfo{{Name: "p"}},
	})
	catalog := src.(Catalog)

	if got := catalog.Resources(context.Background()); len(got) != 0 {
		t.Errorf("没声明 resources 不该列：%+v", got)
	}
	if got := catalog.Prompts(context.Background()); len(got) != 0 {
		t.Errorf("没声明 prompts 不该列：%+v", got)
	}
}
