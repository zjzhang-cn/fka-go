// ## 这个文件守的是什么
//
// `llm` 是接缝，`llm/openai` 是它目前唯一的实现。接缝的价值全在「换一个实现时别的包
// 一个字不改」上。这条边**真的长过**：`internal/agent/loop.go` 曾为了一个常量
// `MaxAnswerTokens` import `llm/openai`，而那个常量已经搬进 `llm`。
//
// ## 为什么只扫 agent，不扫 llm 自己
//
// **Go 的导入环规则已经挡住了接缝伸手的那一半。** `llm/openai` import `llm`，所以
// `llm` import `llm/openai` 是环，编译器直接报错——扫它是白扫，一条测试永远绿。
//
// 拦不住的是**兄弟包伸手**：`internal/agent` 与 `llm/openai` 之间没有环，import 合法、
// 编译通过、运行正常，只是从那天起工具循环再也不能换模型实现了。这才是静默失效的
// 形状，也是这里唯一要守的边。
//
// （同样的道理：`internal/tools` 接缝**不需要**这类测试——`tools/mcp` 与
// `tools/skills` 都 import `tools`，反向 import 会成环。而 `internal/channels` 那边的
// 测试是活的，因为 `channels/ilink/bot` 不 import `channels`，接缝 import 它能编过。）
//
// ## 刻意只看非测试文件
//
// 用例里 import 实现来做夹具是合理的，handler_test.go 就造了个假 agent。
// 要拦的是生产代码。
package llm

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// implementationPrefix 具体模型实现的 import 前缀。加新实现时这里不用改——
// 子目录名天然被前缀覆盖。
const implementationPrefix = "github.com/zjzhang-cn/fka-go/internal/llm/openai"

// TestBoundary_业务层不import具体实现 见包头：这条边合法、编译器不管、且真的长过。
func TestBoundary_业务层不import具体实现(t *testing.T) {
	dir := "../agent"

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读 %s 失败：%v", dir, err)
	}

	seen := false
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		seen = true

		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("解析 %s 失败：%v", filepath.Join(dir, name), err)
		}

		for _, spec := range file.Imports {
			if path := strings.Trim(spec.Path.Value, `"`); strings.HasPrefix(path, implementationPrefix) {
				t.Errorf("%s import 了具体实现 %q——换一个模型实现时这个包就得跟着改",
					filepath.Join(dir, name), path)
			}
		}
	}

	if !seen {
		t.Fatalf("%s 下没有扫到任何非测试 Go 文件——这条约束可能已经扫空了", dir)
	}
}

// TestProvider契约没有多余方法 接口瘦身的依据。
//
// 这条不是架构约束，是**体检**：上一版 Provider 有 7 个方法，其中 IsDefault / Probe /
// Label / CreateComposer 四个零调用方——而「每个新 provider 都得实现一遍没人调的方法」
// 正是可替换性最大的反作用力。所以方法与允许名单一起钉在这里。
func TestProvider契约没有多余方法(t *testing.T) {
	// 契约上刻意只留这三个。见 types.go 的 Provider 注释。
	allowed := map[string]bool{
		"ID":         true,
		"ReadConfig": true,
		"CreateChat": true,
	}

	// 接口类型：Elem() 从「*Provider」取出 Provider 本身
	contract := reflect.TypeOf((*Provider)(nil)).Elem()

	for i := 0; i < contract.NumMethod(); i++ {
		name := contract.Method(i).Name
		if !allowed[name] {
			t.Errorf("Provider.%s 不在允许名单里——加方法前先找到活着的调用方", name)
		}
	}
	if contract.NumMethod() != len(allowed) {
		t.Errorf("Provider 有 %d 个方法，允许名单是 %d 个——方法与名单要一一对应",
			contract.NumMethod(), len(allowed))
	}
}
