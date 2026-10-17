#!/bin/sh
# ============================================================================
# lightmem 在 192.168.0.88 上的部署/重启脚本
# ----------------------------------------------------------------------------
# 默认部署「持久化」档：pgvector（PostgreSQL 18 + pgvector 0.8.2，lims-pg jail）。
#
# 为什么默认不用 Solo/SQLite：
#   `kernel/backend_sqlite.go` 当前是**内存骨架**（map[string]*Memory），
#   profile 里的 sqlite path 完全不落盘 —— 进程一重启记忆全丢。
#   故取名记忆系统必须走 pgvector 档。详见
#   `_R160_DSH_lightmem_联调问题记录.md` 的 I-15。
#
# 其它取舍：
#   * 端口用 18443，避开机器上原有的 :8443 旧实例；
#   * 独立数据库 lims_dsh，**不污染** lims（同机还有别的测试在跑）；
#   * 构建与运行都加 nice，避免抢占同机 KPI 测试；
#   * 用户态 nohup 启动，不需要 root。
#
# 用法：
#   sh deploy_lims_088.sh              # 编译（若需要）+ 迁移（若需要）+ 重启
#   sh deploy_lims_088.sh status
#   sh deploy_lims_088.sh stop
#
# 可用环境变量：
#   LIMS_SRC_DIR (源码)     默认 /tmp/dsh-lightmem/lightmem
#   LIMS_RUN_DIR (运行目录) 默认 /tmp/dsh-lightmem
#   LIMS_PORT               默认 18443
#   LIMS_PROFILE            默认 <src>/profiles/lims-family.profile.yaml（持久化档）
#   LIMS_PG_HOST/PORT/USER/DB  默认 10.0.10.10 / 5432 / postgres / lims_dsh
#   LIMS_MODE=pg|solo       默认 pg；solo 为内存骨架（仅供快速试跑）
# ============================================================================
set -eu

SRC_DIR=${LIMS_SRC_DIR:-/tmp/dsh-lightmem/lightmem}
RUN_DIR=${LIMS_RUN_DIR:-/tmp/dsh-lightmem}
BIN=${LIMS_BIN:-$RUN_DIR/lightmem-kernel}
PORT=${LIMS_PORT:-18443}
MODE=${LIMS_MODE:-pg}
NICE=${LIMS_NICE:-20}

LIMS_PG_HOST=${LIMS_PG_HOST:-10.0.10.10}
LIMS_PG_PORT=${LIMS_PG_PORT:-5432}
LIMS_PG_USER=${LIMS_PG_USER:-postgres}
LIMS_PG_DB=${LIMS_PG_DB:-lims_dsh}
export LIMS_PG_HOST LIMS_PG_PORT LIMS_PG_USER LIMS_PG_DB

if [ "$MODE" = "solo" ]; then
    PROFILE=${LIMS_PROFILE:-$SRC_DIR/profiles/lims-solo.profile.yaml}
else
    PROFILE=${LIMS_PROFILE:-$SRC_DIR/profiles/lims-family.profile.yaml}
fi

LOG=$RUN_DIR/lightmem.log
PIDFILE=$RUN_DIR/lightmem.pid

mkdir -p "$RUN_DIR"

pid_of() { [ -f "$PIDFILE" ] && cat "$PIDFILE"; }
is_running() { pid=$(pid_of); [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; }

case "${1:-start}" in
status)
    if is_running; then echo "RUNNING pid=$(pid_of) port=$PORT mode=$MODE"; else echo "STOPPED"; fi
    exit 0 ;;
stop)
    if is_running; then
        pid=$(pid_of); kill "$pid" 2>/dev/null || true
        for _ in 1 2 3 4 5 6 7 8 9 10; do is_running || break; sleep 1; done
        is_running && kill -9 "$(pid_of)" 2>/dev/null || true
        echo "STOPPED pid=$pid"
    else
        echo "already stopped"
    fi
    rm -f "$PIDFILE"; exit 0 ;;
start) ;;
*) echo "usage: $0 [start|stop|status]" >&2; exit 2 ;;
esac

# ---- 1) 构建（仅在缺失时）-----------------------------------------------
# 注意：lightmem 根模块是 `lightmem`，内核入口在子模块 kernel/ 内，
# 必须在 kernel/ 目录下构建（见问题记录 I-1）。
if [ ! -x "$BIN" ]; then
    echo "[build] building kernel (root module does not contain the cmd package)"
    cd "$SRC_DIR/kernel"
    nice -n "$NICE" go build -o "$BIN" ./cmd/lightmem-kernel
    echo "[build] done"
fi

# ---- 2) 持久化档：确保数据库与 schema 就绪（幂等）-----------------------
if [ "$MODE" != "solo" ]; then
    if ! psql -h "$LIMS_PG_HOST" -p "$LIMS_PG_PORT" -U "$LIMS_PG_USER" -d postgres \
            -tc "select 1 from pg_database where datname='$LIMS_PG_DB'" 2>/dev/null | grep -q 1; then
        echo "[pg] creating database $LIMS_PG_DB"
        createdb -h "$LIMS_PG_HOST" -p "$LIMS_PG_PORT" -U "$LIMS_PG_USER" "$LIMS_PG_DB"
    fi
    # 内核不会自动迁移：表缺失时必须手工应用 001_pgvector.up.sql。
    if ! psql -h "$LIMS_PG_HOST" -p "$LIMS_PG_PORT" -U "$LIMS_PG_USER" -d "$LIMS_PG_DB" \
            -tc "select 1 from information_schema.tables where table_name='memories'" 2>/dev/null | grep -q 1; then
        echo "[pg] applying migration 001_pgvector.up.sql to $LIMS_PG_DB"
        psql -h "$LIMS_PG_HOST" -p "$LIMS_PG_PORT" -U "$LIMS_PG_USER" -d "$LIMS_PG_DB" \
            -v ON_ERROR_STOP=1 -f "$SRC_DIR/kernel/migrations/001_pgvector.up.sql" >/dev/null
    fi
fi

# ---- 3) 停旧实例 -------------------------------------------------------
if is_running; then
    echo "[start] stopping existing pid=$(pid_of)"
    sh "$0" stop >/dev/null 2>&1 || true
fi
for pid in $(pgrep -f "lightmem-kernel .*-listen :$PORT" 2>/dev/null || true); do
    echo "[start] killing stray pid=$pid"
    kill "$pid" 2>/dev/null || true
done
sleep 1

# ---- 4) 启动 -----------------------------------------------------------
echo "[start] launching on :$PORT (mode=$MODE profile=$(basename "$PROFILE"))"
cd "$RUN_DIR"
nohup nice -n "$NICE" "$BIN" \
    -profile "$PROFILE" -listen ":$PORT" -embedder hash \
    > "$LOG" 2>&1 &
echo $! > "$PIDFILE"

# ---- 5) 健康检查 -------------------------------------------------------
for _ in 1 2 3 4 5 6 7 8 9 10; do
    if curl -fsS -m 3 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
        echo "[start] healthy: $(curl -fsS -m 3 "http://127.0.0.1:$PORT/healthz")"
        exit 0
    fi
    sleep 1
done

echo "[start] FAILED to become healthy; last log lines:" >&2
tail -20 "$LOG" >&2
exit 1
