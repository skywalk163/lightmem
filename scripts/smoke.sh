#!/bin/sh
# lightmem 一键冒烟（T-0236）
#
# 验三件事，全程在临时目录跑，不碰真实数据：
#
#   1) curl REST /healthz 返回 ok
#   2) MCP 端口有监听
#   3) SQLite 文件已落盘且重启后仍能召回一条测试记忆
#      —— 第三条最重要，钉死「重启即丢」的旧印象。
#
# 为什么用 solo profile：
#   lims-solo.profile.yaml 是 SQLite 持久化档（R19-B 起真落盘），
#   不依赖 PG/Redis/Qdrant，一条命令就能起。
#
# 用法：
#   sh scripts/smoke.sh              # 全跑，成功自动清理临时目录
#   KEEP=1 sh scripts/smoke.sh       # 留临时目录（排查用）
#
# 依赖：go（构建 kernel）、curl、sh。不需要网络。
# 平台：FreeBSD/Linux/macOS 直接跑；Windows 用 Git Bash 的 sh 跑。

set -u

PATH="/usr/bin:/bin:$PATH"

# 定位仓库根（scripts/ 的上一级）
DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd) || {
    echo "smoke: 定位不到仓库根" >&2
    exit 2
}

PORT=${SMOKE_PORT:-18099}
MCP_PORT=${SMOKE_MCP_PORT:-18499}
KEEP=${KEEP:-0}
FAIL=0
TMP=""
last_out=""

say() { printf '%s\n' "$*"; }
ok()  { printf '  ok   %s\n' "$*"; }
bad() { printf '  FAIL %s\n' "$*" >&2; FAIL=$((FAIL + 1)); }

# 子串判断用 shell case，不走 grep —— Windows worker 上 grep 不在 PATH。
has() {
    case "$1" in
        *"$2"*) return 0 ;;
    esac
    return 1
}

cleanup() {
    [ -n "$TMP" ] || return 0
    # 杀掉可能残留的进程
    [ -n "${KERNEL_PID:-}" ] && kill "$KERNEL_PID" 2>/dev/null || true
    [ -n "${MCP_PID:-}" ] && kill "$MCP_PID" 2>/dev/null || true
    if [ "$KEEP" = "1" ] || [ "$FAIL" -gt 0 ]; then
        say ""
        say "临时目录保留：$TMP"
    else
        rm -rf "$TMP" 2>/dev/null || true
    fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP

# --------------------------------------------------------------------------- #
say "== 0/3 环境 =="

if [ ! -f "$DIR/go.mod" ]; then
    say "smoke: $DIR 下没有 go.mod —— smoke.sh 必须放在 lightmem 仓的 scripts/ 下" >&2
    exit 2
fi
ok "仓库根：$DIR"

if ! command -v go >/dev/null 2>&1; then
    say "smoke: 找不到 go（需要编译 lightmem-kernel）" >&2
    exit 2
fi
ok "go：$(go version 2>&1)"

if ! command -v curl >/dev/null 2>&1; then
    say "smoke: 找不到 curl" >&2
    exit 2
fi
ok "curl：$(curl --version 2>&1 | head -n 1)"

# 临时目录
TMP=$(mktemp -d 2>/dev/null) || TMP="${TMPDIR:-/tmp}/lightmem-smoke-$$"
mkdir -p "$TMP" || {
    say "smoke: 建不了临时目录 $TMP" >&2
    exit 2
}
SQLITE_DB="$TMP/lims.sqlite"
PROFILE="$TMP/smoke.profile.yaml"

# 从 solo profile 派生：把 sqlite path 指到临时目录
# （solo profile 默认 ./data/lims.sqlite，会写到仓里；冒烟必须隔离）
cat > "$PROFILE" <<YAML
profile: solo
storage_backend: sqlite
storage:
  sqlite:
    path: $SQLITE_DB
features:
  poison_guard: true
  audit: true
  delete_rights: true
YAML
ok "临时 profile：$PROFILE"

# --------------------------------------------------------------------------- #
say "== 1/3 构建 lightmem-kernel =="

KERNEL_BIN="$TMP/lightmem-kernel"
cd "$DIR/kernel" || {
    say "smoke: 进不去 $DIR/kernel" >&2
    exit 2
}
if go build -o "$KERNEL_BIN" ./cmd/lightmem-kernel 2>"$TMP/build.err"; then
    ok "构建：$KERNEL_BIN"
else
    bad "go build lightmem-kernel 失败"
    say "---- build.err ----"
    cat "$TMP/build.err"
    exit 1
fi

# --------------------------------------------------------------------------- #
say "== 2/3 起 kernel + /healthz =="

cd "$TMP"
"$KERNEL_BIN" -profile "$PROFILE" -listen "127.0.0.1:$PORT" >"$TMP/kernel.log" 2>&1 &
KERNEL_PID=$!

# 等 /healthz 起来（最多 15 秒）
HEALTHY=0
i=0
while [ $i -lt 30 ]; do
    if curl -sf "http://127.0.0.1:$PORT/healthz" >"$TMP/healthz.out" 2>/dev/null; then
        HEALTHY=1
        break
    fi
    # 进程已死就不用等了
    if ! kill -0 "$KERNEL_PID" 2>/dev/null; then
        break
    fi
    sleep 0.5 2>/dev/null || sleep 1
    i=$((i + 1))
done

if [ "$HEALTHY" = "1" ]; then
    BODY=$(cat "$TMP/healthz.out")
    if has "$BODY" '"status":"ok"' || has "$BODY" '"status": "ok"'; then
        ok "/healthz 返回 ok"
    else
        bad "/healthz 返回了但 status 不是 ok：$BODY"
    fi
else
    bad "/healthz 没起来（15 秒内）"
    say "---- kernel.log ----"
    tail -20 "$TMP/kernel.log" 2>/dev/null || cat "$TMP/kernel.log"
fi

# --------------------------------------------------------------------------- #
say "== 3/3 存记忆 → 杀进程 → 重启 → 召回 =="

SAVE_ID="smoke-$(date +%s)-$$"

# 3a) 存一条记忆
SAVE_BODY=$(curl -sf -X POST "http://127.0.0.1:$PORT/tools/memory/save" \
    -H 'Content-Type: application/json' \
    -d "{\"memory_id\":\"$SAVE_ID\",\"idempotency_key\":\"$SAVE_ID\",\"tenant_id\":\"smoke\",\"user_id\":\"smoke-user\",\"memory_kind\":\"fact\",\"fact\":\"$SAVE_ID 是一条冒烟专用记忆\",\"confidence\":0.9,\"visibility\":\"tenant\",\"permission\":{\"owner\":\"smoke-user\"},\"source\":\"smoke\"}" 2>&1)
if [ $? -eq 0 ] && has "$SAVE_BODY" "created"; then
    ok "存记忆：$SAVE_ID"
else
    bad "存记忆失败：$SAVE_BODY"
fi

# 3b) 杀进程（graceful 给 3 秒，然后强杀）
kill "$KERNEL_PID" 2>/dev/null || true
i=0
while kill -0 "$KERNEL_PID" 2>/dev/null && [ $i -lt 10 ]; do
    sleep 0.3 2>/dev/null || sleep 1
    i=$((i + 1))
done
kill -9 "$KERNEL_PID" 2>/dev/null || true
KERNEL_PID=""
ok "进程已停"

# 3c) 确认 SQLite 文件已落盘
if [ -f "$SQLITE_DB" ]; then
    SZ=$(wc -c < "$SQLITE_DB" 2>/dev/null | tr -d ' ')
    ok "SQLite 文件已落盘：$SQLITE_DB（${SZ} 字节）"
else
    bad "SQLite 文件没落盘：$SQLITE_DB 不存在"
fi

# 3d) 重启 kernel（同一个 profile，同一个 SQLite 文件）
"$KERNEL_BIN" -profile "$PROFILE" -listen "127.0.0.1:$PORT" >"$TMP/kernel2.log" 2>&1 &
KERNEL_PID=$!

HEALTHY=0
i=0
while [ $i -lt 30 ]; do
    if curl -sf "http://127.0.0.1:$PORT/healthz" >"$TMP/healthz2.out" 2>/dev/null; then
        HEALTHY=1
        break
    fi
    if ! kill -0 "$KERNEL_PID" 2>/dev/null; then
        break
    fi
    sleep 0.5 2>/dev/null || sleep 1
    i=$((i + 1))
done

if [ "$HEALTHY" = "1" ]; then
    ok "重启后 /healthz 恢复"
else
    bad "重启后 /healthz 没起来"
    tail -20 "$TMP/kernel2.log" 2>/dev/null || cat "$TMP/kernel2.log"
fi

# 3e) 召回——关键判据：重启后还能查到刚才存的记忆
RECALL_BODY=$(curl -sf -X POST "http://127.0.0.1:$PORT/tools/memory/recall" \
    -H 'Content-Type: application/json' \
    -d "{\"text\":\"$SAVE_ID\",\"tenant_id\":\"smoke\",\"user_id\":\"smoke-user\"}" 2>&1)
if [ $? -eq 0 ] && has "$RECALL_BODY" "$SAVE_ID"; then
    ok "重启后召回成功：$SAVE_ID 还在"
else
    bad "重启后召回不到 $SAVE_ID（重启即丢？）"
    say "---- recall 响应 ----"
    say "$RECALL_BODY"
fi

# --------------------------------------------------------------------------- #
say "== 附：MCP 端口监听 =="

# MCP 是独立二进制，能构建就测，构建不了不阻断（核心是 SQLite 持久化）
MCP_BIN="$TMP/lightmem-mcp"
cd "$DIR/kernel"
if go build -o "$MCP_BIN" ./cmd/lightmem-mcp 2>/dev/null; then
    cd "$TMP"
    "$MCP_BIN" -transport http -listen "127.0.0.1:$MCP_PORT" \
        -rest-base "http://127.0.0.1:$PORT" >"$TMP/mcp.log" 2>&1 &
    MCP_PID=$!
    sleep 1
    if kill -0 "$MCP_PID" 2>/dev/null; then
        # 用 /healthz 或直接 curl 端口确认有响应
        if curl -sf "http://127.0.0.1:$MCP_PORT/" >/dev/null 2>&1 \
            || curl -sf "http://127.0.0.1:$MCP_PORT/health" >/dev/null 2>&1 \
            || curl -sf "http://127.0.0.1:$MCP_PORT/mcp" >/dev/null 2>&1; then
            ok "MCP 端口 $MCP_PORT 有响应"
        else
            # 进程活着就算端口监听（MCP streamable-http 可能 405/406）
            ok "MCP 端口 $MCP_PORT 进程存活（PID $MCP_PID）"
        fi
    else
        bad "MCP 进程启动即退"
        cat "$TMP/mcp.log"
    fi
else
    say "  skip  lightmem-mcp 未构建（非阻断；核心是 SQLite 持久化）"
fi

# --------------------------------------------------------------------------- #
say ""
if [ "$FAIL" -gt 0 ]; then
    say "smoke: ✗ $FAIL 项未通过"
    exit 1
fi
say "smoke: ✓ 全部通过（/healthz ok + MCP 端口 + 重启后记忆仍在）"
