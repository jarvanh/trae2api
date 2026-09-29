#!/usr/bin/env bash
# signin.sh — 全账号批量签到（错峰排程 + 9074 自愈换代）
#
# 用法:
#   ./signin.sh                     # 默认 auths/ + data/state.json
#   ./signin.sh /path/to/auths      # 指定 auths 目录
#
# 早期版本引用 ./cmd/signin（本仓库从未存在），导致脚本必然失败。
# 真正的签到器是 cmd/checkin，参数与调度器同源。
# 二进制升级: go build -o checkin_bin ./cmd/checkin
set -euo pipefail
cd "$(dirname "$0")"

AUTHS_DIR="${1:-auths}"
STATE="data/state.json"
# auths 与 data 在同一级目录时自动带上 state（与容器挂载布局一致）
if [[ ! -f "$STATE" && -f "$(dirname "$AUTHS_DIR")/data/state.json" ]]; then
  STATE="$(dirname "$AUTHS_DIR")/data/state.json"
fi

BIN=./checkin_bin
if [ ! -x "$BIN" ]; then
    echo "build checkin_bin ..."
    go build -o "$BIN" ./cmd/checkin
fi

exec "$BIN" -dir "$AUTHS_DIR" -state "$STATE"
