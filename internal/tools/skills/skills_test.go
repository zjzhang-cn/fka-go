package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/tools"
)

func writeSkill(t *testing.T, dir, name, body string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("建技能目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, SkillFile), []byte(body), 0o644); err != nil {
		t.Fatalf("写技能失败：%v", err)
	}
}

func list(t *testing.T, source tools.Source) []tools.Spec {
	t.Helper()
	specs, err := source.List(context.Background(), tools.Context{})
	if err != nil {
		t.Fatalf("List 返错：%v", err)
	}
	return specs
}

// TestParseSkill_极简frontmatter 不引 YAML 库，只认 key: value 一行一条；
// **认不出的一律当正文，绝不因为格式不合就丢掉一个技能**。
func TestParseSkill_极简frontmatter(t *testing.T) {
	skill := ParseSkill(`---
name: 备份照片
description: 把手机里的照片归档到 NAS
---

先连上 NAS，确认可写。
1. 复制
2. 校验`, "dir-name")

	if skill.Name != "备份照片" {
		t.Errorf("Name = %q", skill.Name)
	}
	if skill.Description != "把手机里的照片归档到 NAS" {
		t.Errorf("Description = %q", skill.Description)
	}
	if strings.Contains(skill.Body, "name:") {
		t.Errorf("front matter 混进正文了：%q", skill.Body)
	}
	if !strings.Contains(skill.Body, "先连上 NAS") {
		t.Errorf("正文不对：%q", skill.Body)
	}
	// Dir 独立于 Name——按名字找时两个都认
	if skill.Dir != "dir-name" {
		t.Errorf("Dir = %q", skill.Dir)
	}
}

// TestParseSkill_没有frontmatter就全文当正文 手工放进去的技能很可能不写 front matter。
func TestParseSkill_没有frontmatter就全文当正文(t *testing.T) {
	skill := ParseSkill("# 标题\n\n这一步做归档，别的都不用管。\n", "backup")

	if skill.Name != "backup" {
		t.Errorf("没写 name 时该退回目录名：%q", skill.Name)
	}
	// 描述取正文第一行有内容、且剥掉行首标题/列表符之后的文字。
	// 注意 `## 标题` 的说明就是「标题」——标题符与其后的空白被剥掉，第一个
	// 非空白字符就停。这与 Node 版的正则 `^[#>\-*\s]+` 一致。
	if skill.Description != "标题" {
		t.Errorf("Description = %q", skill.Description)
	}
	if !strings.HasPrefix(skill.Body, "# 标题") {
		t.Errorf("正文被切掉了：%q", skill.Body)
	}
}

func TestParseSkill_格式坏了不丢技能(t *testing.T) {
	// 开头有 --- 但没有结束标记 → 整篇当正文，**不能丢**
	skill := ParseSkill("---\nname: x\n\n正文还在。", "dir")
	if !strings.Contains(skill.Body, "正文还在") {
		t.Errorf("正文不该丢：%q", skill.Body)
	}
	// 首行不是 --- → 整篇当正文
	skill = ParseSkill("name: x\n正文", "dir")
	if !strings.Contains(skill.Body, "name: x") {
		t.Errorf("没有 front matter 时该全文当正文：%q", skill.Body)
	}
}

func TestFirstLine_跳过标题与列表符(t *testing.T) {
	cases := map[string]string{
		// 行首标题符与其后的空白被剥掉，第一个非空白字符就停
		"## 标题\n\n真正的说明": "标题",
		"- 列表项":          "列表项",
		"\n\n   ":        "（这个技能没有写说明）",
	}
	for input, want := range cases {
		if got := firstLine(input); got != want {
			t.Errorf("firstLine(%q) = %q，期望 %q", input, got, want)
		}
	}
}

// TestNewSource_有技能才声明工具 一个技能都没有时返回空——不声明 list/load。
// 空不等于错，但让模型去调一个必然失败的工具是白烧一轮。
func TestNewSource_有技能才声明工具(t *testing.T) {
	dir := t.TempDir()

	// 空目录 → 不声明
	if specs := list(t, NewSource([]string{dir})); len(specs) != 0 {
		t.Errorf("没有技能时不该声明工具：%+v", specs)
	}

	writeSkill(t, dir, "backup", "---\nname: 备份\ndescription: 归档照片\n---\n步骤")
	specs := list(t, NewSource([]string{dir}))
	if len(specs) != 2 {
		t.Fatalf("有技能时应声明 list 与 load：%+v", specs)
	}
	if specs[0].Name != ToolList || specs[1].Name != ToolLoad {
		t.Errorf("声明的顺序/名字不对：%s / %s", specs[0].Name, specs[1].Name)
	}
}

func TestNewSource_目录不存在是正常状态(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "根本没有这个目录")
	if specs := list(t, NewSource([]string{missing})); len(specs) != 0 {
		t.Errorf("目录不存在不该报错也不该声明工具：%+v", specs)
	}
}

// TestSource_多目录后者覆盖前者 一份安装自带的基础技能，可以被用户目录里的同名技能顶掉，
// 而不必改安装包。
func TestSource_多目录后者覆盖前者(t *testing.T) {
	base, user := t.TempDir(), t.TempDir()
	writeSkill(t, base, "backup", "---\nname: 备份\ndescription: 安装自带的那份\n---\n旧步骤")
	writeSkill(t, user, "backup", "---\nname: 备份\ndescription: 用户自己那份\n---\n新步骤")

	source := NewSource([]string{base, user})

	// load 给的是正文
	result, err := source.Call(context.Background(), ToolLoad, map[string]any{"name": "备份"}, tools.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("不该失败：%s", result.Content)
	}
	if !strings.Contains(result.Content, "新步骤") || strings.Contains(result.Content, "旧步骤") {
		t.Errorf("后一个目录的同名技能该覆盖前一个：%s", result.Content)
	}

	// 覆盖的是**整个技能**，所以说明也跟着换
	listing, err := source.Call(context.Background(), ToolList, nil, tools.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listing.Content, "用户自己那份") {
		t.Errorf("说明也该被覆盖：%s", listing.Content)
	}
}

// TestSource_找技能时大小写与空白不算差别 模型可能把「备份照片」写成「备份 照片」。
func TestSource_找技能时大小写与空白不算差别(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "x", "---\nname: 备份照片\ndescription: d\n---\n步骤")

	source := NewSource([]string{dir})
	for _, wanted := range []string{"备份照片", "备份 照片", " 备份照片 ", "备份照片"} {
		result, err := source.Call(context.Background(), ToolLoad, map[string]any{"name": wanted}, tools.Context{})
		if err != nil {
			t.Fatal(err)
		}
		if !result.OK {
			t.Errorf("「%s」应当找得到：%s", wanted, result.Content)
		}
	}
}

// TestSource_不认目录名 TestSource 也要能按**目录名**找到——front matter 的名字可能
// 带空格或标点，模型照着系统提示里的清单叫的。
func TestSource_也能按目录名找(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "music-catalog", "---\nname: 音乐库分析\ndescription: d\n---\n步骤")

	source := NewSource([]string{dir})
	result, err := source.Call(context.Background(), ToolLoad,
		map[string]any{"name": "music-catalog"}, tools.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Errorf("按目录名应当找得到：%s", result.Content)
	}
}

// TestSource_找不到时列候选 「没有这个技能」而不给出候选，模型就只能瞎猜。
func TestSource_找不到时列候选(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "a", "---\nname: 备份\n---\n步骤")
	writeSkill(t, dir, "b", "---\nname: 报销\n---\n步骤")

	source := NewSource([]string{dir})
	result, err := source.Call(context.Background(), ToolLoad,
		map[string]any{"name": "不存在的技能"}, tools.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if result.OK {
		t.Fatal("不该成功")
	}
	if !strings.Contains(result.Content, "备份") || !strings.Contains(result.Content, "报销") {
		t.Errorf("该列出可用的技能：%s", result.Content)
	}
}

// TestSource_不做子串匹配 「报销」不该被「报销流程」顶替——宁可让模型照候选重叫。
func TestSource_不做子串匹配(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "a", "---\nname: 报销流程\n---\n步骤")

	source := NewSource([]string{dir})
	result, err := source.Call(context.Background(), ToolLoad,
		map[string]any{"name": "报销"}, tools.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if result.OK {
		t.Errorf("子串不该匹配上：%s", result.Content)
	}
}

// TestSource_清单顺序稳定 系统提示里的清单顺序每轮都一样——否则拼出来的提示词
// 前缀就变了，**provider 的前缀缓存命中不了**。
func TestSource_清单顺序稳定(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"c", "a", "b", "e", "d"} {
		writeSkill(t, dir, name, "---\nname: 技能"+name+"\ndescription: d\n---\n步骤")
	}

	source := NewSource([]string{dir})
	first, err := source.Call(context.Background(), ToolList, nil, tools.Context{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := source.Call(context.Background(), ToolList, nil, tools.Context{})
		if err != nil {
			t.Fatal(err)
		}
		if again.Content != first.Content {
			t.Fatalf("清单顺序不稳定：\n%s\n%s", first.Content, again.Content)
		}
	}
	// 名字按字典序
	if !strings.Contains(first.Content, `技能a`) || !strings.Contains(first.Content, `技能e`) {
		t.Errorf("清单内容不对：%s", first.Content)
	}
}

// TestSource_改完不用重启 技能是**随时会变的运维对象**，不该逼人重启服务。
func TestSource_改完不用重启(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "first", "---\nname: 第一个\ndescription: d\n---\n步骤")
	source := NewSource([]string{dir})

	if specs := list(t, source); len(specs) != 2 {
		t.Fatalf("先有一个技能：%+v", specs)
	}

	// 丢一个新技能进去，下一轮就该看得见
	writeSkill(t, dir, "second", "---\nname: 第二个\ndescription: d\n---\n步骤")

	result, err := source.Call(context.Background(), ToolList, nil, tools.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "第二个") {
		t.Errorf("新技能应当立刻可用：%s", result.Content)
	}
}

// TestSource_提示段落列出技能 目录清单是**系统提示里唯一**让模型知道有哪些技能的地方。
func TestSource_提示段落列出技能(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "a", "---\nname: 备份\ndescription: 归档照片\n---\n步骤")

	source := NewSource([]string{dir})
	section, err := source.PromptSection(tools.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(section, "备份") || !strings.Contains(section, "归档照片") {
		t.Errorf("提示段落要含名字与说明：%q", section)
	}
	// 空目录 → 没有这一段
	empty, err := NewSource([]string{t.TempDir()}).PromptSection(tools.Context{})
	if err != nil {
		t.Fatal(err)
	}
	if empty != "" {
		t.Errorf("没有技能时不该有提示段落：%q", empty)
	}
}

func TestResolveDirs_相对路径按安装根(t *testing.T) {
	t.Setenv("FKA_SKILLS_DIR", "")
	if got := ResolveDirs(); len(got) != 1 || got[0] != filepath.Join(os.Getenv("HOME"), "x") &&
		!strings.HasSuffix(got[0], filepath.Join("skills")) {
		t.Errorf("默认应是安装根下的 skills：%v", got)
	}

	abs := t.TempDir()
	t.Setenv("FKA_SKILLS_DIR", abs)
	if got := ResolveDirs(); len(got) != 1 || got[0] != abs {
		t.Errorf("绝对路径应原样使用：%v", got)
	}

	// 多个目录，用系统路径分隔符隔开
	other := t.TempDir()
	t.Setenv("FKA_SKILLS_DIR", abs+string(os.PathListSeparator)+other)
	if got := ResolveDirs(); len(got) != 2 || got[0] != abs || got[1] != other {
		t.Errorf("多个目录：%v", got)
	}
}
