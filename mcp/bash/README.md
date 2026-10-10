# mcp/bash —— 沙盒 bash MCP server

**这里是能力，不是 agent。** agent 在 [`internal/`](../../internal) 里，它**不认识任何数据的存储**——
它的本事全靠 MCP server 与 skill 顶上，本目录就是其中之一。

`bin/fka-bash` 是一个**独立进程**。默认走 **stdio**，由主程序从 `mcp.json` 当子进程拉起；
`--transport` 可以让它在**同一份工具实现**上换成 **HTTP+SSE** 或 **streamable HTTP**。
崩了不影响主程序。

它提供两个工具：

| 工具 | 作用 |
|---|---|
| `run` | 在沙盒里执行一条 shell 命令，返回退出码与 stdout/stderr |
| `read` | 读沙盒里的一个文件，**经 MCP 内容块**把内容交给模型：文本给 `type:"text"` 块（32 KiB 上限、超出截断）、图片给带 base64 的 `type:"image"` 块、音频给带 base64 的 `type:"audio"` 块、其它二进制只给类型与大小 |

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
    "bash": {
      "command": "/path/to/bin/fka-bash",
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
bin/fka-bash --transport sse  --addr 127.0.0.1:8080   # 老式 HTTP+SSE，路径 /sse
bin/fka-bash --transport http --addr 127.0.0.1:8080   # streamable HTTP，路径 /mcp
```

此时主程序那份 `mcp.json` 换成一端 `url`（不再拉子进程）：

```jsonc
{
  "mcpServers": {
    "bash": { "url": "http://127.0.0.1:8080/sse", "transport": "sse" }
  }
}
```

| 值 | 说明 |
|---|---|
| `stdio` | 默认。JSON-RPC 走 stdin/stdout，被主程序当子进程拉起 |
| `sse` | 老式 HTTP+SSE：GET 开着一条流，服务端先给 `endpoint` 事件告诉你往哪 POST |
| `http` | streamable HTTP：直接 POST 那个 url，响应就在响应体里 |

全部参数一览：`fka-bash -h`（或 `--help`）。它也走 stderr——stdio 档下 stdout 是
JSON-RPC 通道，帮助不能往那里写。帮助**早于沙盒构造**：没装 bwrap 也能看。

⚠️ **默认只听 `127.0.0.1:8080`**。这个 server 能在沙盒里跑命令，默认绑 `0.0.0.0`
等于把命令执行权敞开给同网段——要对外必须显式写 `--addr`。
⚠️ **认不出的 `--transport` 按用法错退出（2）**，不静默退回 stdio——否则「我配了 sse」
会变成一句查不出的假象。
⚠️ 内容块是协议层的，不绑定传输：`read` 的文本 / 图片 / 音频块换到 HTTP 后面照旧。

## 容器化：把整道沙盒装进 Docker

`Dockerfile` + `docker-compose.yml` 把 **fka-bash + python3 + nodejs** 打成一个镜像，
让「连 python/node 都在里面」的开箱沙盒一条命令起来：

```bash
make docker-bash                                       # 交叉编译 fka-bash(linux) 并构建镜像
docker compose -f mcp/bash/docker-compose.yml up -d    # 起容器
# 宿主侧 mcp.json 指向容器发布的端口：
# { "mcpServers": { "bash": { "url": "http://127.0.0.1:8080/mcp", "transport": "http" } } }
```

**镜像里不编译**（省掉容器内的 Go 工具链）：`docker-bash` 先在宿主把 fka-bash
交叉编译成 `bin/fka-bash-linux-<arch>`，Dockerfile 只 `COPY` 它进去。手动等价于：

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bin/fka-bash-linux-arm64 ./mcp/bash
docker build -f mcp/bash/Dockerfile -t fka-bash .
```

### 边界换了一处：容器即沙盒

宿主部署用 **bwrap 起命名空间**；容器里 **Docker 就是那道边界**——文件系统、网络、
PID、user 全在容器内——所以容器里的 fka-bash 走 **`--mode direct`**，不再叠一层 bwrap
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

基础镜像是 **`ubuntu:24.04`**（自带 python3.12 与 nodejs 18），环境变量见 Dockerfile；
`PIP_BREAK_SYSTEM_PACKAGES=1` 是为了让 Ubuntu 24.04 上 `pip install` 不被 PEP 668 拦下。
构建上下文必须是**仓库根**（要 `COPY` 到 `bin/`）。

## 构建

与主程序同一个 module，从仓库根构建：

```bash
CGO_ENABLED=0 go build -o bin/fka-bash ./mcp/bash   # 零 CGO
```

依赖只有 `mark3labs/mcp-go`。Bubblewrap 是**运行期**依赖（不是编译期、也不要 CGO）。

## 配置

| 参数 | 环境变量 | 说明 |
|---|---|---|
| `--root` | `BASH_SANDBOX_ROOT` | 沙盒根，默认 `<安装根>/sandbox` |
| `--mode` | `BASH_SANDBOX_MODE` | `bwrap`（默认）或 `direct` |
| `--bwrap` | `BASH_BWRAP` | Bubblewrap 路径，默认从 PATH 找 |
| `--share-net` | `BASH_SHARE_NET` | 保留宿主网络（默认独立网络） |
| — | `BASH_BWRAP_ARGS` | 追加给 bwrap 的参数（空白分隔） |
| `--allow` | `BASH_ALLOW` | 命令白名单（非空即白名单模式） |
| `--deny` | `BASH_DENY` | 追加命令黑名单（内置那份不能清空） |
| `--transport` | `BASH_MCP_TRANSPORT` | `stdio`（默认）/ `sse` / `http`，控制启动方式 |
| `--addr` | `BASH_MCP_ADDR` | SSE / http 的监听地址，默认 `127.0.0.1:8080` |

## 执行日志

每条 `run` 命令**默认**落一行 JSON（JSON Lines）到
`<安装根>/logs/bash-exec-<UTC日期>.log`（`FKA_LOG_DIR` 可改目录，安装根按
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
mcp/bash/
├── Dockerfile             容器化：fka-bash + python3 + nodejs（容器即沙盒）
├── docker-compose.yml     一条命令起容器（硬化参数 + /sandbox、/logs 双卷）
├── boundary_test.go       边界测试：不许依赖树外的包；server 必须是 main 包
├── main.go                启动（沙盒路径/模式/策略解析、--transport 选传输）
├── main_test.go           --transport / --addr 解析与 SSE 真链路用例
├── server.go              run / read 工具的声明与实现
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
