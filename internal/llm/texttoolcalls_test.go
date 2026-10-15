package llm

import "testing"

// TestParseTextToolCalls_真机那一份原样打捞 文件头里那段就是现场观察到的形状：
// 一段前导说明 + 一个 `<tool_call>` 块。断言正文只留说明、调用带对名字与参数。
func TestParseTextToolCalls_真机那一份原样打捞(t *testing.T) {
	content := "我需要确认 z_3427e1 表的结构，看看它有哪些列。\n\n" +
		"<tool_call>\n" +
		"<function=mcp__sqlite__describe_schema>\n" +
		"<parameter=table>\n" +
		"z_3427e1\n" +
		"</parameter>\n" +
		"</function>\n" +
		"</tool_call>"

	calls, remaining := ParseTextToolCalls(content)

	if len(calls) != 1 {
		t.Fatalf("该打捞出 1 条调用，实际 %d 条：%+v", len(calls), calls)
	}
	if calls[0].Name != "mcp__sqlite__describe_schema" {
		t.Errorf("工具名 = %q", calls[0].Name)
	}
	if calls[0].Arguments != `{"table":"z_3427e1"}` {
		t.Errorf("参数 = %q，期望 {\"table\":\"z_3427e1\"}", calls[0].Arguments)
	}
	if calls[0].ID == "" {
		t.Error("打捞出来的调用必须带非空 id——tool 结果要靠它配对")
	}
	// 正文只留前导说明，标记整段摘掉、两端空白收掉
	if remaining != "我需要确认 z_3427e1 表的结构，看看它有哪些列。" {
		t.Errorf("剩余正文 = %q", remaining)
	}
}

// TestParseTextToolCalls_没有标记就一个字都不改 普通答案绝不能被这段兜底吃掉。
func TestParseTextToolCalls_没有标记就一个字都不改(t *testing.T) {
	content := "家里没有这份资料，你可以在线上政务大厅补办。"

	calls, remaining := ParseTextToolCalls(content)

	if calls != nil {
		t.Fatalf("不该打捞出调用：%+v", calls)
	}
	if remaining != content {
		t.Errorf("正文被改动了：%q", remaining)
	}
}

// TestParseTextToolCalls_认不出的块原样保留 只知道开标记、里面没有可用的 `<function=…>`
// 时，宁可整段当正文留着，也不静默删掉一段可能要给用户看的文字。
func TestParseTextToolCalls_认不出的块原样保留(t *testing.T) {
	content := "<tool_call>\n我是乱写的，没有 function 标记\n</tool_call>"

	calls, remaining := ParseTextToolCalls(content)

	if calls != nil {
		t.Fatalf("不该打捞出调用：%+v", calls)
	}
	if remaining != content {
		t.Errorf("正文被改动了：%q", remaining)
	}
}

// TestParseTextToolCalls_一个块里多个函数 模型不老实时别漏；顺序即出现顺序。
func TestParseTextToolCalls_一个块里多个函数(t *testing.T) {
	content := "<tool_call>\n" +
		"<function=a>\n<parameter=x>1</parameter>\n</function>\n" +
		"<function=b>\n<parameter=y>2</parameter>\n</function>\n" +
		"</tool_call>"

	calls, remaining := ParseTextToolCalls(content)

	if len(calls) != 2 {
		t.Fatalf("该有 2 条，实际 %d：%+v", len(calls), calls)
	}
	if calls[0].Name != "a" || calls[0].Arguments != `{"x":1}` {
		t.Errorf("第一条不对：%+v", calls[0])
	}
	if calls[1].Name != "b" || calls[1].Arguments != `{"y":2}` {
		t.Errorf("第二条不对：%+v", calls[1])
	}
	if calls[0].ID == calls[1].ID {
		t.Errorf("同一轮里两条调用的 id 撞了：%q", calls[0].ID)
	}
	if remaining != "" {
		t.Errorf("只剩标记时正文该是空串，实际 %q", remaining)
	}
}

// TestParseTextToolCalls_无参数调用 不带参数的工具该拿到 `{}` 而不是空串——
// 与 ParseToolArguments 对空串的处理一致。
func TestParseTextToolCalls_无参数调用(t *testing.T) {
	calls, _ := ParseTextToolCalls("<tool_call><function=ping></function></tool_call>")

	if len(calls) != 1 {
		t.Fatalf("该有 1 条：%+v", calls)
	}
	if calls[0].Arguments != "{}" {
		t.Errorf("参数 = %q，期望 {}", calls[0].Arguments)
	}
}

// TestParseTextToolCalls_裸字符串加引号数字原样 判据是 json.Valid：`z_1` 不是 JSON，
// 得加引号；`3` 是 JSON 数字，原样嵌入（包成字符串会让整数参数类型不对）。
func TestParseTextToolCalls_裸字符串加引号数字原样(t *testing.T) {
	content := "<tool_call><function=f>" +
		"<parameter=table>z_3427e1</parameter>" +
		"<parameter=limit>3</parameter>" +
		"</function></tool_call>"

	calls, _ := ParseTextToolCalls(content)

	if len(calls) != 1 {
		t.Fatalf("该有 1 条：%+v", calls)
	}
	if calls[0].Arguments != `{"table":"z_3427e1","limit":3}` {
		t.Errorf("参数 = %q", calls[0].Arguments)
	}
}

// TestParseTextToolCalls_对象值原样嵌入 多行 JSON 对象不该被包成字符串。
func TestParseTextToolCalls_对象值原样嵌入(t *testing.T) {
	content := "<tool_call><function=f>\n" +
		"<parameter=filter>\n" +
		"{\"q\":\"学费\"}\n" +
		"</parameter>\n" +
		"</function></tool_call>"

	calls, _ := ParseTextToolCalls(content)

	if len(calls) != 1 {
		t.Fatalf("该有 1 条：%+v", calls)
	}
	if calls[0].Arguments != `{"filter":{"q":"学费"}}` {
		t.Errorf("参数 = %q", calls[0].Arguments)
	}
}

// TestParseTextToolCalls_缺闭合标签也能收 模型写到一半被截断时开标记还在、
// 闭合不在；只看开标记就能把这一条捞回来。
func TestParseTextToolCalls_缺闭合标签也能收(t *testing.T) {
	content := "先查一下\n<tool_call>\n<function=f>\n<parameter=k>v</parameter>\n</function>"

	calls, remaining := ParseTextToolCalls(content)

	if len(calls) != 1 {
		t.Fatalf("该有 1 条：%+v", calls)
	}
	if calls[0].Arguments != `{"k":"v"}` {
		t.Errorf("参数 = %q", calls[0].Arguments)
	}
	if remaining != "先查一下" {
		t.Errorf("剩余正文 = %q", remaining)
	}
}
