#!/bin/bash
# xhttp-go 环境变量启动脚本
# UUID=... PORT=3000 XPATH=/xtp ./run.sh
set -e
: "${UUID:?UUID is required}"
PORT="${PORT:-3000}"
XPATH="${XPATH:-/xtp}"
XHOST="${XHOST:-127.0.0.1}"   # 默认仅本机（配合 Nginx/CDN 前置）；直连公网设 0.0.0.0，IPv6-only 设 ::
exec ./xhttp-go --uuid "$UUID" --port "$PORT" --path "$XPATH" --host "$XHOST"
