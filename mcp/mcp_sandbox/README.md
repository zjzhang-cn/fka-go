# mcp/mcp_sandbox —— 沙盒执行 MCP server

**这里是能力，不是 agent。** agent 在 [`internal/`](../../internal) 里，它**不认识任何数据的存储**——
它的本事全靠 MCP server 与 skill 顶上，本目录就是其中之一。

`bin/fka-sandbox` 是一个**独立进程**。默认走 **stdio**，由主程序从 `mcp.json` 当子进程拉起；
`--transport` 可以让它在**同一份工具实现**上换成 **HTTP+SSE** 或 **streamable HTTP**。
崩了不影响主程序。

它提供五个工具：

| 工具 | 作用 |
|---|---|
| `run` | 在沙盒里执行一条 shell 命令，返回退出码与 stdout/stderr |
| `read` | 读沙盒里的一个文件，**经 MCP 内容块**把内容交给模型：文本给 `type:"text"` 块（32 KiB 上限、超出截断）、图片给带 base64 的 `type:"image"` 块、音频给带 base64 的 `type:"audio"` 块、其它二进制只给类型与大小 |
| `export` | 把一个沙盒文件**发给用户**（不是发给模型）：返回 `audience=["user"]` 的嵌入资源，agent 经用户所在渠道送出 |
| `import` | 反向入口：把**用户本轮发来的图片**存进本租户 `inbox/`，供 `read`/`run` 二次处理。字节由 agent 按 `FKA_MEDIA_SINK` 注入，**模型不传参数**（见 `docs/decisions.md` 第 25 条） |
| `import_skill` | 反向入口：把**某个技能自带的脚本**存进本租户 `skills/<技能>/`，供 `read`/`run` 二次处理。模型只传 `skill` 名，字节由 agent 按 `FKA_SKILL_SINK` 注入。保留子目录结构、重名覆盖（见 `docs/decisions.md` 第 29 条） |

### MCP resources

除了工具，本 server 还把本租户沙盒里的文件按 MCP **resources** 原语暴露出去（方便**别的
MCP 客户端**，如 Claude Desktop 直连 fka-sandbox；agent 侧本来就有 `read` 工具）：

| 资源 | 读出来是什么 |
|---|---|
| `sandbox:///`（固定） | 本租户沙盒根下的文件清单（一行一个「相对路径 \t 大小」） |
| `sandbox:///{path}`（模板） | 按路径读文件，分类与 `read` 一致：文本 text 块、图片/音频 blob 块、其它二进制一行元信息 |

⚠️ **列举只能是静态的**：mark3labs 的资源模板没有 `list` 回调（不像 JS SDK），没法按租户
动态列出文件，所以用固定资源 `sandbox:///`（读取即清单）兜底。身份同样走 `_meta`，缺身份
fail-closed。

`read` 与 CLI 的 `@引用` 是**同一套语义**（文本 / 图片 / 二进制三类、同样的上限），
区别是数据走 **MCP 通道**而不是 agent 读本地路径。图片必须作为独立的 `type:"image"`
内容节点返回（带 base64），模型才看得见图。因此把内容块换到 HTTP MCP 传输后面
（同一个工具、同一份 `CallToolResult`）不用改一行：内容块是协议层的，不绑定传输。
路径与 `run` 的 cwd 共用同一套词法 + 符号链接越界检查。

## 沙盒用 Bubblewrap 做真实隔离

默认模式是 `bwrap`：用 Linux 命名空间起一个沙盒——

| 边界 | 怎么强制的 |
|---|---|
| **文件系统** | `/` 只读绑定，只有**沙盒根可写**。`cd /` 逃不出写边界，因为那不可写 |
| **网络** | `--unshare-all` 起独立网络命名空间（`--share-net` 才保留宿主网络） |
| **进程/用户** | PID / IPC / UTS / user 命名空间各自另起，`--die-with-parent` |
| **工作目录** | `cwd` 参数走词法 + 符号链接两道越界检查，只允许沙盒根下的相对子目录 |
| **时间/输出** | 单条命令硬超时（连进程组一起 SIGKILL）、stdout/stderr 各 32KiB 上限 |
| **命令策略** | 按段取每条串联/管道命令的**首个词**比对白名单/黑名单 |

⚠️ **命令策略是护栏，不是墙。** 首个词检查挡不住 `bash -c '…'`、`eval`、解释器之类的
刻意绕过——它拦的是模型顺手发出的 `mount` / `shutdown` / `sudo`，真正的隔离靠 bwrap 的
命名空间。

⚠️ **找不到 `bwrap` 就拒绝启动**，不会静默退化成不隔离的直接执行（那是最坏的失败形态：
看起来配好了，其实没有沙盒）。只有在 macOS / Windows 上才该用 `--mode direct`，且要明白
它只固定 cwd、挡不住 `cd /`。

## 挂上去

默认（stdio）——主程序把它当子进程拉起：

```jsonc
// <安装根>/mcp.json
{
  "mcpServers": {
    "sandbox": {
      "command": "/path/to/bin/fka-sandbox",
      "args": ["--root", "/path/to/sandbox"]
    }
  }
}
```

然后主程序要放行 `external`（**MCP 工具一律是 external**）：

```
LLM_TOOL_EFFECTS=read,external
```

`--root` 不给时默认 `<安装根>/sandbox`，启动时建。

## 换传输方式（`--transport`）

传输由参数决定，**工具实现一行不改**：

```bash
bin/fka-sandbox --transport sse  --addr 127.0.0.1:8080   # 老式 HTTP+SSE，路径 /sse
bin/fka-sandbox --transport http --addr 127.0.0.1:8080   # streamable HTTP，路径 /mcp
```

此时主程序那份 `mcp.json` 换成一端 `url`（不再拉子进程）：

```jsonc
{
  "mcpServers": {
    "sandbox": { "url": "http://127.0.0.1:8080/sse", "transport": "sse" }
  }
}
```

| 值 | 说明 |
|---|---|
| `stdio` | 默认。JSON-RPC 走 stdin/stdout，被主程序当子进程拉起 |
| `sse` | 老式 HTTP+SSE：GET 开着一条流，服务端先给 `endpoint` 事件告诉你往哪 POST |
| `http` | streamable HTTP：直接 POST 那个 url，响应就在响应体里 |

全部参数一览：`fka-sandbox -h`（或 `--help`）。它也走 stderr——stdio 档下 stdout 是
JSON-RPC 通道，帮助不能往那里写。帮助**早于沙盒构造**：没装 bwrap 也能看。

⚠️ **默认只听 `127.0.0.1:8080`**。这个 server 能在沙盒里跑命令，默认绑 `0.0.0.0`
等于把命令执行权敞开给同网段——要对外必须显式写 `--addr`。
⚠️ **认不出的 `--transport` 按用法错退出（2）**，不静默退回 stdio——否则「我配了 sse」
会变成一句查不出的假象。
⚠️ 内容块是协议层的，不绑定传输：`read` 的文本 / 图片 / 音频块换到 HTTP 后面照旧。

## 容器化：把整道沙盒装进 Docker

`Dockerfile` + `docker-compose.yml` 把 **fka-sandbox + python3 + nodejs** 打成一个镜像，
让「连 python/node 都在里面」的开箱沙盒一条命令起来：

```bash
make docker-sandbox                                       # 交叉编译 fka-sandbox(linux) 并构建镜像
docker compose -f mcp/mcp_sandbox/docker-compose.yml up -d    # 起容器
# 宿主侧 mcp.json 指向容器发布的端口：
# { "mcpServers": { "sandbox": { "url": "http://127.0.0.1:8080/mcp", "transport": "http" } } }
```

### 构建、部署、使用（Docker 三步）

Docker 路线一句话：**先在宿主交叉编译 + 构建镜像 → `up -d` 起容器 → 宿主 `mcp.json`
指向容器端口**。镜像里不编译（见下），所以顺序不能颠倒。

**1. 构建镜像**（在仓库根执行）

```bash
make docker-sandbox
# 或按需裁剪预装库（开关见「预装的 Python 库」）：
make docker-sandbox SANDBOX_PY_DATA=0 SANDBOX_PY_PLOT=0 SANDBOX_PY_DOCS=0
```

**2. 部署**（起容器）

```bash
docker compose -f mcp/mcp_sandbox/docker-compose.yml up -d
```

- 宿主端口默认 `8080`，用 `SANDBOX_MCP_PORT` 覆盖：`SANDBOX_MCP_PORT=8082 docker compose … up -d`。
- 沙盒数据默认落命名卷；要落到宿主目录设 `SANDBOX_ROOT_DIR`（见下一节「查看沙盒文件」）。
- ⚠️ 改了 `mcp/mcp_sandbox/**` 的代码后**必须回到第 1 步重建镜像**再 `up -d`，否则容器里
  跑的还是旧二进制（细节见下）。

**3. 使用**（让 agent 连上）

容器以 streamable HTTP 挂在 `/mcp`，宿主 `mcp.json` 里写一端 `url`（**不再拉子进程**）：

```jsonc
// <安装根>/mcp.json
{
  "mcpServers": {
    "sandbox": { "url": "http://127.0.0.1:8082/mcp", "transport": "http" }
  }
}
```

端口必须与第 2 步发布的宿主端口一致。还要放行 MCP 工具（**MCP 工具一律是 external**）：

```
LLM_TOOL_EFFECTS=read,external
```

之后模型即可调 `mcp__sandbox__run` / `read` / `export` / `import` / `import_skill`
（工具全名 `<源>__<工具>`，源名取 `mcp.json` 里的键，这里是 `sandbox`）。

**一键**：`make setup`（`scripts/setup.sh`）把三步串起来——构建镜像 → 探测空闲端口 → `down`/`up`
→ 把端口写进 `mcp.json`。`--no-docker` 或宿主没装 docker 时跳过容器，sandbox 工具连不上
（补齐后重跑脚本即可）。

**验证**

```bash
docker exec mcp_sandbox_sandbox_1 ls -l /sandbox   # 容器内沙盒根（非交互别加 -t）
fka list all                                       # 应列出 mcp__sandbox__run / read / export / import / import_skill
```

⚠️ **改了 `mcp/mcp_sandbox/**` 的代码后，容器里跑的还是旧二进制——必须重建镜像**：
`make docker-sandbox`（宿主交叉编译 + `docker build`）后**再** `docker compose ... up -d`。
镜像里不编译（见下），所以只 `up` 不重建等于没改；症状是「代码改了但沙盒行为没变」。
`package main` 里加/改工具、改沙盒边界都算——一切都只在**镜像里的那个二进制**生效。

**镜像里不编译**（省掉容器内的 Go 工具链）：`docker-sandbox` 先在宿主把 fka-sandbox
交叉编译成 `bin/fka-sandbox-linux-<arch>`，Dockerfile 只 `COPY` 它进去。手动等价于：

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bin/fka-sandbox-linux-arm64 ./mcp/mcp_sandbox
docker build -f mcp/mcp_sandbox/Dockerfile -t fka-sandbox .
```

### 边界换了一处：容器即沙盒

宿主部署用 **bwrap 起命名空间**；容器里 **Docker 就是那道边界**——文件系统、网络、
PID、user 全在容器内——所以容器里的 fka-sandbox 走 **`--mode direct`**，不再叠一层 bwrap
（那需要额外 capability，且是重复隔离）。

**`direct` 不是「关掉沙盒」**：命令白名单/黑名单、cwd 词法+符号链接越界、硬超时、
输出上限、多租户子目录**全部照旧**，砍掉的只是「OS 级命名空间」那一层，由 Docker 顶上。
真正的加固在运行参数里（compose 已给）：`read_only` 根文件系统、只给 `/sandbox` 可写、
`cap_drop: ALL`、`no-new-privileges`。

| 差异 | 宿主（bwrap） | 容器（direct + Docker） |
|---|---|---|
| 写边界 | `/` 只读，仅沙盒根可写 | 容器根只读，仅挂载的 `/sandbox` 可写 |
| 网络 | **默认独立**（`--share-net` 才保留） | 默认通网（否则 agent 连不上 8080）；要断网见 compose 注释 |
| 解释器 | 只读绑宿主那几份 | 镜像内自带 python3 / nodejs，**与宿主无关** |
| 审计日志 | 落在安装根 `logs/`（只读根保护） | 落在挂载的 `/logs`（与 `/sandbox` 分开） |

基础镜像是 **`ubuntu:24.04`**（自带 python3.12 与 nodejs 18），环境变量见 Dockerfile：
`SANDBOX_ALLOW_PIP=1` 声明本容器默认通网、可 `pip install`；`PIP_BREAK_SYSTEM_PACKAGES=1`
绕过 Ubuntu 的 PEP 668；`PIP_USER=1` 配合只读根，把 pip 安装落到 `$HOME/.local`
（= 沙盒根，可写），该目录已在 `PATH` 最前。构建上下文必须是**仓库根**（要 `COPY` 到 `bin/`）。

### 预装的 Python 库

高频任务（数据处理、出图、读写 Excel/图片）预置进镜像，省去每次会话 pip 联网装；长尾库
仍可 `pip install`。默认装：

| 开关（build-arg） | 默认 | 预装 |
|---|---|---|
| （总是装） | — | `requests`、`openpyxl`、`pillow`、`tzdata` |
| `SANDBOX_PY_DATA` | `1` | `numpy`、`pandas` |
| `SANDBOX_PY_PLOT` | `1` | `matplotlib` + `fonts-noto-cjk`（**中文图不再画成方框**） |
| `SANDBOX_PY_DOCS` | `1` | `lxml`、`bs4` + `pypdf`、`reportlab`（PDF 读取） |
| `SANDBOX_PY_MEDIA` | `0` | `ffmpeg` |
| `SANDBOX_PY_SCIPY` | `0` | `scipy` |

裁剪精简镜像（例如只要 shell 与数据）：

```bash
make docker-sandbox SANDBOX_PY_DATA=0 SANDBOX_PY_PLOT=0
```

库都装在**系统** site-packages（沙盒里直接用），**不装 `/sandbox`**——那是每租户单独的卷，
装进去别人看不到。

### 查看沙盒文件 / 把沙盒落到宿主目录

容器里沙盒根是 `/sandbox`，各租户是它下面的子目录（`<principal>/`，如 `default/`）。直接看：

```bash
docker exec mcp_sandbox_sandbox_1 ls -l /sandbox          # 容器内看一眼
docker exec -ti mcp_sandbox_sandbox_1 bash                # 进容器看
```

⚠️ `-ti` 里的 `-t` 要真 TTY：**脚本 / 管道里带 `-t`（或 `-ti`）会报
`the input device is not a TTY`**，非交互场景去掉 `-t`（`docker exec <容器> ls /sandbox`）。

默认 `/sandbox` 是**命名卷**（属主天然是镜像里 chown 过的 `fka`）。要把它落到宿主某个目录，
给 `SANDBOX_ROOT_DIR` 一个**宿主绝对路径**即可（compose 里是 `${SANDBOX_ROOT_DIR:-sandbox}`，
不设就回落到命名卷 `sandbox`）：

```bash
mkdir -p /宿主目录 && sudo chown 10001:10001 /宿主目录
SANDBOX_ROOT_DIR=/宿主目录 docker compose -f mcp/mcp_sandbox/docker-compose.yml up -d
```

⚠️ bind 宿主目录要**先建好并 `chown 10001:10001`**：容器以非 root 的 `fka`（uid 10001）
运行，宿主目录不可写会让容器**起来即退**（症状 `mkdir /sandbox/...: permission denied`）；
Docker 代建的目录是 root 属主，同样写不进。也**别指向仓库内**，否则 root/10001 属主会污染
构建上下文、下次 `docker build` 直接失败。

## 构建

与主程序同一个 module，从仓库根构建：

```bash
CGO_ENABLED=0 go build -o bin/fka-sandbox ./mcp/mcp_sandbox   # 零 CGO
```

依赖只有 `mark3labs/mcp-go`。Bubblewrap 是**运行期**依赖（不是编译期、也不要 CGO）。

## 配置

| 参数 | 环境变量 | 说明 |
|---|---|---|
| `--root` | `SANDBOX_ROOT` | 沙盒根，默认 `<安装根>/sandbox` |
| `--mode` | `SANDBOX_MODE` | `bwrap`（默认）或 `direct` |
| `--bwrap` | `SANDBOX_BWRAP` | Bubblewrap 路径，默认从 PATH 找 |
| `--share-net` | `SANDBOX_SHARE_NET` | 保留宿主网络（默认独立网络） |
| — | `SANDBOX_BWRAP_ARGS` | 追加给 bwrap 的参数（空白分隔） |
| `--allow` | `SANDBOX_ALLOW` | 命令白名单（非空即白名单模式） |
| `--deny` | `SANDBOX_DENY` | 追加命令黑名单（内置那份不能清空） |
| `--transport` | `SANDBOX_MCP_TRANSPORT` | `stdio`（默认）/ `sse` / `http`，控制启动方式 |
| `--addr` | `SANDBOX_MCP_ADDR` | SSE / http 的监听地址，默认 `127.0.0.1:8080` |

## 执行日志

每条 `run` 命令**默认**落一行 JSON（JSON Lines）到
`<安装根>/logs/sandbox-exec-<UTC日期>.log`（`FKA_LOG_DIR` 可改目录，安装根按
`$FKA_HOME` → 可执行文件目录 → cwd 三级回退）。`read` 不记。

```json
{"time":"2026-10-09T08:11:12.345Z","cwd":"work","command":"ls -la","exit_code":0,"duration_ms":7}
{"time":"2026-10-09T08:11:13.100Z","cwd":"","command":"mount /dev/sda1","duration_ms":0,"error":"命令 \"mount\" 在内置/配置的禁用列表里，已拒绝执行"}
```

记的是**元信息不记输出正文**：时间、cwd、命令、退出码、是否超时、是否截断、耗时。
被策略拒 / cwd 越界 / 启动失败的尝试也记一条（有 `error`、**没有 `exit_code`**——
没跑的命令不能伪装成退出码 0）。超时时不记退出码（命令是被杀的，0 会被误读成正常结束）。

⚠️ **落在安装根而非沙盒根**：沙盒对模型可写，审计不该让被审计者自己擦。bwrap 模式下
`/` 只读绑定，模型写不进也删不掉这份日志。
⚠️ **写不进去不报错**：日志是旁路，磁盘满 / 权限问题不该把一条本来能跑的命令挡下来。

## 三条硬约束

### stdout 一个字都不能有

stdout 是 JSON-RPC 的通道。**任何** `fmt.Println` 都会插进协议流里把 server 打挂。
诊断信息一律走 `internal/log`（stderr + 按天轮转的文件）。

### bwrap 参数顺序不能乱

`--bind <root> <root>` 必须排在 `--ro-bind / /` 与 `--tmpfs /tmp` **之后**。沙盒根常落在
`/tmp` 下，先绑可写再挂 tmpfs 会让它被盖住，症状是 `--chdir` 直接失败。`sandbox_bwrap_test.go`
同时钉参数顺序与真实隔离。

### 超时要连进程组一起杀

`bash -c` 会 fork 子进程。只杀外壳的话 `sleep 100 &` 会留在超时之后继续跑。Unix 下给
子进程单开一个进程组，超时对整个组发 SIGKILL；Windows 上没有等价物（见 `proc_windows.go`）。

## 目录

```
mcp/mcp_sandbox/
├── Dockerfile             容器化：fka-sandbox + python3 + nodejs（容器即沙盒）
├── docker-compose.yml     一条命令起容器（硬化参数 + /sandbox、/logs 双卷）
├── boundary_test.go       边界测试：不许依赖树外的包；server 必须是 main 包
├── main.go                启动（沙盒路径/模式/策略解析、--transport 选传输）
├── main_test.go           --transport / --addr 解析与 SSE 真链路用例
├── server.go              run / read / export / import / import_skill 工具的声明与实现
├── sandbox.go             沙盒核心：cwd/path 限制、超时、输出上限、命令策略
├── execlog.go             执行日志：每次 run 一条 JSON Lines，按 UTC 日期轮转
├── read.go                read 工具：把沙盒文件按类别经 MCP 内容块交给模型
├── proc_unix.go           进程组（Unix）：超时杀整棵进程树
├── proc_windows.go        进程组的 Windows 退化实现
├── sandbox_test.go        cwd/超时/输出/策略用例（direct 档）
├── sandbox_bwrap_test.go  bwrap 参数顺序与真实隔离用例
├── server_test.go         run 工具参数与结果文本用例
├── execlog_test.go        执行日志用例：JSON Lines、按天轮转、没跑的不留假退出码
├── read_test.go           read 工具分类与越界用例
└── internal/log/          stdio 子进程用的 logger（自给自足的刻意副本）
```
