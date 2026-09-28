package llm

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxPassageChars 单条片段送进 prompt 的字符上限。片段本来就是短摘录，超了只会白烧 token。
const MaxPassageChars = 400

// BuildPrompt 片段 → prompt 正文。纯函数，便于钉住「哪些东西进了 prompt」。
func BuildPrompt(question string, passages []Passage, quotedText string) string {
	blocks := make([]string, 0, len(passages))
	for i, passage := range passages {
		text := strings.TrimSpace(passage.Text)
		body := text
		if len(text) == 0 {
			body = "（正文里没有对应句子，只是文件名匹配）"
		} else {
			body = Clamp(text, MaxPassageChars)
		}
		blocks = append(blocks, fmt.Sprintf("[资料 %d] 文件名：%s\n内容：%s", i+1, passage.Filename, body))
	}

	lines := []string{"基于以下资料片段回答问题。", ""}
	lines = append(lines, blocks...)

	// 引用的正文有时在历史里找不到（引用的是很久以前、或没存过的那条），
	// 那种情况下必须内联，否则模型不知道「这个」指什么。已在历史里时调用方不传
	if trimmed := strings.TrimSpace(quotedText); trimmed != "" {
		lines = append(lines, "用户引用了这条消息："+Clamp(trimmed, MaxPassageChars), "")
	}

	lines = append(lines, "问题："+question)
	return strings.Join(lines, "\n")
}

// Clamp 截断到 max 字符，超出补省略号。BuildPrompt 与 openai 的错误体摘要共用。
//
// **按 rune 截，不按字节**——按字节会把一个中文字切成半个 UTF-8 序列，产出非法字符串。
func Clamp(text string, max int) string {
	if utf8.RuneCountInString(text) <= max {
		return text
	}
	count := 0
	for index := range text {
		if count == max {
			return text[:index] + "…"
		}
		count++
	}
	return text
}

// EstimateTokens 估算一段文本的 token 数。bytes/3——中文一个字约一个 token、
// 英文三个字符约一个，够用且不引 tokenizer 依赖。
func EstimateTokens(text string) int {
	return (len([]byte(text)) + 2) / 3
}

// CompressHistory 收拢历史到预算内：**从最老整「组」丢掉，绝不改写留下的任何一条**。
//
// ## 为什么不截断中间的文字
//
// provider 的前缀缓存（KV cache）认的是「从第一条起逐字不变的前缀」。一旦把某条
// assistant 的文字截短，从那条开始往后的前缀全部变样——它之后的历史再长，也一次都
// 命中不了。截得越狠，失效点越靠前。
//
// 只从最老整组丢则不同：丢掉的那一轮会失效，但**留下的部分逐字不动**，此后只要
// 不再丢，下一轮追加的消息就完全复用这一轮的整段前缀。所以丢弃只允许发生在队首。
// 最近的一轮永远保留。
//
// ## 为什么按组丢，而不是按条
//
// 一条 assistant 的 ToolCalls 与紧随其后的 RoleTool 结果是一体的。按条丢会在队首
// 留下孤儿 tool 消息（或留下一条没有结果的 tool_calls），provider 直接报
// `Messages with role 'tool' must be a response to a preceding message with 'tool_calls'`。
// 按组丢保证两者同进同出。
//
// budgetTokens <= 0 表示**不设上限**（LLM_CONTEXT_TOKENS=0 的路径），原样返回。
func CompressHistory(messages []ChatMessage, budgetTokens int) CompressionResult {
	working := cloneMessages(messages)

	total := totalTokens(working)
	if len(working) == 0 || budgetTokens <= 0 {
		return CompressionResult{Messages: working, EstimatedTokens: total}
	}
	if total <= budgetTokens {
		return CompressionResult{Messages: working, EstimatedTokens: total}
	}

	// 超预算：从最老整组丢，丢到装得下或丢空为止。留下的每一条都逐字不动
	groups := groupMessages(working)
	remaining := total
	dropped := 0
	index := 0

	for index < len(groups) && remaining > budgetTokens {
		for _, message := range groups[index] {
			remaining -= EstimateTokens(message.Content)
		}
		dropped += len(groups[index])
		index++
	}

	kept := make([]ChatMessage, 0, len(working))
	for _, group := range groups[index:] {
		kept = append(kept, group...)
	}

	return CompressionResult{Messages: kept, Dropped: dropped, EstimatedTokens: totalTokens(kept)}
}

// groupMessages 把消息切成不可拆的组：带 ToolCalls 的 assistant 连同其后连续的
// tool 结果算一组，其余每条自成一组。只用于「从队首整组丢」，不改变消息内容。
func groupMessages(messages []ChatMessage) [][]ChatMessage {
	groups := make([][]ChatMessage, 0, len(messages))

	for i := 0; i < len(messages); i++ {
		message := messages[i]

		if message.Role == RoleAssistant && len(message.ToolCalls) > 0 {
			group := []ChatMessage{message}
			j := i + 1
			for j < len(messages) && messages[j].Role == RoleTool {
				group = append(group, messages[j])
				j++
			}
			groups = append(groups, group)
			i = j - 1
			continue
		}

		groups = append(groups, []ChatMessage{message})
	}

	return groups
}

func totalTokens(messages []ChatMessage) int {
	sum := 0
	for _, message := range messages {
		sum += EstimateTokens(message.Content)
	}
	return sum
}

// cloneMessages 复制一份。**刻意深拷贝 ToolCalls 切片**——压缩时不能因为共享
// 底层数组而意外改到调用方的那份（那会破坏「留下的逐字不动」）。
func cloneMessages(messages []ChatMessage) []ChatMessage {
	out := make([]ChatMessage, len(messages))
	for i, message := range messages {
		out[i] = message
		if len(message.ToolCalls) > 0 {
			out[i].ToolCalls = append([]ToolCall(nil), message.ToolCalls...)
		}
	}
	return out
}

// HistoryKey 选历史前缀的定位键。
type HistoryKey struct {
	SessionID string
	AccountID string
	History   *History
}

// LoadHistoryPrefix 选出送进模型的**历史前缀**：优先原样读回会话文件，其次才用数据库
// 重建的历史。
//
// 规则（两条路共用，所以只写这一份）：
//   - quote（或无历史）：会话文件里有东西就用它——逐字原样，KV 缓存的前提；
//   - time / all：这两种模式刻意跨会话取历史，数据库那份才是权威，文件只作写入。
//
// 文件空而数据库有历史时**顺手把数据库那份播种进文件**：否则这一轮读了旧历史，
// 下一轮文件只从本回合开始，前面几轮就凭空丢了。播种后文件即成为后续的真相。
func LoadHistoryPrefix(store SessionHistoryStore, key HistoryKey) []ChatMessage {
	fromDB := make([]ChatMessage, 0)
	mode := ""
	if key.History != nil {
		mode = key.History.Mode
		for _, message := range key.History.Messages {
			fromDB = append(fromDB, ChatMessage{Role: message.Role, Content: message.Text})
		}
	}

	if store == nil || key.SessionID == "" || (mode != "" && mode != "quote") {
		return fromDB
	}

	persisted := store.Load(key.SessionID, key.AccountID)
	if len(persisted) > 0 {
		return persisted
	}

	if len(fromDB) > 0 {
		store.Append(key.SessionID, key.AccountID, fromDB)
	}
	return fromDB
}
