#!/bin/sh
# R17-S2：lims-qdrant jail + Qdrant 幂等部署脚本。
# 真源：manuals/p1-freebsd-zfs-setup.md §11A.6“Qdrant 真机部署与联调验收”。
#
# 用途：一条命令在 FreeBSD 宿主机（0.88）上完成
#       “ZFS dataset → jail → 出网(NAT/路由) → pkg 安装 → 存储挂载 → 启动 → pf 放行 → 健康检查”，
#       重复执行不报错（幂等），任一 FAIL 以非零码退出（可接 CI / 交接 runbook）。
#       纯 POSIX sh，只依赖 FreeBSD 基座命令（zfs/jail/pfctl/pkg/jexec），无第三方依赖。
#
# 用法：
#   sudo sh scripts/deploy_qdrant.sh              # 全量部署（幂等）
#   sudo sh scripts/deploy_qdrant.sh --verify     # 只验证，不改动任何东西
#   sudo sh scripts/deploy_qdrant.sh --start      # jail 已存在时快速拉起 Qdrant
#
# 环境变量（均有默认值，覆盖用于换机器/换网段）：
#   JAIL_NAME     jail 名（默认 lims-qdrant）
#   JAIL_IP       jail IP/掩码（默认 10.0.10.60/24）
#   EPAIR_NUM     epair 编号（默认 60 → epair60a/epair60b）
#   BRIDGE        宿主机桥接接口（默认 bridge0）
#   GW            jail 默认网关 = bridge0 IP（默认 10.0.10.1）
#   ZPOOL_DS      Qdrant 存储 dataset（默认 zroot/lims/qdrant）
#   STORAGE_MP    dataset 挂载点（默认 /var/db/qdrant）
#   BASE_JAIL     jail 缺失时用于复制的基座 jail（默认空 → 提示 bsdinstall）
#
# 退出码：0 成功（含全部 SKIP 已就绪）；1 运行期失败；2 参数错误；3 环境缺失（非 FreeBSD / 缺命令）。
#
# 设计要点：
#   1. 幂等：每一步先查后做——dataset 存在跳过、jail 在跑跳过、pkg 已装跳过、
#      pf 规则已含跳过、服务已跑跳过；重复执行全部 SKIP，最终验证仍全 PASS。
#   2. 最小根因修复：R17 S2 实机发现 jail 出网存量缺口（pf 无 NAT + forwarding=0
#      + jail 无默认路由），本脚本把三项修复全部纳入（惠及所有 jail 的 pkg/更新）。
#   3. 存储走 ZFS：qdrant 数据落 zroot/lims/qdrant（nullfs 挂进 jail 的 /var/db/qdrant，
#      即 ports 默认 storage 路径）， jail 销毁不丢数据，快照/回滚链路（§9/§10）直接复用。
#   4. pf 安全：改 pf.conf 前必做 pfctl -nf 语法校验，校验不过不加载（防锁死 SSH）；
#      每次改写前备份 /etc/pf.conf.r17bak（仅首次）。
#   5. 红线：只增不改 Schema；不动 lightmem-k 代码；pf 只增 NAT/放行规则。

set -e

JAIL_NAME="${JAIL_NAME:-lims-qdrant}"
JAIL_IP_CIDR="${JAIL_IP:-10.0.10.60/24}"
JAIL_IP="${JAIL_IP_CIDR%/*}"
EPAIR_NUM="${EPAIR_NUM:-60}"
EPAIR_A="epair${EPAIR_NUM}a"
EPAIR_B="epair${EPAIR_NUM}b"
BRIDGE="${BRIDGE:-bridge0}"
GW="${GW:-10.0.10.1}"
JAIL_NET="${JAIL_IP_CIDR%.*}.0/24"   # 10.0.10.0/24
ZPOOL_DS="${ZPOOL_DS:-zroot/lims/qdrant}"
STORAGE_MP="${STORAGE_MP:-/var/db/qdrant}"
JAIL_ROOT="/usr/jails/${JAIL_NAME}"
JAIL_FSTAB="${JAIL_ROOT}/etc/fstab.lims"
JAIL_CONF="/etc/jail.conf"
PF_CONF="/etc/pf.conf"
PF_BAK="/etc/pf.conf.r17bak"
QDRANT_HTTP_PORT=6333
QDRANT_GRPC_PORT=6334

MODE="deploy"
[ "${1:-}" = "--verify" ] && MODE="verify"
[ "${1:-}" = "--start" ] && MODE="start"

# ---------------------------------------------------------------- 输出工具
say()  { printf '%s\n' "$*"; }
ok()   { printf '  [ OK ] %s\n' "$*"; }
skip() { printf '  [SKIP] %s（已就绪）\n' "$*"; }
fail() { printf '  [FAIL] %s\n' "$*" >&2; exit 1; }

as_root() {
    if [ "$(id -u)" = "0" ]; then
        "$@"
    else
        sudo "$@"
    fi
}

# ---------------------------------------------------------------- 环境预检
preflight() {
    say "== [0/9] 环境预检 =="
    [ "$(uname -s)" = "FreeBSD" ] || { say "  [FAIL] 仅支持 FreeBSD（0.88 实机）"; exit 3; }
    for cmd in zfs jail pfctl pkg jexec curl; do
        command -v "$cmd" >/dev/null 2>&1 || fail "缺少命令: $cmd"
    done
    ok "FreeBSD $(uname -r | cut -d- -f1-2)，命令齐备"
    ifconfig "${BRIDGE}" >/dev/null 2>&1 || fail "桥接接口 ${BRIDGE} 不存在（§11A.2 应已创建）"
    ok "${BRIDGE} 在位（$(ifconfig "${BRIDGE}" | grep 'inet ' | awk '{print $2}' | head -1)）"
}

# ---------------------------------------------------------------- [1] ZFS dataset
step_zfs() {
    say "== [1/9] ZFS dataset ${ZPOOL_DS} =="
    if zfs list "${ZPOOL_DS}" >/dev/null 2>&1; then
        skip "dataset ${ZPOOL_DS} 已存在"
    else
        [ "$MODE" = "verify" ] && fail "dataset ${ZPOOL_DS} 缺失"
        as_root zfs create -o mountpoint="${STORAGE_MP}" -o recordsize=128k -o compression=lz4 "${ZPOOL_DS}" \
            || fail "zfs create ${ZPOOL_DS} 失败"
        ok "已创建 ${ZPOOL_DS}（mountpoint=${STORAGE_MP}, recordsize=128k, lz4）"
    fi
    # recordsize/compression 对齐 profile（幂等：属性已一致则 set 无副作用）
    as_root zfs set recordsize=128k "${ZPOOL_DS}" 2>/dev/null || true
    as_root zfs set compression=lz4 "${ZPOOL_DS}" 2>/dev/null || true
    ok "挂载点确认：$(zfs get -H -o value mountpoint "${ZPOOL_DS}")"
    # qdrant 以 nobody:nobody 运行（ports 默认），存储挂载点需其可写
    if [ "$(stat -f '%Su' "${STORAGE_MP}" 2>/dev/null)" = "nobody" ]; then
        skip "${STORAGE_MP} 属主已是 nobody"
    else
        as_root chown nobody:nobody "${STORAGE_MP}" 2>/dev/null || true
        ok "${STORAGE_MP} 属主 -> nobody:nobody（qdrant 进程用户）"
    fi
}

# ---------------------------------------------------------------- [2] jail 基座
step_jail_base() {
    say "== [2/9] jail 基座 ${JAIL_ROOT} =="
    if [ -d "${JAIL_ROOT}" ]; then
        skip "jail 目录已存在"
    else
        [ "$MODE" = "verify" ] && fail "jail 目录 ${JAIL_ROOT} 缺失"
        if [ -n "${BASE_JAIL:-}" ] && [ -d "/usr/jails/${BASE_JAIL}" ]; then
            as_root cp -a "/usr/jails/${BASE_JAIL}" "${JAIL_ROOT}" || fail "cp -a 基座失败"
            ok "已从 /usr/jails/${BASE_JAIL} 复制基座"
        else
            say "  [FAIL] jail 基座缺失。首次部署请先交互式创建："
            say "         sudo bsdinstall jail /usr/jails/${JAIL_NAME}"
            say "         或设置 BASE_JAIL=<干净基座jail名> 后重试（本机历史做法：cp -a 一个未装包的纯净基座）"
            exit 1
        fi
    fi
    # rc.conf：IP + 默认路由（R17 S2 出网修复第三项；存量 jail 也补齐）
    if as_root grep -q "^defaultrouter=" "${JAIL_ROOT}/etc/rc.conf" 2>/dev/null; then
        skip "jail rc.conf defaultrouter 已在位"
    else
        [ "$MODE" = "verify" ] && fail "jail rc.conf 缺 defaultrouter"
        as_root sh -c "cat >> ${JAIL_ROOT}/etc/rc.conf" <<EOF
defaultrouter="${GW}"
EOF
        ok "rc.conf 已补 defaultrouter=${GW}"
    fi
    if as_root grep -q "ifconfig_${EPAIR_B}=" "${JAIL_ROOT}/etc/rc.conf" 2>/dev/null; then
        skip "jail rc.conf ${EPAIR_B} IP 已在位"
    else
        [ "$MODE" = "verify" ] && fail "jail rc.conf 缺 ${EPAIR_B} IP"
        as_root sh -c "cat >> ${JAIL_ROOT}/etc/rc.conf" <<EOF
ifconfig_${EPAIR_B}="inet ${JAIL_IP_CIDR}"
EOF
        ok "rc.conf 已补 ${EPAIR_B}=${JAIL_IP_CIDR}"
    fi
    # 存储 fstab：宿主机 dataset → jail 内 /var/db/qdrant（nullfs，参照 lims-pg 模式）
    if as_root grep -q "${STORAGE_MP}.*nullfs" "${JAIL_FSTAB}" 2>/dev/null; then
        skip "fstab.lims 已含 nullfs 行"
    else
        [ "$MODE" = "verify" ] && fail "${JAIL_FSTAB} 缺 nullfs 行"
        as_root sh -c "cat > ${JAIL_FSTAB}" <<EOF
# ${JAIL_NAME} jail mount fstab (R17 S2)
${STORAGE_MP}    ${JAIL_ROOT}${STORAGE_MP}    nullfs  rw,noatime  0 0
EOF
        ok "fstab 已写入 nullfs: ${STORAGE_MP} -> ${JAIL_ROOT}${STORAGE_MP}"
    fi
}

# ---------------------------------------------------------------- [3] jail.conf 块
step_jail_conf() {
    say "== [3/9] ${JAIL_CONF} ${JAIL_NAME} 块 =="
    if ! as_root grep -q "^${JAIL_NAME} {" "${JAIL_CONF}" 2>/dev/null; then
        [ "$MODE" = "verify" ] && fail "jail.conf 缺 ${JAIL_NAME} 块"
        as_root sh -c "cat >> ${JAIL_CONF}" <<EOF

${JAIL_NAME} {
    host.hostname = ${JAIL_NAME};
    path = ${JAIL_ROOT};
    vnet;
    vnet.interface = ${EPAIR_B};
    exec.start += "/sbin/ifconfig ${EPAIR_B} ${JAIL_IP_CIDR} up; /sbin/route add default ${GW}; /bin/sh /etc/rc";
    mount.fstab = ${JAIL_FSTAB};
    allow.mount;
    allow.mount.nullfs;
    allow.raw_sockets;
    mount.devfs;
}
EOF
        ok "jail.conf 已追加 ${JAIL_NAME} 块（vnet ${EPAIR_B} + nullfs fstab + 默认路由）"
        return 0
    fi
    # 块已存在：确认块内挂载与权限行在位（存量 jail 补挂，不清空重写）
    if as_root awk -v blk="^${JAIL_NAME} \\{" 'BEGIN{p=0} $0 ~ blk {p=1} p && /^}/ {p=0} p' "${JAIL_CONF}" | grep -q "mount.fstab"; then
        skip "jail.conf 已含 ${JAIL_NAME} 块（含 mount.fstab）"
    else
        [ "$MODE" = "verify" ] && fail "jail.conf ${JAIL_NAME} 块缺 mount.fstab"
        as_root awk -v blk="^${JAIL_NAME} \\{" -v fstab="${JAIL_FSTAB}" '
            $0 ~ blk {inblk=1}
            inblk && /exec.start \+=/ {
                print;
                print "    mount.fstab = " fstab ";";
                print "    allow.mount;";
                print "    allow.mount.nullfs;";
                inblk=0; next
            }
            {print}
        ' "${JAIL_CONF}" > /tmp/jail.conf.r17new
        as_root cp "${JAIL_CONF}" "${JAIL_CONF}.r17bak" 2>/dev/null || true
        as_root cp /tmp/jail.conf.r17new "${JAIL_CONF}" && rm -f /tmp/jail.conf.r17new
        ok "jail.conf ${JAIL_NAME} 块已补 mount.fstab + allow.mount/nullfs"
    fi
}

# ---------------------------------------------------------------- [4] epair + bridge
step_network() {
    say "== [4/9] epair${EPAIR_NUM} 桥接 =="
    if ifconfig "${EPAIR_A}" >/dev/null 2>&1; then
        skip "${EPAIR_A} 已存在"
    else
        [ "$MODE" = "verify" ] && fail "${EPAIR_A} 缺失"
        as_root ifconfig "${EPAIR_A}" create || fail "ifconfig ${EPAIR_A} create 失败"
        ok "已创建 ${EPAIR_A}"
    fi
    # 幂等要点：ifconfig 输出格式为 "member: epair60a flags=..."（冒号后有
    # 空格），且 addm 对已在成员报 "File exists" 并非零退出——双重判定最终状态。
    if ifconfig "${BRIDGE}" | grep -q "member:.*${EPAIR_A}"; then
        skip "${EPAIR_A} 已在 ${BRIDGE}"
    else
        [ "$MODE" = "verify" ] && fail "${EPAIR_A} 未挂 ${BRIDGE}"
        if as_root ifconfig "${BRIDGE}" addm "${EPAIR_A}" 2>/dev/null; then
            ok "${EPAIR_A} 已加入 ${BRIDGE}"
        elif ifconfig "${BRIDGE}" | grep -q "${EPAIR_A}"; then
            skip "${EPAIR_A} 已在 ${BRIDGE}（addm 报已存在，按最终状态判定）"
        else
            fail "${EPAIR_A} 加入 ${BRIDGE} 失败"
        fi
    fi
    as_root ifconfig "${EPAIR_A}" up
    ok "${EPAIR_A} up"
}

# ---------------------------------------------------------------- [5] jail 运行
step_jail_run() {
    say "== [5/9] jail ${JAIL_NAME} 运行 =="
    if jls -j "${JAIL_NAME}" >/dev/null 2>&1; then
        skip "${JAIL_NAME} 已在运行（JID $(jls -j "${JAIL_NAME}" jid)）"
    else
        [ "$MODE" = "verify" ] && fail "${JAIL_NAME} 未运行"
        as_root jail -c "${JAIL_NAME}" || fail "jail -c ${JAIL_NAME} 失败"
        sleep 2
        jls -j "${JAIL_NAME}" >/dev/null 2>&1 || fail "jail 启动后仍不可见"
        ok "${JAIL_NAME} 已启动（JID $(jls -j "${JAIL_NAME}" jid)）"
    fi
    # IP 就绪确认
    if as_root jexec "${JAIL_NAME}" ifconfig "${EPAIR_B}" 2>/dev/null | grep -q "inet ${JAIL_IP} "; then
        ok "${EPAIR_B} = ${JAIL_IP}"
    else
        fail "${EPAIR_B} 未见 ${JAIL_IP}（检查 rc.conf/jail.conf 的 IP 配置）"
    fi
    # 默认路由（jail 内 netstat；缺失时用 jail.conf 的 exec.start 兜底解释）
    if as_root jexec "${JAIL_NAME}" netstat -rn 2>/dev/null | grep -q '^default'; then
        ok "默认路由在位（gateway ${GW}）"
    else
        [ "$MODE" = "verify" ] && fail "jail 缺省路由缺失"
        as_root jexec "${JAIL_NAME}" route add default "${GW}" || true
        ok "已补默认路由（建议同时确认 jail.conf exec.start 含 route add）"
    fi
}

# ---------------------------------------------------------------- [6] 出网三件套
step_outbound() {
    say "== [6/9] jail 出网（forwarding + NAT + DNS）=="
    # 6a IP forwarding（运行时 + 持久化）
    FWD="$(sysctl -n net.inet.ip.forwarding)"
    if [ "$FWD" = "1" ]; then
        skip "net.inet.ip.forwarding=1"
    else
        [ "$MODE" = "verify" ] && fail "net.inet.ip.forwarding=${FWD}"
        as_root sysctl net.inet.ip.forwarding=1 || fail "开启 forwarding 失败"
        ok "运行时 forwarding 0 -> 1"
    fi
    if as_root grep -q '^net.inet.ip.forwarding=1' /etc/sysctl.conf 2>/dev/null; then
        skip "sysctl.conf 持久化 forwarding"
    else
        [ "$MODE" = "verify" ] && fail "sysctl.conf 无 forwarding 持久化"
        as_root sh -c 'echo "net.inet.ip.forwarding=1" >> /etc/sysctl.conf'
        ok "sysctl.conf 已追加 net.inet.ip.forwarding=1（重启后仍生效）"
    fi
    # 6b pf NAT 规则（幂等：已含则跳过；不含则追加 + 语法校验 + 加载）
    if as_root grep -q "^nat on .* from ${JAIL_NET} to any" "${PF_CONF}" 2>/dev/null; then
        skip "pf NAT 规则已存在"
    else
        [ "$MODE" = "verify" ] && fail "pf.conf 缺 jail NAT 规则"
        [ -f "${PF_BAK}" ] || as_root cp "${PF_CONF}" "${PF_BAK}"
        EXT_IF="$(as_root grep -m1 '^ext_if' "${PF_CONF}" | cut -d'"' -f2)"
        [ -n "${EXT_IF}" ] || EXT_IF="em0"
        # 追加在 scrub 之后、首条 pass 之前（translation 段），并保证 jail 表含本 jail
        # 注意 awk 规则顺序：先改 $0（table 行）再 print，否则打印的是旧行
        as_root awk -v net="${JAIL_NET}" -v ext="${EXT_IF}" -v ip="${JAIL_IP}" '
            /^table <lims_jails>/ && $0 !~ ip { gsub(/}/, ", " ip " }") }
            { print }
            /^scrub in all/ && !done {
                print "";
                print "# jail 出网 NAT（R17 S2 deploy_qdrant.sh 追加）";
                print "nat on " ext " from " net " to any -> (" ext ")";
                done=1
            }
        ' "${PF_CONF}" > /tmp/pf.conf.r17new
        as_root pfctl -nf /tmp/pf.conf.r17new || { rm -f /tmp/pf.conf.r17new; fail "新 pf.conf 语法校验失败（未加载，SSH 不受影响）"; }
        as_root cp /tmp/pf.conf.r17new "${PF_CONF}" && rm -f /tmp/pf.conf.r17new
        as_root pfctl -f "${PF_CONF}" || fail "pfctl -f 加载失败"
        ok "pf 已加 NAT（${JAIL_NET} -> ${EXT_IF}）+ jail 表含 ${JAIL_IP} 并重载"
    fi
    # 6c jail 内 DNS + 出网实测
    if as_root jexec "${JAIL_NAME}" getent hosts pkg.freebsd.org >/dev/null 2>&1; then
        ok "jail DNS 解析正常"
    else
        fail "jail DNS 失败（确认 jail /etc/resolv.conf 与宿主机一致）"
    fi
}

# ---------------------------------------------------------------- [7] pkg 安装
step_pkg() {
    say "== [7/9] pkg install qdrant =="
    if as_root jexec "${JAIL_NAME}" pkg info -e qdrant >/dev/null 2>&1; then
        skip "qdrant 已安装（$(as_root jexec "${JAIL_NAME}" pkg query '%n-%v' qdrant 2>/dev/null | head -1)）"
        return 0
    fi
    [ "$MODE" = "verify" ] && fail "qdrant 未安装"
    say "  开始安装（qdrant 依赖 rocksdb 等，包体较大，慢速链路需耐心）..."
    as_root jexec "${JAIL_NAME}" pkg install -y qdrant || fail "pkg install qdrant 失败（检查出网/DNS）"
    ok "qdrant 安装完成"
}

# ---------------------------------------------------------------- [8] 配置 + 启动
step_service() {
    say "== [8/9] Qdrant 配置与启动 =="
    # 存储挂载确认（fstab 在 jail 启动时生效；运行中的 jail 需手动 nullfs 补齐）
    if mount | grep -q "on ${JAIL_ROOT}${STORAGE_MP} "; then
        skip "存储挂载在位（${JAIL_ROOT}${STORAGE_MP}）"
    else
        [ "$MODE" = "verify" ] && fail "${JAIL_ROOT}${STORAGE_MP} 未挂载"
        as_root mkdir -p "${JAIL_ROOT}${STORAGE_MP}"
        as_root mount -t nullfs -o rw,noatime "${STORAGE_MP}" "${JAIL_ROOT}${STORAGE_MP}" \
            || fail "nullfs 挂载失败"
        ok "已手动 nullfs 挂载（fstab 已配置，jail 重启后自动生效）"
    fi
    CFG="${JAIL_ROOT}/usr/local/etc/qdrant/config.yaml"
    if [ -f "${CFG}" ]; then
        skip "config.yaml 已就位"
    else
        [ "$MODE" = "verify" ] && fail "${CFG} 缺失"
        as_root cp "${CFG}.sample" "${CFG}" || fail "复制 config.yaml.sample 失败"
        ok "已从 sample 生成 config.yaml"
    fi
    # storage_path 对齐 dataset 挂载点（ports 默认 /var/db/qdrant/storage，嵌套在
    # storage: 段下；幂等：已是该路径则跳过。注意行首缩进，sed 保留原缩进）
    if as_root grep -qE "^[[:space:]]*storage_path:[[:space:]]*${STORAGE_MP}(/storage)?[[:space:]]*$" "${CFG}" 2>/dev/null; then
        skip "storage_path 已在 ${STORAGE_MP} 下"
    else
        as_root sed -i "" "s|^\([[:space:]]*\)storage_path:.*|\1storage_path: ${STORAGE_MP}/storage|" "${CFG}" 2>/dev/null || \
            as_root sh -c "printf 'storage:\\n  storage_path: %s/storage\\n' '${STORAGE_MP}' >> ${CFG}"
        ok "storage_path -> ${STORAGE_MP}/storage（ZFS dataset）"
    fi
    # rc.conf 使能
    if as_root jexec "${JAIL_NAME}" /usr/sbin/sysrc -n qdrant_enable 2>/dev/null | grep -qi yes; then
        skip "qdrant_enable=YES"
    else
        [ "$MODE" = "verify" ] && fail "qdrant_enable 未设 YES"
        as_root jexec "${JAIL_NAME}" /usr/sbin/sysrc qdrant_enable=YES || fail "sysrc qdrant_enable 失败"
        ok "qdrant_enable=YES（开机自启）"
    fi
    # 运行状态
    if as_root jexec "${JAIL_NAME}" service qdrant status >/dev/null 2>&1; then
        skip "qdrant 服务运行中"
    else
        [ "$MODE" = "verify" ] && fail "qdrant 服务未运行"
        as_root jexec "${JAIL_NAME}" service qdrant start || fail "service qdrant start 失败"
        # 等端口起来（最多 30s； jail 基座用 fetch，curl 为可选加速）
        i=0
        while [ $i -lt 30 ]; do
            as_root jexec "${JAIL_NAME}" sh -c "fetch -qo /dev/null -T 2 http://127.0.0.1:${QDRANT_HTTP_PORT}/ 2>/dev/null || curl -fsS -m 2 http://127.0.0.1:${QDRANT_HTTP_PORT}/ >/dev/null 2>&1" && break
            i=$((i+1)); sleep 1
        done
        ok "qdrant 服务已启动"
    fi
}

# ---------------------------------------------------------------- [9] pf 放行 + 验证
step_pf_and_verify() {
    say "== [9/9] pf 放行 + 端到端验证 =="
    # 9a Qdrant 端口对管理网段放行（幂等）
    if as_root grep -q "port { ${QDRANT_HTTP_PORT}, ${QDRANT_GRPC_PORT} }" "${PF_CONF}" 2>/dev/null; then
        skip "pf 已放行 ${QDRANT_HTTP_PORT}/${QDRANT_GRPC_PORT}"
    else
        [ "$MODE" = "verify" ] && fail "pf 未放行 ${QDRANT_HTTP_PORT}/${QDRANT_GRPC_PORT}"
        [ -f "${PF_BAK}" ] || as_root cp "${PF_CONF}" "${PF_BAK}"
        EXT_IF="$(as_root grep -m1 '^ext_if' "${PF_CONF}" | cut -d'"' -f2)"
        MGMT_TBL="$(as_root grep -m1 '^table <management_net>' "${PF_CONF}" | awk '{print $4}')"
        [ -n "${MGMT_TBL}" ] || MGMT_TBL="192.168.0.0/24"
        as_root awk -v h="${QDRANT_HTTP_PORT}" -v g="${QDRANT_GRPC_PORT}" -v ext="${EXT_IF}" -v mgmt="${MGMT_TBL}" '
            { print }
            /^block in on \$ext_if/ && !done {
                print "";
                print "# Qdrant（R17 S2）：HTTP " h " / gRPC " g "，仅管理网段";
                print "pass in on " ext " proto tcp from " mgmt " to (" ext ") port { " h ", " g " } flags S/SA keep state";
                done=1
            }
        ' "${PF_CONF}" > /tmp/pf.conf.r17new
        as_root pfctl -nf /tmp/pf.conf.r17new || { rm -f /tmp/pf.conf.r17new; fail "新 pf.conf 语法校验失败（未加载）"; }
        as_root cp /tmp/pf.conf.r17new "${PF_CONF}" && rm -f /tmp/pf.conf.r17new
        as_root pfctl -f "${PF_CONF}" || fail "pfctl -f 加载失败"
        ok "pf 已放行并重载"
    fi
    # 9b host -> jail 健康检查（HTTP 6333）
    if curl -fsS -m 5 "http://${JAIL_IP}:${QDRANT_HTTP_PORT}/" >/dev/null 2>&1; then
        ok "宿主机 -> http://${JAIL_IP}:${QDRANT_HTTP_PORT}/ 通（Qdrant 响应）"
    else
        fail "宿主机无法访问 Qdrant（bridge0 放行/服务状态排查）"
    fi
    # 9c jail 内回环健康检查（fetch 为基座命令，curl 可选）
    if as_root jexec "${JAIL_NAME}" sh -c "fetch -qo /dev/null -T 5 http://127.0.0.1:${QDRANT_HTTP_PORT}/ 2>/dev/null || curl -fsS -m 5 http://127.0.0.1:${QDRANT_HTTP_PORT}/ >/dev/null 2>&1"; then
        ok "jail 内回环 http://127.0.0.1:${QDRANT_HTTP_PORT}/ 通"
    else
        fail "jail 内回环不通（service qdrant status / config.yaml 排查）"
    fi
    # 9d 存储落盘确认（ZFS dataset 内出现 storage 目录）
    if [ -d "${STORAGE_MP}/collections" ] || [ -d "${STORAGE_MP}/storage" ]; then
        ok "存储目录在 dataset 内：${STORAGE_MP}"
    else
        say "  [WARN] ${STORAGE_MP} 下未见 collections/storage（首次创建集合后出现，属正常）"
    fi
    # 9e 汇总
    say ""
    say "== 部署汇总 =="
    say "  jail      : ${JAIL_NAME} @ ${JAIL_IP}（JID $(jls -j "${JAIL_NAME}" jid 2>/dev/null || echo '?')）"
    say "  Qdrant    : $(as_root jexec "${JAIL_NAME}" pkg query '%n-%v' qdrant 2>/dev/null | head -1)"
    say "  HTTP/gRPC : ${JAIL_IP}:${QDRANT_HTTP_PORT} / ${JAIL_IP}:${QDRANT_GRPC_PORT}"
    say "  存储      : ${STORAGE_MP}（dataset ${ZPOOL_DS}，nullfs -> ${JAIL_ROOT}${STORAGE_MP}）"
    say "  联调命令  : LIGHTMEM_QDRANT_HOST=${JAIL_IP} LIGHTMEM_QDRANT_PORT=${QDRANT_HTTP_PORT} \\"
    say "              go test -count=1 -run 'TestQdrantReal' ./kernel/"
}

# ---------------------------------------------------------------- 主流程
main() {
    case "$MODE" in
        verify) say "模式：--verify（只检查，不做任何改动）" ;;
        start)   say "模式：--start（快速拉起，跳过基座/网络/出网步骤）" ;;
        *)       say "模式：deploy（幂等全量）" ;;
    esac
    preflight
    if [ "$MODE" != "start" ]; then
        step_zfs
        step_jail_base
        step_jail_conf
        step_network
        step_jail_run
        step_outbound
        step_pkg
    else
        step_jail_run
    fi
    step_service
    step_pf_and_verify
    say ""
    say "R17-S2 部署完成：${JAIL_NAME} / Qdrant @ ${JAIL_IP}:${QDRANT_HTTP_PORT}"
}

main
