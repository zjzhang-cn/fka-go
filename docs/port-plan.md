# 现状与不变量

> 这一页讲**现在是什么样**、**哪些东西不能破**、**还欠什么**。
> 改代码前扫一眼「不变量」那张表——那些是踩过才知道值钱的。

---

## 这个仓库是什么

**一个工具调用 agent，加两个渠道：微信（iLink）与网页（SSE）。** 它**不带任何内置能力，也不拥有任何数据**——
能力只有两条来源：`<安装根>/mcp.json` 里的 MCP server，和 `<安装根>/skills/` 下的 skill。

> **唯一的例外是 `internal/tools/reply`**：它不是能力，是**输出通道**——把「往当前
> 会话回话」（文字 / 文件 / 图片）也交给模型调度。effect 都是 `send`、默认关中；
> 发文件只认发送根内的相对路径，四条越界检查在 `reply.resolve`。见 `decisions.md` 第 17 条。

> **文档 / NAS / 向量那条线已经不在这里了。** `1fae643`（agent 侧不再有任何存储）与
> `0fb673b`（记忆 server 自给自足）把 `internal/{nas,store,ids,domain,searchterms,mcpboot}`
> 与整个 `mcp/docs` **搬了出去**。所以本仓库里：
>
> - **没有** `internal/store`、`internal/nas`、`internal/ids`——看到这些路径的文档段落，
>   记的是搬走之前的状态（`docs/dev-log.md` 里全是，那是日志，不改）；
> - **「文档转换 / 切片 / 向量 / 混合检索」不是本仓库的待办**。要那些能力，接一个外部
>   MCP server——那正是「能力只从 MCP 与 skill 进来」这条架构的意思；
> - 剩下的未开始项只有本仓库自己的那几件，见文末。

---

## 当前状态

**Agent 侧（工具循环 / LLM / 放行 / MCP client / 技能 / 渠道接缝）、微信渠道（iLink）
与记忆 MCP server 都已端到端可用**，两侧之间只有 `mcp.json` 一个接口。

**记忆 server 自给自足**：表、schema、迁移、日志、默认库文件全归它自己，
不与任何人共享一行代码。

已验证的链路（`make verify` 的 `smoke` 每次都在跑这四条）：

```
fka tools                       # 真机装一个 skill + 一个 MCP server，断言四个工具都列出来了
  → 技能读到没、server 连上没有，这两件事静默失败时界面上完全看不出来

fka tools --json                # 第一个字节必须是 `{`——日志混进 stdout 时退出码仍是 0
fka version                     # 零副作用：连 logs/ 都不该建（惰性日志器 + 纯输出命令先返回）

make smoke 的 smoke-memory      # 记忆的端到端，**走真实二进制与真实 SQLite**
  → A 进程 remember_memory（private）
  → B 进程 search_memories 查回来（这才叫落库）
  → 同一次调用换 viewer 再查：查不到（权限过滤在真实链路上生效）
  → stdout 只有 JSON-RPC 帧

make smoke 的 smoke-bash        # 沙盒 bash 的端到端，**真起 bwrap**
  → 命令跑得动且在沙盒根内
  → cwd=../.. 越界被拒
  → 往 /etc 写被只读根绑定挡住（沙盒是真隔离，不是说法）

（另有 fka ask 的完整 tool-loop 链路：需要模型，见 docs/ 里那条
  `fka ask "记一条：…"` → remember_memory → SQLite → search_memories → 作答）
```

**没有验证过的**：真微信那一段。`fka login` 扫码成功过（`8f61b96`），
**收一条 / 回一条 / 发一个文件都没在真机上跑过**——协议层的 91 个用例覆盖不到
长轮询的活体行为。这不是「大概没问题」，是**明确没验**。

---

## 模块清单

用例数是 `go test -list` 的条数（顶层 `Test` 函数，不含 `t.Run` 子测试）；
**全仓 409 个顶层用例**。

| 模块 | 落点 | 用例 | 关键验证 |
|---|---|---|---|
| **工具循环** | `internal/agent` | 13 | 步数上限是硬约束、工具失败转 tool message、收尾兜错、模型的错往上抛 |
| **工具接缝 + 注册表** | `internal/tools` | 11 | 源前缀、effect 放行、参数校验、**两种「没有」是不同的话** |
| **MCP client** | `internal/tools/mcp` | 18 | mark3labs、stdio + HTTP、惰性连接、连不上跳过、工作目录与 transport 的两种错法 |
| **LLM provider** | `internal/llm/openai` | 13 | 流式 + 双超时（整体/断流）、`tool_choice` 裸字符串、**请求体额外字段可配（默认值与旧行为一字不差）**、**推理写注入的 writer（默认 stderr，绝不进 stdout）** |
| **提示词** | `internal/prompts` | 3 | 宪法 + 拒答话术 + 注入防护 + **工具参数真实性**（那是权限边界的提示层）；`Compose` 不留尾随空行 |
| **会话历史** | `internal/llm` | 14 | 按组丢弃不拆散 tool_calls、留下的逐字不动、原样读回会话文件 |
| **日志 + 归属绑定** | `internal/config` | 23 | 行首三格前缀、字段按字典序、`键=值` 而不是 JSON、**一条日志一行**、账号从 ctx 自动合并、**默认控制台级别压住 info/debug** |
| **装配 + CLI** | `internal/app`、`cmd/fka` | 44 | 构造函数链替代 cordis.yml；`ask`/`chat`/`tools`/`serve`/`login`/`version`；**参数只解析一次**（认不出的以 2 退出，绝不当问题）；控制台级别只有一处出处；`chat` 真终端上由 readline 接管行编辑（中文回退按宽度擦除） |
| **iLink 协议层** | `internal/channels/ilink/bot` | 97 | uint64 无损、snake_case、`ret` 缺席算成功、**4xx/非 JSON 应答不算成功**、**游标先落盘再上抛（落盘失败要上报）**、过期清游标、ECB 按块、PKCS#7 逐字节核对、**大响应体的超时归读 body 的那一层**、**失败轮次不算成功（退避才会递增）**、**扫码中间态如实上报且不重复**、**收发都有大小上限** |
| **iLink 适配层** | `internal/channels/ilink` | 36 | 归一化、**能力声明与发送器一致**、每次重读账号表（不缓存）、入站先记上下文再上抛、**会话上下文有上限（留最近的 N 条）**、过期不被 offline 覆盖（**且停机不在轮询自己的 goroutine 上等自己**）、启动失败不留「已启动」假状态、`.env` 块就地替换 |
| **消息层** | `internal/messages` | 17 | 入站 → 身份来自消息层（不是模型说了算）→ 工具循环 → **带同一回复令牌**回原会话；**按账号分片、账号间并行**；身份缺失就拒答；**回话带上工具那一步的 ctx**；发不出去只记日志、不发第二条 |
| **技能源** | `internal/tools/skills` | 21 | **无条件注册**（目录空不藏工具）；极简 front matter（不引 YAML）；多目录**后者覆盖前者**；改完不用重启；清单顺序稳定（**前缀缓存要命中**）；空目录时 list 返 `[]` 而 load 说清放哪 |
| **渠道接缝** | `internal/channels` | 16 | 不认识任何渠道实现；**`(种类,账号)` 与跨渠道账号标识双唯一**；广播**先判退订再投递**（单个 select 会随机挑）；可选能力靠**类型断言**而不是 `ok=false`；**契约上每个方法都有活着的调用方**（`contract_test.go`） |
| **网页渠道（SSE）** | `internal/channels/web` | 15 | SSE 收 + POST 发；**token→HttpOnly cookie 认证**（EventSource/`<img>` 带不了头）；会话 id 校验（进 SessionKey/历史文件名）；**实现 EmitterProvider**（流式推 reasoning/tool，**最终答案走 Senders.Text、Answer 置空避免重复**）；出站媒体内存暂存 + `/files/{id}`；静态目录缺失给说明不 500；未配置不产实例、有地址没令牌拒绝启用 |
| **记忆 server（边界 + 工具层）** | `mcp/memory` | 7 | `mcp/memory/**` 不许 import 树外任何包（编译器管不到，由测试守）；是 `main` 包；只认自己那张表；**参数按声明读**（`limit` 是 number 就得用 `GetInt`）；**别人的 private 不进结果** |
| **记忆存储 + 迁移** | `mcp/memory/internal/store` | 17 | **认领老库 v0→v1，一行不动**；认领失败**绝不重建**；共用库时只认自己那张表；**库比代码新要拒绝启动**；权限过滤在 WHERE、关键词之间是「且」、`%`/`_` 要转义 |
| **沙盒 bash server** | `mcp/bash` | 32 | **bwrap 真隔离**：`/` 只读、只有沙盒根可写、独立网络/PID/IPC/UTS/user；**找不到 bwrap 拒绝启动**（不静默降到不隔离）；cwd 词法+符号链接双检；超时杀整进程组；命令首词策略**是护栏不是墙**（`direct` 档只固定 cwd）；**read 工具**与 `@引用` 同语义、经 MCP 内容块把沙盒文件交给模型（文本 text、图片 `type:"image"`、音频 `type:"audio"`、其余二进制元信息，同一套越界检查）；**export 工具**把文件作为 `audience=["user"]` 的嵌入资源返回，agent 只认该字段、经渠道发给用户（字节端到端，远端 SSE 沙盒也成立）；MCP 客户端识别全部内容类型（text/image/audio/resource/resource_link）与 `annotations.audience`，图片随消息发送、其余作文字元信息 |
| **端到端（跨进程）** | `make smoke` 的 `smoke-memory` / `smoke-bash` | — | 记忆：A 进程记一条 → **另起一个进程**查回来 → 换个 viewer 查不到。沙盒：真起 bwrap，断言越界被拒、只读根挡住沙盒外的写 |

> 仍然没有用例的只有 `internal/app`（纯装配，它的正确性由 `make smoke` 的行为验）
> 与 `mcp/memory/internal/{domain,searchterms,log}`（合计 107 行的小工具）。
> `internal/prompts` 那条**真缺口已经补上**（3 个用例，钉住第一道防线的那几节）。

---

## 不变量（改任何东西都不能破）

这些是**踩过才知道值钱**的，逐条有测试守着：

| 不变量 | 在哪 | 破了会怎样 |
|---|---|---|
| 权限过滤在 SQL 的 `WHERE` 里，不在查完再筛 | `mcp/memory/internal/store` 每个查询 | 别人的 private 数据进结果 |
| **权限过滤在真实链路上生效**（记一条 private → 换个 viewer 查不到） | `make smoke` 的 `smoke-memory` | 上面那条只在同进程里被验过；跨进程与跨 viewer 才是这条防线真正面对的形状 |
| **`mcp/memory/**` 不许 import 树外的包** | `mcp/memory/boundary_test.go` | server 与 agent 绑死，不再是「能单独换掉的能力」（**编译器管不到这条**） |
| **认领失败绝不重建** | `mcp/memory/internal/store` | 重建会「修好」错误，代价是数据没了——用户看到安静的库，不是事故 |
| **认领只核对自己的表** | 同上 | 别人的 schema 变动不该让我拒绝启动 |
| **库比代码新（回滚）要拒绝启动** | `mcp/memory/internal/store` | 旧二进制会把 `user_version` 谎报成自己的最新版，以一份不认识的结构读写 |
| **MCP server 启动后 stdout 一个字都不能有** | `mcp/memory/internal/log` + `smoke-memory` | 一个 `fmt.Println` 插进 JSON-RPC 流，把 server 打挂 |
| **CLI 的 stdout 只有结果**（`--json` 从 `{` 开头、`version` 零副作用） | `cmd/fka/loglevel_test.go` + `make smoke` | 日志插在 JSON 前面而退出码仍是 0，调用方只看到「解析失败」 |
| 库代码不往进程 stdout 写字 | `internal/llm/openai`（推理注入 writer） | `fka ask > 答案.txt` 里混着半截推理，而退出码是 0 |
| **媒体收发都有大小上限** | `internal/channels/ilink/bot` | 出站的路径是模型填的、入站的长度是远端定的——没有上限就是让它们决定我们分配多少内存（失败形态是 OOM kill，连日志都没有） |
| **停机信号不是失败**（退出码 0） | `mcp/memory/main.go` | 每一次干净停机都被记成崩溃，systemd 的 Restart 策略跟着走 |
| 未放行的工具**不告诉模型** | `internal/tools/registry` | 模型以为工具「暂时不可用」，换名字再试，白烧一轮 |
| 两种「没有」是不同的话：名字错了 vs 权限门 | 同上 | 部署的人查错方向——以为工具不存在，实际是没打开 |
| 工具调用后的失败**变成一句话喂回**，不抛 | `internal/agent/loop` | 模型看不到失败，就不会换个方式再试 |
| 模型的错**往上抛**，不静默降级 | 同上 | 「接口没配好」被伪装成「模型偶尔不回」 |
| 历史按**整组**丢弃，绝不改写留下的 | `internal/llm/history` | provider 前缀缓存从改动点起全部失效；留下孤儿 `tool` 消息直接 400 |
| 记忆 id 用**生成器**而非时间戳 | `mcp/memory` | id 撞了会**静默丢掉一条记忆** |
| **`internal/channels` 不许 import 任何具体渠道** | `internal/channels/channels_test.go` | 接缝的价值全在「加渠道零改业务层」上，破了是**静默失效**：照常编译、照常跑 |
| **契约上的每个方法都有活着的调用方** | `internal/channels/contract_test.go` | 没人调的方法只能由实现方写桩——「每个新渠道都得实现一遍没人调的方法」是可替换性最大的反作用力 |
| 工具全名 `<源>__<工具>`、`mcp__<server>__<tool>` | `internal/tools/registry` | 模型在会话历史里逐字重放；改名会让那一轮起前缀缓存全失效 |
| `tool_calls` 的 `arguments` **保持原始 JSON 字符串** | `internal/agent/loop` | 反序列化再序列化会改变字节序 |
| 会话历史按**整组**丢弃，**留下的一组都不改写** | `internal/llm/history` | 留下孤儿 `tool` 消息直接 400 |
| **一条日志 = 一行** | `internal/config/logger.go` | 工具参数、推理片段、驱动报错都带换行；不转义的话一条日志摊成两三行，`cut` 出来的级别与阶段全错 |
| 日志归属**绑在 ctx 上**，不靠各处手抄 | `internal/config/scope.go` | 漏一处，那条日志就成了排查时的假线索：看起来完整，实际缺了归属 |

### 已经不在本仓库的不变量

下面几条**仍然成立**，但代码在别的仓库（Node 版 / 搬出去的文档侧）。
**在这里读到它们，是历史记录，不是本仓库的规矩**：

| 不变量 | 现在在哪 |
|---|---|
| 属主 / 会话 / 文件名走同一套字符集校验 | `internal/ids`（已移出） |
| 文件名去掉冒号 | `internal/nas`（已移出） |
| 解析结果保留**原扩展名** | `internal/nas`（已移出） |
| 批注小节用 HTML 注释标记而不是标题 | `internal/nas`（已移出） |

---

## 已知的债（明确不做，但别忘）

| 债 | 严重度 | 什么时候还 |
|---|---|---|
| **真机收发没验过**（扫码验过） | **高** | 需要真微信账号。`make real-check` 会提示步骤，**但那份步骤文档还没写** |
| 语义阈值 0.994 换模型后失效 | **高且静默** | 文档侧上线前。**5 份文档标出来的阈值是过拟合**，先按保守值（压低召回，宁可答不出也不引用错文件） |
| iLink 的 `image` 发送路径未真机验证 | 中 | 渠道层做的时候，需要真机 |
| iLink 的**媒体入站**没有消费方 | 中 | `MediaFetcher` 那套（下载 + 解密）已修好并有用例，但消息层收到媒体会如实拒答——要读图时接它 |
| RRF 融合（`hybrid` 只并排输出两路） | P1 | 文档侧。真实查询攒够再定权重 |
| 记忆只有关键词检索（无向量索引） | P1 | 文档侧。记忆也建向量索引之后 |
| 自动分类 / 家庭提醒 / 自动总结 / 知识图谱 | P2 | 新功能，不是搬迁义务 |

> 「`internal/prompts` 零用例」这条债已经还了（`a92f3cf`，3 个用例钉住第一道防线的
> 那几节），所以从表里删掉。

---

## 剩下的待办

| # | 事项 | 依赖 | 验收标准 |
|---|---|---|---|
| 1 | **真机验证** | 一个已登录的账号 + `docs/real-machine-test.md`（**先写它**） | 扫码 → 收一条 → 回一条 → 发一个文件。**协议层离线测试覆盖不到的东西** |
| 2 | ~~**提示词用例**~~ **已完成**（`a92f3cf`） | — | `internal/prompts` 现在钉住「工具参数真实性」「拒答话术」「注入防护」与 `Compose` 的拼装形状 |
| 3 | **运维端口** | 无 | 状态 / 上下文 / 扫码登录三件事的 CLI 或 HTTP 出口（`Provider.Ops()` 已有，实现还没接）。**`internal/channels/contract_test.go` 的 `pendingConsumers` 里 `Status` 那条记的就是这一项** |
| 4 | ~~**第二个渠道**~~ **已完成**（网页/SSE，`internal/channels/web`） | — | 加一个 `Provider` 就够，**接缝与业务层未改一行**。实现了可选能力 `EmitterProvider`（流式回显），认证走 token→HttpOnly cookie，见 `decisions.md` 第 19 条 |

**做第 1 项之前先写 `docs/real-machine-test.md`**：现在 `make real-check` 打印的那个
文件名背后什么都没有，而「该看到什么」只有真机跑的人知道——**先记下来再跑第二次**。
