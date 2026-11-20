#!/bin/sh
# lightmem-watchdog.sh —— 应用级探活看门狗（R29-T0 P0-A；与 rc.d 的 daemon -R/-C 自愈**互补**，不替代）
#
# 作用：探 /readyz（200=就绪），未就绪则重启**本服务**（一次只重启 $LIGHTMEM_WATCHDOG_SERVICE），
#       冷却文件防抖（默认 120s），事件经 logger / 日志文件落盘。
#
# 为什么需要：rc.d 的 daemon -R/-C 只处理「进程崩溃/退出」；若进程活着但后端
#       连不上（/readyz 503），daemon 不会重启。看门狗补这个盲区。
#
# 跨平台（T-0412）：
#   * 重启动作走 deploy/platform.sh 的 lm_service_restart：linux→systemctl restart，
#     freebsd→service restart；不再硬依赖 FreeBSD 的 service 兼容层。
#   * 探活回退链：curl → wget -q -O /dev/null →（仅 freebsd）fetch。
#     Linux 上只有 wget 也能工作。
#   * 冷却文件目录按平台选：linux /run/lightmem（不可写则 /var/lib/lightmem）；
#     freebsd /var/run/lightmem。用 /run 而非 /var/run，避开符号链接差异。
#
# 安装（FreeBSD 真机）：
#   install -m 555 deploy/lightmem-watchdog.sh /usr/local/bin/lightmem-watchdog.sh
#   install -m 644 deploy/platform.sh /usr/local/libexec/lightmem/platform.sh
#   再把 deploy/cron.d/lightmem-watchdog 装进 /etc/cron.d/（或 /etc/crontab 追加一行）。
#
# 安装（Linux，二选一）：
#   A) systemd timer（推荐，见同目录 lightmem-watchdog.service/.timer）：
#      install -m 555 deploy/lightmem-watchdog.sh /usr/local/bin/lightmem-watchdog.sh
#      install -m 644 deploy/platform.sh /usr/local/libexec/lightmem/platform.sh
#      install -m 644 deploy/lightmem-watchdog.service deploy/lightmem-watchdog.timer /etc/systemd/system/
#      systemctl daemon-reload && systemctl enable --now lightmem-watchdog.timer
#   B) cron（无 systemd 的容器/老旧发行版）：
#      install -m 644 deploy/cron.d/lightmem-watchdog /etc/cron.d/lightmem-watchdog
#
# 环境变量：
#   LIGHTMEM_WATCHDOG_SERVICE   服务名，默认 lightmem
#   LIGHTMEM_READYZ_URL         就绪探针 URL；默认按平台：linux 7443、freebsd 18443
#   LIGHTMEM_WATCHDOG_COOLDOWN  冷却秒数，默认 120
#   LIGHTMEM_WATCHDOG_RUNDIR    冷却文件目录覆盖（测试/非常规布局）
#   LIGHTMEM_WATCHDOG_LOG       事件日志，默认 /var/log/lightmem-watchdog.log
#   LIGHTMEM_PLATFORM_SH        platform.sh 路径覆盖
#   LM_DRY_RUN=1                只打印计划：不重启、不写冷却文件/日志
#   LM_FAKE_OS=linux|freebsd    测试钩子：强制平台分支（见 deploy/platform.sh）
#
# 红线：只读探活 + 重启本服务，不改任何配置/数据，不误杀其它实例。
set -u

SERVICE="${LIGHTMEM_WATCHDOG_SERVICE:-lightmem}"
COOLDOWN="${LIGHTMEM_WATCHDOG_COOLDOWN:-120}"
case "$COOLDOWN" in ''|*[!0-9]*) COOLDOWN=120 ;; esac
LOG_FILE="${LIGHTMEM_WATCHDOG_LOG:-/var/log/lightmem-watchdog.log}"

LM_WD_DIR=$(dirname "$0")
LM_WD_UNAME=$(uname -s 2>/dev/null) || LM_WD_UNAME=unknown
LM_WD_PLATFORM=""
PROBE_CODE=""

lm_wd_say() { printf '%s\n' "$*"; }

lm_wd_is_dry() {
    case "${LM_DRY_RUN:-0}" in
        1|true|yes|on) return 0 ;;
        *) return 1 ;;
    esac
}

# 事件落点：dry-run 只打印；真实运行优先 logger，其次追加日志文件。
lm_wd_event() {
    if lm_wd_is_dry; then
        lm_wd_say "[dry-run] $*"
        return 0
    fi
    if command -v logger >/dev/null 2>&1; then
        logger -t lightmem-watchdog "$*" 2>/dev/null || true
    fi
    lm_wd_ts=$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null) || lm_wd_ts="unknown-time"
    if [ -w "$(dirname "$LOG_FILE")" ]; then
        printf '%s %s\n' "$lm_wd_ts" "$*" >> "$LOG_FILE" 2>/dev/null || true
    fi
    return 0
}

# 找 platform.sh（T-0410 冻结接口）：显式指定 → 脚本同目录 → deploy/ → 安装位置。
lm_wd_load_platform() {
    for lm_wd_p in \
        "${LIGHTMEM_PLATFORM_SH:-}" \
        "$LM_WD_DIR/platform.sh" \
        "$LM_WD_DIR/../deploy/platform.sh" \
        /usr/local/libexec/lightmem/platform.sh \
        /opt/lightmem/platform.sh \
        /usr/local/lightmem/platform.sh
    do
        if [ -n "$lm_wd_p" ] && [ -f "$lm_wd_p" ]; then
            # shellcheck disable=SC1090
            . "$lm_wd_p"
            LM_WD_PLATFORM=$lm_wd_p
            return 0
        fi
    done
    return 1
}

if lm_wd_load_platform; then
    :
else
    # 兜底：单独拷走看门狗（没带 platform.sh）时仍可工作，语义与 platform.sh 对齐。
    printf '%s\n' "lightmem-watchdog: 未找到 platform.sh（可用 LIGHTMEM_PLATFORM_SH 指定），启用内置兜底" >&2
    lm_detect_os() {
        case "$LM_WD_UNAME" in
            FreeBSD*) lm_wd_say freebsd ;;
            Linux*)   lm_wd_say linux ;;
            *)        return 1 ;;
        esac
    }
    lm_prefix() {
        lm_fb_pfx_os=$(lm_detect_os) || return 1
        case "$lm_fb_pfx_os" in
            linux)   lm_wd_say /opt/lightmem ;;
            freebsd) lm_wd_say /usr/local/lightmem ;;
        esac
    }
    lm_service_restart() {
        if [ $# -lt 1 ] || [ -z "$1" ]; then
            printf '%s\n' "lm_service_restart: 用法: lm_service_restart <service-name>" >&2
            return 2
        fi
        lm_fb_name=$1
        lm_fb_os=$(lm_detect_os) || return 1
        if [ "$lm_fb_os" = "linux" ]; then
            lm_fb_cmd="systemctl restart $lm_fb_name"
        else
            lm_fb_cmd="service $lm_fb_name restart"
        fi
        if lm_wd_is_dry; then
            lm_wd_say "[dry-run] $lm_fb_cmd"
            return 0
        fi
        if [ "$lm_fb_os" = "linux" ]; then
            systemctl restart "$lm_fb_name"
        else
            service "$lm_fb_name" restart
        fi
    }
fi

# 默认探针 URL 按端口口径分开：linux systemd/quickstart=7443，freebsd rc.d=18443，不混用。
if [ -n "${LIGHTMEM_READYZ_URL:-}" ]; then
    READYZ_URL=$LIGHTMEM_READYZ_URL
else
    lm_wd_url_os=$(lm_detect_os 2>/dev/null) || lm_wd_url_os=""
    case "$lm_wd_url_os" in
        linux) READYZ_URL="http://127.0.0.1:7443/readyz" ;;
        *)     READYZ_URL="http://127.0.0.1:18443/readyz" ;;
    esac
fi

# 冷却目录：linux /run/lightmem（不可写则 /var/lib/lightmem）；freebsd /var/run/lightmem。
lm_wd_rundir() {
    if [ -n "${LIGHTMEM_WATCHDOG_RUNDIR:-}" ]; then
        lm_wd_say "$LIGHTMEM_WATCHDOG_RUNDIR"
        return 0
    fi
    lm_rd_os=$(lm_detect_os 2>/dev/null) || lm_rd_os=""
    case "$lm_rd_os" in
        linux)   lm_wd_say /run/lightmem ;;
        freebsd) lm_wd_say /var/run/lightmem ;;
        *)
            lm_rd_pfx=$(lm_prefix 2>/dev/null) || lm_rd_pfx=""
            if [ -n "$lm_rd_pfx" ]; then
                lm_wd_say "$lm_rd_pfx/run"
            else
                lm_wd_say "${TMPDIR:-/tmp}/lightmem-watchdog"
            fi
            ;;
    esac
}

RUNDIR=$(lm_wd_rundir)
COOLDOWN_FILE="$RUNDIR/watchdog.cooldown"

if ! lm_wd_is_dry; then
    if ! mkdir -p "$RUNDIR" 2>/dev/null; then
        # /run 被只读挂载或权限不足时退到持久目录（仅 Linux）
        case "$(lm_detect_os 2>/dev/null)" in
            linux)
                RUNDIR=/var/lib/lightmem
                COOLDOWN_FILE="$RUNDIR/watchdog.cooldown"
                mkdir -p "$RUNDIR" 2>/dev/null || true
                ;;
        esac
    fi
fi

now=$(date +%s 2>/dev/null) || now=0
if [ -f "$COOLDOWN_FILE" ]; then
    lm_wd_last=$(cat "$COOLDOWN_FILE" 2>/dev/null || echo 0)
    case "$lm_wd_last" in ''|*[!0-9]*) lm_wd_last=0 ;; esac
    if [ "$now" -gt 0 ] && [ "$((now - lm_wd_last))" -lt "$COOLDOWN" ]; then
        lm_wd_is_dry && lm_wd_say "[dry-run] 冷却期内（距上次重启 $((now - lm_wd_last))s，阈值 ${COOLDOWN}s），跳过探活与重启"
        exit 0
    fi
fi

# 探 /readyz（免鉴权，见 kernel/auth.go 豁免）。200=ready，其它=未就绪。
# 返回 0=ready，1=not-ready，2=没有任何可用探活工具。
lm_wd_probe() {
    PROBE_CODE=""
    lm_pb_os=$(lm_detect_os 2>/dev/null) || lm_pb_os=""
    if command -v curl >/dev/null 2>&1; then
        PROBE_CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$READYZ_URL" 2>/dev/null) || PROBE_CODE=000
        [ -n "$PROBE_CODE" ] || PROBE_CODE=000
        if [ "$PROBE_CODE" = "200" ]; then
            return 0
        fi
        return 1
    fi
    if command -v wget >/dev/null 2>&1; then
        if wget -q -T 5 -t 1 -O /dev/null "$READYZ_URL" >/dev/null 2>&1; then
            PROBE_CODE=200
            return 0
        fi
        PROBE_CODE=wget-fail
        return 1
    fi
    if [ "$lm_pb_os" = "freebsd" ] && command -v fetch >/dev/null 2>&1; then
        if fetch -q -T 5 -o /dev/null "$READYZ_URL" >/dev/null 2>&1; then
            PROBE_CODE=200
            return 0
        fi
        PROBE_CODE=fetch-fail
        return 1
    fi
    PROBE_CODE=none
    return 2
}

lm_wd_probe
probe_rc=$?
if lm_wd_is_dry; then
    lm_wd_say "[dry-run] 探活 $READYZ_URL -> rc=$probe_rc code=${PROBE_CODE:-unknown}"
fi

if [ "$probe_rc" = "2" ]; then
    # 没有任何探活工具时不做盲重启（避免把「工具缺失」误判成「服务挂了」）。
    lm_wd_event "没有 curl/wget/fetch 可用，跳过探活与重启（请装 curl 或 wget）"
    exit 0
fi

if [ "$probe_rc" != "0" ]; then
    lm_wd_event "readyz 未就绪（code=${PROBE_CODE:-unknown}），重启 $SERVICE"
    if lm_wd_is_dry; then
        if [ -n "$(lm_detect_os 2>/dev/null)" ]; then
            lm_service_restart "$SERVICE"
        else
            lm_wd_say "[dry-run] OS 未识别（uname: $LM_WD_UNAME）；真实部署时由 platform.sh 选择 systemctl/service 重启 $SERVICE"
        fi
    else
        if lm_service_restart "$SERVICE"; then
            lm_wd_event "已重启 $SERVICE"
            date +%s > "$COOLDOWN_FILE" 2>/dev/null || true
        else
            lm_wd_event "重启 $SERVICE 失败（lm_service_restart 非零退出；检查服务名与权限）"
        fi
    fi
fi

exit 0
