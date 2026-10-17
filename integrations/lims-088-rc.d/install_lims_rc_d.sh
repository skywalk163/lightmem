#!/bin/sh
# ============================================================================
# install_lims_rc_d.sh —— 在 192.168.0.88 上安装/升级 lightmem 的 rc.d 自启服务
# ----------------------------------------------------------------------------
# 把「/tmp + nohup」的开发式部署升级为开机自启的系统服务：
#
#   /usr/local/lightmem/                 安装目录（稳定，非 /tmp）
#     ├── lightmem-kernel                内核二进制
#     ├── profiles/                      档位配置
#     ├── migrations/001_pgvector.up.sql 建表脚本（运维备查/重建用）
#     └── VERSION                        来源 commit 与安装时间
#   /usr/local/etc/rc.d/lightmem         rc.d 服务脚本
#   /var/log/lightmem.log                服务日志
#   /var/run/lightmem/lightmem.pid       服务 pid
#
# 用法（需要 root）：
#   sudo sh install_lims_rc_d.sh                      # 用默认构建树
#   sudo sh install_lims_rc_d.sh --src /path/to/lightmem
#   sudo sh install_lims_rc_d.sh --binary /path/to/lightmem-kernel   # 用已编译好的二进制
#   sudo sh install_lims_rc_d.sh --no-start           # 只安装不启动
#   sudo sh install_lims_rc_d.sh --pg-db lims_dsh --port 18443
#
# 重要：本脚本**不以 root 编译**。0.88 无外网，Go 依赖只能命中 module cache，
# 而 `sudo` 会重置环境（丢掉 workbuddy 的 GOPATH/GOMODCACHE），root 下编译必然
# 尝试联网并超时。正确顺序是：先以 workbuddy 编译，再 sudo 安装（脚本可自动
# 发现 $SRC/kernel/lightmem-kernel，或用 --binary 指定）。
#
# 幂等：可反复执行（升级二进制 / 改配置 / 重启服务）。
# 安全：只操作本服务自己的进程与端口；**不会**碰同机上 :8443 的既有实例。
# ============================================================================
set -eu

SRC="${LIMS_SRC_DIR:-/tmp/dsh-lightmem/lightmem}"
PREFIX="${LIMS_PREFIX:-/usr/local/lightmem}"
RCDIR=/usr/local/etc/rc.d
PORT=18443
PG_HOST=10.0.10.10
PG_PORT=5432
PG_USER=postgres
PG_DB=lims_dsh
SVC_USER=workbuddy
DO_START=1
BINARY=""

while [ $# -gt 0 ]; do
    case "$1" in
        --src)      SRC="$2"; shift 2 ;;
        --prefix)   PREFIX="$2"; shift 2 ;;
        --port)     PORT="$2"; shift 2 ;;
        --pg-host)  PG_HOST="$2"; shift 2 ;;
        --pg-db)    PG_DB="$2"; shift 2 ;;
        --user)     SVC_USER="$2"; shift 2 ;;
        --binary)   BINARY="$2"; shift 2 ;;
        --no-start) DO_START=0; shift ;;
        -h|--help)  sed -n '2,32p' "$0"; exit 0 ;;
        *) echo "未知参数：$1" >&2; exit 2 ;;
    esac
done

[ "$(id -u)" -eq 0 ] || { echo "请以 root 运行（sudo sh $0）" >&2; exit 1; }
[ -d "$SRC/kernel" ] || { echo "构建树不存在或不是 lightmem 仓库：$SRC" >&2; exit 1; }

say() { printf '==> %s\n' "$*"; }

# ---------------------------------------------------------------------------
# 1) 选定二进制（不在 root 下编译，见文件头说明）
# ---------------------------------------------------------------------------
# lightmem 根模块是 `lightmem`，内核入口在子模块 kernel/ 内，必须在 kernel/ 下构建。
if [ -n "$BINARY" ]; then
    BUILT="$BINARY"
elif [ -x "$SRC/kernel/lightmem-kernel" ]; then
    BUILT="$SRC/kernel/lightmem-kernel"
elif [ -x "$SRC/lightmem-kernel" ]; then
    BUILT="$SRC/lightmem-kernel"
else
    cat >&2 <<EOF
找不到已编译的内核二进制。请先以 $SVC_USER 身份编译（不要用 sudo）：

    su -m $SVC_USER -c 'cd $SRC/kernel && go build -o lightmem-kernel ./cmd/lightmem-kernel'

或用 --binary /path/to/lightmem-kernel 指定。
（原因：sudo 会重置环境丢掉 Go module cache，而本机无外网，root 下编译必然超时。）
EOF
    exit 1
fi
[ -x "$BUILT" ] || { echo "二进制不可执行：$BUILT" >&2; exit 1; }
say "使用二进制：$BUILT"

# ---------------------------------------------------------------------------
# 2) 安装文件
# ---------------------------------------------------------------------------
say "安装到 $PREFIX"
install -d -m 755 "$PREFIX"
install -d -m 755 "$PREFIX/profiles"
install -d -m 755 "$PREFIX/migrations"

install -m 755 "$BUILT" "$PREFIX/lightmem-kernel"
install -m 644 "$SRC/profiles/lims-family.profile.yaml" "$PREFIX/profiles/"
install -m 644 "$SRC/profiles/lims-solo.profile.yaml"   "$PREFIX/profiles/"
install -m 644 "$SRC/kernel/migrations/001_pgvector.up.sql" "$PREFIX/migrations/"

{
    echo "installed_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    echo "source_commit=$(cd "$SRC" && git rev-parse HEAD 2>/dev/null || echo unknown)"
    echo "port=$PORT"
    echo "pg=${PG_HOST}:${PG_PORT}/${PG_DB}"
} > "$PREFIX/VERSION"
chmod 644 "$PREFIX/VERSION"

# ---------------------------------------------------------------------------
# 3) 数据库与 schema（幂等；内核不会自动迁移）
# ---------------------------------------------------------------------------
say "确保 PostgreSQL 库 $PG_DB 与 schema 就绪"
if ! PGPASSWORD="${LIMS_PG_PASSWORD:-}" psql -h "$PG_HOST" -p "$PG_PORT" -U "$PG_USER" -d postgres \
        -tAc "select 1 from pg_database where datname='$PG_DB'" 2>/dev/null | grep -q 1; then
    createdb -h "$PG_HOST" -p "$PG_PORT" -U "$PG_USER" "$PG_DB"
fi
if ! psql -h "$PG_HOST" -p "$PG_PORT" -U "$PG_USER" -d "$PG_DB" \
        -tAc "select 1 from information_schema.tables where table_name='memories'" 2>/dev/null | grep -q 1; then
    say "应用迁移 001_pgvector.up.sql"
    psql -h "$PG_HOST" -p "$PG_PORT" -U "$PG_USER" -d "$PG_DB" -v ON_ERROR_STOP=1 \
        -f "$PREFIX/migrations/001_pgvector.up.sql" >/dev/null
fi

# ---------------------------------------------------------------------------
# 4) 安装 rc.d 脚本
# ---------------------------------------------------------------------------
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
say "安装 $RCDIR/lightmem"
install -m 555 "$SCRIPT_DIR/lightmem" "$RCDIR/lightmem"

# ---------------------------------------------------------------------------
# 5) 写 rc.conf
# ---------------------------------------------------------------------------
say "写入 /etc/rc.conf"
sysrc lightmem_enable=YES          >/dev/null
sysrc lightmem_chdir="$PREFIX"     >/dev/null
sysrc lightmem_user="$SVC_USER"    >/dev/null
sysrc lightmem_listen=":$PORT"     >/dev/null
sysrc lightmem_pg_host="$PG_HOST"  >/dev/null
sysrc lightmem_pg_port="$PG_PORT"  >/dev/null
sysrc lightmem_pg_user="$PG_USER"  >/dev/null
sysrc lightmem_pg_db="$PG_DB"      >/dev/null
sysrc lightmem_log=/var/log/lightmem.log            >/dev/null
sysrc lightmem_pidfile=/var/run/lightmem/lightmem.pid >/dev/null
echo "  已设置："
for k in lightmem_enable lightmem_chdir lightmem_user lightmem_listen \
         lightmem_pg_host lightmem_pg_port lightmem_pg_user lightmem_pg_db; do
    printf '    %s="%s"\n' "$k" "$(sysrc -n "$k" 2>/dev/null)"
done

# ---------------------------------------------------------------------------
# 6) 迁移期清理：停掉占用同一端口的开发式实例（只针对该端口，不碰 :8443）
# ---------------------------------------------------------------------------
for _pid in $(pgrep -f "lightmem-kernel .*-listen :$PORT" 2>/dev/null || true); do
    say "停掉占用 :$PORT 的旧实例 pid=$_pid"
    kill "$_pid" 2>/dev/null || true
done
sleep 1

if [ "$DO_START" -eq 0 ]; then
    say "已安装（--no-start，未启动）。手动启动：service lightmem start"
    exit 0
fi

# ---------------------------------------------------------------------------
# 7) 启动 + 健康检查
# ---------------------------------------------------------------------------
say "启动服务"
service lightmem start || { echo "服务启动失败，日志尾部：" >&2; tail -20 /var/log/lightmem.log >&2; exit 1; }

_i=0
while [ "$_i" -lt 15 ]; do
    if _health=$(fetch -q -T 5 -o - "http://127.0.0.1:$PORT/healthz" 2>/dev/null) && [ -n "$_health" ]; then
        say "健康检查通过：$_health"
        say "开机自启已启用（lightmem_enable=YES）"
        exit 0
    fi
    _i=$((_i + 1)); sleep 1
done

echo "健康检查超时，日志尾部：" >&2
tail -20 /var/log/lightmem.log >&2
exit 1
