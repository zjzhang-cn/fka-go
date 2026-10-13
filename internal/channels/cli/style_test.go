package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Test颜色开关_第一条命中说了算：参数 > 环境变量 > 自动探测。顺序反了会出现
// 「谁说了算」的不确定，比如 `--no-color` 被一个 FKA_COLOR=always 顶回去。
func Test颜色开关_第一条命中说了算(t *testing.T) {
	// 一个普通文件：**不是**字符设备，所以自动探测那条一律为假
	plain, err := os.Create(filepath.Join(t.TempDir(), "not-a-tty"))
	if err != nil {
		t.Fatalf("建临时文件失败：%v", err)
	}
	defer plain.Close()

	for name, tc := range map[string]struct {
		noColorFlag bool
		env         map[string]string
		want        bool
	}{
		"都不是就不上色": {false, map[string]string{}, false},
		"NO_COLOR 设置了就关": {
			false, map[string]string{"NO_COLOR": "1"}, false,
		},
		"NO_COLOR 空值也要关":   {false, map[string]string{"NO_COLOR": ""}, false},
		"TERM=dumb 就关":     {false, map[string]string{"TERM": "dumb"}, false},
		"FKA_COLOR=always": {false, map[string]string{"FKA_COLOR": "always"}, true},
		"FKA_COLOR=never":  {false, map[string]string{"FKA_COLOR": "never"}, false},
		"参数优先于环境变量": {
			true, map[string]string{"FKA_COLOR": "always"}, false,
		},
		"never 压过 NO_COLOR": {
			false, map[string]string{"FKA_COLOR": "never", "NO_COLOR": ""}, false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			// t.Setenv 会自动还原；先用空串把可能残留的键清掉
			for _, key := range []string{"NO_COLOR", "TERM", "FKA_COLOR"} {
				t.Setenv(key, "")
			}
			os.Unsetenv("NO_COLOR")
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			if got := ShouldColor(plain, tc.noColorFlag); got != tc.want {
				t.Errorf("ShouldColor = %v，想要 %v", got, tc.want)
			}
		})
	}
}

// Test调色板关掉时原样返回：关掉不是「少几个转义」而是**一个都不许有**。
// 每个方法各自判一次开关的话，漏掉的那处会把 `\x1b[0m` 写进重定向的文件。
func Test调色板关掉时原样返回(t *testing.T) {
	off := NewPalette(false)
	on := NewPalette(true)

	if got := off.Err("坏了"); got != "坏了" {
		t.Errorf("关掉时 Err = %q，想要原样", got)
	}
	for _, got := range []string{on.Err("坏了"), on.Assistant("助手"), on.User("> "), on.Tool("x"), on.Dim("y")} {
		if !strings.Contains(got, "\x1b[") || !strings.HasSuffix(got, ansiReset) {
			t.Errorf("开启时应当包一层转义并以 reset 收尾：%q", got)
		}
	}
}

// Test压行_换行变空格并截到60字 工具参数与结果动辄上千字，全量铺开会把对话淹掉。
func Test压行_换行变空格并截到60字(t *testing.T) {
	got := firstLine("第一行\n第二行\n第三行")
	if strings.ContainsAny(got, "\n") {
		t.Errorf("换行该压成空格：%q", got)
	}

	long := strings.Repeat("长", 80)
	clipped := firstLine(long)
	if !strings.HasSuffix(clipped, "…") {
		t.Errorf("超长该以省略号收尾：%q", clipped)
	}
	if runes := []rune(clipped); len(runes) != 61 {
		t.Errorf("该截到 60 字 + 一个省略号，实际 %d 字：%q", len(runes), clipped)
	}
}

// Test非字符设备一律为假 普通文件、nil 都不是终端，IsCharDevice 必须回 false——
// 它决定「要不要上色」与「要不要起 readline」，判错会把转义写进文件。
func Test非字符设备一律为假(t *testing.T) {
	plain, err := os.Create(filepath.Join(t.TempDir(), "not-a-tty"))
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()

	if IsCharDevice(plain) {
		t.Error("普通文件不该被判成终端")
	}
	if IsCharDevice(nil) {
		t.Error("nil 不该被判成终端")
	}
}
