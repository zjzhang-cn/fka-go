# 交互接缝统一 + 用量通知：开发需求说明与实施计划

> 这份文档是**给下一个实施会话照着做的**：需求、已拍板的取舍、改点清单、测试清单、提交分段都在这里。
> **需求已经过一轮逐条确认（见 1.2「已拍板的六条」）——不要重新讨论**；实施时若要改设计，先改这份文档再动代码。
> 基线：`645853e`（当前 HEAD，工作区干净）。文中 `文件:行` 坐标都按这个基线核对过。

## 一、需求说明

### 1.1 背景：要解决的两件事

**需求 A：统一交互接缝——ask / chat / 微信(iLink) / 网页(SSE) 走同一条问答路径。**

现状是四条前端、两套走法：

```
【现状】

  fka ask ─┐
           ├─(直接调)─────────────────────────► agent.Runner ──► llm
  fka chat─┘     判定/回显/会话策略各写一份

  微信 ─► ilink.Channel ─┐
                         ├─► messages.Handler ─► agent.Runner ──► llm
  网页 ─► web.Channel   ─┘         └─► channel.Senders().Text ──► 回话
```

- **ilink / web**：完整接缝——`channels.Channel` → `messages.Handler.Handle` → `agent.Runner`，答复从 `Senders().Text` 回。
- **ask / chat**：不在接缝上——`cmd/fka` 直接调 `application.Agent.Run(...)`，把判定、回显、会话策略写在自己的函数里。

同一批判定写了两遍，措辞各不相同：

| 情形 | 渠道路径（messages.Handler） | CLI 路径（cmd/fka，ask/chat 各写） |
|---|---|---|
| 空内容 | `replyNotText`：「我只处理文字问题……」 | ask：空问题 → 用法错（exit 2）；chat：不发给模型 |
| 缺身份 | 「认不出你是谁」拒答 | Principal 固定 `local` |
| 没接模型 | `replyNoLLM` 原文 | 各自的前置检查 + 退出 |
| 模型出错 | `replyAgentFail`：`这一轮没能答出来：%s` | ask：`问答失败：%v`（exit 1）；chat：红字同句 |
| 空答复 | `replyEmpty`：「我没想出该怎么回。要不换个说法？」 | 输出一个空行 |
| 回显 | 渠道 EmitterProvider / loggingEmitter | `askEmitter` / `chatEmitter` 内联在 cmd |
| 会话 | `渠道:账号:会话` | `--session` / `FKA_SESSION` / `cli-<uuid>` |

「加一个渠道 = 一个 provider + 装配一行、业务层零改」这条判据对 CLI 并不成立——CLI 是绕开接缝长大的。

**需求 B：用量通知——每轮问答的真实 token 用量（服务端报的）要能看到。**

现状是**一个字节都没采**（不是「没展示」，是「没接」）：

- `llm.ChatResult` 只有 `Content / ToolCalls`（`llm/types.go:133`）；
- openai 请求不带 `stream_options.include_usage`（`baseRequest`，`llm/openai/openai.go:352`）；
- 就算服务端发了收尾用量帧（`choices` 为空、只带 usage），`streamOnce` 的
  `len(chunk.Choices)==0 → continue`（`openai/openai.go:497`）也会把它跳掉；
- `agent.RunResult` 同样没有用量字段。

补充（回应最初那一问）：`agent.Emitter`（`agent/emit.go:18`）刻意只有「过程回显」三件事——
推理增量 / 工具调用 / 答案。**用量不进这个接口**，理由见 2.6。

### 1.2 需求确认（已拍板的六条）

| # | 问题 | 拍板 |
|---|---|---|
| 1 | 到底想要什么 | 三个都要：① 以后接新平台改动小；② 同一个问题各端回答一致；③ 同一套规则只写一遍。**③是底线**（现状确实写了两遍），①②顺带得到 |
| 2 | 「统一」到什么程度 | **只统一里面**——四条路走同一套问答逻辑；外面各自不变。**不做**跨渠道功能对齐（微信 `/new`、网页 `@引用` 之类是另一件产品工程） |
| 3 | 微信/网页的代码动不动 | **一行不改**（含 `serve.go`、`app.go`、channels 协议）；发现真 bug 另立提交，不混在这次里 |
| 4 | 命令行算不算「第三个渠道」 | **算**——ask/chat 做成完整的渠道实现，和微信/网页同一模子；不做「只借逻辑、仍是特殊路径」的半截方案 |
| 5 | 三处「故意不一样」 | 全部照办：① 空答复从空行改为诚实句；② 终端前置检查与退出码保留；③ 会话策略各管各的（ask 不落盘 / chat 落盘 / 渠道按账号） |
| 6 | token 用量怎么显示 | 终端每轮**默认**多打一行（不藏开关）；微信/网页这次不显示 |

### 1.3 验收判据

**目标③（底线）——同一套规则只写一遍：**

1. `cmd/fka/ask.go` / `chat.go` 里不再有 `agent.Runner.Run` 的直接调用、不再有
   「没接模型 / 模型出错 / 空答复」的自写判定与措辞——编排一律经 `messages.Ask`。
2. `internal/messages` 与 `internal/agent` 不为 CLI 增删**渠道专属**逻辑——CLI 只是
   又一个 `channels.Channel` 实现。这条同时是目标①「第 N 个渠道零改业务层」的证明。

**目标②——同一个问题各端回答一致：**

3. 行为不变量（逐条钉住）：
   - **stdout 只有答案正文**（ask 可管道；chat 同）；标签与过程信息一律 stderr；
   - 退出码契约：0 成功 / 1 预期内失败 / 2 用法错；
   - 历史文件名 `cli-<uuid>.jsonl` 与 40 字符预算不变；`--session` 不给时 Ephemeral
     不落盘（现状保持，见 `runner.go:70`）；
   - `[推理] [工具] [助手]` / `● 助手` 的格式、去向（stderr）、颜色规则不变；
   - `/quit /help /new /session /tools` 命令族行为不变；`@文件/@图片` 展开仍进模型问题；
   - `fka ask` 一问就走；`fka chat` 的 REPL、行编辑器、**转圈时序**不变
     （引用提示 → 转圈 → 提问）。
4. 已立案的刻意差异（1.2 第 5 条）：**空答复**从「输出一个空行」变成统一的
   `replyEmpty` 句——交互式前端与渠道从此一致。代码注释与本文档双记录。

**目标①——以后接新平台改动小：**

5. cli 渠道通过 `ValidateChannel`；冻结面成立（1.2 第 3 条）：web/ilink、`serve.go`、
   `app.go`、channels 协议零改动。

**需求 B（用量）：**

1. `fka chat` / `fka ask` 每轮在 stderr 多一行用量（**服务端给了才打**，默认不藏开关）；
   stdout 纪律不受影响。
2. `serve` 侧「已作答」日志带 `tokensIn / tokensOut / tokensTotal` 字段（有值才带）。
3. 多步工具轮：用量是**整轮之和**（`loop.go` 两处 `r.chat` 调用点都累加，含收尾那次
   maxSteps 兜底）；断流重试**不重复计**（只有成功那次有 usage 帧）。
4. 服务端不给就**没有**——不以 `llm.EstimateTokens` 冒充（估算只服务历史预算）。

## 二、设计与改点

### 2.1 目标形状

```
【目标】一条问答路径，CLI 成为第三个渠道

  fka ask ──► cli.Channel(ModeAsk)  ─┐
  fka chat ─► cli.Channel(ModeChat) ─┤
                                     ├─► messages.Ask ──► agent.Runner ──► llm
  微信 ─────► ilink.Channel ─────────┤         │
  网页 ─────► web.Channel ───────────┘         └─► Deliver（channel.Senders().Text）─► 回话
```

三条口径：

- **答案通道唯一**：答复一律经 `channel.Senders().Text` 发出（CLI 的 `Out = stdout`
  就在它的实现里收）；`channels.Emitter.Answer` 在 cli 渠道里**置空**——答案不走回显，
  防双份（web 已是这个口径）。
- **回显走 EmitterProvider**：`cli.Channel` 实现 `channels.EmitterProvider`
  （`channels/types.go:437`），`emitterFor` 自动取它；标签（`[助手]` / `● 助手`）在渠道
  `say()` 里打（→ UI/stderr），正文进 Out。
- **`@引用`（fileref）留在 cmd**：`channels.InboundMessage` 里没有「图片字节」的位置，
  展开结果经 `AskInput.Images` 直接递给循环。**不动渠道协议**（1.2 第 3 条）——
  别的渠道也要 @ 引用时，那是另一场协议变更。

### 2.2 动作一：messages 抽出编排入口 `Ask`

新增 `internal/messages/ask.go`：

- `AskInput{ Channel, Message, SessionID, Ephemeral, Images }`
  - `SessionID` 覆盖渠道协议算出的会话键（CLI 用：`--session` / `cli-<uuid>`；渠道不传）；
  - `Ephemeral` 一气带到 `RunnerInput.Ephemeral`（用途现成，`runner.go:70`）；
  - `Images` 传给 `RunnerInput.Images`（CLI 的 @ 图片）；其余字段填充与现 `Handle`
    一致（`TurnID = Message.MessageID`、`QuotedText = quotedTextOf(message)`、
    `Reply = newChannelReply(...)`）。
- `AskOutcome{ Result, Explain, Cause }`
  - `Explain`——没答出来时该说给用户的那句话（空问题 / 缺身份 / 缺模型 / 模型出错 /
    空答复，逐情形用现有措辞）；
  - `Cause`——模型/调用失败的原因（错误类）；拒答类为零值。
- `(*Handler).Ask(ctx, in) AskOutcome`：绑定 ctx（消息层 5 字段，与 `Handle` 同款）→
  逐条判定 → `Runner.Run` → 归类。**不做发送、不做呈现**。
- `handler.go`：`Handle = Ask + Deliver`；私有 `reply` 更名为导出 `Deliver`
  （非渠道调用方与测试也能用）；`messageFields` 抽取。
- 装配根**零改动**（`app.Messages` 已就位）。

测试 `internal/messages/ask_test.go`：6 条——答出来时不带说明也不发送 / 模型出错时
把原因带回来 / 身份缺失只给拒答说明 / 不给会话键时按渠道协议算 / 本地输入三样
（SessionID/Ephemeral/Images）直达循环 / 结果如实透传。

### 2.3 动作二：`internal/channels/cli`（CLI 成为第三个渠道，1.2 第 4 条）

| 文件 | 职责 |
|---|---|
| channel.go | `Channel`：Identity（`local`）/ Capabilities（只有 Text）/ Senders（Text → `say`）/ Start（阻塞到会话结束）/ Stop / Emitter |
| style.go | 调色板与终端判定（`Palette`、`ShouldColor`、`IsCharDevice`、`firstLine`）——从 cmd 原样搬 |
| echo.go | 两种风格的回显（ask 方括号行 / chat 彩色圆点行）；`Answer` 置空（口径见 2.1） |
| repl.go | chat 的 REPL：banner / 行读取（行编辑器优先、bufio 兜底）/ 命令族 / `StartSpinner` |
| session.go | `SessionPrefix = "cli-"`、`NewSessionID()`（uuid，正好 40 字符预算）——从 `cmd/fka/sessionid.go` 迁入 |

- `Options{ Mode, Session, Principal, Question, In, Out, UI, Editor, Palette, Spinner,
  Lister, HistoryPath }`；`ModeAsk` 一问就走（不读 stdin、不画 banner）；`ModeChat` 进 REPL。
- **不进 `Service.StartAll`**（`seam.go:440`）：常驻渠道由装配根启动；CLI 的 Start
  阻塞到会话结束，由 cmd 自己持有生命周期。cli 包**不实现 `channels.Provider`**
  （`types.go:473`）——它没有账号维度、没有可注册的实例工厂；`cmd/fka/serve.go`
  一行不改。
- 答案通道：`say()` 在 `ModeAsk` 打 `[助手]`（UI）+ 正文（Out）；`ModeChat` 打
  `● 助手`（UI 彩色）+ 正文（Out）。

### 2.4 动作三：ask / chat 薄壳化

- `runAsk`：前置检查（空问题 → 用法错 exit 2）与 `expandFileRefs` **保留在 cmd**
  （1.2 第 5 条②）→ `cli.New(Options{Mode: ModeAsk, Session, Principal, Question,
  Out: stdout, UI: stderr, Palette: NewPalette(false), HistoryPath: llm.SessionPath})`
  → `ch.Start(ctx, 回调)`；回调 = `Messages.Ask(SessionID: session,
  Ephemeral: !explicitSession, Images: expanded.images)` → `Cause != nil` → stderr
  「问答失败：」+ exit 1；成功 → `Messages.Deliver`；`FKA_DEBUG` 行照旧。
- `runChat`：三检查 + readline 建立保留（`defer rl.Close()`）→
  `cli.New(Options{Mode: ModeChat, In: stdin, Editor: readline, Spinner: 双终端判定,
  Lister: 工具表桥（application.Tools → []cli.ToolRow）, Palette: ShouldColor(stderr,
  --no-color), HistoryPath: llm.SessionPath})`；回调 = `expandFileRefs` →
  `· 引用文件：` 提示 → `cli.StartSpinner` 包住 `Ask` → `Cause` 呈现（ctx 取消 →
  「已中断。」；否则「问答失败：」红字，均 return 不打空行）→ `Deliver` → debug 行 → 空行。
- `cmd/fka/sessionid.go` **删除**：uuid 逻辑进 cli/session.go；`sessionID()` 解析
  （`--session` > `FKA_SESSION` > `cli.NewSessionID()`，`ask.go:171`）留在 ask.go；
  `chat.go:274` 的 `newCliSessionID()` 改 `cli.NewSessionID()`。
- 行为差异只有 1.3-4 立案的那一条（空答复句）。
- 测试：`ask_test.go` 保留 flag/会话/纯函数用例；回显相关（`toolLine` 等）迁去 cli 包；
  `chat_test.go` 的 REPL 行为用例迁去 cli 包，cmd 侧保留「@引用进模型问题」这类接线
  用例（可抽 `chatTurn` 纯函数便于测试）。

### 2.5 动作四：用量通知（真实用量）

数据流：

```
openai 请求带 stream_options.include_usage
  → 收尾帧（choices 空、带 usage）→ ChatResult.Usage
  → agent.Run 把两处调用点（loop.go:236 / 329）累加
  → RunResult.Usage
      ├─ messages「已作答」日志：tokensIn/tokensOut/tokensTotal（有值才带）
      └─ CLI 回调：stderr 一行用量（有值才打，默认不藏开关）
```

改点：

- `llm/types.go`：`TokenUsage{PromptTokens, CompletionTokens, TotalTokens}`；
  `ChatResult.Usage`（零值 = 服务端没给，不猜、不估算）。
- `llm/openai/openai.go`：`baseRequest` 加
  `StreamOptions: &goopenai.StreamOptions{IncludeUsage: true}`（SDK v1.42.1 支持该字段；
  现端点 `api.deepseek.com` 官方支持）；`streamOnce` 在 `len(chunk.Choices)==0` 的
  `continue` **之前**接住 `chunk.Usage`；「模型返回」日志（openai.go:570）按有无补
  `tokensIn/tokensOut`。
- `agent`：`RunResult.Usage llm.TokenUsage`；两处 `r.chat` 的返回都累加
  （含 `forcedAnswer` 收尾那次）。
- `messages/handler.go`：「已作答」日志带 tokens 字段。
- `cmd`：`usageLine(in, out, total) string` 纯函数；ask 打 `[用量] 入 X / 出 Y（共 Z）`
  （无颜色），chat 打暗色 `· 用量：入 X / 出 Y（共 Z）`；total 缺省时省略「（共 …）」；
  全零不打；**默认就显示、不设开关**（1.2 第 6 条）。打印顺序：答复（Deliver）→
  用量行 → debug 行。
- **不做**：SSE 事件、微信回执（数据已在 `RunResult` 上就位，留作后续）；不引 tokenizer；
  不加开关（无已知需要——若日后换端点建流 400 且错误文本含该字段，删 `StreamOptions`
  一行即回退）。

### 2.6 为什么用量不进 `agent.Emitter` 接口（回应最初那一问）

- Emitter 是**一轮内的过程事件流**：可能来很多次、是增量（推理按块、每个工具一次、
  答案一次）。
- 用量是**一轮的账目**：一次性、轮末才有定论，属于「结果」而不是「过程事件」。
- 做进接口 = 四个已有实现（ask/chat 回显、渠道适配、日志回显）全部被迫实现一个多数
  前端用不上的方法——**接口的宽度就是替换的成本**（`llm/types.go:176` 那条先例：
  没人调的方法不留，「那不是可替换，是可妨碍」）。
- 挂在 `RunResult` 上零破坏：谁想展示谁读，CLI 与日志各自读一处。

## 三、实施顺序与提交分段

强序：一 → 二 → 三；四独立，放最后（1.2 第 6 条：留在同一计划；先薄壳化再加用量，
展示代码不用挪两次）。

| # | 提交信息（示例） | 内容 | 该段验证 |
|---|---|---|---|
| 1 | `refactor(messages): 抽同步编排入口 Ask，Handle 变薄` | ask.go + handler.go + ask_test.go | `go test ./internal/messages ./internal/channels` |
| 2 | `feat(channels): 新增 cli 渠道——ask/chat 同源` | cli 包 5 源 + 5 测试 | `go test ./internal/channels/...` + gofmt -l 为空 |
| 3 | `refactor(cli): ask/chat 薄壳化，差异退化为渠道配置` | cmd 三文件 + 测试重排 | `go test ./cmd/...` + 手动冒烟 `fka ask` / `fka chat` |
| 4 | `feat(llm): 用量通知——采集、聚合、CLI 与日志可见` | llm/agent/messages/cmd | `go test ./internal/llm/... ./internal/agent/...` |
| 5 | `docs: 记录 CLI 渠道与用量口径` | decisions.md 补条（CLI 是第三个渠道；用量挂结果、不进 Emitter）、AGENTS.md 模块树补 cli 一行并更新文件/用例计数 | — |

全部完成后在 HEAD 上跑 `make verify`（fmt-check → vet → test → build → smoke）。
注意：`make ci` 跑不通是现状（零 CGO 与 `-race` 冲突），别去修它。

## 四、测试清单（中文用例名）

**messages/ask_test.go**：答出来时不带说明也不发送 / 模型出错时把原因带回来 / 身份缺失
只给拒答说明 / 不给会话键时按渠道协议算 / 本地输入三样直达循环 / 结果如实透传。

**cli/channel_test.go**：多轮复用同一个会话 / 回答只进 Out、标签只进 UI / Ask 模式一问
就走 / 颜色开启时 Out 仍然干净 / 回显只落在 UI 上 / 一轮没答上不带走会话 / 认不出的
命令不当问题 / 斜杠命令不发模型 / `/new` 换会话 / EOF 与空行 / 中断干净退出 / 状态开停
对得上 / 声明与发送器对得上（ValidateChannel）/ 行编辑器接管输入、EOF 与报错干净退出。

**cli/echo_test.go**：答案不在这里写 / 推理首块打前缀之后逐块原样 / 长内容压成一行截断 /
chat 风格带色圆点。

**cli/style_test.go**：颜色开关第一条命中说了算 / 调色板关掉时原样返回 / 压行换行变空格
并截到 60 字 / 非字符设备一律为假。

**cli/repl_test.go**：拆斜杠命令参数保留空格 / `/tools` 有 Lister 时列出放行的工具 /
三种空情形分得清。

**cli/session_test.go**：每次都是新的 / 正好用满 40 字符预算。

**llm/openai（用量）**：带 usage 帧时收进结果（并断言请求体带 `stream_options.include_usage`）/
没有 usage 帧时保持零值 / 空 choices 的收尾帧不打断正文与工具拼接 / 断流重试后取成功那次。

**agent（用量）**：用量是各步之和（工具轮两步：入 10+20、出 5+7）。

**cmd（用量）**：`usageLine` 渲染与缺项行为。

## 五、边界与不变量（不许破）

- stdout 只有答案；退出码 0/1/2；历史文件名与 40 字符预算；工具全名与 `arguments`
  原始字节；历史整组丢弃、绝不改写留下的；
- 中文测试名（`Test主体_场景`）；文件头注释解释「为什么」；
- 零 CGO 与 5 个直接依赖不变；
- `internal/channels` 不许 import 任何具体渠道（cli 包自己长在 `internal/channels/` 下，
  符合接缝方向）；`mcp/memory` 边界不动；
- **冻结面（1.2 第 3 条）**：web/ilink 两个包一行不改；`cmd/fka/serve.go`、
  `internal/app/app.go` 零改动；channels 协议（types.go/seam.go）不为 CLI 改；
- **不做跨渠道功能对齐（1.2 第 2 条）**：微信 `/new`、网页 `@引用` 之类是另一件
  产品工程；
- `docs/dev-log.md` 不碰。

## 六、风险与回退

- **行为漂移**（最大风险：stdout/stderr 分工、转圈时序、颜色）→ cli 包测试把
  「Out 只有答案、标签只在 UI」钉死；分段提交，任何一段可单独 revert。
- **空答复句与用量行**是对外可见变化 → 已在 1.2 第 5、6 条与 1.3-4 立案，接受。
- **stream_options 兼容性** → 现端点 DeepSeek 官方支持；换端点若建流 400 且错误文本
  含该字段，删 `StreamOptions` 一行即回退。
