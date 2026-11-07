#!/usr/bin/env bash
# Build (if needed) and launch code-review-agent.
# Usage: ./run.sh [-config config.yaml] [-dir /path/to/audit]
set -euo pipefail
cd "$(dirname "$0")"

# Keep Go build/module caches inside the project so the app also builds in
# sandboxed environments where $HOME is not writable.
export GOCACHE="${GOCACHE:-$PWD/.gocache}"
export GOPATH="${GOPATH:-$PWD/.gopath}"
export GOTMPDIR="${GOTMPDIR:-$PWD/.gotmp}"
mkdir -p "$GOCACHE" "$GOPATH" "$GOTMPDIR"

# Optional local secrets file (gitignored, chmod 600). Example:
#   MINIMAX_API_KEY=sk-...
if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

if [[ -z "${MINIMAX_API_KEY:-}" ]]; then
  echo "提示: 未检测到 MINIMAX_API_KEY。程序界面仍会启动，但调用模型时会报缺少 API key。" >&2
  echo "      在 .env 中写入 MINIMAX_API_KEY=sk-... 后重新运行即可。" >&2
fi

VERSION="$(git rev-parse --short HEAD 2>/dev/null || echo dev)"
go build -ldflags "-X main.buildVersion=${VERSION}" -o code-review-agent .
exec ./code-review-agent "$@"
