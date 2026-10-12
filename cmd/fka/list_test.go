package main

import "testing"

// TestListKind_归一 单复数都收，大小写不敏感：敲 `fka list Tools` 也该能用。
func TestListKind_归一(t *testing.T) {
	cases := map[string]string{
		"skill":     "skill",
		"skills":    "skill",
		"tool":      "tool",
		"tools":     "tool",
		"resource":  "resource",
		"resources": "resource",
		"prompt":    "prompt",
		"prompts":   "prompt",
		"server":    "server",
		"servers":   "server",
		"mcp":       "server",
		"all":       "all",
		"ALL":       "all",
	}
	for input, want := range cases {
		got, err := listKind(mustParse(t, "list", input))
		if err != nil {
			t.Errorf("list %s 该认得出：%v", input, err)
			continue
		}
		if got != want {
			t.Errorf("list %s = %q，期望 %q", input, got, want)
		}
	}
}

// TestListKind_取错 缺类型 / 不认识的类型 / 多给类型都该报错（CLI 据此以 2 退出）。
func TestListKind_取错(t *testing.T) {
	for _, args := range [][]string{
		{"list"},
		{"list", "foo"},
		{"list", "tool", "extra"},
	} {
		if _, err := listKind(mustParse(t, args...)); err == nil {
			t.Errorf("%v 该报用法错", args)
		}
	}
}
