// 本文件是消息层的**同步编排入口**：把一条消息跑成一次问答的**结论**——
// 结果、该说给用户的那句话、以及调用失败的原因——但不发送、不呈现。
//
// ## 为什么把「判定」从 Handle 里抽出来
//
// 以前这段判定只长在 `Handle` 里，于是 `cmd/fka` 的 `ask` / `chat` 各自又抄了一份，
// 措辞还不一样（空答复一边写「我没想出该怎么回」，一边输出一个空行）。抽成 `Ask`
// 之后，「没接模型 / 模型出错 / 空答复」这些规则只有这一处；CLI 与渠道拿到的
// 是同一份 `Explain`。
//
// ## 为什么不在这里发送
//
// 发送要拿渠道的发送器，而 CLI 的「渠道」在 cmd 侧才组装得出来（展开 `@引用`、
// 决定退出码）。`Ask` 只负责编排与归类，把「怎么说」留给调用方，`Deliver` 只管发。
package messages

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zjzhang-cn/fka-go/internal/agent"
	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/llm"
)

// AskInput 一次同步问答要绑给编排入口的东西。
type AskInput struct {
	// Channel 这一轮的答复与回话走哪个渠道
	Channel channels.Channel
	// Message 本轮问题与它的归属（账号、身份、会话、回复令牌都在里面）
	Message channels.InboundMessage
	// SessionID 覆盖渠道协议算出的会话键。**CLI 用**：`--session` / `cli-<uuid>`；
	// 渠道不传，按 `渠道:账号:会话` 算。
	SessionID string
	// Ephemeral 这一轮不进也不出会话历史。CLI 不给 `--session` 的一次性问答用。
	Ephemeral bool
	// Images 本轮问题附带的图片（CLI 的 `@图片`）。只在这一轮发给模型。
	Images []llm.ImageAttachment
}

// AskOutcome 一次同步问答的归类结果。
type AskOutcome struct {
	// Result 答出来时的循环结果（含步数、工具、用量）
	Result agent.RunResult
	// Explain 没答出来时该说给用户的那句话（空问题 / 缺身份 / 缺模型 / 模型出错 /
	// 空答复，逐情形用现有措辞）。答出来时为空。
	Explain string
	// Cause 模型/调用失败的原因；拒答类为零值。调用方据此决定退出码。
	Cause error
}

// Text 这一轮该回给用户的正文：没答出来就是 Explain（可能是空答复的诚实句），
// 答出来了就是结果正文。**发送方用这一个入口**——直接读 Result.Text 会在
// 空答复时发出一段空串。
func (o AskOutcome) Text() string {
	if o.Explain != "" {
		return o.Explain
	}
	return o.Result.Text
}

// Ask 跑一轮问答的编排与归类，**不发送、不呈现**。
//
// 绑定 ctx（消息层五字段，与 Handle 同款）之后逐条判定，再交给工具循环；失败与
// 拒答都变成 `AskOutcome`，不往上抛——调用方在长驻循环里，抛出去会中断后续消息。
func (h *Handler) Ask(ctx context.Context, in AskInput) AskOutcome {
	message := in.Message
	channel := in.Channel

	// **归属绑在 ctx 上，往下传**：工具循环、模型请求、工具调用里的每条日志都会
	// 自动带上账号与消息号（见 `internal/config/scope.go`）。渠道的 Handle 也绑过，
	// 再绑一次只是合并同一组字段，无害。
	ctx = config.Bind(ctx, messageFields(message))
	fields := config.FieldsOf(ctx)

	// ── 没有可答的内容，先说清楚 ────────────────────────────
	question := strings.TrimSpace(message.Text())
	if question == "" {
		config.Log().Info(config.TypeMSG, "收到不能作答的消息", fields)
		return AskOutcome{Explain: replyNotText}
	}

	// ── 身份缺失就别跑 ──────────────────────────────────────
	//
	// 工具靠 PrincipalID 做权限过滤，而那个值是**模型填的参数**（见
	// tools.Context.PrincipalID）。缺了它，模型就得自己编一个——那等于没有过滤。
	if strings.TrimSpace(message.PrincipalID) == "" {
		config.Log().Warn(config.TypeMSG, "入站消息没有全局身份，已拒答", fields)
		return AskOutcome{Explain: "这条消息里认不出你是谁，我不敢替你回答。"}
	}

	// 会话键：CLI 显式给的优先，否则按渠道协议算 `渠道:账号:会话`。
	sessionID := in.SessionID
	if sessionID == "" {
		sessionID = channels.SessionKeyOf(&message)
	}

	// ── 跑一轮 ─────────────────────────────────────────────
	result, err := h.Runner.Run(ctx, agent.RunnerInput{
		SessionID:   sessionID,
		PrincipalID: message.PrincipalID,
		TurnID:      message.MessageID,
		Question:    question,
		QuotedText:  quotedTextOf(message),
		Reply:       newChannelReply(channel, message),
		Emitter:     emitterFor(ctx, channel),
		Ephemeral:   in.Ephemeral,
		Images:      in.Images,
	})
	if err != nil {
		// **工具循环缺席要说清是缺席**，不能混进「这一轮没答出来」——用户看到
		// 「稍后再试」只会再问一次，而真因是没配模型或 LLM_TOOLS=off。
		if errors.Is(err, agent.ErrNoRunner) {
			config.Log().Warn(config.TypeMSG, "工具循环缺席，已回一句说明", fields)
			return AskOutcome{Explain: replyNoLLM}
		}
		// 模型的错如实报，**不静默降级**：「稍后再试」会把「接口没配好」
		// 伪装成「模型偶尔不回」
		config.Log().Error(config.TypeMSG, "问答失败", mergeFields(fields, config.Context{"error": err.Error()}))
		return AskOutcome{Explain: fmt.Sprintf(replyAgentFail, err.Error()), Cause: err}
	}

	result.Text = strings.TrimSpace(result.Text)
	if result.Text == "" {
		config.Log().Warn(config.TypeMSG, "模型没有给出正文", mergeFields(fields, config.Context{
			"steps": result.Steps, "stoppedBy": result.StoppedBy,
		}))
		return AskOutcome{Result: result, Explain: replyEmpty}
	}

	// 用量字段**有值才带**（服务端不给就没有这几个数）。serve 侧的「已作答」
	// 因此能直接看到这一轮真实花了多少。
	answered := config.Context{
		"steps": result.Steps, "stoppedBy": result.StoppedBy,
		"tools": strings.Join(result.UsedTools, "、"),
	}
	if !result.Usage.IsZero() {
		answered["tokensIn"] = result.Usage.PromptTokens
		answered["tokensOut"] = result.Usage.CompletionTokens
		answered["tokensTotal"] = result.Usage.TotalTokens
	}
	config.Log().Info(config.TypeMSG, "已作答", mergeFields(fields, answered))
	return AskOutcome{Result: result}
}

// messageFields 一条消息的归属——日志、会话键与工具身份都从这几个字段来。
// **只有这一处**拼它们：以前 `Handle` 内联了一份，新加日志点的人不必再抄一遍。
func messageFields(message channels.InboundMessage) config.Context {
	return config.Context{
		"channel": message.ChannelID, "account": message.AccountID,
		"messageId": message.MessageID, "principal": message.PrincipalID,
		"conversation": message.ConversationID,
	}
}
