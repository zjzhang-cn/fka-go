// 本文件守的是**契约的宽度**：每个方法都要能回答「谁在调它」。
//
// ## 为什么要有这条
//
// `internal/llm/boundary_test.go` 那条「Provider 契约没有多余方法」是这个想法的原型：
// 上一版 `llm.Provider` 有 7 个方法，其中 4 个零调用方——而「每个新实现都得实现一遍
// 没人调的方法」正是可替换性最大的反作用力。
//
// 同一条纪律以前**没有**施加到渠道接缝上，于是它长到了 11 个方法，其中好几个在生产
// 代码里根本没有调用方（为「已搬走的存储那半边」留的 `StorageID`、给控制面留的
// `StartedAt`、以及从未被断言的 `Find`/`ByAccount`/`Providers`/`nowMillis`）。
// 那些方法在 2026-09 的那一轮重构里删掉了，这条用例是防止它们长回来的那道闸门。
//
// ## 手段
//
// 反射拿接口的全部方法（含嵌入的），逐个到**生产代码**里找调用点（`sel.Name(`）。
// 找不到就必须出现在下面的 `pendingConsumers` 里，并写清谁会用它的哪一项待办。
//
// ## 它不是万无一失的
//
// `sel.Name(` 是**文本匹配**：别的类型上同名的方法也算数（`.Status(` 可能是 provider
// 的）。它抓的是最常见的那种腐化——**一个从来没人调过的方法**，而不是「这次调用来自
// 谁」。真要精确到类型，得做完整的类型检查（`go/types`），而代价与收益不成比例。
package channels

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// pendingConsumers 现在**没有生产调用方、但明确有计划**的方法。
//
// 空着是常态。往里加一条 = 承认「这个契约暂时没有消费者」，并写清谁会用它的哪一项
// 待办（见 `docs/port-plan.md` 的「剩下的待办」）。**新增契约方法不该是无声的**——
// 这条用例的全部意义就在这行字上：让「加了一个没人调的方法」必须是一次显式决定，
// 而不是顺手。
var pendingConsumers = map[string]string{
	"Status": "port-plan 待办 #3（运维端口的状态出口）。注意它现在与 `Provider.Ops().Status()` " +
		"是两条路：后者读账号表、已实现；这条是渠道自己的视图，还没有消费者。",
	"FetchMedia": "port-plan 已知的债「iLink 的媒体路径」。消息层现在收到媒体会如实拒答" +
		"（agent 不拥有存储），要读图时接的就是这个可选能力。",
}

// TestContract_渠道接缝的每个方法都有活着的调用方 见文件头。
func TestContract_渠道接缝的每个方法都有活着的调用方(t *testing.T) {
	root := repoRoot(t)

	contracts := map[string]reflect.Type{
		"Channel":         reflect.TypeOf((*Channel)(nil)).Elem(),
		"AddressResolver": reflect.TypeOf((*AddressResolver)(nil)).Elem(),
		"MediaFetcher":    reflect.TypeOf((*MediaFetcher)(nil)).Elem(),
	}

	// 先把「名单里的方法真的存在」验一遍，免得名单自己烂掉（删了方法而留着条目）
	known := map[string]bool{}
	for name, contract := range contracts {
		for i := 0; i < contract.NumMethod(); i++ {
			method := contract.Method(i).Name
			known[method] = true

			called := hasProductionCallSite(t, root, method)
			reason := pendingConsumers[method]

			switch {
			case called && reason != "":
				t.Errorf("%s.%s 在生产代码里**有**调用方了，把它从 pendingConsumers 里删掉"+
					"（名单只记真的没人用的）", name, method)
			case !called && reason == "":
				t.Errorf("%s.%s 在生产代码里没有调用方：要么删掉它，要么在 pendingConsumers 里"+
					"写清谁会用它的哪一项待办", name, method)
			}
		}
	}

	for method := range pendingConsumers {
		if !known[method] {
			t.Errorf("pendingConsumers 里的 %q 不是这几个接口上的方法了——名单要跟着契约走", method)
		}
	}
}

// TestContract_扫描器本身是活的 上面那条用例的全部价值都押在
// `hasProductionCallSite` 上：它要是永远返回 true（或永远返回 false），那条用例
// 就成了摆设——一个永远通过、一个永远误报。
//
// 所以这里用「一定没有的名字」与「一定有调用方的名字」把它两头钉住。
// 与本仓库其它边界测试里那句 `files == 0` 是同一个用意。
func TestContract_扫描器本身是活的(t *testing.T) {
	root := repoRoot(t)

	if hasProductionCallSite(t, root, "这个标识符不可能存在NoSuchMethodName") {
		t.Error("扫出了一个不存在的名字——扫描器坏了，上一条用例等于永远通过")
	}
	// `Senders` 在 `internal/messages/handler.go` 与接缝里都有调用点
	if !hasProductionCallSite(t, root, "Senders") {
		t.Error("扫不出确实存在的调用点（Senders）——扫描器坏了，上一条用例会误报")
	}
}

// hasProductionCallSite 生产代码里有没有 `sel.方法名(` 这样的调用点。
//
// **只扫非测试文件**：测试里调它不算「有人用」——那正是这条用例要区分的事
// （一个只有测试在调的方法，就是没人用）。
func hasProductionCallSite(t *testing.T, root, method string) bool {
	t.Helper()

	pattern := regexp.MustCompile(`\.` + regexp.QuoteMeta(method) + `\(`)
	found := false

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case "bin", "data", "logs", ".smoke", ".git", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if pattern.Match(source) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫目录失败：%v", err)
	}
	return found
}

// repoRoot 从本包往上找到仓库根（认 go.mod）。**不用写死的 `../..`**：
// 层级写错时那条真库用例已经吃过一次亏（静默跳过），这里宁可提前报错。
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("拿不到工作目录：%v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("往上找了 6 层也没找到 go.mod（起点 %s）", dir)
	return ""
}
