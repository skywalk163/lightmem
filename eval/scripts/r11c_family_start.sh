#!/usr/bin/env bash
# R11 C.3 — Family 档真机启动脚本
#
# 对应任务：R11 C 路 — 生产部署硬化
# 对应文档：eval/reports/r11c_family_real_20261014.md
# 适用平台：FreeBSD 15.1-RELEASE-p1（0.88 实机）
# 执行用户：workbuddy（wheel 组，sudo NOPASSWD）
#
# 用法：
#   bash r11c_family_start.sh [start|stop|status|smoke]
#
# 红线：
#   - 不靠 override 启动，走真 PG+pgvector+jail
#   - 环境变量覆盖部署实例属性，不改 profile 真源

set -euo pipefail

# ---- 配置 ----
LIGHTMEM_DIR="${LIGHTMEM_DIR:-/tmp/lightmem}"
PROFILE="${PROFILE:-family}"
LISTEN_ADDR="${LISTEN_ADDR:-127.0.0.1:8443}"
PID_FILE="/tmp/lightmem-kernel.pid"
LOG_FILE="/tmp/lightmem-kernel.log"

# 环境变量（部署实例属性，覆盖 profile 声明）
export LIMS_PG_HOST="${LIMS_PG_HOST:-10.0.10.10}"
export LIMS_PG_PORT="${LIMS_PG_PORT:-5432}"
export LIMS_PG_USER="${LIMS_PG_USER:-postgres}"
export LIMS_PG_DB="${LIMS_PG_DB:-lims}"
export LIMS_ZFS_POOL="${LIMS_ZFS_POOL:-zroot}"

# ---- 工具函数 ----
log() {
    echo "[r11c-family] $*"
}

err() {
    echo "[r11c-family] ERROR: $*" >&2
    exit 1
}

check_dir() {
    if [[ ! -d "${LIGHTMEM_DIR}" ]]; then
        err "目录 ${LIGHTMEM_DIR} 不存在，请先同步源码"
    fi
    if [[ ! -f "${LIGHTMEM_DIR}/lightmem-kernel" ]]; then
        err "文件 ${LIGHTMEM_DIR}/lightmem-kernel 不存在，请先编译"
    fi
}

# ---- 启动 ----
start() {
    log "启动 Family 档（profile=${PROFILE}, listen=${LISTEN_ADDR}）"
    check_dir

    # 检查是否已在运行
    if [[ -f "${PID_FILE}" ]]; then
        local pid=$(cat "${PID_FILE}")
        if kill -0 "${pid}" 2>/dev/null; then
            log "  ⚠ 已在运行（pid=${pid}）"
            return
        fi
    fi

    # 启动
    cd "${LIGHTMEM_DIR}"
    nohup ./lightmem-kernel \
        -profile profiles/lims-${PROFILE}.profile.yaml \
        -listen "${LISTEN_ADDR}" \
        >> "${LOG_FILE}" 2>&1 &
    local pid=$!
    echo "${pid}" > "${PID_FILE}"

    # 等待启动
sleep 3

    # 验证
    if curl -k -s "https://${LISTEN_ADDR}/health" | grep -q '"status":"ok"'; then
        log "  ✓ 启动成功（pid=${pid}）"
        log "  ✓ 健康检查：$(curl -k -s "https://${LISTEN_ADDR}/health")"
    else
        err "启动失败，请检查 ${LOG_FILE}"
    fi
}

# ---- 停止 ----
stop() {
    log "停止 Family 档"
    if [[ -f "${PID_FILE}" ]]; then
        local pid=$(cat "${PID_FILE}")
        kill "${pid}" 2>/dev/null || true
        rm -f "${PID_FILE}"
        log "  ✓ 已停止（pid=${pid}）"
    else
        log "  ⚠ 未运行"
    fi
}

# ---- 状态 ----
status() {
    echo "=== Family 档状态 ==="
    echo "LIGHTMEM_DIR:   ${LIGHTMEM_DIR}"
    echo "PROFILE:        ${PROFILE}"
    echo "LISTEN_ADDR:    ${LISTEN_ADDR}"
    echo "LIMS_PG_HOST:   ${LIMS_PG_HOST}"
    echo "LIMS_PG_PORT:   ${LIMS_PG_PORT}"
    echo "LIMS_ZFS_POOL:  ${LIMS_ZFS_POOL}"
    echo ""
    if [[ -f "${PID_FILE}" ]]; then
        local pid=$(cat "${PID_FILE}")
        if kill -0 "${pid}" 2>/dev/null; then
            echo "运行中: pid=${pid}"
            echo "健康检查:"
            curl -k -s "https://${LISTEN_ADDR}/health" 2>/dev/null || echo "  无法连接"
        else
            echo "已停止（pid 文件残留）"
        fi
    else
        echo "未运行"
    fi
}

# ---- 冒烟测试 ----
smoke() {
    log "执行冒烟测试"

    # 1) 健康检查
    log "  1) /health"
    local health=$(curl -k -s "https://${LISTEN_ADDR}/health" 2>/dev/null || echo "FAILED")
    echo "     结果: ${health}"
    if [[ "${health}" != *'"status":"ok"'* ]]; then
        err "健康检查失败"
    fi

    # 2) 写入记忆
    log "  2) POST /memories"
    local mem_id="01HKPG$(date +%s)"
    local resp=$(curl -k -s -X POST "https://${LISTEN_ADDR}/memories" \
        -H 'Content-Type: application/json' \
        -d "{\"memory_id\":\"${mem_id}\",\"tenant_id\":\"family-1\",\"user_id\":\"alice\",\"memory_kind\":\"fact\",\"fact\":\"测试记忆\",\"confidence\":0.95,\"visibility\":\"shared_user\",\"permission\":{\"owner\":\"alice\",\"readers\":[\"alice\"]},\"source\":\"test\"}" 2>/dev/null || echo "FAILED")
    echo "     结果: ${resp}"

    # 3) 文本检索
    log "  3) GET /search?q=测试"
    local search=$(curl -k -s "https://${LISTEN_ADDR}/search?q=测试&tenant_id=family-1&viewer=alice" 2>/dev/null || echo "FAILED")
    echo "     结果: ${search}"

    # 4) 向量补全
    log "  4) POST /vector/backfill"
    local backfill=$(curl -k -s -X POST "https://${LISTEN_ADDR}/vector/backfill" 2>/dev/null || echo "FAILED")
    echo "     结果: ${backfill}"

    # 5) 向量检索
    log "  5) POST /search/vector"
    local vsearch=$(curl -k -s -X POST "https://${LISTEN_ADDR}/search/vector" \
        -H 'Content-Type: application/json' \
        -d '{"query":"测试","tenant_id":"family-1","viewer":"alice","top_k":5}' 2>/dev/null || echo "FAILED")
    echo "     结果: ${vsearch}"

    log "冒烟测试完成"
}

# ---- 主入口 ----
main() {
    local sub="${1:-status}"

    case "$sub" in
        start)
            start
            ;;
        stop)
            stop
            ;;
        status)
            status
            ;;
        smoke)
            smoke
            ;;
        restart)
            stop
            sleep 1
            start
            ;;
        *)
            echo "Usage: $0 {start|stop|status|smoke|restart}" >&2
            exit 1
            ;;
    esac
}

main "$@"
