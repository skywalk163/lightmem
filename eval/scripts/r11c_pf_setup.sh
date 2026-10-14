#!/usr/bin/env bash
# R11 C.2 — pf 防火墙配置脚本
#
# 对应任务：R11 C 路 — 生产部署硬化
# 对应文档：manuals/pf.rules
# 适用平台：FreeBSD 15.1-RELEASE-p1（0.88 实机）
# 执行用户：root 或 sudo 用户（wheel 组，NOPASSWD）
#
# 用法：
#   sudo bash r11c_pf_setup.sh [apply|backup|restore|status|test]
#
# 红线：
#   - 先备份 /etc/pf.conf，可回滚
#   - 先语法校验，再应用（避免锁死 SSH）

set -euo pipefail

# ---- 配置 ----
PF_CONF="/etc/pf.conf"
PF_BACKUP="${PF_CONF}.bak.$(date +%Y%m%d%H%M%S)"
RULES_SRC="$(dirname "$0")/../../manuals/pf.rules"

# ---- 工具函数 ----
log() {
    echo "[r11c-pf] $*"
}

err() {
    echo "[r11c-pf] ERROR: $*" >&2
    exit 1
}

check_root() {
    if [[ $(id -u) -ne 0 ]]; then
        err "请用 root 或 sudo 执行"
    fi
}

# ---- 备份 ----
backup() {
    if [[ -f "${PF_CONF}" ]]; then
        cp "${PF_CONF}" "${PF_BACKUP}"
        log "  ✓ 已备份 ${PF_CONF} → ${PF_BACKUP}"
    else
        log "  ⚠ ${PF_CONF} 不存在，跳过备份"
    fi
}

# ---- 语法校验 ----
test_rules() {
    log "语法校验（不应用）"
    if pfctl -vnf "${PF_CONF}" 2>&1; then
        log "  ✓ 语法正确"
    else
        err "语法错误，请检查 ${PF_CONF}"
    fi
}

# ---- 应用规则 ----
apply_rules() {
    check_root
    log "应用 pf 规则"

    # 1) 备份
    backup

    # 2) 复制规则文件
    if [[ -f "${RULES_SRC}" ]]; then
        cp "${RULES_SRC}" "${PF_CONF}"
        log "  ✓ 已从 ${RULES_SRC} 复制规则"
    else
        log "  ⚠ 未找到 ${RULES_SRC}，使用内置规则"
        cat > "${PF_CONF}" <<'EOF'
# 光明记忆 Family 档 pf 规则
ext_if = "em0"
int_if = "bridge0"
table <lims_jails> { 10.0.10.10, 10.0.10.20, 10.0.10.30, 10.0.10.40, 10.0.10.50 }
table <management_net> { 192.168.1.0/24 }
set block-policy return
set skip on lo0
block in on $ext_if all
block out on $ext_if all
pass in on $int_if from 10.0.10.0/24 to any keep state
pass out on $int_if from any to 10.0.10.0/24 keep state
pass in on $ext_if proto tcp from any to ($ext_if) port { 443, 8443 } flags S/SA keep state
pass in on $ext_if proto tcp from <management_net> to ($ext_if) port 22 flags S/SA keep state
pass out all keep state
block in on $ext_if all
EOF
    fi

    # 3) 语法校验
    test_rules

    # 4) 启用 pf 并应用
    sysrc pf_enable=YES 2>/dev/null || true
    service pf onestart 2>/dev/null || true
    pfctl -f "${PF_CONF}"
    log "  ✓ pf 规则已应用"

    # 5) 验证
    status
}

# ---- 恢复 ----
restore_rules() {
    check_root
    if [[ -f "${PF_BACKUP}" ]]; then
        cp "${PF_BACKUP}" "${PF_CONF}"
        pfctl -f "${PF_CONF}"
        log "  ✓ 已从 ${PF_BACKUP} 恢复"
    else
        err "未找到备份文件 ${PF_BACKUP}"
    fi
}

# ---- 状态 ----
status() {
    echo "=== pf 状态 ==="
    pfctl -si 2>/dev/null || echo "  pf 未启用"
    echo ""
    echo "=== pf 规则 ==="
    pfctl -sr 2>/dev/null || echo "  无规则"
    echo ""
    echo "=== pf 表 ==="
    pfctl -sTables 2>/dev/null || echo "  无表"
}

# ---- 主入口 ----
main() {
    local sub="${1:-status}"

    case "$sub" in
        apply)
            apply_rules
            ;;
        backup)
            backup
            ;;
        restore)
            restore_rules
            ;;
        test)
            test_rules
            ;;
        status)
            status
            ;;
        *)
            echo "Usage: $0 {apply|backup|restore|test|status}" >&2
            exit 1
            ;;
    esac
}

main "$@"
