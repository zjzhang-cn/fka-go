package llm

import "testing"

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
		if r == '�' {
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

func TestLoadHistoryPrefix_quote模式优先用文件里那份(t *testing.T) {
	store := &memStore{stored: []ChatMessage{
		{Role: RoleUser, Content: "文件里的问题"},
		{Role: RoleAssistant, Content: "文件里的答案"},
	}}
	dbHistory := &History{
		Mode: "quote",
		Messages: []HistoryMessage{
			{Role: RoleUser, Text: "数据库里的问题"},
		},
	}

	got := LoadHistoryPrefix(store, HistoryKey{SessionID: "s", History: dbHistory})
	if len(got) != 2 || got[0].Content != "文件里的问题" {
		t.Errorf("quote 模式应优先用会话文件里逐字那份：%+v", got)
	}
	if len(store.appends) != 0 {
		t.Error("文件里有内容时不该播种")
	}
}

// TestLoadHistoryPrefix_文件空时播种数据库那份 不播种的话，这一轮读了旧历史，
// 下一轮文件里就只有本回合，前面几轮凭空丢了。
func TestLoadHistoryPrefix_文件空时播种数据库那份(t *testing.T) {
	store := &memStore{}
	dbHistory := &History{
		Mode: "quote",
		Messages: []HistoryMessage{
			{Role: RoleUser, Text: "数据库里的问题"},
			{Role: RoleAssistant, Text: "数据库里的答案"},
		},
	}

	got := LoadHistoryPrefix(store, HistoryKey{SessionID: "s", History: dbHistory})
	if len(got) != 2 {
		t.Fatalf("应退回数据库那份：%+v", got)
	}
	if len(store.appends) != 1 || len(store.appends[0]) != 2 {
		t.Errorf("应顺手播种进文件：%+v", store.appends)
	}
}

func TestLoadHistoryPrefix_time模式以数据库为准(t *testing.T) {
	store := &memStore{stored: []ChatMessage{{Role: RoleUser, Content: "文件里的"}}}
	dbHistory := &History{
		Mode: "time",
		Messages: []HistoryMessage{
			{Role: RoleUser, Text: "数据库里的"},
		},
	}

	got := LoadHistoryPrefix(store, HistoryKey{SessionID: "s", History: dbHistory})
	if len(got) != 1 || got[0].Content != "数据库里的" {
		t.Errorf("time 模式跨会话，数据库那份才是权威：%+v", got)
	}
}

func TestLoadHistoryPrefix_没有存储时直接用数据库(t *testing.T) {
	dbHistory := &History{Messages: []HistoryMessage{{Role: RoleUser, Text: "问题"}}}
	got := LoadHistoryPrefix(nil, HistoryKey{SessionID: "s", History: dbHistory})
	if len(got) != 1 || got[0].Content != "问题" {
		t.Errorf("%+v", got)
	}
}

func TestBuildPrompt_文件名匹配时说明正文为空(t *testing.T) {
	got := BuildPrompt("在哪", []Passage{{Filename: "房产证.pdf", Text: ""}}, "")
	if got == "" {
		t.Fatal("空 prompt")
	}
	// 「只是文件名匹配」这件事必须说给模型听，否则它会以为正文里真有内容
	if !contains(got, "只是文件名匹配") {
		t.Errorf("应说明正文里没有对应句子：\n%s", got)
	}
	if !contains(got, "房产证.pdf") {
		t.Errorf("文件名应进 prompt：\n%s", got)
	}
}

func TestBuildPrompt_引用的正文内联(t *testing.T) {
	got := BuildPrompt("在哪", nil, "我昨天说的是户口本")
	if !contains(got, "我昨天说的是户口本") {
		t.Errorf("引用的正文必须内联：\n%s", got)
	}
	// 没传引用时不加那一段
	plain := BuildPrompt("在哪", nil, "")
	if contains(plain, "用户引用了这条消息") {
		t.Errorf("没传引用就不该有那一段：\n%s", plain)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
