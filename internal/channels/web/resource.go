// 本文件是**会话媒体目录**与**`#` 引用**：给每个会话记下「出现过哪些媒体」（用户上传的
// 与 agent 发来的），并让带 `#名字` 的消息在入站时把对应媒体并进本轮。
//
// ## 目录不存字节
//
// 字节还在 `inbound` / `blobs` 两个暂存里过路（TTL 到了就没），这里只记「名字 → 句柄」。
// 所以列清单、解析引用时都要**现查现滤**：字节没了就不列、就跳过——列一个引用不了的
// 名字没有意义，而目录也不替别人续命（这个 agent 不拥有数据）。
//
// ## 为什么名字是契约
//
// 前端打 `#` 时从 `GET /resources` 拿这份目录，选一个插入 `#名字`；服务端收到消息时按
// 名字回查。两个方向都以名字为准，所以名字即接口——重名以**最新那份**为准。
//
// ## 为将来的沙盒资源留的口子
//
// 条目有 `Source` 一栏（现在恒为 `sourceMedia`）。接沙盒资源时新增 `sourceSandbox`，
// `GET /resources` 把沙盒那份并进来、解析时按 source 派发即可——前端不用改。
package web

import (
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/zjzhang-cn/fka-go/internal/channels"
)

// sourceMedia 目录条目的来源：会话媒体（用户上传的 + agent 发来的）。
// 将来接沙盒资源另起一个值，前端按它分组/图标。
const sourceMedia = "media"

// resourceMaxPerConversation 每个会话最多记多少条目录项（超出丢最旧）。有上限是必须的：
// 没有上限就是让一次会话的媒体数无限撑大内存。
const resourceMaxPerConversation = 50

// resource 一条会话媒体目录项。**只记名字与句柄，不存字节**。
type resource struct {
	// Name 显示名，也是 `#引用` 用的名字（文件 basename）
	Name string
	// Mime 声明的 MIME（字节里的那份为准，这里兜底）
	Mime string
	// Kind 大类（image / file），前端挑图标用
	Kind string
	// Source 来源，见 sourceMedia / 将来的 sourceSandbox
	Source string

	// id 句柄 + 落在哪个暂存。**不导出**：只有本包的取字节逻辑用得到。
	id          string
	fromInbound bool
}

// resourceStore 会话 → 资源目录（内部按时间序，取时反成新→旧）。单进程、单账号，
// 所以就是一个带锁的 map。
type resourceStore struct {
	mu     sync.Mutex
	byConv map[string][]resource
}

func newResourceStore() *resourceStore {
	return &resourceStore{byConv: map[string][]resource{}}
}

// add 记一条。**同名覆盖**（最新那份字节为准）并挪到末尾；超出上限丢最旧的。
func (s *resourceStore) add(conversation string, r resource) {
	if conversation == "" || strings.TrimSpace(r.Name) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.byConv[conversation]
	for i := range list {
		if list[i].Name == r.Name {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	list = append(list, r)
	if len(list) > resourceMaxPerConversation {
		list = list[len(list)-resourceMaxPerConversation:]
	}
	s.byConv[conversation] = list
}

// lookup 按名字找，**后记的优先**（最新的名字指向最新那份字节）。
func (s *resourceStore) lookup(conversation, name string) (resource, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.byConv[conversation]
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].Name == name {
			return list[i], true
		}
	}
	return resource{}, false
}

// snapshot 某会话的目录，**新→旧**（选择器把最新的排前面）。返回副本。
func (s *resourceStore) snapshot(conversation string) []resource {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.byConv[conversation]
	out := make([]resource, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		out = append(out, list[i])
	}
	return out
}

// recordInbound 把一条入站媒体记进目录（供之后的 `#引用`）。空名字的跳过——引用不了。
func (c *Channel) recordInbound(conversation string, part channels.Part) {
	name := strings.TrimSpace(part.Media.FileName)
	if name == "" || part.Media.ID == "" {
		return
	}
	c.resources.add(conversation, resource{
		Name: name, Mime: part.Media.MimeType, Kind: resourceKind(part.Media.MimeType),
		Source: sourceMedia, id: part.Media.ID, fromInbound: true,
	})
}

// recordOutbound 把一条 agent 发来的媒体记进目录。它落在 `blobs`（`/files/{id}` 那份），
// 解析时取字节再落进 inbound——消息层只从 inbound 取。
func (c *Channel) recordOutbound(conversation, id, name, mime string) {
	name = strings.TrimSpace(name)
	if conversation == "" || name == "" || id == "" {
		return
	}
	c.resources.add(conversation, resource{
		Name: name, Mime: mime, Kind: resourceKind(mime),
		Source: sourceMedia, id: id, fromInbound: false,
	})
}

// resourceAvailable 目录项背后的字节还在不在（两个暂存各自带 TTL）。
func (c *Channel) resourceAvailable(r resource) bool {
	_, ok := c.resourceBlob(r)
	return ok
}

// resourceBlob 取目录项背后的字节与 MIME。字节没了返 ok=false。
func (c *Channel) resourceBlob(r resource) (blob, bool) {
	store := c.blobs
	if r.fromInbound {
		store = c.inbound
	}
	item, ok := store.get(r.id)
	if !ok {
		return blob{}, false
	}
	if strings.TrimSpace(item.mime) == "" {
		item.mime = r.Mime
	}
	return item, true
}

// resolveRefs 从正文里认出 `#名字`，把命中的会话媒体变成 part 并进本轮。
//
// **名字不存在、字节已过期、认不出，都原样留着文字、不报错**——一次问答不该因为一个
// 笔误的引用就发不出去（与 CLI `@引用`、沙盒 `read` 同一条克制）。`skip` 是同一批已作为
// 附件上传的名字，避免「又传又引用」时同一份媒体进两次。
func (c *Channel) resolveRefs(conversation, text string, skip map[string]bool) []channels.Part {
	var parts []channels.Part
	seen := map[string]bool{}
	for _, name := range parseRefs(text) {
		if seen[name] || skip[name] {
			continue
		}
		seen[name] = true
		r, ok := c.resources.lookup(conversation, name)
		if !ok {
			continue
		}
		if part, ok := c.partForResource(r); ok {
			parts = append(parts, part)
		}
	}
	return parts
}

// partForResource 把一条目录项变成一个 part。入站句柄直接用；出站那份取字节、重新落进
// `inbound`——消息层经 `FetchMedia` 只读 inbound 这一个暂存，两条路在这里合流。
func (c *Channel) partForResource(r resource) (channels.Part, bool) {
	item, ok := c.resourceBlob(r)
	if !ok {
		return channels.Part{}, false
	}
	if r.fromInbound {
		return channels.Part{
			Kind: kindOfMime(item.mime),
			Media: channels.MediaRef{
				ID: r.id, FileName: r.Name, MimeType: item.mime, Size: int64(len(item.data)),
			},
		}, true
	}
	id, stored := c.inbound.put(item.data, r.Name, item.mime)
	if !stored {
		return channels.Part{}, false
	}
	return channels.Part{
		Kind: kindOfMime(item.mime),
		Media: channels.MediaRef{
			ID: id, FileName: r.Name, MimeType: item.mime, Size: int64(len(item.data)),
		},
	}, true
}

// resourceKind 目录项的大类，前端挑图标用。图片单独一类，其余都当文件。
func resourceKind(mime string) string {
	if strings.HasPrefix(strings.ToLower(mime), "image/") {
		return "image"
	}
	return "file"
}

// kindOfMime 把一个媒体件的 MIME 翻成消息层的 part 类别。
func kindOfMime(mime string) channels.MediaKind {
	if strings.HasPrefix(strings.ToLower(mime), "image/") {
		return channels.KindImage
	}
	return channels.KindFile
}

// ── `#引用` 的解析 ──────────────────────────────────────
//
// 与 CLI 的 `@引用` 同构（见 cmd/fka/fileref.go）：只认**词首**的标记、名字到空白或句读
// 为止、含空格/标点的名字用引号。两处是不同进程层的输入解析，刻意不共享代码——那里按
// 文件系统裁路径，这里不需要。

// parseRefs 抽出正文里的 `#引用`。`#名字` 到空白或句读为止；名字含空格/标点时写
// `#"名 字"`。只认词首的 `#`（前一个字符不是 ASCII 文件名字符）：`C#` 不算，
// markdown 标题 `# 标题`（`#` 后是空格）也不算。
func parseRefs(text string) []string {
	var out []string
	for i := 0; i < len(text); {
		if text[i] == '#' && refBoundary(text, i) {
			if name, end, ok := scanRef(text, i); ok {
				out = append(out, name)
				i = end
				continue
			}
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		i += size
	}
	return out
}

// refBoundary `#` 处是不是词首。与 CLI 同一条窄集判断（`foo#bar` 的 `o` 是字母 → 不是词首）。
func refBoundary(s string, at int) bool {
	if at == 0 {
		return true
	}
	prev, _ := utf8.DecodeLastRuneInString(s[:at])
	return !refWordRune(prev)
}

// refWordRune 词首判断用的窄集：连续的 ASCII 文件名字符。
func refWordRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '_', r == '-', r == '.', r == '/', r == '~', r == '\\':
		return true
	}
	return false
}

// scanRef 从 `at`（`#` 处）读出一个引用：返回名字、消费到的结束位置、是否成立。
func scanRef(s string, at int) (name string, end int, ok bool) {
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
		if !refNameRune(r) {
			break
		}
		i += size
	}
	if i == start {
		return "", 0, false
	}
	return s[start:i], i, true
}

// refNameRune 名字扫描用字。**到空白或句读标点为止**；中文文件名也要能读，所以不限于
// ASCII，但 `，。！？` 这些要停下，否则 `#a.png，谢谢` 会把「谢谢」也吞进名字。
func refNameRune(r rune) bool {
	if unicode.IsSpace(r) {
		return false
	}
	switch r {
	case '，', '。', '！', '？', '、', '；', '：', '·',
		'“', '”', '‘', '’', '「', '」', '『', '』', '【', '】', '《', '》', '（', '）',
		',', '!', '?', ';', ':', '"', '\'', '`', '(', ')', '[', ']', '{', '}', '<', '>', '#':
		return false
	}
	return true
}
