#!/bin/sh
# ============================================================================
# install_lims_mcp_rc_d.sh —— 在 192.168.0.88 上安装/升级 lightmem 原生 MCP 服务
# ----------------------------------------------------------------------------
# R21 T3 部署。在既有 lightmem（业务 REST，:18443）之上，新增：
#
#   /usr/local/lightmem/lightmem-mcp           MCP 二进制（streamable-http）
#   /usr/local/lightmem/agent-tokens.json      agent token 表（0600，物主=服务用户）
#   /usr/local/etc/rc.d/lightmem_mcp           rc.d 服务脚本
#   /var/log/lightmem-mcp.log                  日志
#   /var/run/lightmem/lightmem-mcp.pid         pid
#
# 端口分工：18443 = 业务 REST（lightmem）；18444 = MCP-over-HTTP（lightmem_mcp）。
#
# 用法（需要 root）：
#   sudo sh install_lims_mcp_rc_d.sh --src /home/workbuddy/lightmem
#   sudo sh install_lims_mcp_rc_d.sh --src ... --tokens-file /tmp/agent-tokens.json
#   sudo sh install_lims_mcp_rc_d.sh --src ... --require-auth yes
#   sudo sh install_lims_mcp_rc_d.sh --src ... --no-start
#
# 重要（沿用 lightmem 的纪律）：
#   本脚本**不以 root 编译**。0.88 无外网，Go 依赖只能命中 module cache，而
#   `sudo` 会重置环境丢掉 workbuddy 的 GOPATH/GOMODCACHE，root 下编译必然超时。
#   正确顺序：先以 workbuddy 编译，再 sudo 安装（本脚本自动发现已编译产物）。
#
# 幂等：可反复执行（升级二进制 / 换 token / 重启服务）。
# 安全：只在 --tokens-file 显式给出时更新 token 表；不打印 token 值。
# ============================================================================
set -eu

SRC="${LIMS_SRC_DIR:-/home/workbuddy/lightmem}"
PREFIX="${LIMS_PREFIX:-/usr/local/lightmem}"
RCDIR=/usr/local/etc/rc.d
PORT=18444
REST_BASE="http://127.0.0.1:18443"
SVC_USER=workbuddy
LOG=/var/log/lightmem-mcp.log
PIDFILE=/var/run/lightmem/lightmem-mcp.pid
TOKENS_FILE=""
REQUIRE_AUTH="NO"
DO_START=1
BINARY=""

while [ $# -gt 0 ]; do
    case "$1" in
        --src)          SRC="$2"; shift 2 ;;
        --prefix)       PREFIX="$2"; shift 2 ;;
        --port)         PORT="$2"; shift 2 ;;
        --rest-base)    REST_BASE="$2"; shift 2 ;;
        --tokens-file)  TOKENS_FILE="$2"; shift 2 ;;
        --require-auth) REQUIRE_AUTH="$2"; shift 2 ;;
        --user)         SVC_USER="$2"; shift 2 ;;
        --binary)       BINARY="$2"; shift 2 ;;
        --no-start)     DO_START=0; shift ;;
        -h|--help)      sed -n '2,30p' "$0"; exit 0 ;;
        *) echo "未知参数：$1" >&2; exit 2 ;;
    esac
done

[ "$(id -u)" -eq 0 ] || { echo "请以 root 运行（sudo sh $0）" >&2; exit 1; }
[ -d "$SRC/kernel" ] || { echo "构建树不存在或不是 lightmem 仓库：$SRC" >&2; exit 1; }

say() { printf '==> %s\n' "$*"; }

# ---------------------------------------------------------------------------
# 1) 选定二进制（不在 root 下编译）
# ---------------------------------------------------------------------------
if [ -n "$BINARY" ]; then
    BUILT="$BINARY"
elif [ -x "$SRC/kernel/lightmem-mcp" ]; then
    BUILT="$SRC/kernel/lightmem-mcp"
elif [ -x "$SRC/lightmem-mcp" ]; then
    BUILT="$SRC/lightmem-mcp"
else
    cat >&2 <<EOF
找不到已编译的 MCP 二进制。请先以 $SVC_USER 身份编译（不要用 sudo）：

    su -m $SVC_USER -c 'cd $SRC/kernel && go build -o lightmem-mcp ./cmd/lightmem-mcp'

或用 --binary /path/to/lightmem-mcp 指定。
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
install -m 755 "$BUILT" "$PREFIX/lightmem-mcp"

# ---------------------------------------------------------------------------
# 3) token 表（仅显式给出时更新；0600 + 服务用户属主）
# ---------------------------------------------------------------------------
if [ -n "$TOKENS_FILE" ]; then
    [ -r "$TOKENS_FILE" ] || { echo "token 文件不可读：$TOKENS_FILE" >&2; exit 1; }
    # 校验是合法 JSON 对象（避免把坏配置写进去导致服务 fail-fast 起不来）
    if ! python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert isinstance(d,dict) and d' "$TOKENS_FILE" 2>/dev/null; then
        echo "token 文件不是非空 JSON 对象：$TOKENS_FILE" >&2; exit 1
    fi
    install -o "$SVC_USER" -g "$(id -g "$SVC_USER" 2>/dev/null || echo wheel)" -m 600 \
        "$TOKENS_FILE" "$PREFIX/agent-tokens.json"
    _n=$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))))' "$TOKENS_FILE")
    say "token 表已安装：$PREFIX/agent-tokens.json（$_n 条，0600，物主 $SVC_USER）"
else
    if [ -f "$PREFIX/agent-tokens.json" ]; then
        say "保留既有 token 表（未传 --tokens-file）：$PREFIX/agent-tokens.json"
    else
        say "未提供 token 表 → 服务将以**透传态**运行（开发阶段可用，但无鉴权）"
    fi
fi

# 记录来源 commit 与端口
{
    echo "installed_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    echo "source_commit=$(cd "$SRC" && git rev-parse HEAD 2>/dev/null || echo unknown)"
    echo "mcp_port=$PORT"
    echo "rest_base=$REST_BASE"
    echo "require_auth=$REQUIRE_AUTH"
} > "$PREFIX/VERSION.mcp"
chmod 644 "$PREFIX/VERSION.mcp"

# ---------------------------------------------------------------------------
# 4) 安装 rc.d 脚本 + rc.conf
# ---------------------------------------------------------------------------
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
say "安装 $RCDIR/lightmem_mcp"
install -m 555 "$SCRIPT_DIR/lightmem_mcp" "$RCDIR/lightmem_mcp"

say "写入 /etc/rc.conf"
sysrc lightmem_mcp_enable=YES               >/dev/null
sysrc lightmem_mcp_chdir="$PREFIX"          >/dev/null
sysrc lightmem_mcp_user="$SVC_USER"         >/dev/null
sysrc lightmem_mcp_listen=":$PORT"          >/dev/null
sysrc lightmem_mcp_rest_base="$REST_BASE"   >/dev/null
sysrc lightmem_mcp_tokens_file="$PREFIX/agent-tokens.json" >/dev/null
sysrc lightmem_mcp_require_auth="$REQUIRE_AUTH"            >/dev/null
sysrc lightmem_mcp_log="$LOG"               >/dev/null
sysrc lightmem_mcp_pidfile="$PIDFILE"       >/dev/null
for k in lightmem_mcp_enable lightmem_mcp_chdir lightmem_mcp_user lightmem_mcp_listen \
         lightmem_mcp_rest_base lightmem_mcp_tokens_file lightmem_mcp_require_auth; do
    printf '    %s="%s"\n' "$k" "$(sysrc -n "$k" 2>/dev/null)"
done

# ---------------------------------------------------------------------------
# 5) 停掉占用同端口的开发式实例（只针对本端口，不碰 18443）
# ---------------------------------------------------------------------------
for _pid in $(pgrep -f "lightmem-mcp -transport http -listen :$PORT" 2>/dev/null || true); do
    say "停掉占用 :$PORT 的旧实例 pid=$_pid"
    kill "$_pid" 2>/dev/null || true
done
# R25-T0 · L-231（同族）：kill 是异步的，SIGTERM 后进程还要 graceful drain，
# 端口不是立刻消失。原实现固定 `sleep 1` 就启动，R24 实测撞过
# "bind: address already in use" 导致 18443/18444 双停。
# 主服务脚本 install_lims_rc_d.sh 同处已改为轮询等端口释放，本脚本一并改（同一缺陷）。
_i=0
while sockstat -l -P tcp -p "$PORT" 2>/dev/null | grep -q ":$PORT"; do
    _i=$((_i + 1))
    if [ "$_i" -gt 30 ]; then
        say "警告：等待 :$PORT 释放超时（30s），仍尝试启动"
        break
    fi
    sleep 1
done

if [ "$DO_START" -eq 0 ]; then
    say "已安装（--no-start，未启动）。手动启动：service lightmem_mcp start"
    exit 0
fi

# ---------------------------------------------------------------------------
# 6) 启动 + 健康检查
# ---------------------------------------------------------------------------
say "启动服务"
service lightmem_mcp start || { echo "服务启动失败，日志尾部：" >&2; tail -20 "$LOG" >&2; exit 1; }

_i=0
while [ "$_i" -lt 15 ]; do
    if _health=$(fetch -q -T 5 -o - "http://127.0.0.1:$PORT/healthz" 2>/dev/null) && [ -n "$_health" ]; then
        say "健康检查通过：$_health"
        say "开机自启已启用（lightmem_mcp_enable=YES）"
        exit 0
    fi
    _i=$((_i + 1)); sleep 1
done

echo "健康检查超时，日志尾部：" >&2
tail -20 "$LOG" >&2
exit 1
