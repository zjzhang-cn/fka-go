package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRefFile 在临时目录里写一个可引用的文件，返回绝对路径。
func writeRefFile(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("写测试文件失败：%v", err)
	}
	return path
}

// pngBytes 一段以 PNG 签名开头的字节，够 http.DetectContentType 认出 image/png。
func pngBytes(extra int) []byte {
	head := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	return append(head, make([]byte, extra)...)
}

// Test引用文件_展开并去重：问题里两次引用同一个文件，正文只附一次，问题原文不动。
func Test引用文件_展开并去重(t *testing.T) {
	path := writeRefFile(t, "笔记.txt", []byte("三亚的行程"))
	warn := &strings.Builder{}

	got := expandFileRefs("看下 @"+path+" 和 @"+path, warn)

	if !strings.Contains(got.text, "三亚的行程") {
		t.Errorf("正文没展开出来：%q", got.text)
	}
	if !strings.Contains(got.text, "用户引用了这些本地文件") {
		t.Errorf("缺引用小节标题：%q", got.text)
	}
	if !strings.HasPrefix(got.text, "看下 @"+path+" 和 @"+path) {
		t.Errorf("问题原文该保留在最前：%q", got.text)
	}
	if len(got.names) != 1 || got.names[0] != path {
		t.Errorf("引用清单 = %v，想要去重后的一条", got.names)
	}
	if len(got.images) != 0 {
		t.Errorf("文本不该产生图片附件：%v", got.images)
	}
	if warn.String() != "" {
		t.Errorf("不该有告警：%q", warn.String())
	}
}

// Test引用文件_读不到按普通文字：不存在的路径不报错、不中断，原样交给模型。
func Test引用文件_读不到按普通文字(t *testing.T) {
	warn := &strings.Builder{}
	question := "看下 @/no/such/file 吧"

	got := expandFileRefs(question, warn)

	if got.text != question {
		t.Errorf("读不到时问题该原样，实际 %q", got.text)
	}
	if len(got.names) != 0 {
		t.Errorf("读不到不该算引用：%v", got.names)
	}
	if !strings.Contains(warn.String(), "读不到") {
		t.Errorf("该提示一句读不到：%q", warn.String())
	}
}

// Test引用文件_带空格用引号：路径含空格时靠引号界定。
func Test引用文件_带空格用引号(t *testing.T) {
	path := writeRefFile(t, "我的 文档.txt", []byte("内容在此"))

	got := expandFileRefs(`总结 @"`+path+`"`, &strings.Builder{})

	if len(got.names) != 1 || got.names[0] != path {
		t.Fatalf("引号路径没读到：%v", got.names)
	}
	if !strings.Contains(got.text, "内容在此") {
		t.Errorf("正文没展开：%q", got.text)
	}
}

// Test引用文件_邮箱不当引用：`@` 前面是字母就不认——否则邮箱会被拆成路径。
func Test引用文件_邮箱不当引用(t *testing.T) {
	warn := &strings.Builder{}
	question := "发到 foo@bar.com 谢谢"

	got := expandFileRefs(question, warn)

	if got.text != question || len(got.names) != 0 {
		t.Errorf("邮箱不该被当引用：%q / %v", got.text, got.names)
	}
	if warn.String() != "" {
		t.Errorf("邮箱不该触发告警：%q", warn.String())
	}
}

// Test引用文件_图片作为附件：图片不进正文，而是变成随消息发送的附件；
// 正文里只留一句「已作为附件发送」。
func Test引用文件_图片作为附件(t *testing.T) {
	path := writeRefFile(t, "照片.png", pngBytes(64))

	got := expandFileRefs("这是什么 @"+path, &strings.Builder{})

	if len(got.images) != 1 {
		t.Fatalf("图片该成一个附件：%v", got.images)
	}
	if !strings.HasPrefix(got.images[0].DataURI, "data:image/png;base64,") {
		t.Errorf("附件该是 data URI：%q", got.images[0].DataURI)
	}
	if got.images[0].Name != path {
		t.Errorf("附件名不对：%q", got.images[0].Name)
	}
	if !strings.Contains(got.text, "附件发送") {
		t.Errorf("正文该说明图片已作为附件：%q", got.text)
	}
	if len(got.names) != 1 || got.names[0] != path {
		t.Errorf("引用清单该含图片：%v", got.names)
	}
}

// Test引用文件_其它二进制只附元信息：非图片的二进制不发内容，只留路径/类型/大小，
// 交给 MCP 工具去读。
func Test引用文件_其它二进制只附元信息(t *testing.T) {
	path := writeRefFile(t, "blob.bin", []byte("abc\x00def"))

	got := expandFileRefs("@"+path, &strings.Builder{})

	if len(got.images) != 0 {
		t.Errorf("非图片二进制不该成附件：%v", got.images)
	}
	if !strings.Contains(got.text, "二进制文件") {
		t.Errorf("该标出是二进制、内容未附：%q", got.text)
	}
	if len(got.names) != 1 {
		t.Errorf("引用清单该含它：%v", got.names)
	}
}

// Test引用文件_图片过大直接跳过：超过上限的图片不截断（半张图没意义），按读不到处理。
func Test引用文件_图片过大直接跳过(t *testing.T) {
	path := writeRefFile(t, "big.png", pngBytes(fileRefImageLimit))
	warn := &strings.Builder{}

	got := expandFileRefs("@"+path, warn)

	if len(got.images) != 0 {
		t.Errorf("过大图片不该成附件：%v", got.images)
	}
	if !strings.Contains(warn.String(), "图片超过") {
		t.Errorf("该提示图片过大：%q", warn.String())
	}
}

// Test引用文件_目录跳过：引用一个目录不算引用。
func Test引用文件_目录跳过(t *testing.T) {
	warn := &strings.Builder{}

	got := expandFileRefs("@"+t.TempDir(), warn)

	if len(got.names) != 0 {
		t.Errorf("目录不该算引用：%v", got.names)
	}
	if !strings.Contains(warn.String(), "是目录") {
		t.Errorf("该提示是目录：%q", warn.String())
	}
}

// Test引用文件_超长文本截断：超过上限就截断并标出，避免把上下文预算吃光。
func Test引用文件_超长文本截断(t *testing.T) {
	long := strings.Repeat("甲", fileRefLimit+50)
	path := writeRefFile(t, "big.txt", []byte(long))

	got := expandFileRefs("@"+path, &strings.Builder{})

	if len(got.names) != 1 {
		t.Fatalf("应当算一个引用：%v", got.names)
	}
	if !strings.Contains(got.text, "已截断") {
		t.Errorf("该标出已截断：%q", got.text)
	}
	if strings.Contains(got.text, long) {
		t.Error("超长文件该被截断，实际整段都在")
	}
}

// Test引用文件_中文连着写也认：`看下@路径`（`@` 前是中文，没有空格）要能认出；
// 路径后的中文句读（`，`）要停下，不能把后面的正文吞进路径。
func Test引用文件_中文连着写也认(t *testing.T) {
	path := writeRefFile(t, "文档.txt", []byte("正文在此"))

	got := expandFileRefs("看下@"+path+"，谢谢", &strings.Builder{})

	if len(got.names) != 1 || got.names[0] != path {
		t.Fatalf("中文连着写的引用没认出：%v", got.names)
	}
	if !strings.Contains(got.text, "正文在此") {
		t.Errorf("正文没展开：%q", got.text)
	}
	if !strings.Contains(got.text, "，谢谢") {
		t.Errorf("句读后的正文被吞了：%q", got.text)
	}
}

// Test引用文件_没有引用就原样：没有 `@路径` 时一个问题字符都不动。
func Test引用文件_没有引用就原样(t *testing.T) {
	question := "今天天气怎么样？"
	got := expandFileRefs(question, &strings.Builder{})
	if got.text != question || got.names != nil || got.images != nil {
		t.Errorf("无引用时不该改问题：%q / %v / %v", got.text, got.names, got.images)
	}
}
