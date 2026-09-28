# Node → Go 映射与行为差异

> 「平移」的核对表。**目标是行为一致，不是代码相似。**
>
> 凡是下面标了 ⚠️ 的，都是**有意不同**——每条都有理由，不是疏忽。
>
> 左列是 Node 版（**另一个仓库** `fka` 的 `src/`）的模块名，右列是本仓库的。

---

## 模块映射

| Node（`fka` 仓库的 `src/`） | 本仓库 | 状态 |
|---|---|---|
| `utils/domain.ts` | `internal/domain` | ✅ |
| `utils/ids.ts` | `internal/ids` | ✅ |
| `utils/search-terms.ts` | `internal/searchterms` | ✅ |
| `utils/paths.ts` + `utils/logger.ts` | `internal/config` | ✅ |
| `storage/index.ts` | `internal/nas` | ✅ |
| `db/schema.ts` + `db/schema-build.ts` | `internal/store/schema.go` + `migrate.go` | ✅ **有意不同**（见下） |
| `db/types.ts` + `db/annotations.ts` + `db/inspect.ts` + `db/sqlite/` | `internal/store` | 部分（查询侧随文档 MCP server 一起做） |
| `llm/types.ts` + `llm/index.ts` + `llm/session-history.ts` | `internal/llm` | ✅ |
| `llm/openai/index.ts` | `internal/llm/openai` | ✅ **实现方式不同** |
| `llm/agent/{loop,plugin}.ts` | `internal/agent` | ✅ |
| `prompts/index.ts` | `internal/prompts` | ✅ |
| `tools/{types,policy,registry,plugin}.ts` | `internal/tools` | ✅ |
| `tools/mcp/{client,config,index}.ts` | `internal/tools/mcp` | ✅ **换 SDK** |
| `tools/memory/index.ts` | `mcp/memory` | ✅ **搬成独立可执行程序** |
| `tools/builtin/` | `mcp/docs` | ⬜ |
| `tools/skills/` | ⬜ |
| `memory/` | `mcp/memory` | 部分 |
| `vector/` + `embedding/` | ⬜ | **换实现**（手写薄层 + 只有云端嵌入） |
| `parsers/` | ⬜ | 保留接缝 |
| `messages/` | ⬜ | |
| `channels/` | ⬜ | |
| `ipc/` + `cli/` | ⬜ | |
| `cordis.yml` + `boot.ts` + `index.ts` | `internal/app` | ✅ **换成构造函数链** |

---

## 有意的行为差异

### ⚠️ 1. 数据库：零迁移 → 版本化迁移

**Node：** 无迁移。schema 直建 + `__schema` 结构指纹，指纹不匹配就**整库重建**
（旧库改名 `.bak`）。

**Go：** v1 起用 `PRAGMA user_version` 记账，一条迁移一个事务。

**为什么：** Node 那个策略对已跑起来的库意味着**每加一个字段就重建一次**。
`documents.annotations` 就是这么加上去的。

**用户可见的变化：** 加字段不再丢数据。`db stats` 里那句「重建（旧库保留为 .bak）」
消失了，变成「有 N 条待应用的迁移」。

**`__schema` 表不再使用。** 指纹是「结构是否一致」的代理指标，version 是事实本身——
它不会因为 DDL 文本改写而误报。旧库里的 `__schema` 留着无害。

### ⚠️ 2. 架构：Cordis 插件树 → 构造函数链

**Node：** `cordis.yml` 是装配的唯一真相，25 个插件，`ctx` 动态注入，`effect` 逆序卸载。

**Go：** `internal/app/app.go` 是唯一装配点，构造函数链，每个插件用 `defer` / `Close` 收尾。

**为什么：** `loader.await()` 不等插件的异步 setup，逼着入口与测试显式等 services
（忘了就启动到一半才炸）；`ctx.db` / `ctx.tools` 是运行时属性，Go 的编译期类型严格更强；
逆序卸载在 Go 里是 `defer`。

**用户可见的变化：** 「加一个 provider 要改一行 Go 而不是一行 YAML」。改完就编译过。

### ⚠️ 3. 记忆与文档管理：进程内方法 → MCP server

**Node：** `tools/memory/` 与 `tools/builtin/` 是进程内的 `ToolSource`，直接调
`Database` 端口与 `MemoryBackend`。

**Go：** `go/mcp/memory/`、`go/mcp/docs/` 是**独立的可执行程序**（`package main`，
各自出二进制），由主程序从 `mcp.json` 当子进程拉起。

**为什么：** 决定——内部记忆与文档管理都用 MCP 实现。主程序因此**不再直接碰
`memories` / `documents` 表**。

**⚠️ 用户可见的变化（重要）：** `viewer_wxid` 从「代码构造的参数」变成
「模型填的工具参数」，server 不做进程级绑定。**这是已接受的风险**，
详见 [permissions.md](permissions.md)。

**另一处变化：** MCP 工具一律 `external` effect，所以**要用记忆/文档工具必须
`LLM_TOOL_EFFECTS=read,external`**（记忆写入还要加 `memory`——虽然 MCP 源把
`remember_memory` 也声明成 `external`）。

### ⚠️ 4. 嵌入：本地 ONNX → 只有云端

**Node：** 默认本地 bge-small-zh-v1.5（transformers.js + ONNX，512 维，23MB，不联网）。

**Go：** **只有 SCNet 云端**（Qwen3-Embedding-8B，4096 维）。

**为什么：** Go 没有 transformer.js 的等价物。真要本地化只有 CGO 绑 ONNX Runtime
（tokenizer 要自己实现，生态不成熟）或 Python sidecar。

**⚠️ 用户可见的变化：** 文档正文会发到云端；`SCNET_API_KEY` 成硬依赖；向量索引全部
重建；切片参数要重定；**语义阈值必须重标**。详见 [decisions.md](decisions.md) 第 2 条。

### ⚠️ 5. 向量库：LanceDB → 手写薄层

**Node：** LanceDB，按 `storageId` 分账号目录，`where()` 权限预过滤 + 余弦检索。

**Go：** 手写薄层，**零第三方依赖**。

**为什么：** Go 没有生产级 LanceDB SDK；而向量库是**纯派生数据**（真值在
`extracted/*.md`，删了 `reindex` 就回来），所以它不需要事务与备份，选型标准只剩
「够快 + 够简单」。当前 5 份文档的规模下暴力检索是毫秒级。

**用户可见的变化：** `vector check` 的自查询分数会变（不再恒定 ~0.994）；
向量目录结构不再是 LanceDB 的 `*.lance`；`reindex` 变成删目录重建。

---

## 实现方式不同、但行为应一致的

| 东西 | Node | Go | 说明 |
|---|---|---|---|
| LLM client | 手写 fetch + 手解 SSE | `sashabaranov/go-openai` | **双超时仍要自己实现**（SDK 的整体超时会吃掉断流那条） |
| MCP client | `@modelcontextprotocol/sdk` | `mark3labs/mcp-go` | |
| 推理内容 | 手工解 `reasoning_content` | SDK 的 `delta.ReasoningContent` | 仍**只进 stdout，不进答案** |
| `enable_thinking` | 直接写进请求体 | `bodyInjector` 注入 | SDK 结构体里没这个字段 |
| SQLite | `node:sqlite` + drizzle proxy | `modernc.org/sqlite` + 手写 SQL | **零 CGO** |
| ID 生成 | `randomUUID()` | `crypto/rand` 拼 UUID 形状 | 都用密码学随机源：id 撞了会**静默丢数据** |
| 配置 | `dotenv` | 自己写的 60 行 | 不引依赖 |

---

## 必须 1:1 的部分（不许「顺手优化」）

这些的**行为**就是产品的一部分，改了就是改了产品：

| | 为什么不能动 |
|---|---|
| **NAS 布局** `files/{wxid}/{id前8}_{名}` + `extracted/…{名}.{ext}.md` | 两版要能读同一份 `data/nas` |
| **v1 的 DDL** | 现有库要能被直接认领（330 条消息） |
| **数据库列名**（`author_wxid` / `created_at` …） | 改列名 = 全量重导 |
| **工具全名** `<源>__<工具>` | 模型在会话历史里逐字重放；改名会让那一轮的前缀缓存全失效 |
| **MCP 工具名** `mcp__<server>__<tool>` | 同上 |
| **会话历史的消息形状** | 同上：`tool_calls` 的 `arguments` **必须保持原始 JSON 字符串**，
反序列化再序列化会改变字节序 |
| **`/记忆` 的四类** `event` / `reminder` / `experience` / `knowledge` | 数据库里已有数据 |
| **批注的 JSON 字段名** `text` / `authorWxid` / `createdAt`（camelCase） | 同一列，Node 写下的数据在用 |

---

## 还没搬的模块

`parsers/`、`vector/`、`embedding/`、`messages/`、`channels/`、`ipc/`、`cli/`
以及 `tools/{builtin,skills}/`。清单与顺序见 [port-plan.md](port-plan.md)。
