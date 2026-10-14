#!/usr/bin/env bash
# ============================================================
# R11-B: 首批 8 包 Family 档装配脚本
# 真源：eval/packages/family_profile.yaml + eval/packages/manifests/*.json
# 用途：打通「装包 / 配包 / 用包 / 沉淀包经验」四环节，
#       验证 安装 -> 配置 -> 升级 -> 卸载 全链路 + 包记忆抽取 + 拓扑排序。
#
# 运行环境：FreeBSD 15.1（验证机 192.168.0.88），需 Go 1.25。
# 本脚本在 lightmem 仓库根目录执行（须能 go run ./eval/packages/cmd/package-family）。
#
# 红线遵守：
#   - 内存 SQLite（:memory:），不依赖真实 PG/网络/外部服务；
#   - 不调 LLM；全部确定性；可重复执行；
#   - 只调用 kernel 既有公共 API，不改 kernel 语义。
#
# 用法：
#   bash eval/packages/install_family.sh            # 全流程：order + lifecycle + memory
#   bash eval/packages/install_family.sh order      # 仅拓扑排序
#   bash eval/packages/install_family.sh lifecycle  # 仅全链路生命周期
#   bash eval/packages/install_family.sh memory     # 仅包记忆抽取 + 召回验证
#   bash eval/packages/install_family.sh cycle-demo # 演示循环依赖检测报错
# ============================================================
set -euo pipefail

# 仓库根目录（本脚本位于 eval/packages/，向上两层）
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

CMD="./eval/packages/cmd/package-family"
MANIFESTS="eval/packages/manifests"
PROFILE="eval/packages/family_profile.yaml"

# 首批 8 包（与 family_profile.yaml / kernel.FirstPartyPackages 一致）
EXPECTED_PACKAGES=("registry" "gitea" "llm-gateway" "git-connector" "search" "backup" "mesh-sync" "webui")
# 期望拓扑顺序（由 family_profile.yaml 推导，webui 依赖最深）
EXPECTED_ORDER=("registry" "gitea" "llm-gateway" "git-connector" "search" "backup" "mesh-sync" "webui")

log() { printf '[r11b-package] %s\n' "$*"; }
die() { printf '[r11b-package][ERROR] %s\n' "$*" >&2; exit 1; }

precheck() {
    [[ -f "${PROFILE}" ]] || die "family_profile.yaml 缺失: ${PROFILE}"
    for p in "${EXPECTED_PACKAGES[@]}"; do
        [[ -f "${MANIFESTS}/${p}.json" ]] || die "manifest 缺失: ${MANIFESTS}/${p}.json"
    done
    # 校验 manifest 数量正好 8 个
    local count
    count=$(find "${MANIFESTS}" -name '*.json' -maxdepth 1 | wc -l | tr -d ' ')
    [[ "${count}" -eq 8 ]] || die "期望 8 个 manifest，实际 ${count}"
    command -v go >/dev/null 2>&1 || die "未找到 go，请确认 0.88 已安装 Go 1.25"
    log "前置检查通过：profile + 8 manifests + go 就绪"
}

cmd_order() {
    log "计算拓扑安装顺序 ..."
    go run ${CMD} order
    log "校验拓扑顺序与 family_profile.yaml 期望一致 ..."
    local actual
    actual=$(go run ${CMD} order 2>/dev/null | grep -E '^  [0-9]+\.' | sed -E 's/^  [0-9]+\. //')
    # want 也走命令替换，同样会去掉末尾换行，与 actual 对齐
    local want
    want=$(printf '%s\n' "${EXPECTED_ORDER[@]}")
    if [[ "${actual}" != "${want}" ]]; then
        die "拓扑顺序与期望不一致。期望:${want}实际:${actual}"
    fi
    log "拓扑顺序校验通过"
}

cmd_lifecycle() {
    log "跑通全链路：安装 -> 配置 -> 启用 -> 升级 -> 卸载 ..."
    go run ${CMD} lifecycle
}

cmd_memory() {
    log "抽取 8 包包记忆 + 召回验证 ..."
    go run ${CMD} memory
}

# 演示循环依赖检测：直接跑 kernel 单测 TestInstallOrder_CycleDetected，
# 验证错误安装顺序（含回路）会被 InstallOrder 报 cycle detected 提示。
cmd_cycle_demo() {
    log "循环依赖检测验证（跑 kernel 单测）..."
    go test ./kernel/ -run 'TestInstallOrder_CycleDetected|TestInstallOrder_SelfDependency|TestInstallOrder_UnknownDependency' -v -count=1
    log "循环依赖检测验证完成"
}

main() {
    precheck
    local sub="${1:-all}"
    case "${sub}" in
        order)       cmd_order ;;
        lifecycle)   cmd_lifecycle ;;
        memory)      cmd_memory ;;
        cycle-demo)  cmd_cycle_demo ;;
        all|"")      cmd_order; cmd_lifecycle; cmd_memory ;;
        *)           die "未知子命令: ${sub}" ;;
    esac
    log "完成：${sub}"
}

main "$@"