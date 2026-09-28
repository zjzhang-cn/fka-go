# mcp/ —— MCP server 集合

**这里是能力，不是 agent。** agent 在 [`internal/`](../internal) 里，它**不认识任何数据的存储**——
它的本事全靠 MCP server 与 skill 顶上，本目录就是其中之一。

两个 server 是**独立进程**：由主程序从 `mcp.json` 当子进程拉起（stdio）。崩了不影响主程序。

## 有什么

| 可执行程序 | 工具 | 状态 |
|---|---|---|
| `bin/fka-memory` | `search_memories` · `remember_memory` | ✅ 家庭记忆（四类：事件 / 待办 / 经验 / 常识） |
| `bin/fka-docs` | `search_documents` · `list_documents` · `get_document` | ✅ 文档管理（只读） |

## 挂上去

```jsonc
// <安装根>/mcp.json
{
  "mcpServers": {
    "memory": {
      "command": "/path/to/bin/fka-memory",
      "args": ["--db", "/path/to/data/db.sqlite"]
    },
    "docs": {
      "command": "/path/to/bin/fka-docs",
      "args": ["--db", "/path/to/data/db.sqlite", "--storage", "/path/to/data/nas"]
    }
  }
}
```

然后主程序要放行 `external`（**MCP 工具一律是 external**）：

```
LLM_TOOL_EFFECTS=read,external
```

## 构建

与主程序同一个 module，从仓库根构建：

```bash
CGO_ENABLED=0 go build -o bin/fka-memory ./mcp/memory   # 零 CGO
CGO_ENABLED=0 go build -o bin/fka-docs   ./mcp/docs
```

依赖只有 `mark3labs/mcp-go`（钉在 v0.40.0）与 `modernc.org/sqlite`（纯 Go）。**没有 CGO。**

## 边界：这里不碰 agent 的 internal

`mcp/**` **不许 import `fka-go/internal/...`**，由 `boundary_test.go` 守着。

Go 的 `internal` 规则是单向的：`mcp/internal/...` 只有 `mcp/` 树能 import，编译器已经管住了；
但反过来 `mcp/docs` **完全可以** import `fka-go/internal/config` 而不报错（它同样在 `fka-go/` 之下）。
那条正是要防的——两个 server 要的是「自己拥有数据的独立进程」，不是「伸手进 agent 内部拿点东西」。
一旦伸手，它们就跟 agent 的装配、配置、生命周期绑在一起，不再是能被单独构建、发布、换掉的能力。

## 三条硬约束

### stdout 一个字都不能有

stdout 是 JSON-RPC 的通道。**任何** `fmt.Println` 都会插进协议流里把 server 打挂。诊断信息一律走
`internal/log`（stderr + 按天轮转的文件）。这条在每个 `main.go` 的文件头都写着。

### `viewer_wxid` 是模型填的

**server 按约定不做进程级绑定**——工具参数里的 viewer 直接进 SQL 的 `WHERE`：

```sql
(visibility = ? OR owner_wxid = ?)
```

**这一句是本目录唯一的数据防线。** 它必须留在 `WHERE` 里、每次查询都要带——查完再筛的话，
漏一处就泄漏。

而主程序按设计会把家人上传的**不可信文档**喂进 LLM 上下文，所以**提示注入是结构性暴露**，
不是假想风险。收紧的做法（进程级身份 / 签名参数 / 按 viewer 动态注册工具）见
[`docs/permissions.md`](../docs/permissions.md)。

`docs/server_test.go` 里有 6 个用例专门钉这件事：属主搜得到自己的、别人搜不到、陌生人 0 条、
按 id 取私有文档被拒、缺 viewer 一律拒绝。

### 共享逻辑不许长

共享的启动逻辑刻意只有三四个函数（`internal/mcpboot`）——**共享的部分一旦开始长，
就会把「独立」这件事吃掉**。

## 存储

```
data/db.sqlite      documents / memories / messages 三张表
data/nas/files/     原件（按上传者分目录）
data/nas/extracted/ 解析结果（Markdown + YAML front matter）
```

**用 `PRAGMA user_version` 做版本化迁移**，v1 与 Node 版的 DDL 逐字一致，所以现成的库能被
**认领**而不是重建——见 `internal/store/migrate.go`。

## 目录

```
mcp/
├── boundary_test.go  边界测试：这里不许依赖 agent 的 internal
├── docs/             文档管理 server（package main）
├── memory/           家庭记忆 server（package main）
└── internal/         **只有 mcp/ 树能 import**（编译器保证）
    ├── store/        SQLite：DDL / user_version 迁移 / 三张表 / 批注编解码
    ├── nas/          原件与解析结果的落盘
    ├── domain/       跨模块枚举
    ├── searchterms/  拆词与 LIKE 通配符转义
    ├── ids/          外部标识校验
    ├── mcpboot/      两个 server 共用的启动逻辑（刻意只有三四个函数）
    └── log/          stdio 子进程用的 logger
```

**每个 server 一个目录、一个可执行程序**：能单独构建、部署、重启。
