# fka-go —— 通用 agent，带一个渠道接缝

一个**工具调用 agent**：模型自己决定调哪些工具、循环到得出答案为止。

**这个项目不带任何内置能力。** 它的本事全靠两条路进来——**MCP server** 与 **skill**。
它自己不拥有任何数据：没有数据库、没有 NAS、没有「家庭资料」这回事。
要什么能力就接一个 server 或写一份 skill。

> **这是独立仓库。** Node 版（在上级 `fka` 仓库里）仅作**历史参照与迁移来源**，
> 那边是另一个仓库、另一份历史，改动互不相干。**两边的模块映射没有写成文**——
> `1fae643`（agent 侧不再有任何存储）与 `0fb673b`（记忆 server 自给自足）之后，
> 文档/NAS/向量那条线整体搬出了本仓库，而那份映射表一直没写。

**当前状态：** agent（工具循环 / LLM / 五类 effect 放行 / MCP client / skills）、
微信渠道（iLink 协议 + 适配 + 扫码登录）与记忆 MCP server 都已端到端可用。
**没验证过的只剩真机那一段**：扫码登录成功过，收发消息这一段还需要真微信账号。

---

## 快速开始

```bash
make build      # 构建全部（零 CGO）
make test       # 全部测试
make verify     # 提交前的闸门：fmt-check → vet → test → build → smoke
make login      # 扫码登录 iLink（凭证写进 <FKA_HOME>/.env，权限 0600）
make serve      # 常驻：接渠道、收消息、跑问答
make real-check # 真机检查：**它只提示步骤，那份步骤文档还没写**
make why PKG=… # 某个依赖为什么在（`make why PKG=modernc.org/sqlite`）
make release    # 交叉编译 6 个平台到 dist/（带 sha256）
make install    # 装到 PREFIX（默认 /usr/local）
make help       # 全部目标
```

> **`make ci` 跑不通**：它第二步是 `go test -race`，而 Makefile 全局
> `export CGO_ENABLED=0`（零 CGO 是硬约束），Go 直接拒绝 `-race`。
> 要竞态检测就手动 `CGO_ENABLED=1 go test -race -count=2 ./...`。

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
装一个技能与一个 MCP server，然后断言 `fka tools` 真的列出了它们、并且子进程写的
文件落进了 `mcp.json` 指定的工作目录——因为「技能读到了吗」「server 连上了吗」
「server 在哪个目录跑」这三件事**静默失败时从界面上看不出来**：工具列表就是空的
（或者照常有），而你没法区分「没配」与「配了但没生效」。

```bash
# 列出模型现在能看到的工具与五类放行情况
FKA_HOME=/path/to/fka LLM_TOOL_EFFECTS=read,external ./bin/fka tools

# 无头跑一轮工具循环问答（不经过任何渠道）
FKA_HOME=/path/to/fka LLM_TOOL_EFFECTS=read,external \
  FKA_PRINCIPAL=zhang ./bin/fka ask "记一条：2026年3月全家去了三亚"

# 接着上一条 CLI 问的继续（不给 --session 就是每次一个新会话）
./bin/fka ask --session cli-3f2a9c1e-7b4d-4a2f-8e6c-1d5b0a9c3e7f "那去年呢"

# 交互式多轮问答：整场复用一个会话，回答写 stdout、提示与颜色写 stderr（Ctrl-D 退出）
FKA_HOME=/path/to/fka LLM_TOOL_EFFECTS=read,external \
  FKA_PRINCIPAL=zhang ./bin/fka chat

# 常驻：接渠道、收消息、跑问答
FKA_HOME=/path/to/fka ./bin/fka serve
```

**`ask` 不给 `--session` 时每次都是一个新会话**（`cli-<uuid>`，落在
`<安装根>/data/history/cli-<uuid>.jsonl`）。这是有意的：兜底曾是固定的一个名字，
于是**每一条**不带参数的 `ask` 都在续上一条——「模型忽然提起你半小时前随口问过的
那件事」，而命令行里什么都没变。想连续会话就显式给 `--session`（或 `FKA_SESSION`）。

**`ask` 把过程信息写到 stderr**：推理 `[推理] …`（`LLM_SHOW_REASONING=0` 可关）、
每次工具调用 `[工具] 名字（原样参数）→ 结果`、以及答案前的一行 `[助手]`。答案本身
仍只进 stdout，所以 `fka ask "…" > 答案.txt` 拿到的是一份干净答案。

**`fka chat` 正好相反**：它整场只用一个会话（`/new` 才换），所以每一轮都带着上一轮的
上下文，不需要你记 `--session`。回答只进 stdout、提示符与角色标签只进 stderr，
于是 `fka chat < 提问.txt > 回答.txt` 里那份文件是一串干净的回答。
`FKA_DEBUG=1` 会把这一轮用的会话 id 与步数打在 stderr（`ask` 与 `chat` 都认）。

必填环境变量只有 `LLM_API_KEY` 与 `LLM_MODEL`。没配时 `ask` 会**明确报错**，
不会静默降级成空答案。

### 挂上 MCP server

`command` 是**本地进程**（stdio），`url` 是**远程**服务器。两者可以混在同一个 `mcp.json` 里。

```jsonc
// <安装根>/mcp.json
{
  "mcpServers": {
    "memory": {
      "command": "/path/to/fka/bin/fka-memory",
      "args": ["--db", "/path/to/fka/data/memory.sqlite"],
      // 可选：stdio 子进程的工作目录，相对路径按安装根解析
      "cwd": "/path/to/fka"
    },

    "remote": {
      "url": "https://memory.example.com/df8908a6/sse",
      // 可选：sse（老）还是 http（streamable）。不给就按 url 是否以 /sse 结尾猜
      "transport": "sse",
      "headers": { "Authorization": "Bearer …" }
    }
  }
}
```

`cwd` 只对 `command`（stdio）那类服务器有意义。不给就继承 `fka` 自己的当前目录 ——
而 `fka` 是全局命令，从终端、从 launchd、从 Makefile 起各是不同目录，所以**要让
server 里的相对路径稳定，就得显式写 `cwd`**（写 `"."` 就是安装根）。

**HTTP 有两种互不兼容的传输**：老式 `sse`（GET 开着一条流，POST 只回 `202`，响应从流上回来）
与 `http` / streamable（直接 POST，响应在响应体里）。拿错的表现是握手失败，而配置读回来
完全正常。不给 `transport` 时按 **path 是否以 `/sse` 结尾**猜，猜的结果会记进日志
（`MCP 服务器已连接：xxx tools=N transport=sse`）；路径千奇百怪时**显式写 `transport`**。
连不上时错误信息里会直接告诉你该写什么。

**MCP 工具一律是 `external` 类**，要显式放行：`LLM_TOOL_EFFECTS=read,external`。

### 安装根（先看这个，否则找不到文件）

`data/`、`logs/`、`.env`、`mcp.json`、`skills/` 全都按同一个**安装根**解析。规则只有一条：

```
安装根 = $FKA_HOME
       ↓ 没设的话
       = 可执行文件所在目录
```

**「可执行文件所在目录」这件事很容易踩。** 二进制在 `bin/` 里，于是安装根就成了 `bin/`：

| 你怎么跑 | 安装根 | 技能目录 |
|---|---|---|
| `make serve` / `make login`（**推荐**） | 仓库根（Makefile 把 `FKA_HOME` 设成 `$(CURDIR)`） | `<仓库根>/skills/` |
| `FKA_HOME=/path/to/x ./bin/fka serve` | `/path/to/x` | `/path/to/x/skills/` |
| `./bin/fka serve`（不设 `FKA_HOME`） | `bin/` | **`bin/skills/`** |

第三行是绝大多数「我的技能怎么不生效」的来源：**直接跑二进制和走 Makefile 读的是两个不同的目录。**
不确定当前是哪个时，`make help` 的第一行会打印 `FKA_HOME=<实际值>`。

装到 `/usr/local` 时同理：`make install` 装完之后 `fka` 的安装根是 `/usr/local/bin`，
所以配置与技能要放 `/usr/local/bin/` 下面，或者干脆 `export FKA_HOME=/etc/fka`。

### 技能

往 `<安装根>/skills/<名字>/SKILL.md` 放一份操作说明即可，**改完不用重启**——
扫描每次都先看一眼目录，变了才重新读盘。

`FKA_SKILLS_DIR` 可以给多个目录（系统路径分隔符隔开），**同名技能后者覆盖前者**。
技能格式与多目录覆盖规则见 `internal/tools/skills/skills.go` 的文件头注释。

`skills__list` 与 `skills__load` **无条件注册**——目录空的时候它们也在，
`list` 返 `[]`（空列表是合法答案，不是失败），`load` 会把该放哪的目录说清楚。
所以「技能列表是空的」是正常状态，不是没装好。

`./fka tools` 会**直接列出发现到的技能**（名字 + 目录名 + 说明），不只是那两个
查技能的工具——因为「我改了 skills/ 目录，它怎么没反应」是最常见的一类问题，
而 `skills__list` 的存在只说明「有个办法能查」，回答不了「生效了吗」：

```
── 技能（发现到的）
   备份照片        <backup>        把手机里的照片归档到 NAS
   报销流程        <photo-album>   走完报销的每一步
   来自：/path/to/fka/skills
```

**目录名也打出来**，因为 front matter 里的 `name` 可能与目录名不同，而
`skills__load` 两个都认。`--json` 里也有（`skills` 与 `skills_dirs` 两个字段）。

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

> **跨渠道账号唯一**不是洁癖：账号是跨渠道的选择器（`--account` 不给渠道时按它找），
> 撞号会让那个查找有歧义。注册期就拒，比解析期再报「重名」更早、更明确。

**消息层（`internal/messages`）只认三样东西**：进来的 `InboundMessage`、干活的
`agent.Runner`、回去的 `Channel`。**它不认识 iLink，也不认识任何具体渠道。**

`fka serve` 是常驻入口，它保证**先订阅再开收**——反过来会有一个丢消息的窗口。

**iLink（微信）provider 已接上**：`cmd/fka/serve.go` 的 `channelProviders()` 里返回它。
接第二个渠道 = 再加一个 `Provider`，接缝与业务层不动。

登录扫码走 `Provider.Ops().Login`（账号槽位 `ILINK_ACCOUNT_<N>_*`）。
**凭证只落 `.env`（0600）**，状态快照里**不出现 context_token**——它等同于发消息的资格。

---

## 日志

`<安装根>/logs/<UTC 日期>.log`（比如 `2026-09-29.log`），**按天轮转，永远全量**
（控制台调静音也不丢）。一行一条，**普通文本**：

```
2026-09-29T08:03:47.683Z [INFO][account_002][LLM] 提交模型请求 account=account_002 host=127.0.0.1 messages=1 model=deepseek stream=true
```

行首三格是 `[级别][账号][哪一段]`，后面是消息与 `键=值` 字段。**控制台与文件同一套
排版**（控制台没有时间戳），所以三种捞法都成立：

```bash
grep '\[account_002\]' logs/*.log              # 那个账号的整条链路
grep '\[LLM\]'            logs/*.log           # 模型这一段
cut -d' ' -f2,3,4         logs/*.log           # 级别 / 账号 / 哪一段
```

「哪一段」是 `SYS` / `CHAN` / `MSG` / `PRM` / `LLM` / `RSN` / `TOOL` / `HIST`，
**每个日志点自己声明**——它是代码位置的性质，推不出来。账号由消息层绑在 ctx 上，
往下每层自动合并（`config.Bind` / `config.Fields`），**不靠各处手抄**。

控制台级别用 `--log-level` 或 `LOG_LEVEL` 定（`debug`/`info`/`warn`/`error`/`critical`）；
**认不出来直接以 2 退出**，不静默退回默认。CLI 默认把级别压到 `warn`。

**MCP server 的日志一律走 stderr**：`mcp.json` 给了 `env` 时子进程拿不到
`LOG_LEVEL`，级别会落回 debug——而它的 stdout 是 JSON-RPC 通道，一行日志就可能
把 server 打挂。

---

## 目录

```
fka-go/                     **仓库根就是模块根**（没有 go/ 那一层）
├── cmd/fka/               主程序：ask（无头问答）/ tools（工具清单）/ serve（常驻）
│                         / login（扫码登录 iLink）/ version
├── mcp/                   MCP server（能力，不是 agent）
│   ├── memory/            家庭记忆 server —— **自给自足的独立项目**
│   │   └── internal/      store / domain / searchterms / log（**只有本树能 import**）
│   └── README.md          挂载方式、存储布局、三条硬约束
├── docs/                  本项目文档（见下表）
├── internal/              agent 侧。**没有任何持久化**——数据全归 MCP server
│   ├── agent/             工具调用循环 + runner
│   ├── app/               装配根
│   ├── channels/          渠道接缝（不认识任何渠道实现）
│   │   └── ilink/         微信渠道：adapter（实现 Channel）+ provider（实现 Provider）
│   │       └── bot/       协议实现：报文 / 加解密 / 发送 / 上传 / 长轮询 / 登录
│   ├── config/            安装根解析、.env 加载、日志（按天轮转）、**日志归属绑定**
│   ├── llm/               模型契约 + 历史压缩 + 会话历史
│   │   └── openai/        OpenAI 兼容 provider
│   ├── messages/          入站消息 → 工具循环 → 按原路答复（**按账号分片**）
│   ├── prompts/           系统提示词唯一出处
│   └── tools/             工具契约 + 放行策略 + 注册表
│       ├── mcp/           MCP client + mcp.json + 聚合源
│       └── skills/        技能源
├── Makefile               verify / ci / release / install / why…
└── AGENTS.md              给 agent 看的：踩过才知道的规矩
```

`skills/` **不入库**（每个安装自己放），所以仓库里没有这个目录——`fka tools` 的
技能段为空是正常状态，不是没装好。

**本仓库不做文档 / NAS / 向量那条线。** 它在 `1fae643` 与 `0fb673b` 之后
**整个搬出去了**：要那些能力就接一个外部 MCP server（见上面「能力从哪来」）。
`docs/port-plan.md` 里记着这件事与本仓库剩下的待办。

**依赖只有四个：** `sashabaranov/go-openai`、`mark3labs/mcp-go`、`modernc.org/sqlite`
（纯 Go，零 CGO）、`skip2/go-qrcode`（登录时画二维码，**只有 `fka login` 用它**）。
**没有 CGO**——每个模块对 CGO 的需求都逐个检查过，见
[decisions.md](docs/decisions.md#附零-cgo-是怎么达成的)。

---

## 文档

| 文件 | 讲什么 |
|---|---|
| **[docs/port-plan.md](docs/port-plan.md)** | **先读这个。** 模块清单与进度、**改任何东西都不能破的不变量**、已知的债、本仓库剩下要做的 |
| **[docs/decisions.md](docs/decisions.md)** | 技术决策与「为什么是它而不是别的」，含每条的**后果**（很多是已接受的代价） |
| **[docs/permissions.md](docs/permissions.md)** | ⚠️ 权限模型，以及**唯一一处「防线从代码移到模型手上」的地方** |
| **[mcp/README.md](mcp/README.md)** | 记忆 server：自己管自己的表、schema、迁移、日志与库文件 |
| **[AGENTS.md](AGENTS.md)** | 给 agent 看的入口：闸门、安装根、零 CGO、**这些名字改了就是改了产品** |
| [docs/dev-log.md](docs/dev-log.md) | 开发日志（最新在上）。**记的是当时的状态**，里面的路径有的已经搬走了 |

渠道契约目前以**代码与测试**为准：`internal/channels/types.go` 是接口，
`internal/channels/channels_test.go` 是可执行的契约说明。

**两处「该有而没有」的文档**（别去找）：
`docs/real-machine-test.md`（`make real-check` 会打印这个名字，但文件没写），
以及 Node ↔ Go 的模块映射表（`1fae643` 之后一直没写，README 顶部那条链接已经删掉）。

