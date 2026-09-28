# mcp/ —— 记忆 MCP server

**这里是能力，不是 agent。** agent 在 [`internal/`](../internal) 里，它**不认识任何数据的存储**——
它的本事全靠 MCP server 与 skill 顶上，本目录就是其中之一。

`bin/fka-memory` 是一个**独立进程**（stdio），由主程序从 `mcp.json` 当子进程拉起。崩了不影响主程序。

## 自己管理自己

这个 server **自给自足**：

| 归它管 | 归别人管 |
|---|---|
| 建哪张表（`memories` 一张） | agent 的装配、配置、生命周期 |
| schema 是什么、迁到第几版 | 别的 server 的表与版本 |
| 认领什么样的旧库 | 日志由谁在看 |
| 默认库文件是**自己的** `data/memory.sqlite` | — |

它**不 import 本仓库树外的任何包**——没有共享的 `store`、没有共享的 `mcpboot`、没有共享的
`domain`。启动那几行现在就在 `main.go` 里，因为**只有一个 server 时，「共用的启动逻辑」
这个概念就不成立了**，硬要留一个包反而是给自己加一层间接。

这条由 `boundary_test.go` 扫 import 守着。Go 的 `internal` 规则只保证「`mcp/memory/internal/...`
只有 memory 树能 import」；**反过来** `mcp/memory` import `fka-go/internal/config` 编译器不报错，
那是测试的事。

## 挂上去

```jsonc
// <安装根>/mcp.json
{
  "mcpServers": {
    "memory": {
      "command": "/path/to/bin/fka-memory",
      "args": ["--db", "/path/to/data/memory.sqlite"]
    }
  }
}
```

然后主程序要放行 `external`（**MCP 工具一律是 external**）：

```
LLM_TOOL_EFFECTS=read,external
```

`--db` 不给时默认 `<安装根>/data/memory.sqlite`。指到别的文件（含旧的共享 `db.sqlite`）
**照样能用**——认领路径只核对 `memories` 一张表，别的表一概忽略。

## 构建

与主程序同一个 module，从仓库根构建：

```bash
CGO_ENABLED=0 go build -o bin/fka-memory ./mcp/memory   # 零 CGO
```

依赖只有 `mark3labs/mcp-go`（钉在 v0.40.0）与 `modernc.org/sqlite`（纯 Go）。**没有 CGO。**

## 工具

| 工具 | 作用 |
|---|---|
| `search_memories` | 查家庭记忆。四类：事件 / 待办 / 经验 / 常识。关键词之间是「且」 |
| `remember_memory` | 记一条。只增，不改不删 |

## 三条硬约束

### stdout 一个字都不能有

stdout 是 JSON-RPC 的通道。**任何** `fmt.Println` 都会插进协议流里把 server 打挂。诊断信息一律走
`internal/log`（stderr + 按天轮转的文件）。这条在 `main.go` 的文件头就写着。

### `viewer_wxid` 是模型填的

**server 按约定不做进程级绑定**——工具参数里的 viewer 直接进 SQL 的 `WHERE`：

```sql
(visibility = ? OR owner_wxid = ?)
```

**这一句是本目录唯一的数据防线。** 它必须留在 `WHERE` 里、每次查询都要带——查完再筛的话，
漏一处就泄漏。而主程序会把**不可信内容**喂进 LLM 上下文，所以**提示注入是结构性暴露**，
不是假想风险。收紧的做法（进程级身份 / 签名参数）见 [`docs/permissions.md`](../docs/permissions.md)。

### 认领失败绝不重建

Node 版建的老库 `user_version=0`，走**认领**路径：逐列核对 `memories`，对得上就打 v1，
对不上就**明确报错**——**绝不顺手重建**。重建会「修好」错误，代价是数据没了，而用户看到的是一个
安静的库，不是一场事故。

⚠️ **认领会给整份库文件打 `user_version=1`**，而 `user_version` 是库级、不分表的。所以**共用一个
库文件时，谁先认领谁替整份库记账**。这条已由 `TestMigrate_认领会给整份库打版本号` 钉住——
它是共用库这个前提下的**已知后果**，不是 bug。想要互不干扰，就给每个 server 一个自己的库文件。

## 存储

```
data/memory.sqlite    memories 一张表
```

**用 `PRAGMA user_version` 做版本化迁移**，v1 与 Node 版的 DDL 逐字一致，所以现成的库能被
**认领**而不是重建——见 `internal/store/migrate.go`。

## 目录

```
mcp/memory/
├── boundary_test.go  边界测试：不许依赖树外的包；server 必须是 main 包
├── main.go           启动（含库路径解析与迁移）
├── server.go         两个工具的声明与实现
├── id.go             UUID 形状的 id
└── internal/         **只有本树能 import**（编译器保证）
    ├── store/        SQLite：DDL / user_version 迁移 / memories 读写
    ├── domain/       记忆类型与可见性
    ├── searchterms/  拆词与 LIKE 通配符转义
    └── log/          stdio 子进程用的 logger
```
