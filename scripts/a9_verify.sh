#!/bin/sh
# a9_verify.sh — R138-B 线 A9 实机验证脚本（可在 192.168.0.88 上直接复跑）
#
# 用途：一键复现 A9 的 8 项验证（格式 / vet / build / test / 评测集生成 / KPI 基线）
# 用法：sh scripts/a9_verify.sh [仓库根目录]     默认 ROOT="$(pwd)"
#
# 注意（见 .dumate/inbox/_R138_B线收口报告.md §3）：
#   - 必须用 0.88 的 Go 工具链执行，本机不具备 Go 环境；
#   - kernel 是独立子模块，eval 位于根模块并靠 replace 指向 ./kernel；
#   - go build ./... 会在源码树里留下二进制，脚本最后会清理，避免误入交付包。

set -u

ROOT="${1:-$(pwd)}"
echo "ROOT=$ROOT"
if [ ! -d "$ROOT/kernel" ] || [ ! -d "$ROOT/eval" ]; then
    echo "ERROR: $ROOT 下未找到 kernel/ 或 eval/，请传入正确的仓库根目录" >&2
    exit 1
fi

FAIL=0
check() { # check <标签> <RC>
    if [ "$2" -eq 0 ]; then
        echo "  [PASS] $1"
    else
        echo "  [FAIL] $1 (RC=$2)"
        FAIL=1
    fi
}

echo "=== [1/8] gofmt -l kernel eval（期望为空） ==="
cd "$ROOT" || exit 1
UNFMT=$(gofmt -l kernel eval)
if [ -z "$UNFMT" ]; then echo "  [PASS] gofmt 无差异"; else echo "$UNFMT"; echo "  [FAIL] 以上文件需 gofmt"; FAIL=1; fi

echo "=== [2/8] kernel: go vet ./... ==="
cd "$ROOT/kernel" && go vet ./...; check "go vet ./..." $?

echo "=== [3/8] kernel: go build ./... ==="
cd "$ROOT/kernel" && go build ./...; check "go build ./..." $?

echo "=== [4/8] kernel: go test ./... ==="
cd "$ROOT/kernel" && go test ./...; check "go test ./..." $?

echo "=== [5/8] eval: gofmt -l ==="
cd "$ROOT/eval" && gofmt -l .; check "eval gofmt -l" $?

echo "=== [6/8] eval: 生成评测集（期望 570 条） ==="
cd "$ROOT/eval" && go run ./eval_generate.go; check "eval_generate" $?
wc -l "$ROOT"/eval/dataset/*.jsonl

echo "=== [7/8] eval: KPI 基线 ==="
cd "$ROOT/eval" && go run ./run_kpi.go; check "run_kpi" $?
LATEST=$(ls -1t "$ROOT"/eval/reports/kpi_baseline_*.json 2>/dev/null | head -n 1)
echo "  最新报告：$LATEST"

echo "=== [8/8] 清理 build 散落二进制 ==="
cd "$ROOT" && rm -f kernel/lightmem-kernel && rm -rf kernel/kernel
echo "  已清理 kernel/lightmem-kernel, kernel/kernel/"

echo "=== 结果 ==="
if [ "$FAIL" -eq 0 ]; then
    echo "ALL PASS"
else
    echo "存在失败项，见上"
fi
exit "$FAIL"
