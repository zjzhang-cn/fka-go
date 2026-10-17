# 网页渠道协议（浏览器 ↔ agent，SSE + POST）

> 这一页记的是 `internal/channels/web` 与 `web/` 前端之间**实际在跑的线协议**：端点、认证、
> SSE 事件、会话键、媒体与 `#引用`。
>
> 它是**现状**，不是设计书。面向「适配器经 HTTP/SSE 拨入」的未来设计在
> `docs/channel-access-protocol.md`——那份 D2 刻意**复用这里的线形态与鉴权管道**，但**不复用
> `internal/channels/web` 这个包**。读那份之前先读这一页，线形态的出处在这里。
>
> 一句话：**浏览器经 SSE 收、经 POST 发；一个渠道实例——单账号、多用户。**
> 接缝与业务层一行未改（`channels_test.go` 守着这条），本渠道只实现 `EmitterProvider` /
> `MediaFetcher` / `TurnControllable` 三个可选能力。

---

## 1. 为什么是这套（三个决定）

- **SSE 收 + POST 发，不用 WebSocket**：SSE 只服务端→客户端，客户端→服务端另给一个普通
  `POST`。两件事各自是普通 HTTP，可以各自鉴权、各自限流，也不需要在 agent 里维护一条有状态
  的双工连接。代价是「停止」这类反方向动作要单开一个端点（`POST /stop`）。
- **token → HttpOnly cookie 认证，不用 `Authorization` 头**：`EventSource` 带不了自定义头，
  `<img>` / `<video>` / `<a>` 也带不了——而这三处（SSE、媒体、POST）都要认证。只有 cookie
  能让三处统一，且令牌不进 URL、不进日志、JS 读不到。非浏览器客户端仍可用 `Bearer` 头。
- **最终答案不走 SSE 的 `Emitter`**：工具循环同时会调 `Emitter.Answer` **和**
  `Senders.Text`，两者是同一份文本。若两边都推一帧，浏览器会看到答案两次。所以答案**只走
  `Senders.Text`**（推 `event: message`），`Emitter.Answer` 是空实现（见 `emitter.go`）。

---

## 2. 端点

方法+路径模式（Go 1.22+ `ServeMux`，非法方法自动 `405`）。**静态页不鉴权**（页面本身不含
机密，登录框就在页面里）；接口一律鉴权。错误响应统一是 `{"error":"…"}`。

| 端点 | 认证 | 用途 |
|---|---|---|
| `GET /` | 否 | 静态页（`WEB_CHANNEL_STATIC`，默认 `<安装根>/web`） |
| `POST /login` | 否 | 用 token 换一个 HttpOnly cookie |
| `GET /events?conversation=<id>` | 是 | SSE 长连接，按会话订阅 |
| `POST /messages` | 是 | 发一条消息（JSON 纯文字 / multipart 带文件） |
| `POST /stop` | 是 | 停掉该会话进行中的一轮 |
| `GET /resources?conversation=<id>` | 是 | 列该会话可 `#引用` 的媒体 |
| `GET /files/{id}` | 是 | 下载一份出站媒体 |

### 认证

两种携带方式（`principalFor`）：`Authorization: Bearer <token>`（非浏览器客户端）或
cookie `fka_web_token`（浏览器）。token 反查 `PrincipalID` 时**遍历整张表做常量时间比较**
（避免「命中即返回」泄露前缀）。未认证统一 `401 {"error":"未认证"}`。

```
POST /login
Content-Type: application/json
{ "token": "<用户令牌>" }

→ 200 { "ok": true }   +  Set-Cookie: fka_web_token=<token>; Path=/; HttpOnly; SameSite=Strict
→ 401 { "error": "令牌不对" }
→ 400 { "error": "请求体不是合法 JSON" }   （上限 4 KiB）
```

cookie 里放的就是 token 本身，服务端每次再反查 principal。**不加 `Secure`**——TLS 由反向
代理终结，是部署决策。

### `POST /messages`（入站）

按 `Content-Type` 分两条：

```
# JSON：纯文字（上限 1 MiB）
{ "conversation": "room1", "text": "帮我看下这张图" }

# multipart/form-data（整体上限 16 MiB）：字段 conversation / text + 若干文件字段
#   文件字段名不限（前端用 `file`），服务端按字段名排序，保证 part 顺序稳定
```

应答：`202 {"ok":true}` = **已入队**。其余：`400`（会话 id 不合法 / 正文与文件都为空 /
JSON 或 multipart 解析失败 / 单件超 8 MiB / 空文件）、`507 {"error":"文件暂存超限，请少发几个"}`
（入站暂存放不下）、`503 {"error":"渠道还没开始接收"}`（渠道未 `Start`）。

**接受任意文件、不限图片**：每个文件落进独立的 `inbound` 暂存，按内容嗅探的 MIME 定 part
的 `Kind`（`image/*` → `KindImage`，其余 `KindFile`），消息层再按三类语义分派（图片作附件、
文本并进问题、二进制给元信息）。

### `POST /stop`

```
{ "conversation": "room1" }  →  200 { "ok": true, "stopped": <bool> }
```

`stopped=false` 是**如实回答**——没有在跑的一轮就说没有，前端据此决定要不要把键切回「发送」。
会话键按消息层同一条规则算（见 §4），两边指的是同一场。

### `GET /resources`

```
GET /resources?conversation=room1
→ 200 { "resources": [ { "name": "a.png", "kind": "image", "mimeType": "image/png", "source": "media" } ] }
```

**字节已过期的条目不列**——列一个引用不了的名字没意义。`source` 现在恒为 `media`，将来接
沙盒资源会多一个值（前端不用改）。

### `GET /files/{id}`

下载一份出站媒体。响应头：

- `Content-Type`：服务端定的媒体 MIME（`mediaMime`：声明类型 → 内容嗅探 → 后缀，三级）。
- `X-Content-Type-Options: nosniff`：否则「HTML 字节配 `image/png` 头」也能被当 HTML 渲染。
- `Content-Disposition`：**黄金清单之内 inline，之外一律 attachment**（见 §6 的 `inlineMime`）。
- `Content-Security-Policy`：`default-src 'none'; script-src 'none'; ...; form-action 'none';
  base-uri 'none'; frame-ancestors 'self'`——万一有主动内容被误放进内联清单，也跑不起来。

**未鉴权或 id 不存在 → `404`。** `id` 是 16 字节随机十六进制（不是自增），让「猜一个别人的
文件」不可行。

---

## 3. SSE 事件（`GET /events`）

连接建立时响应头 `Content-Type: text/event-stream`、`Cache-Control: no-cache`、
`Connection: keep-alive`、`X-Accel-Buffering: no`（让 nginx 之类别缓冲）。帧格式固定：

```
event: <名字>
data: <JSON>

```

**命名事件 + JSON data**：事件名是契约的一部分，前端按名字分派。服务端推的事件：

| event | data | 说明 |
|---|---|---|
| `ready` | `{"conversation":"<原始 id>","account":"<账号>"}` | 连接建立后的第一帧 |
| `message` | `{"text":"…"}` | 一段**完整答复**（走 `Senders.Text`）；**主动推送也走这里** |
| `reasoning` | `{"text":"增量"}` | 推理增量，一词一句地追加 |
| `tool` | `{"name","arguments","result"}` | 一次工具调用完成（参数与结果都是原样字符串） |
| `file` | `{"url":"/files/<id>","name","mimeType","kind"}` | 出站媒体；`kind` 是 `file` / `image` |
| `usage` | `{"promptTokens","completionTokens","totalTokens","cachedTokens"}` | 轮末的真实用量（全零则不推） |
| `turn` | `{"running":true|false}` | 执行中状态：`true`=一轮开始、`false`=结束 |
| `stored` | `{"paths":["inbox/a.png"]}` | 入站文件已落沙盒（`FKA_MEDIA_MODE=both/sandbox`） |

- **`reasoning` / `tool` / `usage` / `stored` 与 `message` 分属两类**：前者是过程事件与账目
  （由 `sseEmitter` 从 `Emitter` / `Notifier` 转来），`message` 是最终话语。前端把推理与工具
  默认折叠，把用量/落盘拼成一行系统提示——**服务端只给结构化事实，不预先拼整句话**。
- `usage` 的 `cachedTokens` 是 `promptTokens` 的子集，不是所有端都报。
- `stored` 只带**路径**，措辞由前端定。
- **心跳**：每 20 s 一条注释帧 `: ping`（`\n\n` 结尾），只为穿过中间代理并探活。
- **断线**：SSE 读结束 = 断线。浏览器 `EventSource` 会自己重连并再收到 `ready`；前端在
  `ready` 时按「空闲」起步（真在跑会马上收到 `turn`）。
- 投递**非阻塞**：单连接有 256 条有界队列，满则丢该连接的这一帧并记一条 Warn——
  **慢浏览器不能堵住别的连接**（与接缝订阅队列同一条原则）。
- **前端还会看到 `error` 事件**：那是 `EventSource` 内建的重连信号，不是服务端推的。

---

## 4. 会话与身份

- **客户端只发原始会话 id**（如 `room1`），它进 `SessionKey`、最终进会话历史文件名，所以必须
  合法：`^[A-Za-z0-9._-]{1,64}$`。不合法一律 `400 {"error":"会话 id 不合法"}`（在
  `events` / `messages` / `stop` / `resources` 四处校验）。
- **服务端把会话 id 纳入 principal**：内部键 = `<PrincipalID>:<原始会话>`（`sessionConversation`）。
  **必须纳入**——否则两个用户各取同名 `room1` 会落进同一条会话，历史、记忆、SSE 房间全串在
  一起且不报错。客户端只看得到自己的原始 id。
- **身份由 token 决定，模型碰不到**：`PrincipalID = web:<user>`（`web-` 前缀由代码加，配置里
  写裸值）。SSE 房间、`/files`、`/resources`、历史都按这个身份隔离。
- **单账号、多用户**：所有用户共用一个 `AccountID`（默认 `default`，`WEB_CHANNEL_ACCOUNT`
  可改——它进会话键，**改它等于换一套历史**）。分片键含身份，所以**不同 token 的用户并行、
  同一用户的会话串行**。

---

## 5. 入站映射到 `InboundMessage`

一条 `POST /messages` 产出：

| 字段 | 取值 |
|---|---|
| `ChannelID` | `web` |
| `AccountID` | 渠道实例的账号（默认 `default`） |
| `MessageID` | 新生成的随机会话内 id |
| `SenderID` / `PrincipalID` | 认证得到的 principal（同上） |
| `RecipientID` | 账号 id |
| `ConversationID` | **内部键** `<principal>:<原始会话>`（见 §4） |
| `Parts` | 文本 part（若有）+ `#引用` 解析出的 part + 上传文件的 part |
| `Timestamp` | 毫秒 |

之后的一切（分发、问答、按原路答复）与微信渠道走**完全相同**的路径——本渠道在接缝处收敛
成 `InboundMessage`，业务层不认识「web」这个词。

---

## 6. 媒体

### 出站（agent → 浏览器）

`Senders.File` / `Senders.Image` 把字节存进内存 `blobs`，SSE 推一帧 `file` 带一个相对
`/files/{id}` URL（**不内联 base64**：帧会被前端 JSON 解析、也进不了浏览器缓存）。字节来源
两种：`Data`（远端沙盒经 MCP 到达）或 `Path`（本机文件）。

- **TTL 10 分钟、总量上限 64 MiB**（超了淘汰最旧）、单件上限 64 MiB。这个 agent 不拥有数据，
  媒体只是过路。
- `inlineMime` 是**黄金清单**，不是黑名单：`image/*`（含 **SVG**，预览走 `<img>`、直接打开
  的那条路由由 CSP 封脚本）、`video/*`、`audio/*`、`application/pdf`、`text/plain` 内联；其余
  一律附件下载——内联一个能跑脚本的描述就是同源 XSS。**动 `handleFiles` 的 CSP 之前先看这里：
  SVG 能内联以那段 CSP 为前提。**
- 出站媒体同时记进会话媒体目录，供之后的 `#引用`（见下）。

### 入站（浏览器 → agent）

`multipart` 的每个文件落进**独立的 `inbound` 暂存**（与出站 `blobs` 分开：入站那份不该从
`/files/{id}` 暴露出去）。消息层取图时经 `MediaFetcher.FetchMedia` 读回，过期或不存在就如实
回一句「图片取不回来」，不空猜。

- 整体请求体上限 **16 MiB**、单件 **8 MiB**（与消息层一致，免得「网页收下了、消息层又拒了」）。

### `#引用` 与会话媒体目录

正文里的 `#名字` 在入站时被解析（`resource.go`），命中会话媒体的取回字节并成 part，与上传的
文件走**同一条**分派。

- 目录**只记名字与句柄、不存字节**（字节仍在两个暂存里过路）；列清单、解析引用时**现查现
  滤**，字节没了就不列、就跳过。每个会话最多记 50 条，同名覆盖、最新那份为准。
- 名字到空白或句读为止；含空格/句读的名字写成 `#"名 字"`。只认**词首**的 `#`：`C#` 不算，
  markdown 的 `# 标题`（`#` 后是空格）也不算。
- **名字不存在、字节过期、认不出都原样留文字、不报错**——一次问答不该因为一个笔误的引用就
  发不出去（与 CLI `@引用`、沙盒 `read` 同一条克制）。
- 前端打 `#` 时经 `GET /resources` 拿这份目录、选中插入 `#名字`——**两个方向都以名字为准，
  所以名字即接口**。

---

## 7. 配置

| 环境变量 | 默认 | 作用 |
|---|---|---|
| `WEB_CHANNEL_ADDR` | 空（**不接**） | 监听地址，如 `127.0.0.1:8787`。**设了才启用本渠道** |
| `WEB_CHANNEL_USERS` | `<安装根>/web-users.json` | 用户表文件 `{"users":[{"token":"…","user":"alice"}]}` |
| `WEB_CHANNEL_TOKEN` | — | 单用户回落的令牌（配了用户表则忽略） |
| `WEB_CHANNEL_PRINCIPAL` | `web:default` | 单用户回落的身份 |
| `WEB_CHANNEL_ACCOUNT` | `default` | 账号标识。**它进会话键，改它等于换历史** |
| `WEB_CHANNEL_STATIC` | `<安装根>/web` | 静态页目录。缺失时 `/` 返回一段说明而不是 `500` |

**未配置返回空实例而不是返错**（默认不接 web 是常态）；配了地址却没配任何用户才返错——那是
一次明确的配置错误。

---

## 8. 落在哪

- 服务端：`internal/channels/web/`——`server.go`（路由/认证/入站/SSE/文件/静态）、
  `channel.go`（接缝实现 + 出站）、`emitter.go`（`Emitter` + `Notifier` → SSE）、
  `hub.go`（会话房间、有界队列、非阻塞广播、心跳）、`resource.go`（媒体目录 + `#引用`）、
  `store.go`（带 TTL 的 blob 暂存）。
- 前端：`web/app.js`（无框架、无构建：原生 `fetch` + `EventSource`）、`index.html`、`style.css`。
  运行期从 `<安装根>/web` 读，仓库里的 `web/` 是源码。
- 测试：`internal/channels/web/web_test.go`、`resource_test.go` 是这套协议可执行的说明。
