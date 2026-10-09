# mcp/bash —— 沙盒 bash MCP server

**这里是能力，不是 agent。** agent 在 [`internal/`](../../internal) 里，它**不认识任何数据的存储**——
它的本事全靠 MCP server 与 skill 顶上，本目录就是其中之一。

`bin/fka-bash` 是一个**独立进程**（stdio），由主程序从 `mcp.json` 当子进程拉起。崩了不影响主程序。

它提供一个工具 `run`：在沙盒里执行一条 shell 命令，返回退出码与 stdout/stderr。

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
├── boundary_test.go       边界测试：不许依赖树外的包；server 必须是 main 包
├── main.go                启动（沙盒路径/模式/策略解析）
├── server.go              run 工具的声明与实现
├── sandbox.go             沙盒核心：cwd 限制、超时、输出上限、命令策略
├── proc_unix.go           进程组（Unix）：超时杀整棵进程树
├── proc_windows.go        进程组的 Windows 退化实现
├── sandbox_test.go        cwd/超时/输出/策略用例（direct 档）
├── sandbox_bwrap_test.go  bwrap 参数顺序与真实隔离用例
├── server_test.go         工具参数与结果文本用例
└── internal/log/          stdio 子进程用的 logger（自给自足的刻意副本）
```
