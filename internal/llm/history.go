package llm

import "unicode/utf8"

// Clamp 截断到 max 字符，超出补省略号。**工具循环报非法 JSON 与 openai 摘错误体共用。**
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

// EstimateMessageTokens 一条消息的完整口径：正文 + 工具调用的名字与原始参数。
//
// **压缩与预算共用它**：丢组的「总数」与「扣减」若口径不同，就会把装不下的组
// 当成装得下。而 ToolCalls.Arguments 是**逐字重放的原始 JSON**——它也占上下文。
func EstimateMessageTokens(m ChatMessage) int {
	sum := EstimateTokens(m.Content)
	for _, call := range m.ToolCalls {
		sum += EstimateTokens(call.Name) + EstimateTokens(call.Arguments)
	}
	return sum
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
		return CompressionResult{Messages: working}
	}
	if total <= budgetTokens {
		return CompressionResult{Messages: working}
	}

	// 超预算：从最老整组丢，丢到装得下或丢空为止。留下的每一条都逐字不动
	groups := groupMessages(working)
	remaining := total
	dropped := 0
	index := 0

	for index < len(groups) && remaining > budgetTokens {
		for _, message := range groups[index] {
			remaining -= EstimateMessageTokens(message)
		}
		dropped += len(groups[index])
		index++
	}

	kept := make([]ChatMessage, 0, len(working))
	for _, group := range groups[index:] {
		kept = append(kept, group...)
	}

	return CompressionResult{Messages: kept, Dropped: dropped}
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
		sum += EstimateMessageTokens(message)
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

// LoadHistoryPrefix 读回送进模型的**历史前缀**：会话文件里那份，逐字原样。
//
// ## 为什么只剩这一条路
//
// 这里曾经有第二条路：调用方可以给一份「数据库重建的历史」（`History` 结构 +
// `Mode` / `GapMinutes` / `ChatMessages` 三个字段），文件空时把它播种进文件。
// 那是**文档那半边还在本仓库时**的事——`internal/store` 在 `1fae643` 搬走之后，
// 就没有任何生产调用方能给这份历史了（`RunnerInput.History` 全仓无人赋值），
// 于是那条分支只剩测试在跑，而它的存在还让注释说谎
// （「`ChatMessages` 有它时优先于 `Messages`」——代码从不读它）。
//
// 现在它就是「读文件」这一件事。**这不是功能删减**：不能到达的代码不是功能。
func LoadHistoryPrefix(store SessionHistoryStore, sessionID, accountID string) []ChatMessage {
	if store == nil || sessionID == "" {
		return nil
	}
	return store.Load(sessionID, accountID)
}
