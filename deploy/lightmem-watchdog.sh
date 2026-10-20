#!/bin/sh
# lightmem-watchdog.sh —— R29-T0 P0-A 看门狗（与 rc.d 的 daemon -R/-C 自愈**互补**，不替代）
#
# 作用：每 2 分钟探一次 /readyz，未就绪（非 200）则 `service lightmem restart`，
#       并写冷却文件防抖（120s 内不重复重启），事件经 logger 落系统日志。
#
# 为什么需要：rc.d 的 daemon -R/-C 只处理「进程崩溃/退出」；若进程活着但后端
#       连不上（/readyz 503），daemon 不会重启。看门狗补这个盲区。
#
# 安装：cp deploy/lightmem-watchdog.sh /usr/local/bin/ && chmod 555 /usr/local/bin/lightmem-watchdog.sh
#       再把 deploy/cron.d/lightmem-watchdog 装进 cron（每 2 分钟一条）。
#
# 红线：只读探活 + 重启，不改任何配置/数据；失败也只重启本服务，绝不误杀其它实例。
set -u

SERVICE="${LIGHTMEM_WATCHDOG_SERVICE:-lightmem}"
READYZ_URL="${LIGHTMEM_READYZ_URL:-http://127.0.0.1:18443/readyz}"
COOLDOWN="${LIGHTMEM_WATCHDOG_COOLDOWN:-120}"   # 秒；冷却期内不重复重启
RUNDIR="/var/run/lightmem"
COOLDOWN_FILE="$RUNDIR/watchdog.cooldown"
RESTART_LOG="/var/log/lightmem-watchdog.log"

mkdir -p "$RUNDIR" 2>/dev/null || true

now=$(date +%s)
if [ -f "$COOLDOWN_FILE" ]; then
    last=$(cat "$COOLDOWN_FILE" 2>/dev/null || echo 0)
    if [ "$((now - last))" -lt "$COOLDOWN" ]; then
        exit 0   # 冷却期内，跳过（防抖）
    fi
fi

# 探 /readyz（免鉴权，见 kernel/auth.go 豁免）。200=ready，其它=未就绪。
if command -v curl >/dev/null 2>&1; then
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$READYZ_URL" 2>/dev/null || echo 000)
    [ "$code" = "200" ] && ready=1 || ready=0
else
    if fetch -q -o /dev/null "$READYZ_URL" 2>/dev/null; then ready=1; else ready=0; fi
fi

if [ "${ready:-0}" = "0" ]; then
    logger -t lightmem-watchdog "readyz probe not-ready (code=${code:-unknown}), restarting $SERVICE"
    echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) restart trigger: readyz code=${code:-unknown}" >> "$RESTART_LOG"
    service "$SERVICE" restart
    date +%s > "$COOLDOWN_FILE"
fi
