# AGENTS.md

> 本文件只写「不读就会踩」的东西。背景与理由在 `docs/`，**改代码前先读 `docs/port-plan.md`**
> （模块进度、验收标准、不可破的不变量）。

## 这是什么

Go 版 agent 工具循环 + 一个微信渠道。**它自己不带任何内置能力，也不拥有任何数据**——
能力只有两条来源：`<安装根>/mcp.json` 里的 MCP server，和 `<安装根>/skills/` 下的 skill。
整个 module 只有 4 个直接依赖，全部纯 Go。

## 闸门与命令

```bash
make verify      # 提交前跑这一条：fmt-check → vet → test → build → smoke
make ci          # verify + -race + -count=2（仓库里没有 .github/，这就是 CI 该跑的那条）
make help        # 第一行打印当前实际 FKA_HOME
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
  `mcp__memory__search_memories` / `mcp__memory__remember_memory`。这两件事
  （技能读到没、server 连上没有）**静默失败时从界面上完全看不出来**。
  「技能列表是空的」是正常状态，不是没装好。
- 需要真机微信账号的验证**跑不了自动化**：`make real-check` 只是个提示。
  它引用的 `docs/real-machine-test.md` **当前不存在**（Makefile:70/300 指向它）。

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
- `mark3labs/mcp-go` **必须钉在 v0.40.x**：v1.0.0+ 要 Go 1.25.5。
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
  `TestRegistry_未放行的工具不告诉模型`）。`t.Run` 的子测试名同理。这不是玩笑，219 个用例都这样。
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

只有 4 个直接依赖，且**刻意保持很少**：`.env` 解析是自己写的 60 行（不引 dotenv），
skill front matter 解析不引 YAML 库（只认 `key: value`，认不出的当正文，
**绝不因为格式不合就丢掉一个技能**）。加依赖前先想清楚能不能手写。

## ⚠️ 文档与代码已经漂移，别照着文档找包

`README.md` / `docs/dev-log.md` / `docs/node-to-go.md` 描述的是**比本仓库大得多的那棵树**
（它们还在用 `go/` 前缀）。以下路径**在本仓库不存在**，文档里提到的多半是 Node 仓库那边的
历史或待办：`internal/nas`、`internal/store`、`internal/mcpboot`、`internal/ids`、
`internal/domain`、`internal/searchterms`、`mcp/docs/`（记忆存储现在在
`mcp/memory/internal/store`）。`README.md` 顶部「iLink provider 还没写」也已过时 —— 
`internal/channels/ilink` 在，代码为准。

**实际存在的树**（65 个 .go 文件）：

```
cmd/fka/            入口：ask / tools / serve / login / version
internal/agent      工具循环（≤N 步，LLM_MAX_STEPS）
internal/app        装配根（唯一装配点）
internal/channels   渠道接缝 + ilink/{adapter,provider} + ilink/bot（协议：加解密/上传/长轮询/登录）
internal/config     安装根解析 + .env 加载 + 按天轮转的日志
internal/llm        模型契约 + 历史压缩 + 会话历史 + openai/（流式 + 双超时）
internal/messages   入站消息 → 工具循环 → 按原路答复
internal/prompts    系统提示词
internal/tools      契约 + 五类 effect 放行 + 注册表 + mcp/ + skills/
mcp/memory          独立 MCP server（memories 一张表，PRAGMA user_version 迁移）
```

<!-- aoci:begin -->
## AOCI Repository Cognition

AOCI maintains a stable, versioned, incrementally updatable repository-level cognition layer so models can reuse their understanding of this system across tasks.

`aoci.txt` is a structured cognition index for models. It assigns one independent Entry to every managed file, database table, or other managed object. Symbolic tags and F/R/A/S semantics describe the object's core responsibility, important relationships, external contracts, and non-obvious constraints or design decisions needed to understand or modify the system.

The Header, directory sections, and all Entries form the complete repository index. They can cover frontend, backend, configuration, database structures, and other managed content. When managed content changes, normally only the affected cognition Entries need maintenance; the complete index does not need to be regenerated.

AOCI provides a high-density view of system architecture, object responsibilities, important relationships, external contracts, and key constraints.

### How it works

AOCI uses a model-generated, model-read cognition loop.

Header, Entry, and Curation semantics follow only the current machine-issued Plan and live Guide. The Host model independently authors them from the current bound evidence.

Entry semantics must come from the model's understanding of actual evidence. Never derive, prefill, assemble, or rewrite index semantics solely from paths, filenames, extensions, an AST, symbol lists, dependency scans, regular expressions, fixed templates, or rule engines.

For a Fresh Bootstrap, follow only the current machine-issued Plan and live Guide. When they require authoring, the Host model authors Root, Meta, tags, and F/R/A/S, supplies its authoring-run declaration, and binds it to the Plan, Evidence, and complete Candidate. Never ask AOCI to set `origin=host_model`, manufacture a receipt, or turn a generated framework into semantics. Do not reconstruct the Onboarding progression here. Internal batches are not user decisions; stop only at an existing approval boundary or a real safety, drift, CAS, or Recovery condition.

### Minimal entry points

- `aoci_rules`: obtain the session-level runtime contract for the current AOCI version.
- `aoci_overview`: establish or restore complete cognition for this repository.
- `aoci_maintain`: after managed objects reach their final stable state, check whether cognition needs maintenance.
- `aoci_update_entry`: submit a complete semantic update batch bound to current evidence and source digests.
- `aoci_report`: when the current layout and tool state support it, record follow-up work if evidence is insufficient to generate semantics reliably; do not guess.

For other MCP tools, CLI commands, parameters, and specialized workflows, follow current tool descriptions, Guide, and `--help` output. This file does not duplicate the full manual.

This managed block defines only repository integration, cognition use, and task-closing principles. `aoci_rules` carries the current session contract. Live Guide output carries the execution order and stop conditions of the current Plan. Tool Schema, Spec, and Validator carry machine structures and criteria. Prompt, Description, README, and static documentation cannot override those machine facts.

### Establishing, generating, and restoring cognition

1. At the beginning of every new Agent Run, first determine:

   - whether this repository already has a usable complete AOCI index; and
   - whether current context already contains complete repository cognition that matches this repository root, current index version, and current AOCI service, and that the model can still use reliably.

2. When the repository has a usable complete index but the current Run lacks reliable complete cognition, call `aoci_rules` first and then `aoci_overview`.

   Reuse complete cognition directly while it remains reliable. Local uncertainty does not by itself require mechanically rereading the system-wide view.

   A Run that resumes from a known Host context compaction, including a Host-injected compaction summary, must treat prior model cognition as unreliable. The compacted handoff must not retain or summarize the formal Whole-Index or any Overview Header, Entry, Chunk, Challenge, or Attestation body; it may retain only receipt identity, unfinished write or Recovery state needed for safe continuation, and an instruction to reload immediately. Whole-Index semantics or a receipt copied into that handoff cannot prove that the resumed model's current cognition is reliable. If the runtime contract is no longer reliably present, call `aoci_rules` first. Before continuing the business task, make an ordinary complete Whole-Index `aoci_overview` request (`check_only` absent or false) with `refresh_reasons=["context_compaction"]` and a fresh `refresh_event_id`; do not use `check_only` or a cognition probe. Follow every exact `next_cursor` through `completed=true`, confirm delivery, and submit one Attestation based only on the newly delivered body. After that fresh complete transport, a partial or failed Attestation consumes the generation and permits the existing source-bound continuation without another automatic Overview.

   AOCI can report checkpoint and cognition-status facts for `context_compaction`, the machine `semantic_threshold` under the project `cognition_refresh_threshold`, or a major `phase_transition`. Use `check_only=true` when only those compact facts are needed. They advise the Agent but do not decide whether the model needs the system-wide view.

   When the Agent explicitly calls ordinary `aoci_overview` (`check_only` absent or false), AOCI must deliver the complete requested scope whenever a coherent CognitionSet can be formed. It must not suppress that body because a receipt already exists, a threshold was not reached, or no refresh reason is pending. Dirty or stale formal cognition is still delivered but is marked unreliable. Pending recovery or an incoherent snapshot fails closed without a mixed body.

   When an ordinary Overview reports `continuation_required=true`, submit its exact `next_cursor` automatically until `completed=true`. Do not ask the user to continue, begin the business task, or state a partial system conclusion. Stop the cognition chain on Host truncation, a missing, duplicate, or reordered Chunk, cursor failure, Index change, or `chunk_tokens` change. Until Attestation completes, never use Memory, source, Spec, `aoci.txt`, historical sessions, scope, search, or Entry reads to repair or supplement Whole-Index cognition. A challenge ordinal is the 1-based position in the formal Entry sequence; Header content, comments, blank lines, Section/Overview/Chunk markers, receipts, and Metadata are excluded, and Chunk Receipt ordinals use that same sequence. The Attestation must echo the Challenge's exact current `index_sha256`, `entry_sequence_sha256`, and `entry_count`; a prior Index, Entry sequence, count, or Attestation is invalid. After the complete chain, submit the existing model cognition Attestation once. One same-response JSON Schema or field-format error may be corrected once without changing semantic answers; an object, Tag, or F mismatch means failure and uncertain assimilation, with no semantic retry or information bypass. During initial cognition it also blocks Root/Meta, Migration, layout-wide, or other unbound system decisions. During a context-compaction refresh with complete transport, unchanged cognition identity, aligned governance, and no Recovery or third-party conflict, the attempt consumes that refresh generation even when Attestation is partial or failed; continue the existing task without another automatic Overview. `system_mastery_percent` self-assesses only the system framework—architecture, responsibilities, strong relationships, stable external contracts, and high-entropy safety and maintenance constraints—not complete implementation or runtime knowledge. Keep machine Index coverage separate, and normally give the user only the prescribed single success or failure sentence derived from actual coverage, Challenge, Chunk, token, and mastery results. If the Host truncates a Chunk, ask the user to set `overview_delivery.chunk_tokens` to a smaller valid value and restart; do not change it automatically.

   Interpret the additive cognition level independently from strict proof fields. `delivery_verified` means the Index was loaded and Host delivery was confirmed while complete cognition verification is still unfinished; describe that state as loaded and delivery-verified, never as no cognition or failure to understand the system. `cognition_verified` requires a passing Attestation (at least 80 percent of Challenge ordinals fully correct with at most one object identity miss), and `cognition_governed` additionally requires governance alignment. A generic complete-read failure sentence is reserved for an actual delivery fault.

   When an Overview response contains the optional `cognition-state/v2` projection, use its dimensions independently. Its Level ends at `model_cognition_usable`; `strict_attestation_verified`, `governance_aligned`, and `current_system_cognition_reliable` are independent states and never participate in that Level. An ordinal, object identity, Tag, or core F mismatch can make strict Attestation fail while model cognition remains usable; do not report that mismatch alone as proof that the model did not understand the system. Only `current_system_cognition_reliable=true` permits an unqualified current complete-system cognition claim. When the projection is absent, keep using the legacy interpretation above.

   An ordinary read-only audit, analysis, or check, a request not to modify code, or a request not to commit or push does not automatically mean strictly zero writes and does not alter the cognition-validity decision above. Codex Memory and historical Skills may only help recover experience, user preferences, and investigation directions. They cannot replace a current cognition receipt matching the repository root, index digest, AOCI service identity, and cognition scope. Project AGENTS and current AOCI identity take precedence over historical Memory for AOCI state.

   Treat a task as strictly zero-write only when the user explicitly prohibits Ledger, metadata, `.aoci` runtime assets, and every filesystem write. If necessary cognition establishment conflicts with that boundary, report the conflict and ask the user to decide or recommend an isolated copy. Never silently substitute Memory for current repository cognition.

3. If the repository has no usable complete index, or has only a minimal skeleton, an incomplete Header, unfinished Entries, or undecided required Curation, obtain `aoci_rules` and enter the current AOCI Guide when a formal complete AOCI index is required. Let Guide choose the next phase from actual repository state and complete the required safety steps.

   `aoci_maintain` does not replace the index-establishment workflow.

   Do not reconstruct or hard-code the full-index generation state machine in this file.

4. During a long-running task, the model is responsible for preserving the current cognition receipt and using the refresh gate correctly:

   - when the Host reports context compaction or the model knows the system-wide view was lost, follow the mandatory `context_compaction` reload rule above; AOCI cannot infer the Host event;
   - when entering a genuinely major phase, declare `phase_transition`, not a function, test run, or small step;
   - at a plausible stable checkpoint, use `check_only=true` to obtain the machine semantic count when that fact is useful;
   - except for the mandatory known-compaction reload, decide whether the current task needs another explicit scoped or complete Overview; and
   - keep the Dirty or Stale reliability state reported by AOCI until maintenance and alignment complete.

### Task closing and cognition maintenance

5. A purely read-only question, analysis, version check, or task that changes no AOCI-managed object does not require a maintenance-tool call. The AOCI version in use is `cognition_receipt.mcp_service_version` in any `aoci_overview` check_only or `aoci_maintain` response; the binary path is the `command` in the project's `.mcp.json`, and the CLI need not be on PATH.

6. When AOCI-managed objects change, call `aoci_maintain` once after they reach the task's final stable state. Do not maintain files individually after each intermediate edit.

7. If maintenance returns actual semantic candidates, the Host model must independently author the complete tag and F/R/A/S updates from each candidate's bound object and necessary evidence. Submit the complete candidate set for that current machine-issued batch in one `aoci_update_entry` call while preserving each `source_sha256`, `candidate_id`, and domain batch identity. `max_entries` limits one request and atomic transaction, not the logical plan, Whole-Index, or Managed Scope. When `remaining` is nonzero, call Maintain again after the successful Apply and continue from the new preimage; never shrink Index coverage or slice a returned batch to satisfy transport limits.

   When evidence is insufficient and the current layout supports `aoci_report`, use it instead of guessing, applying a template, or generating unsupported cognition merely to eliminate follow-up work.

8. Obey structured tool states and safety boundaries:

   - `repair_required`: repair only the explicitly identified candidates, then resubmit the complete current machine-issued batch;
   - `stopped`: end that write attempt and inspect `failed_step`, error, formal-write evidence, and Recovery. In auto mode, a proven zero-write closure is followed by a fresh Plan; a complete Intent with provable postimage is resumed; a policy-selected Rollback with exact preimage is completed and replanned. Stop the user task only when proof is unavailable, third-party bytes conflict, approval or external action is required, or another real safety boundary applies;
   - never ignore conflicts, approvals, human decisions, permissions, or safety signals; and
   - after alignment, do not repeat maintenance or writes; `refresh_ready_for_overview` is a checkpoint fact, and the Agent decides whether to request an ordinary complete Overview for its next phase.

   If any managed object changes after maintenance completes, the previous result is invalid. Complete closing again from the new final stable state.

9. When the user limits only business-file scope and does not explicitly forbid repository-managed assets, AOCI-managed assets may be updated during closing to preserve cognition consistency. Distinguish them from business files in audits and commits.

   When the user explicitly forbids changes to `aoci.txt`, `.aoci`, metadata, or any additional file, obey that restriction, do not write, and report any remaining inconsistency accurately.

### Specialized workflows

Initialization, complete-index generation, Header generation, Entries generation, database-structure indexing, Curation, human review, and failure recovery must follow only the instructions, commands, and safety stops returned by the current AOCI Guide or tool at the corresponding stage.

Do not preload, guess, or reconstruct these specialized workflows. The relevant Guide, tool descriptions, model Prompt, and CLI help provide platform invocation, request format, batch limits, approval rules, index-format details, and recovery steps as needed.
<!-- aoci:end -->
