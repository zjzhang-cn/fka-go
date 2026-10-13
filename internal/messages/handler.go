// Package messages 是**消息层**：把一条归一化的入站消息变成一次工具循环问答，
// 再把答案按原路发回去。
//
// ## 它只认三样东西
//
// `channels.InboundMessage`（进来的）、`agent.Runner`（干活的）、`channels.Channel`
// （回去的）。**它不认识 iLink，也不认识任何具体渠道**——判据与 channels 层一样：
// 接第二个渠道时这个文件一行都不用改。
//
// ## 现在能答什么
//
// 文字与**图片**（图片经 `MediaFetcher` 取回成附件，见 `images.go`）。文件 / 语音 /
// 视频这些**没有消费方**——这个 agent 不拥有存储，收到文件也没地方归档。与其留一个
// 「收到文件什么都不做」的空壳，不如**把不能答的消息如实说出来**（`replyNotText`），
// 比静默丢弃好。
//
// 以后要加能力，加的是**能力**（MCP / skill），不是新的意图分支。
package messages

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/zjzhang-cn/fka-go/internal/agent"
	"github.com/zjzhang-cn/fka-go/internal/channels"
	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/tools"
)

// 没能答出来时回给用户的话。**逐条都不一样**，因为回同一句话会让人以为机器卡住了。
const (
	replyNoLLM     = "我还没接上模型（缺 LLM_API_KEY / LLM_MODEL，或 LLM_TOOLS=off），现在没法回答。"
	replyAgentFail = "这一轮没能答出来：%s"
	replyNotText   = "我只处理文字和图片。文件、语音这些我现在接不住——发文字或图片给我就行。"
	replyEmpty     = "我没想出该怎么回。要不换个说法？"
	// replyImageUnavailable 消息里有图、但一张都没取回来（渠道不能取媒体，或取回失败）。
	// 单独一句，别混进 replyNotText——用户发的是图，问题出在「取不到」而不是「不支持」。
	replyImageUnavailable = "图片没能取回来，我暂时看不了这张图。要不重新发一次？"
	// replyStopped 用户中途喊停（网页「停止」按钮）造成的一轮结束。**不是错误**——
	// 一句平静的话，别让用户以为出了故障。
	replyStopped = "（已停止）"
)

// Runner 工具循环在消息层需要的那一部分。
//
// ## 为什么声明在这里，而不是在 agent 包里
//
// 消费者侧声明，宽度也按消费者裁：消息层**只调 Run**，从不调 HasTools 或 Enabled
// （那两个是 `cmd/fka` 用的）。`agent.Runner` 有五个方法、这里只要一个——
// 接口的宽度就是替换的成本。
//
// 有了它，「工具循环缺席」不再需要消息层判指针：缺席的 runner 本身实现了 Run，
// 返 `agent.ErrNoRunner`，由 `Ask` 里的 `errors.Is` 认出并回一句实话。
type Runner interface {
	// Run 跑一轮问答
	Run(ctx context.Context, input agent.RunnerInput) (agent.RunResult, error)
}

// Handler 处理入站消息。
//
// **它只持有 Runner**：以前这里还有一个 `Tools tools.Service` 字段，注释写着
// 「给工具提供身份与回话能力」——而它从来没有被读过一次。工具要的东西（身份、
// 回话能力）是**逐条消息**经 `RunnerInput` 传进去的，不是一个注册表。
// 少一个字段也少一条装配依赖：`NewHandler` 现在不需要装配根把注册表也递进来。
type Handler struct {
	// Runner 工具循环。缺席时也是一个实现了 Runner 的对象，见上
	Runner Runner

	// turns 会话键 → 取消函数。**正在跑的那一轮**（一个会话至多一轮，worker 串行）。
	// `CancelTurn` 取出来 cancel，工具循环的 ctx 随之取消、流被掐断。
	mu    sync.Mutex
	turns map[string]context.CancelFunc
}

// NewHandler 造消息处理器。
func NewHandler(runner Runner) *Handler {
	return &Handler{Runner: runner, turns: map[string]context.CancelFunc{}}
}

// CancelTurn 取消某个会话进行中的一轮（实现 `channels.TurnController`）。
// 返回是否真的取消了一个——没有在跑时回 false，让渠道能如实说「没有进行中的一轮」。
func (h *Handler) CancelTurn(sessionKey string) bool {
	h.mu.Lock()
	cancel, ok := h.turns[sessionKey]
	h.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// setTurn / clearTurn 登记与摘除一个会话的取消函数。
func (h *Handler) setTurn(sessionKey string, cancel context.CancelFunc) {
	h.mu.Lock()
	h.turns[sessionKey] = cancel
	h.mu.Unlock()
}

func (h *Handler) clearTurn(sessionKey string) {
	h.mu.Lock()
	delete(h.turns, sessionKey)
	h.mu.Unlock()
}

// Handle 处理一条入站消息。**永不返错**——
//
// 调用点在长驻的消息循环里，抛出去会中断**后续所有消息**的处理。所以这里的失败
// 一律变成回给用户的一句话，而真相进日志。
//
// 编排（判定 + 跑循环 + 归类）在 `Ask` 里；这里只做「绑定归属 → 收到 → 发送」，
// 外加两件只有这一层做得了的事：把这一轮登记成**可取消**的（网页「停止」按钮），
// 以及把**执行中状态**通知给渠道（发送键切成停止键）。
// CLI 那条路走的是同一个 `Ask`，于是「加一个渠道零改业务层」对命令行也成立。
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
	ctx = config.Bind(ctx, messageFields(message))

	// **这一轮可取消**：从这条消息衍生一个可取消的 ctx，登记到 turns。
	// 工具循环/模型请求都跑在它下面，取消它就能立刻掐断（流式请求当场读不到数据）。
	// 键与会话键一致，渠道按同一条规则算出要停的是哪一场。
	sessionKey := channels.SessionKeyOf(&message)
	ctx, cancel := context.WithCancel(ctx)
	h.setTurn(sessionKey, cancel)

	// **执行中状态**：进这条消息就是「一轮开始」，出它就是「一轮结束」。
	// 覆盖空问题/缺身份这类不走工具循环的拒答——前端只认这一对通知切键，不必自己猜。
	notifyChannel(ctx, channel, channels.TurnStateNotice{Running: true})
	defer func() {
		h.clearTurn(sessionKey)
		cancel()
		notifyChannel(ctx, channel, channels.TurnStateNotice{Running: false})
	}()

	// ── 收到 ────────────────────────────────────────────────
	//
	// **每一条都记，包括随后会拒答的那些**：「消息到了没有」与「为什么不答」
	// 是两个问题，混在一条日志里就答不上来了。
	config.Log().Info(config.TypeMSG, "收到消息", config.Fields(ctx, config.Context{
		"parts": describeParts(message.Parts), "chars": len(message.Text()),
	}))

	outcome := h.Ask(ctx, AskInput{Channel: channel, Message: message})
	h.Deliver(ctx, channel, message, outcome.Text())
}

// Deliver 把一句话发回原会话。**发不出去只记日志**——
// 已经答完了，回话失败不该再给用户发第二条消息解释「刚才那条没发出去」。
//
// 它从 ctx 上取归属字段（`Handle` / `Ask` 都绑过），所以非渠道调用方（CLI）
// 也能直接用；`fields` 为空时只是少几列日志，不影响投递。
func (h *Handler) Deliver(ctx context.Context, channel channels.Channel,
	message channels.InboundMessage, text string) {
	fields := config.FieldsOf(ctx)
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

// emitterFor 定这一条消息的回显：**渠道实现了 `channels.EmitterProvider` 就用它的**，
// 否则用默认的 loggingEmitter（只记工具日志、不做终端回显）。
//
// 这正是「加一个渠道只实现接口」在回显上的落点：渠道想要自己的回显，就实现
// `EmitterProvider`，**消息层一行不用改**；不实现也照跑。
func emitterFor(ctx context.Context, channel channels.Channel) agent.Emitter {
	var echo channels.Emitter = loggingEmitter{ctx: ctx}
	if provider, ok := channel.(channels.EmitterProvider); ok {
		if custom := provider.Emitter(ctx); custom != nil {
			echo = custom
		}
	}
	return channelEcho{channel: channel, inner: echo}
}

// notifyChannel 把一个渠道通知交给渠道。**谁能收就发给谁**：渠道本身实现了
// `channels.Notifier` 就给它（CLI 走这条）；否则问它的 `EmitterProvider`（多会话
// 渠道把 Notifier 长在逐条消息的回显对象上，网页走这条）。两边都没有就静默丢弃。
//
// 与 `channelEcho.Notify` 的区别：那条是**工具循环**发来的（用量），走 agent 自己的
// Notice；这条是**消息层**发的（执行中状态），直接用渠道的 Notice——执行中状态只有
// 消息层知道（含不走工具循环的拒答）。
func notifyChannel(ctx context.Context, channel channels.Channel, notice channels.Notice) {
	if notifier, ok := channel.(channels.Notifier); ok {
		notifier.Notify(notice)
		return
	}
	if provider, ok := channel.(channels.EmitterProvider); ok {
		if emitter := provider.Emitter(ctx); emitter != nil {
			if notifier, ok := emitter.(channels.Notifier); ok {
				notifier.Notify(notice)
			}
		}
	}
}

// channelEcho 把渠道的 `channels.Emitter` 适配成工具循环要的 `agent.Emitter`，
// 并把工具循环的**通知**（用量等）转给渠道的 `channels.Notifier`。
// 三处差异（工具事件是结构体/三个基本值、通知是两份密封接口）都收在这一处适配，
// 接缝与 agent 因此互不认识。
type channelEcho struct {
	channel channels.Channel
	inner   channels.Emitter
}

func (e channelEcho) Reasoning(text string) { e.inner.Reasoning(text) }
func (e channelEcho) Tool(event agent.ToolEvent) {
	e.inner.Tool(event.Name, event.Arguments, event.Result)
}
func (e channelEcho) Answer(text string) { e.inner.Answer(text) }

// Notify 把工具循环的通知转给渠道。**谁能收就发给谁**：渠道本身实现了
// `channels.Notifier` 就给它（CLI 走这条）；否则退回默认回显（loggingEmitter，
// 常驻渠道走这条）。两边都没有就静默丢弃——通知是尽力而为。
func (e channelEcho) Notify(notice agent.Notice) {
	usage, ok := notice.(agent.UsageNotice)
	if !ok {
		return
	}
	converted := channels.UsageNotice{Usage: channels.TokenUsage{
		PromptTokens:     usage.Usage.PromptTokens,
		CompletionTokens: usage.Usage.CompletionTokens,
		TotalTokens:      usage.Usage.TotalTokens,
		CachedTokens:     usage.Usage.CachedTokens,
	}}
	if notifier, ok := e.channel.(channels.Notifier); ok {
		notifier.Notify(converted)
		return
	}
	if notifier, ok := e.inner.(channels.Notifier); ok {
		notifier.Notify(converted)
	}
}

// loggingEmitter 默认回显：**不往控制台画东西**——`serve` 是常驻进程，往 stderr 画
// 推理只会是噪音。推理不看、答案由 handler 经渠道发回去，只有工具调用与轮末用量
// 值得记一条 Info 日志（排查「它查了什么、花了多少」时最有用的一步）。
type loggingEmitter struct{ ctx context.Context }

func (loggingEmitter) Reasoning(string) {}
func (loggingEmitter) Answer(string)    {}

func (e loggingEmitter) Tool(name, arguments, result string) {
	config.Log().Info(config.TypeTOOL, "工具调用", config.Fields(e.ctx, config.Context{
		"tool": name, "args": arguments, "result": clipLine(result),
	}))
}

// Notify 收到轮末通知。默认渠道（没自己的回显）用它把用量记一条日志——聊天窗口
// 那一侧不显示，但服务端要能查到。
func (e loggingEmitter) Notify(notice channels.Notice) {
	usage, ok := notice.(channels.UsageNotice)
	if !ok {
		return
	}
	config.Log().Info(config.TypeMSG, "用量", config.Fields(e.ctx, config.Context{
		"tokensIn": usage.Usage.PromptTokens, "tokensOut": usage.Usage.CompletionTokens,
		"tokensTotal": usage.Usage.TotalTokens, "tokensCached": usage.Usage.CachedTokens,
	}))
}

// clipLine 把一段可能带换行的结果压成一行、并截断——日志一条一行（见 logger 的约定）。
func clipLine(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	runes := []rune(text)
	if len(runes) > 120 {
		return string(runes[:120]) + "…"
	}
	return text
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

// Text 往当前会话发一段文字。与最终答案共用渠道的文本发送器（`reply` 那条路），
// 所以「渠道压根不发文字」与「发失败」在这一层的表现一致。
func (r *channelReply) Text(ctx context.Context, text string) error {
	senders := r.channel.Senders()
	if !senders.HasText() {
		return fmt.Errorf("渠道 %s 发不了文字", r.channel.ID())
	}
	_, err := senders.Text(ctx, channels.SendTextParams{
		Target: channels.SendTarget{
			ConversationID: r.message.ConversationID,
			ReplyToken:     r.message.ReplyToken,
		},
		Text: text,
	})
	return err
}

func (r *channelReply) File(ctx context.Context, path string, fileName string) error {
	return r.send(ctx, channels.KindFile, path, fileName, "")
}

// FileBytes 发已在内存里的文件字节。远端沙盒（SSE）的文件走这条——没有本地路径。
func (r *channelReply) FileBytes(ctx context.Context, name string, mimeType string, data []byte) error {
	return r.sendBytes(ctx, channels.KindFile, name, mimeType, data)
}

// ImageBytes 发已在内存里的图片字节。渠道发不了图片就退成文件，与 Image 一致。
func (r *channelReply) ImageBytes(ctx context.Context, name string, mimeType string, data []byte) error {
	if r.channel.Capabilities().Image.Send {
		return r.sendBytes(ctx, channels.KindImage, name, mimeType, data)
	}
	return r.sendBytes(ctx, channels.KindFile, name, mimeType, data)
}

func (r *channelReply) Image(ctx context.Context, path string, fileName string) error {
	// 渠道发不了图片就退成文件——**发成文件比发不出去强**
	if r.channel.Capabilities().Image.Send {
		return r.send(ctx, channels.KindImage, path, fileName, "image/*")
	}
	return r.send(ctx, channels.KindFile, path, fileName, "")
}

// sendBytes 把**字节**交给渠道的发送器。与 send 同一套目标与令牌，只是不落盘。
func (r *channelReply) sendBytes(ctx context.Context, kind channels.MediaKind, name, mimeType string, data []byte) error {
	sender, ok := r.channel.Senders().For(kind)
	if !ok {
		return fmt.Errorf("渠道 %s 发不了 %s", r.channel.ID(), kind)
	}
	_, err := sender(ctx, channels.SendMediaParams{
		Target: channels.SendTarget{
			ConversationID: r.message.ConversationID,
			ReplyToken:     r.message.ReplyToken,
		},
		Data:     data,
		FileName: name,
		MimeType: mimeType,
	})
	return err
}

// send 把文件交给渠道的发送器。
//
// ctx 由调用方（工具那一步）给：发文件是一次网络上传，而**停机时它该能被打断**。
// 这里以前写死 `context.Background()`——调用方传什么都断不掉，而签名上看不出来。
func (r *channelReply) send(ctx context.Context, kind channels.MediaKind, path, fileName, mimeType string) error {
	sender, ok := r.channel.Senders().For(kind)
	if !ok {
		return fmt.Errorf("渠道 %s 发不了 %s", r.channel.ID(), kind)
	}
	_, err := sender(ctx, channels.SendMediaParams{
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
