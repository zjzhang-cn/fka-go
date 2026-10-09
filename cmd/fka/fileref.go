// 本文件给 `fka ask` 与 `fka chat` 加一件事：**问题里用 `@路径` 引用本地文件**。
//
// ## 为什么放在 CLI 而不是 agent
//
// 引用是**输入层**的事：谁问、问了什么由调用方组装。展开后就是一段普通的问题文本，
// agent 那一层不需要知道「这段字是从文件来的」——它的契约一个字都不用改。
// 因此微信那条路（messages）也不受影响：那是渠道的事，不是 CLI 的。
//
// ## 语法
//
//	@README.md              普通路径，到空白为止
//	@"我的 文档.txt"         路径含空格或中文时用引号
//
// 只认**词首**的 `@`：`foo@bar` 里的 `@` 前面是字母，不当引用（否则邮箱会被吃掉）。
// 认得出但读不到的（不存在 / 目录 / 二进制）**不报错、不中断**——原样当普通文字，
// 顶多在 stderr 上提示一句。一次问答不该因为一个笔误的引用就发不出去。
//
// ## 为什么是「附在问题后面」而不是原地替换
//
// 原地把 `@README.md` 换成几千字正文，会让人再看问题时认不出自己问的是什么。
// 所以问题原文保留，文件正文统一追加在末尾一个小节里。
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// fileRefLimit 单个被引用文件最多读多少字节。**有上限是必须的**：
// 一次问答的上下文预算是有限的，而引用是用户随手打的，没有上限就等于让
// 一个 `@大日志` 把整轮问答的预算吃光（失败形态是模型答得莫名其妙）。
const fileRefLimit = 32 * 1024

// fileRef 一个成功读到的引用。
type fileRef struct {
	name string
	body string
}

// expandFileRefs 把问题里形如 `@路径` 的引用展开成追加在末尾的文件正文。
//
// 返回展开后的问题与被引用到的文件名（按首次出现顺序、去重），供调用方提示。
// warn 收「认得出但读不到」的提示（可为 nil）。
func expandFileRefs(question string, warn io.Writer) (string, []string) {
	var refs []fileRef
	seen := map[string]bool{}

	var out strings.Builder
	for i := 0; i < len(question); {
		if question[i] == '@' && atFileRefBoundary(question, i) {
			if name, end, ok := scanFileRef(question, i); ok {
				out.WriteString(question[i:end])
				i = end
				if !seen[name] {
					if body, err := readFileRef(name); err != nil {
						if warn != nil {
							fmt.Fprintf(warn, "引用 @%s：%s（按普通文字处理）\n", name, err.Error())
						}
					} else {
						seen[name] = true
						refs = append(refs, fileRef{name: name, body: body})
					}
				}
				continue
			}
		}
		out.WriteByte(question[i])
		i++
	}

	if len(refs) == 0 {
		return question, nil
	}

	var section strings.Builder
	section.WriteString("\n\n（以下是用户引用的本地文件内容，已附在下面，不必再用工具读取）")
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		fmt.Fprintf(&section, "\n\n──── %s ────\n%s", ref.name, ref.body)
		names = append(names, ref.name)
	}
	return out.String() + section.String(), names
}

// atFileRefBoundary `at` 处的 `@` 是不是词首。**按 ASCII 文件名用字判前一个字符**：
// `foo@bar` 的 `o` 是字母 → 不是词首（邮箱不当引用）；`看@文件` 的 `看` 不是
// ASCII 文件名用字 → 是词首（中文连着写也能认）。
func atFileRefBoundary(s string, at int) bool {
	if at == 0 {
		return true
	}
	prev, _ := utf8.DecodeLastRuneInString(s[:at])
	return !asciiRefWordRune(prev)
}

// asciiRefWordRune 只用于「词首」判断的窄集：连续的 ASCII 文件名用字。
func asciiRefWordRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '_', r == '-', r == '.', r == '/', r == '~', r == '\\':
		return true
	}
	return false
}

// fileRefPathRune 路径扫描用字。**到空白或句读标点为止**——中文路径（`@文档.txt`）
// 也要能读，所以不限于 ASCII；但 `，。！？` 这些句子标点要停下，否则
// `@README.md，谢谢` 会把「谢谢」也吞进路径。
func fileRefPathRune(r rune) bool {
	if unicode.IsSpace(r) {
		return false
	}
	switch r {
	case '，', '。', '！', '？', '、', '；', '：', '·',
		'“', '”', '‘', '’', '「', '」', '『', '』', '【', '】', '《', '》', '（', '）',
		',', '!', '?', ';', ':', '"', '\'', '`', '(', ')', '[', ']', '{', '}', '<', '>':
		return false
	}
	return true
}

// scanFileRef 从 `at`（`@` 处）读出一个引用，返回路径、消费到的结束位置与是否成立。
func scanFileRef(s string, at int) (string, int, bool) {
	i := at + 1
	if i < len(s) && s[i] == '"' {
		offset := strings.IndexByte(s[i+1:], '"')
		if offset < 0 {
			return "", 0, false
		}
		name := s[i+1 : i+1+offset]
		if strings.TrimSpace(name) == "" {
			return "", 0, false
		}
		return name, i + 1 + offset + 1, true
	}

	start := i
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !fileRefPathRune(r) {
			break
		}
		i += size
	}
	if i == start {
		return "", 0, false
	}
	return s[start:i], i, true
}

// readFileRef 读一个被引用的文件。**读不到不算错**——调用方转成一句提示，
// 原样把 token 留给模型。
func readFileRef(name string) (string, error) {
	info, err := os.Stat(name)
	if err != nil {
		return "", errors.New("读不到这个文件")
	}
	if info.IsDir() {
		return "", errors.New("是目录")
	}

	file, err := os.Open(name)
	if err != nil {
		return "", errors.New("打不开这个文件")
	}
	defer file.Close()

	// 多读一个字节：读满上限 +1 才说明后面还有，据此决定要不要标「已截断」
	data, err := io.ReadAll(io.LimitReader(file, fileRefLimit+1))
	if err != nil {
		return "", errors.New("读取失败")
	}

	truncated := false
	if len(data) > fileRefLimit {
		data = data[:fileRefLimit]
		truncated = true
	}
	// 二进制塞进文本提示词只会浪费预算并可能弄坏编码，直接不当引用
	if bytes.IndexByte(data, 0) >= 0 {
		return "", errors.New("看起来是二进制文件")
	}

	text := string(data)
	if truncated {
		text += fmt.Sprintf("\n…（文件超过 %d 字节，已截断）", fileRefLimit)
	}
	return text, nil
}
