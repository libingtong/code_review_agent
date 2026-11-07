#!/usr/bin/env bash
# main 是唯一开发分支；master 只做它的快进镜像。
# 用法：./scripts/sync-master.sh   （必须在 main 上、工作区干净）
set -euo pipefail
cd "$(dirname "$0")/.."

branch=$(git rev-parse --abbrev-ref HEAD)
if [[ "$branch" != "main" ]]; then
  echo "请先切到 main（当前：$branch）。master 只接受来自 main 的快进。" >&2
  exit 1
fi
if [[ -n "$(git status --porcelain)" ]]; then
  echo "工作区不干净，先提交或 stash 再同步。" >&2
  exit 1
fi

git fetch --quiet origin
# 不带 + 的本地 fetch 会拒绝非快进更新，因此分叉会在这里被拦下。
git fetch --quiet . main:master
git push --quiet origin main master

echo "已同步："
printf '  main   %s\n' "$(git log --oneline -1 main)"
printf '  master %s\n' "$(git log --oneline -1 master)"
