#!/bin/sh
# ============================================================================
# check-eol-lf.sh —— deploy/ 部署文件 EOL 门禁（T-0427，T-0417 TODO#4）
# ----------------------------------------------------------------------------
# 背景：deploy/ 下 systemd unit / cron 条目 / logrotate 与 newsyslog 规则，
#       行尾带 \r（CRLF）或 logrotate 配置缺末行换行，都会让对应解析器
#       直接失败且报错隐晦（cron 静默不跑、systemd ExecStart 起不来、
#       logrotate stat 报错——T-0417 真机实测踩过）。
#       .gitattributes 已用路径级 `text eol=lf` 钉死这些文件（它们没有
#       常规扩展名，曾落在 * text=auto 兜底里，Windows 检出成 CRLF，
#       git add 时极易把 CRLF 提上去）；本脚本是可复跑的回归门禁：
#       以后再有人提 CRLF / 丢末行换行时立刻红。
# 检查（对 TARGETS 里每个文件）：
#   1) 不含 CR（0x0d）；
#   2) 末字节是 LF（0x0a）。
# 依赖：POSIX sh + tr/cmp/tail/od（Linux/FreeBSD/macOS/BusyBox 均自带）。
# 用法：sh scripts/check-eol-lf.sh    （全绿 exit 0，任一红 exit 1）
# 新增部署文件时同步两处：.gitattributes 的路径级规则 + 下方 TARGETS。
# ============================================================================
set -u

SELF_DIR=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$SELF_DIR/.." && pwd)
cd "$ROOT" || exit 1

PASS=0
FAIL=0

# 与 .gitattributes 路径级 eol=lf 规则一一对应的部署文件清单
TARGETS='
deploy/lightmem.service
deploy/lightmem-mcp.service
deploy/lightmem-watchdog.service
deploy/lightmem-watchdog.timer
deploy/cron.d/lightmem-watchdog
deploy/logrotate.d/lightmem
deploy/newsyslog.d/lightmem.conf
'

for f in $TARGETS; do
    if [ ! -f "$f" ]; then
        printf 'FAIL  %s 不存在（TARGETS 与 .gitattributes / 实际文件不同步）\n' "$f"
        FAIL=$((FAIL + 1))
        continue
    fi

    # 1) CR 检查：把所有 \r 删掉后与原文件逐字节相同 <=> 无 CR
    if tr -d '\r' < "$f" | cmp -s - "$f"; then
        PASS=$((PASS + 1))
    else
        printf 'FAIL  %s 含 CRLF（\\r 会让 systemd/cron/logrotate 解析失败）\n' "$f"
        FAIL=$((FAIL + 1))
    fi

    # 2) 末行换行检查：末字节必须是 0x0a
    #    （cron 的最后一行不带换行会被忽略；logrotate 曾实测解析失败）
    last=$(tail -c 1 "$f" | od -An -tx1 | tr -d ' \n')
    if [ "$last" = "0a" ]; then
        PASS=$((PASS + 1))
    else
        printf 'FAIL  %s 末字节是 0x%s，应为 0x0a（缺末行换行）\n' "$f" "$last"
        FAIL=$((FAIL + 1))
    fi
done

printf 'check-eol-lf: PASS=%d FAIL=%d\n' "$PASS" "$FAIL"
if [ "$FAIL" -ne 0 ]; then
    printf 'RESULT: FAIL exit=1\n'
    exit 1
fi
printf 'RESULT: PASS exit=0\n'
exit 0
