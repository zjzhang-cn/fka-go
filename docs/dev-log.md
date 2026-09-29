# 开发日志（Go 版）

> 记录 Go 版的功能实现、架构调整与 BUG 修复。**最新记录在上。**
>
> **规范：** 写日志先于提交。一个完整功能一次提交，禁止把不相关改动混在一起。
>
> 本项目是**独立仓库**。Node 版（`fka` 仓库）有自己的开发日志，两边历史互不相干。

## 2026-09-29 日志文件改成普通文本，行首多一格「这段事是哪一段」

**类型：** feature（含三处顺手修的 BUG）

**内容：**
- **落盘格式从 JSON 改成普通文本**：`[级别][账号][哪一段] 消息 键=值 …`，文件行前面
  多一个 UTC 时间戳。`internal/config/logger.go` 与 `mcp/memory/internal/log/logger.go`
  两份 logger 一起改。
- 字段按**字典序**排；值**不加工**，只收拾排版：换行/回车/制表符转成可见的
  `\n`、`\t`，带空格、引号、`=` 的值加引号。
- **控制台与文件共用一个 `renderLine`**：控制台上看见的排版就是文件里的排版。
- 新增 `Type`（`SYS/CHAN/MSG/PRM/LLM/RSN/TOOL/HIST`），每条日志点显式说出自己属于
  哪一段；`--log-level` 参数与 `LOG_LEVEL` 一起定控制台级别，认不出就以用法错（2）退出。
- 顺手修：`rotate` 写成了 `<date>.log`，丢掉了 `app.` 前缀（注释与两个读日志的
  用例都写着 `app.<date>.log`）；`cmd/fka` 那个真跑 `serve` 的用例会被前一个登录
  用例漏进进程环境的 `ILINK_ACCOUNT_1_*` 带着真起长轮询，卡到测试超时；**记忆 MCP
  server 把 INFO 日志打进了 stdout**——那是 JSON-RPC 通道。

**为什么：**
JSON 落盘是给机器看的，而这份日志**几乎只在排查时被 `grep` / `tail` / `cut`**：
`{"account":"acct-1"}` 要写成 `grep '"account":"acct-1"'`，而 `account=acct-1` 是
肉眼就能写出来的。改文本之后还多一样好处：`cut -d' ' -f3` 一下就是级别与阶段。

**⚠️ 换格式最容易丢的一条性质：「一条日志 = 一行」。** JSON 时代换行被 `\n` 天然
转义掉了，普通文本得自己转——工具参数、推理片段、数据库驱动的报错都能带换行，
不转的话一条日志会摊成两三行，后半截看起来像**另一条**的，`cut` 出来的级别与阶段
全错。所以 `oneLine` / `field` 这两个转义函数是有用例钉着的，不是顺手加的。

**为什么 MCP server 的日志一律走 stderr**：`mcp.json` 里给了 `env` 时子进程只拿到
那几项，**没有 `LOG_LEVEL`**，级别落回默认的 debug，于是「server 就绪」那行直接打进
JSON-RPC 通道。之所以没天天把主程序打挂：客户端偶尔会跳过解析不了的行——**这种
「没事」纯属侥幸**。

**关联文件：** `internal/config/logger.go`、`internal/config/scope.go`、
`internal/config/format_test.go`、`mcp/memory/internal/log/logger.go`、
`cmd/fka/loglevel.go`、`internal/{agent,app,channels,llm,messages,tools}/**`

**验证：**
- [x] `gofmt -l .` 无输出；`go vet ./...` 干净；`go test ./...` 全绿；`make verify`
      （含 smoke：真机 `fka tools` 列出两个技能与两个 memory 工具）
- [x] `CGO_ENABLED=1 go test -race -count=2` 覆盖改动的四个包全绿
- [x] 真机：`fka-memory` 手动跑一次，**stdout 只剩 JSON-RPC**，日志在 stderr 与
      `logs/app.<date>.log`（普通文本）
- [ ] `make ci` 跑不通：Makefile 全局 `export CGO_ENABLED=0`，而 `go test -race`
      要 cgo。要竞态检测得手动 `CGO_ENABLED=1 go test -race -count=2 ./...`

## 2026-09-29 一轮问答的全链路日志：每条都带账号

**类型：** feature

**内容：**
- 新增 `internal/config/scope.go`：`Bind(ctx, fields)` / `FieldsOf(ctx)` / `Fields(ctx, extra)`。
- `messages.Handler.Handle` 开头把 `channel / account / messageId / principal / conversation`
  绑到 ctx 上，往下传；整条链上的日志点改用 `config.Fields(ctx, …)`。
- 补齐缺的日志点：`收到消息`、`答复已发出`、`提示词已拼接`、`提交模型请求`、
  `模型返回`、`模型的推理`（推理内容第一次进日志）。
- `llm/session.go` 的 5 处告警从「只给 path」改成直接给 `account` + `session`。
- 新增 `internal/config/scope_test.go`（9 条）与两处抓真实日志输出的用例
  （`internal/messages/accountlog_test.go`、`internal/llm/openai/logscope_test.go`）。

**为什么：**
多账号并行之后，「这条日志是哪个账号触发的」成了排查的基本前提。而**账号只在
消息层是现成的**——`agent` / `llm` / `tools` 三层都看不见渠道，往下就断了。

**为什么不让人在每个日志点手写 `"account": …`**：这条链上以后还会新增日志点，
写的人不会记得抄。而**漏抄的那条日志正是排查时最会误导人的那种**——它看起来
完整，读的人会以为它属于同一轮。

**为什么不能用进程级的「当前账号」**：消息处理是**按账号并行的**
（见 `internal/messages/dispatch.go`）。两个 goroutine 轮流写同一个变量，
出来的日志必然串号——而串号的日志比没有日志更坏，它会把注意力引到错误的账号上。

**所以绑在 ctx 上**：`context.Context` 是唯一能穿过 `llm.ChatClient` 与工具调用
而不改它们签名的载体。绑一次，往下每一层派生出来的 ctx 都带着它，
`Fields` 自动合并。代价是 `context.Context` 与 `config.Context` 会出现在同一行——
两个的区别是：**前者传「这一轮是谁」，后者是「这条日志额外说明什么」**。

**记内容多少的取舍**（结构 + 截断）：
- 提示词**只记结构**——systemChars / toolDefs / historyKept / historyDropped /
  contextBudget / fixedTokens / messages，外加用户那句话原文。全量落盘会让日志
  撑爆，也把家里的内容复制一份到别处。
- 推理**记截断后的前 200 字，且标出总字数**。只看得到开头会让人以为那就是全部。
  顺带一提：推理以前**只进控制台**，重定向到文件或 `LLM_SHOW_REASONING=0` 之后
  就彻底没了，「模型为什么这么答」只能猜——这次才第一次落进日志。
- 提交与返回**不记 key**。有一条用例专门盯着这点。

**提交与返回那两条为什么放在 `llm` 层、不放在工具循环层**：那一层才知道
打到了哪个 `host`、用了多久、推理有多长。两边都打就重了，排查时看到两条意思
相近的记录反而不知道该信哪条。

**会话历史那 5 处为什么没用 ctx**：`SessionHistoryStore.Load/Append` 的签名里
没有 ctx（只有 sessionID 与 accountID）。而 accountID 就在作用域内，直接给更简单
——**为了让加字段而改一个公共接口的签名，不值**。原来只给 `path`（`<账号>_<会话>.jsonl`，
账号其实藏在里面）要人去反推，而这几个都是「按无历史处理」这类**会静默影响回答**的告警。

**关联文件：** `internal/config/scope.go`、`internal/messages/handler.go`、
`internal/agent/loop.go`、`internal/llm/openai/openai.go`、`internal/tools/registry.go`、
`internal/llm/session.go`、`internal/config/scope_test.go`、
`internal/messages/accountlog_test.go`、`internal/llm/openai/logscope_test.go`

**验证：**
- [x] `make verify` 全绿；`CGO_ENABLED=1 go test -race -count=2 ./...` 全绿
- [x] `Test每一跳的日志都带账号` **逐行**核对（不是「出现过一次就算数」——
      那样在单账号用例里必然通过，而漏掉的恰恰是工具层那几行）；
      把 `Bind` 改成绑空集之后立刻变红
- [x] `Test两个账号的日志不会串`：两个账号各发一条，每条「答复已发出」都要同时
      对上**自己的** messageId——这才能证明是并行而没串
- [x] `Test模型请求与返回的日志也带账号`：带 host/model/stream，且断言
      **日志里不出现 API key**
- [x] `Test推理太长会被截断`：完整推理不进日志，但总字数与「共 N 字」的标记都在
- [x] `scope_test.go` 9 条：自动合并、调用点覆盖绑定、后绑覆盖先绑、
      **合并不改到绑定的原值**（否则一个下游能改掉所有人的字段）、
      派生 ctx 保留、空字段不绑、nil ctx 不崩
- [x] `internal/llm/openai` **这个包原先一个用例都没有**，这次补了 3 条

**⚠️ 记一条测试上的坑（已修，值得记）：** 抓日志最直接的做法是换掉 `os.Stdout`
截控制台输出。**那是有竞态的**——`Logger.write` 每次写入时现读 `os.Stdout`，
而恢复函数在写它，`-race` 当场报出 `DATA RACE`。改成读日志文件就没这个问题：
写入全在 `Logger.mu` 之下，且文件**始终全量**（控制台才按级别过滤），
所以连 `LOG_LEVEL` 都不用动，也就不会在用例之间互相影响。

## 2026-09-29 多账号并行：消息处理按账号分片，账号间不再互相等

**类型：** feature

**内容：**
- 新增 `internal/messages/dispatch.go`：`Dispatch` 取代原来的 `HandleFunc`。
  一条 dispatcher 读订阅流，**按账号**把事件分给各自的 worker。
- `internal/app/app.go` 的 `Serve` 换成 `Dispatch`，并加**有上限的停机排空**。
- 删掉 `messages.HandleFunc`（它就是那条串行 for-range，被整条替换而不是留成第二条路）。

**为什么：**
原来 `Serve` 只起**一个** goroutine，`for event := range subscription.C` 里同步
`Handle`。而一次问答是整整一轮 LLM（整体超时 120s），于是：

```
账号 A 发来一条 → 它那轮问答跑完之前，账号 B 的所有消息全部堵着
```

症状是「A 问完长问题之后，B 的消息过了好一会儿才有人回」——**不报错、不丢消息，
只是慢**，所以从界面上完全看不出是并发问题。

接上去的时候查过一遍，**底层每一层都已经是并发安全的**，缺的只是入口：

| 环节 | 状态 |
|---|---|
| 长轮询（每账号一个 goroutine，`bot/poller.go`） | 本来就并行 |
| `agent.Runner` | 无状态——每次调用把依赖按值传给包级 `Run` |
| `tools.Registry` | 有锁（`registry.go:46`） |
| `mcp.Source.Call` | 锁只护状态查询，`CallTool` 在锁外（`source.go:90-102`） |
| mcp-go stdio 传输 | `SendRequest` 用 `mu` 护写、`readResponses` 按 request id 解复用 → **真并发，不是排队** |
| 会话历史 | `lockFor(path)` **早就有 per-path 锁**（`llm/session.go:87`） |

最后那条是关键证据：**per-path 锁说明设计时就预期了并发，只是入口一直没开。**

**分片键为什么是 `channel:account` 而不是光 `account`**：`AccountID` 只在渠道内
唯一，渠道种类之间也可能撞号——接缝自己的查找键 `key(channelID, accountID)`
就是同一个理由。

**为什么同账号必须串行**（不是为了性能）：
- 历史是**单文件 append**，并发写会交叉；
- 模型上下文有前后依赖——同账号两条消息并发跑，会各自基于**同一份旧历史**作答，
  答完再交叉落盘。**那不会报错，历史会静静地错掉。**

**为什么并发上限不用配**：worker 数量 = 见到过消息的账号数，**不随消息量增长**。
所以并发天然有界在账号数上，不需要信号量或环境变量。

**为什么 dispatcher 往账号队列是阻塞发送、且队列无缓冲**：账号忙的时候
dispatcher 就停在那儿等，而不是把消息堆起来把「处理不过来」藏起来。压力接着
堵住订阅流，堵到接缝的 256 满了，接缝自己会记 Warn 并丢弃——沿用 `seam.go`
早就定下的语义，没有另发明一套丢消息的规则。

**⚠️ 顺带记一个并发带出来的坑**：`Serve` 原来在 `<-ctx.Done()` 之后直接返回，
而返回后 `app.Close()` 会关掉 MCP 子进程。串行时窗口小，改成按账号并行之后，
「某个 worker 正调 MCP 工具时子进程被杀」的概率明显变高——症状是「停机时最后
一条消息报工具调用失败」。所以 `Serve` 现在**提前退订并等 dispatcher 排空**再返回。
上限 10s：worker 里可能正跑着一整轮问答（模型超时 120s），干等会让 Ctrl-C 之后
进程两分钟不退出，那比丢几条已排队的消息更让人困惑。

**关联文件：** `internal/messages/dispatch.go`、`internal/messages/handler.go`、
`internal/app/app.go`、`internal/messages/dispatch_test.go`、
`internal/messages/dispatch_internal_test.go`、`internal/messages/handler_test.go`

**验证：**
- [x] `make verify` 全绿；`make test-count`（`-count=2`）全绿
- [x] **`CGO_ENABLED=1 go test -race -count=2 ./...` 全绿**——见下面那条关于
      `make test-race` 的记述，本次改动是第一次真正引入并发，值得单独记
- [x] `Test账号之间并行处理` 在把 dispatcher 换回串行实现后**超时变红**，
      换回来即绿——**这条钉的是行为本身，不是「代码看起来像并行的」**
- [x] `Test同一账号串行处理`：第一条卡在模型里时，第二条 300ms 内进不来；
      放行后恰好回两条、不多跑
- [x] `Test停机后不留goroutine`：连做 5 轮，每轮留两个**正在处理中**的 worker，
      订阅流一关 Dispatch 即返回，goroutine 数回到基线
- [x] `Test分片键带渠道种类`（放包内测，因为 `shardKeyOf` 不导出）：
      `ilink:account_002` 与 `memory:account_002` 必须是两个分片

**⚠️ 记一条仓库自身的矛盾（未改）：** `make test-race` **跑不了**——
`go test -race` 要 cgo，而 `CGO_ENABLED=0` 是本项目的硬约束，两者直接冲突。
也就是说 `make ci` 从建起来那天起就没法跑完。本次靠手工 `CGO_ENABLED=1`
绕过去验了竞态（那不影响出包仍是零 CGO），但**闸门本身还坏着**。
未做：给 `test-race` 单独开 `CGO_ENABLED=1`，或者承认 `-race` 不进本仓库的闸门。

## 2026-09-29 fka login 覆盖已登录的账号：登录前不读 .env，槽位永远挑中 1

**类型：** bugfix

**内容：**
- `cmd/fka/login.go` 抽出 `loginProvider()`：`config.LoadEnv()` 之后再 `Create()` 建账号表。
- `cmd/fka/login_test.go` 两条用例：`Test登录第二个账号不覆盖第一个` 与反向对照
  `Test没登录过就看到空表`。

**为什么：**
账号表是从 `ILINK_ACCOUNT_<N>_*` 这组**环境变量**读出来的（`ilink.AccountsFromEnv`），
而这些值只有 `config.LoadEnv()` 之后才在进程里。`fka serve` 走 `app.Build()`，那条路上
有这一读；`fka login` 刻意不经 app（少一层 socket，见 `runLogin` 的说明），**于是没人读**。

后果是账号表恒为空，`pickSlot("")` 于是永远挑中**槽位 1**：

```
登录第二个账号 → 凭证原地写进账号 1 的块 → 第一个账号被顶掉
```

界面上只显示一句「登录成功」，`.env` 里也确实多出一组看起来正常的
`ILINK_ACCOUNT_1_*`。**账号 1 是被覆盖了，不是登录失败**——这类缺陷从界面上完全
看不出来，只有拿账号 1 去收消息时才会发现它再也收不到了。

带 `--account N` 时 `pickSlot` 直接返回槽位号、不查表，所以那条路没被踩到。

**⚠️ 记一条辨析：** 用户报的「登录账号 2 覆盖账号 1」**不是这个**。那份 `.env` 里
4 个槽位的块全都完好，且槽位 1/2 的 `ILINK_USER_ID` 相同、3/4 也相同——**4 个槽位只有
2 个不同的微信身份**。同一个微信身份再登一次，新登录会接管服务端投递，旧槽位静默失联
（`data/history/` 里只有后登录的两个账号有会话文件，前两个一条消息都没有，而
`cursors.json` 里三个游标都在——**在轮询，只是收不到**）。所以那个现象发生在 iLink
服务端，不在 `.env`。要让多个账号同时处理消息，需要**多个不同的微信身份放进多个槽位**。

**顺带记一个空契约：** `Channel.StorageID()`（`internal/channels/ilink/adapter.go:374`）
返回 `ILinkUserID`，本该用来判「这个微信身份是不是已经登过」，但**全仓库没有任何地方
调用它**。未做：登录成功时拿它跟已有账号比对并明确警告。

**关联文件：** `cmd/fka/login.go`、`cmd/fka/login_test.go`

**验证：**
- [x] `make verify` 全绿（fmt-check → vet → test → build → smoke）
- [x] `Test登录第二个账号不覆盖第一个` 在注掉 `config.LoadEnv()` 后**立刻变红**
      （「该认得已登录的账号 1 … 实际：当前没有配置任何 iLink 账号」），加回来即绿
- [x] `make test-count`（`-count=2`）全绿

## 2026-09-29 make verify 从来没通过过：smoke 里的 SIGPIPE

**类型：** bugfix

**内容：**
- `Makefile` 的 smoke 第 1 步由 `$(FKA) tools | head -3` 改为「落盘再 `head -3`」。

**为什么：**
recipe 的 `.SHELLFLAGS` 是 `-eu -o pipefail -c`。`head` 读够 3 行就退出，`fka tools`
写剩余部分时收到 SIGPIPE（**141**），pipefail 把它当成失败——整条 smoke 挂在
「打印工具列表」上，**跟渠道接没接上毫无关系**。

`fka tools` 的输出恒多于 3 行，所以这一行**从 `1d327d5` 引入 Makefile 起就没通过过**。
这意味着能力链路唯一的自动闸门（技能读到了吗 / MCP server 连上了吗）一直是失效状态，
而它失效的表现恰好是「gate 报错」，容易被当成环境问题忽略过去。

**关联文件：** `Makefile`

**验证：**
- [x] 改前 `make verify` 挂在 `smoke`（Error 141）；改后 `make verify` 末尾打印
      「✓ 全部通过」，且 smoke 第 3 步的四个能力断言（`skills__load` / `skills__list` /
      `mcp__memory__search_memories` / `mcp__memory__remember_memory`）全绿

## 2026-09-28 文档 MCP server（只读）：权限行为钉死，agent 第一次搜到真实家庭文档

**类型：** feature

**内容：**
- `mcp/docs/`：**独立的可执行程序**（`package main`，出 `bin/fka-docs`），提供
  `search_documents` / `list_documents` / `get_document`。
- `mcp/docs/search.go`：关键词检索。逐份读 `extracted/*.md` 做子串匹配，词之间是
  「且」，片段**压成一行**、从**最靠前**的命中处取、首尾补省略号。
- 结果一律 **JSON 文本**而不是中文列表——模型读结构化数据比读排版过的文字更稳。
- 14 个用例，其中 6 个专门验权限行为。

**只声明真正能跑的工具。** `send_document` 要渠道的发送器、`delete_document` 要连
向量索引一起删，都还没接上——**声明一个跑不了的工具比不声明更糟**：模型会调它、
收到失败、再换个方式试，白烧一轮。`mode=vector` 同理：明确告诉模型还没建好、
可以改用 keyword，**不静默退化成 keyword**（用户会以为语义搜过了）。

## 2026-09-28 踩掉两个 Go 特有的坑

**坑一：`make([]T, 0, limit)` 会按 limit 预分配。** 检索为了「候选全过一遍」传了
`limit = 1<<30`，当场申请几十 GB 然后被 OOM kill（测试跑了 16 秒才死）。Node 那边
写的是 `Number.MAX_SAFE_INTEGER`，**JS 不预分配所以没事**——这是一个 Go 特有的坑，
而且症状是「测试卡住被杀」，不是报错，极难往「预分配」上想。
修法：`internal/store` 里把容量提示单独封顶（`capacityFor`），真正 append 时 Go
会按需扩容。**库函数不能因为调用方传了个大数就炸。**

**坑二：`ESCAPE '\' || '%'` 会被 SQLite 解析成两字符的转义表达式。**
`ESCAPE` 的参数是贪婪的，`'\' || '%'` 是一个拼接表达式，值是 `\%`（两字符），
于是报 `ESCAPE expression must be a single character`。通配的 `%` 本来就该在 Go
里 `EscapeLike(id) + "%"` 拼好当参数传进去，不该写进 SQL。

**⚠️ 权限行为已用测试钉死**（这是 viewer 由模型填、server 不做进程级绑定的唯一防线）：

| 身份 | 结果 |
|---|---|
| 属主本人搜自己的词 | 1 条 |
| 另一个属主搜自己的词 | 1 条 |
| **陌生人搜 alice 的词** | **0 条** |
| **陌生人搜 bob 的词** | **0 条** |
| **别人按 id 取 alice 的私有文档** | **拒绝**（且说清是 private 问题） |
| **缺 viewer_wxid** | **三个工具一律拒绝**，绝不当匿名放行 |

`get_document` 的权限判断是**按 id 精确命中之后**才做的，所以必须单独验——漏了它，
模型只要猜对 id 就能读到别人的文档。

**关联文件：** `mcp/docs/**`、`internal/store/db.go`

**验证：**
- [x] `gofmt -l .` 无输出；`go vet ./...` 干净；`go test ./...` 7 个包全绿
- [x] `CGO_ENABLED=0` 下主程序与三个 MCP server 全部构建成功
- [x] 真机：`fka tools` 把 `fka-docs` 当子进程拉起，列出三个工具
- [x] 真机：`fka ask "我上传过哪些跟居住证有关的资料？"` → 2 步命中真实文档
      `ff627ae9_居住证(卡)申领情况记录表.pdf`，片段来自真实施工结果，依据行正常
- [x] 真机（防线）：同一问题换成 alice 身份搜 bob 的私有「居住证」→ **0 条**，
      模型如实报告搜不到

## 2026-09-28 NAS 存储布局：路径推导、清洗、front matter、批注小节

**类型：** feature

**内容：**
- `internal/nas`：原件与解析结果的落盘。布局与 Node 版**逐字一致**（`files/{wxid}/{id前8}_{名}` + `extracted/…{名}.{ext}.md`），所以 Go 版能读同一份 `data/nas`。
- 文件名清洗、字节上限截断、相对/绝对路径互转、`filepath` 推导解析结果路径、YAML front matter 读写、按标记行刷新批注小节。
- 24 个用例把这些**非显然的坑**逐条钉住。

**为什么把这些坑逐条钉住而不是笼统测「能存能读」：**
- **冒号必须去掉**——解析器的挂载参数是 `docker run -v <路径>:<容器内路径>:ro`，名字里多一个冒号，挂载目标整个错位，而症状是「容器里找不到文件」，完全联想不到是文件名的问题。
- **保留原扩展名**——换成 `.md` 会让 `房产证.pdf` 与 `房产证.docx` 撞成同一落点，后者覆盖前者，症状是「两份文件只剩一份的解析结果」。
- **按字节而不是字符截断**——中文一字 3 字节、emoji 4 字节，按字符算会严重低估；不做这件事的症状是写文件报 ENAMETOOLONG 而用户只看到「文件保存失败」。
- **批注小节用 HTML 注释标记而不是标题**——正文里完全可能出现 `## 用户批注` 这样的标题，按标题定位会在重写时误删正文。
- **属主 id 走同一套字符集校验**（`internal/ids`）——`.` 与 `..` 能通过字符集，而它们是路径穿越。

**关联文件：** `internal/nas/**`

**验证：**
- [x] `gofmt -l .` 无输出；`go vet ./...` 干净；`go test ./internal/nas/` 24 个用例全绿
- [x] `CGO_ENABLED=0` 下主程序与 memory server 仍构建成功

## 2026-09-28 存储层版本化迁移 + 记忆 MCP server 端到端打通

**类型：** feature

**内容：**
- `internal/store`：`user_version` 版本化迁移，替掉 Node 版的「结构指纹不匹配就整库重建」。三条路径——全新库跑 v1；**Node 版的库走「认领」**（逐表逐列核对，对得上打 v1，对不上明确报错）；已有 version 逐条跑迁移，每条一个事务。`AssertCurrent` 在启动时对待迁移**拒绝启动**。
- v1 的 DDL 与 Node 版 drizzle-kit 生成的**逐字一致**，所以现有库能被直接认领。`__schema` 指纹表不再使用——指纹是代理指标，version 是事实本身。
- 三张表的读写（documents / memories / messages）+ 批注列的容错编解码。
- `mcp/memory/`：**独立的 MCP server 可执行程序**（`package main`，出 `bin/fka-memory`），提供 `search_memories` / `remember_memory`。主程序不再直接碰 memories 表。
- `internal/mcpboot`：各 server 共用的启动逻辑，**刻意只有三四个函数**——共享部分一旦开始长，就会把「独立」这件事吃掉。

**为什么：**
- 决定 5 是「保留今天的数据 + 版本化迁移」。Node 版的零迁移策略意味着**每加一个字段就重建一次库**，`documents.annotations` 就是这么加上去的；`db stats` 里那句「重建（旧库保留为 .bak）」对 330 条消息和几年积累的记忆来说是不能接受的。
- 认领路径必须**逐列核对而不是比指纹**：那份库若来自更早的 Node 结构，硬认领会让写入在约束上炸、读取少一列，而症状出现在很远的地方。宁可现在说清「缺 documents.annotations」。
- MCP server 做成独立可执行程序而不是主程序的子命令：它能单独构建、部署、重启，崩了不影响主程序。

**⚠️ 记忆 server 里那条 `WHERE` 是唯一的数据防线**：`viewer_wxid` 由模型填，server 按决定不做进程级绑定，所以 `(visibility = ? OR owner_wxid = ?)` 必须留在 WHERE 里、每次查询都带。收紧的做法（未做）是把身份绑到子进程启动参数上。

**关联文件：** `internal/store/**`、`mcp/memory/**`、`internal/mcpboot/**`

**验证：**
- [x] `gofmt -l .` 无输出；`go vet ./...` 干净；`go test ./...` 全绿（新增 store 包 8 个用例）
- [x] **真库认领**：`TestMigrate_真库认领` 复制 `data/db.sqlite` 后跑，`v0 → v1，documents=5 memories=1 messages=330`，一行未动，原库未被打开写入
- [x] 认领失败**不重建**：缺列的库返回 `ErrShapeMismatch` 并指名缺的列，无关表的数据仍在
- [x] `CGO_ENABLED=0` 下主程序与 memory server 都构建成功
- [x] 真机：`fka tools` 把 `fka-memory` 当 stdio 子进程拉起，列出 `mcp__memory__search_memories` / `remember_memory`
- [x] 真机：`fka ask` 走完 3 步（remember → search → 答），直接查库确认落库（`event | 2026年3月全家去了三亚 | wx_zhang | public`），原库仍是 1 条

## 2026-09-28 启动 Go 重写：Agent 层端到端可用

**类型：** feature（Go 版的第一块）

**内容：**
- 新增 `go/` 树，模块路径 `github.com/zjzhang-cn/fka-go`，`go 1.25`，**零 CGO**（`CGO_ENABLED=0` 出静态单文件）。
- **Agent 能力**：`internal/agent` 的工具循环（≤N 步、工具失败转成一句话喂回让模型改、到上限用无工具收尾、收尾失败也返回实话）；`internal/tools` 的接缝 + 五类 effect 放行 + 注册表（源前缀、参数校验、兜错、快照）；`internal/tools/mcp` 的 mark3labs 客户端（stdio + streamable HTTP、惰性连接、连不上跳过）。
- **LLM provider**：`internal/llm/openai` 用 `sashabaranov/go-openai`，流式 + **双超时**（整体 120s / 断流 5s，断流每收一块重置，长思考不被总时长砍断）+ 推理内容只进 stdout 不进答案。
- **架构改为构造函数链**替代 Cordis 插件树与 `cordis.yml`，装配点唯一在 `internal/app/app.go`。
- CLI：`fka ask`（无头跑一轮问答，**不等渠道层就能验 agent**）与 `fka tools`（列出放行情况与被挡下的工具）。
- 12 条决策与「为什么」记在 `go/README.md`。

**为什么：**
- Node 版要做到「单文件、零 CGO、Go 接管生产」，Cordis 的 `ctx` 动态注入与 `loader.await()` 不等异步 setup 那个坑在 Go 里都是纯负债；构造函数链把顺序错误从运行期挪到编译期。
- 记忆与文档管理搬进 MCP server（独立二进制 + stdio 子进程），所以 agent 必须先立起来——它是这套新架构的中枢。
- `fka ask` 是刻意的：渠道层还没搬，但 agent 的输入只有「问题 + 身份 + 工具」三样，先有它就能反复调提示词/阈值/工具描述。

**⚠️ 记一条已接受的风险：** `viewerWxid` 搬进 MCP 后变成**模型填的工具参数**，server 按决定不做进程级绑定。而这份产品按设计要把家人上传的不可信文档喂进 LLM 上下文，**提示注入是结构性暴露**——一次注入或模型判断失误就可能泄露别人的 private 文档。缓解：提示词里有独立的「工具参数的真实性」一节，代码里有醒目注释，`go/README.md` 置顶。未做：进程级身份绑定或签名参数。

**关联文件：** `internal/**`、`cmd/fka/**`

**验证：**
- [x] `gofmt -l .` 无输出；`go vet ./...` 干净
- [x] `go test ./...` 全绿（agent / llm / tools / tools.mcp 四个包）
- [x] `CGO_ENABLED=0 go build` 出 arm64 静态可执行文件
- [x] 真机：`fka tools` 连上 3 个真实 MCP 服务器（time / sqlite / memorix）并正确列出工具
- [x] 真机：`fka ask "库里有哪些表？"` 走完 2 步（调 `mcp__sqlite__list_tools` → 作答），推理内容只进 stdout、依据行正常

