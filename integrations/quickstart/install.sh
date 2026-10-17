#!/bin/sh
# ============================================================================
# lightmem 一键部署（quickstart）
# ----------------------------------------------------------------------------
# 目标：让**任何人**拿到本仓库后，一条命令把 lightmem「共享记忆大脑」跑起来，
#       并产出「客户端接入手册 + 可直接粘给 AI 的安装 prompt」。
#
# 用法（最简，零外部依赖）：
#     sh integrations/quickstart/install.sh
#
# 常用变体：
#     sh install.sh --profile solo            # 个人档：SQLite，无需 PG（默认）
#     sh install.sh --profile family          # 家庭档：需要 PostgreSQL + pgvector
#     sh install.sh --port 7443 --bind 127.0.0.1
#     sh install.sh --tenant myteam --user alice
#     sh install.sh --no-mcp                  # 只起 REST，不起原生 MCP
#     sh install.sh --systemd                 # 额外安装 systemd 服务（Linux）
#     sh install.sh --dry-run                 # 只打印计划，不动任何东西
#
# 它会做：
#   1. 检查 Go（没有就明确告诉你装什么）；
#   2. 编译 lightmem-kernel（REST）与 lightmem-mcp（MCP 服务）；
#   3. 生成 profile（--profile solo 时用 SQLite，**零外部依赖**）；
#   4. 签发 agent token（调用 integrations/agent-onboarding/issue_token.py）；
#   5. 启动两个服务，做健康检查；
#   6. 生成客户端接入包（调用 gen_configs.py）—— 含 MANIFEST.md 与 agent-prompt.md。
#
# 设计原则：
#   - **零第三方依赖**：只用 POSIX sh + Go + Python3（系统自带即可）；
#   - **幂等**：可反复执行；
#   - **不碰系统**：默认全装在当前目录下的 ./_lightmem_run/，不写 /etc、不注册服务
#     （除非显式 --systemd）；不用 root；
#   - **失败即停**：每步 set -e，出错立刻退出并给出原因。
# ============================================================================
set -eu

# ---------------------------------------------------------------- 默认参数
PROFILE=solo
PORT=7443
MCP_PORT=7444
BIND=127.0.0.1
TENANT=myteam
USER_ID=alice
RUN_MCP=1
DO_SYSTEMD=0
DRY=0
AGENTS=""

while [ $# -gt 0 ]; do
    case "$1" in
        --profile)  PROFILE="$2"; shift 2 ;;
        --port)     PORT="$2"; shift 2 ;;
        --mcp-port) MCP_PORT="$2"; shift 2 ;;
        --bind)     BIND="$2"; shift 2 ;;
        --tenant)   TENANT="$2"; shift 2 ;;
        --user)     USER_ID="$2"; shift 2 ;;
        --agents)   AGENTS="$2"; shift 2 ;;
        --no-mcp)   RUN_MCP=0; shift ;;
        --systemd)  DO_SYSTEMD=1; shift ;;
        --dry-run)  DRY=1; shift ;;
        -h|--help)  sed -n '2,40p' "$0"; exit 0 ;;
        *) echo "未知参数：$1（-h 看用法）" >&2; exit 2 ;;
    esac
done

# ---------------------------------------------------------------- 定位仓库
SELF_DIR=$(cd "$(dirname "$0")" && pwd)
# 本脚本位于 <repo>/integrations/quickstart/，故仓库根是上上级
ROOT=$(cd "$SELF_DIR/../.." && pwd)

say()  { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
ok()   { printf '    \033[1;32m✓\033[0m %s\n' "$*"; }
warn() { printf '    \033[1;33m!\033[0m %s\n' "$*"; }
die()  { printf '\n\033[1;31m[x] %s\033[0m\n' "$*" >&2; exit 1; }
run()  { if [ "$DRY" -eq 1 ]; then printf '    [dry-run] %s\n' "$*"; else eval "$@"; fi; }

RUN_DIR="$ROOT/_lightmem_run"
KBIN="$RUN_DIR/lightmem-kernel"
MBIN="$RUN_DIR/lightmem-mcp"
PROFILE_FILE="$RUN_DIR/lims-quickstart.profile.yaml"
TOKENS="$RUN_DIR/agent-tokens.json"
LOG_REST="$RUN_DIR/lightmem.log"
LOG_MCP="$RUN_DIR/lightmem-mcp.log"
PID_REST="$RUN_DIR/lightmem.pid"
PID_MCP="$RUN_DIR/lightmem-mcp.pid"
REST_URL="http://$BIND:$PORT"

say "lightmem 一键部署"
cat <<EOF
    仓库      : $ROOT
    档位      : $PROFILE $( [ "$PROFILE" = solo ] && echo '（SQLite，零外部依赖）' || echo '（需要 PostgreSQL + pgvector）' )
    运行目录  : $RUN_DIR
    REST 地址 : $REST_URL
    MCP  地址 : http://$BIND:$MCP_PORT/mcp
    租户/用户 : $TENANT / $USER_ID
EOF

[ -f "$ROOT/kernel/cmd/lightmem-kernel/main.go" ] || die "这里不像是 lightmem 仓库根：$ROOT"

# ---------------------------------------------------------------- 1) 检查依赖
say "1/6 检查依赖"
command -v go >/dev/null 2>&1 || die "缺少 Go。请先安装 Go 1.25+：https://go.dev/dl/ （macOS: brew install go；Debian/Ubuntu: apt install golang）"
GO_VER=$(go version | awk '{print $3}')
ok "Go: $GO_VER"

HAVE_PY=0
for py in python3 python; do
    if command -v "$py" >/dev/null 2>&1; then
        HAVE_PY=1; PY_BIN="$py"
        ok "Python: $($py --version 2>&1)"
        break
    fi
done
[ "$HAVE_PY" -eq 1 ] || warn "没有 Python3 → 跳过 token 签发与配置生成（可稍后手工做，见 integrations/agent-onboarding/README.md）"

if [ "$PROFILE" = family ]; then
    command -v psql >/dev/null 2>&1 || warn "family 档需要 PostgreSQL 客户端 psql；请确认 PG 已就绪（handbook: manuals/p1-freebsd-zfs-setup.md）"
fi

# ---------------------------------------------------------------- 2) 编译
say "2/6 编译内核与 MCP 服务"
run "mkdir -p '$RUN_DIR'"
run "cd '$ROOT/kernel' && go build -o '$KBIN' ./cmd/lightmem-kernel"
ok "lightmem-kernel -> $KBIN"
if [ "$RUN_MCP" -eq 1 ]; then
    run "cd '$ROOT/kernel' && go build -o '$MBIN' ./cmd/lightmem-mcp"
    ok "lightmem-mcp -> $MBIN"
fi

# ---------------------------------------------------------------- 3) profile
say "3/6 生成 profile（档位 $PROFILE）"
if [ "$PROFILE" = solo ]; then
    run "mkdir -p '$RUN_DIR/data'"
    if [ "$DRY" -eq 0 ]; then
        cat > "$PROFILE_FILE" <<EOF
# lightmem quickstart profile（由 install.sh 生成；solo = SQLite，零外部依赖）
profile: solo
storage_backend: sqlite
storage:
  sqlite:
    path: $RUN_DIR/data/lims.sqlite
    wal_mode: true
    fk_enabled: true
  archive:
    path: $RUN_DIR/data/archive/
  embedding:
    model: BGE-small-zh
    dimensions: 512
    device: cpu
cache: none
mq: none
network:
  bind: $BIND
  api_port: $PORT
  pf:
    enabled: false
security:
  jail: false
  zfs_encryption: false
  mac: false
  acl: false
  ipsec: false
  pkg_audit: true
  process_accounting: true
  minimal_privilege: true
  tls: self_signed
  key_management: local_keyring
features:
  poison_guard: true
  audit: true
  delete_rights: true
  core_memory: true
  consolidation: false
  multi_signal_retrieval: false
  feedback_loop: true
  goal_memory: true
  external_connectors: false
scaling:
  mode: standalone
  nodes: 1
EOF
    fi
    ok "已写 $PROFILE_FILE"
else
    # family：沿用仓库自带 profile（用户可按需改）
    SRC="$ROOT/profiles/lims-family.profile.yaml"
    [ -f "$SRC" ] || die "找不到 $SRC"
    run "cp '$SRC' '$PROFILE_FILE'"
    ok "已复制 family profile -> $PROFILE_FILE（如需改 PG 地址请编辑它）"
fi

# ---------------------------------------------------------------- 4) token
say "4/6 签发 agent token"
if [ "$HAVE_PY" -eq 1 ]; then
    ISS="$ROOT/integrations/agent-onboarding/issue_token.py"
    if [ -f "$ISS" ]; then
        if [ -n "$AGENTS" ]; then
            run "cd '$RUN_DIR' && '$PY_BIN' '$ISS' --tenant '$TENANT' --user '$USER_ID' --agents '$AGENTS' --out '$RUN_DIR'"
        else
            run "cd '$RUN_DIR' && '$PY_BIN' '$ISS' --tenant '$TENANT' --user '$USER_ID' --out '$RUN_DIR'"
        fi
        ok "token 表 -> $TOKENS"
    else
        warn "找不到 issue_token.py，跳过"
    fi
else
    warn "无 Python3，跳过 token 签发（服务端将以**透传态**运行：无鉴权）"
fi

# ---------------------------------------------------------------- 5) 启动
say "5/6 启动服务"
if [ "$DRY" -eq 1 ]; then
    printf '    [dry-run] 启动 REST: %s -profile %s -listen %s:%s\n' "$KBIN" "$PROFILE_FILE" "$BIND" "$PORT"
    [ "$RUN_MCP" -eq 1 ] && printf '    [dry-run] 启动 MCP : %s -transport http -listen %s:%s -rest-base %s\n' "$MBIN" "$BIND" "$MCP_PORT" "$REST_URL"
else
    # 先停旧的：
    #   1) 按 pid 精确停（pid 文件可能是上一轮留下的）；
    #   2) **再确认端口已释放** —— 否则新进程会 `bind: address already in use`
    #      而**静默失败**：健康检查却因为旧进程还在而"通过"，
    #      于是你以为装好了，实际跑的是**旧 token**（新 token 全 401）。
    for pf in "$PID_REST" "$PID_MCP"; do
        [ -f "$pf" ] && kill "$(cat "$pf")" 2>/dev/null || true
        sleep 0.3
        rm -f "$pf"
    done

    port_free() {
        # 端口可用 = 连不上；能连上说明还有进程在监听
        ! curl -fsS --max-time 1 "http://$BIND:$1/healthz" >/dev/null 2>&1
    }
    wait_port() {
        _p=$1; _i=0
        while [ "$_i" -lt 15 ]; do
            if port_free "$_p"; then return 0; fi
            _i=$((_i + 1)); sleep 0.5
        done
        return 1
    }
    wait_port "$PORT" || warn "端口 $PORT 仍被占用（可能是上一轮进程未退出）；新进程可能起不来"
    [ "$RUN_MCP" -eq 1 ] && { wait_port "$MCP_PORT" || warn "端口 $MCP_PORT 仍被占用"; }

    TOKENS_ENV=""
    [ -f "$TOKENS" ] && TOKENS_ENV=$(cat "$TOKENS")

    if [ -n "$TOKENS_ENV" ]; then
        LIMS_AGENT_TOKENS="$TOKENS_ENV" nohup "$KBIN" -profile "$PROFILE_FILE" -listen "$BIND:$PORT" \
            > "$LOG_REST" 2>&1 &
    else
        nohup "$KBIN" -profile "$PROFILE_FILE" -listen "$BIND:$PORT" > "$LOG_REST" 2>&1 &
    fi
    echo $! > "$PID_REST"

    # 等 REST 就绪
    i=0
    while [ "$i" -lt 20 ]; do
        if curl -fsS "$REST_URL/healthz" >/dev/null 2>&1; then break; fi
        i=$((i + 1)); sleep 1
    done
    curl -fsS "$REST_URL/healthz" >/dev/null 2>&1 || { echo "--- 日志尾部 ---"; tail -20 "$LOG_REST" >&2; die "REST 服务未就绪，见日志 $LOG_REST"; }
    ok "REST 就绪：$(curl -fsS "$REST_URL/healthz")"

    if [ "$RUN_MCP" -eq 1 ]; then
        if [ -n "$TOKENS_ENV" ]; then
            LIMS_AGENT_TOKENS="$TOKENS_ENV" nohup "$MBIN" -transport http -listen "$BIND:$MCP_PORT" \
                -rest-base "$REST_URL" > "$LOG_MCP" 2>&1 &
        else
            nohup "$MBIN" -transport http -listen "$BIND:$MCP_PORT" -rest-base "$REST_URL" \
                > "$LOG_MCP" 2>&1 &
        fi
        echo $! > "$PID_MCP"
        i=0
        while [ "$i" -lt 20 ]; do
            if curl -fsS "http://$BIND:$MCP_PORT/healthz" >/dev/null 2>&1; then break; fi
            i=$((i + 1)); sleep 1
        done
        curl -fsS "http://$BIND:$MCP_PORT/healthz" >/dev/null 2>&1 || { tail -20 "$LOG_MCP" >&2; die "MCP 服务未就绪，见日志 $LOG_MCP"; }
        # **关键**：健康检查通过 ≠ 新进程起来了。
        # 若端口上还留着旧进程，上面的 curl 会命中**旧进程**而"通过"，
        # 于是换发的 token 全部 401，而你看到的是一片正常。
        # 这里直接查日志：新进程若 bind 失败会写 `address already in use`。
        if grep -q "address already in use" "$LOG_MCP" 2>/dev/null; then
            echo ""
            echo "  ⚠️  检测到端口 $MCP_PORT 已被占用：新 MCP 进程未启动，"
            echo "     当前响应该端口的是**旧进程**（因此你换发的 token 会 401）。"
            echo "     处置：先彻底停掉占用者，再重跑本脚本。常见停法："
            echo "       - 本脚本上一轮： kill \$(cat $PID_MCP)"
            echo "       - rc.d 部署：   sudo service lightmem_mcp stop"
            echo "       - 查占用者(Linux)： ss -ltnp | grep $MCP_PORT"
            echo "       - 查占用者(macOS)： lsof -nP -iTCP:$MCP_PORT -sTCP:LISTEN"
            echo "       - FreeBSD：      sockstat -4l | grep $MCP_PORT"
            echo ""
            die "MCP 端口 $MCP_PORT 被占用，未启用新 token"
        fi
        ok "MCP 就绪：$(curl -fsS "http://$BIND:$MCP_PORT/healthz")"
    fi
fi

# ---------------------------------------------------------------- 6) 客户端配置
say "6/6 生成客户端接入包"
if [ "$HAVE_PY" -eq 1 ] && [ -f "$RUN_DIR/agent-tokens.json" ]; then
    GEN="$ROOT/integrations/agent-onboarding/gen_configs.py"
    if [ -f "$GEN" ]; then
        MCP_FOR_GEN="http://$BIND:$MCP_PORT"
        run "cd '$RUN_DIR' && '$PY_BIN' '$GEN' --server '$MCP_FOR_GEN' --tokens '$TOKENS' --mcp-binary '$MBIN' --rest-base '$REST_URL' --out '$RUN_DIR/clients'"
        ok "接入包 -> $RUN_DIR/clients/"
    else
        warn "找不到 gen_configs.py，跳过"
    fi
else
    warn "跳过客户端接入包生成（缺 Python3 或 token 表）"
fi

# ---------------------------------------------------------------- systemd（可选）
if [ "$DO_SYSTEMD" -eq 1 ]; then
    say "附加：安装 systemd 服务"
    if [ "$(id -u)" -ne 0 ]; then
        warn "--systemd 需要 root；请用 sudo 重跑，或手工参考下方生成的文件"
    fi
    SD="$RUN_DIR/lightmem.service"
    if [ "$DRY" -eq 0 ]; then
        cat > "$SD" <<EOF
[Unit]
Description=lightmem memory kernel (REST)
After=network.target

[Service]
Type=simple
Environment=LIMS_AGENT_TOKENS=$( [ -f "$TOKENS" ] && cat "$TOKENS" || echo '' )
ExecStart=$KBIN -profile $PROFILE_FILE -listen $BIND:$PORT
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
    fi
    ok "已生成 $SD"
    if [ "$(id -u)" -eq 0 ]; then
        run "cp '$SD' /etc/systemd/system/lightmem.service && systemctl daemon-reload && systemctl enable --now lightmem"
        ok "systemd 服务已启用"
    else
        warn "以 root 执行： cp $SD /etc/systemd/system/lightmem.service && systemctl daemon-reload && systemctl enable --now lightmem"
    fi
fi

# ---------------------------------------------------------------- 总结
cat <<EOF

$(printf '\033[1;32m')============================================================$(printf '\033[0m')
$(printf '\033[1;32m')  lightmem 已就绪$(printf '\033[0m')
$(printf '\033[1;32m')============================================================$(printf '\033[0m')

  REST      : $REST_URL        （业务 API）
  MCP       : http://$BIND:$MCP_PORT/mcp   （给 AI Agent 接的就是这个）
  健康检查  : curl $REST_URL/healthz

  ┌─ 下一步：让 AI 接进来（三种方式，任选）────────────────────
  │
  │  ①【最省事】把这段 prompt 粘给你的 AI（需文件读写权限）：
  │
  │      请阅读 $RUN_DIR/clients/agent-prompt.md 并照它执行，
  │      把 lightmem 的 MCP 配置装到本机各 AI 工具里。
  │
  │  ②【手工】照 $RUN_DIR/clients/MANIFEST.md 逐家粘贴配置
  │
  │  ③【只验服务端】curl 一下：
  │
  │      TOK=\$(python3 -c "import json;print(list(json.load(open('$TOKENS')))[0])" 2>/dev/null || echo '')
  │      curl -s -X POST http://$BIND:$MCP_PORT/mcp \\
  │        -H 'Content-Type: application/json' \\
  │        -H "Accept: application/json, text/event-stream" \\
  │        -H "Authorization: Bearer \$TOK" \\
  │        -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' | head -c 300
  │
  └───────────────────────────────────────────────────────────

  停止服务： kill \$(cat $PID_REST) $( [ "$RUN_MCP" -eq 1 ] && echo "\$(cat $PID_MCP)" )
  数据位置： $RUN_DIR/data/lims.sqlite （solo 档）
  日志：     $LOG_REST  /  $LOG_MCP

  ⚠️  $TOKENS 含明文凭据 —— 请勿提交进 git，生产环境请换发正式 token。
EOF
