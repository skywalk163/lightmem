#!/bin/sh
# T-0412 验收辅助：用假 URL + dry-run 跑一次看门狗（不重启、不写冷却/日志）。
#
# 为什么单独成脚本：Hive 的 verify 在 Windows 上经 cmd.exe 执行，
# `LIGHTMEM_READYZ_URL=... LM_DRY_RUN=1 sh ...` 这种 POSIX 前缀 cmd 不认，
# 会把 `VAR=value` 当成命令名直接失败。把变量放进脚本里，cmd 只需 `sh <脚本>`。
#
# 用法（在仓库根执行）：sh deploy/verify-watchdog-dryrun.sh
set -u
cd "$(dirname "$0")/.." || exit 1

LIGHTMEM_READYZ_URL=http://127.0.0.1:1/readyz
LM_DRY_RUN=1
export LIGHTMEM_READYZ_URL LM_DRY_RUN

exec sh deploy/lightmem-watchdog.sh
