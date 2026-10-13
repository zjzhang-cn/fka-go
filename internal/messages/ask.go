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
	"github.com/zjzhang-cn/fka-go/internal/tools"
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
	//
	// 文字与非文本 part 都没有才拒答。**只有文件/图片**也是可答的消息——媒体部分在
	// 下面取回、分类、交给模型与工具。
	question := strings.TrimSpace(message.Text())
	mediaParts := mediaPartsOf(message)
	if question == "" && len(mediaParts) == 0 {
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

	// ── 入站媒体：取回、分类（唯一转换点在 media.go）────────
	//
	// 两条来源归到一起：CLI 的 `@图片` 已在 `in.Images`，渠道产出的媒体 part 在这里
	// 取回。分类语义与沙盒 `read` / CLI `@引用` 一致——图片作附件、文本并进问题、
	// 二进制只给元信息。**去向由 FKA_MEDIA_MODE 定**（llm / both / sandbox）：
	// 前者喂模型、sandbox 只落沙盒、both 两处都有（见 media.go 的 MediaMode）。
	mode := h.currentMediaMode()
	images := append([]llm.ImageAttachment(nil), in.Images...)
	var inbound []tools.Attachment
	imported := "" // 本轮落沙盒的说明（both/sandbox）
	if len(mediaParts) > 0 {
		media := collectInboundMedia(ctx, channel, mediaParts)
		inbound = append(inbound, media.files...)

		// both/sandbox：**先把本轮字节落沙盒**（走注入器；落点工具仍受 external 闸约束）
		if mode.AutoImports() {
			if importer := h.importer(); importer != nil && len(inbound) > 0 {
				note, err := importer(ctx, message.PrincipalID, sessionID, inbound)
				if err != nil {
					config.Log().Warn(config.TypeMSG, "落沙盒失败",
						mergeFields(fields, config.Context{"error": err.Error()}))
				} else {
					imported = strings.TrimSpace(note)
				}
			}
		}

		if mode.FeedsModel() {
			images = append(images, media.images...)
			// 一件内容都没取回来（也没落成沙盒）：如实说，别跑模型空猜
			if question == "" && len(media.sections) == 0 && imported == "" {
				config.Log().Warn(config.TypeMSG, "入站文件一件都没取回来", mergeFields(fields,
					config.Context{"failures": strings.Join(media.failures, "；")}))
				return AskOutcome{Explain: replyMediaUnavailable}
			}
			// 纯媒体（文字为空）补一句默认提示语：空文本 + 内容块各家模型接受度不一
			if question == "" {
				question = defaultMediaQuestion
			}
			question += media.questionSuffix()
		} else {
			// sandbox：媒体不进模型。一件都没落成时如实说。
			if len(inbound) == 0 {
				config.Log().Warn(config.TypeMSG, "入站文件一件都没取回来", mergeFields(fields,
					config.Context{"failures": strings.Join(media.failures, "；")}))
				return AskOutcome{Explain: replyMediaUnavailable}
			}
			if question == "" {
				// **纯文件、无指令：只存不答、不跑模型**——挂起沙盒说明等下一轮指令；
				// 用一条**通知**（像用量那样，不是聊天回复）告诉用户文件在哪、请下指令。
				h.rememberInbox(sessionID, imported)
				line := sandboxStoredLine(imported)
				config.Log().Info(config.TypeMSG, "文件已落沙盒，等待指令", mergeFields(fields,
					config.Context{"line": line}))
				if !notifyChannelReporting(ctx, channel, channels.MediaStoredNotice{Line: line}) {
					// 渠道收不到通知（如 iLink）：退回一条文本，至少让人看见。
					return AskOutcome{Explain: line}
				}
				return AskOutcome{}
			}
		}
	}

	// 挂起的沙盒说明（上一个「只存」的文件）+ 本轮落沙盒的说明，并进本轮问题——
	// 于是模型这一轮与后续轮都知道文件在沙盒的哪些路径。
	if note := joinInboxNotes(h.takeInbox(sessionID), imported); note != "" {
		question += "\n\n（用户发来的文件已存入沙盒）\n" + note
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
		Images:      images,
		Inbound:     inbound,
	})
	if err != nil {
		// **被喊停不是错误**：网页「停止」按钮或终端 Ctrl-C 取消的是这一轮的 ctx，
		// 工具循环随之返回一个 ctx 错。回一句平静的话（`Cause` 仍带上，CLI 靠它区分
		// 「已中断」与真失败）。
		if ctx.Err() != nil {
			config.Log().Info(config.TypeMSG, "这一轮被取消", mergeFields(fields, config.Context{"error": err.Error()}))
			return AskOutcome{Explain: replyStopped, Cause: err}
		}
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

	config.Log().Info(config.TypeMSG, "已作答", mergeFields(fields, config.Context{
		"steps": result.Steps, "stoppedBy": result.StoppedBy,
		"tools": strings.Join(result.UsedTools, "、"),
	}))
	return AskOutcome{Result: result}
}

// sandboxStoredLine 拼「已落沙盒」的**单行**通知：带上沙盒里的路径与文件名（落点工具
// 返回的 `- inbox/x.png` 行）；抽不到路径就退回一句不带列表的。
func sandboxStoredLine(note string) string {
	if paths := sandboxPathList(note); len(paths) > 0 {
		return "已存入沙盒：" + strings.Join(paths, "、") + "。" + sandboxAskPrompt
	}
	return "文件已存入沙盒。" + sandboxAskPrompt
}

// sandboxPathList 从落点工具返回的说明里抽出 `- <路径>` 那几行（沙盒给的相对路径，
// 自带文件名）。认不出就返回空。
func sandboxPathList(note string) []string {
	var out []string
	for _, line := range strings.Split(note, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		if entry := strings.TrimSpace(strings.TrimPrefix(line, "- ")); entry != "" {
			out = append(out, entry)
		}
	}
	return out
}

// joinInboxNotes 合并两段沙盒说明（上一轮挂起的 + 本轮的），空串忽略。
func joinInboxNotes(first, second string) string {
	first, second = strings.TrimSpace(first), strings.TrimSpace(second)
	switch {
	case first == "":
		return second
	case second == "":
		return first
	default:
		return first + "\n" + second
	}
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
