#!/usr/bin/env bash
#
# 一键混淆音乐商店示例库：跑 `mcp/mcp_sqlite/obfuscate.mjs` 生成「混淆库 + sqlite.sql +
# 技能字典」，并把这三样产物同步进安装根（默认 <仓库>/bin），省得手抄。
#
# 用法：
#   scripts/obfuscate.sh [选项]
#
# 选项：
#   --seed VALUE   混淆种子（默认 chinook-obf；换它则整份产物一致重生）
#   --no-bin       只生成，不同步进安装根
#   -h, --help     这份帮助
#
# 其余参数（--in / --out / --sql / --skill / --map）原样透传给 obfuscate.mjs。
#
# **不自动重启服务**：生成只改磁盘文件，正在跑的 `fka serve` 在它自己的 MCP 子进程里
# 已经打开了旧库，必须重启才会读到新库/新技能。脚本末尾只提醒一句——在这里替你 kill
# 会把某个终端里的前台进程杀掉，那是调用方该决定的事。
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOME_DIR="${FKA_HOME:-$REPO/bin}"
SYNC_BIN=1
SEED=""
PASSTHRU=()

while [ $# -gt 0 ]; do
  case "$1" in
    --seed)    SEED="${2:?--seed 需要一个值}"; shift 2 ;;
    --seed=*)  SEED="${1#*=}"; shift ;;
    --no-bin)  SYNC_BIN=0; shift ;;
    -h|--help) sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)         PASSTHRU+=("$1"); shift ;;
  esac
done

step() { printf '\n\033[1m==> %s\033[0m\n' "$1"; }
die()  { printf '\033[31m[错误] %s\033[0m\n' "$1" >&2; exit 1; }

command -v node >/dev/null 2>&1 || die "缺少 node（obfuscate.mjs 依赖 node:sqlite）"
[ -f "$REPO/mcp/mcp_sqlite/obfuscate.mjs" ] || die "找不到 mcp/mcp_sqlite/obfuscate.mjs"
[ -f "$REPO/mcp/mcp_sqlite/chinook.original.db" ] || die "找不到 chinook.original.db（脚本输入；原始库）"

step "生成混淆库 / sqlite.sql / 技能字典"
ARGS=()
[ -n "$SEED" ] && ARGS+=(--seed "$SEED")
# bash 3.2 + set -u 下，空数组展开会报未绑定变量，所以要加 ${arr[@]+...} 守卫
ARGS+=(${PASSTHRU[@]+"${PASSTHRU[@]}"})
node "$REPO/mcp/mcp_sqlite/obfuscate.mjs" ${ARGS[@]+"${ARGS[@]}"}

if [ "$SYNC_BIN" -eq 1 ]; then
  if [ -d "$HOME_DIR/mcp_sqlite" ]; then
    step "同步进安装根 $HOME_DIR"
    cp "$REPO/mcp/mcp_sqlite/chinook.db" "$HOME_DIR/mcp_sqlite/chinook.db"
    cp "$REPO/mcp/mcp_sqlite/sqlite.sql" "$HOME_DIR/mcp_sqlite/sqlite.sql"
    mkdir -p "$HOME_DIR/skills/music-catalog-analytics"
    cp "$REPO/skills/music-catalog-analytics/SKILL.md" "$HOME_DIR/skills/music-catalog-analytics/SKILL.md"
    printf '  已同步 chinook.db / sqlite.sql / SKILL.md\n'
  else
    printf '\033[33m[跳过]\033[0m %s/mcp_sqlite 不存在，未同步进安装根（用 --no-bin 可跳过此提示）\n' "$HOME_DIR"
  fi
fi

printf '\n\033[32m完成。\033[0m 若 %s 下有正在跑的 fka serve，需重启它才会读到新库 / 新技能。\n' "$HOME_DIR"
