package cli

import (
	"regexp"
	"strings"
	"testing"
)

// TestSession_每次都是新的 兜底曾经是常量 "cli"，于是**每一次**不带 `--session`
// 的命令行问答都在续上一个：历史按会话落文件，命令行里什么都没变，而症状只在恰好
// 问到相关话题时显形。
func TestSession_每次都是新的(t *testing.T) {
	first := NewSessionID()
	second := NewSessionID()

	if first == second {
		t.Fatalf("两次该是两个会话，实际都是 %q —— 历史会串", first)
	}
	for _, got := range []string{first, second} {
		if !strings.HasPrefix(got, SessionPrefix) {
			t.Errorf("命令行会话该带 %s 前缀，实际 %q", SessionPrefix, got)
		}
	}
}

// TestSession_正好用满文件名预算 `llm.safeSegment` 把会话段截到 40 字符，**截断是
// 静默的**：两个只在末尾不同的会话会落进同一个历史文件，串历史的问题又回来了。
// 所以这个长度是被钉住的，不是碰巧。
func TestSession_正好用满文件名预算(t *testing.T) {
	const budget = 40

	got := NewSessionID()
	if len(got) != budget {
		t.Errorf("会话 id 该正好 %d 字符（%s + UUID 36），实际 %d：%q",
			budget, SessionPrefix, len(got), got)
	}
	if !uuidShape.MatchString(got) {
		t.Errorf("该是 %s<uuid v4> 的形状，实际 %q", SessionPrefix, got)
	}
}

// uuidShape UUID v4 的形状（8-4-4-4-12，版本位 4、变体位 8/9/a/b）。
var uuidShape = regexp.MustCompile(`^cli-[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
