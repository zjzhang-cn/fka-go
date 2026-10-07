package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Test控制台日志只写stderr CLI 的 stdout 是**结果**（`fka tools --json` 的第一个
// 字节必须是 `{`、`fka ask` 的第一行必须是答案），日志不是。
//
// ## 这条为什么必须钉在源码上
//
// `write` 的旧实现是 `level >= Error → stderr，否则 → stdout`。看着合理，却让
// `LLM_TOOL_EFFECTS` 的一个笔误、或某个 MCP server 连不上所记下的那条 WARN
// （默认控制台级别**本来就放行** WARN）顶在结果前面，而**退出码仍是 0**——
// 调用方只看到「JSON 解析失败」。
//
// 这类「写错了流」的回归用行为用例很难钉：抓控制台要换掉 `os.Stdout`，而
// `write` 与恢复函数会同时读写那个变量，`-race` 会当场报出来（同包
// `renderLine` 那段注释说的就是这件事）。所以这里**扫源码**——与
// `internal/channels/channels_test.go`、`mcp/memory/boundary_test.go` 同一个
// 思路：编译器管不了「写到哪条流」，那就让测试管。
//
// 真正的端到端闸门在 `make smoke`：独立进程里、故意配一个笔误的
// `LLM_TOOL_EFFECTS` 跑 `fka tools --json`，断言首字节仍是 `{`。
func Test控制台日志只写stderr(t *testing.T) {
	// 默认落点就是 stderr（生产路径不注入，见 consoleStream）
	if got := consoleStream(); got != os.Stderr {
		t.Fatalf("consoleStream() = %v，期望 os.Stderr", got)
	}

	// 整个 config 包不许出现 os.Stdout：日志的唯一出口是 consoleStream()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读包目录失败：%v", err)
	}
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("读 %s 失败：%v", name, err)
		}
		checked++
		if strings.Contains(stripLineComments(string(data)), "os.Stdout") {
			t.Errorf("%s 里出现了 os.Stdout——CLI 的 stdout 是结果输出，日志一律走 "+
				"consoleStream()（stderr）。要往 stdout 写东西，先想清楚它算结果还是算日志", name)
		}
	}
	if checked == 0 {
		t.Fatal("一个非测试 .go 文件都没扫到，这条用例没真跑")
	}
}

// stripLineComments 去掉 `//` 起的行内注释。
//
// **注释里提到 `os.Stdout` 是合法的**——`logger.go` 就在解释「换掉 os.Stdout
// 抓输出那种测法有竞态」。不剥注释的话这条用例会因为一句解释而红，
// 而红在一个**说法**上比红在一个真写错流上更容易被直接改掉而不是修好。
func stripLineComments(src string) string {
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		if at := strings.Index(line, "//"); at >= 0 {
			lines[i] = line[:at]
		}
	}
	return strings.Join(lines, "\n")
}
