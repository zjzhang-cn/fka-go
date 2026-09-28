# 开发日志（Go 版）

> 记录 Go 版的功能实现、架构调整与 BUG 修复。**最新记录在上。**
>
> **规范：** 写日志先于提交。一个完整功能一次提交，禁止把不相关改动混在一起。
>
> 本项目是**独立仓库**。Node 版（`fka` 仓库）有自己的开发日志，两边历史互不相干。

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

