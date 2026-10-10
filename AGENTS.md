# AGENTS.md

> 本文件只写「不读就会踩」的东西。背景与理由在 `docs/`，**改代码前先读 `docs/port-plan.md`**
> （模块进度、验收标准、不可破的不变量）。

## 这是什么

Go 版 agent 工具循环 + 一个微信渠道。**它自己不带任何内置能力，也不拥有任何数据**——
能力只有两条来源：`<安装根>/mcp.json` 里的 MCP server，和 `<安装根>/skills/` 下的 skill。
整个 module 只有 5 个直接依赖，全部纯 Go。

**唯一的例外是 `internal/tools/reply`**：它**不是能力，是输出通道**——把「往当前会话
回话」（文字 / 文件 / 图片）也做成工具交给模型，好让它能先发说明再发文件。它不拥有
数据、不出网、不拉进程，所以没破坏上面那条原则。三个工具的 effect 都是 `send`，
**默认关着**（`LLM_TOOL_EFFECTS` 默认只有 `read`）；发文件只认**发送根**内的相对路径
（默认 `<安装根>/sandbox`，即 `fka-bash` 的沙盒根，`FKA_SEND_ROOT` 可改），越界、软链
逃逸、非普通文件、超上限都在 `reply.resolve` 里拒掉。见 `docs/decisions.md` 第 17 条。

**远端沙盒（SSE）的文件走另一条路：字节端到端。** 沙盒用 MCP 标准内容块返回字节，
`annotations.audience=["user"]` 表示「给用户」；agent **只认这个字段、不认来源**，由
循环经渠道投递（`mcp/bash` 的 `export` 工具是第一个这么做的）。投递与 reply 同一道
`send` 闸门。这条把沙盒与 agent 的耦合从「共享一块磁盘」降成「遵守同一个 MCP 字段」——
见 `docs/decisions.md` 第 18 条。

## 闸门与命令

```bash
make verify      # 提交前跑这一条：fmt-check → vet → test → build → smoke
make help        # 第一行打印当前实际 FKA_HOME
```

**`make ci` 现在跑不通**（2026-09-29 实测）：第二步是 `go test -race`，而 Makefile
全局 `export CGO_ENABLED=0`（零 CGO 是硬约束），Go 直接拒绝 `-race`。
要竞态检测就手动：

```bash
CGO_ENABLED=1 go test -race -count=2 ./...
```

- **顺序是有意的**：先 fmt-check 再 vet，否则 vet 的报错里混着格式噪音。
- **`gofmt -w` 改完文件还返回 0**，所以「跑一下 gofmt 看看格式对不对」查不出问题。
  必须用 `gofmt -l .`（`make fmt-check`）看输出是否为空。
- 单包 / 单用例（测试名是中文，`-run` 要整体加引号）：

```bash
go test ./internal/tools -run 'TestRegistry_未放行的工具不告诉模型' -v
go test -race ./internal/channels
```

- `make verify` 里的 `smoke` 是**能力链路唯一的自动闸门**：它在隔离的临时目录里装一个
  skill 与一个 MCP server，断言 `fka tools` 真列出了 `skills__load` / `skills__list` /
  `mcp__memory__search_memories` / `mcp__memory__remember_memory`，并且记忆库落进了
  `mcp.json` 里 `cwd` 指定的那份安装根。这三件事（技能读到没、server 连上没有、
  子进程在哪个目录跑）**静默失败时从界面上完全看不出来**——`cwd` 失效的表现是
  工具照常列出来、文件写到别处去了，所以只能查文件落没落对。
  「技能列表是空的」是正常状态，不是没装好。
  同一道闸门还管另外三件：`fka tools --json` 的第一个字节必须是 `{`（日志混进 stdout
  时退出码仍是 0）、`fka version` **零副作用**（连 `logs/` 都不该建）、以及
  `smoke-memory` 的**跨进程端到端**（A 进程记一条 private → B 进程查回来 →
  换个 viewer 查不到 → stdout 只有 JSON-RPC 帧）。最后那条是记忆 server 唯一
  真正的端到端验证：它的单元用例只覆盖迁移与边界，**不覆盖 JSON-RPC 那条路**。
- 需要真机微信账号的验证**跑不了自动化**：`make real-check` 只是个提示。
  它引用的 `docs/real-machine-test.md` **当前不存在**（Makefile:70/315 指向它）。

## 安装根：最容易踩的一件事

`data/ logs/ .env mcp.json skills/` 全按**安装根**解析，规则只有一条（`internal/config/config.go`）：

```
安装根 = $FKA_HOME  →  没设就是「可执行文件所在目录」  →  再没有才是 cwd
```

| 怎么跑 | 安装根 | 技能目录 |
|---|---|---|
| `make serve` / `make login`（Makefile 把 `FKA_HOME` 设成 `$(CURDIR)`） | 仓库根 | `<根>/skills/` |
| `FKA_HOME=/x ./bin/fka serve` | `/x` | `/x/skills/` |
| **`./bin/fka serve`（不设 `FKA_HOME`）** | **`bin/`** | **`bin/skills/`** |

第三行是绝大多数「我的技能怎么不生效」的来源：**直接跑二进制和走 Makefile 读的不是同一份配置。**
仓库里**没有** `skills/` 目录（`skills/` 未入库），所以 `fka tools` 的技能段为空是对的。
`make install` 装到 `/usr/local` 后安装根会变成 `/usr/local/bin` —— 要么把配置放那儿，要么 `export FKA_HOME`。

## 零 CGO 是硬约束

- 每个 Makefile 构建目标都**显式** `CGO_ENABLED=0`，不靠外部环境变量（靠环境变量的话
  一次 `CGO_ENABLED=1 make` 就悄悄破掉了）。
- `fka version` 打印的 `cgo: off` 由 build tag 判定（`cmd/fka/cgo_on.go` / `cgo_off.go`），
  所以它**只可能来自构建方式**。加了要 cgo 的依赖 = 一次有意识的决定，不是顺手。
- `mark3labs/mcp-go` 现在钉在 **v1.1.1**（2026-09-29 从 v0.40.0 升上来）。
  原来的「必须钉在 v0.40.x」的理由是 **v1.0.0+ 要 Go 1.25.5**，而本机工具链已经是
  go1.27.1，那条理由不再成立；`go.mod` 的 `go` 指令因此从 1.25.0 抬到 **1.25.5**
  （依赖的最低要求，抬它是被迫的）。
  升级踩到的**唯一一个真问题**在 `internal/tools/mcp/client.go`：
  `mcp.LATEST_PROTOCOL_VERSION` 在 v1.1.1 起是 `2026-07-28`（无会话的 stateless 协议），
  而我们仍然走 initialize 握手 —— 客户端于是认定自己 stateless、**不再发
  `Mcp-Session-Id`**，第二次请求就得到 `session terminated (404)`。所以那里报的是
  `mcp.LATEST_LEGACY_PROTOCOL_VERSION`。**再升一次 SDK 之前先读这一段。**
- 代价是 SQLite 用 `modernc.org/sqlite`（比 CGO 版慢 20–50%，家庭规模不可测量）。
  **连接池限 1、DSN 加 `_txlock=immediate`**，否则 `SQLITE_BUSY` / 升级写锁死锁。

## 两条有测试守的架构边界

Go 的 `internal` 规则只管「树内不许外泄」，**管不了「树外不许伸手」**，所以这两条靠 AST 扫描的测试守：

- `mcp/memory/**` 不许 import 本仓库树外的任何包（`mcp/memory/boundary_test.go`）。
  破了它，server 就跟别人的装配绑死，不再是「能单独构建、部署、换掉的能力」。
  注意 `mcp/memory` 必须保持 `package main`。
- `internal/channels` 不许 import 任何具体渠道（`internal/channels/channels_test.go`）。
  接缝的价值全在「加渠道零改业务层」上，破了是**静默失效**：照常编译、照常跑。

**MCP server 启动后 stdout 一个字都不能有** —— stdout 是 JSON-RPC 通道，任何
`fmt.Println` 都会把 server 打挂。诊断一律走 `internal/log`。

## 权限过滤必须在 SQL 的 WHERE 里

`viewer_wxid` 现在是**模型填的工具参数**，server 按决定不做进程级绑定，而这份产品按设计
就要把不可信内容喂进 LLM 上下文 —— **提示注入是结构性暴露，不是假想风险**，且失败时不报错。

`(visibility = ? OR owner_wxid = ?)` 是唯一的数据防线：**必须留在 `WHERE` 里、每次查询都带**。
查完再筛要每处都记得加一遍，漏一处就泄漏。收紧的做法见 `docs/permissions.md`。

## 这些名字改了就是改了产品

| 不能动 | 为什么 |
|---|---|
| 工具全名 `<源>__<工具>`、`mcp__<server>__<tool>` | 模型在会话历史里逐字重放；改名会让那一轮起前缀缓存全失效 |
| `tool_calls` 的 `arguments` **保持原始 JSON 字符串** | 反序列化再序列化会改变字节序 |
| 历史按**整组**丢弃，绝不改写留下的 | 同上；留下孤儿 `tool` 消息直接 400 |
| 记忆四类 `event`/`reminder`/`experience`/`knowledge` | 库里已有数据 |
| 认领失败**绝不重建** | 重建会「修好」错误，代价是数据没了，而用户看到的是安静的库，不是事故 |

## 本仓库的写法约定

- **测试名是中文句子**：`Test主体_场景` 或 `Test一句话描述`（`TestMigrate_认领失败不重建`、
  `TestRegistry_未放行的工具不告诉模型`）。`t.Run` 的子测试名同理。这不是玩笑，409 个顶层用例都这样。
- **文件头注释解释「为什么」，不解释「是什么」**：几乎每个非平凡文件开头都有 `//` 头，
  带 `##` 小节、`**加粗**` 的关键论断、常见「刻意这么做」的解释（有时还写清
  Node 版原来的做法与它的痛点）。写新文件请照这个密度写。
- **库代码里不用 `fmt.Println`**，走 `config.Log().Warn/Info/Error/Debug` + `config.Context{...}`。
  CLI 的 stdout 是给人看的结果，刻意把日志级别压到 Warn（`cmd/fka/main.go`）。
- 退出码是契约：`0` 成功 / `1` 预期内失败 / `2` 用法错。`fka serve` 在没接上渠道时**必须**
  明确报错并以 1 退出，不能安静地收不到消息（`make smoke` 就在钉这条）。
- 提交信息用 conventional commits + 中文主题：`feat(skills): …` / `fix(ilink): …` / `build: …`。
  `docs/dev-log.md` 约定「写日志先于提交，一个完整功能一次提交」。

## 装配点唯一

`internal/app/app.go` 是**唯一**装配点（构造函数链，没有 cordis.yml）。业务包互相不认识，只认契约。

- 加一个工具源 = `Build()` 里加一行 `app.Tools.Use(...)`。
- 加一个渠道 = `cmd/fka/serve.go:channelProviders()` 里多一个 provider。
- `internal/prompts` 是系统提示词的**唯一出处**，别在别处内联提示词文本。
- 加 MCP tool effect：**MCP 工具一律是 `external`**，`LLM_TOOL_EFFECTS` 默认只有 `read`
  —— 不显式加 `external` 就一个 MCP 工具都看不到。五类：`read/memory/send/delete/external`。
- 只声明**真的能跑**的工具：声明一个跑不了的比不声明更糟（模型会调它、收失败、再换个方式试）。

## 依赖纪律

只有 5 个直接依赖，且**刻意保持很少**：`.env` 解析是自己写的 60 行（不引 dotenv），
skill front matter 解析不引 YAML 库（只认 `key: value`，认不出的当正文，
**绝不因为格式不合就丢掉一个技能**）。加依赖前先想清楚能不能手写。

唯一的例外是 `github.com/ergochat/readline`（`fka chat` 的行编辑）：中文回退残留是
内核 canonical 回显按「列」而不是按「字符宽度」擦除导致的，**这一件手写不出来**
（要么自己维护一整套 East Asian 宽度表与折行重绘，要么引它），而它纯 Go、CJK 宽度
按 `x/text/width` 算、跨平台，且只多一个模块（`x/sys`、`x/text` 本就在 go.sum 里）。

## ⚠️ 这个仓库只有「agent + 一个渠道」，文档/NAS/向量那半边已经搬走了

`1fae643`（agent 侧不再有任何存储）与 `0fb673b`（记忆 server 自给自足）把
`internal/{nas,store,ids,domain,searchterms,mcpboot}` 与整个 `mcp/docs` **搬了出去**。
所以下面这些路径**在本仓库不存在**，在文档里读到它们时先想一下是「历史」还是「待办」：
`internal/nas`、`internal/store`、`internal/mcpboot`、`internal/ids`、
`internal/domain`、`internal/searchterms`、`mcp/docs`（记忆存储现在在
`mcp/memory/internal/store`）。

`README.md` / `port-plan.md` / `decisions.md` / `permissions.md` 已按现状整理过（2026-09-29）；
**`docs/dev-log.md` 不要改**——它记的是当时的状态，里面的路径有的已经搬走了。
`docs/node-to-go.md` 被人删掉了（一直没写），引用它的死链已清掉。

**实际存在的树**（107 个 .go 文件，409 个顶层用例）：

```
cmd/fka/            入口：ask / chat / tools / serve / login / version
internal/agent      工具循环（≤N 步，LLM_MAX_STEPS）
internal/app        装配根（唯一装配点）
internal/channels   渠道接缝 + ilink/{adapter,provider} + ilink/bot（协议：加解密/上传/长轮询/登录）
internal/config     安装根解析 + .env 加载 + 按天轮转的日志
internal/llm        模型契约 + 历史压缩 + 会话历史 + openai/（流式 + 双超时）
internal/messages   入站消息 → 工具循环 → 按原路答复
internal/prompts    系统提示词
internal/tools      契约 + 五类 effect 放行 + 注册表 + mcp/ + skills/
mcp/memory          独立 MCP server（memories 一张表，PRAGMA user_version 迁移）
mcp/bash            独立 MCP server（沙盒 bash 执行；默认 bwrap 命名空间隔离）
```

> `fka-bash` 是**第二个** MCP server，同样自给自足（boundary 测试禁止引树外包）。
> 它默认用 **Bubblewrap** 做真实隔离：`/` 只读、只有沙盒根可写、独立网络/PID/user；
> **找不到 `bwrap` 拒绝启动**，不会静默退化成不隔离的 `direct` 档。`direct` 只固定 cwd，
> 挡不住 `cd /`，只在没有 bwrap 的平台（macOS/Windows）用。这是本仓库**唯一的外部
> 运行期依赖**（不是 Go 依赖，不破坏「5 个直接依赖」与零 CGO）。

<!-- aoci:begin -->
## AOCI 仓库认知

AOCI 为本仓库维护一个稳定、可版本化、可增量更新的仓库级认知层，供模型跨任务复用对系统的理解。

`aoci.txt` 是面向模型的结构化认知索引。它以每个受管理文件、数据库表或其他受管理对象一条独立 Entry 的方式，用符号标签与 F/R/A/S 语义表达对象的核心职责、重要关系、对外契约，以及理解或修改系统时必须知道的非显然约束和设计决策。

Header、目录段和全部 Entry 共同组成完整仓库索引，可以覆盖前端、后端、配置、数据库结构及其他受管理内容。受管理内容发生变化时，通常只需维护受影响的认知条目，不需要重新生成整个索引。

AOCI 提供系统架构、对象职责、重要关系、对外契约和关键约束的高密度视图。

### 工作原理

AOCI 采用“模型生成、模型读取”的认知闭环。

Header、Entry 和 Curation 语义的创作只按当前机器签发的 Plan 与实时 Guide 执行；由 Host 模型基于当前绑定证据独立完成。

Entry 的语义必须来自模型对真实证据的理解。不得仅依据路径、文件名、扩展名、AST、符号列表、依赖扫描、正则、固定模板或规则引擎推导、预填、拼接或改写索引语义。

对 Fresh Bootstrap，只按当前机器签发的 Plan 和实时 Guide 执行。当它们要求创作时，Host 模型创作 Root、Meta、Tag 和 F/R/A/S，提供 authoring-run 声明，并把它绑定到 Plan、Evidence 与完整 Candidate。不得要求 AOCI 填写 `origin=host_model`、制造 Receipt 或把程序生成的 Framework 当作语义。本文件不自行重建 Onboarding 流程。内部批次不是用户决策；只有遇到既有批准边界或真实的安全、漂移、CAS、Recovery 条件才停止。

### 最小使用入口

- `aoci_rules`：取得当前AOCI版本的会话运行合同。
- `aoci_overview`：建立或恢复本仓库的完整认知。
- `aoci_maintain`：受管理对象达到最终稳定状态后检查认知是否需要维护。
- `aoci_update_entry`：提交与当前证据和源码摘要绑定的完整语义更新批次。
- `aoci_report`：仅当当前布局和工具状态支持时，在证据不足、无法可靠生成语义时登记待办，不猜写。

其他MCP工具、CLI命令、参数和专项流程，以当前工具说明、Guide和 `--help` 返回内容为准，不在本文件中重复完整手册。

本区块只规定仓库接入、认知使用和收尾原则。`aoci_rules` 承载当前会话合同，Guide实时输出承载当前Plan的执行顺序与停点，工具Schema、Spec和Validator承载机器结构与判据；Prompt、Description、README和静态文档不能覆盖这些机器事实。

### 建立、生成和恢复认知

1. 每个新的 Agent Run 开始时，应先判断：

   - 本仓库是否已经存在可用的完整AOCI索引；
   - 当前上下文中是否已有与本仓库根、当前索引版本和当前AOCI服务相匹配，并且模型仍可可靠使用的完整仓库认知。

2. 仓库已经存在可用的完整索引，但当前Run没有可靠完整认知时，先调用 `aoci_rules`，再调用 `aoci_overview`。

   完整认知仍可靠时直接复用。局部不确定本身不要求机械重读系统全貌。

   本Run从已知Host上下文压缩恢复时（包括宿主注入的压缩摘要），必须把此前模型认知视为不可靠。压缩handoff不得保留或摘要正式Whole-Index，也不得保留或摘要任何Overview Header、Entry、Chunk、Challenge或Attestation正文；只能保留安全续接所需的receipt身份、未完成write或Recovery状态，以及立即重载指令。复制进handoff的Whole-Index语义或receipt不能证明恢复后模型的当前认知可靠。若当前上下文已无法可靠保留运行合同，先调用 `aoci_rules`。继续业务任务前，使用 `refresh_reasons=["context_compaction"]` 和新的 `refresh_event_id` 调用普通完整Whole-Index `aoci_overview`（不设置 `check_only` 或设为false）；不得使用 `check_only` 或认知probe。原样跟随每个 `next_cursor` 直到 `completed=true`，确认交付，并且只基于新交付正文提交一次Attestation。完成这次新的完整传输后，即使Attestation为partial或fail也消费该generation，并按既有合同继续source-bound任务，不再自动调用第二次Overview。

   AOCI可以针对 `context_compaction`、项目 `cognition_refresh_threshold` 下的机器 `semantic_threshold` 或主要 `phase_transition` 提供checkpoint与认知状态事实。只需要这些紧凑事实时使用 `check_only=true`；这些事实只向Agent提供建议，不替模型决定是否需要系统全貌。

   Agent显式调用普通 `aoci_overview`（未设置 `check_only` 或为false）时，只要能形成一致的CognitionSet，AOCI必须完整交付请求scope。不得因为已有receipt、阈值未达到或没有待处理刷新原因而抑制正文。正式认知Dirty或Stale时仍交付正文，但必须标记不可靠。存在未决恢复或无法形成一致snapshot时失败关闭，不返回混合正文。

   普通Overview返回 `continuation_required=true` 时，必须原样提交 `next_cursor` 并自动继续到 `completed=true`。不得询问用户、开始业务任务或给出阶段性系统结论。Host截断、缺块、重复、乱序、cursor失败、Index变化或`chunk_tokens`变化时停止本次认知链。Attestation完成前不得用Memory、源码、Spec、`aoci.txt`、历史会话、scope、search或Entry读取修补或补充Whole-Index认知。Challenge ordinal是正式Entry序列中的1-based位置；Header内容、注释、空行、Section/Overview/Chunk Marker、Receipt与Metadata均不计数，Chunk Receipt ordinal使用同一序列。Attestation必须原样回绑本次Challenge发布的当前`index_sha256`、`entry_sequence_sha256`与`entry_count`；旧Index、旧Entry序列、旧数量或旧Attestation均无效。完整链结束后只正式提交一次既有模型认知Attestation；同一响应只允许一次不改变语义答案的JSON Schema或字段格式修正。对象、Tag或F不匹配即失败且认知吸收不确定，不得语义重试或旁路补答。首次认知失败时还不得执行Root/Meta、Migration、全局布局或其他未重新绑定的系统级决策。上下文压缩刷新若传输完整、认知身份不变、治理对齐且没有Recovery或第三方冲突，即使Attestation为partial或fail也消耗该refresh generation，并继续原任务，不再自动重读Overview。`system_mastery_percent`只自评系统框架——架构、职责、强关系、稳定外部契约以及高熵安全和维护约束——不表示完整实现或运行实况知识；机器索引覆盖率必须分开。默认只向用户输出由本次真实覆盖率、Challenge、块数、Token和掌握度生成的规定成功或失败一句话。Host截断时提示用户把 `overview_delivery.chunk_tokens` 设置为更小的合法值后重新开始，不得自动修改。

   加法认知等级必须与严格证明字段分开解释。`delivery_verified`表示已加载Index且Host交付已确认，但完整认知验证仍未完成；应表达为“已加载且交付已验证”，不得描述为“没有认知”或“没有理解系统”。`cognition_verified`要求Attestation通过（Challenge至少80%的ordinal完全正确且对象身份至多失手一处），`cognition_governed`还要求治理对齐。通用完整读取失败句只用于真实交付故障。

   当Overview响应包含可选`cognition-state/v2`投影时，必须分别解释各维度。其Level止于`model_cognition_usable`；`strict_attestation_verified`、`governance_aligned`与`current_system_cognition_reliable`都是独立状态，绝不参与该Level。ordinal、对象身份、Tag或核心F不匹配可以导致严格Attestation失败，而模型认知仍然可用；不得仅凭这种不匹配就宣称模型没有理解系统。只有`current_system_cognition_reliable=true`允许无保留地声称当前完整系统认知可靠。投影缺失时继续使用上述Legacy解释。

   普通的只读审计、分析、检查、不修改代码或不提交、不push，不自动等于严格零写入，也不改变上述认知有效性判断。Codex Memory和历史Skill只能辅助恢复经验、用户偏好与调查方向，不能替代与当前仓库根、索引摘要、AOCI服务身份和认知范围匹配的当前认知收据；项目AGENTS和当前AOCI身份在AOCI状态上优先于历史Memory。

   只有用户明确禁止Ledger、元数据、`.aoci`运行资产及任何文件写入时，才按严格零写入处理。若必要的认知建立与该边界冲突，必须报告冲突并请求用户裁决或建议使用隔离副本，不得静默以Memory替代当前仓库认知。

3. 仓库没有可用的完整索引，或当前只有最小骨架、Header不完整、Entries未完成、必要Curation尚未裁决时，如果需要建立正式完整AOCI索引，先取得 `aoci_rules`，然后进入当前AOCI Guide。由Guide依据仓库真实状态决定下一阶段并完成必要安全步骤。

   `aoci_maintain` 不替代索引建立流程。

   不在本文件中自行重建或硬编码完整索引生成状态机。

4. 在长程任务中，模型负责保留当前认知收据并正确使用刷新门禁：

   - Host报告上下文压缩或模型已知系统全貌丢失时，执行上述强制 `context_compaction` 重载规则；AOCI不能自行推断Host事件；
   - 进入真正的主要阶段时声明 `phase_transition`，不得把函数、测试运行或小步骤当作阶段；
   - 在有用的稳定检查点通过 `check_only=true` 取得机器语义计数；
   - 除已知压缩的强制重载外，由Agent判断当前任务是否需要再次显式获取指定scope或完整Overview；
   - 在维护和对齐完成前，保留AOCI报告的Dirty或Stale可靠性状态。

### 任务收尾与认知维护

5. 纯只读问答、分析、版本核验，或没有产生受AOCI管理对象变化的任务，不需要调用维护工具。当前AOCI版本是任意`aoci_overview` check_only或`aoci_maintain`响应里的`cognition_receipt.mcp_service_version`；二进制路径是项目`.mcp.json`里的`command`，CLI不必在PATH上。

6. 发生受AOCI管理对象变化时，待其达到本次任务的最终稳定状态后，只调用一次 `aoci_maintain`。不要在每次中间修改后逐文件维护。

7. 若维护结果返回真实语义候选，Host 模型必须基于每个候选绑定的对象和必要证据，独立创作完整标签与F/R/A/S更新。通过 `aoci_update_entry` 一次提交当前机器签发批次的完整候选集合，同时原样保留每项 `source_sha256`、`candidate_id` 与对应domain批次身份。`max_entries`只限制单次请求和原子事务，不限制logical plan、Whole-Index或Managed Scope。`remaining`非零时，在当前批次成功Apply后重新调用Maintain并从新preimage继续；绝不能为满足transport上限缩减Index覆盖或自行截取返回批次。

   没有足够证据且当前布局支持 `aoci_report` 时，使用它而不猜测、套用模板或为消除待办而生成缺乏证据的认知。

8. 必须遵守工具返回的结构化状态和安全边界：

   - `repair_required`：只修复明确命中的候选，再重新提交当前机器签发的完整批次；
   - `stopped`：结束当前写入尝试并检查 `failed_step`、错误、正式写入证据与Recovery。auto模式下，已证明零写入则记录closure并重新Plan；完整Intent和可证明postimage则Resume；策略要求Rollback且preimage可证明则精确恢复后重新Plan。只有证据不足、第三方正式字节冲突、需要审批或外部动作，或命中其他真实安全边界时，才停止整个用户任务；
   - 冲突、审批、人工裁决、权限和安全信号不得忽略；
   - 已经对齐后不得重复维护或重复写入；`refresh_ready_for_overview` 是checkpoint事实，由Agent决定是否为下一阶段请求普通完整Overview。

   维护完成后如果又修改了任何受管理对象，之前的维护结果失效，应在新的最终稳定状态重新完成收尾。

9. 用户只限制业务文件范围，但没有明确禁止仓库托管资产时，AOCI托管资产可以在收尾阶段为保持认知一致而更新，并应在审计和提交中与业务文件区分。

   用户明确禁止修改 `aoci.txt`、`.aoci`、元数据或任何额外文件时，以用户限制为准，不得写入，并如实报告剩余不一致。

### 专项流程

初始化、完整索引生成、Header生成、Entries生成、数据库结构索引、Curation、人工评审和故障恢复，只按当前AOCI Guide或工具在对应阶段返回的指令、命令和安全停点执行。

不预加载、不猜测，也不自行重建这些专项流程。平台调用方式、请求格式、批次上限、审批规则、索引格式细节和恢复步骤由对应Guide、工具说明、模型Prompt和CLI帮助按需提供。
<!-- aoci:end -->
