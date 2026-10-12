#!/usr/bin/env bash
#
# 一键配置：把这份仓库装成一个可跑的**安装根**——二进制、mcp.json、.env、web 前端、
# 两个 Node MCP server，以及沙盒容器，全部就位。生成的东西与仓库里 `bin/` 那套一致。
#
# 用法：
#   scripts/setup.sh [选项]
#
# 选项：
#   --home DIR     安装根（默认 $FKA_HOME，再默认 <仓库>/bin）
#   --no-docker    不构建/启动沙盒容器（sandbox 工具将不可用）
#   --no-build     不重新编译二进制（用已有的）
#   --ilink        接入微信 iLink（**默认不接**；给了才读 ILINK_ACCOUNT_1_* 写进 .env）
#   --force        覆盖已存在的 .env（**会丢已有的密钥**；默认保留不覆盖）
#   -h, --help     这份帮助
#
# 密钥/配置从环境变量读，缺省写占位：
#   LLM_BASE_URL LLM_API_KEY LLM_MODEL
#   ILINK_ACCOUNT_1_BOT_TOKEN / _BASE_URL / _BOT_ID / _USER_ID（仅 --ilink 时读；
#   _ID 是内部槽位标识，写死 account_001，不用配）
#   WEB_CHANNEL_ADDR（默认 127.0.0.1:8787，留空则不接网页渠道）
#   WEB_CHANNEL_USER / WEB_CHANNEL_TOKEN（网页用户表：默认用户 admin + 随机令牌）
#   SANDBOX_MCP_PORT（沙盒容器发布到宿主回环的端口；默认从 8080 起找空闲的）
#
# 安装根的锚点与 fka 一致（$FKA_HOME → 可执行文件目录 → cwd）。默认用 <仓库>/bin，
# 因为 `make` 系列产出的二进制就在那儿。
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOME_DIR="${FKA_HOME:-$REPO/bin}"
DO_DOCKER=1
DO_BUILD=1
DO_ILINK=0   # 默认不接微信；--ilink 才接
FORCE=0

while [ $# -gt 0 ]; do
  case "$1" in
    --home)      HOME_DIR="${2:?--home 需要一个目录}"; shift 2 ;;
    --home=*)    HOME_DIR="${1#*=}"; shift ;;
    --no-docker) DO_DOCKER=0; shift ;;
    --no-build)  DO_BUILD=0; shift ;;
    --ilink)     DO_ILINK=1; shift ;;
    --force)     FORCE=1; shift ;;
    -h|--help)   sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "认不出的选项：$1（-h 看用法）" >&2; exit 2 ;;
  esac
done

step() { printf '\n\033[1m==> %s\033[0m\n' "$1"; }
warn() { printf '\033[33m[警告] %s\033[0m\n' "$1" >&2; }
die()  { printf '\033[31m[错误] %s\033[0m\n' "$1" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"; }

# 随机令牌：优先 openssl；退回 /dev/urandom（head 只读固定字节，不触发 SIGPIPE）。
rand_token() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 16
  elif [ -r /dev/urandom ]; then
    head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n'
  else
    printf '%s%s' "$(date +%s)" "$$"
  fi
}

step "解析路径"
mkdir -p "$HOME_DIR"
# 转绝对路径：相对路径会按进程 cwd 解析（mcp.json 里写相对命令尤其危险），
# 与 fka 的 resolveRoot 同一条理由。
HOME_DIR="$(cd "$HOME_DIR" && pwd)"
echo "仓库：    $REPO"
echo "安装根：  $HOME_DIR"

# ── 1. 编译 Go 二进制（零 CGO）───────────────────────────
if [ "$DO_BUILD" -eq 1 ]; then
  need go
  step "编译二进制（fka / fka-memory / fka-sandbox）"
  make -C "$REPO" build
  # make 把产物写到 <仓库>/bin；安装根不同就搬过去。
  if [ "$HOME_DIR" != "$REPO/bin" ]; then
    cp "$REPO/bin/fka" "$REPO/bin/fka-memory" "$REPO/bin/fka-sandbox" "$HOME_DIR/"
  fi
else
  step "跳过编译（--no-build）"
  for b in fka fka-memory fka-sandbox; do
    if [ ! -x "$HOME_DIR/$b" ] && [ ! -x "$REPO/bin/$b" ]; then
      die "没有可用的 $b 二进制（去掉 --no-build，或先 make build）"
    fi
  done
  if [ "$HOME_DIR" != "$REPO/bin" ]; then
    for b in fka fka-memory fka-sandbox; do
      [ -x "$HOME_DIR/$b" ] || cp "$REPO/bin/$b" "$HOME_DIR/"
    done
  fi
fi

# ── 2. 沙盒容器（镜像里不编译，宿主编译 + COPY）───────────
if [ "$DO_DOCKER" -eq 1 ]; then
  if command -v docker >/dev/null 2>&1; then
    step "构建沙盒镜像 fka-sandbox:latest"
    make -C "$REPO" docker-sandbox
    step "启动沙盒容器"
    # compose 有两种形态：v2 插件 `docker compose` 与 v1 独立命令 `docker-compose`。
    # 两者都没有时**降级跳过**而不是失败——照「可选能力缺失不阻断」的规矩，其余
    # 安装照常完成；sandbox 工具暂时连不上，补齐 compose 后重跑本脚本即可。
    if docker compose version >/dev/null 2>&1; then
      COMPOSE=(docker compose)
    elif command -v docker-compose >/dev/null 2>&1; then
      COMPOSE=(docker-compose)
    else
      COMPOSE=()
    fi
    if [ "${#COMPOSE[@]}" -gt 0 ]; then
      # 宿主端口优先级：显式设的 SANDBOX_MCP_PORT > 上次 mcp.json 里记的端口 > 从
      # 8080 起找空闲的。**必须复用上次记的端口**，否则重跑时会把正在运行的容器
      # 占着的端口当成「被占用」而一路前移，每次重跑都换端口并重建容器。
      if [ -z "${SANDBOX_MCP_PORT:-}" ] && [ -f "$HOME_DIR/mcp.json" ]; then
        SANDBOX_MCP_PORT="$(grep -o '127\.0\.0\.1:[0-9]*' "$HOME_DIR/mcp.json" 2>/dev/null | head -1 | cut -d: -f2 || true)"
      fi
      if [ -z "${SANDBOX_MCP_PORT:-}" ]; then
        SANDBOX_MCP_PORT=8080
        while (exec 3<>"/dev/tcp/127.0.0.1/$SANDBOX_MCP_PORT") 2>/dev/null; do
          SANDBOX_MCP_PORT=$((SANDBOX_MCP_PORT + 1))
          [ "$SANDBOX_MCP_PORT" -gt 8180 ] && { SANDBOX_MCP_PORT=8080; break; }
        done
      fi
      echo "沙盒容器宿主端口：$SANDBOX_MCP_PORT"
      # 先 down 再 up：老的 docker-compose v1 在「重建已有容器」时会读镜像的
      # ContainerConfig 来合并卷绑定，而新版 Docker 已不下发该字段，重建必崩
      # （KeyError: 'ContainerConfig'）。先移除容器让它走「新建」路径即可绕开；
      # 数据在命名卷里，down 不带 -v 不会丢。
      SANDBOX_MCP_PORT="$SANDBOX_MCP_PORT" \
        "${COMPOSE[@]}" -f "$REPO/mcp/mcp_sandbox/docker-compose.yml" down >/dev/null 2>&1 || true
      SANDBOX_MCP_PORT="$SANDBOX_MCP_PORT" \
        "${COMPOSE[@]}" -f "$REPO/mcp/mcp_sandbox/docker-compose.yml" up -d
    else
      warn "没装 docker compose（v2 插件或 v1 docker-compose 都没有），跳过启动沙盒容器；sandbox 工具将连不上（补齐后重跑本脚本即可）"
    fi
  else
    warn "没装 docker，跳过沙盒容器；sandbox 工具将连不上（补齐后再跑一次本脚本即可）"
  fi
else
  step "跳过沙盒容器（--no-docker）"
fi

# 没走到 docker 分支时（--no-docker / 没装 docker），mcp.json 仍按默认端口写 URL。
SANDBOX_MCP_PORT="${SANDBOX_MCP_PORT:-8080}"

# ── 3. .env（含密钥，默认不覆盖）────────────────────────
ENV_FILE="$HOME_DIR/.env"
step "写 $ENV_FILE"
if [ -f "$ENV_FILE" ] && [ "$FORCE" -ne 1 ]; then
  echo "已存在，保留原样（要覆盖加 --force）"
else
  {
    cat <<EOF
# 日志输出目录
LOG_DIR=./logs

# 语言模型
LLM_BASE_URL=${LLM_BASE_URL:-https://api.deepseek.com/v1}
LLM_API_KEY=${LLM_API_KEY:-sk-请替换成你的key}
LLM_MODEL=${LLM_MODEL:-deepseek-flash}

# 工具放行：MCP 工具一律是 external，不放行就一个都看不到
LLM_TOOL_EFFECTS=read,send,external
LLM_CONTEXT_TOKENS=0
LLM_MAX_STEPS=999
EOF
    # 微信渠道：**默认不接**，加 --ilink 且给了 BOT_TOKEN 才写进去（缺 token 起 serve
    # 会因为空账号报错，所以两者都要满足）。
    if [ "$DO_ILINK" -eq 1 ] && [ -n "${ILINK_ACCOUNT_1_BOT_TOKEN:-}" ]; then
      cat <<EOF

# 微信 iLink（_ID 是内部槽位标识，固定 account_001）
ILINK_ACCOUNT_1_ID=account_001
ILINK_ACCOUNT_1_BOT_TOKEN=${ILINK_ACCOUNT_1_BOT_TOKEN}
ILINK_ACCOUNT_1_BASE_URL=${ILINK_ACCOUNT_1_BASE_URL:-https://ilinkai.weixin.qq.com}
ILINK_ACCOUNT_1_BOT_ID=${ILINK_ACCOUNT_1_BOT_ID:-}
ILINK_ACCOUNT_1_USER_ID=${ILINK_ACCOUNT_1_USER_ID:-}
EOF
    else
      echo
      echo "# 微信 iLink：默认不接。要接就设 ILINK_ACCOUNT_1_BOT_TOKEN 并加 --ilink 重跑。"
    fi
    # 网页渠道：WEB_CHANNEL_ADDR 为空则不写（= 不接 web）。
    if [ -n "${WEB_CHANNEL_ADDR:-127.0.0.1:8787}" ]; then
      cat <<EOF

# 网页渠道（serve 模式）——设了 WEB_CHANNEL_ADDR 才启用
WEB_CHANNEL_ADDR=${WEB_CHANNEL_ADDR:-127.0.0.1:8787}
# 用户表默认读 <安装根>/web-users.json（本脚本会生成一份示例）
#WEB_CHANNEL_USERS=web-users.json
#WEB_CHANNEL_ACCOUNT=default
#WEB_CHANNEL_STATIC=web
EOF
    fi
  } > "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  echo "已写入"
fi

# ── 4. mcp.json（无密钥，直接覆盖，先备份旧的）───────────
MCP_FILE="$HOME_DIR/mcp.json"
step "写 $MCP_FILE"
if [ -f "$MCP_FILE" ]; then cp "$MCP_FILE" "$MCP_FILE.bak"; fi
cat > "$MCP_FILE" <<EOF
{
  "mcpServers": {
    "sandbox": { "url": "http://127.0.0.1:$SANDBOX_MCP_PORT/mcp", "transport": "http" },
    "memory": {
      "command": "$HOME_DIR/fka-memory",
      "args": ["--db", "memory.sqlite"],
      "cwd": "$HOME_DIR"
    },
    "sqlite": {
      "command": "node",
      "args": ["$HOME_DIR/mcp_sqlite/server.mjs"],
      "cwd": "$HOME_DIR"
    },
    "time-demo": {
      "command": "node",
      "args": ["$HOME_DIR/mcp_time/server.mjs"],
      "cwd": "$HOME_DIR"
    }
  }
}
EOF
echo "已写入"

# ── 5. web 前端 + 用户表 ────────────────────────────────
step "装 web 前端与网页用户表"
mkdir -p "$HOME_DIR/web"
cp "$REPO/web/"* "$HOME_DIR/web/"
if [ ! -f "$HOME_DIR/web-users.json" ]; then
  WEB_USER="${WEB_CHANNEL_USER:-admin}"
  WEB_TOKEN="${WEB_CHANNEL_TOKEN:-$(rand_token)}"
  cat > "$HOME_DIR/web-users.json" <<EOF
{
  "users": [
    { "token": "$WEB_TOKEN", "user": "$WEB_USER" }
  ]
}
EOF
  chmod 600 "$HOME_DIR/web-users.json"
  echo "已生成 $HOME_DIR/web-users.json"
  echo "  网页登录 → 用户：$WEB_USER   令牌：$WEB_TOKEN"
else
  echo "已存在 web-users.json，保留（用户 / 令牌见该文件）"
fi

# ── 6. 两个 Node MCP server ────────────────────────────
step "装 Node MCP server（mcp_time / mcp_sqlite）"
for name in mcp_time mcp_sqlite; do
  src="$REPO/mcp/$name"
  [ -d "$src" ] || { warn "仓库里没有 ${src}，跳过"; continue; }
  dst="$HOME_DIR/$name"
  mkdir -p "$dst"
  # 源码 + 锁文件 + （若已在仓库装好的）node_modules 一起拷过去
  cp -R "$src/." "$dst/"
  if [ ! -d "$dst/node_modules" ]; then
    if command -v npm >/dev/null 2>&1; then
      echo "  ${name}：node_modules 缺失，npm ci…"
      ( cd "$dst" && npm ci --silent ) || warn "$name 的 npm ci 失败（离线？），该 server 可能起不来"
    else
      warn "$name 没有 node_modules 且本机没有 npm"
    fi
  fi
  echo "  ✓ $name"
done

# ── 7. skills 目录 ────────────────────────────────────
mkdir -p "$HOME_DIR/skills"   # 空的技能目录是正常状态

# ── 完成 ───────────────────────────────────────────────
step "完成"
cat <<EOF
安装根：$HOME_DIR

启动常驻（接渠道、收消息）：
  FKA_HOME=$HOME_DIR $HOME_DIR/fka serve
  # 或： cd $HOME_DIR && ./fka serve

看一眼模型现在能看到哪些工具：
  FKA_HOME=$HOME_DIR $HOME_DIR/fka tools

说明：
- sandbox 工具走容器 http://127.0.0.1:$SANDBOX_MCP_PORT/mcp，容器没起时会被跳过并 WARN
  （补跑： docker compose -f $REPO/mcp/mcp_sandbox/docker-compose.yml up -d
    或本机只有 v1 时： docker-compose -f $REPO/mcp/mcp_sandbox/docker-compose.yml up -d）
- .env 里 LLM_API_KEY 若是占位值，先改成真实 key
- 直接用 ./fka 时的安装根就是 ${HOME_DIR}，配置也读这里
EOF
