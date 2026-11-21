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
#     sh install.sh --systemd                 # 额外安装 systemd 服务（Linux；kernel+MCP 双 unit）
#     sh install.sh --systemd --service-user lightmem   # 指定 unit 的 User/Group
#     sh install.sh --uninstall-systemd       # 卸载双 unit（停服务 + 删 unit + daemon-reload）
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
UNINSTALL_SYSTEMD=0
SERVICE_USER=""
DRY=0
AGENTS=""
WRITE_ENV_DIR=""   # 内部钩子（见 lm_write_token_env 注释），供 envfile_test.sh 调用

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
        --uninstall-systemd) UNINSTALL_SYSTEMD=1; shift ;;
        --service-user) SERVICE_USER="$2"; shift 2 ;;
        --dry-run)  DRY=1; shift ;;
        # 内部钩子：只走「压平 JSON → 写 EnvironmentFile」这一段后退出（不编译/不装服务）。
        # 现成调用者：integrations/quickstart/envfile_test.sh；不是给用户的手动参数。
        --write-token-env) WRITE_ENV_DIR="$2"; shift 2 ;;
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

# ---------------------------------------------------------------- 凭据写入 EnvironmentFile（必须单行）
# 为什么：systemd 的 EnvironmentFile **按行解析**。agent-tokens.json 是约 42 行美化 JSON，
# 原样写进去只会保留首行 `{`，kernel 一启动就 Fatal：
#   [boot] auth config: LIMS_AGENT_TOKENS: parse agent tokens json: unexpected end of JSON input
#   → status=1/FAILURE → 连着重启 5 次 → Start request repeated too quickly
# 做法（T-0424 根修 T-0417 TODO#1）：
#   - 仓库完整 → 直接调用 scripts/lm-env-compact.sh（同一份已发布口径，避免两处实现漂移）；
#   - 只拿到 integrations/ 子集（scripts/ 不可达）→ 内联 python3 json.dumps 压平；
#   - python3 缺失 → **明确失败**（绝不静默写出一份会崩的 env）；
#   - 写完自检：恰好 1 行、前缀对、解析回来与源 JSON 等价、权限 600。
# 只改「怎么塞进 EnvironmentFile」：agent-tokens.json 一个字节都不动（不增删字段、不改大小写）。
lm_write_token_env() {
    _lm_run_dir=$1
    _lm_tokens="$_lm_run_dir/agent-tokens.json"
    _lm_envf="$_lm_run_dir/lightmem.env"
    [ -f "$_lm_tokens" ] || die "env 写入：缺少 token 文件 $_lm_tokens"

    if ! command -v python3 >/dev/null 2>&1; then
        die "env 写入需要 python3（把多行 JSON 压成单行）：$_lm_envf 未生成。
      没有 python3 就会写出一份 systemd 只认首行、kernel 启动即 Fatal 的 env，故此处直接失败。
      装好 python3（Debian/Ubuntu: apt install -y python3；FreeBSD: pkg install -y python3）后重跑本脚本；
      或事后手工修：sudo sh $ROOT/scripts/lm-env-compact.sh $_lm_run_dir && sudo systemctl restart lightmem.service"
    fi

    _lm_compact="$ROOT/scripts/lm-env-compact.sh"
    if [ -f "$_lm_compact" ]; then
        ( umask 077; sh "$_lm_compact" "$_lm_run_dir" ) >/dev/null \
            || die "env 写入失败：scripts/lm-env-compact.sh $_lm_run_dir 返回非 0（原因见上方 stderr）"
    else
        # 退化：只拿到 integrations/ 子集时 scripts/ 不可达；内联同一口径（ensure_ascii=False + 紧凑分隔符）
        ( umask 077
          python3 - "$_lm_tokens" "$_lm_envf" <<'PY'
import json, os, sys

tokens_path, env_path = sys.argv[1], sys.argv[2]
with open(tokens_path, encoding='utf-8') as f:
    data = json.load(f)
line = 'LIMS_AGENT_TOKENS=' + json.dumps(data, ensure_ascii=False, separators=(',', ':'))
with open(env_path, 'w', encoding='utf-8') as f:
    f.write(line + '\n')
os.chmod(env_path, 0o600)
PY
        ) || die "env 写入失败：内联 python3 压平 $_lm_tokens 返回非 0"
    fi

    chmod 600 "$_lm_envf" 2>/dev/null || true
    # 自检：行数 / 前缀 / JSON 等价 / 权限。任何一条不满足都算写入失败（不 break 后续步骤）。
    python3 - "$_lm_tokens" "$_lm_envf" <<'PY' || die "env 自检失败：$_lm_envf 不是「单行且与 agent-tokens.json 等价」的 EnvironmentFile"
import json, os, stat, sys

tokens_path, env_path = sys.argv[1], sys.argv[2]
with open(tokens_path, encoding='utf-8') as f:
    want = json.load(f)
with open(env_path, encoding='utf-8') as f:
    raw = f.read()
prefix = 'LIMS_AGENT_TOKENS='
if raw.count('\n') != 1 or not raw.endswith('\n'):
    sys.exit('行数=%d（期望恰好 1 行 + 结尾换行）' % raw.count('\n'))
line = raw[:-1]
if not line.startswith(prefix):
    sys.exit('首行不是 %s 开头：%.40s' % (prefix, line))
got = json.loads(line[len(prefix):])
if got != want:
    sys.exit('env 里的 token 与 %s 不等价（字段/大小写/值不一致）' % tokens_path)
mode = stat.S_IMODE(os.stat(env_path).st_mode)
if mode != 0o600:
    sys.exit('权限=%o（期望 600）' % mode)
print('OK: %s 单行 %d bytes, json 等价, 600' % (env_path, len(raw)))
PY
}

# 内部钩子（调用者 integrations/quickstart/envfile_test.sh）：只跑 env 写入路径，然后退出
if [ -n "$WRITE_ENV_DIR" ]; then
    mkdir -p "$WRITE_ENV_DIR"
    lm_write_token_env "$WRITE_ENV_DIR"
    exit 0
fi

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

# ---------------------------------------------------------------- 仅卸载（--uninstall-systemd）
# 单独成段并提前退出：卸载不该顺带编译/起裸进程。端口口径见文末 systemd 段。
if [ "$UNINSTALL_SYSTEMD" -eq 1 ]; then
    say "卸载 systemd 服务（lightmem-mcp + lightmem）"
    if [ "$DRY" -eq 0 ] && [ "$(id -u)" -ne 0 ]; then
        warn "卸载 unit 需要 root：sudo sh $0 --uninstall-systemd"
        die "未做任何改动（权限不足）"
    fi
    # 先停 MCP 再停 kernel：反序会让 MCP 在 kernel 消失后反复 on-failure 重试
    run "systemctl disable --now lightmem-mcp.service 2>/dev/null || true"
    run "systemctl disable --now lightmem.service 2>/dev/null || true"
    run "rm -f /etc/systemd/system/lightmem-mcp.service /etc/systemd/system/lightmem.service"
    run "systemctl daemon-reload"
    run "systemctl reset-failed lightmem-mcp.service lightmem.service 2>/dev/null || true"
    ok "已卸载双 unit（幂等：未安装时同样不报错）"
    say "卸载完成；要重装请去掉 --uninstall-systemd 再跑一次"
    exit 0
fi

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
if command -v go >/dev/null 2>&1; then
    GO_VER=$(go version | awk '{print $3}')
    ok "Go: $GO_VER"
elif [ "$DRY" -eq 1 ]; then
    # dry-run 只出计划、不编译：缺 Go 不该让"看计划"这一步失败（CI 容器常见）
    warn "未找到 Go（dry-run 不编译，仅提示）；真跑前请装 Go 1.25+：https://go.dev/dl/"
else
    die "缺少 Go。请先安装 Go 1.25+：https://go.dev/dl/ （macOS: brew install go；Debian/Ubuntu: apt install golang）"
fi

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
# R29-T0 PE-2：注入 BuildVersion（启动自检的「二进制 vs 库」一致性比对）。
# 未注入也能跑（退化为不判定）；注入后 /healthz.version 与 VERSION 校验才真正生效。
BUILD_VER="$(cd "$ROOT" && git rev-parse HEAD 2>/dev/null || echo unknown)"
run "cd '$ROOT/kernel' && go build -ldflags '-X github.com/lightmem/kernel.BuildVersion=$BUILD_VER' -o '$KBIN' ./cmd/lightmem-kernel"
ok "lightmem-kernel -> $KBIN (version=$BUILD_VER)"
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
# 端口口径（**两套不得混**）：本脚本 + systemd = 7443/7444；FreeBSD rc.d = 18443/18444。
# LIGHTMEM_UNIT_DIR 可覆盖 unit 落点（仅用于测试/沙箱，默认 /etc/systemd/system）。
UNIT_DIR="${LIGHTMEM_UNIT_DIR:-/etc/systemd/system}"

systemd_hint() {
    cat >&2 <<'EOF'
    [!] systemctl 调用失败。先确认这台机器真的有 systemd（容器里通常没有）：
          systemctl --version
        若在 FreeBSD：本节不适用，请走 rc.d（口径 :18443/:18444，与本脚本 :7443/:7444 不同，勿混改）：
          sudo cp integrations/lims-088-rc.d/lightmem* /usr/local/etc/rc.d/
          sudo sysrc lightmem_enable=YES lightmem_mcp_enable=YES
          sudo service lightmem start && sudo service lightmem_mcp start
        若 unit 已装但起不来：journalctl -u lightmem.service -u lightmem-mcp.service -n 60 --no-pager
EOF
}

if [ "$DO_SYSTEMD" -eq 1 ]; then
    say "附加：安装 systemd 服务（kernel :$PORT / MCP :$MCP_PORT）"

    OS_NAME=$(uname -s 2>/dev/null || echo unknown)
    SKIP_SYSTEMD=0
    if [ "$OS_NAME" = FreeBSD ]; then
        warn "检测到 FreeBSD：systemd 不适用；本脚本不会碰任何 rc.d 文件"
        SKIP_SYSTEMD=1      # dry-run 仍把计划打出来（供 CI 校验脚本路径），真跑一律不动手
    fi

    # 平台抽象：优先复用 T0 的 deploy/platform.sh（接口已冻结：lm_service_install <src> <name>）。
    # 本分支若尚未合入 T0，则降级为等价的内置 cp —— 行为一致，只是少了 OS 判断那层封装。
    PLATFORM_SH="$ROOT/deploy/platform.sh"
    if [ -f "$PLATFORM_SH" ]; then
        . "$PLATFORM_SH"
        LM_DRY_RUN="$DRY"; export LM_DRY_RUN
        ok "已引入 deploy/platform.sh，用 lm_service_install 装 unit"
    else
        warn "未找到 deploy/platform.sh（T0 尚未合入）；降级为内置 cp 安装（等价）"
    fi

    [ -n "$SERVICE_USER" ] || SERVICE_USER=$( [ "$(id -u)" -eq 0 ] && echo "${SUDO_USER:-root}" || id -un )

    TPL_KERNEL="$ROOT/deploy/lightmem.service"
    TPL_MCP="$ROOT/deploy/lightmem-mcp.service"
    UNIT_KERNEL="$RUN_DIR/lightmem.service"
    UNIT_MCP="$RUN_DIR/lightmem-mcp.service"
    ENV_FILE="$RUN_DIR/lightmem.env"

    [ -f "$TPL_KERNEL" ] || die "找不到 unit 模板 $TPL_KERNEL"
    if [ "$RUN_MCP" -eq 1 ] && [ ! -f "$TPL_MCP" ]; then
        die "找不到 unit 模板 $TPL_MCP"
    fi

    # 把 /opt 布局的模板渲染成本次 quickstart 的 $RUN_DIR 布局：
    #   * 二进制 / profile / 数据 / 日志全部指向 $RUN_DIR
    #   * 端口按本脚本口径（7443 / 7444）
    #   * User/Group 换成实际运行者（仓库不在 lightmem 账号名下时，写死 lightmem 会 Permission denied）
    #   * ProtectHome 关闭（quickstart 目录可能落在 /home）
    render_unit() {
        _tpl=$1; _out=$2; _usr=$3
        sed -e "s|/opt/lightmem/profiles/lims-family.profile.yaml|$PROFILE_FILE|g" \
            -e "s|127.0.0.1:7443|$BIND:$PORT|g" \
            -e "s|127.0.0.1:7444|$BIND:$MCP_PORT|g" \
            -e "s|^User=lightmem$|User=$_usr|" \
            -e "s|^Group=lightmem$|Group=$_usr|" \
            -e "s|^ReadWritePaths=.*|ReadWritePaths=$RUN_DIR|" \
            -e "s|^ProtectHome=.*|# ProtectHome 已关闭：quickstart 目录可能位于 /home 下|" \
            -e "s|/opt/lightmem|$RUN_DIR|g" \
            "$_tpl" > "$_out"
    }

    install_unit() {   # $1 = 源 unit 文件, $2 = unit 名（如 lightmem.service）
        _u_src=$1; _u_name=$2
        if command -v lm_service_install >/dev/null 2>&1; then
            lm_service_install "$_u_src" "$_u_name"   # T0 接口：Linux 装 unit / FreeBSD 装 rc 脚本；LM_DRY_RUN 可空跑
        elif [ "$DRY" -eq 1 ]; then
            printf '    [dry-run] cp %s %s/%s\n' "$_u_src" "$UNIT_DIR" "$_u_name"
        else
            cp "$_u_src" "$UNIT_DIR/$_u_name"
        fi
    }

    if [ "$SKIP_SYSTEMD" -eq 1 ] && [ "$DRY" -eq 0 ]; then
        ok "已跳过 systemd 安装：FreeBSD 请走 rc.d（口径 :18443/:18444，勿与本脚本 :7443/:7444 混改）"
    elif [ "$(id -u)" -ne 0 ] && [ "$DRY" -eq 0 ]; then
        warn "--systemd 需要 root；请用 sudo 重跑本脚本。手工等价步骤："
        cat >&2 <<EOF
      # 1) 先按本脚本把二进制/profile 装到 $RUN_DIR（不带 --systemd 跑一遍即可）
      # 2) 渲染两个 unit（把 /opt/lightmem 换成 $RUN_DIR、端口 $PORT/$MCP_PORT、User 换成运行者）
      sudo sh $0 --profile $PROFILE --port $PORT --mcp-port $MCP_PORT --systemd
      # 3) 或直接照模板手装：
      sudo install -m 644 $TPL_KERNEL $UNIT_DIR/lightmem.service
      sudo install -m 644 $TPL_MCP    $UNIT_DIR/lightmem-mcp.service
      sudo systemctl daemon-reload && sudo systemctl enable --now lightmem.service lightmem-mcp.service
EOF
    else
        if [ "$DRY" -eq 0 ]; then
            render_unit "$TPL_KERNEL" "$UNIT_KERNEL" "$SERVICE_USER"
            if [ "$RUN_MCP" -eq 1 ]; then
                render_unit "$TPL_MCP" "$UNIT_MCP" "$SERVICE_USER"
            fi
            ok "已渲染 unit -> $UNIT_KERNEL $( [ "$RUN_MCP" -eq 1 ] && echo "/ $UNIT_MCP" )"
            # 凭据只进 EnvironmentFile（600），不进 unit 本体
            # 必须压成**单行** JSON：EnvironmentFile 按行解析，多行只有首行 `{` 进 kernel（T-0424）
            if [ -f "$TOKENS" ]; then
                lm_write_token_env "$RUN_DIR"
                ok "凭据写入 $ENV_FILE（600，单行 JSON）"
            else
                warn "没有 $TOKENS：unit 将以透传态运行（无鉴权）"
            fi
        else
            printf '    [dry-run] 压平 %s → %s（单行 JSON，600）\n' "$TOKENS" "$ENV_FILE"
        fi


        # 撤掉第 5 步用 nohup 起的裸进程：否则 systemd 启动时 bind 冲突 → 服务"看起来起来了"其实 401
        for pf in "$PID_REST" "$PID_MCP"; do
            if [ -f "$pf" ]; then
                run "kill '$(cat "$pf")' 2>/dev/null || true"
            fi
            run "rm -f '$pf'"
        done

        # dry-run 时用模板路径做"计划展示"（渲染产物尚未落盘）
        if [ "$DRY" -eq 1 ]; then
            SRC_K="$TPL_KERNEL"; SRC_M="$TPL_MCP"
        else
            SRC_K="$UNIT_KERNEL"; SRC_M="$UNIT_MCP"
        fi
        install_unit "$SRC_K" lightmem.service
        if [ "$RUN_MCP" -eq 1 ]; then
            install_unit "$SRC_M" lightmem-mcp.service
        fi
        run "systemctl daemon-reload"
        if [ "$RUN_MCP" -eq 1 ]; then
            ENABLE_UNITS="lightmem.service lightmem-mcp.service"
        else
            ENABLE_UNITS="lightmem.service"
        fi
        run "systemctl enable --now $ENABLE_UNITS" || { systemd_hint; die "systemctl enable --now 失败（见上方提示）"; }
        if [ "$DRY" -eq 1 ]; then
            ok "(dry-run) 将启用：$ENABLE_UNITS（开机自启 + 立即启动）"
        else
            ok "systemd 已启用：$ENABLE_UNITS（开机自启 + 立即启动）"
        fi

        if [ "$DRY" -eq 0 ]; then
            i=0
            while [ "$i" -lt 15 ]; do
                if curl -fsS "$REST_URL/healthz" >/dev/null 2>&1; then break; fi
                i=$((i + 1)); sleep 1
            done
            if curl -fsS "$REST_URL/healthz" >/dev/null 2>&1; then
                ok "systemd: REST 就绪 $(curl -fsS "$REST_URL/healthz")"
            else
                warn "REST 未就绪：journalctl -u lightmem.service -n 50 --no-pager"
            fi
            if [ "$RUN_MCP" -eq 1 ]; then
                if curl -fsS "http://$BIND:$MCP_PORT/healthz" >/dev/null 2>&1; then
                    ok "systemd: MCP 就绪 $(curl -fsS "http://$BIND:$MCP_PORT/healthz")"
                else
                    warn "MCP 未就绪：journalctl -u lightmem-mcp.service -n 50 --no-pager"
                fi
            fi
            warn "unit 里是 $RUN_DIR 的绝对路径：移动/删除本仓库会让这两个 unit 失效（先 --uninstall-systemd）"
        fi
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
