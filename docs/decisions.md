# 决策记录

Go 版的每一条技术选择，以及**为什么是它而不是别的**。改动前先读这里——尤其是「后果」那一列，很多是**已接受的代价**，不是疏忽。

按提问顺序记录（决策是盘问出来的，不是拍脑袋定的）。

> **第 2、3、6、8 条讲的是文档 / NAS / 向量那条线**，它在 `1fae643` 与 `0fb673b`
> 之后**搬出了本仓库**。那些决策对搬走的那半边仍然有效，所以原文留着；
> 在**本仓库**读到它们时要知道：这里没有嵌入、没有向量、没有 parsers——
> 要那些能力，接一个外部 MCP server。

---

## 1. 完全替代，Node 版退役

**决定：** Go 版最终接手生产，Node 版不再维护。

**后果：** 没有兜底。Go 版独自承担生产，验收标准只能是「真机跑通」，不能靠「先回退到 Node 版」。

**为什么不是并存：** 并存要同时维护两套数据访问路径，而它们对同一份 SQLite 与 NAS 的理解会慢慢漂移；漂移之后查出来的结果不一致，排查成本比重写还高。

---

## 2. 嵌入只走 SCNet 云端，删掉本地实现

**决定：** 不做本地嵌入（原 Node 版是 transformers.js + ONNX 跑 bge-small-zh-v1.5，512 维）。只留 SCNet 云端（Qwen3-Embedding-8B，4096 维）。

**后果：**

| 后果 | 严重度 | 说明 |
|---|---|---|
| **文档正文要发到云端** | **高** | 「嵌入不联网」是原版写进 README 的隐私底座。这是**产品行为变更**，不是技术细节 |
| 语义阈值必然失效 | **高，且静默** | `SEMANTIC_MIN_SCORE = 0.994` 是在 512 维上、**只用 2 份文档**量过一次。换成 4096 维不重标，bot 会**一本正经引用不相关文件**，且不报错 |
| 切片参数要重定 | P0 | 350/450/60 是按 512-token 窗定的；SCNet 是 8192 token |
| 现有索引全废 | 中 | 2 个账号 × 512 维 → 4096 维，必须 `reindex` |
| `SCNET_API_KEY` 成硬依赖 | 高 | 今天没配任何 key 系统照样跑 |

**为什么：** Go 没有 transformer.js 的等价物。真要本地化只有两条路——CGO 绑 ONNX Runtime（tokenizer 要在 Go 里自己实现，HF tokenizer 生态不成熟，失败概率高），或者起一个 Python sidecar（多一个常驻进程，且「无 Node」变成了「有 Python」）。既然目标是零 CGO 单文件，走云端最干净。

**⚠️ 待办：** README 里「嵌入不联网」那句话要改；语义阈值要重标（**5 份文档标出来的阈值是过拟合**，先按保守值上线，等语料涨上去再标）。

---

## 3. 向量库手写薄层，零第三方依赖

**决定：** 不引入向量数据库。向量按账号存成文件，查询时内存展开 + 暴力余弦 + 阈值。

**依据（真实规模）：** 5 份文档、1 条记忆、330 条消息、11MB NAS。

**后果：** 规模涨到百万级向量才需要加 HNSW/IVF。**在家庭规模下暴力检索是几十毫秒的事**，加索引反而是负担。

**为什么可行——一个关键事实：** 向量库是**纯派生数据**。切片正文的真值在 `extracted/*.md`，向量只是它的索引。整个目录删掉跑一次 `reindex` 就回来了。所以向量库不需要事务、不需要备份、不需要和 SQLite 保持一致。**它的选型标准只剩「够快 + 够简单」。**

---

## 4. 接缝按实现数量遴选，不复刻 Cordis

**决定：**

| 接缝 | 原本实现数 | 处置 |
|---|---|---|
| `parsers` | 3（PyMuPDF / MarkItDown / SCNet OCR） | ✅ 保留完整接缝语义（路由、`*` 兜底、重叠拒绝启动） |
| `tools` | 4（builtin / memory / skills / mcp） | ✅ 保留 |
| `channels` | 1，但目标是「加渠道零改业务层」 | ✅ 保留（**产品需求驱动**，不是实现数量驱动） |
| `memory` | 1 | 保留**契约接口**（工具、命令、问答兜底都只认它） |
| `db` / `embedding` / `vector` | 各 1 | ⚠️ 降为直接用具体实现 |

**同时扔掉 Cordis 与 `cordis.yml`**，改成构造函数链，装配点唯一在 `internal/app/app.go`。

**为什么：** Cordis 在 Go 里是**三重负债**，每一条 Node 版自己的注释里就写着痛点：

1. **顺序错误从运行期挪到编译期。** `loader.await()` 不等插件的异步 setup，所以入口与测试必须显式等 services，忘了就启动到一半才炸。构造函数链里「B 需要 A」是函数签名上的依赖，编译器管。
2. **`ctx` 动态属性在 Go 里是负资产。** `ctx.db` / `ctx.tools` 是运行时注入的属性；Go 的结构体 + 接口在编译期就把「谁有什么」写死。
3. **`effect` 逆序卸载在 Go 里是 `defer`。** 不用再造一套生命周期。

**代价：** 「加一个 provider 要改一行 Go 而不是一行 YAML」。**这在 Go 里是好事**——改完就编译过，不需要重启才知道配错了。

---

## 5. NAS 布局不变 + `user_version` 版本化迁移

**决定：** 存储布局与 Node 版**逐字一致**（`files/{wxid}/{id前8}_{名}` + `extracted/…{名}.{ext}.md`），所以能读同一份 `data/nas`。数据库把「今天那份 schema」定为 **v1**，用 SQLite 自带的 `PRAGMA user_version` 记账。

**为什么这很重要：** Node 版**没有迁移**——schema 直建 + 结构指纹，指纹不匹配就整库重建（旧库改名 `.bak`）。那是开发阶段的合理选择，但对已跑起来的库意味着**每加一个字段就重建一次**。`documents.annotations` 就是这么加上去的。`db stats` 里那句「重建（旧库保留为 .bak）」对 330 条消息和几年积累的记忆不能接受。

**三条路径：**

| 场景 | 行为 |
|---|---|
| 全新库 | 跑 v1 起全部迁移 |
| **Node 版的库**（`user_version=0` 但结构是今天的） | **认领**：逐表逐列核对，对得上就打 v1；**对不上明确报错，不猜、不重建** |
| 已有 version | 逐条跑高于它的迁移，**每条一个事务** |
| 启动时还有待迁移 | **拒绝启动**并说清差什么 |

**为什么用 `user_version` 而不是自建 `__migrations` 表：** 它在事务里一起提交或回滚，不需要额外保证「台账与数据同生共死」。自建台账在「台账写成功但数据没改」时会永久不一致。

**为什么 `__schema` 指纹表不再使用：** 指纹是「结构是否一致」的**代理指标**，而 version 是**事实本身**——它不会因为 DDL 文本改写而误报。

**真实验收：** `TestMigrate_真库认领` 复制 `data/db.sqlite` 后跑一遍 → `v0 → v1，documents=5 memories=1 messages=330`，一行未动。

---

## 6. ExifTool 走系统命令 + exec

**决定：** 不 vendor exiftool 二进制，`exec` 调系统装的。

**为什么：** Go 生态没有 `exiftool-vendored` 的对应物，而**纯 Go 做不到**——Office/PDF 元数据（标题、作者、创建时间）没有可靠的纯 Go 解析方案。vendor 一个 5MB 二进制进仓库会让仓库膨胀且跨平台要各放一份。`doctor` 负责体检与提示。

---

## 7. iLink 协议：行为一致 + 修掉已知缺陷

**决定：** 13 个协议文件 1:1 翻译，**外加**修掉两个已知缺陷（`image` 发送路径「尚未真机验证」、`encryptMedia` 注释已过期）。

**照搬不修的协议事实**（它们不是实现瑕疵）：

- `ret` 成功时**完全省略**字段而不是返回 0——判成功必须用 `isSuccessRet`，写 `ret == 0` 会把所有成功当失败；
- message id 是 uint64，Node 里靠 `quoteLargeIds()` 文本级预处理保精度（Go 要按字段处理，不能全局用 `UseNumber`）；
- `image_item` 的 aeskey 是**扁平 hex**、`file_item` 的 key 在 `media.aes_key` 的 **base64**，两种形态并存；
- 上传成功**只看 `x-encrypted-param` 响应头**；
- `context_token` 只能从收到消息时捕获，**无法主动发起会话**（这也是「家庭提醒/自动总结」至今做不了的根因）。

**不需要决策的部分：** 「每账号一个 worker 线程」在 Go 里用 goroutine + `recover` 天然等价，而且 CPU 密集的 ONNX 推理已经砍掉了，goroutine 毫无压力。

**⚠️ 风险：** 修 `image` 这类**尚未真机验证**的路径需要真机确认。改完会明确标出「未经真机验证」，不假装它和已验证路径同等级。

---

## 8. 平移 + 还 3 条前置债

**决定：** 现有功能 1:1 搬，不补新功能。但必须做三条——

1. **语义阈值重标**（换模型必然引发）
2. **切片参数重定**（换模型必然引发）
3. **启用 SCNet OCR**（扫描件正文为空但状态 `ready`，是 P0 且**静默**——房产证/保单/发票搜不到且不报错）

**前三条不是「债」，是「刚做的决定所引发的前置义务」**——不处理，Go 版不是「少功能」，是「算错」。

**明确不做的（真债，可选）：** RRF 融合、记忆向量索引、自动分类、家庭提醒、自动总结、知识图谱。这些是**新功能**，不是搬迁义务。

---

## 9. MCP client 用社区实现 mark3labs v1.1.1

**决定：** 不用官方 `modelcontextprotocol/go-sdk`，用 `github.com/mark3labs/mcp-go` v1.1.1。

**历史：** 最初因为官方 SDK 要 Go ≥ 1.25 而本机是 1.23，被迫切到社区库。工具链升上来之后约束消失，**确认继续用社区库**（不折腾，且 API 稳定）。原先钉在 v0.40.x，理由只剩「v1.0.0+ 要 Go 1.25.5」；本机工具链到 go1.27.1 之后**那条理由也没了**，于是 2026-09-29 升到 v1.1.1。`go.mod` 的 `go` 指令因此从 1.25.0 抬到 **1.25.5**（依赖的最低要求）。

**⚠️ 升级踩到的唯一一处行为变化（不是编译错，是运行时的）：**
`mcp.LATEST_PROTOCOL_VERSION` 在 v1.1.1 起是 `2026-07-28` —— **无会话的 stateless 协议**，
文档明说它「没有 initialize 握手」。而我们仍在走 initialize 握手，于是客户端认定自己
stateless、**不再发 `Mcp-Session-Id`**，而服务器那边会话已经建好：第二次请求得到
`session terminated (404). need to re-initialize`。所以 `internal/tools/mcp/client.go` 报的是
`mcp.LATEST_LEGACY_PROTOCOL_VERSION`（2025-11-25，仍用握手的最新版）——**再升 SDK 之前先读这一段**。

---

## 10. LLM provider 用 `sashabaranov/go-openai`

**决定：** 不用官方 `openai/openai-go`，用社区的 `github.com/sashabaranov/go-openai`。

**用它的一个后果：** 它的 `ChatCompletionRequest` **没有 `enable_thinking` 字段**（推理开关是非 OpenAI 规范的扩展）。走 `ClientConfig.HTTPClient` 包一层 `bodyInjector` 注入——**只重排请求体外层的键序，不碰 `messages` 里的任何内容**。这是安全的：provider 的前缀缓存认的是**分词后的消息序列**，不是 JSON 字节序。

**另一个后果：** `tool_choice` 必须是**裸字符串** `"auto"`。SDK 自带的 `ToolChoice` 结构体序列化成 `{"type":"auto"}`，而规范里 `auto` 是 tool_choice 的**取值**不是**类型**，会被服务端拒（实测 DeepSeek 返回 422）。

---

## 11. SQLite 纯 Go 驱动，零 CGO

**决定：** `modernc.org/sqlite`，`CGO_ENABLED=0` 出静态单文件。

**代价：** 比 CGO 版（`mattn/go-sqlite3`）慢 20-50%。**在 5 份文档 / 330 条消息的规模上完全不可测量。**

**收益：** `CGO_ENABLED=0` 直接出 arm64 静态可执行文件，不需要 C 工具链、不需要带 glibc 的镜像。

**顺带的坑：** 连接池限制为 1——SQLite 写是全库串行的，多连接会让「开始事务」和「另一条连接写」撞上，表现为 `SQLITE_BUSY` 而不是逻辑错。另外 DSN 要加 `_txlock=immediate`，否则两个「读-改-写」事务会在升级写锁那一步死锁。

---

## 12. 记忆与文档管理搬进 MCP，IPC 保留 1:1

**决定：** 记忆与文档管理做成**独立的 MCP server 可执行程序**（`mcp/<名字>/`，各自 `package main`、各自出二进制），由主程序从 `mcp.json` 当子进程拉起。IPC（CLI ↔ 服务）保留 1:1，含降级与快照语义。

> **后来只剩一个 server，那句「共用的启动逻辑」也就没了意义**：`0fb673b` 把启动那几行
> 放回 `mcp/memory/main.go`，`internal/mcpboot` 这个包被撤掉。现在树里只有记忆 server。

**⚠️ 这条带一个已接受的风险：`viewer_wxid` 变成模型填的工具参数。** 详见 [permissions.md](permissions.md)。

**为什么 MCP server 要独立可执行程序而不是主程序的子命令：** 它能单独构建、单独部署、单独重启，崩了不影响主程序。当时计划里的共享启动逻辑刻意只有三四个函数——**共享的部分一旦开始长，就会把「独立」这件事吃掉**；而只有一个 server 时，那个包本身就成了纯间接，撤掉更干净。

---

## 13. 日志落盘用普通文本，不用 JSON

**决定：** `<安装根>/logs/<日期>.log`（2026-09-29 起，原先是 `app.<日期>.log`）一行一条纯文本：
`<UTC 时间戳> [级别][账号][哪一段] 消息 键=值 …`。控制台与文件共用同一个渲染函数。

**为什么：** 这份日志**几乎只在排查时被 `grep` / `tail` / `cut` 读**，而它不是被机器消费的。
JSON 要写 `grep '"account":"acct-1"'`，纯文本是 `grep 'account=acct-1'`；
`cut -d' ' -f3` 一下就是级别、账号与阶段。**为人读的东西就用为人读的格式。**

**⚠️ 代价与要自己守住的东西：**

| 代价 | 说明 |
|---|---|
| 换行必须自己转义 | JSON 时代 `\n` 是天然的；纯文本不转，工具参数、推理片段、驱动报错都能把一条日志撑成两三行，后半截看起来像**另一条**的 |
| 字段顺序要排 | Go 遍历 map 是随机的，不排的话同一份字段两次落盘 diff 全是噪音 |
| 带空格的值要加引号 | 否则 `text=第一句 第二句` 会被读成两个字段——那个字段根本不存在 |
| 结构化查询没了 | 真要按字段做统计得再写一个 JSON 输出；**到那天为止不值**（`jq` 那条路已经够用） |

**顺带修的一条真 bug：记忆 server 把 INFO 日志打进了 stdout**——那是 JSON-RPC 通道。
`mcp.json` 给了 `env` 时子进程只拿到那几项，**拿不到 `LOG_LEVEL`**，级别落回默认的
debug 就一定会打进去。所以那个进程的日志**一律 stderr**。

---

## 14. 命令行参数只解析一次

**决定：** 整份 CLI 只有一份参数名单（`cmd/fka/ask.go` 的 `knownFlags`）与一个解析函数
（`parseFlags`）。`run()` 解析一次，把结果传给每个子命令；**没人再扫原始 args**。

**为什么（这条是被一个真 bug 教出来的）：** 之前 `runAsk` 直接
`strings.Join(args, " ")` 当问题，于是

```
fka ask --session aabbcc 你的名字加小航
→ 送进模型的 user 消息：--session aabbcc 你的名字加小航
```

**参数原样跟着问题进了用户提示词。** 症状极难认：模型答得挺好，只是把参数当成问题
的一部分——「刚才我说的是啥」会连着 `--session aabbcc` 一起复述。证据在
`data/history/<会话>.jsonl` 里，而扫日志是扫不出来的。

**为什么不是「在 `ask` 里把认识的参数剔掉」：** 名单会增长，每加一个参数就多一个
「忘了剔」的机会，而那种失败**一次都不报错**。「认不认识一个参数」「它算不算问题」
必须各只有一处判断。

**顺带定了两件事：** 认不出的参数**报用法错（2）**，不当问题——`--sesion x` 被当问题
的话模型会拿到一句莫名其妙的话并认真回答；`--` 之后一律当问题，给「以 `-` 开头的问题」
留一个出口。


---

## 15. `ask` 不给 `--session` 时每次生成一个新会话
**决定：** 兜底从常量 `"cli"` 改成 `cli-<uuid v4>`（先落在 `cmd/fka/sessionid.go`，
第 21 条之后搬到 `internal/channels/cli/session.go`）。
`--session` 与 `FKA_SESSION` 显式给了就照用，那仍然是连续会话。

**为什么：** 会话历史按会话落文件（`<安装根>/data/history/<会话>.jsonl`），而兜底是
一个**固定名字**——于是**每一条**不带 `--session` 的 `ask` 都在续上一条。症状是模型
忽然提起你半小时前随口问过的那件事，而**命令行里什么都没变**；更糟的是它只在恰好
问到相关话题时显形，没有任何线索指向「会话」这个原因。

**顺带钉住两件事：**

- `cli-` + UUID 正好 **40 字符**，而 `llm.safeSegment` 把会话段截到 40。**截断是静默的**：
  名字撞了就是两个不相关的会话共用一份历史，同样的 bug 换个更隐蔽的方式回来。
  这条由 `internal/llm/session_test.go` 钉住——改前缀长度会当场红。
- 会话 id 在 `--debug` 输出里印出来。不印的话，「接着刚才那条 CLI 问的继续」
  在每次会话都换 id 之后**没法做到**——用户拿不到那个 id。
  随机源用 `crypto/rand`：这个 id 唯一的作用就是每次都不一样，可预测的随机数
  在多开几个终端时恰好最容易撞，而撞的表现（两个会话共享历史）极难查。


---

## 16. HTTP 类型的 MCP：两种传输 + headers 必须真发出去

**决定：** `mcp.json` 里 `url` 那类服务器可以给 `transport`（`sse` / `http`）；
不给就按 **path 是否以 `/sse` 结尾**猜，猜出来的结果记进日志。`headers` 两个分支都真的交给 SDK。

**为什么是这两种：** MCP 的 HTTP 传输有两个互不兼容的世代。老式 `sse`：GET 开着一条流，
服务端先发一个 `endpoint` 事件告诉你往哪 POST，之后 POST 只回 `202 Accepted`，
**真正的响应从那条流上回来**。streamable：直接 POST 那个 url，响应在响应体里。
拿新的客户端去 POST 一个 `/sse` 地址，拿到的是 `404 session terminated`——
而配置读回来完全正常，一眼看去像是「那台服务器坏了」。

**为什么不给「先试一种，失败再换一种」：** streamable 的失败原因五花八门（401、404、
超时、DNS），只有一部分说明「这是台老服务器」。为了让那一小类能落到 SSE 上得给错误
分类，而分类判错时的表现是**两段都试过、两段都失败**，用户拿到的是一条更长的错误信息，
却没有「该写什么」的建议。不如猜错时**把该写什么直接说进错误里**。

**顺带修掉两个静默失效**（都是 HTTP 那条路独有的，stdio 全都正常，所以极难自己发现）：

- **`headers` 之前根本没发出去。** 配了 `Authorization` 的服务器只会回 401，
  而 `mcp.json` 读回来是完整的 —— 看起来像服务器坏了。现在有一条用例让服务器把
  收到的头记下来。
- **SSE 那条长连 GET 流绑错了 ctx。** 它必须活过整个连接期，而连接期的 ctx 在握手
  一返回就被 `defer cancel()` 取消了。`initialize` 是 POST，所以**握手照样成功**，
  之后 `tools/list` 才炸：本地服务器说 `Invalid session ID`（会话随流一起关了），
  真实服务器则是等不到响应、60 秒后超时。现在 `Start` 拿的是
  `context.WithoutCancel(ctx)`，流由 `Close()` 收（`Close` 不阻塞）。

**这两条为什么以前没人发现：** HTTP 那条路**一个测试都没有**，`make smoke` 只起 stdio
的记忆 server。现在 `internal/tools/mcp/http_test.go` 用 mcp-go 自己的两种 server
起 `httptest`，把「两种传输都能连上并调得动」「显式 transport 压过自动」
「headers 真的发出去了」三条钉住 —— 全程离线，不依赖任何外部服务器。

---

## 17. 把「回话」做成内置工具（`internal/tools/reply`）

**决定：** 新增 `internal/tools/reply`，把「往当前会话发文字 / 文件 / 图片」做成三个
工具（`reply__send_text` / `reply__send_file` / `reply__send_image`）交给模型。
effect 都是 `send`，**默认关着**；这是整个仓库**唯一的内置工具源**。

**为什么这不算破「不带任何内置能力」那条原则：** `reply` **不是能力，是输出通道**。
能力（查资料、记忆、执行、出网）仍然只从 MCP 与 skill 进来；这里只是把「回话」这件事
的调度权也交给模型——最终答案本来就走同一条出口（`Senders.Text`），现在模型可以先发
一段说明、再发文件，或分几条发。它不拥有数据、不出网、不拉进程。区别是：一个能力源
决定了 agent **能知道什么**，输出通道只决定**它怎么把结果说出来**。

**为什么需要它：** 在这之前，模型产出的文件 / 图片**没有任何路径到达用户**——工具结果
里的图片只作为附件喂回模型自己（`loop.go`），从不发给渠道。`tools.Context.Reply`
这个接缝早就存在、也有端到端用例，但**没有真实消费者**：能力只从 MCP 与 skill 进来，
而 MCP 是独立子进程，够不到 agent 的渠道连接。所以要让模型能发文件，就只能有一个
内置源。

**已接受的代价（三条，都在代码里落地）：**

1. **任意文件外带口。** `Reply.File` 收本机路径，而路径是模型填的——提示注入是这份
   产品的结构性暴露。所以 `reply.resolve` 每次调用都过四道闸：只认**相对路径**
   （绝对路径与 `..` 拒）、求值符号链接后仍须在**发送根**内、只发**普通文件**、不超
   **64MiB**（与 iLink 出站上限取齐）。
2. **发送根默认是 `<安装根>/sandbox`**（与 `fka-sandbox` 的沙盒根同目录，`SANDBOX_ROOT`
   或 `FKA_SEND_ROOT` 可改）。落在沙盒里意味着「模型能发的，正是它本就能读写的那些
   文件」，没有把权限放大。要放开到别处就显式设 `FKA_SEND_ROOT`。
3. **iLink 仍然无法主动发起会话**（`context_token` 只能从收到的消息里捕获，见第 7 条）。
   所以这里的「主动发送」只是**在收到的那条会话里回复**，不是给任意人发消息——那条
   在当前渠道上做不到。

**`Reply` 接口补了 `Text`：** 原先只有 `File` / `Image`，文字走不到工具层。补上之后
模型才有「先说明、后发文件」的能力，而不只是「把文件塞进最终答案后面」。

---

## 18. 字节端到端：远端沙盒的文件经 MCP 送到渠道

**问题：** 第 17 条的 `reply` 工具收的是**本地路径**，SSE 访问的沙盒在别的机器上，
文件没有本地路径——`reply__send_file` 在远端沙盒上必然失败。

**决定：** 文件以**字节**贯穿「沙盒 → MCP → agent → 渠道」，本地文件仍走路径。

- 沙盒用 MCP **标准内容块**返回字节：嵌入 `resource` 的 blob（二进制安全）、`image`、
  `audio`；
- 意图用 **`annotations.audience=["user"]`** 表达——这是 MCP 规范字段，本义就是「这段
  内容的预期消费者是谁」；
- agent **只认这个字段**：audience 含 user 的块由循环经渠道发给用户，其余（图片）仍
  作为附件喂模型；
- 渠道出站 `SendMediaParams` 增加 `Data []byte`，iLink 底层 `UploadMedia` 本就收
  `[]byte`，路径版本只是先读文件再委托。

**为什么这样解耦：** 沙盒与 agent 之间**没有私有约定**——只有 MCP 规范定义的内容类型
与 audience 字段。agent 不特判来源（不看工具名、不看 server 名），任何 MCP server 返回
audience=user 的块都能被投递；沙盒也不需要知道渠道、传输、用户。**耦合从「两个进程共享
一块磁盘」降成「两个进程遵守同一个 MCP 字段」。** 反过来说，agent 里一旦出现
`if tool == "sandbox__export"`，这条解耦当场作废——所以它没有。

**新增 sandbox `export` 工具**（`read` 不动，仍是"给模型看"，不自动外发）。模型要「把文件
给用户」时用 export；这解决了「模型只是读图却被自动发出去」的错误。

**闸门：** 投递要求 `LLM_TOOL_EFFECTS` 含 `send`（与 reply 同一道闸）。MCP 工具都是
`external`，不加这道闸，任何被配置的 server 都能让 agent 往用户发东西。

**边界：** export 与 read 同一条路径检查（只读沙盒根内）；大小上限 export 32 MiB、
agent 侧投递 32 MiB、渠道侧 64 MiB。

---

## 19. 第二个渠道：网页（SSE）

**决定：** 新增 `internal/channels/web`，浏览器经 **SSE 收、POST 发**。这是
`port-plan.md` 待办 #4 说的「第二个渠道」——**接缝与业务层一行未改**，只加了一个
`Provider`，在 `cmd/fka/serve.go:channelProviders()` 里多一行。

**用 SSE + POST，不用 WebSocket：** SSE 只能服务端→客户端，客户端→服务端另给
`POST /messages`。这与接缝一一对应（出站走 Senders、入站产 `InboundMessage`），
且不引入 WebSocket 那套连接状态管理。

**多用户身份：token → PrincipalID，cookie 承载。** 每个用户一个 token，配在用户表
`<安装根>/web-users.json`（`WEB_CHANNEL_USERS` 可改路径；**含密钥，不入库**）；
单用户也可只设 `WEB_CHANNEL_TOKEN` + `WEB_CHANNEL_PRINCIPAL` 作为回落。`POST /login`
用 token 反查 PrincipalID 后种一个 HttpOnly cookie：`EventSource` 与 `<img>` 都带不了
自定义头，只有 cookie 能让 SSE、媒体、POST 三处统一认证，且令牌不进 URL、不进日志。
`/messages` 另收 `Authorization: Bearer` 给非浏览器客户端。

身份名的裸值（`alice`）落到 `PrincipalID` 时补成 `web:alice`——命名空间由代码加。
**内部会话键纳入 principal**（`<principal>:<原始会话>`），所以两个用户各取同名
conversation id 也不会串历史/记忆/SSE 房间；客户端只看到自己的原始 conversation。
身份由配置决定，**模型碰不到**（不是工具参数）。

**流式：实现 `EmitterProvider`。** `channels.Emitter` 接口早就存在却无人实现，
SSE 是它最自然的落点：推理增量与工具调用边发生边推。**最终答案不走 Emitter**——
它经 `Senders.Text` 送出，`Emitter.Answer` 必须置空，否则同一份文本会推两次。

**已接受的取舍：**
- **单账号**：所有用户共用一个 `AccountID`（`WEB_CHANNEL_ACCOUNT`，默认 **`default`**
  ——**刻意不叫 `web`**，账号是跨渠道的选择器，与渠道种类同名只会让人分不清）。
  它进会话键，因此也决定历史文件名。身份区分靠 `PrincipalID`。
- **并发：不同 token 并行、同一用户串行。** 消息层的分片键已含 `PrincipalID`
  （见 `internal/messages/dispatch.go` 的 `shardKeyOf`），而会话历史本就按身份分
  命名空间（`<principal>:<原始会话>`），所以两个用户天然各写一份历史、不存在共享
  状态。早先只按账号分片时**所有 web 会话串行**，那正是「两个 token 也并行不起来」
  的根因；那时说的「改成一用户一账号实例」不再是必需。
- **File/Image 只发不收**（`receive=false`）。业务层没有入站媒体的消费者
  （收到媒体会如实拒答，见 `internal/messages`），声明能收却没有那条路就是
  「声明一个跑不了的能力」。等有落点再补 upload。
- **出站媒体走内存暂存 + `GET /files/{id}`**（10 分钟 TTL、64 MiB 上限），SSE 事件
  只给相对 URL，不内联 base64。
- **前端不打包**：`web/` 是仓库里的源码，运行期从 `<安装根>/web`（`WEB_CHANNEL_STATIC`
  可改）读，**不用 `go:embed`、不上构建链**。

**开关：** `WEB_CHANNEL_ADDR` 设了才启用（产出 1 个实例）；没设则 0 实例、行为与
以前一致。启用时必须至少配一个用户（用户表，或单用户回落的 `WEB_CHANNEL_TOKEN`），
否则拒绝启用。

## 20. MCP sandbox 多租户隔离

**问题：** `fka-sandbox` 是一个进程、一个沙盒根，所有会话共享同一棵树——A 建的文件 B
能看到。而内容不可信早就是既成前提（提示注入），所以隔离不能只防手滑。

**决定：每个 `PrincipalID` 一个沙盒子目录。**

- **身份通道用 MCP `_meta`**：agent 在每次 `CallTool` 时把 ctx 上的 `principal`
  注入 `_meta["fka/principal"]`。**模型改不了它**（不是工具参数），stdio/HTTP 都通。
  反过来说，**不能用工具参数**（记忆 server 的 `viewer_wxid` 就是那种）——那能被提示
  注入改写；对文件系统来说，改写 = 读别人的文件，比 SQL 过滤漏一行严重得多。
- sandbox server 从 `_meta` 取身份，把根切成 `<基根>/<safe(principal)>`，按需创建。
  `run`/`read`/`export`/`import` 与 MCP resources 全在这一层里执行；bwrap 只把这层绑成可写并 chdir。
- **resources 也按租户**：固定资源 `sandbox:///` 读出本租户文件清单，模板 `sandbox:///{path}`
  按路径读文件（分类与 `read` 一致）。读取请求的 `_meta` 同样带身份，缺身份 fail-closed。
  ⚠️ **列举只能是静态的**：mark3labs 的资源模板没有 `list` 回调（不像 JS SDK），按租户动态
  列举做不到——所以用「读根 URI 得到清单」兜底，而不是把文件注册成一堆独立资源。
- **缺身份 fail-closed**：没有 `_meta` 直接拒绝，**绝不落到共享目录**——那等于
  「绕过 agent 就全共享」，多租户下最坏的失败形态。
- 执行日志（JSON Lines）多一个 `principal` 字段，多租户审计才分得清是谁跑的。
- **宿主只读暴露一并堵上**：bwrap 不再 `--ro-bind / /`，只绑命令需要的
  `/usr /bin /lib /etc` 等。原来整盘只读映射进来，租户能 `cat <安装根>/.env`
  （LLM / 微信凭证）、`data/*.sqlite`（记忆原文，**绕过记忆 server 的 SQL 权限过滤**）、
  `logs/*`，以及**兄弟租户目录**——单租户时可接受，多租户下就是凭证泄漏。
- **自装工具链点名挂进来**：最小根只绑 `/usr /bin /lib /etc` 等系统位，而 node（常在
  `/opt`）、conda/python（常在 `~`）、nvm/pyenv 都不在里面——不挂的话沙盒里
  `command not found`。用 `SANDBOX_RO_BINDS`（冒号分隔的路径）或 `--ro-bind` 把它们
  **只读**挂进去；只暴露点名的目录，不放整个 `/opt` 或家目录。系统包管理器装的
  解释器（在 `/usr` 下）本来就能用。

**解耦：** 两侧只共享一个协议字段（`_meta` 的键），sandbox 不认识 agent、agent 不认识
sandbox；键在两边各写一份字面量（sandbox 自给自足，不能 import 树内包），改动要一起改。

**已接受的代价：**
- `safeSegment` 把非法字符换成 `_`、截到 64 字符，理论上两个不同身份可能撞目录名；
  与历史文件名同一个取舍（那处也重写了一份）。
- 配额/清理**还没有**：bwrap 隔离了写路径，但没隔离磁盘占用。租户目录按需创建、
  只增不减，需要运维侧清理。
- 身份本身由渠道决定：web 现在能按用户区分，iLink 用裸 wxid（未加命名空间），
  CLI 默认 `cli`。**要按用户隔离，身份必须先能区分用户**。

---

## 21. CLI 是第三个渠道（`internal/channels/cli`）

**问题：** 四条前端、两套走法。ilink / web 走完整接缝（`channels.Channel` →
`messages.Handler` → `agent.Runner`），而 `ask` / `chat` 绕开接缝、各自把判定
（空问题 / 缺身份 / 没接模型 / 模型出错 / 空答复）、回显、会话策略又写了一遍，
措辞还各不相同（空答复一边说「我没想出该怎么回」，一边输出一个空行）。
「加一个渠道 = 一个 provider + 装配一行、业务层零改」对 CLI 并不成立。

**决定：** `ask` / `chat` 做成完整的渠道实现，和微信、网页同一模子。

- **编排抽成 `messages.Ask`**：绑定归属 → 逐条判定 → `Runner.Run` → 归类成
  `AskOutcome{Result, Explain, Cause}`。**它不发送、不呈现**；`Handler.Handle =
  Ask + Deliver`，`reply` 更名导出为 `Deliver`，CLI 与渠道用的是同一个 `Ask`。
  于是「同一套规则只写一遍」这条底线成立。
- **`internal/channels/cli` 只实现 `channels.Channel`，不实现 `channels.Provider`**：
  命令行没有账号维度、也没有可注册的实例工厂。它不进 `Service.StartAll`——由
  `cmd/fka` 持有生命周期（`Start` 阻塞到会话结束）。`serve.go`、`app.go`、
  channels 协议**零改动**。
- **答案通道唯一**：答复一律经 `Senders().Text`（`say`）发出——标签进 stderr、
  正文进 stdout；回显的 `Answer` 置空，防双份（web 已是这个口径）。
- **冻结面**：web / ilink 两个包一行不改；`InboundMessage` 里没有「图片字节」的
  位置，所以 `@引用` 仍留在 cmd 展开、经 `AskInput.Images` 递给循环——渠道协议
  不为 CLI 改。

**后果：**

- 已立案的对外可见变化：**空答复**从「输出一个空行」变成统一的 `replyEmpty` 句
  ——交互式前端与渠道从此一致。
- 行为不变量照旧：stdout 只有答案、退出码 0/1/2、`cli-<uuid>` 与 40 字符预算、
  `[推理]/[工具]/[助手]` 与 `● 助手` 的格式与去向、命令族、转圈时序——由
  `internal/channels/cli` 的用例钉死。

---

## 22. 用量：挂结果 + 可扩展通知，不进 `agent.Emitter` 的方法集

**决定：** 每轮问答的真实 token 用量（服务端报的）走两条路：

1. **挂结果**：`llm.TokenUsage` → `agent.RunResult.Usage`（`llm/types.go`），消息层、
   日志、将来的消费者都能读；
2. **通知渠道**：工具循环在**轮末**把整轮之和包成一条通知，发给实现了**可选接口**
   `channels.Notifier` / `agent.Notifier` 的前端。当前只有一条通知：
   `UsageNotice`。

前端各取所需：CLI 渠道把用量**缓冲到 `say`（打答复时）**再补一行——呈现顺序
「答复 → 用量 → debug」不变；默认回显（常驻渠道）记一条 `用量` 日志；网页（自定义
回显、本轮冻结）暂不收。

- **采集**：openai 请求带 `stream_options.include_usage`；收尾帧（`choices` 为空、
  只带 usage）在 `len(chunk.Choices)==0` 的 `continue` **之前**接住。
- **缓存命中**：`prompt_tokens_details.cached_tokens` 收进 `TokenUsage.CachedTokens`。
  它是 **OpenAI 兼容的约定**（DeepSeek、Qwen/DashScope 的兼容模式都报它），**不是跨协议
  通用**——Anthropic 那种叫 `cache_read_input_tokens`，要在它的 provider 包里翻译。
  它是**输入的子集**，展示时贴在「入」后面；端点不报就是 0、不显示，不会错。
- **聚合是整轮之和**：工具循环两处 `r.chat` 调用点都累加（含收尾那次 maxSteps 兜底）；
  断流重试**不重复计**（只有成功那次有 usage 帧）。
- **服务端不给就没有**：零值表示没给，**不用 `llm.EstimateTokens` 冒充**——估算只
  服务历史预算；零值不发通知、不打那行。

**为什么通知走可选接口，而不是给 `Emitter` 加一个方法：** `Emitter` 是**一轮内的
过程事件流**（推理按块、每个工具一次、答案一次，可能来很多次、是增量），用量是
**一轮的账目**（一次性、轮末才有定论）。给 `Emitter` 加第四个方法 = 四个已有实现
全部被迫实现一个多数前端用不上的方法——**接口的宽度就是替换的成本**。所以按本包
一贯做法单开 `Notifier`，调用方用**类型断言**探测，不实现也照跑。

**为什么通知类型是接口而不是「结构体 + Kind」：** 加一种通知 = 加一个实现
（如 `UsageNotice`），`Notify` 的方法集不动、已有实现不用改；结构体 + Kind 每加一种
都要动那个结构体。`Notice` 是**密封接口**（未导出标记），`Notify` 的实现方可以放心
断言、未知类型静默忽略。`agent` 与 `channels` 各持一份同形状的定义，由消息层的
`channelEcho` 搬运——与 `ToolEvent` 之于 `channels.Emitter.Tool` 同一条规矩。

**回退：** 换端点若建流 400 且错误文本含 `stream_options`，删 `baseRequest` 里那
一行即回退（用量随之回到零值，展示层自动不打那一行）。

**网页已接上：** `web` 渠道的逐条消息回显对象（`sseEmitter`）实现 `channels.Notifier`，
把用量推一帧 `event: usage`（`promptTokens/completionTokens/totalTokens`），前端
`app.js` 渲染成一行 `用量：入 X / 出 Y（共 Z）`、样式 `.note.usage` 更淡。
**通知长在回显对象上而不是渠道上**——用量属于某条会话，而渠道实例是多会话共享的，
没有「当前会话」可指；`sseEmitter` 是逐条消息造的，知道发给哪个房间（接缝适配器
`channelEcho` 先问渠道、再问回显对象）。

**不做（留作后续）：** 微信回执——机制已就位（iLink 的回显对象实现
`channels.Notifier` 即可）；不引 tokenizer；不加开关。

---

## 23. 执行中状态与中途停止（网页「停止」按钮）

**决定：** 两条独立的东西，缺一个「停止」都做不成。

**① 执行中状态是通知（下行）**：`channels.TurnStateNotice{Running bool}`，
由**消息层**在 `Handle` 进/出时各发一次，经 `channels.Notifier` 到达渠道
（网页推 `event: turn`，前端据此把「发送」键切成「停止」键、结束时切回）。
为什么不放工具循环：空问题/缺身份这类**不走循环的拒答**也是「在处理一条消息」，
前端只认这一对通知就不必自己猜状态。前端**不做乐观切换**——只在收到通知时才切。

**② 中途停止是控制（上行）**：`channels.TurnController.CancelTurn(sessionKey)`
由**业务层实现**（消息层才对：它调度 worker、持有这一轮的 ctx），经
`channels.TurnControllable` 在**装配根启动前**注入给实现了它的渠道
（`app.Serve` 里 `SetTurnController`）。网页 `POST /stop` → 渠道算出会话键
（`渠道:账号:发起人:客户端会话`，与消息层同一条规则）→ 取消那一轮的 ctx →
流被掐断。

- **取消不是错误**：`Ask` 见 `ctx.Err() != nil` 就回一句平静的 `（已停止）`，
  并把 `Cause` 带上（CLI 靠它区分「已中断」与真失败）。
- **没有在跑就如实说没有**：`CancelTurn` 返回 false，`/stop` 回 `stopped:false`，
  不假装成功。
- **两处都是可选能力**：渠道不实现 `TurnControllable` 就没有「停止」这个功能；
  不实现 `Notifier` 就收不到状态。与 `AddressResolver`/`MediaFetcher` 同一条规矩。

**已接受的代价：** 取消掐的是「这一轮的 ctx」——模型请求会中断、可能的半句答案
丢掉；工具那一步如果是**不可逆的外部动作**（发出去的消息、写下的文件）已经发生
就追不回，取消只保证**不再继续**。

---

## 24. 入站媒体：接缝统一，转换点唯一（图片 / 文本 / 二进制）

**决定：** 网页与微信的「读文件」走**同一条**业务链路，差异只留在渠道的
`channels.MediaFetcher` 实现里。分类语义**与沙盒 `read`、CLI `@引用` 完全一致**（三类）：

- **图片** → `llm.ImageAttachment`（base64 data URI，模型当图看）；
- **文本**（无 NUL 字节）→ 正文并进本轮问题（32 KiB 上限、超出截断），**进会话历史**；
- **二进制**（音频/视频/压缩包…）→ 只给「名字 + 类型 + 大小」的元信息；
- **语音**带转写文本时，转写当文本用（音频字节仍收进 `files`）。

- **入站**：渠道产出 `channels.Part{Kind, Media: MediaRef}`——只带句柄、不带字节
  （微信是 CDN 参数，网页是内存暂存的 id）。
- **唯一转换点**在 `internal/messages/media.go`：对每个媒体 part 调 `MediaFetcher.FetchMedia`
  取回字节、按嗅探分类，转成附件 / 问题正文 / 元信息。**agent 与 llm 里不出现任何渠道
  分支**——加第三个渠道，这个文件一行不改。这正是「接缝是唯一统一处」那条判据的落点。
- **`FKA_MEDIA_SINK`**（第 25 条）：所有取回的原始字节另走 `tools.Context.Inbound`，只有
  模型调用**被点名的那个工具**（如 `mcp__sandbox__import`）时才交付给沙盒。
- **纯媒体补一句默认提示语**（`defaultMediaQuestion`）：各家模型对「空文本 + 内容块」
  接受度不一。取不回来且没有文字时回 `replyMediaUnavailable`，**不调模型空猜**。
- 网页侧 `POST /messages` 支持 multipart，**接受任意文件**；字节进独立的 `inbound` 暂存
  （与出站 `/files/{id}` 的 `blobs` 分开），`Capabilities.{File,Image}.Receive` 均为 true。
  微信侧无需改动——它早已解析各类 part 并实现了 `FetchMedia`。`contract_test.go` 里
  `FetchMedia` 的待办债同时销掉。

**已接受的代价：** 单件上限 8 MiB、一条最多 8 件（`media.go`），超限跳过并记日志；图片
base64 后约膨胀 1/3，计入本轮请求大小但不进历史预算。

---

## 25. 入站图片落到沙盒：agent → sandbox 的字节端到端

**决定：** 用户发来的图片，除了喂给模型，还能落到**一个被显式点名的**沙盒里，供工具
（`read` / `run`）二次处理。落点由 `FKA_MEDIA_SINK` 指定，值是一个**工具全名**
（如 `mcp__sandbox__import`）。

- **反向复用「字节端到端」**：与第 18 条（沙盒 `export` 把字节经 MCP 内容块送给用户）
  对称。沙盒新增 `import` 工具，把 agent 注进来的字节写进本租户沙盒的 `inbox/` 下。
- **字节走工具参数、由 agent 注入**：模型手里只有图片、没有 base64 文本，造不出
  `files`。所以循环（`internal/agent` 的 `injectInbound`）在模型调用**恰好那个落点
  工具**时，把本轮入站附件的原始字节填进 `files`（`tools.Context.Inbound` 逐轮携带）。
- **为什么点名而不是按工具名猜**：MCP server 是通用的，agent 不认识「沙盒」。按后缀
  `__import` 注入的话，有几个同名工具就往几个 server 塞用户图片——**扇出**，且「有几个」
  取决于运行时谁连上了，路由会静默漂移。点名的失效是可诊断的。
- **没配或点名的没连上**：不导入，记一条 Warn，图片照常喂模型（vision 不受影响）。
  **不改路由去猜一个沙盒**。
- 三个条件同时成立才落盘：被点名、连上了、确实列了 `import`。

**已接受的代价：** `import` 是 MCP 工具（effect `external`），要 `LLM_TOOL_EFFECTS` 放行
`external` 模型才看得到——与其它沙盒工具同一条闸。`inbox/` 只有单文件 8 MiB 上限，
文件名取 basename、重名自动让位。

---

## 26. 入站媒体去向：`FKA_MEDIA_MODE=llm|both|sandbox`

**决定：** 一个枚举决定用户发来的媒体去哪，**默认 `llm` = 现状，行为零变化**。

- `llm`（默认）：进模型——图片 vision 附件、文本并进问题、二进制只给元信息；沙盒**按需**
  （模型当轮调 `FKA_MEDIA_SINK` 那个工具时才落，见 #25）。
- `both`：既进模型，又在**开轮时自动**落一次沙盒；沙盒返回的路径拼进本轮问题，于是
  **这一轮以及后续轮**都能引用同一份文件（解决了「文件先到、指令后到」跨轮拿不到媒体
  的缺口——图片是 `json:"-"`、本就不进历史）。
- `sandbox`：**不进模型**，自动落沙盒，把路径交给模型（适合非视觉模型 / 只想让工具处理）。
  **纯文件、无指令文字时「只存不答」——完全不触发 LLM**：发一条 `channels.MediaStoredNotice`
  （**一行**，带沙盒路径与文件名）给前端，当系统提示渲染（像用量，不是聊天气泡）；收不到通知的
  渠道（iLink）由消息层退回文本。沙盒说明挂到会话上；用户下一条（带指令的）消息才跑模型，
  并把挂起的沙盒路径并进问题。于是「先传文件、再下指令」两轮都成立。

**落沙盒在哪做：** `both/sandbox` 的自动落沙盒在**消息层**完成（装配根注入 `InboundImporter`，
它用 `app.Tools.Call` 调被点名的落点工具）——这样 `sandbox` 的纯文件可以**不跑模型**即落盘；
agent 循环只保留「模型自己调落点工具时注入字节」那条按需路径。

**三条前提（沙盒值共有，都按 fail-closed 处理）：**

1. **必须配 `FKA_MEDIA_SINK`**：没配就没有落点，`both`/`sandbox` 退成只进模型并记 Warn。
2. **`external` 仍受闸**：自动落沙盒走的是**注册表调用**（`tools.Service.Call`），所以
   `LLM_TOOL_EFFECTS` 没放行 `external` 时**落不了**——不是绕过闸门，而是被闸门挡住。
3. **`inbox/` 有 TTL**：`mcp_sandbox` 的 `import` 每次会清掉超过 `InboxTTL`（默认 24h）的
   旧文件，避免每收一次用户文件就永久堆一份。

**为什么不是两个开关**（`import` × `feed`）：两个正交布尔有无效组合（不喂模型又不落沙盒
= 媒体丢失），一个枚举把有效态说清。`NormalizeMediaMode` 对认不出的取值记 Warn 并退回 `llm`。

---

## 附：零 CGO 是怎么达成的

盘问时逐个检查过每个模块对 CGO 的需求，最后只剩 SQLite 一个候选，选了纯 Go：

| 模块 | 方案 | CGO | 在本仓库？ |
|---|---|---|---|
| 嵌入 | SCNet HTTP | 无 | 文档侧，已移出 |
| 向量 | 手写薄层 | 无 | 文档侧，已移出 |
| 文档转换 | `docker run` exec | 无 | 文档侧，已移出 |
| ExifTool | 系统命令 exec | 无 | 文档侧，已移出 |
| MCP client | mark3labs（纯 Go） | 无 | ✅ `internal/tools/mcp` |
| SQLite | modernc.org/sqlite | 无 | ✅ `mcp/memory/internal/store` |
| 二维码 | `skip2/go-qrcode` | 无 | ✅ `cmd/fka/login.go`（`fka login` 画码） |
