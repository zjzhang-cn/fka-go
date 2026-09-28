package skills

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// TestDiscover和模型看到的是同一份 `fka tools` 要显示「有哪些技能」，
// 而模型看到的是源自己的扫描结果。**两条路径必须一致**——
// 不一致的话就会出现「清单里没有、模型却说有」。
func TestDiscover和模型看到的是同一份(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "backup", "---\nname: 备份\ndescription: 归档照片\n---\n步骤")
	writeSkill(t, dir, "broken", "没有 front matter，就是正文。")

	found := Discover([]string{dir})
	if len(found) != 2 {
		t.Fatalf("该发现两个技能，实际 %d：%+v", len(found), found)
	}

	// 走一遍模型那条路：经由源的 PromptSection
	section, err := NewSource([]string{dir}).PromptSection(toolsContext())
	if err != nil {
		t.Fatal(err)
	}
	for _, skill := range found {
		if !containsText(section, skill.Name) {
			t.Errorf("Discover 找到的 %q 不在模型看到的清单里：\n%s", skill.Name, section)
		}
	}
}

func TestDiscover空目录返空(t *testing.T) {
	if found := Discover([]string{t.TempDir()}); len(found) != 0 {
		t.Errorf("该是空的，实际 %+v", found)
	}
	// **目录不存在也该是空的，不是错**——那是「还没建 skills 目录」的正常状态
	if found := Discover([]string{filepath.Join(t.TempDir(), "没有这个")}); len(found) != 0 {
		t.Errorf("目录不存在该返空，实际 %+v", found)
	}
}

// TestDiscover带目录名 因为 front matter 里的 name 可能与目录名不同，
// 而 skills__load 两个都认——**清单上要能看出自己建的是哪个**。
func TestDiscover带目录名(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "photo-album", "---\nname: 报销流程\ndescription: 走完每一步\n---\n步骤")

	found := Discover([]string{dir})
	if len(found) != 1 {
		t.Fatalf("该发现一个，实际 %d", len(found))
	}
	if found[0].Name != "报销流程" {
		t.Errorf("Name = %q", found[0].Name)
	}
	if found[0].Dir != "photo-album" {
		t.Errorf("Dir = %q，该是目录名", found[0].Dir)
	}
}

func toolsContext() tools.Context { return tools.Context{} }

func containsText(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
