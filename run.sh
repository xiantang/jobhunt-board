#!/usr/bin/env bash
# 带上 .env 里的密钥启动服务。
# 用法：./run.sh                     直接跑一次
#       ./run.sh --watch             用 air 热重载（改 go/html/css/js 自动重启）
#       ./run.sh --exec CMD [ARG..]  只负责加载 .env 再 exec 指定命令（air 的 entrypoint 走这条）
set -euo pipefail
cd "$(dirname "$0")"
if [ -f .env ]; then
  set -a
  # shellcheck disable=SC1091
  source .env
  set +a
fi

case "${1:-}" in
  --watch|-w)
    shift
    air_bin=$(command -v air || echo "$(go env GOPATH)/bin/air")
    if [ ! -x "$air_bin" ]; then
      echo "没找到 air，先装一下：go install github.com/air-verse/air@latest" >&2
      exit 1
    fi
    exec "$air_bin" -c .air.toml -- "$@"
    ;;
  --exec)
    shift
    exec "$@"
    ;;
esac

exec go run ./cmd/server "$@"
