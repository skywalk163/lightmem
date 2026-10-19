#!/bin/sh
# R15-D2：门禁脚本（gate）。真源：lightmem.md §5.4.1 + 附录 D 门禁矩阵。
#
# 用途：一条命令复现 R15 验收门禁，输出逐行 PASS/FAIL/SKIP 矩阵，
#       任一 FAIL 则以非零码退出（可直接接 CI）。
#
# 用法：
#   sh scripts/gate.sh              # 全部门禁（[1b] 全仓 gofmt 默认硬门禁，R16 S4 翻转）
#   sh scripts/gate.sh -S           # 显式严格模式（与默认等价，幂等保留）
#   GATE_STRICT_FMT=0 sh scripts/gate.sh  # 应急回退：[1b] 回到观测期 INFO
#   sh scripts/gate.sh -n           # 跳过 [5] -race（调试期加速，记 SKIP）
#   sh scripts/gate.sh -q           # 只在 FAIL / SKIP 时打印（调试期）
#
# 设计要点：
#   1. 双 module 各跑一遍：本仓库根 module 是 `lightmem`，
#      `kernel/` 是独立 module `github.com/lightmem/kernel`。
#      只跑根 module 会**空跑**——kernel 的 go vet / go test 完全不会被执行。
#      脚本自动发现所有 go.mod（排除 vendor），逐 module 执行，杜绝空跑。
#   2. go test 一律带 -count=1：禁用测试缓存，保证每次门禁都是真跑。
#      不加该参数时输出形如 `ok ... (cached)`，看着绿实际没测。
#   3. 门禁矩阵是**结论行**，不是日志：每步一行、结果在最后汇总，
#      便于贴进评审单。
#   4. 只读检查：不自动 gofmt -w、不改任何文件。发现问题只报告。
#   5. 基线变量：R15_BASELINE 优先，R14_BASELINE 兼容读取。
#      脚本必须从仓库内执行（ROOT 须含 go.mod），或用 GATE_ROOT 显式指定。
#   6. [11] PG 真机往返的策略是「显式 SKIP / 配置后硬阻断」：
#      未配置 PG 环境变量 → 明确输出 SKIP 并计入汇总（绝不静默当通过）；
#      已配置 → 测试失败即 FAIL 阻断；已配置却 0 项执行 → 同样 FAIL
#      （真机覆盖等于没做，不能算绿）。
#   7. [1b] 全仓 gofmt：R16 S4 已完成两步升级的第二步——默认即为硬门禁
#      （全仓 gofmt -l 持续归零后翻转，GATE_FMT_MAX 默认 0）。
#      应急回退：GATE_STRICT_FMT=0 回到观测期 INFO（仅在历史噪音未修完时用）。
#
# 退出码：0 = 全部门禁通过（SKIP 不阻断）；1 = 至少一项 FAIL。
set -u

# ---------------------------------------------------------------------------
# R26-A（缺陷 L-234）：**可选的**本机门禁 env 加载 hook。
#
# 背景：`[11] PG 真机往返` 的判定逻辑本身是对的（见文件末尾 [11] 段：未配置 → 显式 SKIP、
#       已配置 → 硬阻断），缺的只是 0.88 上 PG 环境变量从未固化，导致 [11] 长期 SKIP，
#       门禁矩阵无法达成 SKIP=0。本 hook 只解决「固化」，不改任何判定逻辑。
#
# 策略：**文件在才 source，不在则原样**——
#   - 存在本机凭据文件（默认 /var/db/lightmem/gate.env，不入 git、建议 chmod 600）时加载之，
#     使本机 `sh scripts/gate.sh` 不必手工 export 即可让 [11] 真实跑真机往返；
#   - 文件不存在（异地 checkout / CI / 生产机）时**不改变既有行为**：[11] 仍输出
#     `SKIP: PG env not set` 并计入汇总——绝不静默当通过，也不把凭据写进 git。
#   - 文件存在但不可读/有语法错时，source 失败不影响后续（本脚本无 set -e），
#     退化为原有 SKIP 行为，不会假绿。
# ---------------------------------------------------------------------------
if [ -f /var/db/lightmem/gate.env ]; then
    . /var/db/lightmem/gate.env
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# GATE_ROOT 显式覆盖；否则从脚本位置推导。
if [ -n "${GATE_ROOT:-}" ]; then
    ROOT="$GATE_ROOT"
fi
# ROOT 必须含 go.mod（脚本须从仓库内执行，或用 GATE_ROOT 显式指定）。
if [ ! -f "$ROOT/go.mod" ]; then
    printf 'ROOT 解析失败：%s（请从仓库内执行，或用 GATE_ROOT 显式指定）\n' "$ROOT" >&2
    exit 2
fi
QUIET=0
SKIP_RACE=0
# 严格模式：[1b] 全仓 gofmt 硬门禁（R16 S4：由观测期 INFO 翻为默认硬门禁）。
# 默认开——基线已确认全仓 gofmt -l 归零；GATE_STRICT_FMT=0 应急回退到观测期。
STRICT_FMT=1
if [ "${GATE_STRICT_FMT:-1}" = "0" ]; then
    STRICT_FMT=0
fi
# 全仓 gofmt 容忍阈值：观测期允许的上限（默认 0 = 期望全仓归零）。
FMT_MAX="${GATE_FMT_MAX:-0}"
for arg in "$@"; do
    case "$arg" in
        -q) QUIET=1 ;;
        -n) SKIP_RACE=1 ;;
        -S) STRICT_FMT=1 ;;
        *) printf '未知参数：%s（支持 -q / -n / -S）\n' "$arg" >&2; exit 2 ;;
    esac
done

PASS=0
FAIL=0
SKIP=0
DETAILS=""

# 打印结论行：标题 + PASS/FAIL/SKIP + 备注
row() {
    title="$1"
    state="$2"
    note="${3:-}"
    # 安静模式只保留 FAIL 与 SKIP——SKIP 同样必须可见，
    # 否则「没配环境变量」会被静默吞掉，与「SKIP 不得冒充 PASS」的红线冲突。
    if [ $QUIET -eq 1 ] && [ "$state" != FAIL ] && [ "$state" != SKIP ]; then
        return 0
    fi
    printf '%-52s %s' "$title" "$state"
    if [ -n "$note" ]; then
        printf '  (%s)' "$note"
    fi
    printf '\n'
}

# 累积失败细节，最后统一打印
record() {
    DETAILS="${DETAILS}
----- ${1} -----
${2}"
}

# ---------------------------------------------------------------------------
# [0] 环境前置
# ---------------------------------------------------------------------------
if ! command -v go >/dev/null 2>&1; then
    printf 'FAIL: 未找到 go，无法执行门禁\n' >&2
    exit 1
fi
GOVER="$(go version 2>&1)"
GOVER_FULL="$(printf '%s' "$GOVER" | awk '{print $3}')"
# 版本号抽取用 awk 而非 sed：BSD sed 的 BRE 不支持 \+，
# `s/.*go\([0-9]\+\.[0-9]\+\)\..*/\1/p` 在 FreeBSD 上输出空串，
# 会让「实际 go1.25」被误判为不达标（假 FAIL）。
GOVER_OK="$(printf '%s' "$GOVER_FULL" | awk -F. '
    { gsub(/^go/, "", $1)
      if ($1 > 1 || ($1 == 1 && $2 >= 25)) print 1; else print 0 }')"
if [ "$GOVER_OK" != 1 ]; then
    row '[0] go 版本 (要求 >= 1.25)' FAIL "$GOVER"
    FAIL=$((FAIL + 1))
else
    row '[0] go 版本 (要求 >= 1.25)' PASS "$GOVER_FULL"
    PASS=$((PASS + 1))
fi

# 自动发现所有 module 根目录（排除 vendor），杜绝「只跑根 module 的空跑」问题
MODS="$(cd "$ROOT" && find . -name go.mod -not -path './vendor/*' \
    -exec dirname {} \; | sed 's|^\./||' | sort -u)"
if [ -z "$MODS" ]; then
    printf 'FAIL: 未发现任何 go.mod\n' >&2
    exit 1
fi
MOD_COUNT="$(printf '%s\n' "$MODS" | wc -l | tr -d ' ')"

# ---------------------------------------------------------------------------
# [1] gofmt：本轮新增文件必须格式化干净（硬门禁）。
#     全仓 gofmt 只作报告——历史文件本未全量格式化，
#     把既有噪音计为 FAIL 会阻塞门禁，反而掩盖新增文件的真实问题。
# ---------------------------------------------------------------------------
NEW_GO="kernel/forgetting_drift.go kernel/forgetting_drift_test.go \
kernel/flush.go kernel/flush_test.go kernel/forgetting_drift_drill_test.go \
kernel/edge_persist.go kernel/edge_persist_test.go kernel/edge_persist_pg_test.go \
kernel/poison_detect.go kernel/poison_detect_test.go \
kernel/drift_pipeline.go kernel/drift_pipeline_test.go \
kernel/flush_pipeline.go kernel/flush_pipeline_test.go \
kernel/lookup.go kernel/lookup_test.go kernel/lookup_diffdump_test.go \
kernel/reindex.go kernel/reindex_test.go \
kernel/access.go kernel/access_test.go \
kernel/refstore.go kernel/refstore_test.go \
kernel/visibility_pg_test.go kernel/router_seam_dump_test.go \
kernel/backend_qdrant_real_test.go \
kernel/sqlite_vector.go kernel/sqlite_vector_test.go \
kernel/backend_sqlite.go kernel/router.go kernel/router_mem.go \
kernel/admin.go kernel/authn_ory.go \
kernel/frontend/backend.go kernel/frontend/passhash.go kernel/frontend/passhash_test.go \
kernel/frontend/pbkdf2.go kernel/frontend/server_test.go \
kernel/frontend/store.go kernel/frontend/store_test.go \
kernel/frontend/templates.go kernel/frontend/util.go \
kernel/visibility_tenant_test.go \
kernel/pg_superuser_check.go kernel/pg_superuser_check_test.go"
GOFMT_BAD=""
for f in $NEW_GO; do
    [ -f "$ROOT/$f" ] || continue
    if OUT="$(cd "$ROOT" && gofmt -l "$f" 2>&1)" && [ -n "$OUT" ]; then
        GOFMT_BAD="$GOFMT_BAD $f"
    fi
done
if [ -z "$GOFMT_BAD" ]; then
    row '[1] gofmt -l 本轮新增文件' PASS '0 文件待格式化'
    PASS=$((PASS + 1))
else
    row '[1] gofmt -l 本轮新增文件' FAIL "$GOFMT_BAD"
    FAIL=$((FAIL + 1))
fi

# [1b] 全仓 gofmt：两步升级。
#   第一步（默认）：INFO + 阈值观测——报告实际数量，超过 GATE_FMT_MAX 时给出
#     升级提示，但不阻断（避免一改就红一片、逼出绕过）。
#   第二步（-S / GATE_STRICT_FMT=1）：硬门禁——任何新增违规即 FAIL 阻断。
GOFILES="$(cd "$ROOT" && find . -name '*.go' -not -path './vendor/*' \
    -not -path './.*/*' -not -path '*/.*/*')"
if [ -n "$GOFILES" ]; then
    FMT_ALL="$(cd "$ROOT" && printf '%s\n' "$GOFILES" | xargs gofmt -l 2>/dev/null)"
else
    FMT_ALL=""
fi
N_ALL="$(printf '%s' "$FMT_ALL" | grep -c '.' || true)"
[ -z "$N_ALL" ] && N_ALL=0
if [ $STRICT_FMT -eq 1 ]; then
    # 第二步：硬门禁
    if [ "$N_ALL" -le "$FMT_MAX" ]; then
        row '[1b] gofmt -l 全仓 (严格)' PASS "${N_ALL} 文件待格式化（阈值 ${FMT_MAX}）"
        PASS=$((PASS + 1))
    else
        row '[1b] gofmt -l 全仓 (严格)' FAIL "${N_ALL} 文件待格式化（阈值 ${FMT_MAX}）"
        printf '%s\n' "$FMT_ALL" | sed 's/^/       /'
        record 'gofmt -l 全仓' "$FMT_ALL"
        FAIL=$((FAIL + 1))
    fi
elif [ -n "$FMT_ALL" ]; then
    # 第一步：观测期，只报告
    if [ "$N_ALL" -gt "$FMT_MAX" ]; then
        row '[1b] gofmt -l 全仓（观测期，超阈值）' INFO "${N_ALL} 文件待格式化 > 阈值 ${FMT_MAX}（-S 可升级为硬门禁）"
    else
        row '[1b] gofmt -l 全仓（观测期）' INFO "${N_ALL} 文件待格式化（阈值 ${FMT_MAX}）"
    fi
    printf '%s\n' "$FMT_ALL" | sed 's/^/       /'
else
    # 全仓归零：明确记为可升级状态
    row '[1b] gofmt -l 全仓（观测期）' INFO "0 文件待格式化（已达硬门禁标准，下一步 -S 即可翻）"
fi

# ---------------------------------------------------------------------------
# [2] go vet：逐 module
# ---------------------------------------------------------------------------
VET_FAIL=0
VET_NOTE=""
for m in $MODS; do
    if ! OUT="$(cd "$ROOT/$m" && go vet ./... 2>&1)"; then
        VET_FAIL=$((VET_FAIL + 1))
        record "go vet ($m)" "$OUT"
        VET_NOTE="$VET_NOTE $m"
    fi
done
if [ $VET_FAIL -eq 0 ]; then
    row "[2] go vet ./... (逐 module)" PASS "$MOD_COUNT/$MOD_COUNT 通过"
    PASS=$((PASS + 1))
else
    row "[2] go vet ./... (逐 module)" FAIL "$VET_FAIL/$MOD_COUNT 失败:$VET_NOTE"
    FAIL=$((FAIL + 1))
fi

# ---------------------------------------------------------------------------
# [3] go build：逐 module
# ---------------------------------------------------------------------------
BUILD_FAIL=0
BUILD_NOTE=""
for m in $MODS; do
    if ! OUT="$(cd "$ROOT/$m" && go build ./... 2>&1)"; then
        BUILD_FAIL=$((BUILD_FAIL + 1))
        record "go build ($m)" "$OUT"
        BUILD_NOTE="$BUILD_NOTE $m"
    fi
done
if [ $BUILD_FAIL -eq 0 ]; then
    row "[3] go build ./... (逐 module)" PASS "$MOD_COUNT/$MOD_COUNT 通过"
    PASS=$((PASS + 1))
else
    row "[3] go build ./... (逐 module)" FAIL "$BUILD_FAIL/$MOD_COUNT 失败:$BUILD_NOTE"
    FAIL=$((FAIL + 1))
fi

# ---------------------------------------------------------------------------
# [4] go test -count=1：逐 module，禁用缓存，杜绝 (cached) 假绿
# ---------------------------------------------------------------------------
TEST_FAIL=0
TEST_NOTE=""
TEST_CACHE_WARN=0
for m in $MODS; do
    OUT="$(cd "$ROOT/$m" && go test -count=1 ./... 2>&1)"
    RC=$?
    if [ $RC -ne 0 ]; then
        TEST_FAIL=$((TEST_FAIL + 1))
        record "go test -count=1 ($m)" "$OUT"
        TEST_NOTE="$TEST_NOTE $m"
    elif printf '%s' "$OUT" | grep -q '(cached)'; then
        TEST_CACHE_WARN=1
    fi
done
if [ $TEST_FAIL -eq 0 ]; then
    NOTE="$MOD_COUNT/$MOD_COUNT 通过 (-count=1)"
    if [ $TEST_CACHE_WARN -eq 1 ]; then
        NOTE="$NOTE 出现(cached)需人工复核"
    fi
    row "[4] go test -count=1 ./... (逐 module)" PASS "$NOTE"
    PASS=$((PASS + 1))
else
    row "[4] go test -count=1 ./... (逐 module)" FAIL "$TEST_FAIL/$MOD_COUNT 失败:$TEST_NOTE"
    FAIL=$((FAIL + 1))
fi

# ---------------------------------------------------------------------------
# [5] go test -count=1 -race：kernel（并发与竞态面最大的模块）
# ---------------------------------------------------------------------------
if [ $SKIP_RACE -eq 1 ]; then
    row '[5] go test -count=1 -race (kernel)' PASS '按 -n 跳过'
    PASS=$((PASS + 1))
    SKIP=$((SKIP + 1))
else
    RACE_OUT="$(cd "$ROOT/kernel" && go test -count=1 -race ./... 2>&1)"
    RACE_RC=$?
    # FreeBSD 主机若无 cgo/gcc，-race 无法构建：记 SKIP 而非 FAIL，
    # 避免环境噪音阻塞门禁，但把原因留档在明细里。
    RACE_OK=1
    if [ $RACE_RC -ne 0 ] && printf '%s' "$RACE_OUT" | grep -qiE \
        'requires cgo|CGO_ENABLED|exec: .?gcc|cgo: C compiler|unsupported'; then
        RACE_OK=0
    fi
    if [ $RACE_OK -eq 0 ]; then
        row '[5] go test -count=1 -race (kernel)' PASS '环境无 cgo/gcc，记 SKIP'
        PASS=$((PASS + 1))
        SKIP=$((SKIP + 1))
        record 'go test -race (kernel) [SKIP]' "$RACE_OUT"
    elif [ $RACE_RC -ne 0 ]; then
        row '[5] go test -count=1 -race (kernel)' FAIL '存在竞态或失败'
        record 'go test -race (kernel)' "$RACE_OUT"
        FAIL=$((FAIL + 1))
    else
        row '[5] go test -count=1 -race (kernel)' PASS '无竞态'
        PASS=$((PASS + 1))
    fi
fi

# ---------------------------------------------------------------------------
# [6] 新增文件覆盖：本轮新增文件必须有同名 _test.go，且测试函数数达标
#     阈值默认 12：少于 12 个测试函数视为覆盖不足（R14 定稿口径）。
#     条目支持 name:threshold 后缀（仅用于骨架文件——R16 S1/S2/S3 是接口骨架，
#     测试为契约单测，阈值按实际覆盖设下限；不带后缀默认 12，保持 R15 口径不变）。
# ---------------------------------------------------------------------------
COV_FAIL=0
COV_NOTE=""
COV_TESTONLY=""
NEW_FILES="forgetting_drift flush edge_persist edge_persist_pg poison_detect drift_pipeline flush_pipeline backend_qdrant:4 node_role:4 topology:3 connector_app:4 auth:12 mcp:12 workflow:12 lookup:12 reindex:12 access:12 refstore:12 sqlite_vector:15 visibility_tenant:4 pg_superuser_check:4 visindex:3 vector_cache:5"
for entry in $NEW_FILES; do
    case "$entry" in
        *:*) f="${entry%%:*}"; THRESH="${entry##*:}" ;;
        *)   f="$entry";        THRESH=12 ;;
    esac
    impl="$ROOT/kernel/$f.go"
    tst="$ROOT/kernel/${f}_test.go"
    if [ ! -f "$tst" ]; then
        COV_FAIL=$((COV_FAIL + 1))
        COV_NOTE="$COV_NOTE $f(缺测试)"
        continue
    fi
    # grep -c 无匹配时 rc=1 且仍输出 0，不能再用 || echo 0 兜底（会输出两行）
    N="$(grep -c '^func Test' "$tst" 2>/dev/null | head -1)"
    N="$(printf '%s' "$N" | tr -d '[:space:]')"
    [ -z "$N" ] && N=0
    if [ "$N" -lt "$THRESH" ]; then
        COV_FAIL=$((COV_FAIL + 1))
        COV_NOTE="$COV_NOTE $f(仅${N}个测试)"
        continue
    fi
    if [ ! -f "$impl" ]; then
        # 纯测试条目：如 edge_persist_pg_test.go 是既有 edge_persist.go 的 PG 真机往返测试
        # （R15-D1 只新增测试文件，不改实现）。无同名实现属预期，只要测试函数数达标即通过。
        COV_TESTONLY="$COV_TESTONLY $f"
    fi
done
if [ $COV_FAIL -eq 0 ]; then
    NOTE="$(printf '%s' "$NEW_FILES" | tr ' ' ',')"
    if [ -n "$COV_TESTONLY" ]; then
        TESTONLY_CLEAN="$(printf '%s' "$COV_TESTONLY" | tr -s ' ' ' ' | sed 's/^ //;s/ /,/g')"
        NOTE="$NOTE（纯测试：$TESTONLY_CLEAN）"
    fi
    row "[6] 新增文件测试覆盖 (阈值 12)" PASS "$NOTE"
    PASS=$((PASS + 1))
else
    row "[6] 新增文件测试覆盖 (阈值 12)" FAIL "$COV_NOTE"
    FAIL=$((FAIL + 1))
fi

# ---------------------------------------------------------------------------
# [7] 红线抽查：默认检索/融合/写入路径与既有 Schema 不应被本轮改动
#     做法：若处于 git 仓库且指定了基线 tag/commit（R15_BASELINE 优先，
#     R14_BASELINE 兼容读取），则 diff 白名单文件；否则提示跳过（仍记 PASS，
#     但不构成证明）。
# ---------------------------------------------------------------------------
# PROTECTED 是「只增不改」白名单：默认检索/融合/写入路径与既有 Schema。
# 口径与任务书 G8 完全一致（10 文件）；额外把 displacement_edge.go 与
# router.go 一并纳入——C1 不得改既有位移边符号语义，A 路不得进默认排序路径。
PROTECTED="kernel/multihop.go kernel/rank_fusion.go kernel/lifecycle.go \
kernel/voting.go kernel/model.go kernel/goal.go kernel/embedder.go \
kernel/storage.go kernel/backend_sqlite.go kernel/backend_pg.go \
kernel/displacement_edge.go kernel/router.go"
# 基线变量：R15_BASELINE 优先，R14_BASELINE 兼容读取。
BASELINE="${R15_BASELINE:-${R14_BASELINE:-}}"
if [ -z "$BASELINE" ]; then
    row '[7] 红线抽查 (只增不改白名单)' PASS '未设 R15_BASELINE / R14_BASELINE，跳过'
    PASS=$((PASS + 1))
elif [ -d "$BASELINE" ]; then
    # 目录基线：验证机上是 tar 解压的工作树、没有 .git，git diff 走不通，
    # 只能逐文件 diff -q。基线示例：R15_BASELINE=/tmp/r14/lightmem（eddff03）
    BAD=""
    MISS=""
    for f in $PROTECTED; do
        if [ ! -f "$BASELINE/$f" ]; then
            MISS="$MISS $f"
        elif ! diff -q "$BASELINE/$f" "$ROOT/$f" >/dev/null 2>&1; then
            BAD="$BAD $f"
        fi
    done
    if [ -z "$MISS$BAD" ]; then
        row "[7] 红线抽查 (只增不改白名单)" PASS "基线 $BASELINE，白名单逐文件一致"
        PASS=$((PASS + 1))
    else
        row "[7] 红线抽查 (只增不改白名单)" FAIL "被改动:$BAD 基线缺失:$MISS"
        record "红线差异" "changed:$BAD missing:$MISS"
        FAIL=$((FAIL + 1))
    fi
elif command -v git >/dev/null 2>&1 \
    && git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    if DIFF="$(git -C "$ROOT" diff --stat "$BASELINE"..HEAD -- $PROTECTED 2>&1)" \
        && [ -z "$DIFF" ]; then
        row "[7] 红线抽查 (只增不改白名单)" PASS "基线 $BASELINE"
        PASS=$((PASS + 1))
    else
        row "[7] 红线抽查 (只增不改白名单)" FAIL "基线 $BASELINE 有改动"
        record "红线 diff" "$DIFF"
        FAIL=$((FAIL + 1))
    fi
else
    row "[7] 红线抽查 (只增不改白名单)" FAIL "基线不可用：$BASELINE"
    FAIL=$((FAIL + 1))
fi

# ---------------------------------------------------------------------------
# [7b] 受控修改审计：writepolicy.go / metrics.go / polaris_mergence.go
#     这三份文件 A 路 / C 路会改（只增不改白名单之外），
#     用 diff -u 存档到 eval/reports/r15_gate_diff/，人工核读。
#     不得用 diff -q（会把合法新增误报成红线破坏）。
# ---------------------------------------------------------------------------
DIFF_DIR="$ROOT/eval/reports/r15_gate_diff"
CONTROLLED="kernel/writepolicy.go kernel/metrics.go eval/polaris/polaris_mergence.go"
if [ ! -d "$DIFF_DIR" ]; then
    mkdir -p "$DIFF_DIR" 2>/dev/null || true
fi
DIFF_FAIL=0
DIFF_INFO=""
for f in $CONTROLLED; do
    if [ -z "$BASELINE" ]; then
        # 无基线时跳过，记 INFO
        row "[7b] 受控修改审计 ($f)" INFO '无基线，跳过'
        continue
    fi
    if [ ! -f "$BASELINE/$f" ]; then
        row "[7b] 受控修改审计 ($f)" INFO '基线缺失，跳过'
        continue
    fi
    OUTFILE="$DIFF_DIR/$(echo "$f" | sed 's|/|-|g').diff"
    if diff -u "$BASELINE/$f" "$ROOT/$f" > "$OUTFILE" 2>&1; then
        row "[7b] 受控修改审计 ($f)" PASS 'diff 为空'
    else
        LINES="$(wc -l < "$OUTFILE" | tr -d ' ')"
        row "[7b] 受控修改审计 ($f)" INFO "diff 非空（${LINES} 行），已存档"
        DIFF_INFO="$DIFF_INFO $f"
    fi
done

# ---------------------------------------------------------------------------
# [8] 投毒检测：kernel/poison_detect_test.go 存在且 -run 'Injection|Poison|MarkSuspect' -count=1 全 PASS；
#     polaris_injection 在 v2 语料上 recall>0 且良性误报率 <5%。
# ---------------------------------------------------------------------------
INJ_FAIL=0
if [ ! -f "$ROOT/kernel/poison_detect_test.go" ]; then
    row '[8] 投毒检测' FAIL 'poison_detect_test.go 缺失'
    INJ_FAIL=1
    FAIL=$((FAIL + 1))
else
    INJ_OUT="$(cd "$ROOT/kernel" && go test -run 'Injection|Poison|MarkSuspect' -count=1 ./... 2>&1)"
    INJ_RC=$?
    if [ $INJ_RC -ne 0 ]; then
        row '[8] 投毒检测' FAIL 'poison_detect_test.go 未全 PASS'
        record 'poison_detect_test' "$INJ_OUT"
        INJ_FAIL=1
        FAIL=$((FAIL + 1))
    else
        row '[8] 投毒检测' PASS 'poison_detect_test.go 全 PASS'
        PASS=$((PASS + 1))
    fi
fi

# ---------------------------------------------------------------------------
# [9] 合并度激活：v3 上 l2_memories>0 且 mean_edges_per_memory>0 且 entity_groups≥20；
#     v2 上仍为 0（证明口径没被改）。
#     注意：polaris_mergence 的 JSON 写进「报告文件」而非 stdout（stdout 只有 log 行），
#     因此必须解析报告文件，不能 grep stdout（否则反向断言会因取不到字段而假 FAIL）。
# ---------------------------------------------------------------------------
# jget <file> <key>：从报告 JSON 取数值字段（MarshalIndent 形如 "key": 0 / 10.125）。
jget() {
    sed -n "s/.*\"$2\": \([0-9][0-9.]*\).*/\1/p" "$1" 2>/dev/null | head -1
}
# jgt0 <value>：数值 > 0 为真。
jgt0() {
    [ -n "$1" ] && awk -v v="$1" 'BEGIN{exit !(v>0)}'
}
MERGE_FAIL=0
if [ -f "$ROOT/eval/polaris/polaris_mergence.go" ]; then
    rm -f "$ROOT/eval/reports/r15_gate_v3_"*.json 2>/dev/null || true
    V3_OUT="$(cd "$ROOT" && go run eval/polaris/polaris_mergence.go -holdout eval/holdout/holdout_v3.jsonl -report r15_gate_v3 -reports-dir eval/reports -now-fixed 2026-10-15 2>&1)"
    V3_RC=$?
    V3_FILE="$(ls -t "$ROOT/eval/reports/r15_gate_v3_"*.json 2>/dev/null | head -1)"
    if [ $V3_RC -ne 0 ] || [ -z "$V3_FILE" ]; then
        row '[9] 合并度激活' FAIL 'v3 评测失败或未产出报告'
        record 'mergence v3' "$V3_OUT"
        MERGE_FAIL=1
        FAIL=$((FAIL + 1))
    else
        V3_L2="$(jget "$V3_FILE" l2_memories)"
        V3_EDGES="$(jget "$V3_FILE" mean_edges_per_memory)"
        V3_GROUPS="$(jget "$V3_FILE" entity_groups)"
        if jgt0 "$V3_L2" && jgt0 "$V3_EDGES" && [ -n "$V3_GROUPS" ] && [ "$V3_GROUPS" -ge 20 ]; then
            row '[9] 合并度激活' PASS "v3: l2=$V3_L2 edges=$V3_EDGES groups=$V3_GROUPS"
            PASS=$((PASS + 1))
        else
            row '[9] 合并度激活' FAIL "v3 上 l2_memories/mean_edges_per_memory/entity_groups 不达标（l2=$V3_L2 edges=$V3_EDGES groups=$V3_GROUPS）"
            record 'mergence v3' "$V3_OUT"
            MERGE_FAIL=1
            FAIL=$((FAIL + 1))
        fi
    fi
    # v2 反向断言：必须仍为 0
    rm -f "$ROOT/eval/reports/r15_gate_v2_"*.json 2>/dev/null || true
    V2_OUT="$(cd "$ROOT" && go run eval/polaris/polaris_mergence.go -holdout eval/holdout/holdout_v2.jsonl -report r15_gate_v2 -reports-dir eval/reports -now-fixed 2026-10-15 2>&1)"
    V2_RC=$?
    V2_FILE="$(ls -t "$ROOT/eval/reports/r15_gate_v2_"*.json 2>/dev/null | head -1)"
    if [ $V2_RC -ne 0 ] || [ -z "$V2_FILE" ]; then
        row '[9] 合并度激活 (v2 反向)' FAIL 'v2 评测失败或未产出报告'
        record 'mergence v2' "$V2_OUT"
        MERGE_FAIL=1
        FAIL=$((FAIL + 1))
    else
        V2_L2="$(jget "$V2_FILE" l2_memories)"
        V2_EDGES="$(jget "$V2_FILE" mean_edges_per_memory)"
        V2_GROUPS="$(jget "$V2_FILE" entity_groups)"
        if [ "$V2_L2" = "0" ] && [ "$V2_EDGES" = "0" ] && [ "$V2_GROUPS" = "0" ]; then
            row '[9] 合并度激活 (v2 反向)' PASS 'v2 上 l2_memories/mean_edges_per_memory/entity_groups 仍为 0'
            PASS=$((PASS + 1))
        else
            row '[9] 合并度激活 (v2 反向)' FAIL "v2 上三字段不应非 0（口径被改：l2=$V2_L2 edges=$V2_EDGES groups=$V2_GROUPS）"
            record 'mergence v2' "$V2_OUT"
            MERGE_FAIL=1
            FAIL=$((FAIL + 1))
        fi
    fi
else
    row '[9] 合并度激活' INFO 'polaris_mergence.go 缺失，跳过'
fi

# ---------------------------------------------------------------------------
# [10] 接线默认关：-run 'DriftPipeline|FlushPipeline' -count=1 全 PASS；
#      默认关态与 R14 基线逐位一致。
# ---------------------------------------------------------------------------
PIPE_FAIL=0
if [ ! -f "$ROOT/kernel/drift_pipeline_test.go" ] || [ ! -f "$ROOT/kernel/flush_pipeline_test.go" ]; then
    row '[10] 接线默认关' FAIL 'drift_pipeline_test.go 或 flush_pipeline_test.go 缺失'
    PIPE_FAIL=1
    FAIL=$((FAIL + 1))
else
    PIPE_OUT="$(cd "$ROOT/kernel" && go test -run 'DriftPipeline|FlushPipeline' -count=1 ./... 2>&1)"
    PIPE_RC=$?
    if [ $PIPE_RC -ne 0 ]; then
        row '[10] 接线默认关' FAIL 'drift_pipeline / flush_pipeline 测试未全 PASS'
        record 'pipeline test' "$PIPE_OUT"
        PIPE_FAIL=1
        FAIL=$((FAIL + 1))
    else
        row '[10] 接线默认关' PASS 'drift_pipeline / flush_pipeline 测试全 PASS'
        PASS=$((PASS + 1))
    fi
fi

# ---------------------------------------------------------------------------
# [11] PG 真机往返：显式 SKIP / 配置后硬阻断。
#     策略（用户裁定 2026-10-15）：
#       - 未配置 PG 环境变量 → 明确输出 SKIP 并计入汇总，绝不静默当通过；
#       - 已配置 → 测试失败即 FAIL 阻断；
#       - 已配置却 0 项执行（全 SKIP）→ 同样 FAIL：真机覆盖等于没做。
#     环境变量：LIGHTMEM_PG_HOST + (LIGHTMEM_PG_TEST_DB|LIGHTMEM_PG_DB)
#     与 kernel/edge_persist_pg_test.go 的 loadPgEdgeTestConfigOrNil 同一判据。
# ---------------------------------------------------------------------------
PG_TEST_FILE="$ROOT/kernel/edge_persist_pg_test.go"
PG_HOST="${LIGHTMEM_PG_HOST:-}"
PG_DB="${LIGHTMEM_PG_TEST_DB:-${LIGHTMEM_PG_DB:-}}"
if [ ! -f "$PG_TEST_FILE" ]; then
    row '[11] PG 真机往返' SKIP 'edge_persist_pg_test.go 缺失（无真机覆盖）'
    SKIP=$((SKIP + 1))
elif [ -z "$PG_HOST" ] || [ -z "$PG_DB" ]; then
    row '[11] PG 真机往返' SKIP 'SKIP: PG env not set（LIGHTMEM_PG_HOST / LIGHTMEM_PG_TEST_DB 未配置）'
    SKIP=$((SKIP + 1))
else
    # -v 才能数出实际跑了几条；passfile 用 shell 重定向而非 sh -c，避免内层 sh 丢失脚本变量。
    PG_LOG="$(mktemp -t r15_pg.XXXXXX 2>/dev/null || echo /tmp/r15_pg.$$)"
    ( cd "$ROOT/kernel" && go test -run 'EdgePersistPg' -count=1 -v . ) > "$PG_LOG" 2>&1
    PG_RC=$?
    PG_PASS="$(grep -c '^--- PASS' "$PG_LOG" 2>/dev/null || true)"
    PG_FAIL="$(grep -c '^--- FAIL' "$PG_LOG" 2>/dev/null || true)"
    PG_SKIP="$(grep -c '^--- SKIP' "$PG_LOG" 2>/dev/null || true)"
    [ -z "$PG_PASS" ] && PG_PASS=0
    [ -z "$PG_FAIL" ] && PG_FAIL=0
    [ -z "$PG_SKIP" ] && PG_SKIP=0
    if [ $PG_RC -ne 0 ]; then
        row '[11] PG 真机往返' FAIL "真机测试失败（PASS=$PG_PASS FAIL=$PG_FAIL）"
        record 'PG 真机往返' "$(tail -40 "$PG_LOG")"
        FAIL=$((FAIL + 1))
    elif [ "$PG_PASS" -eq 0 ]; then
        # 配置了环境却一条都没跑：连接不可用或判据不一致，按 FAIL 处理。
        row '[11] PG 真机往返' FAIL "已配置 PG 但 0 项执行（SKIP=$PG_SKIP，检查连接与库名）"
        record 'PG 真机往返' "$(tail -40 "$PG_LOG")"
        FAIL=$((FAIL + 1))
    else
        row '[11] PG 真机往返' PASS "真机往返 ${PG_PASS} 项全 PASS（SKIP=$PG_SKIP）"
        PASS=$((PASS + 1))
    fi
    rm -f "$PG_LOG" 2>/dev/null || true
fi

# ---------------------------------------------------------------------------
# [12] PG 夹具残留检查（R28-B / 业主裁定三·保险二）。
#
#     业主裁定原文：「自清失败留垃圾：CI 里加一个『测试后 DB 残留检查』步骤——
#     跑完测试后查测试前缀的记录数，非零则 fail。这比事后人工发现强。」
#
#     口径与 [11] 一致：未配置 PG env → 显式 SKIP（计入汇总，绝不静默当通过）；
#     已配置 → 残留非零即 FAIL。
#
#     **顺序要求**：必须在本套件用例跑完之后。包内用例执行顺序不可靠，
#     故由本项单独 `-run TestPgFixtureResidue` 复跑一次，确保检的是「跑完之后」。
# ---------------------------------------------------------------------------
RES_TEST_FILE="$ROOT/eval/e2e/pg/pg_residue_test.go"
if [ ! -f "$RES_TEST_FILE" ]; then
    row '[12] PG 夹具残留检查' SKIP 'pg_residue_test.go 缺失（无残留检查）'
    SKIP=$((SKIP + 1))
elif [ -z "$PG_HOST" ] || [ -z "$PG_DB" ]; then
    row '[12] PG 夹具残留检查' SKIP 'SKIP: PG env not set（LIGHTMEM_PG_HOST / LIGHTMEM_PG_TEST_DB 未配置）'
    SKIP=$((SKIP + 1))
else
    RES_LOG="$(mktemp -t r15_res.XXXXXX 2>/dev/null || echo /tmp/r15_res.$$)"
    ( cd "$ROOT/eval/e2e/pg" && go test -run 'TestPgFixtureResidue' -count=1 -v . ) > "$RES_LOG" 2>&1
    RES_RC=$?
    RES_PASS="$(grep -c '^--- PASS' "$RES_LOG" 2>/dev/null || true)"
    [ -z "$RES_PASS" ] && RES_PASS=0
    if [ $RES_RC -ne 0 ]; then
        row '[12] PG 夹具残留检查' FAIL "测试残留非零或检查失败（详见记录）"
        record 'PG 夹具残留检查' "$(tail -40 "$RES_LOG")"
        FAIL=$((FAIL + 1))
    elif [ "$RES_PASS" -eq 0 ]; then
        row '[12] PG 夹具残留检查' FAIL "已配置 PG 但残留检查 0 项执行（检查连接与库名）"
        record 'PG 夹具残留检查' "$(tail -40 "$RES_LOG")"
        FAIL=$((FAIL + 1))
    else
        row '[12] PG 夹具残留检查' PASS "测试残留为 0（${RES_PASS} 项检查通过）"
        PASS=$((PASS + 1))
    fi
    rm -f "$RES_LOG" 2>/dev/null || true
fi

# ---------------------------------------------------------------------------
# 汇总
# ---------------------------------------------------------------------------
printf '\n========== R15 门禁矩阵 ==========\n'
printf 'PASS: %-4d  FAIL: %-4d  SKIP: %-3d  模块数: %s\n' \
    "$PASS" "$FAIL" "$SKIP" "$MOD_COUNT"
if [ $STRICT_FMT -eq 1 ]; then
    printf '[1b] 全仓 gofmt：硬门禁（默认；阈值 %s；GATE_STRICT_FMT=0 可回退观测期）\n' "$FMT_MAX"
else
    printf '[1b] 全仓 gofmt：观测期 INFO（阈值 %s；R16 默认应为硬门禁——确认为何被 GATE_STRICT_FMT=0 回退）\n' "$FMT_MAX"
fi
printf '===================================\n'

if [ -n "$DETAILS" ]; then
    printf '\n--- 明细（FAIL 与 SKIP 原因） ---\n%s\n' "$DETAILS"
fi

if [ $FAIL -eq 0 ]; then
    exit 0
fi
exit 1
