// 本文件钉「推理片段写到哪儿」。
//
// 这不是小事：以前它直接 `fmt.Print` 到**进程 stdout**，而 stdout 是 CLI 的结果通道
// （`fka ask` 的答案、`fka tools --json` 的 JSON）——于是 `fka ask > 答案.txt` 里
// 混着半截推理，**退出码还是 0**，脚本拿到一份既不是日志也不是结果的东西。
package openai

import (
	"bytes"
	"os"
	"testing"
)

// Test推理写注入的writer 落点由装配根决定（`internal/app` 注入 stderr）。
func Test推理写注入的writer(t *testing.T) {
	t.Setenv("LLM_SHOW_REASONING", "")

	var got bytes.Buffer
	sink := newReasoningSink(&got)
	sink("先想想")
	sink("再想想")
	sink("\n") // 流结束时喂的换行，见 postCompletion

	if want := "[推理] 先想想再想想\n"; got.String() != want {
		t.Errorf("推理输出 = %q，期望 %q（首块一次前缀，之后原样）", got.String(), want)
	}
}

// Test推理可以关掉 重定向日志、CLI 或不想看的部署用 LLM_SHOW_REASONING=0。
func Test推理可以关掉(t *testing.T) {
	t.Setenv("LLM_SHOW_REASONING", "0")

	var got bytes.Buffer
	sink := newReasoningSink(&got)
	sink("不该出现")

	if got.Len() != 0 {
		t.Errorf("关掉之后不该写任何东西，实际 %q", got.String())
	}
}

// Test默认推理写stderr而不是stdout 没注入 writer 时的兜底。
//
// **库代码绝不能默认写 stdout**：这条是这个字段存在的全部理由，所以它值得一条用例
// ——将来有人「顺手改回 fmt.Print」时，红的是这条。
func Test默认推理写stderr而不是stdout(t *testing.T) {
	if writer := (Provider{}).reasoningWriter(); writer != os.Stderr {
		t.Errorf("没注入时该写 stderr，实际 %T", writer)
	}
}
