# 渠道接入协议（网络层统一渠道接口）——设计与后续开发计划

> **北星：agent 定义一套统一的「渠道接入协议」，任何渠道只要写一个适配器（微信、短信、……）
> 作为客户端连进来，agent 就在协议之上把它统一成内部那条 `channels.Channel` 接缝。**
>
> 一句话：**接缝是内部契约，协议是它的网络形态，适配器是「渠道驱动」。**
>
> 这是一份**待开发**的设计书，不是现状。现状见 `port-plan.md`；本页描述目标形态、
> 线协议、包布局、迁移阶段与不变量。**动手前先读本页与 `port-plan.md` 的「不变量」。**

---

## 1. 为什么做（动机）

现有两个渠道（iLink、web）都是**进程内**的 Go 值：`seam.startOne` 把一个 Go 闭包交给
`Channel.Start`，入站经 `broadcast` 扇出到有界 channel——**没有任何序列化/线边界**
（`internal/channels/seam.go`）。这带来的问题：

- 渠道与 agent 同生共死，一个渠道的崩溃/资源占用会拖累 agent；
- 渠道无法分机部署（微信桥想跑在另一台机器/群晖）；
- 每加一个渠道（短信、Telegram…）都要重新编译整个 agent。

目标是把「渠道」从「进程内的一个接口实现」变成「**能通过网络拨入的适配器**」，
让 agent 只需要维护**一套协议**，而不是为每个渠道改代码或重新发布。

> 直接动机来自把 **iLink 桥整个提取出去**（登录、账号表、长轮询、游标、CDN、bot 协议
> 全在远端），使 iLink 成为这套协议的**第一个适配器**；短信是**第二个**，用来验证协议的通用性。

---

## 2. 设计决策（已拍板）

| # | 决策 | 取值 | 要点 |
|---|---|---|---|
| D1 | 方向 | **agent 当 server，适配器拨入** | 适配器可在 NAT 后；agent 暴露一组端点 |
| D2 | 机制 | **复用 web 协议形态并扩展** | 复用线形态与鉴权管道，**不复用 `internal/channels/web` 这个包** |
| D3 | 范围 | **渠道细节全在适配器侧** | 登录、账号表、长轮询、游标、CDN、协议解析都搬走 |
| D4 | 接口形态 | **协议镜像接缝，版本化** | `InboundMessage` + `Senders/SendTarget` + `Capabilities` + `Status` 的线表示 |
| D5 | 注册 | **动态注册 + 白名单** | 适配器拨入时上报账号与能力；agent 校验唯一性与能力 |
| D6 | 身份 | **适配器受信声明 `PrincipalID`** | 命名空间由适配器保证；**浏览器路径仍「身份不由请求体决定」** |
| D7 | 媒体 | **出站 pull 复用 `/files/{id}`；入站暂不定** | 入站媒体目前业务层不消费 |
| D8 | 登录 | **纯适配器本地，agent 不参与** | `Provider.Ops().Login` 不再进 agent |
| D9 | 传输实现 | **抽出通用 HTTP/SSE/token 传输** | web 与网络渠道共用，web 行为不变 |
| D10 | 进程内渠道 | **保留**；新增 `netchannel` 对接远程适配器 | `channels.Channel` 是内部契约，谁实现都行 |
| D11 | 断线 | **保留注册、状态置 offline** | `expired` 由适配器显式上报，不能被 offline 覆盖 |
| D12 | 投递 | **背压**：收不下回 `503`，适配器不推进游标、退避重试 | 延续 iLink「宁可重放，不要漏」 |
| D13 | 版本 | 握手带 `protocolVersion`，主号不匹配拒接并说清 | 连接时唯一能说清「为何连不上」的机会 |
| D14 | 连接 | **每适配器一条 SSE**，事件带 `kind/account` | 连接数 = 适配器数 |
| D15 | `kind` | **agent 白名单签发**，适配器不得自创 | `kind` 进会话键与历史文件名，改了等于换历史 |

---

## 3. 线协议 v1（FKA Channel Access Protocol）

传输 **HTTP/1.1 + SSE**。前缀 `/channels`。认证 `Authorization: Bearer <适配器 token>`。
TLS 建议由反向代理终结（是否强制留作部署决策）。

### 端点

| 端点 | 方向 | 用途 |
|---|---|---|
| `POST /channels/register` | 适配器 → agent | 握手：协议版本、适配器身份、账号与能力清单 |
| `GET /channels/events?session=<id>` | agent → 适配器（SSE） | 出站 `send`、`ready`、心跳 `: ping` |
| `POST /channels/inbound` | 适配器 → agent | 推一条入站消息；`202` = 已接收 |
| `POST /channels/status` | 适配器 → agent | `online`/`offline`/`expired` 状态变更 |
| `GET /files/{id}` | 适配器 → agent | 取本轮出站文件/图片字节（复用 web 的 blob 存储） |

### 注册

```json
{
  "protocolVersion": "1.0",
  "adapter": { "name": "ilink-adapter", "version": "0.1.0" },
  "channels": [
    {
      "kind": "ilink",
      "account": "account_001",
      "capabilities": {
        "text":  { "send": true,  "receive": true },
        "file":  { "send": true,  "receive": true },
        "image": { "send": true,  "receive": true },
        "voice": { "send": false, "receive": true },
        "video": { "send": false, "receive": true },
        "proactivePush": false
      }
    }
  ]
}
```

应答：`{"session":"<id>","accepted":[{"kind":"ilink","account":"account_001"}],
"rejected":[{"kind":"sms","account":"x","reason":"kind 未登记"}]}`。

- 主版本不匹配 → `400`，文案说清（对应本仓「只报错不指路等于没帮上忙」）。
- `kind` 不在白名单 → 该渠道 `rejected`，其余照常接入（一个渠道配错不牵连别的）。
- 账号唯一性：`(kind, account)` 冲突 → 拒绝后到者（对应 `seam.go:225-241`）。

### 入站

```json
{
  "kind": "ilink",
  "account": "account_001",
  "messageId": "…",
  "principalId": "<wxid>",
  "senderId": "<渠道内稳定 id>",
  "senderName": "…",
  "conversationId": "<wxid>",
  "replyToken": "<context_token>",
  "timestamp": 0,
  "parts": [
    { "type": "text", "text": "…" },
    { "type": "image", "media": { "handle": "<适配器自有句柄>" } }
  ],
  "quoted": null
}
```

- **`principalId` 是受信字段**：适配器按 `types.go:197-205` 的约定产生（不透明、全局唯一、
  带命名空间如 `ilink:<wxid>`）。**这是信任边界**：适配器进程受信，模型碰不到它。
- 应答：`202 {"ok":true}` / `503 {"error":"…"}`（背压）/ `401`/`403`。
- **`202` 是「已入队」的真承诺**：适配器收到 `202` 后才推进自己的游标；非 2xx 则退避重试。

### 出站（SSE 帧）

| event | payload | 说明 |
|---|---|---|
| `ready` | `{"session":…}` | 连接就绪 |
| `send` | `{"kind","account","type":"text\|file\|image","text"?,"conversationId","replyToken"?,"media":{"id","name","mimeType"}?}` | 要适配器发出的回复；文件/图片先 `GET /files/{id}` 取字节 |

- `replyToken` 缺省时表示主动推送（仅当 `proactivePush=true`）。
- 复用 web 的心跳（每 ~20s 注释帧）；SSE 读结束 = 断线（D11）。

### 状态

```json
{ "kind": "ilink", "account": "account_001", "status": "online|offline|expired" }
```

`expired` 与 `offline` 必须可分（微信 `expired` 要重扫码，`offline` 重启即可）——
沿用 iLink 现在刻意不把 `expired` 覆盖成 `offline` 的语义。

---

## 4. agent 侧改造

1. **抽出通用传输** `internal/channels/httpx`（名待定，见 §8）
   从 `internal/channels/web` 提出：
   - SSE `hub`（房间隔离、有界队列、非阻塞广播、心跳）；
   - `blobStore` + `GET /files/{id}`（内存暂存、TTL、上限、CSP）；
   - `requireAuth` / 常量时间 token 比对 / cookie 或 Bearer。
   `web` 改为复用它，**满足现有 web 测试**，行为不变。

2. **新增网络渠道** `internal/channels/remote`
   - 一个 `Provider`，跑适配器 HTTP server，维护「已连接适配器 → 渠道」注册表；
   - 为每个上报的 `(kind, account)` 造一个 `Channel`：`Senders` 经该适配器 SSE 推 `send`，
     `Status` 来自 `POST /channels/status`，`Start/Stop` 只管连接生命周期；
   - 出站媒体先落 `httpx` 的 blob，再把 `/files/{id}` 给适配器。

3. **接缝的最小扩展（唯一动接缝处）**
   现在 `Provider.Create(ctx)` 在**启动时同步**产出渠道，而适配器是**启动后连入**的。
   新增一个可选接口，让 app 把 `*channels.Service` 注入愿意动态登记的 provider：

   ```go
   // Registrar 让 provider 在运行期登记/注销渠道
   type Registrar interface { Attach(Channel) error }

   // DynamicProvider 由「运行期才有渠道」的 provider 实现
   type DynamicProvider interface { Bind(Registrar) }
   ```

   app 在 `registerChannels` 时 `if dyn, ok := provider.(channels.DynamicProvider); ok { dyn.Bind(a.Channels) }`。
   适配器连上后经 `Attach` 动态登记；`Service.Attach` 在服务已启动时**自启**该渠道（与 `Register` 对称）。
   **必须**：登记前按 `kind:account` 排序、冲突拒绝——守住稳定序与双唯一。

4. **配置**
   - 统一监听地址：提议 `CHANNEL_ADDR`（`WEB_CHANNEL_ADDR` 作兼容别名）。
   - 适配器 token 与 `kind` 白名单：提议 `CHANNEL_ADAPTERS` / `CHANNEL_KINDS`。
   - **分片键不变**：`kind` 当 `ChannelID`、适配器账号当 `AccountID`、发送者当 `PrincipalID`
     （见 `internal/messages/dispatch.go` 的 `shardKeyOf`）。跨适配器天然隔离，同「账号+身份」仍串行。

---

## 5. 适配器侧

### 共享协议包

`channelproto/`（放**非 internal**，因为适配器可能独立部署/另仓）：只定义线类型与版本常量，**零依赖**，
agent 与所有适配器共用。**这是本仓「自给自足」原则下的一次有意识例外**，需在 `AGENTS.md`/`decisions.md` 记明。

### iLink 适配器（第一个）

`adapters/ilink/`（新顶层目录，`package main`，仿 `mcp/` 的自足模式）：

- 把 `internal/channels/ilink/bot/**` **搬出** `internal` 到适配器
  （扫码登录、账号表 `ILINK_ACCOUNT_<N>_*`、长轮询、游标 `data/cursors.json`、CDN、bot 协议）；
- 登录变**适配器本地子命令**（二维码在适配器侧打印），凭证由适配器自持；
  `cmd/fka/login.go` 的 iLink 路径移除（web 登录仍在 agent）；
- 存量测试随代码一起搬（`internal/channels/ilink/bot/bot_test.go` 等）；
- **边界测试**：`adapters/ilink/**` 不许 import 本仓 `internal/...`（仿 `mcp/*/boundary_test.go`）。

### SMS 适配器（第二个）

本轮只留协议说明 + 空骨架目录，用来验证「协议对不同渠道是否够用」；不实现。

---

## 6. 迁移阶段（每段可编译、相关测试过、**分段提交**）

| 阶段 | 内容 | 验收标准 |
|---|---|---|
| **P0 抽传输** | `httpx` 抽出，`web` 复用 | `internal/channels/web` 全部测试绿；`fka` 行为不变 |
| **P1 协议 + agent 侧** | `channelproto/`、`remote` server、接缝 `DynamicProvider` | 用**假适配器**跑端到端：注册 → 入站 → 出站 → 状态 → 背压(503) → 版本拒绝 → kind 白名单 |
| **P2 iLink 适配器** | 搬 `bot`、独立进程跑通、`ILINK_ACCOUNT_*` 兼容 | 双进程端到端：扫码（适配器侧）→ 收一条 → 回一条 → 发一个文件；iLink 从进程内渠道切到远程 |
| **P3 收尾** | 退役 `internal/channels/ilink` 渠道实现；docs；边界与冒烟 | `make verify` 绿；`decisions.md` 新增条目；`port-plan.md` 更新 |

> **P2→P3 之间的双轨**：是否短暂保留进程内 iLink（可回退）由 §8 决定。
> 若保留，用一个开关（如 `ILINK_MODE=local|remote`）过渡，P3 删本地。

---

## 7. 不变量与风险

- **`internal` 规则**：协议包必须在非 `internal`；适配器自足（边界测试守）。
- **身份信任边界**：适配器受信声明 `PrincipalID`；**浏览器路径必须继续「身份不由请求体决定」**——
  两类客户端在协议里可区分（浏览器走 `/messages`+token→identity，适配器走 `/channels/inbound`）。
- **会话键/历史文件名**依赖 `kind`+`account`+`conversation`（`channels.SessionKeyOf`），
  所以 `kind` 必须白名单且稳定。
- **投递语义**：`202` = 已入队；`503` = 背压，适配器保留游标重试。这条把「收了但可能已丢」变成真承诺。
- **动态注册**是唯一需要动接缝的地方，风险集中：排序确定性、唯一性、服务已启动时的启动时机、
  断线时**不注销**（D11）。
- **连接生命周期**：SSE 断 = offline；重连恢复；`expired` 只能显式上报。
- 已知环境坑：本机 `make smoke` 因仓库根 `.env` 有渠道而卡在 `serve`（与本计划无关）。

---

## 8. 待拍板（命名/取舍，其余按本文）

| # | 待定 | 建议 |
|---|---|---|
| 1 | 协议包名 | `channelproto/`（备选 `wire/`、`channelwire/`） |
| 2 | 网络渠道包 / 适配器目录 | `internal/channels/remote` + `adapters/ilink/` |
| 3 | 统一监听地址 | `CHANNEL_ADDR`，兼容旧 `WEB_CHANNEL_ADDR` |
| 4 | P2→P3 是否双轨 | 建议双轨（`ILINK_MODE=local\|remote`），P3 删本地 |

---

## 9. 与现有代码的映射（动手时按这张表找）

| 现状 | 去向 |
|---|---|
| `internal/channels/web/{hub,store,server 的鉴权与 /files}` | 抽到 `internal/channels/httpx` |
| `internal/channels/web/*` 其余（浏览器语义） | 留在 `web`，复用 `httpx` |
| `internal/channels/ilink/provider.go`、`adapter.go` | 逻辑迁入 `adapters/ilink`（成为协议客户端） |
| `internal/channels/ilink/bot/**` | **搬出** `internal` 到 `adapters/ilink/bot/**` |
| `cmd/fka/login.go` 的 iLink 路径 | 移入适配器子命令；`cmd/fka/login.go` 只保留 web |
| `internal/channels/{types,seam}.go` | 新增 `DynamicProvider`/`Registrar`；`Attach` 对称自启 |
| `internal/app/app.go:registerChannels` | 命中 `DynamicProvider` 时 `Bind(a.Channels)` |
| `internal/messages/dispatch.go:shardKeyOf` | **不改**（分片键已含身份） |
| `cmd/fka/serve.go:channelProviders` | 加 `remote.NewProvider()`；iLink provider 移除 |
