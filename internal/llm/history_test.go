package llm

import (
	"testing"
	"unicode/utf8"
)

func TestClamp_按rune截不切半个字(t *testing.T) {
	if got := Clamp("房产证在哪", 100); got != "房产证在哪" {
		t.Errorf("没超长时不该动：%q", got)
	}
	// 5 个中文字，截到 3 个 → 不能出现半个 UTF-8 序列
	got := Clamp("房产证在哪", 3)
	if got != "房产证…" {
		t.Errorf("Clamp = %q，期望 %q", got, "房产证…")
	}
	for _, r := range got {
		// **用 utf8.RuneError，不写那个字面量**：它的字形与编码事故留下的替换字符
		// 一模一样，下一次扫仓库时会被当成损坏的注释（这一段就是这么被误伤的）
		if r == utf8.RuneError {
			t.Errorf("截出了半个字符：%q", got)
		}
	}
}

func TestEstimateTokens(t *testing.T) {
	// bytes/3 向上取整：中文一字 3 字节 → 1 token
	if got := EstimateTokens("房产证"); got != 3 {
		t.Errorf("EstimateTokens(\"房产证\") = %d，期望 3", got)
	}
	if got := EstimateTokens(""); got != 0 {
		t.Errorf("空串应为 0：%d", got)
	}
}

func TestCompressHistory_预算内原样返回(t *testing.T) {
	messages := []ChatMessage{
		{Role: RoleUser, Content: "问题一"},
		{Role: RoleAssistant, Content: "答案一"},
	}
	got := CompressHistory(messages, 1000)
	if got.Dropped != 0 || len(got.Messages) != 2 {
		t.Errorf("预算充足时不该丢：%+v", got)
	}
	if got.Messages[0].Content != "问题一" {
		t.Errorf("内容被改写了：%q", got.Messages[0].Content)
	}
}

func TestCompressHistory_零预算是不设上限(t *testing.T) {
	messages := []ChatMessage{
		{Role: RoleUser, Content: "很长的问题……"},
		{Role: RoleAssistant, Content: "很长的答案……"},
	}
	got := CompressHistory(messages, 0)
	if got.Dropped != 0 || len(got.Messages) != 2 {
		t.Errorf("budget=0 意为不压缩：%+v", got)
	}
}

// TestCompressHistory_按组丢不拆散toolCalls provider 明确要求 role=tool 必须紧跟在
// 带 tool_calls 的 assistant 之后。按条丢会在队首留下孤儿 tool 消息，provider 直接报错。
func TestCompressHistory_按组丢不拆散toolCalls(t *testing.T) {
	messages := []ChatMessage{
		{Role: RoleUser, Content: "第一轮问题，这段很长很长很长很长很长"},
		{Role: RoleAssistant, Content: "第一轮回答", ToolCalls: []ToolCall{
			{ID: "c1", Name: "search", Arguments: `{"q":"x"}`},
		}},
		{Role: RoleTool, ToolCallID: "c1", Content: "第一条结果"},
		{Role: RoleTool, ToolCallID: "c1", Content: "第二条结果"},
		{Role: RoleUser, Content: "第二轮问题"},
		{Role: RoleAssistant, Content: "第二轮回答"},
	}

	// 预算只够最后两条
	got := CompressHistory(messages, EstimateTokens("第二轮问题")+EstimateTokens("第二轮回答"))

	if got.Dropped != 4 {
		t.Errorf("Dropped = %d，期望 4（第一轮的 user + assistant + 两条 tool 整组丢）", got.Dropped)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("剩下 %d 条，期望 2", len(got.Messages))
	}
	if got.Messages[0].Role != RoleUser || got.Messages[0].Content != "第二轮问题" {
		t.Errorf("留下的第一条不对：%+v", got.Messages[0])
	}
	// 留下的必须逐字不动
	if got.Messages[1].Content != "第二轮回答" {
		t.Errorf("留下的内容被改写：%q", got.Messages[1].Content)
	}
}

func TestCompressHistory_留下的逐字不动(t *testing.T) {
	original := []ChatMessage{
		{Role: RoleUser, Content: "很长的老问题，这段文字非常长，长到一定会超预算"},
		{Role: RoleAssistant, Content: "很长的老答案，这段文字也非常长，长到一定会超预算"},
		{Role: RoleUser, Content: "新问题"},
	}
	got := CompressHistory(original, 10)

	// **不改写输入**：压缩只能从队首整组丢，改写任何一条都会让 provider 的前缀缓存
	// 从那条起全部失效
	if original[1].Content != "很长的老答案，这段文字也非常长，长到一定会超预算" {
		t.Error("压缩改写了输入切片——那会让调用方的历史被动过")
	}
	for i := range got.Messages {
		if got.Messages[i].Role != original[i+2].Role || got.Messages[i].Content != original[i+2].Content {
			t.Errorf("第 %d 条与原文不一致", i)
		}
	}
}

func TestCompressHistory_全丢光也不报错(t *testing.T) {
	messages := []ChatMessage{
		{Role: RoleUser, Content: "非常长的一段文字，长到预算装不下"},
	}
	got := CompressHistory(messages, 1)
	if len(got.Messages) != 0 {
		t.Errorf("应能丢到空：%+v", got.Messages)
	}
	if got.Dropped != 1 {
		t.Errorf("Dropped = %d，期望 1", got.Dropped)
	}
}

// memStore 记住所做的 Load / Append。
type memStore struct {
	stored  []ChatMessage
	loads   int
	appends [][]ChatMessage
}

func (m *memStore) Load(sessionID, accountID string) []ChatMessage {
	m.loads++
	return m.stored
}

func (m *memStore) Append(sessionID, accountID string, messages []ChatMessage) {
	m.appends = append(m.appends, append([]ChatMessage(nil), messages...))
}

// TestLoadHistoryPrefix_读回文件里那份 这就是历史的**唯一**来源：会话文件。
//
// 这里曾经还有三条用例，验「调用方给一份数据库重建的历史」那条路（quote/time 模式、
// 文件空时播种）。`internal/store` 搬走之后没有任何生产调用方能给那份历史
// （`RunnerInput.History` 全仓无人赋值），于是那三条只在测试里活着——
// 不能到达的代码不是功能，随那条路一起删掉了。
func TestLoadHistoryPrefix_读回文件里那份(t *testing.T) {
	store := &memStore{stored: []ChatMessage{
		{Role: RoleUser, Content: "文件里的问题"},
		{Role: RoleAssistant, Content: "文件里的答案"},
	}}

	got := LoadHistoryPrefix(store, "s", "acct-1")
	if len(got) != 2 || got[0].Content != "文件里的问题" {
		t.Errorf("该原样读回会话文件里那份：%+v", got)
	}
}

// TestLoadHistoryPrefix_没存储就没有历史 SESSION_HISTORY=0 时这一轮从零开始，
// **不返错**（历史是锦上添花，不是这一轮回答的前提）。
func TestLoadHistoryPrefix_没存储就没有历史(t *testing.T) {
	if got := LoadHistoryPrefix(nil, "s", "acct-1"); got != nil {
		t.Errorf("没有存储时该是什么都没有：%+v", got)
	}
	if got := LoadHistoryPrefix(&memStore{}, "", "acct-1"); got != nil {
		t.Errorf("没有会话 id 时也该是什么都没有：%+v", got)
	}
}
