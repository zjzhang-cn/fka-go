// 本文件给 `fka ask` 与 `fka chat` 加一件事：**问题里用 `@路径` 引用本地文件**。
//
// ## 为什么放在 CLI 而不是 agent
//
// 引用是**输入层**的事：谁问、问了什么由调用方组装。展开后就是一段普通的问题文本，
// 文本与图片都经 `RunnerInput` 交上去，agent 那一层不需要知道「这段字是从文件来的」。
// 因此微信那条路（messages）也不受影响：那是渠道的事，不是 CLI 的。
//
// ## 语法
//
//	@README.md              普通路径，到空白或句读为止
//	@"我的 文档.txt"         路径含空格或中文时用引号
//
// 路径按**进程的当前目录**解析（不是安装根）。无引号、且路径后紧跟中文正文时
// （`@图.png里面有什么`），取「确实存在的**最长路径前缀**」断开——文件系统就是那个
// 分隔符；没有存在的前缀就原样当文字并提示一句。
//
// 只认**词首**的 `@`：`foo@bar` 里的 `@` 前面是 ASCII 字母，不当引用（否则邮箱会被
// 吃掉）。认得出但读不到的（不存在 / 目录 / 过大的图片）**不报错、不中断**——原样当
// 普通文字，顶多在 stderr 上提示一句。一次问答不该因为一个笔误的引用就发不出去。
//
// ## 三类文件，三种去处
//
//   - 文本：正文追加在问题末尾的小节里（原地替换会让人再看时认不出自己问的是什么）；
//   - 图片：**作为本条消息的附件发给模型**（base64 data URI，见 llm.ImageAttachment），
//     只在这一轮发送、不落进会话历史（图片字节不该每轮重放）；
//   - 其它二进制：不发送内容，只附上「路径 + 类型 + 大小」，交给 MCP 工具去读——
//     这个 agent 自己不带读取能力。
package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zjzhang-cn/fka-go/internal/llm"
)

// fileRefLimit 单个**文本**引用最多读多少字节。有上限是必须的：引用是用户随手打的，
// 没有上限就等于让一个 `@大日志` 把整轮问答的上下文预算吃光。
const fileRefLimit = 32 * 1024

// fileRefImageLimit 单张**图片**最多读多少字节。比文本宽松，因为图片本来就不小；
// 超过这个尺寸不再截断（半张图没有意义），直接按「读不到」处理并提示。
const fileRefImageLimit = 5 << 20

// fileRefKind 一个被引用文件的类别。
type fileRefKind int

const (
	refText fileRefKind = iota
	refImage
	refBinary
)

// loadedRef 读完一个引用后的结果。
type loadedRef struct {
	name  string
	kind  fileRefKind
	mime  string
	size  int64
	text  string              // kind == refText 时的正文
	image llm.ImageAttachment // kind == refImage 时的附件
}

// expandedQuestion 展开后的输入。text 是交给模型的问题；names 是引用到的文件名
// （提示用）；images 是本轮要随消息发送的图片。
type expandedQuestion struct {
	text   string
	names  []string
	images []llm.ImageAttachment
}

// expandFileRefs 把问题里形如 `@路径` 的引用展开。warn 收「认得出但读不到」的提示
// （可为 nil）。
func expandFileRefs(question string, warn io.Writer) expandedQuestion {
	var refs []loadedRef
	seen := map[string]bool{}

	var out strings.Builder
	for i := 0; i < len(question); {
		if question[i] == '@' && atFileRefBoundary(question, i) {
			if name, end, quoted, ok := scanFileRef(question, i); ok {
				if !quoted {
					// 无引号时，路径可能把紧跟的中文正文一起吞进来（`@图.png里面有什么`
					// ——中文文件名要支持，所以扫描器不停在汉字上）。用**确实存在的
					// 最长路径前缀**把它断开：文件系统就是那个分隔符。
					name, end = trimToExisting(question, i, end)
				}
				out.WriteString(question[i:end])
				i = end
				if !seen[name] {
					if ref, err := loadRef(name); err != nil {
						if warn != nil {
							fmt.Fprintf(warn, "引用 @%s：%s（按普通文字处理）\n", name, err.Error())
						}
					} else {
						seen[name] = true
						refs = append(refs, ref)
					}
				}
				continue
			}
		}
		out.WriteByte(question[i])
		i++
	}

	result := expandedQuestion{text: question}
	if len(refs) == 0 {
		return result
	}

	var section strings.Builder
	section.WriteString("\n\n（用户引用了这些本地文件）")
	for _, ref := range refs {
		result.names = append(result.names, ref.name)
		switch ref.kind {
		case refText:
			fmt.Fprintf(&section, "\n\n──── %s ────\n%s", ref.name, ref.text)
		case refImage:
			result.images = append(result.images, ref.image)
			fmt.Fprintf(&section, "\n\n──── %s（%s，%s）────\n（图片已作为本条消息的附件发送，请直接查看）",
				ref.name, ref.mime, humanBytes(ref.size))
		case refBinary:
			fmt.Fprintf(&section, "\n\n──── %s（%s，%s）────\n（二进制文件，内容未附；如需处理请用工具读取）",
				ref.name, ref.mime, humanBytes(ref.size))
		}
	}
	result.text = out.String() + section.String()
	return result
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

// scanFileRef 从 `at`（`@` 处）读出一个引用，返回路径、消费到的结束位置、
// **是否带引号**与是否成立。
func scanFileRef(s string, at int) (name string, end int, quoted bool, ok bool) {
	i := at + 1
	if i < len(s) && s[i] == '"' {
		offset := strings.IndexByte(s[i+1:], '"')
		if offset < 0 {
			return "", 0, false, false
		}
		name := s[i+1 : i+1+offset]
		if strings.TrimSpace(name) == "" {
			return "", 0, false, false
		}
		return name, i + 1 + offset + 1, true, true
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
		return "", 0, false, false
	}
	return s[start:i], i, false, true
}

// trimToExisting 把无引号引用里**确实存在的最长路径前缀**截出来，让尾巴（多半是紧跟着
// 的中文正文）回到问题里。
//
// 两条克制：**整段存在就不裁**（哪怕它是目录，交给 loadRef 去说「是目录」）；**只裁到
// 「存在的普通文件」**——否则 `/no/such/file` 会被裁到存在的祖先 `/`，把「读不到」变成
// 「@/ 是目录」。裁不到就原样返回，让 loadRef 报读不到。退位按 rune（不能按字节，会切坏
// 汉字）。
func trimToExisting(s string, at, end int) (string, int) {
	token := s[at+1 : end]
	if _, err := os.Stat(token); err == nil {
		return token, end
	}
	for cut := len(token); cut > 0; {
		candidate := token[:cut]
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, at + 1 + cut
		}
		_, size := utf8.DecodeLastRuneInString(token[:cut])
		cut -= size
	}
	return token, end
}

// loadRef 读一个被引用文件并分类。**读不到不算错**——调用方转成一句提示，
// 原样把 token 留给模型。
func loadRef(name string) (loadedRef, error) {
	info, err := os.Stat(name)
	if err != nil {
		return loadedRef{}, statReason(name, err)
	}
	if info.IsDir() {
		return loadedRef{}, errors.New("是目录")
	}

	file, err := os.Open(name)
	if err != nil {
		if os.IsPermission(err) {
			return loadedRef{}, errors.New("没有读权限")
		}
		return loadedRef{}, errors.New("打不开这个文件")
	}
	defer file.Close()

	// 先读开头一段做类型判断：图片与文本的上限不同，读多少取决于先判出来的类型
	head := make([]byte, 512)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return loadedRef{}, errors.New("读取失败")
	}
	head = head[:n]

	ref := loadedRef{name: name, mime: http.DetectContentType(head), size: info.Size()}

	if strings.HasPrefix(ref.mime, "image/") {
		rest, err := io.ReadAll(io.LimitReader(file, fileRefImageLimit+1))
		if err != nil {
			return loadedRef{}, errors.New("读取失败")
		}
		data := append(append([]byte(nil), head...), rest...)
		if len(data) > fileRefImageLimit {
			return loadedRef{}, fmt.Errorf("图片超过 %d MiB", fileRefImageLimit>>20)
		}
		ref.kind = refImage
		ref.image = llm.ImageAttachment{
			Name:    name,
			DataURI: "data:" + ref.mime + ";base64," + base64.StdEncoding.EncodeToString(data),
		}
		return ref, nil
	}

	// 二进制塞进文本提示词只会浪费预算并可能弄坏编码：只留元信息，内容交给工具
	if bytes.IndexByte(head, 0) >= 0 {
		ref.kind = refBinary
		return ref, nil
	}

	rest, err := io.ReadAll(io.LimitReader(file, fileRefLimit+1))
	if err != nil {
		return loadedRef{}, errors.New("读取失败")
	}
	data := append(append([]byte(nil), head...), rest...)
	truncated := false
	if len(data) > fileRefLimit {
		data = data[:fileRefLimit]
		truncated = true
	}
	ref.kind = refText
	ref.text = string(data)
	if truncated {
		ref.text += fmt.Sprintf("\n…（文件超过 %d 字节，已截断）", fileRefLimit)
	}
	return ref, nil
}

// statReason 把 `os.Stat` 的错翻成一句人话。**相对路径按进程的当前目录解析**是最常
// 踩的一个坑：用户按安装根（FKA_HOME）的心智去找文件，而引用走的是 cwd——两种跑法
// （`make ask` 与直接跑二进制）cwd 不一样，症状就是「明明在那儿却读不到」。
func statReason(name string, err error) error {
	switch {
	case os.IsNotExist(err):
		if !filepath.IsAbs(name) {
			return errors.New("文件不存在（相对路径按当前目录解析，不是安装根）")
		}
		return errors.New("文件不存在")
	case os.IsPermission(err):
		return errors.New("没有读权限")
	default:
		return err
	}
}

// humanBytes 把字节数写成人看的大小。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
