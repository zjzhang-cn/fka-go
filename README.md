# fka-go —— 通用 agent，带一个渠道接缝

一个**工具调用 agent**：模型自己决定调哪些工具、循环到得出答案为止。

**这个项目不带任何内置能力。** 它的本事全靠两条路进来——**MCP server** 与 **skill**。
它自己不拥有任何数据：没有数据库、没有 NAS、没有「家庭资料」这回事。
要什么能力就接一个 server 或写一份 skill。

> **这是独立仓库。** Node 版（在上级 `fka` 仓库里）仅作**历史参照与迁移来源**，
> [docs/node-to-go.md](docs/node-to-go.md) 记着两边的映射。那边是另一个仓库、另一份历史，
> 改动互不相干。

**当前状态：** agent（工具循环 / LLM / 五类 effect 放行 / MCP client / skills）已端到端可用；
channel 接缝已就位、**iLink provider 还没写**。

---

## 快速开始

```bash
make build      # 构建全部（零 CGO）
make test       # 全部测试
make verify     # 提交前的闸门：fmt-check → vet → test → build → smoke
make release    # 交叉编译 6 个平台到 dist/（带 sha256）
make install    # 装到 PREFIX（默认 /usr/local）
make help       # 全部目标
```

**`fka version` 能查出二进制是哪一版编的**，还能验证零 CGO 那条硬约束：

```
$ fka version
1d327d5-dirty
commit:    1d327d5
built:     2026-09-28T14:46:22Z
channel:   darwin/arm64
cgo:       off
```

cgo 那行靠 build tag 判定（`CGO_ENABLED=0` 时 cgo 包根本不编译，运行时问不出来），
所以它**只可能来自构建方式，不可能来自运行时猜测**。

**`make verify` 是提交前该跑的那一条。** 里面的 `smoke` 会在**隔离的临时目录**里
装一个技能与一个 MCP server，然后断言 `fka tools` 真的列出了它们——因为「技能
读到了吗」「server 连上了吗」这两件事**静默失败时从界面上看不出来**：工具列表
就是空的，而你没法区分「没配」与「配了但没生效」。

```bash
# 列出模型现在能看到的工具与五类放行情况
FKA_HOME=/path/to/fka LLM_TOOL_EFFECTS=read,external ./bin/fka tools

# 无头跑一轮工具循环问答（不经过任何渠道）
FKA_HOME=/path/to/fka LLM_TOOL_EFFECTS=read,external \
  FKA_PRINCIPAL=zhang ./bin/fka ask "记一条：2026年3月全家去了三亚"

# 常驻：接渠道、收消息、跑问答
FKA_HOME=/path/to/fka ./bin/fka serve
```

必填环境变量只有 `LLM_API_KEY` 与 `LLM_MODEL`。没配时 `ask` 会**明确报错**，
不会静默降级成空答案。

### 挂上 MCP server

```jsonc
// <安装根>/mcp.json
{
  "mcpServers": {
    "memory": {
      "command": "/path/to/fka/bin/fka-memory",
      "args": ["--db", "/path/to/fka/data/memory.sqlite"]
    }
  }
}
```

**MCP 工具一律是 `external` 类**，要显式放行：`LLM_TOOL_EFFECTS=read,external`。

`FKA_HOME` 指向安装根（`data/`、`logs/`、`.env`、`mcp.json`、`skills/` 都按它解析）。
不设时取**可执行文件所在目录**——静态二进制的天然锚点，跨机器拷贝后仍然自洽。

### 技能

往 `<安装根>/skills/<名字>/SKILL.md` 放一份操作说明即可，**改完不用重启**。
`FKA_SKILLS_DIR` 可以给多个目录（系统路径分隔符隔开），**同名技能后者覆盖前者**。
技能格式与多目录覆盖规则见 `internal/tools/skills/skills.go` 的文件头注释。

---

## 能力从哪来

只有两条路，**都是外部的**：

```
                     ┌─────────────────────────────┐
                     │  internal/agent  工具循环     │
   一段问题 ────────▶│  internal/llm    模型契约     │
   + 一个身份         │  internal/tools  放行策略     │
                     │  internal/prompts 系统提示词  │
                     └──────┬───────────────┬──────┘
                            │               │
              ① MCP server  │               │  ② skill
                            ▼               ▼
              ┌───────────────────┐  ┌──────────────────┐
              │ stdio / HTTP 子进程 │  │ skills__list     │
              │ mcp/memory         │  │ skills__load     │
              │ 你的任何一个 server  │  └──────────────────┘
              └───────────────────┘
```

**技能是「做法」，MCP 是「能力」。** MCP 工具是查一下、调一下外部系统；
skill 是写给人看的操作步骤——先做什么、注意什么、怎么算做完。
所以技能**只在系统提示里给名字与一句话说明**，需要时再 `skills__load` 取全文：
全文动辄几千字，全塞进 prompt 会把上下文预算吃光，而大部分轮次用不上。

---

## ⚠️ 一句话必须知道的

身份参数搬进 MCP 之后**变成模型填的工具参数**，server 按决定不做进程级绑定。
而 server 认的身份参数是**模型填的**——它一旦把**不可信内容**喂进 LLM 上下文，**提示注入就是
结构性暴露**：一次注入或模型判断失误就可能泄露别人的 private 数据，且**不报错**。

**怎么补**（改动很小）见 [docs/permissions.md](docs/permissions.md#没做的缓解以及怎么补)。

---

## 架构

```
fka ask "…"  /  渠道来的 InboundMessage
   │
   ▼
┌──────────────────────────────────────────────────────┐
│ cmd/fka  主程序                                        │
│  ├─ internal/agent     工具循环（≤N 步）              │
│  ├─ internal/llm       模型契约 + 历史压缩 + 会话历史   │
│  │   └─ openai/        OpenAI 兼容 provider（流式+双超时）│
│  ├─ internal/prompts   系统提示词唯一出处               │
│  ├─ internal/tools     工具接缝 + 五类 effect 放行      │
│  │   ├─ mcp/           MCP client（mark3labs）         │
│  │   └─ skills/        技能源（读 SKILL.md）           │
│  ├─ internal/channels  渠道接缝（注册/唯一性/广播/寻址）  │
│  └─ internal/app       装配根（唯一装配点）             │
└──────────────────────┬───────────────────────────────┘
                       │ mcp.json：stdio 子进程
       ┌───────────────┴───────────────┐
       ▼                               ▼
┌────────────────────────┐
│ mcp/memory             │
│ bin/fka-memory         │
│ 自给自足：表/schema/   │
│ 迁移/日志/默认库全归它 │
└───────────┬────────────┘
            ▼
   ┌─────────────────┐
   │mcp/memory/      │
   │  internal/store │
   │  memories 一张表 │
   └─────────────────┘
```

**装配点唯一**在 `internal/app/app.go`——构造函数链，没有 Cordis 也没有 `cordis.yml`
（理由见 [decisions.md](docs/decisions.md) 第 4 条）。

**两侧是硬边界**：`mcp/memory/**` 不许 import 本仓库树外的任何包，由 `boundary_test.go` 守着
（Go 的 `internal` 规则只管「树内不许外泄」，管不了「树外不许伸手」）。agent 侧不认识存储，
server 侧不认识 agent——所以 server 能单独构建、部署、换掉。
详见 [mcp/README.md](mcp/README.md#自己管理自己)。

---

## 渠道

`internal/channels` 是**接缝**，它不认识任何渠道实现（`channels` 包的边界测试守着）。
渠道以 `Provider` 形式注册，接缝负责四件事：登记实例、保证 `(种类, 账号)` 与
**跨渠道账号标识**全局唯一、把入站回调广播给订阅者、按用户给的 `--channel/--account` 寻址。

> **跨渠道账号唯一**不是洁癖：会话历史文件名是 `<账号>_<会话>.jsonl`，
> 撞号会让两个渠道的对话写进同一个文件，症状出现在很远的地方且**不报错**。

**消息层（`internal/messages`）只认三样东西**：进来的 `InboundMessage`、干活的
`agent.Runner`、回去的 `Channel`。**它不认识 iLink，也不认识任何具体渠道。**

`fka serve` 是常驻入口，它保证**先订阅再开收**——反过来会有一个丢消息的窗口。

**iLink（微信）provider 已接上**：`cmd/fka/serve.go` 的 `channelProviders()` 里返回它。
接第二个渠道 = 再加一个 `Provider`，接缝与业务层不动。

登录扫码走 `Provider.Ops().Login`（账号槽位 `ILINK_ACCOUNT_<N>_*`）。
**凭证只落 `.env`（0600）**，状态快照里**不出现 context_token**——它等同于发消息的资格。

---

## 目录

```
go/
├── cmd/fka/            主程序：ask（无头问答）/ tools（工具清单）
├── mcp/                MCP server（能力，不是 agent）
│   ├── memory/         家庭记忆 server —— **自给自足的独立项目**
│   │   └── internal/   **只有本树能 import**（编译器保证）
│   └── README.md       挂载方式、存储布局、三条硬约束
├── docs/               本项目文档（见下表）
└── internal/           agent 侧。**没有 store / nas / 任何持久化**
    ├── agent/          工具调用循环 + runner
    ├── app/            装配根
    ├── channels/       渠道接缝（不认识任何渠道实现）
    │   └── ilink/      微信渠道：adapter（实现 Channel）+ provider（实现 Provider）
    │       └── bot/    协议实现：报文 / 加解密 / 发送 / 上传 / 长轮询 / 登录
    ├── config/         安装根解析、.env 加载、日志（按天轮转）
    ├── llm/            模型契约 + 历史压缩 + 会话历史
    │   └── openai/     OpenAI 兼容 provider
    ├── messages/       入站消息 → 工具循环 → 按原路答复
    ├── prompts/        系统提示词唯一出处
    └── tools/          工具契约 + 放行策略 + 注册表
        ├── mcp/        MCP client + mcp.json + 聚合源
        └── skills/     技能源
```

**依赖只有三个：** `sashabaranov/go-openai`、`mark3labs/mcp-go`、`modernc.org/sqlite`
（纯 Go，零 CGO）。**没有 CGO**——每个模块对 CGO 的需求都逐个检查过，见
[decisions.md](docs/decisions.md#附零-cgo-是怎么达成的)。

---

## 文档

| 文件 | 讲什么 |
|---|---|
| **[docs/port-plan.md](docs/port-plan.md)** | **先读这个。** 模块清单与进度、实施顺序、每层的验收标准、**改任何东西都不能破的不变量** |
| **[docs/decisions.md](docs/decisions.md)** | 技术决策与「为什么是它而不是别的」，含每条的**后果**（很多是已接受的代价） |
| **[docs/permissions.md](docs/permissions.md)** | ⚠️ 权限模型，以及**唯一一处「防线从代码移到模型手上」的地方** |
| **[mcp/README.md](mcp/README.md)** | 记忆 server：自己管自己的表、schema、迁移、日志与库文件 |
| **[docs/node-to-go.md](docs/node-to-go.md)** | Node → Go 的模块映射、**有意不同的几处**、**必须 1:1 不许优化的几处** |
| [docs/dev-log.md](docs/dev-log.md) | 开发日志（最新在上） |

渠道契约目前以**代码与测试**为准：`internal/channels/types.go` 是接口，
`internal/channels/channels_test.go` 是可执行的契约说明。iLink 接进来时再补设计文档。
