# syntax=docker/dockerfile:1
#
# fka-sandbox 沙盒容器：把 mcp/mcp_sandbox 和 python3 / nodejs 一起打进一个镜像。
#
# ## 不在容器里编译
#
# 镜像**不构建** fka-sandbox —— 它假设 `bin/fka-sandbox-linux-<arch>` 已经存在，
# 由宿主交叉编译好（零 CGO，一条 `GOOS=linux go build` 就够；见 Makefile 的
# `docker-sandbox`）。这样镜像里没有 Go 工具链，构建也快得多、可复现。
#
#   make docker-sandbox          # 交叉编译 linux/amd64,arm64 并构建镜像
#   或手动（**必须传 --build-arg TARGETARCH**：没装 buildx 时 legacy builder
#   不会自动注入它，`COPY bin/fka-sandbox-linux-` 会报 file not found）：
#     GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bin/fka-sandbox-linux-arm64 ./mcp/mcp_sandbox
#     docker build -f mcp/mcp_sandbox/Dockerfile --build-arg TARGETARCH=arm64 -t fka-sandbox .
#
# 上下文必须是**仓库根**（要能 COPY 到 bin/），Dockerfile 自己则在 mcp/mcp_sandbox/。
#
# ## 为什么容器里用 direct 模式
#
# 宿主部署时默认用 Bubblewrap 起命名空间沙盒（见 sandbox.go）。容器化时**反过来**：
# Docker 容器自己就是那道边界——文件系统、网络、PID、user 全在容器内，所以容器里的
# fka-sandbox 走 `--mode direct`，不再叠一层 bwrap（那需要额外 capability、且是重复隔离）。
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

# ── 运行阶段：Ubuntu + python3 + nodejs + fka-sandbox ─────────
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

# ── Python 常用库（可选，按用途用 build-arg 开关）─────────────
#
# **为什么预装**：容器里虽能 pip 联网装（SANDBOX_ALLOW_PIP=1），但每次会话重装慢、
# 依赖网络；「处理数据 / 出图 / 读写 Excel 图片」这类高频任务预置进镜像更稳。
# 装到**系统** site-packages（/usr 只读但可读，沙盒里直接用），**绝不装 /sandbox**——
# 那是每个租户单独的卷，装进去别人看不到。
#
# 开关（1 开 / 0 关），默认烘焙「数据 + 可视化 + 文档」这一档（DOCS 带上 pypdf/reportlab，
# 普通 PDF 才读得了）；大件（scipy/ffmpeg）默认关。
#   SANDBOX_PY_DATA   numpy/pandas             SANDBOX_PY_PLOT  matplotlib + 中文字体
#   SANDBOX_PY_DOCS   lxml/bs4 + pypdf/reportlab    SANDBOX_PY_MEDIA ffmpeg
#   SANDBOX_PY_SCIPY  scipy
ARG SANDBOX_PY_DATA=1
ARG SANDBOX_PY_PLOT=1
ARG SANDBOX_PY_DOCS=1
ARG SANDBOX_PY_MEDIA=0
ARG SANDBOX_PY_SCIPY=0
RUN set -eux; \
    pkgs="python3-requests python3-openpyxl python3-pillow tzdata"; \
    if [ "$SANDBOX_PY_DATA"  = 1 ]; then pkgs="$pkgs python3-numpy python3-pandas"; fi; \
    if [ "$SANDBOX_PY_PLOT"  = 1 ]; then pkgs="$pkgs python3-matplotlib fontconfig fonts-noto-cjk"; fi; \
    if [ "$SANDBOX_PY_SCIPY" = 1 ]; then pkgs="$pkgs python3-scipy"; fi; \
    if [ "$SANDBOX_PY_DOCS"  = 1 ]; then pkgs="$pkgs python3-lxml python3-bs4"; fi; \
    if [ "$SANDBOX_PY_MEDIA" = 1 ]; then pkgs="$pkgs ffmpeg"; fi; \
    apt-get update -o Acquire::Retries=5; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        -o Acquire::Retries=5 $pkgs; \
    rm -rf /var/lib/apt/lists/*; \
    # apt 没有的（pypdf/reportlab）用 pip 补到系统；--no-cache-dir 不把下载缓存撑进镜像。
    # 走与 apt 同一档的清华 PyPI 镜像（与本文件换 apt 镜像的理由相同）。
    if [ "$SANDBOX_PY_DOCS" = 1 ]; then \
        pip3 install --break-system-packages --no-cache-dir \
            -i https://pypi.tuna.tsinghua.edu.cn/simple pypdf reportlab; fi

# matplotlib 中文：默认字体没有 CJK，中文标题会全是方框；负号也默认渲染成方块。
# **就地改** matplotlib 自带的 matplotlibrc（模板里这三行是注释掉的），而不是往末尾
# 追加——追加会和模板里的键重复，每次 import 都刷一条 "Duplicate key" 警告。
# 字体名用 matplotlib 实际认得的 "Noto Sans CJK JP"（Debian 的 .ttc 合集里
# matplotlib 只登记了 JP 这一面；JP 面同样含全部 CJK 字形，中文照常显示）。
RUN if [ "$SANDBOX_PY_PLOT" = 1 ]; then \
        rc="$(python3 -c 'import os,matplotlib;print(os.path.join(matplotlib.get_data_path(),"matplotlibrc"))')"; \
        sed -i \
            -e 's/^#\?[[:space:]]*font\.family:.*/font.family: sans-serif/' \
            -e 's/^#\?[[:space:]]*font\.sans-serif:.*/font.sans-serif: Noto Sans CJK JP, Noto Sans CJK SC, DejaVu Sans/' \
            -e 's/^#\?[[:space:]]*axes\.unicode_minus:.*/axes.unicode_minus: False/' \
            "$rc"; \
        python3 -c "import matplotlib; assert 'Noto Sans CJK JP' in matplotlib.rcParams['font.sans-serif']"; \
    fi

# 宿主交叉编译好的静态二进制（零 CGO）。COPY 保留可执行位。
COPY bin/fka-sandbox-linux-${TARGETARCH} /usr/local/bin/fka-sandbox

# 沙盒根（可挂卷）与审计日志目录（**必须与沙盒根分开**：模型能写沙盒，不能擦自己的审计）。
# 目录归非 root 用户所有；**server 与它拉起的沙箱命令都以此用户运行**，不要 root。
RUN mkdir -p /sandbox /logs \
    && useradd --create-home --uid 10001 --shell /usr/sbin/nologin fka \
    && chown -R fka:fka /sandbox /logs
USER fka
VOLUME ["/sandbox", "/logs"]

# 配置全部走环境变量（main.go 的解析优先级是 --flag > env > 默认）。
#   SANDBOX_MODE=direct           —— 容器即边界，不再叠 bwrap
#   SANDBOX_MCP_TRANSPORT=http    —— 容器无 stdin，用 streamable HTTP 供 agent 连
#   FKA_LOG_DIR=/logs             —— 执行日志落在挂载卷，不写进只读根
#   PIP_BREAK_SYSTEM_PACKAGES=1   —— Ubuntu 24.04 是 PEP 668 externally-managed，放开系统 pip
#   PIP_USER=1                    —— 根文件系统只读，pip 默认装到 $HOME/.local（= /sandbox，可写）
#   SANDBOX_ALLOW_PIP=1           —— 容器默认通网，声明允许 pip/npm 联网装包，run 工具据此提示模型
ENV SANDBOX_ROOT=/sandbox \
    SANDBOX_MODE=direct \
    SANDBOX_MCP_TRANSPORT=http \
    SANDBOX_MCP_ADDR=0.0.0.0:8080 \
    FKA_LOG_DIR=/logs \
    PIP_BREAK_SYSTEM_PACKAGES=1 \
    PIP_USER=1 \
    SANDBOX_ALLOW_PIP=1

EXPOSE 8080

# 容器内听 0.0.0.0 是惯例（由宿主 publish 决定对外面）；HTTP transport 挂 /mcp。
# 用 `docker run <image> --help` 可覆盖成任意 fka-sandbox 参数。
ENTRYPOINT ["fka-sandbox"]
