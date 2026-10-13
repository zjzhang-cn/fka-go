// 本文件是 CLI 渠道的**回显**：把工具循环推来的过程事件画到 UI（stderr）上。
//
// ## 答案不在这里写
//
// 答复走**唯一那条答案通道**——渠道的 `Senders().Text`（`channel.go` 的 `say`）。
// 所以这里的 `Answer` 是空的：答案若也从这里写，就会与 `say` 各写一份，界面上出现
// 两次。web 渠道早就是这个口径。
//
// ## 两种风格
//
//   - ask：`[推理] …` / `[工具] 名字（参数）→ 结果`（方括号行，不上色）；
//   - chat：暗色 `[推理] ` / 黄色 `· 名字（参数）→ 结果`（带色圆点行）。
//
// 风格差异只在这里，工具循环与消息层一行不用改——这正是 `channels.Emitter` 这层
// 接缝想达到的效果。
package cli

import (
	"fmt"
	"io"

	"github.com/zjzhang-cn/fka-go/internal/channels"
)

// echo 两种风格共用的回显实现。
type echo struct {
	ui      io.Writer
	palette Palette
	mode    Mode

	reasoningStarted bool
}

func newEcho(ui io.Writer, palette Palette, mode Mode) *echo {
	return &echo{ui: ui, palette: palette, mode: mode}
}

// Reasoning 推理增量。首块打一次 `[推理] ` 前缀，之后逐块原样；流结束会来一个
// 换行把这一行收掉（见 openai 的 reasoningSink）。
func (e *echo) Reasoning(text string) {
	if text == "" {
		return
	}
	if !e.reasoningStarted {
		fmt.Fprint(e.ui, e.palette.Dim("[推理] "))
		e.reasoningStarted = true
	}
	fmt.Fprint(e.ui, e.palette.Dim(text))
}

// Tool 一次工具调用。**原样参数不解析**——解析再序列化会改字节序（见 llm.ToolCall）。
func (e *echo) Tool(name, arguments, result string) {
	if e.mode == ModeAsk {
		fmt.Fprintln(e.ui, askToolLine(name, arguments, result))
		return
	}
	call := e.palette.Tool(name)
	if args := firstLine(arguments); args != "" {
		call += e.palette.Dim("（" + args + "）")
	}
	fmt.Fprintf(e.ui, "%s %s %s\n", e.palette.Tool("·"), call, e.palette.Dim("→ "+firstLine(result)))
}

// Answer 最终答案**不在这里写**，见文件头。方法必须存在（`channels.Emitter` 的
// 契约），但什么都不做。
func (e *echo) Answer(string) {}

// askToolLine 把一次工具调用拼成 `[工具] 名字（原样参数）→ 结果`。
// 三者都经 firstLine 压成一行（与 chat 的显示同口径）。
func askToolLine(name, arguments, result string) string {
	call := "[工具] " + name
	if args := firstLine(arguments); args != "" {
		call += "（" + args + "）"
	}
	return call + " → " + firstLine(result)
}

// 确保 echo 满足渠道的回显契约。
var _ channels.Emitter = (*echo)(nil)
