# fka-go —— 家庭知识管家（Go 版）

家庭知识管家：家人把文件发给 Bot，Bot 解析归到 NAS、建立索引，之后用一句话就能把资料找回来并拿到带出处的答案。

**这是一个独立项目，独立仓库。** 本目录的代码与文档不依赖任何外部目录。

Node 版（在 `fka` 仓库里）仅作**历史参照与迁移来源**：本项目从它逐层平移而来，
[docs/node-to-go.md](docs/node-to-go.md) 记着两边的映射、有意不同的 5 处、
以及必须 1:1 的 8 处。那边是另一个仓库、另一份历史，改动互不相干。

**当前状态：Agent 能力 + 存储层 + 记忆 MCP server + NAS 布局已端到端可用。**

---

## 快速开始

```bash
cd go
go build ./...
go test ./...
CGO_ENABLED=0 go build -o bin/fka        ./cmd/fka      # 主程序
CGO_ENABLED=0 go build -o bin/fka-memory ./mcp/memory   # 记忆 MCP server
CGO_ENABLED=0 go build -o bin/fka-docs   ./mcp/docs     # 文档 MCP server
```

```bash
# 列出模型现在能看到的工具与五类放行情况
FKA_HOME=/path/to/fka LLM_TOOL_EFFECTS=read,external ./bin/fka tools

# 无头跑一轮工具循环问答（不经过微信）
FKA_HOME=/path/to/fka LLM_TOOL_EFFECTS=read,external,memory \
  FKA_VIEWER=wx_zhang ./bin/fka ask "请用 remember_memory 记一条：2026年3月全家去了三亚"
```

必填环境变量只有 `LLM_API_KEY` 与 `LLM_MODEL`。没配时 `ask` 与问答都会**明确报错**，
不会静默降级成空答案。

### 挂上记忆 server

```jsonc
// mcp.json
{
  "mcpServers": {
    "memory": {
      "command": "/path/to/fka-go/bin/fka-memory",
      "args": ["--db", "/path/to/fka/data/db.sqlite"]
    },
    "docs": {
      "command": "/path/to/fka-go/bin/fka-docs",
      "args": ["--db", "/path/to/fka/data/db.sqlite", "--storage", "/path/to/fka/data/nas"]
    }
  }
}
```

`FKA_HOME` 指向安装根（`data/`、`logs/`、`.env`、`mcp.json` 都按它解析）。
不设时取**可执行文件所在目录**——静态二进制的天然锚点，跨机器拷贝后仍然自洽。

---

## 文档

| 文件 | 讲什么 |
|---|---|
| **[docs/port-plan.md](docs/port-plan.md)** | **先读这个。** 模块清单与进度、实施顺序、每层的验收标准、**改任何东西都不能破的不变量** |
| **[docs/decisions.md](docs/decisions.md)** | 12 条技术决策与「为什么是它而不是别的」，含每条的**后果**（很多是已接受的代价） |
| **[docs/permissions.md](docs/permissions.md)** | ⚠️ 权限模型，以及**唯一一处「防线从代码移到模型手上」的地方** |
| **[docs/node-to-go.md](docs/node-to-go.md)** | Node → Go 的模块映射、**有意不同的 5 处**、**必须 1:1 不许优化的 8 处** |
| [docs/dev-log.md](docs/dev-log.md) | 开发日志（最新在上） |

---

## ⚠️ 一句话必须知道的

`viewer_wxid` 搬进 MCP 之后**变成模型填的工具参数**，server 按决定不做进程级绑定。
而这份产品按设计就要把家人上传的**不可信文档**喂进 LLM 上下文——所以**提示注入是
结构性暴露**，一次注入或模型判断失误就可能泄露别人的 private 文档，且**不报错**。

**怎么补**（改动很小）见 [docs/permissions.md](docs/permissions.md#没做的缓解以及怎么补)。

---

## 架构

```
fka ask "…"
   │
   ▼
┌──────────────────────────────────────────────────────┐
│ cmd/fka  主程序                                        │
│  ├─ internal/agent     工具循环（≤N 步）              │
│  ├─ internal/llm       模型契约 + 历史压缩 + 会话历史   │
│  │   └─ openai/        OpenAI 兼容 provider（流式+双超时）│
│  ├─ internal/prompts   系统提示词唯一出处               │
│  ├─ internal/tools     工具接缝 + 五类 effect 放行      │
│  │   └─ mcp/           MCP client（mark3labs）         │
│  └─ internal/app       装配根（唯一装配点）             │
└──────────────────────┬───────────────────────────────┘
                       │ mcp.json：stdio 子进程
        ┌──────────────┴──────────────┐
        ▼                             ▼
┌────────────────────┐   ┌──────────────────────┐
│ mcp/memory         │   │ mcp/docs             │
│ 家庭记忆 server     │   │ 文档管理 server（只读）│
│ bin/fka-memory     │   │ bin/fka-docs         │
└─────────┬──────────┘   └──────────┬───────────┘
          │                          │
          ▼                          ▼
   ┌──────────────┐          ┌──────────────┐
   │ internal/store│         │ internal/nas  │
   │ SQLite +     │         │ files/        │
   │ user_version │         │ extracted/    │
   │ 迁移          │         └──────────────┘
   └──────────────┘
```

**装配点唯一**在 `internal/app/app.go`——构造函数链，没有 Cordis 也没有 `cordis.yml`
（理由见 [decisions.md](docs/decisions.md) 第 4 条）。

**每个 MCP server 一个独立目录 + 独立可执行程序**：能单独构建、部署、重启，崩了不影响
主程序。共享的启动逻辑刻意只有三四个函数（`internal/mcpboot`）——**共享的部分一旦开始
长，就会把「独立」这件事吃掉**。

---

## 目录

```
go/
├── cmd/fka/            主程序：ask（无头问答）/ tools（工具清单）
├── mcp/                每个 MCP server 一个独立目录 + 独立可执行程序
│   └── memory/         家庭记忆 server
├── docs/               本项目文档（见上表）
└── internal/
    ├── agent/          工具调用循环 + runner
    ├── app/            装配根
    ├── config/         安装根解析、.env 加载、日志（按天轮转）
    ├── domain/         跨模块枚举
    ├── ids/            外部标识校验（同一套字符集，两处共用）
    ├── llm/            模型契约 + 历史压缩 + 会话历史
    │   └── openai/     OpenAI 兼容 provider
    ├── mcpboot/        各 MCP server 共用的启动逻辑（刻意只有三四个函数）
    ├── nas/            原件与解析结果的落盘
    ├── prompts/        系统提示词唯一出处
    ├── searchterms/    拆词与 LIKE 通配符转义
    ├── store/          SQLite：DDL / user_version 迁移 / 三张表 / 批注编解码
    └── tools/          工具契约 + 放行策略 + 注册表
        └── mcp/        MCP client + mcp.json + 聚合源
```

**依赖只有三个：** `sashabaranov/go-openai`、`mark3labs/mcp-go`、`modernc.org/sqlite`
（纯 Go，零 CGO）。**没有 CGO**——每个模块对 CGO 的需求都逐个检查过，见
[decisions.md](docs/decisions.md#附零-cgo-是怎么达成的)。
