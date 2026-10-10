# syntax=docker/dockerfile:1
#
# fka-bash 沙盒容器：把 mcp/bash 和 python3 / nodejs 一起打进一个镜像。
#
# ## 不在容器里编译
#
# 镜像**不构建** fka-bash —— 它假设 `bin/fka-bash-linux-<arch>` 已经存在，
# 由宿主交叉编译好（零 CGO，一条 `GOOS=linux go build` 就够；见 Makefile 的
# `docker-bash`）。这样镜像里没有 Go 工具链，构建也快得多、可复现。
#
#   make docker-bash          # 交叉编译 linux/amd64,arm64 并构建镜像
#   或手动：
#     GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bin/fka-bash-linux-arm64 ./mcp/bash
#     docker build -f mcp/bash/Dockerfile -t fka-bash .
#
# 上下文必须是**仓库根**（要能 COPY 到 bin/），Dockerfile 自己则在 mcp/bash/。
#
# ## 为什么容器里用 direct 模式
#
# 宿主部署时默认用 Bubblewrap 起命名空间沙盒（见 sandbox.go）。容器化时**反过来**：
# Docker 容器自己就是那道边界——文件系统、网络、PID、user 全在容器内，所以容器里的
# fka-bash 走 `--mode direct`，不再叠一层 bwrap（那需要额外 capability、且是重复隔离）。
#
# **direct 档不是「关掉沙盒」**：命令白名单/黑名单、cwd 的词法+符号链接越界检查、
# 硬超时、输出上限、多租户子目录全部照旧。它只是把「OS 级命名空间」那一层交给 Docker。
# 真正的加固在运行参数里（docker-compose.yml：read_only / cap_drop / no-new-privileges）。
#
# ## 非 root
#
# 容器里**不用 root** 启动 server 与沙箱：建一个非特权用户 `fka`（uid 10001），
# `/sandbox`、`/logs` 归它所有，`USER fka`。于是沙箱里跑的任何命令都是这个非 root
# 身份，写不进只读根、也拿不到容器外的权限。
#
# ⚠️ **direct 档不做跨租户隔离**：容器里没有 bwrap（Docker 默认禁 unprivileged
# user namespace），所以 `/sandbox` 下的兄弟租户目录对同 uid 是可读的。要真正
# 「禁止跨用户读取」得在容器内另加机制（Landlock）或每租户一容器。

# ── 运行阶段：Ubuntu + python3 + nodejs + fka-bash ─────────
FROM ubuntu:24.04

# TARGETARCH（amd64 / arm64）由 BuildKit 注入，用来挑对应架构的预编译二进制。
ARG TARGETARCH

# python3 / nodejs 是给沙盒里的模型用的能力（run 工具会提示它用 python3/node 跑脚本）；
# bash / coreutils / findutils 是 run 工具的地基。git / curl / jq / unzip 是常见刚需。
# Ubuntu 24.04 用 deb822 格式（/etc/apt/sources.list.d/ubuntu.sources）。换中国镜像，
# 否则从 archive.ubuntu.com / security.ubuntu.com 拉包在部分网络下几乎必然超时。
# 只换主机名、保留路径：amd64 是 archive/security.ubuntu.com/ubuntu，
# arm64 是 ports.ubuntu.com/ubuntu-ports，两者都指向清华对应的路径。
RUN sed -i \
        -e 's@//archive.ubuntu.com@//mirrors.tuna.tsinghua.edu.cn@g' \
        -e 's@//security.ubuntu.com@//mirrors.tuna.tsinghua.edu.cn@g' \
        -e 's@//ports.ubuntu.com@//mirrors.tuna.tsinghua.edu.cn@g' \
        /etc/apt/sources.list.d/ubuntu.sources \
    && apt-get update -o Acquire::Retries=5 \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends -o Acquire::Retries=5 \
        bash coreutils findutils procps passwd \
        ca-certificates \
        python3 python3-pip python3-venv \
        nodejs npm \
        git curl jq unzip \
    && rm -rf /var/lib/apt/lists/*

# 宿主交叉编译好的静态二进制（零 CGO）。COPY 保留可执行位。
COPY bin/fka-bash-linux-${TARGETARCH} /usr/local/bin/fka-bash

# 沙盒根（可挂卷）与审计日志目录（**必须与沙盒根分开**：模型能写沙盒，不能擦自己的审计）。
# 目录归非 root 用户所有；**server 与它拉起的沙箱命令都以此用户运行**，不要 root。
RUN mkdir -p /sandbox /logs \
    && useradd --create-home --uid 10001 --shell /usr/sbin/nologin fka \
    && chown -R fka:fka /sandbox /logs
USER fka
VOLUME ["/sandbox", "/logs"]

# 配置全部走环境变量（main.go 的解析优先级是 --flag > env > 默认）。
#   BASH_SANDBOX_MODE=direct  —— 容器即边界，不再叠 bwrap
#   BASH_MCP_TRANSPORT=http   —— 容器无 stdin，用 streamable HTTP 供 agent 连
#   FKA_LOG_DIR=/logs         —— 执行日志落在挂载卷，不写进只读根
#   PIP_BREAK_SYSTEM_PACKAGES=1 —— Ubuntu 24.04 是 PEP 668 externally-managed，放开系统 pip
ENV BASH_SANDBOX_ROOT=/sandbox \
    BASH_SANDBOX_MODE=direct \
    BASH_MCP_TRANSPORT=http \
    BASH_MCP_ADDR=0.0.0.0:8080 \
    FKA_LOG_DIR=/logs \
    PIP_BREAK_SYSTEM_PACKAGES=1

EXPOSE 8080

# 容器内听 0.0.0.0 是惯例（由宿主 publish 决定对外面）；HTTP transport 挂 /mcp。
# 用 `docker run <image> --help` 可覆盖成任意 fka-bash 参数。
ENTRYPOINT ["fka-bash"]
