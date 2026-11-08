#!/usr/bin/env bash
# 清理运行时产物（会话、日志、dump）。
#
# 教训：直接 `rm logs/*.log` 会把**正在运行**进程的日志文件删掉——进程仍持有已
# unlink 的句柄，继续往里写，但外部再也看不到（2026-11-08 就这么丢过一份）。
# 所以这里先问 lsof：被进程打开的文件一律跳过，并给出提示。
set -euo pipefail

cd "$(dirname "$0")/.."
root="$PWD"
keep_state="${KEEP_STATE:-1}"

echo "仓库：$root"

# 收集正在被 code-review-agent 打开的文件
busy_files=""
if command -v lsof >/dev/null 2>&1; then
  for pid in $(pgrep -f "code-review-agent" || true); do
    busy_files+=$'\n'$(lsof -p "$pid" 2>/dev/null | awk 'NR>1 && $NF ~ /^\// {print $NF}' || true)
  done
fi

is_busy() {
  [ -n "$busy_files" ] || return 1
  printf '%s\n' "$busy_files" | grep -Fxq "$1"
}

removed=0
skipped=0

if [ -d sessions ]; then
  for file in sessions/*.json; do
    [ -e "$file" ] || continue
    absolute="$root/$file"
    if is_busy "$absolute"; then
      echo "跳过（进程正在写）：$file"
      skipped=$((skipped+1))
      continue
    fi
    rm -f "$file"
    removed=$((removed+1))
  done
fi

if [ -d logs ]; then
  for file in logs/*.log logs/dumps/*.json; do
    [ -e "$file" ] || continue
    absolute="$root/$file"
    if is_busy "$absolute"; then
      echo "跳过（进程正在写）：$file"
      skipped=$((skipped+1))
      continue
    fi
    rm -f "$file"
    removed=$((removed+1))
  done
fi
mkdir -p logs/dumps

if [ "$keep_state" = "1" ]; then
  echo "保留 state/（selection/quota/concurrency 是运行时学习值）"
else
  rm -f state/selection.json state/quota.json state/concurrency.json
  echo "已删除 state/"
fi

echo "完成：删除 $removed 个文件，跳过 $skipped 个正在使用的文件。"
if [ "$skipped" != "0" ]; then
  echo "提示：如需清理被占用的日志，先退出程序再运行本脚本。"
fi
