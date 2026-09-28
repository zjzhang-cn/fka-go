// Package searchterms 是关键词检索的**纯文本工具**：拆词与 LIKE 通配符转义。
//
// 放在这里而不是 messages/search：这两件事与「文档怎么搜」无关，记忆的关键词路径
// 也要用同一套规则。留在 messages 会让存储实现反向依赖消息层。
package searchterms

import (
	"regexp"
	"strings"
)

var whitespace = regexp.MustCompile(`\s+`)

// SplitTerms 按空白拆词。
//
// **不引入分词器**：中文没有空格，真要分词得挑一套词表；而家庭文档量下，
// 「多写几个字」比「分词器把『选课通知』切错」更可预期。按空白拆是最小惊讶的选择。
func SplitTerms(query string) []string {
	parts := whitespace.Split(query, -1)
	out := make([]string, 0, len(parts))
	for _, t := range parts {
		t = strings.TrimSpace(t)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// EscapeLike 转义 LIKE 的通配符。
//
// **不做这一步，用户输入的 `%` 就会变成「匹配任意内容」**——搜 `100%` 会把整库都搜出来，
// 而用户以为自己在搜一个百分号。`_` 同理（匹配任意单字符）。
//
// 反斜杠自身要先转义，否则 `\%` 会被理解成「转义后的 %」而不是「反斜杠 + 通配符」。
// 配套的 `ESCAPE '\` 写在每条 LIKE 后面。
func EscapeLike(term string) string {
	var b strings.Builder
	for _, r := range term {
		switch r {
		case '\\', '%', '_':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
