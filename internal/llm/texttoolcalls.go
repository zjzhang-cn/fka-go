// 这个文件把「被模型写成文本的工具调用」打捞成真正的 ToolCall。
//
// ## 它要兜的是什么
//
// 工具调用本该走 OpenAI 的 `tool_calls` 字段。但有些模型（或某些兼容端点）不往那个
// 字段里放，而是照着训练时的模板**把调用当成文字吐出来**。真机上见到过两种落点：
//
//   - 正文（`content`）——`我需要确认…` 后面直接跟 `<tool_call>…`；
//   - 推理（`reasoning_content`）——调用被算进「思考」里，正文反而是空的。
//
// 两份形状相同：
//
//	我需要确认 z_3427e1 表的结构，看看它有哪些列。
//	<tool_call>
//	<function=mcp__sqlite__describe_schema>
//	<parameter=table>
//	z_3427e1
//	</parameter>
//	</function>
//	</tool_call>
//
// `llm/openai` 只从结构化的 `delta.ToolCalls` 收集调用。正文那一份会被循环当成
// 「最终答案」原样发给用户（工具一次没跑）；推理那一份更糟——正文为空，provider 直接
// 报「既没有给出回答也没有调用工具」。两种都得在 provider 里翻译成真正的调用，
// **推理缓冲区只在 provider 手里**，所以打捞放在这一层（见 openai.go）。
//
// ## 认不出就一个字都不改
//
// 只有**确实打捞到调用**时才返回新正文与新调用；否则原样返回 `content` 与 nil，
// 与今天的行为逐字一致。这段兜底绝不能把普通答案吃掉——宁可漏兜底，不可误吞。
package llm

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 模型文本模板里的那几个标记。**只认这一种形状**——它来自真机观察，不是一张
// 「兼容所有模型」的清单。认不出的模板一律当普通正文。
const (
	textToolOpen   = "<tool_call>"
	textToolClose  = "</tool_call>"
	textFuncOpen   = "<function="
	textFuncClose  = "</function>"
	textParamOpen  = "<parameter="
	textParamClose = "</parameter>"
)

// ParseTextToolCalls 从一段模型文本（正文或推理）里打捞出被写成文本的工具调用。
//
// 返回打捞到的调用（顺序即出现顺序）与**去掉那些 `<tool_call>` 块之后的文本**。
// 一个都没打捞到时返回 `(nil, content)`——输入逐字不变。
func ParseTextToolCalls(content string) ([]ToolCall, string) {
	if !strings.Contains(content, textToolOpen) {
		return nil, content
	}

	blocks := splitToolCallBlocks(content)
	if len(blocks) == 0 {
		return nil, content
	}

	var calls []ToolCall
	var kept strings.Builder
	cursor := 0
	// id 只需在这一段历史里唯一，且要能区分同一轮里的多次调用与不同轮之间。
	// 纳秒 + 序号足够，不引任何依赖。
	stamp := time.Now().UnixNano()

	for _, block := range blocks {
		functions := parseFunctions(block.text)
		if len(functions) == 0 {
			// 认得 `<tool_call>` 但里面没有可用的 `<function=…>`：**不吞**这段文字，
			// 免得把本来要给用户看的内容删掉
			continue
		}
		kept.WriteString(content[cursor:block.start])
		cursor = block.end
		for _, function := range functions {
			calls = append(calls, ToolCall{
				ID:        fmt.Sprintf("call_text_%d_%d", stamp, len(calls)+1),
				Name:      function.name,
				Arguments: encodeArguments(function.params),
			})
		}
	}
	if len(calls) == 0 {
		return nil, content
	}
	kept.WriteString(content[cursor:])
	return calls, strings.TrimSpace(kept.String())
}

// toolCallBlock 一段 `<tool_call>…</tool_call>` 及其在正文里的位置。
// `end` 是 `</tool_call>` 之后一位，用来从正文里整段摘掉它。
type toolCallBlock struct {
	text       string
	start, end int
}

// splitToolCallBlocks 扫出正文里的所有 `<tool_call>` 块，按出现顺序返回。
func splitToolCallBlocks(content string) []toolCallBlock {
	var blocks []toolCallBlock

	pos := 0
	for {
		start := strings.Index(content[pos:], textToolOpen)
		if start < 0 {
			break
		}
		start += pos
		bodyStart := start + len(textToolOpen)

		closeIdx := strings.Index(content[bodyStart:], textToolClose)
		if closeIdx < 0 {
			// 缺 `</tool_call>`（模型被截断、或干脆没写）：把剩下的都当这一块。
			// 有显式开标记才这么干——纯正文里不会凭空冒出它
			blocks = append(blocks, toolCallBlock{text: content[bodyStart:], start: start, end: len(content)})
			break
		}
		closeIdx += bodyStart
		blocks = append(blocks, toolCallBlock{
			text:  content[bodyStart:closeIdx],
			start: start,
			end:   closeIdx + len(textToolClose),
		})
		pos = closeIdx + len(textToolClose)
	}

	return blocks
}

// parsedFunction 一个 `<function=…>`；parsedParameter 一个 `<parameter=…>…</parameter>`。
type parsedFunction struct {
	name   string
	params []parsedParameter
}

type parsedParameter struct {
	key   string
	value string
}

// parseFunctions 从一个 `<tool_call>` 块里扫出所有 `<function=…>`。
// 一个块里放多个函数不是我们观察到的形状，但多扫几行代价极低，模型不老实时不至于漏。
func parseFunctions(block string) []parsedFunction {
	var out []parsedFunction

	rest := block
	for {
		openIdx := strings.Index(rest, textFuncOpen)
		if openIdx < 0 {
			break
		}
		after := rest[openIdx+len(textFuncOpen):]
		gt := strings.IndexByte(after, '>')
		if gt < 0 {
			break
		}
		name := strings.TrimSpace(after[:gt])
		body := after[gt+1:]

		// 函数体到下一个 `<function=` 或 `</function>` 为止；两个都没有就到块尾。
		// `</function>` 在真机上**可能没写**，所以它是「有就收、没有就算」的边
		end := len(body)
		if nextFn := strings.Index(body, textFuncOpen); nextFn >= 0 && nextFn < end {
			end = nextFn
		}
		if closeFn := strings.Index(body, textFuncClose); closeFn >= 0 && closeFn < end {
			end = closeFn
		}

		if name != "" {
			out = append(out, parsedFunction{name: name, params: parseParameters(body[:end])})
		}

		// 从函数体末尾接着扫。碰上 `</function>` 就跳过它，免得把它的字面当成
		// 下一个函数的正文
		rest = body[end:]
		if strings.HasPrefix(rest, textFuncClose) {
			rest = rest[len(textFuncClose):]
		}
	}

	return out
}

// parseParameters 从一个函数体里扫出所有 `<parameter=key>value</parameter>`。
// value 里的换行、缩进原样保留——多行 SQL、缩进过的 JSON 都靠它。
func parseParameters(body string) []parsedParameter {
	var out []parsedParameter

	rest := body
	for {
		openIdx := strings.Index(rest, textParamOpen)
		if openIdx < 0 {
			break
		}
		after := rest[openIdx+len(textParamOpen):]
		gt := strings.IndexByte(after, '>')
		if gt < 0 {
			break
		}
		key := strings.TrimSpace(after[:gt])
		value := after[gt+1:]

		closeIdx := strings.Index(value, textParamClose)
		if closeIdx < 0 {
			// 缺 `</parameter>`：这个参数取不到边界，连同后面的内容一起放弃。
			// 不猜一个截止位置——猜错会把下一个参数的值并进来
			break
		}
		if key != "" {
			out = append(out, parsedParameter{key: key, value: value[:closeIdx]})
		}
		rest = value[closeIdx+len(textParamClose):]
	}

	return out
}

// encodeArguments 把参数编成工具调用要的那份原始 JSON 字符串。
//
// **保序、去重（后出现的覆盖先出现的）**。不用 map：map 的键序会被 encoding/json
// 排序，而稳定的字节序更好读、也让同样的模型输出生成同样的历史。
func encodeArguments(params []parsedParameter) string {
	if len(params) == 0 {
		return "{}"
	}

	type slot struct{ key, value string }
	slots := make([]slot, 0, len(params))
	at := map[string]int{}
	for _, param := range params {
		if index, ok := at[param.key]; ok {
			slots[index].value = param.value
			continue
		}
		at[param.key] = len(slots)
		slots = append(slots, slot{param.key, param.value})
	}

	var builder strings.Builder
	builder.WriteByte('{')
	for index, item := range slots {
		if index > 0 {
			builder.WriteByte(',')
		}
		key, _ := json.Marshal(item.key)
		builder.Write(key)
		builder.WriteByte(':')
		builder.WriteString(encodeValue(item.value))
	}
	builder.WriteByte('}')
	return builder.String()
}

// encodeValue 把一个参数值编成 JSON 片段。
//
// 值多半是**裸字符串**（`z_3427e1`），得加引号。但模型也可能原样贴一段 JSON
// （对象 / 数组 / 数字 / `true`），那就原样嵌入——包上引号会把一个对象变成一个
// 字符串，工具收到就是参数类型不对。判据只有一条：`json.Valid`。
func encodeValue(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		return trimmed
	}
	encoded, err := json.Marshal(trimmed)
	if err != nil {
		// 正常输入到不了这里（非法 UTF-8 会被 Marshal 换成 U+FFFD 而不返错）。
		// 真出了意外就退化成空串，不 panic——保住这一轮比这一条参数重要
		return `""`
	}
	return string(encoded)
}
