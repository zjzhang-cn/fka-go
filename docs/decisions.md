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
**决定：** 兜底从常量 `"cli"` 改成 `cli-<uuid v4>`（`cmd/fka/sessionid.go`）。
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
2. **发送根默认是 `<安装根>/sandbox`**（与 `fka-bash` 的沙盒根同目录，`BASH_SANDBOX_ROOT`
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
`if tool == "bash__export"`，这条解耦当场作废——所以它没有。

**新增 bash `export` 工具**（`read` 不动，仍是"给模型看"，不自动外发）。模型要「把文件
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

**认证：token → HttpOnly cookie。** `WEB_CHANNEL_TOKEN` 必填，没配就不启用。
`POST /login` 校验后种一个 HttpOnly cookie：`EventSource` 与 `<img>` 都带不了自定义
头，只有 cookie 能让 SSE、媒体、POST 三处统一认证，且令牌不进 URL、不进日志。
`/messages` 另收 `Authorization: Bearer` 给非浏览器客户端。

**流式：实现 `EmitterProvider`。** `channels.Emitter` 接口早就存在却无人实现，
SSE 是它最自然的落点：推理增量与工具调用边发生边推。**最终答案不走 Emitter**——
它经 `Senders.Text` 送出，`Emitter.Answer` 必须置空，否则同一份文本会推两次。

**已接受的取舍：**
- **单账号**（`AccountID = "web"`）。接缝按账号分片，所以**所有 web 会话串行执行**。
  要多用户并发得改成「一个用户一个账号实例」，那一步只动 provider。
- **File/Image 只发不收**（`receive=false`）。业务层没有入站媒体的消费者
  （收到媒体会如实拒答，见 `internal/messages`），声明能收却没有那条路就是
  「声明一个跑不了的能力」。等有落点再补 upload。
- **出站媒体走内存暂存 + `GET /files/{id}`**（10 分钟 TTL、64 MiB 上限），SSE 事件
  只给相对 URL，不内联 base64。
- **前端不打包**：`web/` 是仓库里的源码，运行期从 `<安装根>/web`（`WEB_CHANNEL_STATIC`
  可改）读，**不用 `go:embed`、不上构建链**。

**开关：** `WEB_CHANNEL_ADDR` 设了才启用（产出 1 个实例）；没设则 0 实例、行为与
以前一致。`WEB_CHANNEL_PRINCIPAL` 定 `PrincipalID`（默认 `web:default`）。

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
