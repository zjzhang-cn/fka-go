// Package messages 是**消息层**：把一条归一化的入站消息变成一次工具循环问答，
// 再把答案按原路发回去。
//
// ## 它只认三样东西
//
// `channels.InboundMessage`（进来的）、`agent.Runner`（干活的）、`channels.Channel`
// （回去的）。**它不认识 iLink，也不认识任何具体渠道**——判据与 channels 层一样：
// 接第二个渠道时这个文件一行都不用改。
//
// ## 为什么现在只有「文本提问」一条路
//
// 原来的实现有五意图（文件 / 图片 / 命令 / 提问 / 说明）。现在**文件那条路随文档
// 能力一起没了**——这个 agent 不拥有存储，收到文件它也没有地方归档。
//
// 与其留一个「收到文件但什么都不做」的空壳，不如**把不能答的消息如实说出来**：
// 用户发来一张照片，回一句「我只处理文字问题」，比静默丢弃好。
//
// 以后要加能力，加的是**能力**（MCP / skill），不是新的意图分支。
package messages

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zjzhang-cn/fka-go/internal/agent"
	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// 没能答出来时回给用户的话。**逐条都不一样**，因为回同一句话会让人以为机器卡住了。
const (
	replyNoLLM     = "我还没接上模型（缺 LLM_API_KEY / LLM_MODEL），现在没法回答。"
	replyNoAgent   = "我的工具循环被关掉了（LLM_TOOLS=off），现在没法回答。"
	replyAgentFail = "这一轮没能答出来：%s"
	replyNotText   = "我只处理文字问题。图片、文件这些我现在接不住——发文字给我就行。"
	replyEmpty     = "我没想出该怎么回。要不换个说法？"
)

// Handler 处理入站消息。
type Handler struct {
	// Runner 工具循环。为 nil 时每条消息都会得到一句「没接上模型」
	Runner *agent.Runner
	// Tools 工具注册表。给工具提供身份与回话能力
	Tools tools.Service
}

// NewHandler 造消息处理器。
func NewHandler(runner *agent.Runner, registry tools.Service) *Handler {
	return &Handler{Runner: runner, Tools: registry}
}

// Handle 处理一条入站消息。**永不返错**——
//
// 调用点在长驻的消息循环里，抛出去会中断**后续所有消息**的处理。所以这里的失败
// 一律变成回给用户的一句话，而真相进日志。
func (h *Handler) Handle(ctx context.Context, event channels.Event) {
	message := event.Message
	channel := event.Channel

	// **这一轮的归属绑在 ctx 上，往下传**——消息层是唯一知道账号的地方
	//（agent / llm / tools 都看不见渠道），所以绑一次之后，工具循环、模型请求、
	// 工具调用里打出来的每一条日志都会自动带上账号与消息号。
	//
	// 之所以不在下面每个日志点手写 `"account": …`：这条链上新加日志点的人
	// 不会记得抄，而**漏抄的那条日志正是排查时最会误导人的那种**。
	// 也不能用全局变量——消息处理是按账号并行的（见 `dispatch.go`），
	// 详见 `internal/config/scope.go`。
	ctx = config.Bind(ctx, config.Context{
		"channel": message.ChannelID, "account": message.AccountID,
		"messageId": message.MessageID, "principal": message.PrincipalID,
		"conversation": message.ConversationID,
	})
	fields := config.FieldsOf(ctx)

	// ── 收到 ────────────────────────────────────────────────
	//
	// **每一条都记，包括随后会拒答的那些**：「消息到了没有」与「为什么不答」
	// 是两个问题，混在一条日志里就答不上来了。
	config.Log().Info(config.TypeMSG, "收到消息", config.Fields(ctx, config.Context{
		"parts": describeParts(message.Parts), "chars": len(message.Text()),
	}))

	// ── 没有可答的内容，先说清楚 ────────────────────────────
	question := strings.TrimSpace(message.Text())
	if question == "" {
		config.Log().Info(config.TypeMSG, "收到不能作答的消息", fields)
		h.reply(ctx, channel, message, replyNotText, fields)
		return
	}

	// ── 身份缺失就别跑 ──────────────────────────────────────
	//
	// 工具靠 PrincipalID 做权限过滤，而那个值是**模型填的参数**（见
	// tools.Context.PrincipalID）。缺了它，模型就得自己编一个——那等于没有过滤。
	// 所以这里宁可回一句，也不让它猜。
	if strings.TrimSpace(message.PrincipalID) == "" {
		config.Log().Warn(config.TypeMSG, "入站消息没有全局身份，已拒答", fields)
		h.reply(ctx, channel, message, "这条消息里认不出你是谁，我不敢替你回答。", fields)
		return
	}

	if h.Runner == nil {
		config.Log().Warn(config.TypeMSG, "工具循环缺席，已回一句说明", fields)
		h.reply(ctx, channel, message, replyNoLLM, fields)
		return
	}

	// ── 跑一轮 ─────────────────────────────────────────────
	result, err := h.Runner.Run(ctx, agent.RunnerInput{
		SessionID:   channels.SessionKeyOf(&message),
		AccountID:   message.AccountID,
		PrincipalID: message.PrincipalID,
		TurnID:      message.MessageID,
		Question:    question,
		QuotedText:  quotedTextOf(message),
		Reply:       newChannelReply(channel, message),
	})
	if err != nil {
		// 模型的错如实报，**不静默降级**：「稍后再试」会把「接口没配好」
		// 伪装成「模型偶尔不回」
		config.Log().Error(config.TypeMSG, "问答失败", mergeFields(fields, config.Context{"error": err.Error()}))
		h.reply(ctx, channel, message, fmt.Sprintf(replyAgentFail, err.Error()), fields)
		return
	}

	text := strings.TrimSpace(result.Text)
	if text == "" {
		config.Log().Warn(config.TypeMSG, "模型没有给出正文", mergeFields(fields, config.Context{
			"steps": result.Steps, "stoppedBy": result.StoppedBy,
		}))
		h.reply(ctx, channel, message, replyEmpty, fields)
		return
	}

	config.Log().Info(config.TypeMSG, "已作答", mergeFields(fields, config.Context{
		"steps": result.Steps, "stoppedBy": result.StoppedBy,
		"tools": strings.Join(result.UsedTools, "、"),
	}))
	h.reply(ctx, channel, message, text, fields)
}

// reply 把一句话发回原会话。**发不出去只记日志**——
// 已经答完了，回话失败不该再给用户发第二条消息解释「刚才那条没发出去」。
func (h *Handler) reply(ctx context.Context, channel channels.Channel,
	message channels.InboundMessage, text string, fields config.Context) {
	senders := channel.Senders()
	if !senders.HasText() {
		config.Log().Warn(config.TypeMSG, "渠道没有文本发送器，答复没发出去", mergeFields(fields,
			config.Context{"channel": channel.ID()}))
		return
	}

	if _, err := senders.Text(ctx, channels.SendTextParams{
		Target: channels.SendTarget{
			ConversationID: message.ConversationID,
			ReplyToken:     message.ReplyToken,
		},
		Text: text,
	}); err != nil {
		config.Log().Error(config.TypeMSG, "答复发送失败", mergeFields(fields, config.Context{"error": err.Error()}))
		return
	}

	// **发成功也要记**：排查「机器人到底答没答」时，「已作答」与「答复已发出」
	// 是两条独立的证据，缺一条就只能猜
	config.Log().Info(config.TypeMSG, "答复已发出", config.Fields(ctx, config.Context{"chars": len(text)}))
}

// quotedTextOf 被引用那条的正文。**拿不到就返回空串**——
// 引用只是上下文，缺了不该拦住回答。
func quotedTextOf(message channels.InboundMessage) string {
	if message.Quoted == nil {
		return ""
	}
	return strings.TrimSpace(message.Quoted.Body)
}

func describeParts(parts []channels.Part) string {
	kinds := make([]string, 0, len(parts))
	for _, part := range parts {
		kinds = append(kinds, string(part.Kind))
	}
	return strings.Join(kinds, "、")
}

func mergeFields(base, extra config.Context) config.Context {
	out := make(config.Context, len(base)+len(extra))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range extra {
		out[key] = value
	}
	return out
}

// channelReply 让工具能往当前会话发文件。**逐条消息造一个**——「能发给谁」取决于
// 这条消息的会话与回复令牌，那是消息层的事实，工具层不该知道。
type channelReply struct {
	channel channels.Channel
	message channels.InboundMessage
}

func newChannelReply(channel channels.Channel, message channels.InboundMessage) tools.Reply {
	return &channelReply{channel: channel, message: message}
}

func (r *channelReply) File(path string, fileName string) error {
	return r.send(channels.KindFile, path, fileName, "")
}

func (r *channelReply) Image(path string, fileName string) error {
	// 渠道发不了图片就退成文件——**发成文件比发不出去强**
	if r.channel.Capabilities().Image.Send {
		return r.send(channels.KindImage, path, fileName, "image/*")
	}
	return r.send(channels.KindFile, path, fileName, "")
}

func (r *channelReply) send(kind channels.MediaKind, path, fileName, mimeType string) error {
	sender, ok := r.channel.Senders().For(kind)
	if !ok {
		return fmt.Errorf("渠道 %s 发不了 %s", r.channel.ID(), kind)
	}
	_, err := sender(context.Background(), channels.SendMediaParams{
		Target: channels.SendTarget{
			ConversationID: r.message.ConversationID,
			ReplyToken:     r.message.ReplyToken,
		},
		Path:     path,
		FileName: fileName,
		MimeType: mimeType,
	})
	return err
}

// ErrNotReady 装配没齐就跑消息循环。**启动期就该发现**，而不是等第一条消息来暴露。
var ErrNotReady = errors.New("装配没齐：没有可跑的工具循环")
