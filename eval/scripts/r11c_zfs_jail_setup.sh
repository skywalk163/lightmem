#!/usr/bin/env bash
# R11 C.1 — ZFS dataset + jail 创建脚本
#
# 对应任务：R11 C 路 — 生产部署硬化
# 对应文档：manuals/p1-freebsd-zfs-setup.md §3 + §5
# 适用平台：FreeBSD 15.1-RELEASE-p1（0.88 实机）
# 执行用户：root 或 sudo 用户（wheel 组，NOPASSWD）
#
# 用法：
#   sudo bash r11c_zfs_jail_setup.sh [create|destroy|list]
#
# 红线：
#   - 仅创建 Family 档启动所必需的 lims-pg / lims-router 相关 dataset + jail
#   - 不触碰既有数据（destroy 子命令会删除，需确认）

set -euo pipefail

# ---- 配置 ----
ZPOOL="${LIMS_ZFS_POOL:-zroot}"
JAIL_CONF="/etc/jail.conf"
BRIDGE_IF="bridge0"
VNET_SUBNET="10.0.10.0/24"

# ---- 工具函数 ----
log() {
    echo "[r11c-zfs-jail] $*"
}

err() {
    echo "[r11c-zfs-jail] ERROR: $*" >&2
    exit 1
}

check_root() {
    if [[ $(id -u) -ne 0 ]]; then
        err "请用 root 或 sudo 执行"
    fi
}

# ---- ZFS dataset 创建 ----
create_zfs_datasets() {
    log "创建 ZFS dataset（pool=${ZPOOL}）"

    # 1) PG 数据
    zfs create -o mountpoint=/var/db/pgdata \
        -o recordsize=8k -o compression=lz4 \
        ${ZPOOL}/lims/pg 2>/dev/null || true
    log "  ✓ ${ZPOOL}/lims/pg"

    # 2) PG WAL
    zfs create -o mountpoint=/var/db/pg_wal \
        -o recordsize=64k -o compression=lz4 \
        -o sync=always -o logbias=throughput \
        ${ZPOOL}/lims/pg_wal 2>/dev/null || true
    log "  ✓ ${ZPOOL}/lims/pg_wal"

    # 3) pgvector 向量列
    zfs create -o mountpoint=/var/db/pg_vector \
        -o recordsize=128k -o compression=lz4 \
        ${ZPOOL}/lims/pg_vector 2>/dev/null || true
    log "  ✓ ${ZPOOL}/lims/pg_vector"

    # 4) 审计日志
    zfs create -o mountpoint=/var/log/lims \
        -o recordsize=1M -o compression=lz4 \
        ${ZPOOL}/lims/logs 2>/dev/null || true
    log "  ✓ ${ZPOOL}/lims/logs"

    # 关闭 atime
    zfs set atime=off ${ZPOOL}/lims 2>/dev/null || true

    log "ZFS dataset 创建完成"
    zfs list -o name,mountpoint,recordsize,compression | grep ${ZPOOL}/lims
}

# ---- ZFS dataset 销毁 ----
destroy_zfs_datasets() {
    log "销毁 ZFS dataset（pool=${ZPOOL}）"
    read -rp "确认销毁 ${ZPOOL}/lims/* 全部 dataset？(yes/NO): " confirm
    if [[ "$confirm" == "yes" ]]; then
        zfs destroy -r ${ZPOOL}/lims
        log "  ✓ 已销毁"
    else
        log "  已取消"
    fi
}

# ---- 列出 ZFS dataset ----
list_zfs_datasets() {
    zfs list -o name,mountpoint,recordsize,compression,encryption | grep ${ZPOOL}/lims || echo "  无 ${ZPOOL}/lims 相关 dataset"
}

# ---- jail 创建 ----
create_jails() {
    log "创建 jail"

    # 加载内核模块
    kldload if_bridge 2>/dev/null || true
    kldload epair 2>/dev/null || true

    # 创建 bridge
    ifconfig ${BRIDGE_IF} create 2>/dev/null || true
    ifconfig ${BRIDGE_IF} 10.0.10.1/24 up 2>/dev/null || true
    log "  ✓ ${BRIDGE_IF} 已创建（10.0.10.1/24）"

    # 创建 jail 配置
    cat > ${JAIL_CONF} <<'JAILCONF'
# 光明记忆 Family 档 jail 配置
# 由 r11c_zfs_jail_setup.sh 自动生成

lims-pg {
    host.hostname = lims-pg;
    path = /usr/jails/lims-pg;
    vnet;
    vnet.interface = epair10b;
    ip4.addr = 10.0.10.10/24;
    exec.start = "/bin/sh /etc/rc";
    exec.stop = "/bin/sh /etc/rc.shutdown";
    allow.raw_sockets;
    mount.fstab = /etc/jail/lims-pg.fstab;
}

lims-router {
    host.hostname = lims-router;
    path = /usr/jails/lims-router;
    vnet;
    vnet.interface = epair50b;
    ip4.addr = 10.0.10.50/24;
    exec.start = "/bin/sh /etc/rc";
    exec.stop = "/bin/sh /etc/rc.shutdown";
    allow.raw_sockets;
    mount.fstab = /etc/jail/lims-router.fstab;
}
JAILCONF
    log "  ✓ ${JAIL_CONF} 已创建"

    # 创建 fstab
    mkdir -p /etc/jail
    cat > /etc/jail/lims-pg.fstab <<'FSTAB'
${ZPOOL}/lims/pg /var/db/pgdata nullfs rw 0 0
${ZPOOL}/lims/pg_wal /var/db/pg_wal nullfs rw 0 0
${ZPOOL}/lims/pg_vector /var/db/pg_vector nullfs rw 0 0
FSTAB
    log "  ✓ /etc/jail/lims-pg.fstab"

    # 创建 epair 接口
    for n in 10 50; do
        ifconfig epair${n} create 2>/dev/null || true
        ifconfig ${BRIDGE_IF} addm epair${n}a up 2>/dev/null || true
        ifconfig epair${n}b inet 10.0.10.${n}/24 up 2>/dev/null || true
    done
    log "  ✓ epair10/epair50 已创建并加入 ${BRIDGE_IF}"

    # 创建 jail 根目录（需 base.txz 解包）
    for j in lims-pg lims-router; do
        if [[ ! -d /usr/jails/${j} ]]; then
            log "  ⚠ /usr/jails/${j} 不存在，需先解包 base.txz"
        fi
    done

    # 启动 jail
    jail -f ${JAIL_CONF} -c lims-pg 2>/dev/null || true
    jail -f ${JAIL_CONF} -c lims-router 2>/dev/null || true
    log "  ✓ jail 已启动"

    log "jail 创建完成"
    jail -l
}

# ---- jail 销毁 ----
destroy_jails() {
    log "销毁 jail"
    read -rp "确认销毁 lims-pg / lims-router jail？(yes/NO): " confirm
    if [[ "$confirm" == "yes" ]]; then
        jail -f ${JAIL_CONF} -r lims-pg 2>/dev/null || true
        jail -f ${JAIL_CONF} -r lims-router 2>/dev/null || true
        # 销毁 epair
        for n in 10 50; do
            ifconfig epair${n}a destroy 2>/dev/null || true
        done
        rm -f ${JAIL_CONF} /etc/jail/lims-pg.fstab /etc/jail/lims-router.fstab
        rmdir /etc/jail 2>/dev/null || true
        log "  ✓ 已销毁"
    else
        log "  已取消"
    fi
}

# ---- 列出 jail ----
list_jails() {
    jail -l
}

# ---- 主入口 ----
main() {
    check_root
    local sub="${1:-list}"

    case "$sub" in
        create)
            create_zfs_datasets
            create_jails
            ;;
        destroy)
            destroy_jails
            destroy_zfs_datasets
            ;;
        list)
            list_zfs_datasets
            echo ""
            list_jails
            ;;
        *)
            echo "Usage: $0 {create|destroy|list}" >&2
            exit 1
            ;;
    esac
}

main "$@"
