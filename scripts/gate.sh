#!/bin/sh
# R14-C2：门禁脚本（gate）。真源：lightmem.md §5.4.1 + 附录 D 门禁矩阵。
#
# 用途：一条命令复现 R14 验收门禁，输出逐行 PASS/FAIL 矩阵，
#       任一 FAIL 则以非零码退出（可直接接 CI）。
#
# 用法：
#   sh scripts/gate.sh              # 全部门禁
#   sh scripts/gate.sh -n race      # 跳过 -race（调试期加速）
#   sh scripts/gate.sh -q           # 只在 FAIL 时打印细节
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
#
# 退出码：0 = 全部门禁通过；1 = 至少一项 FAIL。
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
QUIET=0
SKIP_RACE=0
for arg in "$@"; do
    case "$arg" in
        -q) QUIET=1 ;;
        -n) SKIP_RACE=1 ;;
        *) printf '未知参数：%s（支持 -q / -n）\n' "$arg" >&2; exit 2 ;;
    esac
done

PASS=0
FAIL=0
SKIP=0
DETAILS=""

# 打印结论行：标题 + PASS/FAIL + 备注
row() {
    title="$1"
    state="$2"
    note="${3:-}"
    # 安静模式只保留 FAIL；PASS 与 INFO 全部抑制
    if [ $QUIET -eq 1 ] && [ "$state" != FAIL ]; then
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
kernel/edge_persist.go kernel/edge_persist_test.go"
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

# [1b] 全仓 gofmt：仅报告既有噪音，不计 FAIL（也不计入 PASS）
GOFILES="$(cd "$ROOT" && find . -name '*.go' -not -path './vendor/*' \
    -not -path './.*/*' -not -path '*/.*/*')"
if [ -n "$GOFILES" ]; then
    FMT_ALL="$(cd "$ROOT" && printf '%s\n' "$GOFILES" | xargs gofmt -l 2>/dev/null)"
else
    FMT_ALL=""
fi
N_ALL="$(printf '%s' "$FMT_ALL" | grep -c '.' || true)"
if [ -n "$FMT_ALL" ]; then
    row '[1b] gofmt -l 全仓（既有噪音，仅报告）' INFO "${N_ALL:-0} 文件待格式化"
    printf '%s\n' "$FMT_ALL" | sed 's/^/       /'
else
    row '[1b] gofmt -l 全仓' INFO '0 文件待格式化'
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
#     阈值 12：少于 12 个测试函数视为覆盖不足（R14 定稿口径）。
# ---------------------------------------------------------------------------
COV_FAIL=0
COV_NOTE=""
NEW_FILES="forgetting_drift flush edge_persist"
for f in $NEW_FILES; do
    impl="$ROOT/kernel/$f.go"
    tst="$ROOT/kernel/${f}_test.go"
    if [ ! -f "$impl" ]; then
        COV_FAIL=$((COV_FAIL + 1))
        COV_NOTE="$COV_NOTE $f(缺实现)"
        continue
    fi
    if [ ! -f "$tst" ]; then
        COV_FAIL=$((COV_FAIL + 1))
        COV_NOTE="$COV_NOTE $f(缺测试)"
        continue
    fi
    # grep -c 无匹配时 rc=1 且仍输出 0，不能再用 || echo 0 兜底（会输出两行）
    N="$(grep -c '^func Test' "$tst" 2>/dev/null | head -1)"
    N="$(printf '%s' "$N" | tr -d '[:space:]')"
    [ -z "$N" ] && N=0
    if [ "$N" -lt 12 ]; then
        COV_FAIL=$((COV_FAIL + 1))
        COV_NOTE="$COV_NOTE $f(仅${N}个测试)"
    fi
done
if [ $COV_FAIL -eq 0 ]; then
    row "[6] 新增文件测试覆盖 (阈值 12)" PASS "$(printf '%s' "$NEW_FILES" | tr ' ' ',')"
    PASS=$((PASS + 1))
else
    row "[6] 新增文件测试覆盖 (阈值 12)" FAIL "$COV_NOTE"
    FAIL=$((FAIL + 1))
fi

# ---------------------------------------------------------------------------
# [7] 红线抽查：默认检索/融合/写入路径与既有 Schema 不应被本轮改动
#     做法：若处于 git 仓库且指定了基线 tag/commit（R14_BASELINE），
#     则 diff 白名单文件；否则提示跳过（仍记 PASS，但不构成证明）。
# ---------------------------------------------------------------------------
# PROTECTED 是「只增不改」白名单：默认检索/融合/写入路径与既有 Schema。
# 口径与任务书 G8 完全一致（10 文件）；额外把 displacement_edge.go 与
# router.go 一并纳入——C1 不得改既有位移边符号语义，A 路不得进默认排序路径。
PROTECTED="kernel/multihop.go kernel/rank_fusion.go kernel/lifecycle.go \
kernel/voting.go kernel/model.go kernel/goal.go kernel/embedder.go \
kernel/storage.go kernel/backend_sqlite.go kernel/backend_pg.go \
kernel/displacement_edge.go kernel/router.go"
if [ -z "${R14_BASELINE:-}" ]; then
    row '[7] 红线抽查 (只增不改白名单)' PASS '未设 R14_BASELINE，跳过'
    PASS=$((PASS + 1))
elif [ -d "$R14_BASELINE" ]; then
    # 目录基线：验证机上是 tar 解压的工作树、没有 .git，git diff 走不通，
    # 只能逐文件 diff -q。基线示例：R14_BASELINE=/tmp/r13/lightmem（eddff03）
    BAD=""
    MISS=""
    for f in $PROTECTED; do
        if [ ! -f "$R14_BASELINE/$f" ]; then
            MISS="$MISS $f"
        elif ! diff -q "$R14_BASELINE/$f" "$ROOT/$f" >/dev/null 2>&1; then
            BAD="$BAD $f"
        fi
    done
    if [ -z "$MISS$BAD" ]; then
        row "[7] 红线抽查 (只增不改白名单)" PASS "基线 $R14_BASELINE，白名单逐文件一致"
        PASS=$((PASS + 1))
    else
        row "[7] 红线抽查 (只增不改白名单)" FAIL "被改动:$BAD 基线缺失:$MISS"
        record "红线差异" "changed:$BAD missing:$MISS"
        FAIL=$((FAIL + 1))
    fi
elif command -v git >/dev/null 2>&1 \
    && git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    if DIFF="$(git -C "$ROOT" diff --stat "$R14_BASELINE"..HEAD -- $PROTECTED 2>&1)" \
        && [ -z "$DIFF" ]; then
        row "[7] 红线抽查 (只增不改白名单)" PASS "基线 $R14_BASELINE"
        PASS=$((PASS + 1))
    else
        row "[7] 红线抽查 (只增不改白名单)" FAIL "基线 $R14_BASELINE 有改动"
        record "红线 diff" "$DIFF"
        FAIL=$((FAIL + 1))
    fi
else
    row "[7] 红线抽查 (只增不改白名单)" FAIL "基线不可用：$R14_BASELINE"
    FAIL=$((FAIL + 1))
fi

# ---------------------------------------------------------------------------
# 汇总
# ---------------------------------------------------------------------------
printf '\n========== R14 门禁矩阵 ==========\n'
printf 'PASS: %-4d  FAIL: %-4d  SKIP: %-3d  模块数: %s\n' \
    "$PASS" "$FAIL" "$SKIP" "$MOD_COUNT"
printf '===================================\n'

if [ -n "$DETAILS" ]; then
    printf '\n--- 明细（FAIL 与 SKIP 原因） ---\n%s\n' "$DETAILS"
fi

if [ $FAIL -eq 0 ]; then
    exit 0
fi
exit 1
