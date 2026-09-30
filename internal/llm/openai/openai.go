// Package openai 是 OpenAI 兼容协议的模型实现：**一种 provider**。
//
// ## 协议：OpenAI 兼容，用社区 SDK
//
// 请求体与响应体的形状交给 github.com/sashabaranov/go-openai。换 provider 时
// **只改这个文件**：契约在 llm/types.go，业务层只认 ChatClient。
//
// ## 用流式，超时算空闲不算总时长
//
// 每次都发流式请求。两个超时并存：
//
//   - 整体（LLM_TIMEOUT_MS，默认 120s）：从发请求到读完响应，硬上限；
//   - 断流（LLM_STREAM_TIMEOUT_MS，默认 5s）：两块数据之间的最大间隔，**每收到
//     一块就重置**。
//
// 所以模型持续吐推理或正文时不会超时，只有真正卡住才中止。长思考的模型因此不会
// 被总时长砍掉——这正是原 Node 版改成流式要解决的问题。
//
// Go 里用 `context.WithTimeoutCause` / `context.WithCancelCause` 表达两种中止原因：
// `context.Cause` 能取回「到底是被整体超时掐的，还是断流掐的」，报给人看的错误
// 因此能说清是哪一种，而不是笼统的「context deadline exceeded」。
//
// ## enable_thinking 为什么要包一层 HTTPDoer
//
// SDK 的请求结构体里没有这个字段（它不是 OpenAI 规范的一部分，是各家推理模型的
// 扩展），而它必须原样出现在请求体里。走 ClientConfig.HTTPClient 注入比 fork
// SDK 轻得多：**只重排请求体外层的键序，不碰 messages 里的任何内容**——而 provider
// 的前缀缓存认的是**分词后的消息序列**，不是 JSON 字节序，所以这么改是安全的。
//
// ## 没配就是没有，不阻断任何东西
//
// ReadConfig 在缺 key 或 model 时返回 false，组装据此不启用，问答照常回原文片段。
// 可选能力缺失不该让服务起不来，也不该让整条问答失败。
//
// ## 调用失败要返错，而不是自己吞掉
//
// 这一层**返错**（超时、非 2xx、响应体不认识）；由 qa 包 / agent 循环接住并降级。
// 理由：降级的措辞与「用哪些片段」是上层知识，放在这里会让两处各写一半。
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	goopenai "github.com/sashabaranov/go-openai"

	"github.com/zjzhang-cn/fka-go/internal/config"
	"github.com/zjzhang-cn/fka-go/internal/llm"
)

// DefaultBaseURL 默认供应商。LLM_BASE_URL 不填时用它——但 key 与 model 仍必须自己给。
const DefaultBaseURL = "https://api.openai.com/v1"

// DefaultTimeoutMs 单次回复的整体超时。长思考的模型可能想很久，给到 120s。
const DefaultTimeoutMs = 120_000

// DefaultStreamTimeoutMs 流式接收的断流超时：这么久没有新数据就认为卡死。
// **每收到一块都会重置**。
const DefaultStreamTimeoutMs = 10_000

// DefaultContextTokens 默认上下文预算。0 表示不压缩历史。
const DefaultContextTokens = 0

// Temperature 采样温度。问答要的是**贴着资料**，不是发挥，所以调得很低。
const Temperature = 0.2

// ReadConfig 从环境变量读配置。缺 LLM_API_KEY 或 LLM_MODEL 就返回 false。
//
// 纯函数（只读入参），便于测试与 doctor 复用。
func ReadConfig() (llm.Config, bool) {
	apiKey := strings.TrimSpace(os.Getenv("LLM_API_KEY"))
	model := strings.TrimSpace(os.Getenv("LLM_MODEL"))

	// 只配了一半（比如填了地址忘了 key）与完全没配，对调用方是一回事：都用不了。
	// 不在这里报「配置不完整」——那要到第一次提问才炸，不如统一降级
	if apiKey == "" || model == "" {
		return llm.Config{}, false
	}

	baseURL := strings.TrimSpace(os.Getenv("LLM_BASE_URL"))
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	return llm.Config{
		// 去掉尾部斜杠：否则会拼出 `.../v1//chat/completions`
		BaseURL:         strings.TrimRight(baseURL, "/"),
		APIKey:          apiKey,
		Model:           model,
		TimeoutMs:       positiveInt(os.Getenv("LLM_TIMEOUT_MS"), DefaultTimeoutMs),
		StreamTimeoutMs: positiveInt(os.Getenv("LLM_STREAM_TIMEOUT_MS"), DefaultStreamTimeoutMs),
		ContextTokens:   nonNegativeInt(os.Getenv("LLM_CONTEXT_TOKENS"), DefaultContextTokens),
	}, true
}

func positiveInt(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

// nonNegativeInt 允许 0 的整数。空值 / 非法值退回默认。
func nonNegativeInt(raw string, fallback int) int {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fallback
	}
	value, err := strconv.Atoi(trimmed)
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

// ReasoningVisible 推理内容是否在前台打印。默认开；LLM_SHOW_REASONING=0 关掉。
func ReasoningVisible() bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("LLM_SHOW_REASONING")))
	if raw == "" {
		return true
	}
	switch raw {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// HostOf 只取主机名，供日志用。**不打完整 URL、更不打 key**。
func HostOf(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return "(地址不合法)"
	}
	return parsed.Host
}

// Provider OpenAI 兼容 provider。**零字段**——以前那个 SystemPrompt 只服务于已删的
// 单发路径，而它一旦有字段，装配根就得知道每个 provider 怎么配。
type Provider struct{}

func (Provider) ID() string { return "openai" }

func (Provider) ReadConfig() (llm.Config, bool) { return ReadConfig() }

// newClient 按配置造 SDK 客户端。
//
// **刻意不设 http.Client.Timeout**：那是「整次请求」的上限，会让断流那条永远
// 命中不到（两者语义重叠时，短的那个先生效）。两套超时都由 ctx 表达，见
// postCompletion。
func newClient(cfg llm.Config) *goopenai.Client {
	clientCfg := goopenai.DefaultConfig(cfg.APIKey)
	clientCfg.BaseURL = cfg.BaseURL
	clientCfg.HTTPClient = &bodyInjector{
		next:   &http.Client{},
		extras: map[string]any{"enable_thinking": true},
		apiKey: cfg.APIKey,
	}
	return goopenai.NewClientWithConfig(clientCfg)
}

// bodyInjector 在请求体 JSON 的**外层**补几个字段。
//
// ## 为什么不直接改 SDK 的结构体
//
// 请求结构体是第三方代码，fork 它就要一直跟着上游改。走 HTTPDoer 注入只碰
// 请求体外层，**不碰 messages 里的任何内容**——而 provider 的前缀缓存认的是
// **分词后的消息序列**，不是 JSON 的键序，所以重编码请求体是安全的。
//
// ## 读不出 JSON 就原样放行
//
// 注入失败不该让请求失败：那说明我们对请求体的假设错了（SDK 换了形状、或有人换了
// HTTPDoer），而原样发出去**多半仍能工作**（服务端不认识这个字段就忽略）。
// 真不行的话服务端会回 400，错误信息里能看出来。
type bodyInjector struct {
	next   *http.Client
	extras map[string]any
	apiKey string
}

func (b *bodyInjector) Do(req *http.Request) (*http.Response, error) {
	if req.Body == nil || len(b.extras) == 0 {
		return b.next.Do(req)
	}

	original, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		// 读不出来就还回去，让底层自己再试一次
		req.Body = io.NopCloser(bytes.NewReader(original))
		return b.next.Do(req)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(original, &payload); err != nil {
		req.Body = io.NopCloser(bytes.NewReader(original))
		return b.next.Do(req)
	}
	for key, value := range b.extras {
		encoded, err := json.Marshal(value)
		if err != nil {
			continue
		}
		payload[key] = encoded
	}

	patched, err := json.Marshal(payload)
	if err != nil {
		req.Body = io.NopCloser(bytes.NewReader(original))
		return b.next.Do(req)
	}

	req.Body = io.NopCloser(bytes.NewReader(patched))
	req.ContentLength = int64(len(patched))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(patched)), nil
	}
	return b.next.Do(req)
}

// CreateChat 造一个带工具能力的调用函数。**tools 为空时就是普通聊天补全**，
// 所以「工具循环」与「单发」不再需要两条路。
func (p Provider) CreateChat(cfg llm.Config) llm.ChatClient {
	client := newClient(cfg)
	host := HostOf(cfg.BaseURL)

	return func(ctx context.Context, messages []llm.ChatMessage, tools []llm.ToolDef) (llm.ChatResult, error) {
		request := baseRequest(cfg, messages)
		if len(tools) > 0 {
			request.Tools = toAPITools(tools)
			// **必须是裸字符串**。SDK 自带的 ToolChoice 结构体序列化成
			// `{"type":"auto"}`，而规范里 tool_choice 是
			// `"auto" | "none" | "required" | {"type":"function","function":{...}}`
			// ——`{"type":"auto"}` 里的 auto 是 tool 的类型，不是取值，会被拒。
			request.ToolChoice = "auto"
		}

		result, err := postCompletion(ctx, client, cfg, request, host)
		if err != nil {
			return llm.ChatResult{}, err
		}

		// 空回答会让上层把它当成「模型没说话」而静默降级，把「接口形状变了」
		// 伪装成「模型偶尔不回」。这里返错，与原实现同一条理由。
		if result.Content == "" && len(result.ToolCalls) == 0 {
			return llm.ChatResult{}, errors.New("模型既没有给出回答也没有调用工具")
		}
		return result, nil
	}
}

func baseRequest(cfg llm.Config, messages []llm.ChatMessage) goopenai.ChatCompletionRequest {
	return goopenai.ChatCompletionRequest{
		Model:       cfg.Model,
		Messages:    toAPIMessages(messages),
		Temperature: Temperature,
		MaxTokens:   llm.MaxAnswerTokens,
		// 只要工具就发流式：没有它拿不到增量，也就没有「每收一块重置断流预算」
		Stream: true,
	}
}

// postCompletion 发一次流式请求并把分片拼成最终结果。**两个调用方共用**，
// 这样「超时怎么报、非 2xx 怎么报」只有一份实现。
func postCompletion(
	ctx context.Context,
	client *goopenai.Client,
	cfg llm.Config,
	request goopenai.ChatCompletionRequest,
	host string,
) (llm.ChatResult, error) {
	overallDur := time.Duration(cfg.TimeoutMs) * time.Millisecond
	idleDur := time.Duration(cfg.StreamTimeoutMs) * time.Millisecond

	overallErr := errors.New("模型调用整体超时")
	idleErr := errors.New("模型断流")

	// 整体：从发请求起一次性计时，不重置
	overallCtx, cancelOverall := context.WithTimeoutCause(ctx, overallDur, overallErr)
	defer cancelOverall()

	// 断流：每次收到数据就重置。已触发过就不管了——ctx 已取消，Reset 只是白设一次
	streamCtx, cancelIdle := context.WithCancelCause(overallCtx)
	defer cancelIdle(nil)
	idleTimer := time.AfterFunc(idleDur, func() { cancelIdle(idleErr) })
	defer idleTimer.Stop()

	stream, err := client.CreateChatCompletionStream(streamCtx, request)
	if err != nil {
		return llm.ChatResult{}, wrapRequestError(streamCtx, err, cfg, host, overallErr, idleErr)
	}
	defer func() { _ = stream.Close() }()

	// ── 提交了 ──────────────────────────────────────────────
	//
	// **Host 与 model 一定要记**：「答得不对」时第一件要确认的就是打到了哪个
	// 接口、哪个模型——换过 baseURL 或 model 的部署，光看答案猜不出来。
	// **绝不含 key**，见 `SanitizeError` 旁边那条同类的约定。
	//
	// 账号与会话号由 ctx 带过来（见 `internal/config/scope.go`）：
	// 这一层看不见渠道，不绑在 ctx 上的话就只有一条「谁调的模型」都查不出来的日志。
	startedAt := time.Now()
	config.Log().Info(config.TypeLLM, "提交模型请求", config.Fields(ctx, config.Context{
		"model": cfg.Model, "host": host, "stream": true,
		"messages": len(request.Messages), "tools": len(request.Tools),
		"timeoutMs": cfg.TimeoutMs, "streamTimeoutMs": cfg.StreamTimeoutMs,
	}))

	sink := newReasoningSink()
	var (
		content   strings.Builder
		reasoning strings.Builder
		toolCalls = map[int]*llm.ToolCall{}
		order     []int
		sawReason bool
	)

	for {
		// **每收到一块就重置断流预算**（整体预算不动）
		idleTimer.Reset(idleDur)

		chunk, err := stream.Recv()
		if err != nil {
			// io.EOF 是「服务端发了 [DONE]」的正常结束，不是失败
			if errors.Is(err, io.EOF) {
				break
			}
			return llm.ChatResult{}, wrapRequestError(streamCtx, err, cfg, host, overallErr, idleErr)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta

		// 推理：送到前台打印，但**不进答案**
		//
		// **顺手也累加起来**，为了流结束后能记一条带原文的 DEBUG。
		// 以前它只进控制台——重定向到文件或关掉前台之后就彻底没了，
		// 「模型为什么这么答」就成了只能猜的事
		if delta.ReasoningContent != "" {
			sink(delta.ReasoningContent)
			reasoning.WriteString(delta.ReasoningContent)
			sawReason = true
		}

		content.WriteString(delta.Content)

		for _, call := range delta.ToolCalls {
			// 分片按 index 归并。index 缺省时按到达顺序串行接在最后一条上
			index := len(order)
			if call.Index != nil {
				index = *call.Index
			}
			merged, ok := toolCalls[index]
			if !ok {
				merged = &llm.ToolCall{}
				toolCalls[index] = merged
				order = append(order, index)
			}
			if call.ID != "" {
				merged.ID = call.ID
			}
			if call.Function.Name != "" {
				merged.Name = call.Function.Name
			}
			// **arguments 必须逐字累加**：它是原始 JSON 文本，解析交给调用方，
			// 而会话历史要靠它逐字重放
			merged.Arguments += call.Function.Arguments
		}
	}

	if sawReason {
		sink("\n")
	}

	// 服务端没发 [DONE] 就断流：按已收到的内容收尾，**不假装失败**
	calls := make([]llm.ToolCall, 0, len(order))
	for _, index := range order {
		call := toolCalls[index]
		if call.ID != "" && call.Name != "" {
			calls = append(calls, *call)
		}
	}

	// ── 推理与答案 ──────────────────────────────────────────
	//
	// **推理只记截断后的开头**：它可能有几千字，全量落盘会把日志撑爆。
	// 完整的推理在控制台（`LLM_SHOW_REASONING=0` 可关）与 transcript 里。
	config.Log().Debug(config.TypeRSN, "模型的推理", config.Fields(ctx, config.Context{
		"chars": len([]rune(reasoning.String())),
		"text":  snippetRunes(reasoning.String(), reasoningLogChars),
	}))

	answer := strings.TrimSpace(content.String())
	config.Log().Info(config.TypeLLM, "模型返回", config.Fields(ctx, config.Context{
		"chars": len([]rune(answer)), "toolCalls": len(calls),
		"reasoningChars": len([]rune(reasoning.String())),
		"ms":             time.Since(startedAt).Milliseconds(),
	}))

	return llm.ChatResult{Content: answer, ToolCalls: calls}, nil
}

// reasoningLogChars 推理在日志里最多记多少字。
const reasoningLogChars = 200

// snippetRunes 截断到前 n 个字（按字不是按字节，所以不会切坏一个汉字）。
// 超长时**在末尾标出被截掉多少**——只看得到开头会让人以为那就是全部。
func snippetRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + fmt.Sprintf("…（共 %d 字）", len(runes))
}

// wrapRequestError 把底层错误翻译成一句人话，并区分是被整体超时还是断流掐掉的。
func wrapRequestError(
	ctx context.Context,
	err error,
	cfg llm.Config,
	host string,
	overallErr, idleErr error,
) error {
	if cause := context.Cause(ctx); cause != nil {
		switch {
		case errors.Is(cause, overallErr):
			return fmt.Errorf("模型请求失败（%s）：整体超过 %dms 未完成", host, cfg.TimeoutMs)
		case errors.Is(cause, idleErr):
			return fmt.Errorf("模型请求失败（%s）：断流，超过 %dms 没有返回新数据", host, cfg.StreamTimeoutMs)
		}
	}
	return fmt.Errorf("模型请求失败（%s）：%s", host, SanitizeError(err, cfg))
}

// reasoningSink 把推理片段写到**前台控制台**：首块打一次 `[推理] ` 前缀，之后逐块
// 原样输出；流结束时再喂一个换行收尾。
//
// LLM_SHOW_REASONING=0 时是空操作——重定向日志、CLI 或不想看的部署可以关掉。
type reasoningSink func(text string)

func newReasoningSink() reasoningSink {
	if !ReasoningVisible() {
		return func(string) {}
	}
	started := false
	return func(text string) {
		if text == "" {
			return
		}
		if !started {
			fmt.Print("[推理] ")
			started = true
		}
		fmt.Print(text)
	}
}

// toAPIMessages 把内部消息转成 SDK 的消息。
//
// assistant 带 tool_calls 时 Content **不能是空串**——OpenAI 明确要求这一项在带
// tool_calls 时可以是 null，但不能是空串。SDK 的 `omitempty` 会把空串直接删掉，
// 正好得到「没有这个字段」的效果，而各家实现都接受这个形状。
func toAPIMessages(messages []llm.ChatMessage) []goopenai.ChatCompletionMessage {
	out := make([]goopenai.ChatCompletionMessage, 0, len(messages))

	for _, message := range messages {
		switch {
		case message.Role == llm.RoleAssistant && len(message.ToolCalls) > 0:
			calls := make([]goopenai.ToolCall, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				calls = append(calls, goopenai.ToolCall{
					ID:   call.ID,
					Type: goopenai.ToolTypeFunction,
					Function: goopenai.FunctionCall{
						Name:      call.Name,
						Arguments: call.Arguments,
					},
				})
			}
			out = append(out, goopenai.ChatCompletionMessage{
				Role:      goopenai.ChatMessageRoleAssistant,
				Content:   message.Content,
				ToolCalls: calls,
			})

		case message.Role == llm.RoleTool:
			out = append(out, goopenai.ChatCompletionMessage{
				Role:       goopenai.ChatMessageRoleTool,
				Content:    message.Content,
				ToolCallID: message.ToolCallID,
			})

		case message.Role == llm.RoleSystem:
			out = append(out, goopenai.ChatCompletionMessage{
				Role:    goopenai.ChatMessageRoleSystem,
				Content: message.Content,
			})

		default:
			out = append(out, goopenai.ChatCompletionMessage{
				Role:    goopenai.ChatMessageRoleUser,
				Content: message.Content,
			})
		}
	}

	return out
}

// toAPITools 把工具声明转成 SDK 的 function 工具。
//
// **Parameters 原样透传**：工具 schema 是数据（MCP 服务器各自定义），Go 侧不该
// 逐个翻译成强类型结构——翻译一遍就多一处会漂移的地方。
func toAPITools(tools []llm.ToolDef) []goopenai.Tool {
	out := make([]goopenai.Tool, 0, len(tools))
	for _, tool := range tools {
		definition := &goopenai.FunctionDefinition{Name: tool.Name}
		if tool.Description != "" {
			definition.Description = tool.Description
		}
		if len(tool.Parameters) > 0 {
			definition.Parameters = tool.Parameters
		}
		out = append(out, goopenai.Tool{Type: goopenai.ToolTypeFunction, Function: definition})
	}
	return out
}

// SanitizeError 抹掉错误里可能出现的 **key**，并把响应体截短。
//
// SDK 的错误有时会把请求头原样带进 message。不打 key 是硬规矩——日志会进文件、
// 文件会进 NAS。
func SanitizeError(err error, cfg llm.Config) string {
	message := err.Error()
	if cfg.APIKey != "" {
		message = strings.ReplaceAll(message, cfg.APIKey, "***")
	}
	// 不把整页 HTML 塞进日志/消息
	message = strings.Join(strings.Fields(message), " ")
	return llm.Clamp(message, 200)
}
