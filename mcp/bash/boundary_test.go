// bash server 的边界测试：**这个 server 自给自足**。
//
// ## 为什么这条要专门用测试守
//
// Go 的 `internal` 规则已经保证了「`mcp/bash/internal/...` 只有 bash 树能 import」——
// 那是编译器的事，本测试不重复。
//
// 但它**没有**保证「bash 不 import 树外的东西」：`mcp/bash` 完全可以 import
// `fka-go/internal/config` 或别的 server 的包而不报错（它们都在 `fka-go/` 之下）。
// 而那正是要防的——一旦伸手，这个 server 就跟别人的装配、配置、生命周期绑在一起，
// 它不再是「能单独构建、部署、换掉的能力」。
//
// 代价很低：扫一遍 import 而已。
package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestBash不依赖树外任何包 bash 的每个 .go 文件都只许 import：
//
//   - 标准库；
//   - `fka-go/mcp/bash/...`（**它自己**的 internal）；
//   - 第三方库（mark3labs —— 它不认识本仓库的任何一个包）。
//
// 出现别的 `fka-go/...` 路径就失败，并报出**哪个文件第几行**。
func TestBash不依赖树外任何包(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("拿不到工作目录：%v", err)
	}

	const ownPrefix = "github.com/zjzhang-cn/fka-go/mcp/bash"
	const repoPrefix = "github.com/zjzhang-cn/fka-go"

	fset := token.NewFileSet()
	var violations []string
	files := 0

	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
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
			return err
		}

		rel, _ := filepath.Rel(root, path)
		for _, spec := range parsed.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			// 只看本仓库的路径：标准库与第三方库不认识我们，拦不住也没必要拦
			if !strings.HasPrefix(imported, repoPrefix) {
				continue
			}
			if strings.HasPrefix(imported, ownPrefix) {
				continue
			}
			violations = append(violations,
				rel+":"+itoa(fset.Position(spec.Pos()).Line)+" 引用了 "+imported)
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
		t.Errorf("bash server 必须自给自足，不许依赖树外的包：\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// TestServer是一个可执行程序 钉住「它是**独立进程**」这件事——不是 agent 的一部分，
// 也不是某个库里的一个函数。
func TestServer是一个可执行程序(t *testing.T) {
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "main.go", nil, parser.PackageClauseOnly)
	if err != nil {
		t.Fatalf("解析 main.go 失败：%v", err)
	}
	if parsed.Name.Name != "main" {
		t.Errorf("main.go 的包名是 %s，必须是 main 包", parsed.Name.Name)
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
