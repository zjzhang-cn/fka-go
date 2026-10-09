package llm

import (
	"context"
	"io"
)

// reasoningWriterKey 承载「这一轮的推理增量写到哪」，见 WithReasoningWriter。
type reasoningWriterKey struct{}

// WithReasoningWriter 把推理增量的落点绑到 ctx 上。
//
// provider **优先用它**，其次才是自己配置里的 writer。这样**每一轮**都能决定推理
// 去哪（chat 画带色的、ask 打 `[推理]`、渠道记日志或干脆不看），不必在装配期把落点
// 定死在一处。前端通常不直接调它——它由工具循环在调模型前、根据 `agent.Emitter`
// 绑好。
func WithReasoningWriter(ctx context.Context, w io.Writer) context.Context {
	return context.WithValue(ctx, reasoningWriterKey{}, w)
}

// ReasoningWriter 取这一轮的推理落点。没绑定过时返回 false，provider 退回自己的配置。
func ReasoningWriter(ctx context.Context) (io.Writer, bool) {
	w, ok := ctx.Value(reasoningWriterKey{}).(io.Writer)
	return w, ok
}
