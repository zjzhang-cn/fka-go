// Package mcp 的边界测试：本目录树**不许依赖 agent 的 internal 包**。
//
// ## 为什么这条要专门用测试守
//
// Go 的 `internal` 规则是**单向**的。`fka-go/mcp/internal/...` 只有 `mcp/` 树能
// import，这条编译器已经管住了。但反过来——`mcp/docs` 完全可以 import
// `fka-go/internal/...`，因为它同样在 `fka-go/` 之下，**编译器不会报错**。
//
// 而那正是这里要防的事：两个 server 要的是「自己拥有数据的独立进程」，不是
// 「伸手进 agent 内部拿点东西」。一旦 `mcp/docs` 引用了 `internal/config` 之类，
// 这个 server 就跟 agent 的装配、配置、生命周期绑在一起了——它不再是能被
// 单独构建、单独发布、单独换掉的**能力**。
//
// 所以这条边界由测试守，而不是由命名约定守。
package mcp

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// agentInternalPrefix agent 侧 internal 包的 import 前缀。
//
// **注意**这和 `mcp/internal/` 不是一回事：agent 的 internal 是 agent、app、
// channels、config、llm、prompts、tools 这些东西，与 MCP server 无关。
const agentInternalPrefix = "github.com/zjzhang-cn/fka-go/internal/"

// TestMcp不依赖AgentInternal 扫本目录树下所有 .go 文件的 import，出现
// agent internal 的直接失败，并把**哪个文件的哪一行**报出来——不然只有一个
// 「不许 import」的断言，排查得自己去找。
func TestMcp不依赖AgentInternal(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("拿不到工作目录：%v", err)
	}

	fset := token.NewFileSet()
	var violations []string
	files := 0

	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// 跳过数据目录与构建产物：里面不会有 Go 源码
			switch entry.Name() {
			case "bin", "data", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++

		parsed, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			// 解析不了是**代码本身坏了**，不是边界问题，让它以解析错误的形式冒出来
			return err
		}

		rel, _ := filepath.Rel(root, path)
		for _, spec := range parsed.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			if strings.HasPrefix(imported, agentInternalPrefix) {
				violations = append(violations,
					rel+":"+itoa(fset.Position(spec.Pos()).Line)+" 引用了 "+imported)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫目录失败：%v", err)
	}

	if files == 0 {
		t.Fatalf("一个 .go 文件都没扫到——测试本身失效了（工作目录 %s）", root)
	}
	if len(violations) > 0 {
		t.Errorf("MCP server 不许依赖 agent 的 internal 包：\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// Test两个Server自成一体 钉住「每个 server 都是一个独立 main 包」这件事——
// 它们是**独立进程**，不是 agent 的一部分。
//
// 少了这个断言，一个 server 哪天长成了普通库函数、被人从 agent 里直接调用，
// 那「能力通过 MCP 进来」就名存实亡，而上面那条 import 检查还照样通过。
func Test两个Server自成一体(t *testing.T) {
	for _, name := range []string{"docs", "memory"} {
		dir := filepath.Join(".", name)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("读 %s 失败：%v", dir, err)
		}

		found := false
		for _, entry := range entries {
			if entry.Name() != "main.go" {
				continue
			}
			found = true
			fset := token.NewFileSet()
			parsed, err := parser.ParseFile(fset, filepath.Join(dir, "main.go"), nil, parser.PackageClauseOnly)
			if err != nil {
				t.Fatalf("解析 %s/main.go 失败：%v", dir, err)
			}
			if parsed.Name.Name != "main" {
				t.Errorf("%s/main.go 的包名是 %s，server 必须是 main 包", dir, parsed.Name.Name)
			}
		}
		if !found {
			t.Errorf("%s 没有 main.go —— 它得是一个能单独跑起来的进程", dir)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := make([]byte, 0, 12)
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
