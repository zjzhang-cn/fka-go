# 迁移计划与进度

按**自底向上、每层带测试**推进。一层做完能独立跑绿再进下一层。

---

## 当前状态

**Agent 侧（循环 / LLM / 放行 / MCP client / 技能 / 渠道接缝）与记忆 MCP server 都已端到端可用，
两侧之间只有 `mcp.json` 一个接口。**

**记忆 server 自给自足**：表、schema、迁移、日志、默认库文件全归它自己，不与任何人共享一行代码。

**agent 自己不拥有任何数据。** 它的能力只有两条来源：MCP server 与 skill。

已验证的真实链路：

```
fka ask "记一条：2026年3月全家去了三亚"
  → agent 决定调 mcp__memory__remember_memory
  → MCP client 拉起 bin/fka-memory 子进程（stdio）
  → server 写入 SQLite
  → 下一轮 mcp__memory__search_memories 查回来
  → 模型作答
直接查库确认落库；原库未被污染（测试用副本）
```

---

## 模块清单

### ✅ 已完成

| 模块 | 落点 | 用例 | 关键验证 |
|---|---|---|---|
| **工具循环** | `internal/agent` | 11 | 步数上限是硬约束、工具失败转 tool message、收尾兜错、模型的错往上抛 |
| **工具接缝 + 注册表** | `internal/tools` | 11 | 源前缀、effect 放行、参数校验、**两种「没有」是不同的话** |
| **MCP client** | `internal/tools/mcp` | 7 | mark3labs、stdio + HTTP、惰性连接、连不上跳过 |
| **LLM provider** | `internal/llm/openai` | — | 流式 + 双超时（整体/断流）、推理只进 stdout、`tool_choice` 裸字符串 |
| **提示词** | `internal/prompts` | — | 宪法 + 拒答话术 + 注入防护 + **工具参数真实性** |
| **会话历史** | `internal/llm` | 14 | 按组丢弃不拆散 tool_calls、留下的逐字不动、文件空时播种 |
| **装配 + CLI** | `internal/app`、`cmd/fka` | — | 构造函数链替代 cordis.yml；`fka ask` / `fka tools` |
| **技能源** | `internal/tools/skills` | 16 | 极简 front matter（不引 YAML）；多目录**后者覆盖前者**；改完不用重启；清单顺序稳定（**前缀缓存要命中**）；空目录不声明工具 |
| **渠道接缝** | `internal/channels` | 14 | 不认识任何渠道实现；**`(种类,账号)` 与跨渠道账号标识双唯一**；广播**先判退订再投递**（单个 select 会随机挑）；两个 server 自成一体 |
| **MCP 侧边界** | `mcp` | 2 | `mcp/**` 不许 import `fka-go/internal/**`（编译器管不到，由测试守）；两个 server 必须都是 `main` 包 |
| **记忆存储 + 迁移** | `mcp/memory/internal/store` | 10 | **认领老库 v0→v1，一行不动**；认领失败**绝不重建**；共用库时只认自己那张表 |
| **记忆 MCP server** | `mcp/memory` | — | 独立可执行程序，3 步端到端落库 |

### ⬜ 未开始

按依赖顺序：

| # | 模块 | 依赖 | 验收标准 |
|---|---|---|---|
| 1 | **文档转换** | nas + docker | parsers 接缝（按扩展名路由、显式优先 `*` 兜底、重叠拒绝启动）；PyMuPDF / MarkItDown 走 `docker run`（单文件只读挂载、`--network=none`、只看 exit code）；SCNet 异步 OCR；ExifTool 走系统命令 |
| 2 | **切片 + 嵌入 + 向量薄层** | 文档转换 | 切片、SCNet 嵌入（batch ≤5、按 index 重排、429/5xx 退避）、手写向量层（暴力余弦 + 阈值）。**阈值必须重标** |
| 3 | **混合检索** | 上面 | 全文扫 `extracted/*.md` + 向量；关键词之间是「且」；`LIKE` 通配符要转义 |
| 4 | **消息处理** | 上面 | `classify()` 五意图（顺序：文件 > 图片 > 命令 > 提问 > 说明）+ `handleInbound` 穷尽分发 + 批注 + 命令 |
| 5 | **iLink provider** | **渠道接缝（已有）** | 接缝已就位，**provider 还没写**：13 个协议文件；goroutine 代替 worker；真机验证。业务层不 import 具体渠道 |
| 6 | **装配 + 常驻** | 上面 | 接缝进 `internal/app` 与 `serve` 子命令；订阅 → `agent.Run` 的那条链路 |
| 7 | **IPC + CLI 全量** | 上面 | 5+1 个方法、只读可降级到快照、写不可降级、0600 权限 |

---

## 每层的验收标准

不是「能编译」，而是这三条：

1. **`gofmt -l .` 无输出、`go vet ./...` 干净、`go test ./...` 全绿。**
2. **`CGO_ENABLED=0 go build` 成功**（零 CGO 是硬约束，加 C 依赖要有意识地做决定）。
3. **能真机验的就真机验。** 只读命令直接跑；需要模型的用 `fka ask`；需要微信的留到渠道层。

---

## 不变量（改任何东西都不能破）

这些是**踩过才知道值钱**的，逐条有测试守着：

| 不变量 | 在哪 | 破了会怎样 |
|---|---|---|
| 权限过滤在 SQL 的 `WHERE` 里，不在查完再筛 | `mcp/internal/store` 每个查询 | 别人的 private 数据进结果 |
| **`mcp/memory/**` 不许 import 树外的包** | `mcp/memory/boundary_test.go` | server 与 agent 绑死，不再是「能单独换掉的能力」（**编译器管不到这条**） |
| **认领失败绝不重建** | `mcp/memory/internal/store` | 重建会「修好」错误，代价是数据没了——用户看到安静的库，不是事故 |
| **认领只核对自己的表** | 同上 | 别人的 schema 变动不该让我拒绝启动 |
| 未放行的工具**不告诉模型** | `tools/registry` | 模型以为工具「暂时不可用」，换名字再试，白烧一轮 |
| 两种「没有」是不同的话：名字错了 vs 权限门 | `tools/registry` | 部署的人查错方向——以为工具不存在，实际是没打开 |
| 工具调用后的失败**变成一句话喂回**，不抛 | `agent/loop` | 模型看不到失败，就不会换个方式再试 |
| 模型的错**往上抛**，不静默降级 | `agent/loop` | 「接口没配好」被伪装成「模型偶尔不回」 |
| 历史按**整组**丢弃，绝不改写留下的 | `llm/history` | provider 前缀缓存从改动点起全部失效；留下孤儿 `tool` 消息直接 400 |
| 记忆用 `id 生成器`而非时间戳 | `mcp/internal/store`、`mcp/memory` | id 撞了会**静默丢掉一条记忆** |
| 文件名去掉冒号 | `mcp/internal/nas` | 容器挂载错位，症状是「找不到文件」 |
| 解析结果保留**原扩展名** | `mcp/internal/nas` | `房产证.pdf` 与 `房产证.docx` 撞成同一落点 |
| 批注小节用 **HTML 注释标记**而不是标题 | `mcp/internal/nas` | 正文里同名标题被误删 |
| 属主 / 会话 / 文件名走**同一套字符集校验** | `mcp/internal/ids` | 两个不同的东西清洗后别名到同一路径 |
| MCP server 是**独立可执行程序** | `mcp/*` | 共享逻辑会长进 server 里，「独立」名存实亡 |
| MCP server 启动后 **stdout 一个字都不能有** | `mcp/*` | 一个 `fmt.Println` 插进 JSON-RPC 流，把 server 打挂 |

---

## 已知的债（明确不做，但别忘）

| 债 | 严重度 | 什么时候还 |
|---|---|---|
| 语义阈值 0.994 换模型后失效 | **高且静默** | 上线前。**5 份文档标出来的阈值是过拟合**，先按保守值（压低召回，宁可答不出也不引用错文件） |
| README 里「嵌入不联网」那句话 | 高 | 对外表述变更，不是技术问题 |
| iLink 的 `image` 发送路径未真机验证 | 中 | 渠道层做的时候，需要真机 |
| RRF 融合（`hybrid` 只并排输出两路） | P1 | 真实查询攒够再定权重 |
| 记忆只有关键词检索（无向量索引） | P1 | 记忆也建向量索引之后 |
| 自动分类 / 家庭提醒 / 自动总结 / 知识图谱 | P2 | 新功能，不是搬迁义务 |

---

## 下一步

做**文档转换**（清单第 1 项）：parsers 接缝（按扩展名路由、显式扩展名优先 `*` 兜底、
重叠即拒绝启动）+ PyMuPDF / MarkItDown 走 `docker run`（单文件只读挂载、
`--network=none`、只看 exit code）+ SCNet 异步 OCR + ExifTool 走系统命令。

它是「文档 MCP server 的归档侧」的必要前置——归档流水线是「去重 → 落盘 → 转换 →
入库 → 索引」，而**转换**在其中占最重的一块。
