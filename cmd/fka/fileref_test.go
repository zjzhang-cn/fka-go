package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRefFile 在临时目录里写一个可引用的文件，返回绝对路径。
func writeRefFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写测试文件失败：%v", err)
	}
	return path
}

// Test引用文件_展开并去重：问题里两次引用同一个文件，正文只附一次，问题原文不动。
func Test引用文件_展开并去重(t *testing.T) {
	path := writeRefFile(t, "笔记.txt", "三亚的行程")
	warn := &strings.Builder{}

	expanded, attached := expandFileRefs("看下 @"+path+" 和 @"+path, warn)

	if !strings.Contains(expanded, "三亚的行程") {
		t.Errorf("正文没展开出来：%q", expanded)
	}
	if !strings.Contains(expanded, "不必再用工具读取") {
		t.Errorf("缺引用小节标题：%q", expanded)
	}
	if !strings.HasPrefix(expanded, "看下 @"+path+" 和 @"+path) {
		t.Errorf("问题原文该保留在最前：%q", expanded)
	}
	if len(attached) != 1 || attached[0] != path {
		t.Errorf("引用清单 = %v，想要去重后的一条", attached)
	}
	if warn.String() != "" {
		t.Errorf("不该有告警：%q", warn.String())
	}
}

// Test引用文件_读不到按普通文字：不存在的路径不报错、不中断，原样交给模型。
func Test引用文件_读不到按普通文字(t *testing.T) {
	warn := &strings.Builder{}
	question := "看下 @/no/such/file 吧"

	expanded, attached := expandFileRefs(question, warn)

	if expanded != question {
		t.Errorf("读不到时问题该原样，实际 %q", expanded)
	}
	if len(attached) != 0 {
		t.Errorf("读不到不该算引用：%v", attached)
	}
	if !strings.Contains(warn.String(), "读不到") {
		t.Errorf("该提示一句读不到：%q", warn.String())
	}
}

// Test引用文件_带空格用引号：路径含空格时靠引号界定。
func Test引用文件_带空格用引号(t *testing.T) {
	path := writeRefFile(t, "我的 文档.txt", "内容在此")

	expanded, attached := expandFileRefs(`总结 @"`+path+`"`, &strings.Builder{})

	if len(attached) != 1 || attached[0] != path {
		t.Fatalf("引号路径没读到：%v", attached)
	}
	if !strings.Contains(expanded, "内容在此") {
		t.Errorf("正文没展开：%q", expanded)
	}
}

// Test引用文件_邮箱不当引用：`@` 前面是字母就不认——否则邮箱会被拆成路径。
func Test引用文件_邮箱不当引用(t *testing.T) {
	warn := &strings.Builder{}
	question := "发到 foo@bar.com 谢谢"

	expanded, attached := expandFileRefs(question, warn)

	if expanded != question || len(attached) != 0 {
		t.Errorf("邮箱不该被当引用：%q / %v", expanded, attached)
	}
	if warn.String() != "" {
		t.Errorf("邮箱不该触发告警：%q", warn.String())
	}
}

// Test引用文件_二进制跳过：带 NUL 的文件不当引用（塞进提示词只会浪费预算）。
func Test引用文件_二进制跳过(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blob.bin")
	if err := os.WriteFile(path, []byte("abc\x00def"), 0o644); err != nil {
		t.Fatal(err)
	}
	warn := &strings.Builder{}

	expanded, attached := expandFileRefs("@"+path, warn)

	if len(attached) != 0 {
		t.Errorf("二进制不该算引用：%v", attached)
	}
	if !strings.Contains(warn.String(), "二进制") {
		t.Errorf("该提示是二进制：%q", warn.String())
	}
	_ = expanded
}

// Test引用文件_目录跳过：引用一个目录不算引用。
func Test引用文件_目录跳过(t *testing.T) {
	warn := &strings.Builder{}

	_, attached := expandFileRefs("@"+t.TempDir(), warn)

	if len(attached) != 0 {
		t.Errorf("目录不该算引用：%v", attached)
	}
	if !strings.Contains(warn.String(), "是目录") {
		t.Errorf("该提示是目录：%q", warn.String())
	}
}

// Test引用文件_超长截断：超过上限就截断并标出，避免把上下文预算吃光。
func Test引用文件_超长截断(t *testing.T) {
	long := strings.Repeat("甲", fileRefLimit+50)
	path := writeRefFile(t, "big.txt", long)

	expanded, attached := expandFileRefs("@"+path, &strings.Builder{})

	if len(attached) != 1 {
		t.Fatalf("应当算一个引用：%v", attached)
	}
	if !strings.Contains(expanded, "已截断") {
		t.Errorf("该标出已截断：%q", expanded)
	}
	if strings.Contains(expanded, long) {
		t.Error("超长文件该被截断，实际整段都在")
	}
}

// Test引用文件_中文连着写也认：`看下@路径`（`@` 前是中文，没有空格）要能认出；
// 路径后的中文句读（`，`）要停下，不能把后面的正文吞进路径。
func Test引用文件_中文连着写也认(t *testing.T) {
	path := writeRefFile(t, "文档.txt", "正文在此")

	expanded, attached := expandFileRefs("看下@"+path+"，谢谢", &strings.Builder{})

	if len(attached) != 1 || attached[0] != path {
		t.Fatalf("中文连着写的引用没认出：%v", attached)
	}
	if !strings.Contains(expanded, "正文在此") {
		t.Errorf("正文没展开：%q", expanded)
	}
	if !strings.Contains(expanded, "，谢谢") {
		t.Errorf("句读后的正文被吞了：%q", expanded)
	}
}

// Test引用文件_没有引用就原样：没有 `@路径` 时一个问题字符都不动。
func Test引用文件_没有引用就原样(t *testing.T) {
	question := "今天天气怎么样？"
	expanded, attached := expandFileRefs(question, &strings.Builder{})
	if expanded != question || attached != nil {
		t.Errorf("无引用时不该改问题：%q / %v", expanded, attached)
	}
}
